package providerresponse

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"one-api/types"
)

func TestSanitizeErrorPayloadPreservesOrdinaryPayloadBytes(t *testing.T) {
	payload := []byte("{\n  \"type\": \"response.output_text.delta\",\n  \"delta\": \"a  b\\n\\nhttps://example.com\",\n  \"api_key\": \"model-authored-value\"\n}\n")

	if safe := SanitizeErrorPayload(payload, nil); !bytes.Equal(safe, payload) {
		t.Fatalf("ordinary provider payload changed:\nwant: %q\n got: %q", payload, safe)
	}
}

func TestSanitizeErrorPayloadPreservesEnvelopeAndHidesAccountFailure(t *testing.T) {
	payload := []byte(`{"type":"response.failed","sequence_number":7,"response":{"id":"resp_1","status":"failed","error":{"type":"usage_limit_reached","code":"insufficient_quota","message":"organization org-secret exhausted","param":"account"}},"account_id":"acct-secret"}`)
	apiErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Type: "usage_limit_reached", Code: "insufficient_quota", Message: "organization org-secret exhausted", Param: "account"},
		StatusCode:  http.StatusTooManyRequests,
	}
	safe := SanitizeErrorPayload(payload, apiErr)
	for _, secret := range []string{"org-secret", "acct-secret", "insufficient_quota", "usage_limit_reached"} {
		if strings.Contains(string(safe), secret) {
			t.Fatalf("provider account detail %q leaked from %s", secret, safe)
		}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(safe, &object); err != nil {
		t.Fatalf("safe payload is invalid JSON: %v", err)
	}
	if string(object["type"]) != `"response.failed"` || string(object["sequence_number"]) != "7" || !strings.Contains(string(object["response"]), `"id":"resp_1"`) || !strings.Contains(string(object["response"]), `"code":"provider_account_error"`) {
		t.Fatalf("provider lifecycle envelope changed: %s", safe)
	}
}

func TestSanitizeErrorPayloadKeepsBusinessErrorAndRedactsIdentityFields(t *testing.T) {
	payload := []byte(`{"type":"error","sequence_number":3,"code":"bad_input","message":"invalid tool","account_id":"acct-secret","secret":"shared-secret","credential":"shared-credential"}`)
	apiErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Type: "invalid_request_error", Code: "bad_input", Message: "invalid tool"},
		StatusCode:  http.StatusBadRequest,
	}
	safe := SanitizeErrorPayload(payload, apiErr)
	if !strings.Contains(string(safe), `"code":"bad_input"`) || !strings.Contains(string(safe), `"message":"invalid tool"`) || !strings.Contains(string(safe), `"account_id":"[redacted]"`) || strings.Contains(string(safe), "shared-secret") || strings.Contains(string(safe), "shared-credential") {
		t.Fatalf("business error semantics or redaction changed: %s", safe)
	}
}

func TestSanitizeAPIErrorRetainsControlClassification(t *testing.T) {
	safe := SanitizeAPIError(&types.OpenAIErrorWithStatusCode{
		OpenAIError:       types.OpenAIError{Type: "authentication_error", Code: "invalid_api_key", Message: "key belongs to org-secret"},
		StatusCode:        http.StatusUnauthorized,
		RawBody:           []byte(`{"error":{"message":"key belongs to org-secret"}}`),
		ReplayRawResponse: true,
	})
	if safe.Code != "provider_account_error" || !safe.ProviderAuthRejected || strings.Contains(safe.Message, "org-secret") || len(safe.RawBody) != 0 || safe.ReplayRawResponse {
		t.Fatalf("unexpected safe control error: %+v", safe)
	}
}

func TestSanitizeNestedRealtimeErrorDoesNotInventTopLevelFields(t *testing.T) {
	payload := []byte(`{"type":"error","event_id":"evt_1","error":{"type":"authentication_error","code":"invalid_api_key","message":"account org-secret rejected"}}`)
	apiErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Type: "authentication_error", Code: "invalid_api_key", Message: "account org-secret rejected"},
		StatusCode:  http.StatusUnauthorized,
	}
	safe := SanitizeErrorPayload(payload, apiErr)
	var object map[string]json.RawMessage
	if err := json.Unmarshal(safe, &object); err != nil {
		t.Fatal(err)
	}
	if _, exists := object["message"]; exists {
		t.Fatalf("nested Realtime error gained a top-level message: %s", safe)
	}
	if _, exists := object["code"]; exists {
		t.Fatalf("nested Realtime error gained a top-level code: %s", safe)
	}
	if !strings.Contains(string(object["error"]), `"code":"provider_account_error"`) || string(object["event_id"]) != `"evt_1"` {
		t.Fatalf("nested error or event identity changed: %s", safe)
	}
}

func TestSanitizeAPIErrorDoesNotTreatEveryForbiddenAsAuthentication(t *testing.T) {
	safe := SanitizeAPIError(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Type: "invalid_request_error", Code: "tool_forbidden", Message: "tool is not allowed"},
		StatusCode:  http.StatusForbidden,
	})
	if safe.Code != "tool_forbidden" || safe.ProviderAuthRejected {
		t.Fatalf("ordinary provider permission error was reclassified as account auth: %+v", safe)
	}
}
