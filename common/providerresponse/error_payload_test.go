package providerresponse

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"one-api/types"
)

func TestSanitizeErrorPayloadPreservesOrdinaryPayloadBytes(t *testing.T) {
	payload := []byte("{\n  \"type\": \"response.output_text.delta\",\n  \"delta\": \"a  b\\n\\nhttps://example.com\",\n  \"api_key\": \"model-authored-value\"\n}\n")

	if safe, _ := SanitizeErrorPayload(payload); !bytes.Equal(safe, payload) {
		t.Fatalf("ordinary provider payload changed:\nwant: %q\n got: %q", payload, safe)
	}
}

func TestSanitizeErrorPayloadPreservesEnvelopeAndHidesAccountFailure(t *testing.T) {
	payload := []byte(`{"type":"response.failed","sequence_number":7,"response":{"id":"resp_1","status":"failed","error":{"type":"usage_limit_reached","code":"insufficient_quota","message":"organization org-secret exhausted","param":"account"}},"account_id":"acct-secret"}`)
	safe, _ := SanitizeErrorPayload(payload)
	for _, secret := range []string{"org-secret"} {
		if strings.Contains(string(safe), secret) {
			t.Fatalf("provider account detail %q leaked from %s", secret, safe)
		}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(safe, &object); err != nil {
		t.Fatalf("safe payload is invalid JSON: %v", err)
	}
	if string(object["type"]) != `"response.failed"` || string(object["sequence_number"]) != "7" || !strings.Contains(string(object["response"]), `"id":"resp_1"`) || !strings.Contains(string(object["response"]), `"code":"insufficient_quota"`) {
		t.Fatalf("provider lifecycle envelope changed: %s", safe)
	}
}

func TestSanitizeErrorPayloadKeepsBusinessErrorAndRedactsIdentityFields(t *testing.T) {
	payload := []byte(`{"type":"error","sequence_number":3,"code":"bad_input","message":"invalid tool","account_id":"acct-secret","secret":"shared-secret","credential":"shared-credential"}`)
	safe, _ := SanitizeErrorPayload(payload)
	if !strings.Contains(string(safe), `"code":"bad_input"`) || !strings.Contains(string(safe), `"message":"invalid tool"`) || !strings.Contains(string(safe), `"account_id":"[redacted]"`) || strings.Contains(string(safe), "shared-secret") || strings.Contains(string(safe), "shared-credential") {
		t.Fatalf("business error semantics or redaction changed: %s", safe)
	}
}

func TestSanitizeAPIErrorRetainsControlClassification(t *testing.T) {
	safe := SanitizeAPIError(&types.OpenAIErrorWithStatusCode{
		OpenAIError:          types.OpenAIError{Type: "authentication_error", Code: "invalid_api_key", Message: "organization org-secret rejected"},
		StatusCode:           http.StatusUnauthorized,
		ProviderAuthRejected: true,
		RawBody:              []byte(`{"error":{"message":"organization org-secret rejected"}}`),
		ReplayRawResponse:    true,
	})
	if safe.Code != "invalid_api_key" || !safe.ProviderAuthRejected || strings.Contains(safe.Message, "org-secret") || len(safe.RawBody) == 0 || !safe.ReplayRawResponse {
		t.Fatalf("unexpected safe control error: %+v", safe)
	}
}

func TestSanitizeAPIErrorProtectsOpenErrorExtensions(t *testing.T) {
	apiErr := &types.OpenAIErrorWithStatusCode{StatusCode: 400, OpenAIError: types.OpenAIError{Message: "bad input", Param: "session", Code: map[string]any{"detail": "provider-secret"}, InnerError: map[string]any{"id_token": "private", "number": json.Number("9007199254740993")}}}
	safe := SanitizeAPIError(apiErr, "provider-secret")
	raw, err := json.Marshal(safe.OpenAIError)
	if err != nil || strings.Contains(string(raw), "private") || !strings.Contains(string(raw), "9007199254740993") || !strings.Contains(string(raw), "provider-secret") {
		t.Fatalf("错误扩展或协议字段错误: %s %v", raw, err)
	}
	if apiErr.InnerError.(map[string]any)["id_token"] != "private" {
		t.Fatal("原始诊断被修改")
	}
}
func TestSanitizeNestedRealtimeErrorDoesNotInventTopLevelFields(t *testing.T) {
	raw := `{"type":"error","event_id":"evt_1","error":{"type":"authentication_error","code":"invalid_api_key","message":"account org-secret rejected"}}`
	safe, _ := SanitizeErrorPayload([]byte(raw))
	want := strings.ReplaceAll(raw, "org-secret", "[redacted]")
	if string(safe) != want {
		t.Fatalf("错误 envelope 改变: %s", safe)
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

func TestSanitizeErrorFieldsPreservesProtocolValues(t *testing.T) {
	for _, test := range []struct {
		name string
		code any
	}{
		{"int", 403}, {"int64", int64(9007199254740993)}, {"float64", float64(403)},
		{"json number", json.Number("1e3")}, {"structured", map[string]any{"number": int64(9007199254740993), "detail": "provider-secret"}},
		{"raw JSON", json.RawMessage(`{"value":9007199254740993}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := types.OpenAIError{Code: test.code, Type: "provider-secret", Param: "provider-secret", Message: "account_id=private provider-secret", InnerError: map[string]any{"access_token": "inner-private"}}
			safe := SanitizeErrorFields(original, "provider-secret")
			if !reflect.DeepEqual(safe.Code, original.Code) || reflect.TypeOf(safe.Code) != reflect.TypeOf(original.Code) || safe.Type != original.Type || safe.Param != original.Param {
				t.Fatalf("协议字段被脱敏转换: before=%#v after=%#v", original, safe)
			}
			if strings.Contains(safe.Message, "private") || strings.Contains(safe.Message, "provider-secret") || safe.InnerError.(map[string]any)["access_token"] != "[redacted]" {
				t.Fatalf("诊断未处理: %+v", safe)
			}
			if original.Message != "account_id=private provider-secret" || original.InnerError.(map[string]any)["access_token"] != "inner-private" {
				t.Fatal("原始诊断被修改")
			}
		})
	}
}
