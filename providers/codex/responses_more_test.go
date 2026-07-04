package codex

import (
	"context"
	"errors"
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

	provider = newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"codex":{"default_originator":"bad value"}}`, nil)
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
	target.TextBuilder.WriteString("assistant transcript")
	resolved := &types.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8}
	applyResolvedCodexUsage(target, resolved)
	if target.PromptTokens != 3 || target.TotalTokens != 8 || target.TextBuilder.String() != "assistant transcript" {
		t.Fatalf("expected resolved usage to replace counters while preserving text, got %+v text=%q", target, target.TextBuilder.String())
	}

	if got := codexResponsesSearchType(nil); got != "" {
		t.Fatalf("expected nil search type to be empty, got %q", got)
	}
	if got := codexResponsesSearchType(&types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview}}}); got != "medium" {
		t.Fatalf("expected web search default search type, got %q", got)
	}
	if got := codexResponsesSearchType(&types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}}}); got != "high" {
		t.Fatalf("expected explicit web search type, got %q", got)
	}

	usage := &types.Usage{}
	applyCodexResponsesAddedToolBilling(usage, &types.ResponsesOutput{Type: types.InputTypeWebSearchCall}, "")
	applyCodexResponsesAddedToolBilling(usage, &types.ResponsesOutput{Type: types.InputTypeCodeInterpreterCall}, "")
	applyCodexResponsesAddedToolBilling(usage, &types.ResponsesOutput{Type: types.InputTypeFileSearchCall}, "")
	applyCodexResponsesAddedToolBilling(usage, &types.ResponsesOutput{Type: types.InputTypeImageGenerationCall, Quality: "high", Size: "1024x1024"}, "")
	if usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")].CallCount != 1 {
		t.Fatalf("expected web search preview billing, got %+v", usage.ExtraBilling)
	}
	if usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeCodeInterpreter, "")].CallCount != 1 {
		t.Fatalf("expected code interpreter billing, got %+v", usage.ExtraBilling)
	}
	if usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeFileSearch, "")].CallCount != 1 {
		t.Fatalf("expected file search billing, got %+v", usage.ExtraBilling)
	}
	if usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "high-1024x1024")].CallCount != 1 {
		t.Fatalf("expected image generation billing, got %+v", usage.ExtraBilling)
	}

	if _, ok := commonresponses.ParseStreamUsageEvent([]byte(`{"type":"response.done"}`)); !ok {
		t.Fatal("expected response.done to be tracked by shared usage parser")
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
			{Type: types.InputTypeWebSearchCall, ID: "ws_1", Status: "completed"},
		},
		Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}},
	}

	seed := &types.Usage{PromptTokens: 7}
	seed.TextBuilder.WriteString("seed transcript")
	resolvedUsage := resolveCodexResponsesUsage(seed, nil, response, "gpt-5", true)
	if resolvedUsage == nil || resolvedUsage.PromptTokens != 7 || resolvedUsage.CompletionTokens <= 0 || resolvedUsage.TotalTokens <= resolvedUsage.PromptTokens {
		t.Fatalf("expected resolved usage to backfill prompt/output tokens, got %+v", resolvedUsage)
	}
	if resolvedUsage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected resolved usage to preserve extra billing, got %+v", resolvedUsage.ExtraBilling)
	}

	finalUsage := &types.Usage{PromptTokens: 3}
	finalUsage.TextBuilder.WriteString("final transcript")
	finalizeCodexResponsesUsage(finalUsage, &types.OpenAIResponsesResponses{
		Usage: &types.ResponsesUsage{InputTokens: 2, OutputTokens: 4, TotalTokens: 6},
	}, "gpt-5", false)
	if finalUsage.PromptTokens != 2 || finalUsage.CompletionTokens != 4 || finalUsage.TextBuilder.String() != "final transcript" {
		t.Fatalf("expected finalizeCodexResponsesUsage to overwrite counters and keep text, got %+v text=%q", finalUsage, finalUsage.TextBuilder.String())
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
		if errWithCode == nil || errWithCode.StatusCode != http.StatusUnauthorized || errWithCode.Code != "codex_token_error" {
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

func TestCodexStaleResponsesWSContinuationErrorIncludesEventID(t *testing.T) {
	err := codexStaleResponsesWSContinuationError("evt_stale")
	payload := responsesws.ClientPayloadFromError(err)
	if !strings.Contains(string(payload), `"event_id":"evt_stale"`) {
		t.Fatalf("expected stale continuation payload to include event_id, got %s", payload)
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

	stableKeyRequest := &types.OpenAIResponsesRequest{}
	ensureStablePromptCacheKey(stableKeyRequest, ctx, codexPromptCacheStrategyUserID)
	expectedStableKey := uuid.NewSHA1(uuid.NameSpaceOID, []byte("one-hub:codex:prompt-cache:user:7")).String()
	if stableKeyRequest.PromptCacheKey != expectedStableKey {
		t.Fatalf("expected generated stable prompt cache key %q, got %q", expectedStableKey, stableKeyRequest.PromptCacheKey)
	}
	ensureStablePromptCacheKey(stableKeyRequest, ctx, codexPromptCacheStrategyOff)
	if stableKeyRequest.PromptCacheKey != expectedStableKey {
		t.Fatalf("expected existing prompt cache key to remain stable, got %q", stableKeyRequest.PromptCacheKey)
	}

	derivedKeyRequest := &types.OpenAIResponsesRequest{}
	requesthints.Set(ctx, map[string]string{requesthints.ResponsesPromptCacheKey: "derived-prompt-cache"})
	ensureStablePromptCacheKey(derivedKeyRequest, ctx, codexPromptCacheStrategyOff)
	if derivedKeyRequest.PromptCacheKey != "derived-prompt-cache" {
		t.Fatalf("expected derived prompt cache key from relay context to win, got %q", derivedKeyRequest.PromptCacheKey)
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
	nilHandler.observeUsageEvent(`{"type":"response.output_text.delta","delta":"hello"}`)

	usage := &types.Usage{}
	handler := newCodexResponsesStreamHandler(usage)
	handler.observeUsageEvent("{bad-json")
	handler.observeUsageEvent(`{"type":"response.output_text.delta","delta":"hello"}`)
	handler.observeUsageEvent(`{"type":"response.reasoning_summary_text.delta","delta":" summary"}`)
	handler.observeUsageEvent(`{"type":"response.output_item.added","item":{"type":"web_search_call","id":"ws_1"},"response":{"tools":[{"type":"web_search_preview","search_context_size":"high"}]}}`)
	handler.observeUsageEvent(`{"type":"response.done","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8},"tools":[{"type":"web_search_preview","search_context_size":"high"}],"output":[{"type":"web_search_call","id":"ws_1","status":"completed"}]}}`)

	if usage.TextBuilder.String() != "hello summary" {
		t.Fatalf("expected stream observer to accumulate text deltas, got %q", usage.TextBuilder.String())
	}
	if usage.TotalTokens != 8 || usage.PromptTokens != 3 || usage.CompletionTokens != 5 {
		t.Fatalf("expected terminal usage snapshot to be applied, got %+v", usage)
	}
	if usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected stream observer to preserve tool billing, got %+v", usage.ExtraBilling)
	}

}
