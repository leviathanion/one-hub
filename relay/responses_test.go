package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func responsesTestRawEnvelope(t *testing.T, request types.OpenAIResponsesRequest) *commonresponses.RawEnvelope {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal responses test request: %v", err)
	}
	envelope, err := commonresponses.ParseRawEnvelope(raw)
	if err != nil {
		t.Fatalf("parse responses test raw envelope: %v", err)
	}
	return envelope
}

type affinityResponsesProvider struct {
	providersBase.BaseProvider
}

func (p *affinityResponsesProvider) GetRequestHeaders() map[string]string {
	return map[string]string{}
}

func (p *affinityResponsesProvider) CreateResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return &types.OpenAIResponsesResponses{
		ID:     "resp_123",
		Model:  "gpt-5",
		Object: "response",
		Status: "completed",
	}, nil
}

func (p *affinityResponsesProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *affinityResponsesProvider) CompactResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return &types.OpenAIResponsesResponses{}, nil
}

type compactRejectProvider struct {
	providersBase.BaseProvider
	compactCalled bool
}

type compactSuccessProvider struct {
	providersBase.BaseProvider
	response *types.OpenAIResponsesResponses
}

type stalePreviousResponseProvider struct {
	providersBase.BaseProvider
	createCalls int
}

type streamAffinityResponsesProvider struct {
	providersBase.BaseProvider
	stream requester.StreamReaderInterface[string]
}

func (p *streamAffinityResponsesProvider) GetRequestHeaders() map[string]string {
	return map[string]string{}
}

func (p *streamAffinityResponsesProvider) CreateResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *streamAffinityResponsesProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return p.stream, nil
}

func (p *streamAffinityResponsesProvider) CompactResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

type compatibleStreamChatProvider struct {
	providersBase.BaseProvider
	stream requester.StreamReaderInterface[string]
}

type compatibleResponsesChatProvider struct {
	providersBase.BaseProvider
	response          *types.ChatCompletionResponse
	createCalls       int
	createStreamCalls int
}

type chatFallbackResponsesProvider struct {
	providersBase.BaseProvider
	request *commonresponses.Request
}

func (p *compatibleStreamChatProvider) GetRequestHeaders() map[string]string {
	return map[string]string{}
}

func (p *compatibleStreamChatProvider) CreateChatCompletion(*types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *compatibleStreamChatProvider) CreateChatCompletionStream(*types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return p.stream, nil
}

func (p *compatibleResponsesChatProvider) GetRequestHeaders() map[string]string {
	return map[string]string{}
}

func (p *compatibleResponsesChatProvider) CreateChatCompletion(*types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	p.createCalls++
	return p.response, nil
}

func (p *compatibleResponsesChatProvider) CreateChatCompletionStream(*types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	p.createStreamCalls++
	return nil, nil
}

func (p *chatFallbackResponsesProvider) GetRequestHeaders() map[string]string {
	return map[string]string{}
}

func (p *chatFallbackResponsesProvider) CreateResponses(_ context.Context, request *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	p.request = request
	return &types.OpenAIResponsesResponses{
		ID:     "resp_mapped",
		Model:  request.Model,
		Object: "response",
		Status: types.ResponseStatusCompleted,
		Output: []types.ResponsesOutput{},
		Usage:  &types.ResponsesUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	}, nil
}

func (p *chatFallbackResponsesProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *chatFallbackResponsesProvider) CompactResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *compactRejectProvider) GetRequestHeaders() map[string]string {
	return map[string]string{}
}

func (p *compactRejectProvider) CreateResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *compactRejectProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *compactRejectProvider) CompactResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	p.compactCalled = true
	return &types.OpenAIResponsesResponses{}, nil
}

func (p *compactSuccessProvider) GetRequestHeaders() map[string]string {
	return map[string]string{}
}

func (p *compactSuccessProvider) CreateResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *compactSuccessProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *compactSuccessProvider) CompactResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return p.response, nil
}

func (p *stalePreviousResponseProvider) GetRequestHeaders() map[string]string {
	return map[string]string{}
}

func (p *stalePreviousResponseProvider) CreateResponses(_ context.Context, req *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	p.createCalls++
	request := &types.OpenAIResponsesRequest{}
	if req != nil && req.Body != nil {
		projection := req.Body.Projection
		request = &projection
	}
	if strings.TrimSpace(request.PreviousResponseID) != "" {
		return nil, &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{
				Code:    "previous_response_not_found",
				Message: "previous response not found",
				Type:    "invalid_request_error",
			},
			StatusCode: http.StatusNotFound,
		}
	}
	return &types.OpenAIResponsesResponses{
		ID:             "resp_recovered",
		Model:          request.Model,
		Object:         "response",
		Status:         "completed",
		PromptCacheKey: request.PromptCacheKey,
	}, nil
}

func (p *stalePreviousResponseProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *stalePreviousResponseProvider) CompactResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func TestRelayResponsesCompactRejectsStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)

	provider := &compactRejectProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel:         &model.Channel{},
			SupportResponse: true,
		},
	}

	relay := &relayResponses{
		relayBase: relayBase{
			c:         ctx,
			provider:  provider,
			modelName: "gpt-5",
		},
		responsesRequest: types.OpenAIResponsesRequest{
			Model:  "gpt-5",
			Stream: true,
		},
		operation: responsesOperationCompact,
	}

	errWithCode, done := relay.send()
	if !done {
		t.Fatal("expected compact relay to stop on invalid stream request")
	}
	if errWithCode == nil {
		t.Fatal("expected compact relay to return an error")
	}
	if errWithCode.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected bad request status, got %d", errWithCode.StatusCode)
	}
	if provider.compactCalled {
		t.Fatal("expected provider compact call to be skipped")
	}
}

func TestRelayResponsesNativeRequiresRawEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)

	provider := &compactRejectProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel:         &model.Channel{},
			SupportResponse: true,
		},
	}
	relay := &relayResponses{
		relayBase: relayBase{
			c:         ctx,
			provider:  provider,
			modelName: "gpt-5",
		},
		responsesRequest: types.OpenAIResponsesRequest{Model: "gpt-5"},
		operation:        responsesOperationCompact,
	}

	errWithCode, done := relay.send()
	if !done {
		t.Fatal("expected native responses relay to stop without raw envelope")
	}
	if errWithCode == nil || errWithCode.StatusCode != http.StatusBadRequest || openAIErrorCodeString(errWithCode.Code, "") != "invalid_request_error" {
		t.Fatalf("expected missing raw envelope to fail as invalid request, got %+v", errWithCode)
	}
	if provider.compactCalled {
		t.Fatal("expected provider compact call to be skipped without raw envelope")
	}
}

func TestResponsesHTTPAttemptReplayPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	apiErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Type: "rate_limit_error", Code: "rate_limit_exceeded"},
		StatusCode:  http.StatusTooManyRequests,
	}

	newRelay := func() (*relayResponses, *httptest.ResponseRecorder) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		return &relayResponses{
			relayBase:        relayBase{c: ctx},
			responsesRequest: types.OpenAIResponsesRequest{Model: "gpt-5"},
			operation:        responsesOperationCreate,
		}, recorder
	}

	responsesRelay, _ := newRelay()
	if !responsesHTTPAttemptShouldRetry(responsesRelay, apiErr, config.ChannelTypeOpenAI) {
		t.Fatal("expected first-turn pre-commit 429 to retry")
	}

	responsesRelay, _ = newRelay()
	responsesRelay.responsesRequest.PreviousResponseID = "resp_previous"
	if responsesHTTPAttemptShouldRetry(responsesRelay, apiErr, config.ChannelTypeOpenAI) {
		t.Fatal("expected previous_response_id continuation to block HTTP attempt replay")
	}

	responsesRelay, _ = newRelay()
	responsesRelay.c.Set(channelAffinityPreferredChannelContextKey, 17)
	responsesRelay.c.Set(channelAffinitySelectedPreferredContextKey, true)
	responsesRelay.c.Set(channelAffinitySkipRetryContextKey, true)
	if responsesHTTPAttemptShouldRetry(responsesRelay, apiErr, config.ChannelTypeOpenAI) {
		t.Fatal("expected preferred skip-retry affinity to block HTTP attempt replay")
	}

	responsesRelay, _ = newRelay()
	responsesRelay.c.Set("specific_channel_id", 17)
	if responsesHTTPAttemptShouldRetry(responsesRelay, apiErr, config.ChannelTypeOpenAI) {
		t.Fatal("expected explicit channel pin to block HTTP attempt replay")
	}
	if responsesHTTPAttemptShouldRetry(responsesRelay, &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Message: "Your credit balance is too low"},
		StatusCode:  http.StatusBadRequest,
	}, config.ChannelTypeAnthropic) {
		t.Fatal("expected explicit channel pin to block channel-specific HTTP 400 retry")
	}

	responsesRelay, _ = newRelay()
	responsesRelay.c.Writer.WriteHeader(http.StatusOK)
	responsesRelay.c.Writer.WriteHeaderNow()
	if responsesHTTPAttemptShouldRetry(responsesRelay, apiErr, config.ChannelTypeOpenAI) {
		t.Fatal("expected committed downstream response to block HTTP attempt replay")
	}

	responsesRelay, _ = newRelay()
	if responsesHTTPAttemptShouldRetry(responsesRelay, common.StringErrorWrapperLocal("quota rollback failed", "quota_rollback_failed", http.StatusInternalServerError), config.ChannelTypeOpenAI) {
		t.Fatal("expected local quota rollback failure to block HTTP attempt replay")
	}
}

func TestRelayChatResponsesFallbackUsesMappedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	provider := &chatFallbackResponsesProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 71, Type: config.ChannelTypeOpenAI},
		},
	}
	relay := &relayChat{
		relayBase: relayBase{
			c:         ctx,
			provider:  provider,
			modelName: "o3-pro",
		},
		chatRequest: types.ChatCompletionRequest{
			Model: "client-o3-alias",
			Messages: []types.ChatCompletionMessage{
				{Role: types.ChatMessageRoleUser, Content: "hello"},
			},
		},
	}

	errWithCode, done := relay.send()
	if done || errWithCode != nil {
		t.Fatalf("expected chat fallback send to succeed, done=%v err=%v", done, errWithCode)
	}
	if provider.request == nil {
		t.Fatal("expected fallback provider to receive a Responses request")
	}
	if provider.request.Model != "o3-pro" {
		t.Fatalf("expected commonresponses request model to use mapped model, got %q", provider.request.Model)
	}
	if provider.request.Body == nil || provider.request.Body.Object == nil {
		t.Fatal("expected raw responses envelope")
	}
	if rawModel := strings.TrimSpace(string(provider.request.Body.Object.Fields["model"])); rawModel != `"o3-pro"` {
		t.Fatalf("expected raw envelope model to use mapped model, got %s", rawModel)
	}
	if !strings.Contains(string(provider.request.Body.Object.Raw), `"model":"o3-pro"`) {
		t.Fatalf("expected serialized fallback body to use mapped model, got %s", provider.request.Body.Object.Raw)
	}
	if relay.chatRequest.Model != "o3-pro" {
		t.Fatalf("expected relay chat request to carry mapped model for send operation, got %q", relay.chatRequest.Model)
	}
}

func TestPrepareResponsesChannelAffinityPrefersRecordedChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 99)

	request := &types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-responses-hit",
	}

	prepareResponsesChannelAffinity(ctx, request)
	recordCurrentChannelAffinity(ctx, channelAffinityKindResponses, 9527)

	nextCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	nextCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	nextCtx.Set("token_id", 99)

	prepareResponsesChannelAffinity(nextCtx, request)

	if got := currentPreferredChannelID(nextCtx); got != 9527 {
		t.Fatalf("expected recorded responses affinity channel 9527, got %d", got)
	}
}

func TestRelayResponsesSendRecordsChannelAffinityOnSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 12345)

	request := types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-record-success",
	}
	prepareResponsesChannelAffinity(ctx, &request)

	provider := &affinityResponsesProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel:         &model.Channel{Id: 88},
			SupportResponse: true,
		},
	}

	relay := &relayResponses{
		relayBase: relayBase{
			c:         ctx,
			provider:  provider,
			modelName: "gpt-5",
		},
		responsesRequest: request,
		rawEnvelope:      responsesTestRawEnvelope(t, request),
		operation:        responsesOperationCreate,
	}

	errWithCode, done := relay.send()
	if done {
		t.Fatal("expected successful responses relay to keep processing")
	}
	if errWithCode != nil {
		t.Fatalf("expected success, got %v", errWithCode)
	}

	if got, ok := lookupChannelAffinity(ctx, channelAffinityKindResponses, request.PromptCacheKey); !ok || got != 88 {
		t.Fatalf("expected responses affinity to be recorded on channel 88, got channel=%d ok=%v", got, ok)
	}
}

func TestPrepareResponsesChannelAffinityUsesPreviousResponseIDBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 321)
	ctx.Set("token_group", "default")

	initialRequest := &types.OpenAIResponsesRequest{
		Model: "gpt-5",
	}
	prepareResponsesChannelAffinity(ctx, initialRequest)
	recordResponsesChannelAffinity(ctx, 77, &types.OpenAIResponsesResponses{
		ID:     "resp_prev_affinity",
		Model:  "gpt-5",
		Object: "response",
		Status: "completed",
	})

	nextCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	nextCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	nextCtx.Set("token_id", 321)
	nextCtx.Set("token_group", "default")

	nextRequest := &types.OpenAIResponsesRequest{
		Model:              "gpt-5",
		PreviousResponseID: "resp_prev_affinity",
	}
	prepareResponsesChannelAffinity(nextCtx, nextRequest)

	if got := currentPreferredChannelID(nextCtx); got != 77 {
		t.Fatalf("expected previous_response_id affinity to reuse channel 77, got %d", got)
	}
}

func TestPrepareResponsesChannelAffinitySkipsMismatchedResumeFingerprint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	initialCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	initialCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	initialCtx.Set("token_id", 322)
	initialCtx.Set("token_group", "default")

	initialRequest := &types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-model-bound",
	}
	prepareResponsesChannelAffinity(initialCtx, initialRequest)
	recordResponsesChannelAffinity(initialCtx, 78, &types.OpenAIResponsesResponses{
		ID:             "resp_model_bound",
		Model:          "gpt-5",
		Object:         "response",
		Status:         "completed",
		PromptCacheKey: "pc-model-bound",
	})

	nextCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	nextCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	nextCtx.Set("token_id", 322)
	nextCtx.Set("token_group", "default")

	nextRequest := &types.OpenAIResponsesRequest{
		Model:          "gpt-4.1",
		PromptCacheKey: "pc-model-bound",
	}
	prepareResponsesChannelAffinity(nextCtx, nextRequest)

	if got := currentPreferredChannelID(nextCtx); got != 0 {
		t.Fatalf("expected mismatched response model fingerprint to skip affinity hit, got %d", got)
	}
}

func TestRelayResponsesSendDoesNotRecoverStalePreviousResponseIDInternally(t *testing.T) {
	gin.SetMode(gin.TestMode)

	seedCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	seedCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	seedCtx.Set("token_id", 777)
	seedCtx.Set("token_group", "default")
	prepareResponsesChannelAffinity(seedCtx, &types.OpenAIResponsesRequest{Model: "gpt-5"})
	recordResponsesChannelAffinity(seedCtx, 41, &types.OpenAIResponsesResponses{
		ID:     "resp_stale",
		Model:  "gpt-5",
		Object: "response",
		Status: "completed",
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 777)
	ctx.Set("token_group", "default")

	request := types.OpenAIResponsesRequest{
		Model:              "gpt-5",
		PromptCacheKey:     "pc-recover-stale",
		PreviousResponseID: "resp_stale",
	}
	prepareResponsesChannelAffinity(ctx, &request)
	if got := currentPreferredChannelID(ctx); got != 41 {
		t.Fatalf("expected stale previous_response_id affinity to resolve before recovery, got %d", got)
	}

	initialProvider := &stalePreviousResponseProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel:         &model.Channel{Id: 41},
			SupportResponse: true,
		},
	}
	recoveredProvider := &stalePreviousResponseProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel:         &model.Channel{Id: 55},
			SupportResponse: true,
		},
	}
	ctx.Set("channel_id", recoveredProvider.GetChannel().Id)
	ctx.Set("channel_type", recoveredProvider.GetChannel().Type)
	cacheProviderSelection(ctx, "gpt-5", recoveredProvider, "gpt-5")

	relay := &relayResponses{
		relayBase: relayBase{
			c:         ctx,
			provider:  initialProvider,
			modelName: "gpt-5",
		},
		responsesRequest: request,
		rawEnvelope:      responsesTestRawEnvelope(t, request),
		operation:        responsesOperationCreate,
	}

	errWithCode, done := relay.send()
	if done {
		t.Fatal("expected provider-side stale previous_response_id errors to remain retryable")
	}
	if errWithCode == nil {
		t.Fatal("expected stale previous_response_id send to return the upstream error")
	}
	if initialProvider.createCalls != 1 {
		t.Fatalf("expected initial stale-affinity provider to be called exactly once, got %d calls", initialProvider.createCalls)
	}
	if recoveredProvider.createCalls != 0 {
		t.Fatalf("expected send() not to reroute onto the cached recovery provider, got %d calls", recoveredProvider.createCalls)
	}
	if relay.responsesRequest.PreviousResponseID != "resp_stale" {
		t.Fatalf("expected send() not to clear previous_response_id, got %q", relay.responsesRequest.PreviousResponseID)
	}
	if channel := relay.provider.GetChannel(); channel == nil || channel.Id != 41 {
		t.Fatalf("expected send() to keep the original provider channel 41, got %#v", channel)
	}
	if got, ok := lookupChannelAffinity(ctx, channelAffinityKindResponses, "pc-recover-stale"); ok && got == 55 {
		t.Fatalf("expected send() not to record recovered prompt_cache_key affinity, got channel=%d ok=%v", got, ok)
	}
	if ctx.GetBool(responsesPreviousResponseRecoveredContextKey) {
		t.Fatal("expected send() not to mark the request as recovered")
	}
}

func TestRelayResponsesClearStalePreviousResponseAffinityRemovesAllRequestBindings(t *testing.T) {
	gin.SetMode(gin.TestMode)

	seedCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	seedCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	seedCtx.Set("token_id", 888)
	seedCtx.Set("token_group", "default")
	prepareResponsesChannelAffinity(seedCtx, &types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-stale-bindings",
	})
	recordResponsesChannelAffinity(seedCtx, 41, &types.OpenAIResponsesResponses{
		ID:             "resp_stale_bindings",
		Model:          "gpt-5",
		Object:         "response",
		Status:         "completed",
		PromptCacheKey: "pc-stale-bindings",
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 888)
	ctx.Set("token_group", "default")

	request := types.OpenAIResponsesRequest{
		Model:              "gpt-5",
		PromptCacheKey:     "pc-stale-bindings",
		PreviousResponseID: "resp_stale_bindings",
	}
	prepareResponsesChannelAffinity(ctx, &request)
	if got := currentPreferredChannelID(ctx); got != 41 {
		t.Fatalf("expected stale affinity to resolve before cleanup, got %d", got)
	}

	relay := &relayResponses{
		relayBase:        relayBase{c: ctx},
		responsesRequest: request,
	}
	relay.clearStalePreviousResponseAffinity()

	staleLookupCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	staleLookupCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	staleLookupCtx.Set("token_id", 888)
	staleLookupCtx.Set("token_group", "default")
	prepareResponsesChannelAffinity(staleLookupCtx, &types.OpenAIResponsesRequest{
		Model:              "gpt-5",
		PromptCacheKey:     "pc-stale-bindings",
		PreviousResponseID: "resp_stale_bindings",
	})
	if got := currentPreferredChannelID(staleLookupCtx); got != 0 {
		t.Fatalf("expected stale previous_response_id and prompt_cache_key bindings to be cleared, got %d", got)
	}

	lookupCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	lookupCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	lookupCtx.Set("token_id", 888)
	lookupCtx.Set("token_group", "default")
	prepareResponsesChannelAffinity(lookupCtx, &types.OpenAIResponsesRequest{
		Model:              "gpt-5",
		PromptCacheKey:     "pc-stale-bindings",
		PreviousResponseID: "resp_stale_bindings",
	})
	if _, ok := lookupChannelAffinity(lookupCtx, channelAffinityKindResponses, "resp_stale_bindings"); ok {
		t.Fatal("expected previous_response_id binding to be deleted")
	}
	if _, ok := lookupChannelAffinity(lookupCtx, channelAffinityKindResponses, "pc-stale-bindings"); ok {
		t.Fatal("expected prompt_cache_key binding to be deleted together with the stale continuation key")
	}
	if relay.responsesRequest.PreviousResponseID != "resp_stale_bindings" {
		t.Fatalf("expected cleanup not to mutate previous_response_id, got %q", relay.responsesRequest.PreviousResponseID)
	}
}

func TestRelayResponsesStreamRecordsPreviousResponseIDAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 456)
	ctx.Set("token_group", "default")

	request := types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-stream-affinity",
		Stream:         true,
	}
	prepareResponsesChannelAffinity(ctx, &request)

	stream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	go func() {
		stream.dataChan <- "event: response.created\n"
		stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_stream_affinity\",\"object\":\"response\",\"model\":\"gpt-5\",\"prompt_cache_key\":\"pc-stream-affinity\",\"status\":\"in_progress\"}}\n"
		stream.dataChan <- "\n"
		stream.dataChan <- "event: response.completed\n"
		stream.dataChan <- "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_stream_affinity\",\"object\":\"response\",\"model\":\"gpt-5\",\"prompt_cache_key\":\"pc-stream-affinity\",\"status\":\"completed\"}}\n"
		stream.dataChan <- "\n"
		stream.dataChan <- "data: [DONE]\n"
		stream.errChan <- io.EOF
	}()

	provider := &streamAffinityResponsesProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel:         &model.Channel{Id: 66},
			SupportResponse: true,
		},
		stream: stream,
	}

	relay := &relayResponses{
		relayBase: relayBase{
			c:         ctx,
			provider:  provider,
			modelName: "gpt-5",
		},
		responsesRequest: request,
		rawEnvelope:      responsesTestRawEnvelope(t, request),
		operation:        responsesOperationCreate,
	}

	errWithCode, done := relay.send()
	if done {
		t.Fatal("expected successful stream relay to keep processing")
	}
	if errWithCode != nil {
		t.Fatalf("expected success, got %v", errWithCode)
	}

	nextCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	nextCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	nextCtx.Set("token_id", 456)
	nextCtx.Set("token_group", "default")

	nextRequest := &types.OpenAIResponsesRequest{
		Model:              "gpt-5",
		PreviousResponseID: "resp_stream_affinity",
	}
	prepareResponsesChannelAffinity(nextCtx, nextRequest)

	if got := currentPreferredChannelID(nextCtx); got != 66 {
		t.Fatalf("expected streamed previous_response_id affinity to reuse channel 66, got %d", got)
	}
}

func TestRelayResponsesCompatibleStreamRecordsPreviousResponseIDAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 654)
	ctx.Set("token_group", "default")

	request := types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-compatible-stream",
		Stream:         true,
	}
	prepareResponsesChannelAffinity(ctx, &request)

	stream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	go func() {
		stream.dataChan <- `{"id":"chatcmpl_stream_affinity","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`
		stream.dataChan <- `{"id":"chatcmpl_stream_affinity","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
		stream.errChan <- io.EOF
	}()

	provider := &compatibleStreamChatProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel:         &model.Channel{Id: 67, CompatibleResponse: true},
			SupportResponse: false,
		},
		stream: stream,
	}

	relay := &relayResponses{
		relayBase: relayBase{
			c:         ctx,
			provider:  provider,
			modelName: "gpt-5",
		},
		responsesRequest: request,
		operation:        responsesOperationCreate,
	}

	errWithCode, done := relay.send()
	if done {
		t.Fatal("expected compatible stream relay to keep processing")
	}
	if errWithCode != nil {
		t.Fatalf("expected success, got %v", errWithCode)
	}

	nextCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	nextCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	nextCtx.Set("token_id", 654)
	nextCtx.Set("token_group", "default")

	nextRequest := &types.OpenAIResponsesRequest{
		Model:              "gpt-5",
		PreviousResponseID: "chatcmpl_stream_affinity",
	}
	prepareResponsesChannelAffinity(nextCtx, nextRequest)

	if got := currentPreferredChannelID(nextCtx); got != 67 {
		t.Fatalf("expected compatible streamed previous_response_id affinity to reuse channel 67, got %d", got)
	}
}

func TestRelayResponsesHelperFunctionsAndCompatibleNonStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	relay := &relayResponses{
		relayBase: relayBase{c: ctx},
		responsesRequest: types.OpenAIResponsesRequest{
			Model: "gpt-5",
			Input: "hello",
		},
		operation: responsesOperationCreate,
	}

	if relay.getRequest() != &relay.responsesRequest {
		t.Fatal("expected getRequest to expose the current responses request")
	}
	if relay.IsStream() {
		t.Fatal("expected non-stream create operation not to be treated as stream")
	}
	relay.responsesRequest.Stream = true
	if !relay.IsStream() {
		t.Fatal("expected create operation with stream enabled to be treated as stream")
	}
	relay.operation = responsesOperationCompact
	if relay.IsStream() {
		t.Fatal("expected compact responses operations never to stream")
	}
	relay.operation = responsesOperationCreate
	relay.responsesRequest.Stream = false

	relay.provider = &affinityResponsesProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel:         &model.Channel{},
			SupportResponse: true,
		},
	}
	relay.modelName = "gpt-5"

	if detectResponsesOperation("/v1/responses") != responsesOperationCreate {
		t.Fatal("expected standard responses path to select create operation")
	}
	if detectResponsesOperation("/v1/responses/compact") != responsesOperationCompact {
		t.Fatal("expected compact responses path to select compact operation")
	}

	if shouldRecoverStalePreviousResponse(nil) {
		t.Fatal("expected nil stale previous response error not to trigger recovery")
	}
	if shouldRecoverStalePreviousResponse(&types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Message: " "}}) {
		t.Fatal("expected blank stale previous response message not to trigger recovery")
	}
	if !shouldRecoverStalePreviousResponse(&types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Message: "previous response was not found by upstream"}}) {
		t.Fatal("expected previous response not found message to trigger recovery")
	}
	if !shouldRecoverStalePreviousResponse(&types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Code: "previous_response_not_found"}}) {
		t.Fatal("expected previous_response_not_found code to trigger recovery")
	}

	if plan := relay.stalePreviousResponseHandlingPlan(&types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Code: "previous_response_not_found"}}); plan != nil {
		t.Fatal("expected stale previous response handling to require a previous_response_id")
	}
	relay.responsesRequest.PreviousResponseID = "resp-stale"
	plan := relay.stalePreviousResponseHandlingPlan(&types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Message: "previous response not found"}})
	if plan == nil {
		t.Fatal("expected stale previous response handling plan to be created")
	}
	if plan.clientError == nil || plan.clientError.StatusCode != http.StatusConflict {
		t.Fatalf("expected stale previous response plan to return an explicit conflict error, got %#v", plan.clientError)
	}
	if plan.recoveryCandidateMeta["responses_continuation_recovery_strategy"] != "manual_replay_required" {
		t.Fatalf("expected stale previous response plan to expose recovery candidate meta, got %#v", plan.recoveryCandidateMeta)
	}
	relay.clearStalePreviousResponseAffinity()
	if relay.responsesRequest.PreviousResponseID != "resp-stale" {
		t.Fatalf("expected stale previous response cleanup to keep previous_response_id intact, got %q", relay.responsesRequest.PreviousResponseID)
	}
	if ctx.GetBool(responsesPreviousResponseRecoveredContextKey) {
		t.Fatal("expected stale previous response cleanup not to tag the request as recovered")
	}

	compatRecorder := httptest.NewRecorder()
	compatCtx, _ := gin.CreateTestContext(compatRecorder)
	compatCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	compatCtx.Set("token_id", 999)
	compatCtx.Set("token_group", "default")

	compatRequest := types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		Input:          "hello",
		PromptCacheKey: "pc-compatible-non-stream",
	}
	prepareResponsesChannelAffinity(compatCtx, &compatRequest)

	provider := &compatibleResponsesChatProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 77},
		},
		response: &types.ChatCompletionResponse{
			ID:     "chatcmpl_compatible",
			Object: "chat.completion",
			Model:  "gpt-5",
			Choices: []types.ChatCompletionChoice{
				{
					Index: 0,
					Message: types.ChatCompletionMessage{
						Role:    "assistant",
						Content: "hello from chat compatibility",
					},
					FinishReason: "stop",
				},
			},
			Usage: &types.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		},
	}

	compatRelay := &relayResponses{
		relayBase: relayBase{
			c:         compatCtx,
			provider:  provider,
			modelName: "gpt-5",
		},
		responsesRequest: compatRequest,
		operation:        responsesOperationCreate,
	}

	errWithCode, done := compatRelay.compatibleSend(provider)
	if done {
		t.Fatal("expected compatible non-stream send to succeed without terminating the relay")
	}
	if errWithCode != nil {
		t.Fatalf("expected compatible non-stream send success, got %v", errWithCode)
	}
	if got, ok := lookupChannelAffinity(compatCtx, channelAffinityKindResponses, compatRequest.PromptCacheKey); !ok || got != 77 {
		t.Fatalf("expected compatible chat fallback to record prompt_cache_key affinity on channel 77, got channel=%d ok=%v", got, ok)
	}
	if body := compatRecorder.Body.String(); !strings.Contains(body, `"object":"response"`) || !strings.Contains(body, `"chatcmpl_compatible"`) {
		t.Fatalf("expected compatible non-stream response body to be rewritten as responses json, got %q", body)
	}
}

func TestRelayResponsesSetRequestAndCompactSuccessBranches(t *testing.T) {
	gin.SetMode(gin.TestMode)

	compactRecorder := httptest.NewRecorder()
	compactCtx, _ := gin.CreateTestContext(compactRecorder)
	compactCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"gpt-5","prompt_cache_key":"pc-compact"}`))
	compactCtx.Request.Header.Set("Content-Type", "application/json")
	compactRelay := NewRelayResponses(compactCtx)
	if err := compactRelay.setRequest(); err != nil {
		t.Fatalf("expected compact setRequest to succeed, got %v", err)
	}
	if compactRelay.responsesRequest.PromptCacheKey != "pc-compact" || compactRelay.getOriginalModel() != "gpt-5" {
		t.Fatalf("expected compact request parsing to populate relay state, got %+v", compactRelay.responsesRequest)
	}

	createRecorder := httptest.NewRecorder()
	createCtx, _ := gin.CreateTestContext(createRecorder)
	createCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","prompt_cache_key":"pc-create"}`))
	createCtx.Request.Header.Set("Content-Type", "application/json")
	createRelay := NewRelayResponses(createCtx)
	if err := createRelay.setRequest(); err != nil {
		t.Fatalf("expected create setRequest to succeed, got %v", err)
	}
	if createRelay.responsesRequest.PromptCacheKey != "pc-create" || createRelay.getOriginalModel() != "gpt-5" {
		t.Fatalf("expected create request parsing to populate relay state, got %+v", createRelay.responsesRequest)
	}

	sendRecorder := httptest.NewRecorder()
	sendCtx, _ := gin.CreateTestContext(sendRecorder)
	sendCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	sendCtx.Set("token_id", 88)
	sendCtx.Set("token_group", "default")

	request := types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-compact-success",
	}
	prepareResponsesChannelAffinity(sendCtx, &request)

	provider := &compactSuccessProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel:         &model.Channel{Id: 120},
			SupportResponse: true,
		},
		response: &types.OpenAIResponsesResponses{
			ID:             "resp_compact",
			Model:          "gpt-5",
			Object:         "response",
			Status:         "completed",
			PromptCacheKey: "pc-compact-success",
		},
	}

	sendRelay := &relayResponses{
		relayBase: relayBase{
			c:         sendCtx,
			provider:  provider,
			modelName: "gpt-5",
		},
		responsesRequest: request,
		rawEnvelope:      responsesTestRawEnvelope(t, request),
		operation:        responsesOperationCompact,
	}
	errWithCode, done := sendRelay.send()
	if done || errWithCode != nil {
		t.Fatalf("expected compact responses success path, done=%v err=%v", done, errWithCode)
	}
	if got, ok := lookupChannelAffinity(sendCtx, channelAffinityKindResponses, request.PromptCacheKey); !ok || got != 120 {
		t.Fatalf("expected compact responses success to record affinity on channel 120, got channel=%d ok=%v", got, ok)
	}

	var nilRelay *relayResponses
	nilRelay.clearStalePreviousResponseAffinity()

	streamRecorder := httptest.NewRecorder()
	streamCtx, _ := gin.CreateTestContext(streamRecorder)
	streamCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	streamRelay := &relayResponses{
		relayBase: relayBase{
			c:        streamCtx,
			provider: &compatibleResponsesChatProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{}}},
		},
		responsesRequest: types.OpenAIResponsesRequest{Model: "gpt-5"},
	}
	closedStream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	close(closedStream.dataChan)
	close(closedStream.errChan)
	firstResponseTime, finalResponse, errWithCode := streamRelay.chatToResponseStreamClient(closedStream)
	if errWithCode != nil {
		t.Fatalf("expected closed response stream to finish without error, got %v", errWithCode.Message)
	}
	if !firstResponseTime.IsZero() {
		t.Fatalf("expected closed response stream to return zero first response time, got %v", firstResponseTime)
	}
	if finalResponse == nil || finalResponse.Status != types.ResponseStatusCompleted {
		t.Fatalf("expected closed response stream to process terminal response, got %#v", finalResponse)
	}
}

func TestRelayResponsesSetRequestRequiresModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name string
		body string
	}{
		{name: "missing model", body: `{"input":"hello"}`},
		{name: "blank model", body: `{"model":"   ","input":"hello"}`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tt.body))
			ctx.Request.Header.Set("Content-Type", "application/json")

			relay := NewRelayResponses(ctx)
			err := relay.setRequest()
			if err == nil || !strings.Contains(err.Error(), "field Model is required") {
				t.Fatalf("expected required model validation error, got %v", err)
			}
			if relay.getOriginalModel() != "" {
				t.Fatalf("expected missing model request not to populate original model, got %q", relay.getOriginalModel())
			}
		})
	}
}

func TestRelayResponsesChatToResponsesStreamErrorIsClientSafe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	streamRelay := &relayResponses{
		relayBase: relayBase{
			c:        ctx,
			provider: &compatibleResponsesChatProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{}}},
		},
		responsesRequest: types.OpenAIResponsesRequest{Model: "gpt-5"},
	}
	stream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	close(stream.dataChan)
	stream.errChan <- errors.New("upstream stream broken Authorization: Bearer secret-token api_key=query-secret https://provider.example/v1?token=url-secret session session-secret sk-testSECRET123")
	close(stream.errChan)

	streamRelay.chatToResponseStreamClient(stream)

	body := recorder.Body.String()
	if !strings.Contains(body, `"message":"stream interrupted"`) {
		t.Fatalf("expected stable client stream error message, got %q", body)
	}
	for _, forbidden := range []string{"upstream stream broken", "Authorization", "secret-token", "query-secret", "provider.example", "url-secret", "session-secret", "sk-testSECRET123"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("expected client stream body not to leak %q, got %q", forbidden, body)
		}
	}

	entries, _ := logger.GetLatestLogs(20)
	var streamLog string
	for i := len(entries) - 1; i >= 0; i-- {
		if strings.Contains(entries[i].Message, "Stream err:") {
			streamLog = entries[i].Message
			break
		}
	}
	if streamLog == "" {
		t.Fatal("expected stream error to be logged")
	}
	for _, forbidden := range []string{"secret-token", "query-secret", "provider.example", "url-secret", "session-secret", "sk-testSECRET123"} {
		if strings.Contains(streamLog, forbidden) {
			t.Fatalf("expected stream error log to redact %q, got %q", forbidden, streamLog)
		}
	}
}

func TestRelayResponsesCompatibleFallbackRejectsStatefulResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	storeTrue := true
	testCases := []struct {
		name          string
		request       types.OpenAIResponsesRequest
		expectedParam string
	}{
		{
			name: "store true",
			request: types.OpenAIResponsesRequest{
				Model: "gpt-5",
				Input: "hello",
				Store: &storeTrue,
			},
			expectedParam: "store",
		},
		{
			name: "previous response id",
			request: types.OpenAIResponsesRequest{
				Model:              "gpt-5",
				Input:              "hello",
				PreviousResponseID: "resp_prev",
			},
			expectedParam: "previous_response_id",
		},
		{
			name: "conversation state",
			request: types.OpenAIResponsesRequest{
				Model:        "gpt-5",
				Input:        "hello",
				Conversation: map[string]any{"id": "conv_123"},
			},
			expectedParam: "conversation",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

			provider := &compatibleResponsesChatProvider{
				BaseProvider: providersBase.BaseProvider{
					Channel: &model.Channel{Id: 88, CompatibleResponse: true},
				},
				response: &types.ChatCompletionResponse{
					ID:     "chatcmpl_unused",
					Object: "chat.completion",
					Model:  "gpt-5",
				},
			}

			relay := &relayResponses{
				relayBase: relayBase{
					c:         ctx,
					provider:  provider,
					modelName: "gpt-5",
				},
				responsesRequest: tc.request,
				operation:        responsesOperationCreate,
			}

			errWithCode, done := relay.sendCurrentProvider()
			if errWithCode == nil {
				t.Fatal("expected stateful responses compatibility fallback to be rejected")
			}
			if done {
				t.Fatal("expected stateful fallback rejection to remain retryable")
			}
			if errWithCode.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("expected status 503, got %d", errWithCode.StatusCode)
			}
			if errWithCode.LocalError {
				t.Fatal("expected stateful fallback rejection not to be marked local")
			}
			if errWithCode.Param != tc.expectedParam {
				t.Fatalf("expected param %q, got %q", tc.expectedParam, errWithCode.Param)
			}
			if errWithCode.Code != "responses_native_support_required" {
				t.Fatalf("expected error code responses_native_support_required, got %q", errWithCode.Code)
			}
			if provider.createCalls != 0 || provider.createStreamCalls != 0 {
				t.Fatalf("expected chat fallback not to be attempted, got create=%d stream=%d", provider.createCalls, provider.createStreamCalls)
			}
		})
	}
}
