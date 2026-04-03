package authutil

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestParseCredentialNormalizesPrefixesAndSelectors(t *testing.T) {
	credential := ParseCredential(" bearer \tsk-example-token#12#ignore ")

	if credential.Value != "example-token" {
		t.Fatalf("expected canonical credential value, got %q", credential.Value)
	}
	if !reflect.DeepEqual(credential.SelectorParts, []string{"12", "ignore"}) {
		t.Fatalf("expected selector parts to be preserved, got %#v", credential.SelectorParts)
	}
}

func TestParseCredentialTreatsOpenAIKeyPrefixCaseInsensitively(t *testing.T) {
	credential := ParseCredential("Bearer SK-Upper-Token")
	if credential.Value != "Upper-Token" {
		t.Fatalf("expected case-insensitive sk- normalization, got %q", credential.Value)
	}
}

func TestParseCredentialRejectsEmptyAndPrefixOnlyInput(t *testing.T) {
	testCases := []string{"", "   ", "sk-", "Bearer sk-"}
	for _, raw := range testCases {
		if credential := ParseCredential(raw); !credential.Empty() {
			t.Fatalf("expected %q to normalize to an empty credential, got %#v", raw, credential)
		}
	}
}

func TestExtractStableRequestCredentialSupportsOpenAIWebsocketProtocol(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/realtime", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.sk-websocket-token#42#ignore")

	credential := ExtractStableRequestCredential(req)
	if credential.Value != "websocket-token" {
		t.Fatalf("expected websocket credential canonical value, got %q", credential.Value)
	}
	if !reflect.DeepEqual(credential.SelectorParts, []string{"42", "ignore"}) {
		t.Fatalf("expected websocket selector parts, got %#v", credential.SelectorParts)
	}
}

func TestExtractStableRequestCredentialPrefersExplicitHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer sk-primary-token")
	req.Header.Set("x-api-key", "sk-secondary-token")
	req.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.sk-websocket-token")

	credential := ExtractStableRequestCredential(req)
	if credential.Value != "primary-token" {
		t.Fatalf("expected authorization header to win, got %q", credential.Value)
	}
}

func TestExtractStableRequestCredentialRejectsOpenAIWebsocketProtocolOnPlainHTTP(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.sk-websocket-token#42#ignore")

	credential := ExtractStableRequestCredential(req)
	if !credential.Empty() {
		t.Fatalf("expected plain HTTP request to ignore websocket credential, got %#v", credential)
	}
}

func TestExtractOpenAIRequestCredentialRejectsOpenAIWebsocketProtocolOnPlainHTTP(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.sk-websocket-token")

	credential := ExtractOpenAIRequestCredential(req)
	if !credential.Empty() {
		t.Fatalf("expected plain HTTP request auth to ignore websocket credential, got %#v", credential)
	}
}

func TestExtractOpenAIRequestCredentialSupportsOpenAIWebsocketProtocolOnUpgrade(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/realtime", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.sk-websocket-token#7#shadow")

	credential := ExtractOpenAIRequestCredential(req)
	if credential.Value != "websocket-token" {
		t.Fatalf("expected websocket upgrade to accept subprotocol credential, got %q", credential.Value)
	}
	if !reflect.DeepEqual(credential.SelectorParts, []string{"7", "shadow"}) {
		t.Fatalf("expected websocket selectors to be preserved, got %#v", credential.SelectorParts)
	}
}

func TestStableRequestCredentialNamespaceUsesCanonicalCredential(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer sk-primary-token")

	if got := StableRequestCredentialNamespace(req); got != "auth:130aff3c5e27db20" {
		t.Fatalf("expected stable credential namespace, got %q", got)
	}
}

func TestCredentialHelperEdgeBranches(t *testing.T) {
	var nilReq *http.Request
	if credential := ExtractOpenAIWebsocketCredential(nilReq); !credential.Empty() {
		t.Fatalf("expected nil websocket request to return an empty credential, got %#v", credential)
	}
	if credential := extractRequestCredential(nilReq, []string{"Authorization"}, true); !credential.Empty() {
		t.Fatalf("expected nil request credential extraction to return empty credential, got %#v", credential)
	}

	req := httptest.NewRequest("GET", "/v1/realtime", nil)
	req.Header.Add("Sec-WebSocket-Protocol", "realtime, ignored")
	if credential := ExtractOpenAIWebsocketCredential(req); !credential.Empty() {
		t.Fatalf("expected websocket protocols without OpenAI credential prefix to be ignored, got %#v", credential)
	}

	if got := trimBearerPrefix("Bearer"); got != "" {
		t.Fatalf("expected a standalone Bearer prefix to collapse to empty, got %q", got)
	}
	if got := trimBearerPrefix("BearerXYZ"); got != "BearerXYZ" {
		t.Fatalf("expected non-space Bearer prefix continuation to be preserved, got %q", got)
	}
	if got := trimInsensitivePrefix("token", "sk-"); got != "token" {
		t.Fatalf("expected unmatched insensitive prefix trim to preserve input, got %q", got)
	}
	if !isASCIISpace('\t') {
		t.Fatal("expected tab to classify as ASCII space")
	}
	if isASCIISpace('x') {
		t.Fatal("expected non-space byte not to classify as ASCII space")
	}
}
