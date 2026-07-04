package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/relay/relay_util"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

type mainTestProvider struct {
	providersBase.BaseProvider
}

func (p *mainTestProvider) GetRequestHeaders() map[string]string {
	return nil
}

type retryProviderSetupFailureRelay struct {
	c        *gin.Context
	provider providersBase.ProviderInterface
	setupErr error
}

func (r *retryProviderSetupFailureRelay) send() (*types.OpenAIErrorWithStatusCode, bool) {
	return nil, false
}

func (r *retryProviderSetupFailureRelay) getPromptTokens() (int, error) {
	return 0, nil
}

func (r *retryProviderSetupFailureRelay) setRequest() error {
	return nil
}

func (r *retryProviderSetupFailureRelay) getRequest() any {
	return nil
}

func (r *retryProviderSetupFailureRelay) setProvider(string) error {
	return r.setupErr
}

func (r *retryProviderSetupFailureRelay) getProvider() providersBase.ProviderInterface {
	return r.provider
}

func (r *retryProviderSetupFailureRelay) getOriginalModel() string {
	return "gpt-test"
}

func (r *retryProviderSetupFailureRelay) getModelName() string {
	return "gpt-test"
}

func (r *retryProviderSetupFailureRelay) getContext() *gin.Context {
	return r.c
}

func (r *retryProviderSetupFailureRelay) IsStream() bool {
	return false
}

func (r *retryProviderSetupFailureRelay) GetFirstResponseTime() time.Time {
	return time.Time{}
}

func (r *retryProviderSetupFailureRelay) HandleJsonError(*types.OpenAIErrorWithStatusCode) {}

func (r *retryProviderSetupFailureRelay) HandleStreamError(*types.OpenAIErrorWithStatusCode) {}

func (r *retryProviderSetupFailureRelay) SetHeartbeat(bool) *relay_util.Heartbeat {
	return nil
}

func TestApplyPreMappingBeforeRequestReSelectsProviderWhenToolsAreInjected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	weight := uint(1)
	proxy := ""
	preAdd := `{"pre_add":true,"tools":[{"type":"function","function":{"name":"lookup_weather","description":"lookup weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`

	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{
			1: {
				Channel: &model.Channel{
					Id:              1,
					Type:            config.ChannelTypeOpenAI,
					Status:          config.ChannelStatusEnabled,
					Group:           "default",
					Models:          "gpt-4o",
					Weight:          &weight,
					Proxy:           &proxy,
					OnlyChat:        true,
					CustomParameter: &preAdd,
				},
			},
			2: {
				Channel: &model.Channel{
					Id:       2,
					Type:     config.ChannelTypeOpenAI,
					Status:   config.ChannelStatusEnabled,
					Group:    "default",
					Models:   "gpt-4o",
					Weight:   &weight,
					Proxy:    &proxy,
					OnlyChat: false,
				},
			},
		},
		Rule: map[string]map[string][][]int{
			"default": {
				"gpt-4o": {{1}, {2}},
			},
		},
		ModelGroup: map[string]map[string]bool{
			"gpt-4o": {
				"default": true,
			},
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("token_group", "default")

	relay := NewRelayChat(ctx)
	applyPreMappingBeforeRequest(ctx)

	if !ctx.GetBool("skip_only_chat") {
		t.Fatal("expected pre-mapping to refresh skip_only_chat after injected tools are applied")
	}

	if err := relay.setRequest(); err != nil {
		t.Fatalf("setRequest failed: %v", err)
	}

	ctx.Set("is_stream", relay.IsStream())
	if relay.chatRequest.Tools == nil {
		t.Fatal("expected injected tools to be visible before provider re-selection")
	}

	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		t.Fatalf("setProvider failed: %v", err)
	}

	if got := relay.getProvider().GetChannel().Id; got != 2 {
		t.Fatalf("expected provider to be re-selected onto non-OnlyChat channel, got %d", got)
	}

	if !common.GetRequestBodyReparseNeeded(ctx) {
		t.Fatal("expected provider re-selection to request a body reparse")
	}

	if err := reparseRequestAfterProviderSelection(relay); err != nil {
		t.Fatalf("reparseRequestAfterProviderSelection failed: %v", err)
	}

	if relay.chatRequest.Tools != nil {
		t.Fatalf("expected reparsed request to drop injected tools, got %#v", relay.chatRequest.Tools)
	}
	if ctx.GetBool("skip_only_chat") {
		t.Fatal("expected skip_only_chat to be refreshed from the re-selected provider body")
	}

	requestMap, err := common.CloneReusableBodyMap(ctx)
	if err != nil {
		t.Fatalf("CloneReusableBodyMap failed: %v", err)
	}
	if _, exists := requestMap["tools"]; exists {
		encoded, _ := json.Marshal(requestMap["tools"])
		t.Fatalf("expected request body to be rebuilt from the original payload, got tools=%s", encoded)
	}
}

func TestExecuteRelayAttemptsHandlesResponsesContinuationMissOutsideHealthFailurePath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	seedCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	seedCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	seedCtx.Set("token_id", 909)
	seedCtx.Set("token_group", "default")
	prepareResponsesChannelAffinity(seedCtx, &types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-continuation-miss",
	})
	recordResponsesChannelAffinity(seedCtx, 41, &types.OpenAIResponsesResponses{
		ID:             "resp_continuation_miss",
		Model:          "gpt-5",
		Object:         "response",
		Status:         "completed",
		PromptCacheKey: "pc-continuation-miss",
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 909)
	ctx.Set("token_group", "default")

	request := types.OpenAIResponsesRequest{
		Model:              "gpt-5",
		PromptCacheKey:     "pc-continuation-miss",
		PreviousResponseID: "resp_continuation_miss",
	}
	prepareResponsesChannelAffinity(ctx, &request)

	relay := &relayResponses{
		relayBase: relayBase{
			c: ctx,
			provider: &affinityResponsesProvider{
				BaseProvider: providersBase.BaseProvider{
					Channel:         &model.Channel{Id: 41, Name: "responses-primary", Type: config.ChannelTypeOpenAI},
					SupportResponse: true,
				},
			},
			modelName: "gpt-5",
		},
		responsesRequest: request,
		operation:        responsesOperationCreate,
	}

	originalRelayHandler := relayHandlerFunc
	originalProcessChannelRelayError := processChannelRelayErrorFunc
	originalShouldRetry := shouldRetryFunc
	originalShouldCooldowns := shouldCooldownsFunc
	t.Cleanup(func() {
		relayHandlerFunc = originalRelayHandler
		processChannelRelayErrorFunc = originalProcessChannelRelayError
		shouldRetryFunc = originalShouldRetry
		shouldCooldownsFunc = originalShouldCooldowns
	})

	handlerCalls := 0
	processCalls := 0
	retryChecks := 0
	cooldownCalls := 0
	relayHandlerFunc = func(relay RelayBaseInterface) (*types.OpenAIErrorWithStatusCode, bool) {
		handlerCalls++
		return &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{
				Code:    "previous_response_not_found",
				Message: "previous response not found",
				Type:    "invalid_request_error",
			},
			StatusCode: http.StatusNotFound,
		}, false
	}
	processChannelRelayErrorFunc = func(_ context.Context, _ int, _ string, _ *types.OpenAIErrorWithStatusCode, _ int) {
		processCalls++
	}
	shouldRetryFunc = func(_ *gin.Context, _ *types.OpenAIErrorWithStatusCode, _ int) bool {
		retryChecks++
		return true
	}
	shouldCooldownsFunc = func(_ *gin.Context, _ *model.Channel, _ *types.OpenAIErrorWithStatusCode) {
		cooldownCalls++
	}

	apiErr := executeRelayAttempts(relay)
	if apiErr == nil {
		t.Fatal("expected executeRelayAttempts to surface an explicit continuation miss error")
	}
	if apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("expected continuation miss to map to 409 conflict, got %d", apiErr.StatusCode)
	}
	if apiErr.Code != "previous_response_not_found" {
		t.Fatalf("expected continuation miss to preserve previous_response_not_found code, got %#v", apiErr.Code)
	}
	if !apiErr.LocalError {
		t.Fatal("expected continuation miss error to be marked as local")
	}
	if handlerCalls != 1 {
		t.Fatalf("expected continuation miss handling to stop after one attempt, got %d handler calls", handlerCalls)
	}
	if processCalls != 0 {
		t.Fatalf("expected continuation miss not to hit processChannelRelayError, got %d calls", processCalls)
	}
	if retryChecks != 0 {
		t.Fatalf("expected continuation miss not to evaluate ordinary retry logic, got %d calls", retryChecks)
	}
	if cooldownCalls != 0 {
		t.Fatalf("expected continuation miss not to trigger cooldowns, got %d calls", cooldownCalls)
	}
	if relay.responsesRequest.PreviousResponseID != "resp_continuation_miss" {
		t.Fatalf("expected continuation miss handling not to clear previous_response_id, got %q", relay.responsesRequest.PreviousResponseID)
	}
	if ctx.GetBool(responsesPreviousResponseRecoveredContextKey) {
		t.Fatal("expected continuation miss handling not to mark the request as recovered")
	}

	lookupCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	lookupCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	lookupCtx.Set("token_id", 909)
	lookupCtx.Set("token_group", "default")
	prepareResponsesChannelAffinity(lookupCtx, &types.OpenAIResponsesRequest{
		Model:              "gpt-5",
		PromptCacheKey:     "pc-continuation-miss",
		PreviousResponseID: "resp_continuation_miss",
	})
	if got := currentPreferredChannelID(lookupCtx); got != 0 {
		t.Fatalf("expected continuation miss cleanup to clear all request bindings, got preferred channel %d", got)
	}
	if _, ok := lookupChannelAffinity(lookupCtx, channelAffinityKindResponses, "resp_continuation_miss"); ok {
		t.Fatal("expected previous_response_id binding to be cleared after continuation miss")
	}
	if _, ok := lookupChannelAffinity(lookupCtx, channelAffinityKindResponses, "pc-continuation-miss"); ok {
		t.Fatal("expected prompt_cache_key binding to be cleared after continuation miss")
	}

	meta := currentChannelAffinityLogMeta(ctx)
	if meta["responses_continuation_miss"] != true {
		t.Fatalf("expected continuation miss meta to be recorded, got %#v", meta)
	}
	if meta["responses_continuation_recovery_candidate"] != true {
		t.Fatalf("expected recovery candidate meta to be recorded, got %#v", meta)
	}
	if meta["responses_continuation_recovery_strategy"] != "manual_replay_required" {
		t.Fatalf("expected manual replay recovery strategy meta, got %#v", meta)
	}
}

func TestExecuteRelayAttemptsReturnsRetryProviderSetupError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("requestStartTime", time.Now())

	relay := &retryProviderSetupFailureRelay{
		c: ctx,
		provider: &mainTestProvider{BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 17, Name: "primary", Type: config.ChannelTypeOpenAI},
		}},
		setupErr: errors.New("provider selection unavailable"),
	}

	originalRelayHandler := relayHandlerFunc
	originalProcessChannelRelayError := processChannelRelayErrorFunc
	originalShouldRetry := shouldRetryFunc
	originalShouldCooldowns := shouldCooldownsFunc
	originalRetryTimes := config.RetryTimes
	originalRetryTimeout := config.RetryTimeOut
	t.Cleanup(func() {
		relayHandlerFunc = originalRelayHandler
		processChannelRelayErrorFunc = originalProcessChannelRelayError
		shouldRetryFunc = originalShouldRetry
		shouldCooldownsFunc = originalShouldCooldowns
		config.RetryTimes = originalRetryTimes
		config.RetryTimeOut = originalRetryTimeout
	})

	config.RetryTimes = 1
	config.RetryTimeOut = 60
	relayHandlerFunc = func(RelayBaseInterface) (*types.OpenAIErrorWithStatusCode, bool) {
		return common.StringErrorWrapperLocal("upstream unavailable", "provider_upstream_failed", http.StatusBadGateway), false
	}
	processChannelRelayErrorFunc = func(context.Context, int, string, *types.OpenAIErrorWithStatusCode, int) {}
	shouldRetryFunc = func(*gin.Context, *types.OpenAIErrorWithStatusCode, int) bool {
		return true
	}
	shouldCooldownsFunc = func(*gin.Context, *model.Channel, *types.OpenAIErrorWithStatusCode) {}

	apiErr := executeRelayAttempts(relay)
	if apiErr == nil {
		t.Fatal("expected retry provider setup failure to be returned")
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable || apiErr.Code != "one_hub_error" {
		t.Fatalf("expected 503 one_hub_error from retry provider setup, got status=%d code=%#v err=%+v", apiErr.StatusCode, apiErr.Code, apiErr)
	}
	if !strings.Contains(apiErr.Message, "provider selection unavailable") {
		t.Fatalf("expected provider setup error message, got %q", apiErr.Message)
	}
}

func TestExecuteRelayAttemptsStopsAfterQuotaRollbackFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("requestStartTime", time.Now())

	relay := &retryProviderSetupFailureRelay{
		c: ctx,
		provider: &mainTestProvider{BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 19, Name: "primary", Type: config.ChannelTypeOpenAI},
		}},
		setupErr: errors.New("retry should not select another provider"),
	}

	originalRelayHandler := relayHandlerFunc
	originalProcessChannelRelayError := processChannelRelayErrorFunc
	originalShouldRetry := shouldRetryFunc
	originalShouldCooldowns := shouldCooldownsFunc
	originalRetryTimes := config.RetryTimes
	originalRetryTimeout := config.RetryTimeOut
	t.Cleanup(func() {
		relayHandlerFunc = originalRelayHandler
		processChannelRelayErrorFunc = originalProcessChannelRelayError
		shouldRetryFunc = originalShouldRetry
		shouldCooldownsFunc = originalShouldCooldowns
		config.RetryTimes = originalRetryTimes
		config.RetryTimeOut = originalRetryTimeout
	})

	config.RetryTimes = 1
	config.RetryTimeOut = 60
	handlerCalls := 0
	retryChecks := 0
	relayHandlerFunc = func(RelayBaseInterface) (*types.OpenAIErrorWithStatusCode, bool) {
		handlerCalls++
		return common.StringErrorWrapperLocal("quota rollback failed", "quota_rollback_failed", http.StatusInternalServerError), true
	}
	processChannelRelayErrorFunc = func(context.Context, int, string, *types.OpenAIErrorWithStatusCode, int) {}
	shouldRetryFunc = func(*gin.Context, *types.OpenAIErrorWithStatusCode, int) bool {
		retryChecks++
		return true
	}
	shouldCooldownsFunc = func(*gin.Context, *model.Channel, *types.OpenAIErrorWithStatusCode) {}

	apiErr := executeRelayAttempts(relay)
	if apiErr == nil {
		t.Fatal("expected quota rollback failure to be returned")
	}
	if apiErr.Code != "quota_rollback_failed" || !apiErr.LocalError {
		t.Fatalf("expected local quota_rollback_failed error, got %+v", apiErr)
	}
	if handlerCalls != 1 {
		t.Fatalf("expected rollback failure to stop after one attempt, got %d handler calls", handlerCalls)
	}
	if retryChecks != 0 {
		t.Fatalf("expected rollback failure done=true to skip retry checks, got %d", retryChecks)
	}
}
