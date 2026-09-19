package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/requestctx"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
	"one-api/internal/requesthints"
	"one-api/providers/codex/wire"
	runtimerealtime "one-api/runtime/realtime"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestCodexOfficialChannelPolicyValidatesDefaultOriginator(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"codex":{"default_originator":" codex_cli_rs.test-1 "}}`, nil)
	policy, err := provider.codexOfficialChannelPolicy()
	if err != nil {
		t.Fatalf("expected valid default_originator, got %v", err)
	}
	if policy.DefaultOriginator != "codex_cli_rs.test-1" {
		t.Fatalf("expected trimmed default_originator, got %q", policy.DefaultOriginator)
	}

	provider = newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"codex":{"default_originator":"bad\r\nvalue"}}`, nil)
	_, err = provider.codexOfficialChannelPolicy()
	if err == nil || !strings.Contains(err.Error(), "default_originator") {
		t.Fatalf("expected invalid default_originator rejection, got %v", err)
	}
}

func TestCodexResponsesUsageAndBillingHelpers(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
	})

	if cloned := cloneCodexExtraBilling(nil); cloned != nil {
		t.Fatalf("expected nil extra billing clone, got %+v", cloned)
	}

	billing := map[string]types.ExtraBilling{
		types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high"): {
			ServiceType: types.APIToolTypeWebSearchPreview,
			Type:        "high",
			CallCount:   1,
		},
	}
	cloned := cloneCodexExtraBilling(billing)
	cloned[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")] = types.ExtraBilling{ServiceType: types.APIToolTypeWebSearchPreview, Type: "high", CallCount: 99}
	if billing[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected cloned extra billing to be detached from source, got %+v", billing)
	}

	target := &types.Usage{PromptTokens: 1}
	resolved := &types.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8}
	applyResolvedCodexUsage(target, resolved)
	if target.PromptTokens != 3 || target.TotalTokens != 8 {
		t.Fatalf("expected resolved usage to replace counters: %+v", target)
	}

	if _, ok := commonresponses.ParseStreamUsageEvent([]byte(`{"type":"response.done"}`)); ok {
		t.Fatal("expected Realtime response.done not to be tracked by the Responses usage parser")
	}
	if _, ok := commonresponses.ParseStreamUsageEvent([]byte(`{"type":"response.updated"}`)); ok {
		t.Fatal("expected unsupported response.updated event to be ignored by shared usage parser")
	}

	response := &types.OpenAIResponsesResponses{
		Output: []types.ResponsesOutput{
			{
				Type:    types.InputTypeMessage,
				Role:    types.ChatMessageRoleAssistant,
				Content: []types.ContentResponses{{Type: types.ContentTypeOutputText, Text: "hello"}},
			},
			{Type: types.InputTypeWebSearchCall, ID: "ws_1", Status: "completed", Action: map[string]any{"type": "search"}},
		},
		Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}},
	}

	seed := &types.Usage{PromptTokens: 7}
	accumulator := newCodexTurnUsageAccumulator()
	if err := accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: "response.completed", Response: response}); err != nil {
		t.Fatalf("observe terminal billing: %v", err)
	}
	resolvedUsage := resolveCodexResponsesUsage(seed, accumulator, response)
	if resolvedUsage == nil || resolvedUsage.PromptTokens != 7 || resolvedUsage.CompletionTokens != 0 || resolvedUsage.TotalTokens != 7 || resolvedUsage.ProviderReported {
		t.Fatalf("provider-missing content became authoritative token usage: %+v", resolvedUsage)
	}
	if resolvedUsage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected resolved usage to preserve extra billing, got %+v", resolvedUsage.ExtraBilling)
	}

	finalUsage := &types.Usage{PromptTokens: 3}
	if err := finalizeCodexResponsesUsage(finalUsage, &types.OpenAIResponsesResponses{
		Usage: &types.ResponsesUsage{InputTokens: 2, OutputTokens: 4, TotalTokens: 6},
	}); err != nil {
		t.Fatalf("finalize Codex usage: %v", err)
	}
	if finalUsage.PromptTokens != 2 || finalUsage.CompletionTokens != 4 {
		t.Fatalf("expected finalizeCodexResponsesUsage to overwrite counters: %+v", finalUsage)
	}
}

func TestCodexWireErrorSeparatesValidationFromInternalErrors(t *testing.T) {
	apiErr := codexWireError(&wire.Violation{Param: "session-id", Message: "secret detail"})
	if apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || apiErr.Param != "session-id" || strings.Contains(apiErr.Message, "secret detail") {
		t.Fatalf("expected sanitized 400 validation error, got %+v", apiErr)
	}

	apiErr = codexWireError(errors.New("planner bug"))
	if apiErr == nil || apiErr.StatusCode != http.StatusInternalServerError || apiErr.Code != "internal_server_error" {
		t.Fatalf("expected internal wire error to map to 500, got %+v", apiErr)
	}
}

func TestPrepareResponsesOfficialHTTPRequestErrorBranches(t *testing.T) {
	newRawReq := func(t *testing.T, body string, headers map[string]string) *commonresponses.Request {
		t.Helper()
		envelope, err := commonresponses.ParseRawEnvelope([]byte(body))
		if err != nil {
			t.Fatalf("parse raw envelope: %v", err)
		}
		httpHeaders := http.Header{}
		for key, value := range headers {
			httpHeaders.Set(key, value)
		}
		return &commonresponses.Request{
			Operation: commonresponses.ResponsesCreate,
			Headers:   requestctx.NewHeaderSnapshot(httpHeaders),
			Body:      envelope,
			ChannelID: 424299,
			Model:     "gpt-5",
		}
	}

	body := []byte(`{"model":"gpt-5","input":"hello","stream":true}`)

	t.Run("metadata validation error", func(t *testing.T) {
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
		req := newRawReq(t, `{"model":"gpt-5","client_metadata":null}`, nil)
		_, errWithCode := provider.prepareResponsesOfficialHTTPRequest(context.Background(), req, wire.OpResponsesCreate, "", "gpt-5", body)
		if errWithCode == nil || errWithCode.StatusCode != http.StatusBadRequest || errWithCode.Param != "client_metadata" {
			t.Fatalf("expected sanitized metadata validation error, got %+v", errWithCode)
		}
	})

	t.Run("channel policy error", func(t *testing.T) {
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
		modelHeaders := `{"User-Agent":"custom"}`
		provider.Channel.ModelHeaders = &modelHeaders
		req := newRawReq(t, `{"model":"gpt-5","input":"hello"}`, nil)
		_, errWithCode := provider.prepareResponsesOfficialHTTPRequest(context.Background(), req, wire.OpResponsesCreate, "", "gpt-5", body)
		if errWithCode == nil || errWithCode.StatusCode != http.StatusServiceUnavailable || errWithCode.Code != "channel_config_error" {
			t.Fatalf("expected channel config error, got %+v", errWithCode)
		}
	})

	t.Run("missing codex identity secret", func(t *testing.T) {
		original := config.CodexIdentitySecret
		config.CodexIdentitySecret = ""
		t.Cleanup(func() {
			config.CodexIdentitySecret = original
		})

		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"codex":{"auto_generate":{"installation_id":true}}}`, nil)
		req := newRawReq(t, `{"model":"gpt-5","input":"hello"}`, nil)
		_, errWithCode := provider.prepareResponsesOfficialHTTPRequest(context.Background(), req, wire.OpResponsesCreate, "", "gpt-5", body)
		if errWithCode == nil || errWithCode.StatusCode != http.StatusServiceUnavailable || errWithCode.Code != "channel_config_error" || !strings.Contains(errWithCode.Message, "codex_identity_secret") {
			t.Fatalf("expected missing codex identity secret channel config error, got %+v", errWithCode)
		}
	})

	t.Run("token error", func(t *testing.T) {
		provider := newTestCodexProviderWithContext(t, `{}`, "", nil)
		req := newRawReq(t, `{"model":"gpt-5","input":"hello"}`, nil)
		_, errWithCode := provider.prepareResponsesOfficialHTTPRequest(context.Background(), req, wire.OpResponsesCreate, "", "gpt-5", body)
		if errWithCode == nil || errWithCode.StatusCode != http.StatusServiceUnavailable || !errWithCode.LocalError || errWithCode.Code != "codex_token_error" {
			t.Fatalf("expected token error, got %+v", errWithCode)
		}
	})

	t.Run("identity validation error", func(t *testing.T) {
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
		req := newRawReq(t, `{"model":"gpt-5","input":"hello"}`, map[string]string{"x-oai-attestation": "abc.def"})
		_, errWithCode := provider.prepareResponsesOfficialHTTPRequest(context.Background(), req, wire.OpResponsesCreate, "", "gpt-5", body)
		if errWithCode == nil || errWithCode.StatusCode != http.StatusBadRequest || errWithCode.Param != "x-oai-attestation" || strings.Contains(errWithCode.Message, "trusted") {
			t.Fatalf("expected sanitized identity validation error, got %+v", errWithCode)
		}
	})

	t.Run("header plan error", func(t *testing.T) {
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
		req := newRawReq(t, `{"model":"gpt-5","input":"hello"}`, nil)
		_, errWithCode := provider.prepareResponsesOfficialHTTPRequest(context.Background(), req, wire.Operation("bad-operation"), "", "gpt-5", body)
		if errWithCode == nil || errWithCode.StatusCode != http.StatusBadRequest || errWithCode.Param != "operation" {
			t.Fatalf("expected header plan operation error, got %+v", errWithCode)
		}
	})

	t.Run("requester missing", func(t *testing.T) {
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
		provider.Requester = nil
		req := newRawReq(t, `{"model":"gpt-5","input":"hello"}`, nil)
		_, errWithCode := provider.prepareResponsesOfficialHTTPRequest(context.Background(), req, wire.OpResponsesCreate, "", "gpt-5", body)
		if errWithCode == nil || errWithCode.StatusCode != http.StatusServiceUnavailable || errWithCode.Code != "channel_error" {
			t.Fatalf("expected requester missing error, got %+v", errWithCode)
		}
	})

	t.Run("new request error", func(t *testing.T) {
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
		badBaseURL := "http://[::1"
		provider.Channel.BaseURL = &badBaseURL
		req := newRawReq(t, `{"model":"gpt-5","input":"hello"}`, nil)
		_, errWithCode := provider.prepareResponsesOfficialHTTPRequest(context.Background(), req, wire.OpResponsesCreate, "", "gpt-5", body)
		if errWithCode == nil || errWithCode.StatusCode != http.StatusInternalServerError || errWithCode.Code != "new_request_failed" {
			t.Fatalf("expected new request error, got %+v", errWithCode)
		}
	})
}

func TestPrepareResponsesCreateRequestProjectsMultiAgentBetaFromRawBody(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	for _, tc := range []struct {
		name     string
		body     string
		wantBeta string
	}{
		{name: "absent", body: `{"model":"gpt-5.6","input":"hello"}`},
		{name: "disabled", body: `{"model":"gpt-5.6","input":"hello","multi_agent":{"enabled":false}}`},
		{name: "enabled", body: `{"model":"gpt-5.6","input":"hello","multi_agent":{"enabled":true,"max_concurrent_subagents":3}}`, wantBeta: "responses_multi_agent=v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope, err := commonresponses.ParseRawEnvelope([]byte(tc.body))
			if err != nil {
				t.Fatalf("parse raw request: %v", err)
			}
			req := &commonresponses.Request{
				Operation: commonresponses.ResponsesCreate,
				Body:      envelope,
				ChannelID: provider.Channel.Id,
				Model:     "gpt-5.6",
			}
			httpReq, errWithCode := provider.prepareResponsesCreateRequest(context.Background(), req)
			if errWithCode != nil {
				t.Fatalf("prepare Responses request: %+v", errWithCode)
			}
			if got := httpReq.Header.Get("OpenAI-Beta"); got != tc.wantBeta {
				t.Fatalf("expected OpenAI-Beta %q, got %q", tc.wantBeta, got)
			}
		})
	}
}

func TestCodexStaleResponsesWSContinuationErrorIncludesEventID(t *testing.T) {
	err := codexStaleResponsesWSContinuationError("evt_stale")
	payload := runtimerealtime.ClientPayloadFromError(err)
	if !strings.Contains(string(payload), `"event_id":"evt_stale"`) {
		t.Fatalf("expected stale continuation payload to include event_id, got %s", payload)
	}
	if !runtimerealtime.ClientPayloadErrorIsRecoverable(err) || responsesws.ClientPayloadFromError(err) != nil {
		t.Fatal("expected stale continuation to use the recoverable realtime transport carrier only")
	}
}

func TestCodexResponsesPromptCacheHelpers(t *testing.T) {
	response := &types.OpenAIResponsesResponses{}
	backfillCodexResponsePromptCacheKey(response, &types.OpenAIResponsesRequest{PromptCacheKey: "stable"})
	if response.PromptCacheKey != "stable" {
		t.Fatalf("expected response prompt cache key backfill, got %q", response.PromptCacheKey)
	}
	backfillCodexResponsePromptCacheKey(response, &types.OpenAIResponsesRequest{PromptCacheKey: "ignored"})
	if response.PromptCacheKey != "stable" {
		t.Fatalf("expected existing response prompt cache key to win, got %q", response.PromptCacheKey)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("Authorization", "Bearer sk-test-auth-header")
	ctx.Request.Header.Set("X-Session-Id", "session-xyz")
	ctx.Request.Header.Set("Session_Id", "legacy-session")
	ctx.Set("token_id", int32(42))
	ctx.Set("id", int64(7))
	ctx.Set("float_id", float64(9))
	ctx.Set("string_id", " 11 ")
	ctx.Set("bad_id", "nan")

	if got, ok := codexContextInt(ctx, "token_id"); !ok || got != 42 {
		t.Fatalf("expected int32 context coercion, got %d ok=%v", got, ok)
	}
	if got, ok := codexContextInt(ctx, "id"); !ok || got != 7 {
		t.Fatalf("expected int64 context coercion, got %d ok=%v", got, ok)
	}
	if got, ok := codexContextInt(ctx, "float_id"); !ok || got != 9 {
		t.Fatalf("expected float64 context coercion, got %d ok=%v", got, ok)
	}
	if got, ok := codexContextInt(ctx, "string_id"); !ok || got != 11 {
		t.Fatalf("expected string context coercion, got %d ok=%v", got, ok)
	}
	if _, ok := codexContextInt(ctx, "bad_id"); ok {
		t.Fatal("expected invalid numeric string to fail coercion")
	}
	if _, ok := codexContextInt(ctx, "missing_id"); ok {
		t.Fatal("expected missing context key to fail coercion")
	}

	if got := normalizePromptCacheStrategy(""); got != codexPromptCacheStrategyOff {
		t.Fatalf("expected blank strategy normalization, got %q", got)
	}
	if got := normalizePromptCacheStrategy("AUTO"); got != codexPromptCacheStrategyAuto {
		t.Fatalf("expected auto strategy normalization, got %q", got)
	}
	if got := normalizePromptCacheStrategy(" session_id "); got != codexPromptCacheStrategySessionID {
		t.Fatalf("expected session-id strategy normalization, got %q", got)
	}
	if got := normalizePromptCacheStrategy("weird"); got != codexPromptCacheStrategyOff {
		t.Fatalf("expected unknown strategy fallback, got %q", got)
	}
	if got := codexPromptCacheIdentity(nil, codexPromptCacheStrategyAuto); got != "" {
		t.Fatalf("expected nil context prompt cache identity to be empty, got %q", got)
	}
	if got := codexPromptCacheIdentity(ctx, codexPromptCacheStrategyOff); got != "" {
		t.Fatalf("expected off strategy to disable prompt cache identity, got %q", got)
	}
	if got := codexPromptCacheIdentity(ctx, codexPromptCacheStrategySessionID); got != "one-hub:codex:prompt-cache:session:session-xyz" {
		t.Fatalf("expected session-id prompt cache identity, got %q", got)
	}
	if got := codexPromptCacheIdentity(ctx, codexPromptCacheStrategyTokenID); got != "one-hub:codex:prompt-cache:token:42" {
		t.Fatalf("expected token-id prompt cache identity, got %q", got)
	}
	if got := codexPromptCacheIdentity(ctx, codexPromptCacheStrategyUserID); got != "one-hub:codex:prompt-cache:user:7" {
		t.Fatalf("expected user-id prompt cache identity, got %q", got)
	}
	if got := codexPromptCacheIdentity(ctx, codexPromptCacheStrategyAuthHeader); got != "one-hub:codex:prompt-cache:auth:test-auth-header" {
		t.Fatalf("expected auth-header prompt cache identity, got %q", got)
	}
	if got := codexPromptCacheIdentity(ctx, codexPromptCacheStrategyAuto); got != "one-hub:codex:prompt-cache:session:session-xyz" {
		t.Fatalf("expected auto strategy to prefer session identity, got %q", got)
	}
	if got := promptCacheKeyForRequestStrategy(&types.OpenAIResponsesRequest{PreviousResponseID: "resp-auto-direct"}, ctx, codexPromptCacheStrategyAuto); got != "resp-auto-direct" {
		t.Fatalf("expected auto strategy to use previous_response_id directly, got %q", got)
	}

}

func TestCodexResponsesRoutingHintResolver(t *testing.T) {
	originalSettings := RoutingHintSettingsInstance
	RoutingHintSettingsInstance = RoutingHintSettings{
		PromptCacheKeyStrategy: codexPromptCacheStrategyAuto,
		ModelRegex:             "^gpt-5$",
		UserAgentRegex:         "CodexClient",
	}
	t.Cleanup(func() {
		RoutingHintSettingsInstance = originalSettings
	})

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("User-Agent", "CodexClient/1.0")
	ctx.Request.Header.Set("X-Session-Id", "hint-session")
	ctx.Set("token_id", 42)

	request := &types.OpenAIResponsesRequest{Model: "gpt-5"}
	hints := requesthints.ResolveResponses(ctx, request)
	expectedKey := promptCacheKeyForRequestStrategy(request, ctx, codexPromptCacheStrategyAuto)
	if got := hints[requesthints.ResponsesPromptCacheKey]; got != expectedKey {
		t.Fatalf("expected resolver to publish derived prompt cache key %q, got %#v", expectedKey, hints)
	}

	requesthints.Set(ctx, nil)
	previousRequest := &types.OpenAIResponsesRequest{Model: "gpt-5", PreviousResponseID: "resp_hint_direct"}
	if hints := requesthints.ResolveResponses(ctx, previousRequest); hints[requesthints.ResponsesPromptCacheKey] != "resp_hint_direct" {
		t.Fatalf("expected resolver to publish previous_response_id directly, got %#v", hints)
	}

	requesthints.Set(ctx, nil)
	request.PromptCacheKey = "client-key"
	if hints := requesthints.ResolveResponses(ctx, request); len(hints) != 0 {
		t.Fatalf("expected explicit prompt_cache_key to skip resolver, got %#v", hints)
	}
}

func TestCodexResponsesPolicyUsesRouteHint(t *testing.T) {
	envelope, err := commonresponses.ParseRawEnvelope([]byte(`{"model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse raw envelope: %v", err)
	}

	req := &commonresponses.Request{
		Body: envelope,
		Control: commonresponses.Control{
			DownstreamDialect: commonresponses.DownstreamResponses,
		},
		Policy: commonresponses.PolicyInput{
			PromptCache: &commonresponses.PromptCacheDecision{
				Key:    "pc-route-hint",
				Source: commonresponses.PromptCacheRouteHint,
			},
		},
	}
	policy := responsesPolicyInput(req)
	if policy.PromptCache == nil || policy.PromptCache.Key != "pc-route-hint" || policy.PromptCache.Source != commonresponses.PromptCacheRouteHint {
		t.Fatalf("expected route hint prompt-cache policy, got %+v", policy.PromptCache)
	}

	req.Policy = commonresponses.PolicyInput{}
	policy = responsesPolicyInput(req)
	if policy.PromptCache != nil || req.Policy.PromptCache != nil {
		t.Fatalf("expected provider policy reader not to synthesize or write back prompt cache, got policy=%+v req=%+v", policy.PromptCache, req.Policy.PromptCache)
	}
}

func TestCodexChannelProbeMetadataIsProviderOwned(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	rawReq, errWithCode := provider.rawResponsesRequestForTest(&types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hi"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("rawResponsesRequestForTest returned error: %v", errWithCode.Message)
	}

	httpReq, errWithCode := provider.prepareResponsesCreateRequest(context.Background(), rawReq)
	if errWithCode != nil {
		t.Fatalf("prepareResponsesCreateRequest returned error: %v", errWithCode.Message)
	}
	body := readPreparedCodexResponsesBody(t, httpReq)
	if _, ok := body["client_metadata"]; ok {
		t.Fatalf("expected ordinary Responses request not to receive probe metadata, got %#v", body["client_metadata"])
	}
	if got := httpReq.Header.Get("session-id"); got != "" {
		t.Fatalf("expected ordinary request not to synthesize session header, got %q", got)
	}

	rawReq.Control.Purpose = commonresponses.RequestPurposeChannelProbe
	httpReq, errWithCode = provider.prepareResponsesCreateRequest(context.Background(), rawReq)
	if errWithCode != nil {
		t.Fatalf("prepareResponsesCreateRequest returned error: %v", errWithCode.Message)
	}
	bodyBytes, probeBody := readPreparedCodexResponsesBodyBytes(t, httpReq)
	clientMetadata := requireStringMap(t, probeBody["client_metadata"])

	for _, key := range []string{"session_id", "thread_id", "turn_id", "x-codex-window-id", "x-codex-turn-metadata"} {
		if strings.TrimSpace(stringValue(clientMetadata[key])) == "" {
			t.Fatalf("expected probe client_metadata.%s to be set, metadata=%#v", key, clientMetadata)
		}
	}
	if _, exists := clientMetadata["x-codex-installation-id"]; exists {
		t.Fatalf("expected installation id to stay absent when policy does not auto-generate it, metadata=%#v", clientMetadata)
	}
	for _, forbidden := range []string{"probe_id", "channel_id", "channel_probe", "one_hub_probe"} {
		if strings.Contains(mustJSON(t, clientMetadata), forbidden) {
			t.Fatalf("expected probe metadata not to contain %q, got %#v", forbidden, clientMetadata)
		}
	}

	turnMetadata := parseTurnMetadata(t, stringValue(clientMetadata["x-codex-turn-metadata"]))
	if turnMetadata["request_kind"] != "turn" {
		t.Fatalf("expected request_kind turn, got %#v", turnMetadata)
	}
	if _, exists := turnMetadata["installation_id"]; exists {
		t.Fatalf("expected turn metadata not to force installation id, got %#v", turnMetadata)
	}
	if turnMetadata["session_id"] != clientMetadata["session_id"] || turnMetadata["thread_id"] != clientMetadata["thread_id"] || turnMetadata["turn_id"] != clientMetadata["turn_id"] || turnMetadata["window_id"] != clientMetadata["x-codex-window-id"] {
		t.Fatalf("expected flat metadata and turn metadata identities to match, flat=%#v turn=%#v", clientMetadata, turnMetadata)
	}
	if _, ok := turnMetadata["turn_started_at_unix_ms"].(float64); !ok {
		t.Fatalf("expected turn_started_at_unix_ms numeric field, got %#v", turnMetadata)
	}
	if httpReq.Header.Get("session-id") != stringValue(clientMetadata["session_id"]) || httpReq.Header.Get("thread-id") != stringValue(clientMetadata["thread_id"]) || httpReq.Header.Get("x-codex-window-id") != stringValue(clientMetadata["x-codex-window-id"]) || httpReq.Header.Get("x-codex-turn-metadata") != stringValue(clientMetadata["x-codex-turn-metadata"]) {
		t.Fatalf("expected headers to be built from injected body metadata, headers=%v metadata=%#v", httpReq.Header, clientMetadata)
	}
	if got := httpReq.Header.Get("x-codex-installation-id"); got != "" {
		t.Fatalf("expected installation header to stay absent when policy does not auto-generate it, got %q", got)
	}

	envelope, err := commonresponses.ParseRawEnvelope(bodyBytes)
	if err != nil {
		t.Fatalf("parse prepared body: %v", err)
	}
	metadata, err := wire.MetadataFromResponsesBody(envelope.Object)
	if err != nil {
		t.Fatalf("MetadataFromResponsesBody returned error: %v", err)
	}
	if got, state, err := metadata.String("session_id", nil); err != nil || state != wire.FieldPresent || got != stringValue(clientMetadata["session_id"]) {
		t.Fatalf("expected wire metadata reader to see injected session_id, got value=%q state=%v err=%v", got, state, err)
	}
}

func TestCodexChannelProbeMetadataRespectsInstallationPolicy(t *testing.T) {
	originalSecret := config.CodexIdentitySecret
	config.CodexIdentitySecret = "unit-test-secret"
	t.Cleanup(func() {
		config.CodexIdentitySecret = originalSecret
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"codex":{"auto_generate":{"installation_id":true}}}`, nil)
	provider.Context.Set("token_id", 123)
	rawReq, errWithCode := provider.rawResponsesRequestForTest(&types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hi"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("rawResponsesRequestForTest returned error: %v", errWithCode.Message)
	}
	rawReq.Control.Purpose = commonresponses.RequestPurposeChannelProbe

	httpReq, errWithCode := provider.prepareResponsesCreateRequest(context.Background(), rawReq)
	if errWithCode != nil {
		t.Fatalf("prepareResponsesCreateRequest returned error: %v", errWithCode.Message)
	}
	body := readPreparedCodexResponsesBody(t, httpReq)
	clientMetadata := requireStringMap(t, body["client_metadata"])
	installationID := stringValue(clientMetadata["x-codex-installation-id"])
	if installationID == "" {
		t.Fatalf("expected auto-generated installation id, metadata=%#v", clientMetadata)
	}
	if got := httpReq.Header.Get("x-codex-installation-id"); got != installationID {
		t.Fatalf("expected header installation id %q, got %q", installationID, got)
	}
	turnMetadata := parseTurnMetadata(t, stringValue(clientMetadata["x-codex-turn-metadata"]))
	if turnMetadata["installation_id"] != installationID {
		t.Fatalf("expected turn metadata installation id to match, turn=%#v flat=%#v", turnMetadata, clientMetadata)
	}
	if _, exists := body["prompt_cache_key"]; exists {
		t.Fatalf("expected probe metadata injection not to synthesize prompt_cache_key, body=%#v", body)
	}
}

func TestCodexChatChannelProbeCarriesProviderMetadata(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	typed := &types.OpenAIResponsesRequest{
		Model:       "gpt-5",
		ConvertChat: true,
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hi"},
				},
			},
		},
	}

	rawReq, errWithCode := provider.chatResponsesRequestFromTyped(typed)
	if errWithCode != nil {
		t.Fatalf("chatResponsesRequestFromTyped returned error: %v", errWithCode.Message)
	}
	httpReq, errWithCode := provider.prepareResponsesCreateRequest(provider.codexProviderContext(), rawReq)
	if errWithCode != nil {
		t.Fatalf("prepareResponsesCreateRequest returned error: %v", errWithCode.Message)
	}
	body := readPreparedCodexResponsesBody(t, httpReq)
	if _, ok := body["client_metadata"]; ok {
		t.Fatalf("expected ordinary chat adapter request not to receive probe metadata, got %#v", body["client_metadata"])
	}

	probeCtx := commonresponses.ContextWithRequestPurpose(provider.Context.Request.Context(), commonresponses.RequestPurposeChannelProbe)
	provider.Context.Request = provider.Context.Request.WithContext(probeCtx)
	rawReq, errWithCode = provider.chatResponsesRequestFromTyped(typed)
	if errWithCode != nil {
		t.Fatalf("chatResponsesRequestFromTyped returned error: %v", errWithCode.Message)
	}
	if rawReq.Control.Purpose != commonresponses.RequestPurposeChannelProbe {
		t.Fatalf("expected chat adapter to preserve channel probe purpose, got %+v", rawReq.Control)
	}
	httpReq, errWithCode = provider.prepareResponsesCreateRequest(provider.codexProviderContext(), rawReq)
	if errWithCode != nil {
		t.Fatalf("prepareResponsesCreateRequest returned error: %v", errWithCode.Message)
	}
	body = readPreparedCodexResponsesBody(t, httpReq)
	clientMetadata := requireStringMap(t, body["client_metadata"])
	for _, key := range []string{"session_id", "thread_id", "turn_id", "x-codex-window-id", "x-codex-turn-metadata"} {
		if strings.TrimSpace(stringValue(clientMetadata[key])) == "" {
			t.Fatalf("expected probe client_metadata.%s to be set, metadata=%#v", key, clientMetadata)
		}
	}
}

func readPreparedCodexResponsesBody(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	_, body := readPreparedCodexResponsesBodyBytes(t, req)
	return body
}

func readPreparedCodexResponsesBodyBytes(t *testing.T, req *http.Request) ([]byte, map[string]any) {
	t.Helper()
	if req == nil || req.Body == nil {
		t.Fatal("expected prepared HTTP request with body")
	}
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read prepared request body: %v", err)
	}
	if err := req.Body.Close(); err != nil {
		t.Fatalf("close prepared request body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("decode prepared request body %s: %v", string(bodyBytes), err)
	}
	return bodyBytes, body
}

func requireStringMap(t *testing.T, value any) map[string]any {
	t.Helper()
	out, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected JSON object, got %#v", value)
	}
	return out
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func parseTurnMetadata(t *testing.T, raw string) map[string]any {
	t.Helper()
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		t.Fatalf("decode x-codex-turn-metadata %q: %v", raw, err)
	}
	return metadata
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	return string(raw)
}

func TestCodexPromptCacheAutoPriorityFallsBackAcrossSignals(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newCtx := func(headers map[string]string) *gin.Context {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		for key, value := range headers {
			ctx.Request.Header.Set(key, value)
		}
		return ctx
	}

	sessionCtx := newCtx(map[string]string{
		"Authorization": "Bearer sk-auth-priority",
		"X-Session-Id":  "session-priority",
	})
	sessionCtx.Set("token_id", 11)
	sessionCtx.Set("id", 22)
	previousRequest := &types.OpenAIResponsesRequest{PreviousResponseID: "resp-priority"}
	if got := promptCacheKeyForRequestStrategy(previousRequest, sessionCtx, codexPromptCacheStrategyAuto); got != "resp-priority" {
		t.Fatalf("expected previous_response_id to win auto priority directly, got %q", got)
	}
	if got := promptCacheKeyForRequestStrategy(&types.OpenAIResponsesRequest{}, sessionCtx, codexPromptCacheStrategyAuto); got != uuid.NewSHA1(uuid.NameSpaceOID, []byte("one-hub:codex:prompt-cache:session:session-priority")).String() {
		t.Fatalf("expected session id to win auto priority when previous_response_id is absent, got %q", got)
	}

	authCtx := newCtx(map[string]string{
		"Authorization": "Bearer sk-auth-priority",
	})
	authCtx.Set("token_id", 11)
	authCtx.Set("id", 22)
	if got := codexPromptCacheIdentity(authCtx, codexPromptCacheStrategyAuto); got != "one-hub:codex:prompt-cache:auth:auth-priority" {
		t.Fatalf("expected auth header to win when session id is absent, got %q", got)
	}

	tokenCtx := newCtx(nil)
	tokenCtx.Set("token_id", 11)
	tokenCtx.Set("id", 22)
	if got := codexPromptCacheIdentity(tokenCtx, codexPromptCacheStrategyAuto); got != "one-hub:codex:prompt-cache:token:11" {
		t.Fatalf("expected token id to win when auth header is absent, got %q", got)
	}

	userCtx := newCtx(nil)
	userCtx.Set("id", 22)
	if got := codexPromptCacheIdentity(userCtx, codexPromptCacheStrategyAuto); got != "one-hub:codex:prompt-cache:user:22" {
		t.Fatalf("expected user id to win when token id is absent, got %q", got)
	}
}

func TestCodexResponsesStreamObserverHelpers(t *testing.T) {
	var nilHandler *CodexResponsesStreamHandler
	nilHandler.observeUsageEvent(`{"type":"response.created"}`)

	usage := &types.Usage{}
	handler := newCodexResponsesStreamHandler(usage)
	handler.observeUsageEvent("{bad-json")
	handler.observeUsageEvent(`{"type":"response.output_item.added","item":{"type":"web_search_call","id":"ws_1"},"response":{"tools":[{"type":"web_search_preview","search_context_size":"high"}]}}`)
	handler.observeUsageEvent(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8},"tools":[{"type":"web_search_preview","search_context_size":"high"}],"output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search"}}]}}`)

	if usage.TotalTokens != 8 || usage.PromptTokens != 3 || usage.CompletionTokens != 5 {
		t.Fatalf("expected terminal usage snapshot to be applied, got %+v", usage)
	}
	if usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected stream observer to preserve tool billing, got %+v", usage.ExtraBilling)
	}

}
