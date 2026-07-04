package codex

import (
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/requestctx"
	"one-api/providers/codex/wire"
	"one-api/types"
)

func TestChatAdapterResolvesTemperatureTopPConflict(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)

	temperature := 0.7
	topP := 0.9
	chatRequest := &types.ChatCompletionRequest{
		Model:       "gpt-5",
		Temperature: &temperature,
		TopP:        &topP,
		Messages: []types.ChatCompletionMessage{
			{Role: types.ChatMessageRoleUser, Content: "hello"},
		},
	}

	converted := provider.chatToResponsesRequest(chatRequest)
	if converted.Temperature == nil || *converted.Temperature != temperature {
		t.Fatalf("expected temperature preserved, got %+v", converted.Temperature)
	}
	if converted.TopP != nil {
		t.Fatalf("expected top_p dropped at the adapter boundary, got %v", *converted.TopP)
	}

	// The synthesized body must satisfy the planner's reject rules: the
	// planner applies the same rules to every raw body, no relaxation branch.
	rawReq, errWithCode := provider.chatResponsesRequestFromTyped(converted)
	if errWithCode != nil {
		t.Fatalf("chatResponsesRequestFromTyped returned error: %v", errWithCode.Message)
	}
	if _, err := wire.PlanResponsesCreateBody(rawReq.Body.Object, wire.CreateBodyInput{
		Model:  "gpt-5",
		Stream: true,
	}); err != nil {
		t.Fatalf("expected synthesized body to pass planner reject rules, got %v", err)
	}
}

func TestChatAdapterKeepsSoleSamplingParameter(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)

	topP := 0.9
	converted := provider.chatToResponsesRequest(&types.ChatCompletionRequest{
		Model: "gpt-5",
		TopP:  &topP,
		Messages: []types.ChatCompletionMessage{
			{Role: types.ChatMessageRoleUser, Content: "hello"},
		},
	})
	if converted.TopP == nil || *converted.TopP != topP {
		t.Fatalf("expected sole top_p preserved, got %+v", converted.TopP)
	}
}

func TestCodexOfficialChannelPolicyRejectsModelHeaders(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)

	for _, empty := range []string{"", "  ", "{}", "null"} {
		value := empty
		provider.Channel.ModelHeaders = &value
		if _, err := provider.codexOfficialChannelPolicy(); err != nil {
			t.Fatalf("expected empty model_headers %q to be accepted, got %v", empty, err)
		}
	}

	stale := `{"User-Agent":"custom"}`
	provider.Channel.ModelHeaders = &stale
	_, err := provider.codexOfficialChannelPolicy()
	if err == nil || !strings.Contains(err.Error(), "model_headers") {
		t.Fatalf("expected model_headers to be rejected, got %v", err)
	}
}

func TestCodexOfficialChannelPolicyValidatesResidency(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"codex":{"residency":" us-east:fedramp "}}`, nil)
	policy, err := provider.codexOfficialChannelPolicy()
	if err != nil {
		t.Fatalf("expected valid residency, got %v", err)
	}
	if policy.Residency != "us-east:fedramp" {
		t.Fatalf("expected residency to be trimmed, got %q", policy.Residency)
	}

	provider = newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"codex":{"residency":"bad value"}}`, nil)
	if _, err := provider.codexOfficialChannelPolicy(); err == nil || !strings.Contains(err.Error(), "residency") {
		t.Fatalf("expected invalid residency rejection, got %v", err)
	}
}

func TestCodexOfficialChannelPolicyParsesBooleanFields(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"codex":{"fedramp":true,"trust_client_attestation":true,"auto_generate":{"session_id":true,"thread_id":true,"client_request_id":true,"installation_id":true,"ws_stream_request_start_ms":true}}}`, nil)
	policy, err := provider.codexOfficialChannelPolicy()
	if err != nil {
		t.Fatalf("expected boolean policy fields to parse, got %v", err)
	}
	if !policy.FedRAMP || !policy.TrustClientAttestation {
		t.Fatalf("unexpected boolean policy values: %+v", policy)
	}
	if !policy.AutoGenerate.SessionID || !policy.AutoGenerate.ThreadID || !policy.AutoGenerate.ClientRequestID || !policy.AutoGenerate.InstallationID || !policy.AutoGenerate.WSStreamRequestStartMS {
		t.Fatalf("expected all auto_generate fields to parse true, got %+v", policy.AutoGenerate)
	}

	provider = newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"codex":{"trust_client_attestation":"false"}}`, nil)
	if _, err := provider.codexOfficialChannelPolicy(); err == nil || !strings.Contains(err.Error(), "trust_client_attestation") {
		t.Fatalf("expected invalid boolean policy field to be rejected, got %v", err)
	}

	provider = newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"codex":{"auto_generate":{"installation_id":"true"}}}`, nil)
	if _, err := provider.codexOfficialChannelPolicy(); err == nil || !strings.Contains(err.Error(), "auto_generate.installation_id") {
		t.Fatalf("expected invalid auto_generate boolean field to be rejected, got %v", err)
	}
}

func TestCodexOfficialChannelPolicyDefaultsDoNotAutoGenerateIdentity(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	policy, err := provider.codexOfficialChannelPolicy()
	if err != nil {
		t.Fatalf("expected default policy to parse, got %v", err)
	}
	if policy.AutoGenerate.SessionID || policy.AutoGenerate.ThreadID || policy.AutoGenerate.ClientRequestID || policy.AutoGenerate.InstallationID || policy.AutoGenerate.WSStreamRequestStartMS {
		t.Fatalf("expected default policy to leave identity generation disabled, got %+v", policy.AutoGenerate)
	}
}

func TestCodexOfficialChannelPolicyCacheInvalidatesOnChannelConfigChange(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"codex":{"residency":"us"}}`, nil)
	policy, err := provider.codexOfficialChannelPolicy()
	if err != nil {
		t.Fatalf("expected initial policy to parse, got %v", err)
	}
	if policy.Residency != "us" {
		t.Fatalf("expected initial residency, got %q", policy.Residency)
	}

	provider.Channel.Other = `{"codex":{"residency":"eu"}}`
	policy, err = provider.codexOfficialChannelPolicy()
	if err != nil {
		t.Fatalf("expected changed policy to parse, got %v", err)
	}
	if policy.Residency != "eu" {
		t.Fatalf("expected policy cache to reparse changed channel config, got %q", policy.Residency)
	}
}

func TestCodexPrincipalFingerprintRequiresDedicatedSecret(t *testing.T) {
	originalIdentity := config.CodexIdentitySecret
	t.Cleanup(func() {
		config.CodexIdentitySecret = originalIdentity
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	principal := requestctx.Principal{Kind: "api_key", StableID: "42"}

	config.CodexIdentitySecret = ""
	if _, err := provider.codexPrincipalFingerprint(principal); err == nil || !strings.Contains(err.Error(), "codex_identity_secret") {
		t.Fatalf("expected missing codex identity secret to fail, got %v", err)
	}

	config.CodexIdentitySecret = "dedicated-identity-secret-a"
	first, err := provider.codexPrincipalFingerprint(principal)
	if err != nil {
		t.Fatalf("expected dedicated identity secret to produce fingerprint, got %v", err)
	}
	config.CodexIdentitySecret = "dedicated-identity-secret-b"
	second, err := provider.codexPrincipalFingerprint(principal)
	if err != nil {
		t.Fatalf("expected changed dedicated identity secret to produce fingerprint, got %v", err)
	}
	if first.HMAC == "" || second.HMAC == "" || first.HMAC == second.HMAC {
		t.Fatalf("expected fingerprint to depend on dedicated identity secret, first=%q second=%q", first.HMAC, second.HMAC)
	}
}
