package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/datatypes"
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

type inputTokensSuccessProvider struct {
	providersBase.BaseProvider
}

func (p *inputTokensSuccessProvider) GetRequestHeaders() map[string]string {
	return map[string]string{}
}

func (p *inputTokensSuccessProvider) CountResponsesInputTokens(context.Context, *commonresponses.Request) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
	}, nil
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

func (p *affinityResponsesProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
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

type redirectResponsesProvider struct {
	providersBase.BaseProvider
	apiErr       *types.OpenAIErrorWithStatusCode
	createCalls  int
	compactCalls int
}

type stalePreviousResponseProvider struct {
	providersBase.BaseProvider
	createCalls int
}

type streamAffinityResponsesProvider struct {
	providersBase.BaseProvider
	stream commonresponses.EventStream
}

func (p *streamAffinityResponsesProvider) GetRequestHeaders() map[string]string {
	return map[string]string{}
}

func (p *streamAffinityResponsesProvider) CreateResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *streamAffinityResponsesProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	return p.stream, nil
}

func (p *streamAffinityResponsesProvider) CompactResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

type compatibleStreamChatProvider struct {
	providersBase.BaseProvider
	stream requester.StreamReaderInterface[string]
}

type cancellationTrackingRelayStream struct {
	dataChan  chan string
	errChan   chan error
	recv      chan struct{}
	closed    chan struct{}
	recvOnce  sync.Once
	closeOnce sync.Once
}

func (s *cancellationTrackingRelayStream) Recv() (<-chan string, <-chan error) {
	s.recvOnce.Do(func() { close(s.recv) })
	return s.dataChan, s.errChan
}

func (s *cancellationTrackingRelayStream) Close() {
	s.closeOnce.Do(func() { close(s.closed) })
}

type compatibleResponsesChatProvider struct {
	providersBase.BaseProvider
	response          *types.ChatCompletionResponse
	createCalls       int
	createStreamCalls int
}

type chatFallbackResponsesProvider struct {
	providersBase.BaseProvider
	request     *commonresponses.Request
	chatRequest *types.ChatCompletionRequest
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

func (p *chatFallbackResponsesProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	p.chatRequest = request
	return &types.ChatCompletionResponse{
		ID:     "chatcmpl_compatible",
		Object: "chat.completion",
		Model:  request.Model,
		Choices: []types.ChatCompletionChoice{{
			Index:   0,
			Message: types.ChatCompletionMessage{Role: types.ChatMessageRoleAssistant, Content: "ok"},
		}},
		Usage: &types.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

func (p *chatFallbackResponsesProvider) CreateChatCompletionStream(*types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return nil, common.StringErrorWrapperLocal("unexpected stream call", "test_error", http.StatusInternalServerError)
}

func (p *chatFallbackResponsesProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
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

func (p *compactRejectProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
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

func (p *compactSuccessProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *compactSuccessProvider) CompactResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return p.response, nil
}

func (p *redirectResponsesProvider) GetRequestHeaders() map[string]string {
	return map[string]string{}
}

func (p *redirectResponsesProvider) CreateResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	p.createCalls++
	return nil, p.apiErr
}

func (p *redirectResponsesProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	return nil, common.StringErrorWrapperLocal("unexpected stream call", "test_error", http.StatusInternalServerError)
}

func (p *redirectResponsesProvider) CompactResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	p.compactCalls++
	return nil, p.apiErr
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

func (p *stalePreviousResponseProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func (p *stalePreviousResponseProvider) CompactResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}

func TestRelayResponsesCompactRejectsStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)

	provider := &compactRejectProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{},
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
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)

	provider := &compactRejectProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{},
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

func TestRelayChatRoutesResponsesOnlyModelThroughResponsesAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(ctx)
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
		t.Fatalf("expected Responses-only model conversion to succeed, done=%v err=%v", done, errWithCode)
	}
	if provider.request == nil || provider.request.Body == nil || provider.request.Body.Projection.Model != "o3-pro" {
		t.Fatalf("expected the mapped model to reach Responses, request=%+v", provider.request)
	}
}

func TestPrepareResponsesChannelAffinityPrefersRecordedChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 99)

	request := &types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-responses-hit",
	}

	prepareResponsesChannelAffinity(ctx, request)
	recordCurrentChannelAffinity(ctx, channelAffinityKindResponses, 9527)

	nextCtx, _ := gin.CreateTestContext(httptest.NewRecorder())

	enableResponsesTestDeadline(nextCtx)
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
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 12345)

	store := false
	request := types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-record-success",
		Store:          &store,
	}
	prepareResponsesChannelAffinity(ctx, &request)

	provider := &affinityResponsesProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 88, Type: config.ChannelTypeOpenAI},
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

func TestRelayResponsesInputTokensDoesNotUseChannelAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := config.DefaultChannelAffinitySettings()
	for index := range settings.Rules {
		if settings.Rules[index].Kind == string(channelAffinityKindResponses) {
			settings.Rules[index].PathRegex = `^/v1/responses(?:/.*)?$`
		}
	}
	settings.Normalize()
	withChannelAffinitySettings(t, settings)

	seedCtx := newAffinityTestContext(http.MethodPost, "/v1/responses/input_tokens")
	seedCtx.Set("token_id", 7001)
	seedCtx.Set("token_group", "default")
	seedRequest := &types.OpenAIResponsesRequest{Model: "gpt-5", PromptCacheKey: "pc-input-tokens-read"}
	prepareResponsesChannelAffinity(seedCtx, seedRequest)
	recordCurrentChannelAffinity(seedCtx, channelAffinityKindResponses, 71)

	readCtx := newAffinityTestContext(http.MethodPost, "/v1/responses/input_tokens")
	readCtx.Set("token_id", 7001)
	readCtx.Set("token_group", "default")
	readCtx.Request.Body = io.NopCloser(strings.NewReader(`{"model":"gpt-5","prompt_cache_key":"pc-input-tokens-read"}`))
	readCtx.Request.Header.Set("Content-Type", "application/json")
	readRelay := NewRelayResponses(readCtx)
	if err := readRelay.setRequest(); err != nil {
		t.Fatalf("set input_tokens request: %v", err)
	}
	if currentChannelAffinityState(readCtx) != nil || currentPreferredChannelID(readCtx) != 0 || currentChannelAffinityLogMeta(readCtx) != nil {
		t.Fatalf("input_tokens must not prepare or read affinity, state=%#v preferred=%d meta=%#v", currentChannelAffinityState(readCtx), currentPreferredChannelID(readCtx), currentChannelAffinityLogMeta(readCtx))
	}

	writeCtx := newAffinityTestContext(http.MethodPost, "/v1/responses/input_tokens")
	writeCtx.Set("token_id", 7001)
	writeCtx.Set("token_group", "default")
	writeRequest := types.OpenAIResponsesRequest{Model: "gpt-5", PromptCacheKey: "pc-input-tokens-write"}
	prepareResponsesChannelAffinity(writeCtx, &writeRequest)
	provider := &inputTokensSuccessProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 72}}}
	writeRelay := &relayResponses{
		relayBase:        relayBase{c: writeCtx, provider: provider, modelName: "gpt-5"},
		responsesRequest: writeRequest,
		rawEnvelope:      responsesTestRawEnvelope(t, writeRequest),
		operation:        responsesOperationInputTokens,
	}
	originalLogConsumeEnabled := config.LogConsumeEnabled
	config.LogConsumeEnabled = false
	t.Cleanup(func() { config.LogConsumeEnabled = originalLogConsumeEnabled })
	apiErr, done := writeRelay.send()
	if apiErr != nil || done {
		t.Fatalf("expected input_tokens success, done=%v err=%v", done, apiErr)
	}
	if channelID, ok := lookupChannelAffinity(writeCtx, channelAffinityKindResponses, writeRequest.PromptCacheKey); ok {
		t.Fatalf("input_tokens must not record affinity, got channel %d", channelID)
	}
}

func TestPrepareResponsesChannelAffinityUsesPreviousResponseIDBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(ctx)
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

	enableResponsesTestDeadline(nextCtx)
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

	enableResponsesTestDeadline(initialCtx)
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

	enableResponsesTestDeadline(nextCtx)
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

	enableResponsesTestDeadline(seedCtx)
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
	enableResponsesTestDeadline(ctx)
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
			Channel: &model.Channel{Id: 41, Type: config.ChannelTypeOpenAI},
		},
	}
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
	if relay.responsesRequest.PreviousResponseID != "resp_stale" {
		t.Fatalf("expected send() not to clear previous_response_id, got %q", relay.responsesRequest.PreviousResponseID)
	}
	if channel := relay.provider.GetChannel(); channel == nil || channel.Id != 41 {
		t.Fatalf("expected send() to keep the original provider channel 41, got %#v", channel)
	}
	if ctx.GetBool(responsesPreviousResponseRecoveredContextKey) {
		t.Fatal("expected send() not to mark the request as recovered")
	}
}

func TestRelayResponsesClearStalePreviousResponseAffinityRemovesAllRequestBindings(t *testing.T) {
	gin.SetMode(gin.TestMode)

	seedCtx, _ := gin.CreateTestContext(httptest.NewRecorder())

	enableResponsesTestDeadline(seedCtx)
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
	enableResponsesTestDeadline(ctx)
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

	enableResponsesTestDeadline(staleLookupCtx)
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

	enableResponsesTestDeadline(lookupCtx)
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
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 456)
	ctx.Set("token_group", "default")

	store := false
	request := types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-stream-affinity",
		Stream:         true,
		Store:          &store,
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
			Channel: &model.Channel{Id: 66, Type: config.ChannelTypeOpenAI},
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

	enableResponsesTestDeadline(nextCtx)
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
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 654)
	ctx.Set("token_group", "default")

	request := types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-compatible-stream",
		Stream:         true,
	}
	storeFalse := false
	request.Store = &storeFalse
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
			Channel: &model.Channel{Id: 67, Type: config.ChannelTypeAnthropic, CompatibleResponse: true},
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
		selectedDataPath: providersBase.DataPathCrossProtocol,
	}
	prepared, err := request.ToChatCompletionRequest()
	if err != nil {
		t.Fatalf("prepare compatibility fixture: %v", err)
	}
	prepared.Model = relay.modelName
	relay.preparedChatRequest = prepared

	errWithCode, done := relay.send()
	if done {
		t.Fatal("expected compatible stream relay to keep processing")
	}
	if errWithCode != nil {
		t.Fatalf("expected success, got %v", errWithCode)
	}

	nextCtx, _ := gin.CreateTestContext(httptest.NewRecorder())

	enableResponsesTestDeadline(nextCtx)
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
	enableResponsesTestDeadline(ctx)
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
			Channel: &model.Channel{Type: config.ChannelTypeOpenAI},
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
	if !shouldRecoverStalePreviousResponse(&types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Message: "previous response was not found by upstream", Param: "previous_response_id"}, StatusCode: http.StatusNotFound}) {
		t.Fatal("expected previous response not found message to trigger recovery")
	}
	if !shouldRecoverStalePreviousResponse(&types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Code: "previous_response_not_found"}, StatusCode: http.StatusBadRequest}) {
		t.Fatal("expected previous_response_not_found code to trigger recovery")
	}
	if shouldRecoverStalePreviousResponse(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Message: "previous response was not found in an unrelated cache"},
		StatusCode:  http.StatusNotFound,
	}) {
		t.Fatal("expected message-only fallback without previous_response_id param not to clear affinity")
	}
	if shouldRecoverStalePreviousResponse(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Message: "previous response was not found in an unrelated cache", Param: "input"},
		StatusCode:  http.StatusBadRequest,
	}) {
		t.Fatal("expected another error param not to clear continuation affinity")
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		apiErr := &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{Message: "previous response was not found by upstream"},
			StatusCode:  status,
		}
		if shouldRecoverStalePreviousResponse(apiErr) {
			t.Fatalf("status %d must preserve the provider error instead of triggering stale continuation recovery", status)
		}
	}

	if plan := relay.stalePreviousResponseHandlingPlan(&types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Code: "previous_response_not_found"}, StatusCode: http.StatusNotFound}); plan != nil {
		t.Fatal("expected stale previous response handling to require a previous_response_id")
	}
	relay.responsesRequest.PreviousResponseID = "resp-stale"
	upstreamMiss := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "previous_response_not_found", Message: "previous response not found"},
		StatusCode:  http.StatusNotFound,
	}
	plan := relay.stalePreviousResponseHandlingPlan(upstreamMiss)
	if plan == nil {
		t.Fatal("expected stale previous response handling plan to be created")
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
	enableResponsesTestDeadline(compatCtx)
	compatCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	compatCtx.Set("token_id", 999)
	compatCtx.Set("token_group", "default")

	compatRequest := types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		Input:          "hello",
		PromptCacheKey: "pc-compatible-non-stream",
	}
	storeFalse := false
	compatRequest.Store = &storeFalse
	prepareResponsesChannelAffinity(compatCtx, &compatRequest)

	provider := &compatibleResponsesChatProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 77, Type: config.ChannelTypeAnthropic, CompatibleResponse: true},
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
	prepared, err := compatRequest.ToChatCompletionRequest()
	if err != nil {
		t.Fatalf("prepare compatibility fixture: %v", err)
	}
	prepared.Model = compatRelay.modelName
	compatRelay.preparedChatRequest = prepared

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
	enableResponsesTestDeadline(compactCtx)
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
	enableResponsesTestDeadline(createCtx)
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
	enableResponsesTestDeadline(sendCtx)
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
			Channel: &model.Channel{Id: 120},
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
	enableResponsesTestDeadline(streamCtx)
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

func TestRelayResponsesProviderRequestPreservesRawQuery(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact?api-version=preview&feature=one&feature=two", strings.NewReader(`{"model":"gpt-5","input":"hello"}`))
	relay := NewRelayResponses(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("set Responses request: %v", err)
	}

	got := relay.providerRequest(commonresponses.ResponsesCompact).RawQuery
	if got != "api-version=preview&feature=one&feature=two" {
		t.Fatalf("raw query changed before provider selection: %q", got)
	}
}

func TestRelayResponsesSurfacesExactWireRedirectWithoutRetryableSuccessWork(t *testing.T) {
	store := false
	for _, operation := range []responsesOperation{responsesOperationCreate, responsesOperationCompact} {
		name := "create"
		if operation == responsesOperationCompact {
			name = "compact"
		}
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			enableResponsesTestDeadline(ctx)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			request := types.OpenAIResponsesRequest{Model: "gpt-5", Store: &store}
			apiErr := &types.OpenAIErrorWithStatusCode{
				OpenAIError:       types.OpenAIError{Message: "provider returned an HTTP redirect", Code: "provider_redirect_response"},
				StatusCode:        http.StatusTemporaryRedirect,
				ReplayRawResponse: true,
				RawBody:           []byte("redirect body\n"),
				ResponseHeaders: http.Header{
					"Content-Type":        {"text/plain"},
					"Location":            {"https://api.openai.com/v1/responses/redirected"},
					"X-Request-Id":        {"req-redirect"},
					"Set-Cookie":          {"provider_session=secret"},
					"Openai-Organization": {"org-secret"},
				},
			}
			provider := &redirectResponsesProvider{
				BaseProvider: providersBase.BaseProvider{
					Channel: &model.Channel{Type: config.ChannelTypeOpenAI},
				},
				apiErr: apiErr,
			}
			relay := &relayResponses{
				relayBase:        relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
				responsesRequest: request,
				rawEnvelope:      responsesTestRawEnvelope(t, request),
				operation:        operation,
			}

			gotErr, done := relay.sendCurrentProvider()
			if gotErr != apiErr || !done {
				t.Fatalf("redirect must be terminal before retry/success work: done=%v err=%+v", done, gotErr)
			}
			if calls := provider.createCalls + provider.compactCalls; calls != 1 {
				t.Fatalf("provider calls=%d, want 1", calls)
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("send path wrote before error rendering: %q", recorder.Body.String())
			}

			relay.HandleJsonError(gotErr)
			if recorder.Code != http.StatusTemporaryRedirect || recorder.Body.String() != "redirect body\n" {
				t.Fatalf("redirect changed: status=%d body=%q", recorder.Code, recorder.Body.String())
			}
			for name, want := range map[string]string{
				"Content-Type": "text/plain",
				"Location":     "https://api.openai.com/v1/responses/redirected",
				"X-Request-Id": "req-redirect",
			} {
				if got := recorder.Header().Get(name); got != want {
					t.Fatalf("%s=%q, want %q in %#v", name, got, want, recorder.Header())
				}
			}
			for _, name := range []string{"Set-Cookie", "Openai-Organization"} {
				if got := recorder.Header().Get(name); got != "" {
					t.Fatalf("unsafe header %s=%q", name, got)
				}
			}
		})
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
			enableResponsesTestDeadline(ctx)
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

func TestRelayResponsesSetRequestRejectsSavedPromptBeforeModelValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"prompt":{"id":"pmpt_123"}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	relay := NewRelayResponses(ctx)
	err := relay.setRequest()
	apiErr := capabilityGateAPIError(err)
	if apiErr == nil || apiErr.Param != "prompt" || openAIErrorCodeString(apiErr.Code, "") != unsupportedCapabilityCode {
		t.Fatalf("expected prompt-only request to fail at the saved prompt capability gate, got err=%v api=%+v", err, apiErr)
	}
	if !strings.Contains(apiErr.Message, "2026-11-30") || !strings.Contains(apiErr.Message, "instructions or input") {
		t.Fatalf("expected actionable migration guidance and close date, got %q", apiErr.Message)
	}
	if relay.getOriginalModel() != "" {
		t.Fatalf("expected rejected prompt-only request not to populate original model, got %q", relay.getOriginalModel())
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
	enableResponsesTestDeadline(ctx)
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

	_, finalResponse, streamErr := streamRelay.chatToResponseStreamClient(stream)
	if streamErr == nil || streamErr.StatusCode != http.StatusBadGateway || finalResponse != nil {
		t.Fatalf("expected terminal stream error without a final response, err=%v final=%+v", streamErr, finalResponse)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `"message":"stream interrupted"`) {
		t.Fatalf("expected stable client stream error message, got %q", body)
	}
	if !strings.Contains(body, `"code":"invalid_provider_response"`) || !strings.Contains(body, `"param":null`) || !strings.Contains(body, `"sequence_number":0`) {
		t.Fatalf("expected a valid sequenced Responses stream error, got %q", body)
	}
	if strings.Count(body, "event: error") != 1 || !ctx.GetBool(responsesStreamErrorAlreadyRenderedContextKey) {
		t.Fatalf("expected one rendered terminal error and outer-render suppression, body=%q rendered=%v", body, ctx.GetBool(responsesStreamErrorAlreadyRenderedContextKey))
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
	for _, expected := range []string{"secret-token", "query-secret", "provider.example", "url-secret", "session-secret", "sk-testSECRET123"} {
		if !strings.Contains(streamLog, expected) {
			t.Fatalf("系统日志丢失原始诊断 %q: %q", expected, streamLog)
		}
	}
}

func TestChatToResponsesConsumesLogicalDataFromExactRawSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	usage := &types.Usage{}
	provider := &compatibleResponsesChatProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{}, Usage: usage}}
	store := false
	streamRelay := &relayResponses{
		relayBase:        relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
		responsesRequest: types.OpenAIResponsesRequest{Model: "gpt-5", Stream: true, Store: &store},
	}
	body := ": keepalive\r\n\r\n" +
		"event: chat.chunk\r\nid: 7\r\ndata: {\"id\":\"chatcmpl_raw\",\"model\":\"gpt-5\",\"message\":\"future success metadata\",\"code\":\"future_code\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello raw\"}}]}\r\n\r\n" +
		"data: {\"id\":\"chatcmpl_raw\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\r\n\r\n" +
		"data:[DONE]\r\n\r\n"
	handler := &openai.OpenAIStreamHandler{Usage: usage}
	stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, handler.HandleExactChatSSE, requester.StreamReadOptions{RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatalf("create raw Chat stream: %+v", apiErr)
	}

	_, finalResponse, apiErr := streamRelay.chatToResponseStreamClient(stream)
	if apiErr != nil {
		t.Fatalf("convert raw Chat SSE: %+v", apiErr)
	}
	if finalResponse == nil || finalResponse.Status != types.ResponseStatusCompleted {
		t.Fatalf("raw Chat SSE did not produce a completed Responses result: %+v", finalResponse)
	}
	if finalResponse.Usage != nil {
		t.Fatalf("missing provider usage was exposed as a synthetic zero value: %+v", finalResponse.Usage)
	}
	output := recorder.Body.String()
	if !strings.Contains(output, "hello raw") || strings.Contains(output, "keepalive") || strings.Contains(output, "chat.chunk") || strings.Contains(output, "data:[DONE]") {
		t.Fatalf("raw SSE framing leaked into Chat→Responses conversion: %s", output)
	}
	if strings.Count(output, "event: response.completed") != 1 {
		t.Fatalf("raw [DONE] finalized converter more than once: %s", output)
	}
}

func TestUnaryChatObservationFallbackCannotBecomeEmptyResponsesSuccess(t *testing.T) {
	for _, rawBody := range []string{
		`{"id":"chat_future","model":"gpt-5","choices":{"future":true},"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		`{"id":"chat_future","model":"gpt-5","choices":[],"error":"future error"}`,
		`{"id":"chat_future","model":"gpt-5","choices":[],"error":{"future_error_union":true}}`,
	} {
		t.Run(rawBody, func(t *testing.T) {
			ctx, recorder := responsesOwnerTestContext(221, 222)
			raw := []byte(rawBody)
			observed := &openai.OpenAIProviderChatResponse{}
			if err := observed.DecodeCapturedProviderJSON(raw); err != nil {
				t.Fatal(err)
			}
			observed.SetProviderRawJSON(raw)
			observed.EnableProviderRawJSONReplay()
			provider := &compatibleResponsesChatProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Type: config.ChannelTypeAnthropic, CompatibleResponse: true}}, response: &observed.ChatCompletionResponse}
			store := false
			r := &relayResponses{
				relayBase:           relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
				responsesRequest:    types.OpenAIResponsesRequest{Model: "gpt-5", Store: &store},
				preparedChatRequest: &types.ChatCompletionRequest{Model: "gpt-5"},
			}
			apiErr, done := r.compatibleSend(provider)
			if apiErr == nil || apiErr.Code != "invalid_provider_response" || !apiErr.UpstreamAccepted || !done || provider.createCalls != 1 || recorder.Body.Len() != 0 {
				t.Fatalf("unrepresentable raw response became success or replayable: error=%+v done=%t calls=%d body=%s", apiErr, done, provider.createCalls, recorder.Body.String())
			}
		})
	}
}

func TestChatToResponsesRawProviderErrorStopsBeforeLateUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	usage := &types.Usage{}
	provider := &compatibleResponsesChatProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{}, Usage: usage}}
	streamRelay := &relayResponses{
		relayBase:        relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
		responsesRequest: types.OpenAIResponsesRequest{Model: "gpt-5", Stream: true},
	}
	body := "data: {\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_value\"}}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":9,\"total_tokens\":18}}\n\n" +
		"data: [DONE]\n\n"
	handler := &openai.OpenAIStreamHandler{Usage: usage}
	stream, constructionErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, handler.HandleExactChatSSE, requester.StreamReadOptions{RequireProtocolTerminal: true})
	if constructionErr != nil {
		t.Fatalf("create raw Chat error stream: %+v", constructionErr)
	}

	_, finalResponse, apiErr := streamRelay.chatToResponseStreamClient(stream)
	if apiErr == nil || openAIErrorCodeString(apiErr.Code, "") != "invalid_value" || !apiErr.UpstreamAccepted {
		t.Fatalf("raw provider error was not mapped as a failed accepted stream: %+v", apiErr)
	}
	if finalResponse != nil || usage.ProviderReported || usage.TotalTokens != 0 {
		t.Fatalf("late raw data changed terminal/accounting state: final=%+v usage=%+v", finalResponse, usage)
	}
	output := recorder.Body.String()
	if strings.Count(output, "event: error") != 1 || strings.Contains(output, "response.completed") || strings.Contains(output, `"total_tokens":18`) {
		t.Fatalf("raw provider error was forged as successful conversion: %s", output)
	}
}

func TestCompatibleResponsesConversionFailureIsClientError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	storeFalse := false
	summary := "auto"
	provider := &compatibleResponsesChatProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Type: config.ChannelTypeAnthropic, CompatibleResponse: true}}}
	relay := &relayResponses{
		relayBase: relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
		responsesRequest: types.OpenAIResponsesRequest{
			Model:     "gpt-5",
			Input:     "hello",
			Store:     &storeFalse,
			Reasoning: &types.ReasoningEffort{Summary: &summary},
		},
		operation: responsesOperationCreate,
	}

	finalizeErr := finalizeSelectedProviderRequest(relay)
	apiErr := wrapRelaySetupError(relay, "provider_request", finalizeErr, "one_hub_error", http.StatusServiceUnavailable)
	if finalizeErr == nil || apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || apiErr.Type != "invalid_request_error" || openAIErrorCodeString(apiErr.Code, "") != unsupportedCapabilityCode || !apiErr.LocalError {
		t.Fatalf("conversion failure = %+v; want local unsupported capability/400", apiErr)
	}
	if provider.createCalls != 0 || provider.createStreamCalls != 0 {
		t.Fatalf("conversion failure reached provider: create=%d stream=%d", provider.createCalls, provider.createStreamCalls)
	}
}

func TestRelayResponsesChatFallbackStopsOnClientCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(ctx)
	requestContext, cancel := context.WithCancel(context.Background())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
	streamRelay := &relayResponses{
		relayBase: relayBase{
			c:        ctx,
			provider: &compatibleResponsesChatProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{}}},
		},
		responsesRequest: types.OpenAIResponsesRequest{Model: "gpt-5"},
	}
	stream := &cancellationTrackingRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
		recv:     make(chan struct{}),
		closed:   make(chan struct{}),
	}

	result := make(chan *types.OpenAIErrorWithStatusCode, 1)
	go func() {
		_, _, apiErr := streamRelay.chatToResponseStreamClient(stream)
		result <- apiErr
	}()

	select {
	case <-stream.recv:
	case <-time.After(time.Second):
		t.Fatal("fallback stream did not start receiving")
	}
	cancel()

	select {
	case apiErr := <-result:
		if apiErr == nil || openAIErrorCodeString(apiErr.Code, "") != "request_canceled" || apiErr.StatusCode != 499 {
			t.Fatalf("fallback cancellation = %+v, want request_canceled/499", apiErr)
		}
	case <-time.After(time.Second):
		t.Fatal("fallback stream remained blocked after client cancellation")
	}
	select {
	case <-stream.closed:
	case <-time.After(time.Second):
		t.Fatal("fallback stream was not closed after client cancellation")
	}
}

func TestCompatibleResponsesStreamFailurePreservesAcceptedQuotaAndStopsRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	stream := &fakeRelayStream{dataChan: make(chan string), errChan: make(chan error, 1)}
	close(stream.dataChan)
	stream.errChan <- errors.New("provider stream failed")
	close(stream.errChan)
	provider := &compatibleStreamChatProvider{
		BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Type: config.ChannelTypeAnthropic, CompatibleResponse: true}},
		stream:       stream,
	}
	storeFalse := false
	streamRelay := &relayResponses{
		relayBase: relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
		responsesRequest: types.OpenAIResponsesRequest{
			Model:  "gpt-5",
			Input:  "hello",
			Stream: true,
			Store:  &storeFalse,
		},
		operation: responsesOperationCreate,
	}
	if err := finalizeSelectedProviderRequest(streamRelay); err != nil {
		t.Fatalf("finalize compatible request: %v", err)
	}

	apiErr, done := streamRelay.compatibleSend(provider)
	if apiErr == nil || apiErr.StatusCode != http.StatusBadGateway || !apiErr.UpstreamAccepted || !done {
		t.Fatalf("accepted stream failure must preserve quota and stop retry, err=%+v done=%v", apiErr, done)
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
			name: "store omitted defaults to stored",
			request: types.OpenAIResponsesRequest{
				Model: "gpt-5",
				Input: "hello",
			},
			expectedParam: "store",
		},
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
			enableResponsesTestDeadline(ctx)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

			provider := &compatibleResponsesChatProvider{
				BaseProvider: providersBase.BaseProvider{
					Channel: &model.Channel{Id: 88, Type: config.ChannelTypeAnthropic, CompatibleResponse: true},
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
				selectedDataPath: providersBase.DataPathCrossProtocol,
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

func TestRelayResponsesSelectedProviderRejectsLossyFallbackBeforeProviderWork(t *testing.T) {
	gin.SetMode(gin.TestMode)

	storeFalse := false
	maxToolCalls := 2
	request := types.OpenAIResponsesRequest{
		Model:        "gpt-5",
		Input:        "hello",
		Store:        &storeFalse,
		MaxToolCalls: &maxToolCalls,
	}
	provider := &compatibleResponsesChatProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 89, Type: config.ChannelTypeAnthropic, CompatibleResponse: true},
		},
	}
	relay := &relayResponses{
		relayBase: relayBase{
			provider:  provider,
			modelName: "gpt-5",
		},
		responsesRequest: request,
		rawEnvelope:      responsesTestRawEnvelope(t, request),
		operation:        responsesOperationCreate,
	}

	err := relay.validateSelectedProviderRequest()
	assertCapabilityGateError(t, err, "max_tool_calls")
	if provider.createCalls != 0 || provider.createStreamCalls != 0 {
		t.Fatalf("expected representability failure before provider work, got create=%d stream=%d", provider.createCalls, provider.createStreamCalls)
	}
}

func TestRelayResponsesSelectedProviderSkipsRepresentabilityGateForNativeResponses(t *testing.T) {
	maxToolCalls := 2
	request := types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hello", MaxToolCalls: &maxToolCalls}
	provider := &compatibleResponsesChatProvider{
		BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 90, Type: config.ChannelTypeOpenAI, CompatibleResponse: true},
		},
	}
	relay := &relayResponses{
		relayBase:        relayBase{provider: provider, modelName: "gpt-5"},
		responsesRequest: request,
		rawEnvelope:      responsesTestRawEnvelope(t, request),
		operation:        responsesOperationCreate,
	}
	if err := relay.validateSelectedProviderRequest(); err != nil {
		t.Fatalf("native Responses provider must retain the complete surface: %v", err)
	}
}

func TestRelayResponsesUsesAdapterDataPathForDisabledCustomResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	plugin := datatypes.NewJSONType(model.PluginType{
		"endpoints": {
			"openai.chat_completions": map[string]any{"enabled": true, "upstream_url": ""},
			"openai.responses":        map[string]any{"enabled": false, "upstream_url": ""},
		},
	})
	channel := &model.Channel{
		Id:                 93,
		Type:               config.ChannelTypeCustom,
		CompatibleResponse: true,
		Plugin:             &plugin,
	}
	compatibleBaseURL := "https://compatible.example"
	channel.BaseURL = &compatibleBaseURL
	provider := &chatFallbackResponsesProvider{BaseProvider: providersBase.BaseProvider{
		Channel: channel,
	}}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(ctx)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	store := false
	request := types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hello", Store: &store}
	relay := &relayResponses{
		relayBase:        relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
		responsesRequest: request,
		rawEnvelope:      responsesTestRawEnvelope(t, request),
		operation:        responsesOperationCreate,
	}

	if err := relay.validateSelectedProviderRequest(); err != nil {
		t.Fatalf("representable cross-protocol request was rejected: %v", err)
	}
	apiErr, done := relay.sendCurrentProvider()
	if apiErr != nil || done {
		t.Fatalf("expected Chat compatibility path, done=%v err=%+v", done, apiErr)
	}
	if provider.chatRequest == nil {
		t.Fatal("expected disabled native Responses endpoint to use Chat compatibility")
	}
	if provider.request != nil {
		t.Fatalf("native Responses endpoint was called despite adapter CrossProtocol path: %+v", provider.request)
	}
}

func TestDisabledCustomResponsesMaterializesChatBodyBeforeProvider(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var providerBody []byte
			providerCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				providerCalls++
				if request.URL.Path != "/v1/chat/completions" {
					t.Errorf("provider path=%q", request.URL.Path)
				}
				providerBody, _ = io.ReadAll(request.Body)
				w.Header().Set("X-Request-Id", "chat-provider-request")
				w.Header().Set("Content-Type", "application/json")
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
					_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
					return
				}
				_, _ = io.WriteString(w, `{"id":"chatcmpl_unary","object":"chat.completion","created":1,"model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			raw := fmt.Sprintf(` {"model":"gpt-5","input":"hello","store":false,"stream":%t} `, stream)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			enableResponsesTestDeadline(ctx)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
			ctx.Request.Header.Set("Content-Type", "application/json")
			relay := NewRelayResponses(ctx)
			if err := relay.setRequest(); err != nil {
				t.Fatalf("parse Responses request: %v", err)
			}
			originalEnvelope := string(relay.rawEnvelope.Object.Raw)

			plugin := datatypes.NewJSONType(model.PluginType{"endpoints": {"openai.chat_completions": map[string]any{"enabled": true, "upstream_url": ""}, "openai.responses": map[string]any{"enabled": false, "upstream_url": ""}}})
			preAdd := `{"pre_add":true,"temperature":0.37}`
			proxy := ""
			channel := &model.Channel{Id: 93, Type: config.ChannelTypeCustom, CompatibleResponse: true, AllowExtraBody: true, Plugin: &plugin, Proxy: &proxy, BaseURL: &server.URL, CustomParameter: &preAdd}
			provider := openai.CreateOpenAIProvider(channel, server.URL)
			provider.SetContext(ctx)
			provider.SetOriginalModel("gpt-5")
			provider.SetUsage(&types.Usage{})
			relay.provider = provider
			relay.modelName = "gpt-5"

			if err := relay.validateSelectedProviderRequest(); err != nil {
				t.Fatalf("validate compatible request: %v", err)
			}
			if err := relay.prepareSelectedProviderRemoteMedia(); err != nil {
				t.Fatalf("materialize compatible request: %v", err)
			}
			apiErr, done := relay.sendCurrentProvider()
			if apiErr != nil || done {
				t.Fatalf("compatible provider failed: done=%v err=%+v", done, apiErr)
			}

			var sent map[string]json.RawMessage
			if err := json.Unmarshal(providerBody, &sent); err != nil {
				t.Fatalf("decode provider body: %v body=%s", err, providerBody)
			}
			if _, exists := sent["input"]; exists {
				t.Fatalf("Responses input leaked to Chat provider: %s", providerBody)
			}
			if string(sent["model"]) != `"gpt-5"` || !strings.Contains(string(sent["messages"]), "hello") {
				t.Fatalf("prepared Chat request was not sent: %s", providerBody)
			}
			if string(sent["temperature"]) != "0.37" {
				t.Fatalf("Responses-to-Chat pre_add was not materialized once: %s", providerBody)
			}
			if providerCalls != 1 {
				t.Fatalf("Responses-to-Chat provider calls=%d, want 1", providerCalls)
			}
			if stream && string(sent["stream"]) != "true" {
				t.Fatalf("stream mode was not materialized: %s", providerBody)
			}
			if string(relay.rawEnvelope.Object.Raw) != originalEnvelope {
				t.Fatalf("Responses envelope was overwritten: got=%q want=%q", relay.rawEnvelope.Object.Raw, originalEnvelope)
			}
			providerResponseHeaders, ok := ctx.Get(requestctx.ProviderResponseHeadersContextKey)
			if !ok || providerResponseHeaders.(http.Header).Get("X-Request-Id") != "chat-provider-request" {
				t.Fatalf("provider response headers were captured on the wrong context: %#v", providerResponseHeaders)
			}
			originalBody, ok := common.GetOriginalRequestBody(ctx)
			if !ok || string(originalBody) != raw {
				t.Fatalf("original Responses body ownership changed: ok=%v body=%q", ok, originalBody)
			}
			canonical, ok := common.GetCanonicalRequestBody(ctx)
			if !ok || string(canonical) == raw || !strings.Contains(string(canonical), `"messages"`) {
				t.Fatalf("provider canonical body was not Chat: ok=%v body=%q", ok, canonical)
			}
			if stream {
				if !strings.Contains(recorder.Body.String(), "response.completed") {
					t.Fatalf("stream was not converted back to Responses: %q", recorder.Body.String())
				}
			} else if !strings.Contains(recorder.Body.String(), `"object":"response"`) {
				t.Fatalf("unary response was not converted back to Responses: %q", recorder.Body.String())
			}
		})
	}
}

func TestRelayResponsesFutureUnionRequiresNonCrossProtocolPath(t *testing.T) {
	envelope, err := commonresponses.ParseRawEnvelope([]byte(`{"model":"gpt-5","input":"hello","store":false,"tools":[{"type":"future_tool","max_num_results":"future-shape"}]}`))
	if err != nil || envelope.ProjectionError == nil {
		t.Fatalf("expected a raw envelope with a partial projection, envelope=%+v err=%v", envelope, err)
	}

	crossProvider := &compatibleResponsesChatProvider{BaseProvider: providersBase.BaseProvider{
		Channel: &model.Channel{Id: 91, Type: config.ChannelTypeAnthropic, CompatibleResponse: true},
	}}
	crossRelay := &relayResponses{
		relayBase:        relayBase{provider: crossProvider, modelName: "gpt-5"},
		responsesRequest: envelope.Projection,
		rawEnvelope:      envelope,
		operation:        responsesOperationCreate,
	}
	if err := crossRelay.validateSelectedProviderRequest(); err == nil {
		t.Fatal("expected the cross-protocol adapter to reject an incomplete typed projection")
	}

	exactProvider := &compatibleResponsesChatProvider{BaseProvider: providersBase.BaseProvider{
		Channel: &model.Channel{Id: 92, Type: config.ChannelTypeOpenAI},
	}}
	exactRelay := &relayResponses{
		relayBase:        relayBase{provider: exactProvider, modelName: "gpt-5"},
		responsesRequest: envelope.Projection,
		rawEnvelope:      envelope,
		operation:        responsesOperationCreate,
	}
	if err := exactRelay.validateSelectedProviderRequest(); err != nil {
		t.Fatalf("expected exact-wire provider to accept the raw future union: %v", err)
	}
}

func TestProviderRedactionChatToResponsesPreservesBody(t *testing.T) {
	for _, test := range []struct{ name, delta, event string }{
		{"text", `{"role":"assistant","content":"provider-secret-12345"}`, "response.output_text"},
		{"tool", `{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"test","arguments":"{\"key\":\"provider-secret-12345\"}"}}]}`, "response.function_call_arguments"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			enableResponsesTestDeadline(ctx)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			requestctx.SetProviderCredentials(ctx, []string{"provider-secret-12345"})
			r := &relayResponses{relayBase: relayBase{c: ctx, provider: &compatibleResponsesChatProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{}, Usage: &types.Usage{}}}}, responsesRequest: types.OpenAIResponsesRequest{Model: "gpt-5"}}
			stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
			stream.dataChan <- `{"id":"chat_1","choices":[{"index":0,"delta":` + test.delta + `}]}`
			close(stream.dataChan)
			close(stream.errChan)
			_, final, err := r.chatToResponseStreamClient(stream)
			if err != nil {
				t.Fatal(err)
			}
			body := recorder.Body.String()
			if !strings.Contains(body, "provider-secret-12345") {
				t.Fatalf("转换正文被脱敏改变: %s", body)
			}
			for _, event := range []string{test.event + ".delta", test.event + ".done", "response.completed"} {
				if !strings.Contains(body, "event: "+event+"\n") {
					t.Fatalf("转换事件缺失 %s: %s", event, body)
				}
			}
			rawFinal, _ := json.Marshal(final)
			if !strings.Contains(string(rawFinal), "provider-secret-12345") {
				t.Fatalf("交付脱敏修改了内部聚合证据: %s", rawFinal)
			}
		})
	}
}

func TestProviderRedactionConvertedMetadataDoesNotBlockDelivery(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	requestctx.SetProviderCredentials(ctx, []string{"provider-secret"})
	r := &relayResponses{relayBase: relayBase{c: ctx, provider: &compatibleResponsesChatProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{}, Usage: &types.Usage{}}}}, responsesRequest: types.OpenAIResponsesRequest{Metadata: map[string]string{"provider-secret": "value"}}}
	stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
	stream.dataChan <- `{"id":"chat_1","choices":[{"index":0,"delta":{"content":"hello"}}]}`
	close(stream.dataChan)
	close(stream.errChan)
	_, final, err := r.chatToResponseStreamClient(stream)
	if err != nil || final == nil || recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "provider-secret") {
		t.Fatalf("正文 key 与凭据相同不应拒绝: body=%q err=%v", recorder.Body.String(), err)
	}
}

func TestProviderRedactionDoesNotBlockConvertedErrorDelivery(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	requestctx.SetProviderCredentials(ctx, []string{"sequence_number"})
	r := &relayResponses{relayBase: relayBase{c: ctx, provider: &compatibleResponsesChatProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{}, Usage: &types.Usage{}}}}}
	stream := &fakeRelayStream{dataChan: make(chan string), errChan: make(chan error, 1)}
	close(stream.dataChan)
	stream.errChan <- errors.New("upstream interrupted")
	close(stream.errChan)
	_, _, err := r.chatToResponseStreamClient(stream)
	if err == nil || !ctx.Writer.Written() || !ctx.GetBool(responsesStreamErrorAlreadyRenderedContextKey) || !strings.Contains(recorder.Body.String(), "stream interrupted") {
		t.Fatalf("脱敏不应拒绝错误事件交付: body=%q err=%v", recorder.Body.String(), err)
	}
}
