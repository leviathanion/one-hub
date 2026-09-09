package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestRelayChatSetRequestRequiresOnlyProxyOwnedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{name: "missing model", body: `{"messages":[{"role":"user","content":"hi"}]}`, wantField: "Model"},
		{name: "blank model", body: `{"model":"  ","messages":[{"role":"user","content":"hi"}]}`, wantField: "Model"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.body))
			ctx.Request.Header.Set("Content-Type", "application/json")

			relay := NewRelayChat(ctx)
			err := relay.setRequest()
			if err == nil || !strings.Contains(err.Error(), "field "+tt.wantField+" is required") {
				t.Fatalf("expected required %s validation error, got %v", tt.wantField, err)
			}
			apiErr := wrapRelaySetupError(relay, "request", err, "one_hub_error", http.StatusBadRequest)
			if apiErr.StatusCode != http.StatusBadRequest || !apiErr.LocalError {
				t.Fatalf("expected a local 400 setup error, got %+v", apiErr)
			}
		})
	}
}

func TestRelayChatSetRequestLeavesMessagesValidationToProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{
		`{"model":"gpt-4o"}`,
		`{"model":"gpt-4o","messages":[]}`,
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")

		relay := NewRelayChat(ctx)
		if err := relay.setRequest(); err != nil {
			t.Fatalf("proxy rejected provider-owned messages semantics for %s: %v", body, err)
		}
	}
}

func (p *mainTestProvider) GetRequestHeaders() map[string]string {
	return nil
}

func TestNonStreamingRelayDoesNotStartHeartbeat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("token_setting", &model.TokenSetting{Heartbeat: model.HeartbeatSetting{Enabled: true, TimeoutSeconds: 1}})

	relay := &relayBase{allowHeartbeat: true, c: ctx}
	if heartbeat := relay.SetHeartbeat(false); heartbeat != nil {
		heartbeat.Close()
		t.Fatal("expected non-streaming response to keep its status uncommitted")
	}
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatalf("expected no heartbeat bytes or committed response, code=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestRelayBaseReplaysAuthorizedExactWireRedirect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	relay := &relayBase{c: ctx}
	relay.HandleJsonError(&types.OpenAIErrorWithStatusCode{
		OpenAIError:       types.OpenAIError{Code: "provider_redirect_response"},
		StatusCode:        http.StatusTemporaryRedirect,
		ReplayRawResponse: true,
		RawBody:           []byte("redirect body\n"),
		ResponseHeaders: http.Header{
			"Content-Type": {"text/plain"},
			"Location":     {"/target"},
			"Set-Cookie":   {"provider-secret"},
		},
	})
	if recorder.Code != http.StatusTemporaryRedirect || recorder.Body.String() != "redirect body\n" || recorder.Header().Get("Location") != "/target" {
		t.Fatalf("exact redirect was normalized: status=%d header=%v body=%q", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	if recorder.Header().Get("Set-Cookie") != "" {
		t.Fatalf("unsafe provider header leaked: %v", recorder.Header())
	}
}

func TestChatUsageChunkPreservesStreamEnvelope(t *testing.T) {
	provider := &mainTestProvider{BaseProvider: providersBase.BaseProvider{}}
	usage := &types.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8}
	usage.MarkProviderReported()
	provider.SetUsage(usage)
	relay := &relayChat{
		relayBase: relayBase{provider: provider},
		chatRequest: types.ChatCompletionRequest{
			Model:         "client-model",
			StreamOptions: &types.StreamOptions{IncludeUsage: true},
		},
	}
	relay.observeStreamResponseMetadata(`{"id":"chatcmpl-provider","object":"chat.completion.chunk","created":1700000000,"model":"provider-model","service_tier":"priority","choices":[{"index":0,"delta":{"content":"hi"}}]}`)

	var chunk types.ChatCompletionStreamResponse
	if err := json.Unmarshal([]byte(relay.getUsageResponse()), &chunk); err != nil {
		t.Fatalf("decode usage chunk: %v", err)
	}
	created, createdOK := chunk.Created.(float64)
	if chunk.ID != "chatcmpl-provider" || chunk.Object != "chat.completion.chunk" || !createdOK || created != 1700000000 || chunk.Model != "provider-model" || chunk.ServiceTier != "priority" {
		t.Fatalf("expected provider stream envelope on terminal usage chunk, got %+v", chunk)
	}
	if chunk.Usage == nil || chunk.Usage.TotalTokens != 8 || len(chunk.Choices) != 0 {
		t.Fatalf("expected proxy-settled usage with empty choices, got %+v", chunk)
	}
}

func TestChatUsageChunkDoesNotExposeLocalEstimate(t *testing.T) {
	provider := &mainTestProvider{BaseProvider: providersBase.BaseProvider{}}
	provider.SetUsage(&types.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8})
	relay := &relayChat{
		relayBase: relayBase{provider: provider},
		chatRequest: types.ChatCompletionRequest{
			StreamOptions: &types.StreamOptions{IncludeUsage: true},
		},
	}
	if got := relay.getUsageResponse(); got != "" {
		t.Fatalf("local estimate escaped as provider usage: %s", got)
	}
}

func TestChatUsageChunkIsNotSynthesizedAfterProviderDeliveredUsage(t *testing.T) {
	relay := &relayChat{
		chatRequest: types.ChatCompletionRequest{
			StreamOptions: &types.StreamOptions{IncludeUsage: true},
		},
	}
	relay.observeStreamResponseMetadata(`{"id":"chatcmpl-provider","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)
	if got := relay.getUsageResponse(); got != "" {
		t.Fatalf("provider usage chunk was duplicated with %s", got)
	}
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

type retryProviderValidationRelay struct {
	retryProviderSetupFailureRelay
	validationCalls int
	validationErr   error
}

type representabilityFallbackRelay struct {
	retryProviderSetupFailureRelay
	candidates []*mainTestProvider
	index      int
	setCalls   int
}

func (r *representabilityFallbackRelay) setProvider(string) error {
	r.setCalls++
	r.index++
	if r.index >= len(r.candidates) {
		return errors.New("no channel available")
	}
	r.provider = r.candidates[r.index]
	return nil
}

func (r *representabilityFallbackRelay) validateSelectedProviderRequest() error {
	if r.index < len(r.candidates)-1 {
		return fmt.Errorf("channel %d cannot represent request", r.candidates[r.index].GetChannel().Id)
	}
	return nil
}

func (r *retryProviderValidationRelay) validateSelectedProviderRequest() error {
	r.validationCalls++
	return r.validationErr
}

func TestFinalizeSelectedProviderRequestDoesNotCreateSecondSelectionLoop(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	candidates := make([]*mainTestProvider, 5)
	for index := range candidates {
		candidates[index] = &mainTestProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: index + 1, Type: config.ChannelTypeOpenAI}}}
	}
	relay := &representabilityFallbackRelay{
		retryProviderSetupFailureRelay: retryProviderSetupFailureRelay{c: ctx, provider: candidates[0]},
		candidates:                     candidates,
	}
	if err := finalizeSelectedProviderRequest(relay); err == nil || !strings.Contains(err.Error(), "cannot represent") {
		t.Fatalf("expected selected candidate validation error, got %v", err)
	}
	if relay.getProvider().GetChannel().Id != 1 || relay.setCalls != 0 {
		t.Fatalf("finalization re-entered selection: channel=%d selections=%d", relay.getProvider().GetChannel().Id, relay.setCalls)
	}
}

func TestFinalizeSelectedProviderRequestDoesNotRerouteExplicitPin(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("specific_channel_id", 1)
	candidates := []*mainTestProvider{
		{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 1, Type: config.ChannelTypeOpenAI}}},
		{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 2, Type: config.ChannelTypeOpenAI}}},
	}
	relay := &representabilityFallbackRelay{
		retryProviderSetupFailureRelay: retryProviderSetupFailureRelay{c: ctx, provider: candidates[0]}, candidates: candidates,
	}
	if err := finalizeSelectedProviderRequest(relay); err == nil || !strings.Contains(err.Error(), "cannot represent") {
		t.Fatalf("expected pinned candidate representability error, got %v", err)
	}
	if relay.setCalls != 0 {
		t.Fatalf("expected explicit pin not to try another channel, selections=%d", relay.setCalls)
	}
}

type acceptedDecodeFailureRelay struct {
	retryProviderSetupFailureRelay
}

func (r *acceptedDecodeFailureRelay) send() (*types.OpenAIErrorWithStatusCode, bool) {
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError:      types.OpenAIError{Code: "decode_response_failed", Message: "malformed provider response"},
		StatusCode:       http.StatusInternalServerError,
		UpstreamAccepted: true,
	}, false
}

func (r *acceptedDecodeFailureRelay) getModelName() string {
	return "gpt-5"
}

func (r *acceptedDecodeFailureRelay) getOriginalModel() string {
	return "gpt-5"
}

func TestRelayHandlerAcceptedDecodeFailureWithoutUsageCancels(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	relay := &acceptedDecodeFailureRelay{retryProviderSetupFailureRelay: retryProviderSetupFailureRelay{
		c: ctx,
		provider: &mainTestProvider{BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI},
		}},
	}}

	apiErr, _ := RelayHandler(relay)
	if apiErr == nil || !apiErr.UpstreamAccepted {
		t.Fatalf("expected accepted decode failure to surface, got %+v", apiErr)
	}
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 1000 || user.UsedQuota != 0 || token.RemainQuota != 1000 || token.UsedQuota != 0 {
		t.Fatalf("expected accepted request without usage to cancel, user=%+v token=%+v", user, token)
	}
}

type ambiguousTransportFailureRelay struct {
	retryProviderSetupFailureRelay
}

type notAttemptedTransportFailureRelay struct {
	retryProviderSetupFailureRelay
}

type workActionProviderResponseRelay struct {
	retryProviderSetupFailureRelay
	status           int
	sendCalls        int
	setProviderCalls int
}

func (r *workActionProviderResponseRelay) send() (*types.OpenAIErrorWithStatusCode, bool) {
	r.sendCalls++
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "provider_error", Message: "provider returned an error"},
		StatusCode:  r.status,
	}, false
}

func (r *workActionProviderResponseRelay) setProvider(string) error {
	r.setProviderCalls++
	return nil
}

func (r *workActionProviderResponseRelay) getModelName() string     { return "gpt-5" }
func (r *workActionProviderResponseRelay) getOriginalModel() string { return "gpt-5" }

type sideEffectFreeObservationRelay struct {
	retryProviderSetupFailureRelay
	sendCalls        int
	setProviderCalls int
}

func (r *sideEffectFreeObservationRelay) skipQuotaSettlement() bool                  { return true }
func (r *sideEffectFreeObservationRelay) allowsSideEffectFreeObservationRetry() bool { return true }
func (r *sideEffectFreeObservationRelay) send() (*types.OpenAIErrorWithStatusCode, bool) {
	r.sendCalls++
	if r.sendCalls == 1 {
		return &types.OpenAIErrorWithStatusCode{
			OpenAIError:       types.OpenAIError{Code: "observation_unavailable", Message: "temporary observation failure"},
			StatusCode:        http.StatusServiceUnavailable,
			UpstreamAmbiguous: true,
		}, false
	}
	return nil, false
}

func (r *sideEffectFreeObservationRelay) setProvider(string) error {
	r.setProviderCalls++
	return nil
}

func (r *notAttemptedTransportFailureRelay) send() (*types.OpenAIErrorWithStatusCode, bool) {
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError:          types.OpenAIError{Code: "http_request_failed", Message: "connection refused before write"},
		StatusCode:           http.StatusInternalServerError,
		UpstreamNotAttempted: true,
	}, false
}

func (r *notAttemptedTransportFailureRelay) getModelName() string     { return "gpt-5" }
func (r *notAttemptedTransportFailureRelay) getOriginalModel() string { return "gpt-5" }

func (r *ambiguousTransportFailureRelay) send() (*types.OpenAIErrorWithStatusCode, bool) {
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError:       types.OpenAIError{Code: "http_request_failed", Message: "connection reset after write"},
		StatusCode:        http.StatusInternalServerError,
		UpstreamAmbiguous: true,
	}, false
}

func (r *ambiguousTransportFailureRelay) getModelName() string     { return "gpt-5" }
func (r *ambiguousTransportFailureRelay) getOriginalModel() string { return "gpt-5" }

func TestRelayHandlerAmbiguousTransportWithoutUsageCancels(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	relay := &ambiguousTransportFailureRelay{retryProviderSetupFailureRelay: retryProviderSetupFailureRelay{
		c: ctx,
		provider: &mainTestProvider{BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI},
		}},
	}}

	apiErr, _ := RelayHandler(relay)
	if apiErr == nil || !apiErr.UpstreamAmbiguous {
		t.Fatalf("expected ambiguous transport failure to surface, got %+v", apiErr)
	}
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 1000 || user.UsedQuota != 0 || token.RemainQuota != 1000 || token.UsedQuota != 0 {
		t.Fatalf("expected ambiguous execution without usage to cancel, user=%+v token=%+v", user, token)
	}
}

func TestRelayHandlerNotAttemptedTransportAfterSubmissionClaimCancels(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	relay := &notAttemptedTransportFailureRelay{retryProviderSetupFailureRelay: retryProviderSetupFailureRelay{
		c: ctx,
		provider: &mainTestProvider{BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI},
		}},
	}}

	apiErr, _ := RelayHandler(relay)
	if apiErr == nil || !apiErr.UpstreamNotAttempted || apiErr.UpstreamAmbiguous {
		t.Fatalf("expected not-attempted transport failure to surface, got %+v", apiErr)
	}
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 1000 || user.UsedQuota != 0 || token.RemainQuota != 1000 || token.UsedQuota != 0 {
		t.Fatalf("expected not-attempted request without usage to cancel, user=%+v token=%+v", user, token)
	}
}

func TestExecuteRelayAttemptsNeverReplaysClaimedWorkActionProviderResponse(t *testing.T) {
	tests := []struct {
		status   int
		wantCode any
	}{
		{status: http.StatusBadRequest, wantCode: "provider_error"},
		{status: http.StatusUnauthorized, wantCode: "provider_account_error"},
		{status: http.StatusTooManyRequests, wantCode: "provider_error"},
		{status: http.StatusInternalServerError, wantCode: "provider_error"},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			ctx := setupResponsesWSQuotaFixture(t, 1000)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			ctx.Set("requestStartTime", time.Now())
			relay := &workActionProviderResponseRelay{
				retryProviderSetupFailureRelay: retryProviderSetupFailureRelay{
					c: ctx,
					provider: &mainTestProvider{BaseProvider: providersBase.BaseProvider{
						Channel: &model.Channel{Id: 17, Name: "work-action", Type: config.ChannelTypeOpenAI},
					}},
				},
				status: test.status,
			}

			originalHandler := relayHandlerFunc
			originalProcess := processChannelRelayErrorFunc
			originalShouldRetry := shouldRetryFunc
			originalRetryTimes := config.RetryTimes
			originalRetryTimeout := config.RetryTimeOut
			retryChecks := 0
			relayHandlerFunc = RelayHandler
			processChannelRelayErrorFunc = func(context.Context, int, string, *types.OpenAIErrorWithStatusCode, int) {}
			shouldRetryFunc = func(*gin.Context, *types.OpenAIErrorWithStatusCode, int) bool {
				retryChecks++
				return true
			}
			config.RetryTimes = 3
			config.RetryTimeOut = 60
			t.Cleanup(func() {
				relayHandlerFunc = originalHandler
				processChannelRelayErrorFunc = originalProcess
				shouldRetryFunc = originalShouldRetry
				config.RetryTimes = originalRetryTimes
				config.RetryTimeOut = originalRetryTimeout
			})

			apiErr := executeRelayAttempts(relay)
			if apiErr == nil || apiErr.StatusCode != test.status || apiErr.Code != test.wantCode {
				t.Fatalf("expected safe provider HTTP %d error, got %+v", test.status, apiErr)
			}
			if relay.sendCalls != 1 || relay.setProviderCalls != 0 || retryChecks != 0 {
				t.Fatalf("claimed Work Action was reconsidered: sends=%d selections=%d retry_checks=%d", relay.sendCalls, relay.setProviderCalls, retryChecks)
			}
		})
	}
}

func TestExecuteRelayAttemptsRetriesExplicitSideEffectFreeObservation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", nil)
	ctx.Set("requestStartTime", time.Now())
	relay := &sideEffectFreeObservationRelay{retryProviderSetupFailureRelay: retryProviderSetupFailureRelay{
		c: ctx,
		provider: &mainTestProvider{BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 17, Name: "observation", Type: config.ChannelTypeOpenAI},
		}},
	}}

	originalHandler := relayHandlerFunc
	originalProcess := processChannelRelayErrorFunc
	originalShouldRetry := shouldRetryFunc
	originalShouldCooldowns := shouldCooldownsFunc
	originalRetryTimes := config.RetryTimes
	originalRetryTimeout := config.RetryTimeOut
	relayHandlerFunc = RelayHandler
	processChannelRelayErrorFunc = func(context.Context, int, string, *types.OpenAIErrorWithStatusCode, int) {}
	shouldRetryFunc = func(*gin.Context, *types.OpenAIErrorWithStatusCode, int) bool { return true }
	shouldCooldownsFunc = func(*gin.Context, *model.Channel, *types.OpenAIErrorWithStatusCode) {}
	config.RetryTimes = 1
	config.RetryTimeOut = 60
	t.Cleanup(func() {
		relayHandlerFunc = originalHandler
		processChannelRelayErrorFunc = originalProcess
		shouldRetryFunc = originalShouldRetry
		shouldCooldownsFunc = originalShouldCooldowns
		config.RetryTimes = originalRetryTimes
		config.RetryTimeOut = originalRetryTimeout
	})

	if apiErr := executeRelayAttempts(relay); apiErr != nil {
		t.Fatalf("side-effect-free observation retry did not recover: %+v", apiErr)
	}
	if relay.sendCalls != 2 || relay.setProviderCalls != 1 {
		t.Fatalf("observation retry budget was not exact: sends=%d selections=%d", relay.sendCalls, relay.setProviderCalls)
	}
}

type unconfirmedServiceTierRelay struct {
	retryProviderSetupFailureRelay
	request types.OpenAIResponsesRequest
}

func (r *unconfirmedServiceTierRelay) getRequest() any      { return &r.request }
func (r *unconfirmedServiceTierRelay) getModelName() string { return "gpt-5" }

func TestRelayHandlerDoesNotTreatRequestedServiceTierAsBillingEvidence(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	provider := &mainTestProvider{BaseProvider: providersBase.BaseProvider{
		Channel: &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI},
	}}
	relay := &unconfirmedServiceTierRelay{
		retryProviderSetupFailureRelay: retryProviderSetupFailureRelay{c: ctx, provider: provider},
		request:                        types.OpenAIResponsesRequest{Model: "gpt-5", ServiceTier: "priority"},
	}

	apiErr, _ := RelayHandler(relay)
	if apiErr != nil {
		t.Fatalf("relay handler: %v", apiErr)
	}
	if usage := provider.GetUsage(); usage == nil || usage.ServiceTier != "" {
		t.Fatalf("requested tier became final billing evidence: %+v", usage)
	}
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 1000 || token.RemainQuota != 1000 {
		t.Fatalf("successful response without provider usage was charged: user=%+v token=%+v", user, token)
	}
}

type providerUsageRelay struct {
	retryProviderSetupFailureRelay
}

func (r *providerUsageRelay) send() (*types.OpenAIErrorWithStatusCode, bool) {
	usage := r.provider.GetUsage()
	usage.PromptTokens = 10
	usage.CompletionTokens = 20
	usage.TotalTokens = 30
	usage.MarkProviderReported()
	return nil, true
}

func (r *providerUsageRelay) getModelName() string     { return "gpt-5" }
func (r *providerUsageRelay) getOriginalModel() string { return "gpt-5" }

func TestRelayHandlerConfirmsProviderUsage(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	provider := &mainTestProvider{BaseProvider: providersBase.BaseProvider{
		Channel: &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI},
	}}
	relay := &providerUsageRelay{retryProviderSetupFailureRelay: retryProviderSetupFailureRelay{c: ctx, provider: provider}}
	if apiErr, _ := RelayHandler(relay); apiErr != nil {
		t.Fatalf("relay handler: %v", apiErr)
	}
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 900 || user.UsedQuota != 100 || token.RemainQuota != 900 || token.UsedQuota != 100 {
		t.Fatalf("provider usage was not confirmed with the old calculator: user=%+v token=%+v", user, token)
	}
}

func TestExecuteRelayAttemptsSanitizesProviderErrorBeforeControlAndRender(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	provider := &mainTestProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{
		Id: 17, Name: "embedded-error", Type: config.ChannelTypeOpenAI,
	}}}
	relay := &retryProviderSetupFailureRelay{c: ctx, provider: provider}

	originalHandler := relayHandlerFunc
	originalProcess := processChannelRelayErrorFunc
	originalShouldRetry := shouldRetryFunc
	originalRetryTimes := config.RetryTimes
	processed := make(chan *types.OpenAIErrorWithStatusCode, 1)
	relayHandlerFunc = func(RelayBaseInterface) (*types.OpenAIErrorWithStatusCode, bool) {
		return &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{
				Type: "authentication_error", Code: "invalid_api_key",
				Message: "Authorization: Bearer sk-provider-secret belongs to org-secret",
			},
			StatusCode:        http.StatusBadRequest,
			RawBody:           []byte(`{"error":{"message":"Authorization: Bearer sk-provider-secret belongs to org-secret"}}`),
			ReplayRawResponse: true,
		}, false
	}
	processChannelRelayErrorFunc = func(_ context.Context, _ int, _ string, apiErr *types.OpenAIErrorWithStatusCode, _ int) {
		processed <- apiErr
	}
	shouldRetryFunc = func(*gin.Context, *types.OpenAIErrorWithStatusCode, int) bool { return false }
	config.RetryTimes = 0
	t.Cleanup(func() {
		relayHandlerFunc = originalHandler
		processChannelRelayErrorFunc = originalProcess
		shouldRetryFunc = originalShouldRetry
		config.RetryTimes = originalRetryTimes
	})

	apiErr := executeRelayAttempts(relay)
	if apiErr == nil || apiErr.Code != "provider_account_error" || strings.Contains(apiErr.Message, "provider-secret") || len(apiErr.RawBody) != 0 || apiErr.ReplayRawResponse {
		t.Fatalf("provider error was not sanitized before render: %+v", apiErr)
	}
	select {
	case controlled := <-processed:
		if controlled.Code != "provider_account_error" || strings.Contains(controlled.Message, "provider-secret") {
			t.Fatalf("control path received unsanitized provider error: %+v", controlled)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for provider control processing")
	}
}

func TestCandidateOverlayFiltersOnlyChatBeforeSelection(t *testing.T) {
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
	if err := relay.setRequest(); err != nil {
		t.Fatalf("setRequest failed: %v", err)
	}
	ctx.Set("is_stream", relay.IsStream())
	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		t.Fatalf("setProvider failed: %v", err)
	}
	if got := relay.getProvider().GetChannel().Id; got != 2 {
		t.Fatalf("expected candidate overlay to filter channel 1 before selection, got %d", got)
	}
	if common.GetRequestBodyReparseNeeded(ctx) {
		t.Fatal("unselected channel transform leaked into the selected request")
	}
	if err := finalizeSelectedProviderRequest(relay); err != nil {
		t.Fatalf("finalizeSelectedProviderRequest failed: %v", err)
	}
	if got := relay.getProvider().GetChannel().Id; got != 2 {
		t.Fatalf("expected transformed tool request to be re-selected onto non-OnlyChat channel, got %d", got)
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

func TestPreMappingCapabilityDoesNotFilterNextCandidate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	weight := uint(1)
	proxy := ""
	preAdd := `{"pre_add":true,"parallel_tool_calls":false,"tools":[{"type":"function","function":{"name":"lookup_weather","parameters":{"type":"object"}}}]}`
	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{
			1: {Channel: &model.Channel{
				Id: 1, Type: config.ChannelTypeAnthropic, Status: config.ChannelStatusEnabled,
				Group: "default", Models: "gpt-4o", Weight: &weight, Proxy: &proxy,
				CustomParameter: &preAdd,
			}},
			2: {Channel: &model.Channel{
				Id: 2, Type: config.ChannelTypeAnthropic, Status: config.ChannelStatusEnabled,
				Group: "default", Models: "gpt-4o", Weight: &weight, Proxy: &proxy,
				OnlyChat: true,
			}},
		},
		Rule: map[string]map[string][][]int{
			"default": {"gpt-4o": {{1}, {2}}},
		},
		ModelGroup: map[string]map[string]bool{
			"gpt-4o": {"default": true},
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("token_group", "default")

	relay := NewRelayChat(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("setRequest failed: %v", err)
	}
	ctx.Set("is_stream", relay.IsStream())
	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		t.Fatalf("setProvider failed: %v", err)
	}
	if got := relay.getProvider().GetChannel().Id; got != 2 {
		t.Fatalf("expected candidate 1 transform to be rejected before selection, got %d", got)
	}

	if err := finalizeSelectedProviderRequest(relay); err != nil {
		t.Fatalf("finalizeSelectedProviderRequest failed: %v", err)
	}
	if got := relay.getProvider().GetChannel().Id; got != 2 {
		t.Fatalf("expected original request to select candidate 2, got %d", got)
	}
	if relay.chatRequest.ParallelToolCalls != nil {
		t.Fatalf("candidate 1 pre-mapping leaked into candidate 2 request: %+v", relay.chatRequest.ParallelToolCalls)
	}
	if relay.chatRequest.Tools != nil || ctx.GetBool("skip_only_chat") {
		t.Fatalf("candidate 1 tool state leaked into OnlyChat candidate 2: tools=%#v skip_only_chat=%t", relay.chatRequest.Tools, ctx.GetBool("skip_only_chat"))
	}
	requestMap, err := common.CloneReusableBodyMap(ctx)
	if err != nil {
		t.Fatalf("CloneReusableBodyMap failed: %v", err)
	}
	if _, exists := requestMap["parallel_tool_calls"]; exists {
		t.Fatalf("candidate 1 pre-mapping leaked into candidate 2 body: %#v", requestMap)
	}
	if _, exists := requestMap["tools"]; exists {
		t.Fatalf("candidate 1 tools leaked into candidate 2 body: %#v", requestMap)
	}
}

func TestSelectedCandidateOverlayMaterializesWithoutReenteringRequestPlanning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(channelGroupSnapshot) })

	weight := uint(1)
	proxy := ""
	preAdd := `{"pre_add":true,"temperature":0.25}`
	channel := &model.Channel{
		Id: 41, Type: config.ChannelTypeOpenAI, Status: config.ChannelStatusEnabled,
		Group: "default", Models: "gpt-4o", Weight: &weight, Proxy: &proxy,
		CustomParameter: &preAdd,
	}
	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{channel.Id: {Channel: channel}},
		Rule:     map[string]map[string][][]int{"default": {"gpt-4o": {{channel.Id}}}},
		ModelGroup: map[string]map[string]bool{
			"gpt-4o": {"default": true},
		},
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	ctx.Set("token_group", "default")
	relay := NewRelayChat(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatal(err)
	}
	initialAffinityState := currentChannelAffinityState(ctx)
	ctx.Set("is_stream", relay.IsStream())
	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		t.Fatal(err)
	}
	if err := finalizeSelectedProviderRequest(relay); err != nil {
		t.Fatal(err)
	}
	if relay.chatRequest.Temperature == nil || *relay.chatRequest.Temperature != 0.25 {
		t.Fatalf("selected overlay was not materialized: %+v", relay.chatRequest.Temperature)
	}
	if relay.getOriginalModel() != "gpt-4o" || relay.getProvider().GetChannel().Id != channel.Id {
		t.Fatalf("materialization changed planning facts: model=%q channel=%d", relay.getOriginalModel(), relay.getProvider().GetChannel().Id)
	}
	if currentChannelAffinityState(ctx) != initialAffinityState {
		t.Fatal("selected overlay re-entered affinity planning")
	}
}

func TestChatRoutingSkipsOpenAIDataResidencyEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name         string
		requestModel string
		mapping      string
		wantModel    string
	}{
		{name: "direct model", requestModel: "gpt-5.6-sol", wantModel: "gpt-5.6-sol"},
		{name: "mapped alias", requestModel: "sol-alias", mapping: `{"sol-alias":"gpt-5.6-sol"}`, wantModel: "gpt-5.6-sol"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channelGroupSnapshot := snapshotChannelGroup()
			t.Cleanup(func() {
				restoreChannelGroup(channelGroupSnapshot)
			})

			weight := uint(1)
			proxy := ""
			regionalBaseURL := "https://eu.api.openai.com"
			var mapping *string
			if test.mapping != "" {
				mappingValue := test.mapping
				mapping = &mappingValue
			}
			model.ChannelGroup = model.ChannelsChooser{
				Channels: map[int]*model.ChannelChoice{
					1: {
						Channel: &model.Channel{
							Id:           1,
							Type:         config.ChannelTypeOpenAI,
							Key:          "regional-openai-key",
							Status:       config.ChannelStatusEnabled,
							Group:        "default",
							Models:       test.requestModel,
							Weight:       &weight,
							Proxy:        &proxy,
							BaseURL:      &regionalBaseURL,
							ModelMapping: mapping,
						},
					},
					2: {
						Channel: &model.Channel{
							Id:           2,
							Type:         config.ChannelTypeOpenAI,
							Key:          "openai-key",
							Status:       config.ChannelStatusEnabled,
							Group:        "default",
							Models:       test.requestModel,
							Weight:       &weight,
							Proxy:        &proxy,
							ModelMapping: mapping,
						},
					},
				},
				Rule: map[string]map[string][][]int{
					"default": {
						test.requestModel: {{1}, {2}},
					},
				},
				ModelGroup: map[string]map[string]bool{
					test.requestModel: {"default": true},
				},
			}

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, test.requestModel)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Set("token_group", "default")

			relay := NewRelayChat(ctx)
			if err := relay.setRequest(); err != nil {
				t.Fatalf("setRequest failed: %v", err)
			}
			ctx.Set("is_stream", relay.IsStream())
			if err := relay.setProvider(relay.getOriginalModel()); err != nil {
				t.Fatalf("setProvider should fall back to an eligible channel: %v", err)
			}
			if got := relay.getProvider().GetChannel().Id; got != 2 {
				t.Fatalf("expected official OpenAI channel, got %d", got)
			}
			if got := relay.getModelName(); got != test.wantModel {
				t.Fatalf("expected canonical model %q, got %q", test.wantModel, got)
			}
		})
	}
}

func TestProviderSelectionPreservesCapabilityErrorWhenAllChannelsReject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(channelGroupSnapshot) })

	weight := uint(1)
	proxy := ""
	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{1: {Channel: &model.Channel{
			Id: 1, Type: config.ChannelTypeAnthropic, Key: "anthropic-key", Status: config.ChannelStatusEnabled,
			Group: "default", Models: "generic-chat-model", Weight: &weight, Proxy: &proxy,
		}}},
		Rule:       map[string]map[string][][]int{"default": {"generic-chat-model": {{1}}}},
		ModelGroup: map[string]map[string]bool{"generic-chat-model": {"default": true}},
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"generic-chat-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"shell"}]}`))
	ctx.Set("token_group", "default")
	relay := NewRelayChat(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("setRequest: %v", err)
	}
	err := relay.setProvider(relay.getOriginalModel())
	apiErr := capabilityGateAPIError(err)
	if apiErr == nil || openAIErrorCodeString(apiErr.Code, "") != unsupportedCapabilityCode {
		t.Fatalf("expected stable unsupported_capability error, got err=%v api=%+v", err, apiErr)
	}
}

func TestResponsesRoutingSkipsChannelWhoseCustomParametersChangeLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(channelGroupSnapshot) })

	weight := uint(1)
	proxy := ""
	customParameter := `{"store":false}`
	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{
			1: {Channel: &model.Channel{
				Id: 1, Type: config.ChannelTypeOpenAI, Key: "incompatible-key", Status: config.ChannelStatusEnabled,
				Group: "default", Models: "gpt-5", Weight: &weight, Proxy: &proxy, CustomParameter: &customParameter,
			}},
			2: {Channel: &model.Channel{
				Id: 2, Type: config.ChannelTypeOpenAI, Key: "compatible-key", Status: config.ChannelStatusEnabled,
				Group: "default", Models: "gpt-5", Weight: &weight, Proxy: &proxy,
			}},
		},
		Rule:       map[string]map[string][][]int{"default": {"gpt-5": {{1}, {2}}}},
		ModelGroup: map[string]map[string]bool{"gpt-5": {"default": true}},
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hello"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("token_group", "default")

	relay := NewRelayResponses(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("setRequest failed: %v", err)
	}
	ctx.Set("is_stream", relay.IsStream())
	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		t.Fatalf("setProvider should fall back to a lifecycle-compatible channel: %v", err)
	}
	if got := relay.getProvider().GetChannel().Id; got != 2 {
		t.Fatalf("expected channel 2 after lifecycle capability filtering, got %d", got)
	}
}

func TestChatRoutingSkipsAdapterThatCannotRepresentCustomTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(channelGroupSnapshot) })

	weight := uint(1)
	proxy := ""
	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{
			1: {Channel: &model.Channel{
				Id: 1, Type: config.ChannelTypeAnthropic, Key: "anthropic-key", Status: config.ChannelStatusEnabled,
				Group: "default", Models: "generic-chat-model", Weight: &weight, Proxy: &proxy,
			}},
			2: {Channel: &model.Channel{
				Id: 2, Type: config.ChannelTypeOpenAI, Key: "openai-key", Status: config.ChannelStatusEnabled,
				Group: "default", Models: "generic-chat-model", Weight: &weight, Proxy: &proxy,
			}},
		},
		Rule:       map[string]map[string][][]int{"default": {"generic-chat-model": {{1}, {2}}}},
		ModelGroup: map[string]map[string]bool{"generic-chat-model": {"default": true}},
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"generic-chat-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"shell"}]}`))
	ctx.Set("token_group", "default")
	relay := NewRelayChat(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("setRequest: %v", err)
	}
	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		t.Fatalf("setProvider should select a lossless adapter: %v", err)
	}
	if got := relay.getProvider().GetChannel().Id; got != 2 {
		t.Fatalf("expected custom tool request to skip Claude conversion and use OpenAI wire, got channel %d", got)
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
					Channel: &model.Channel{Id: 41, Name: "responses-primary", Type: config.ChannelTypeOpenAI},
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
	upstreamMiss := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Code:    "previous_response_not_found",
			Message: "provider-specific previous response miss",
			Type:    "invalid_request_error",
			Param:   "provider_previous_response_id",
		},
		StatusCode:      http.StatusNotFound,
		RawBody:         []byte(`{"error":{"message":"provider-specific previous response miss","code":"previous_response_not_found"}}`),
		ResponseHeaders: http.Header{"X-Request-Id": []string{"req-upstream-continuation-miss"}},
	}
	relayHandlerFunc = func(relay RelayBaseInterface) (*types.OpenAIErrorWithStatusCode, bool) {
		handlerCalls++
		return upstreamMiss, false
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
	if apiErr != upstreamMiss {
		t.Fatalf("expected continuation miss to preserve the original provider error, got %+v", apiErr)
	}
	if apiErr.StatusCode != http.StatusNotFound || apiErr.LocalError {
		t.Fatalf("expected provider status and error origin to remain unchanged, got %+v", apiErr)
	}
	if string(apiErr.RawBody) != string(upstreamMiss.RawBody) || apiErr.ResponseHeaders.Get("X-Request-Id") != "req-upstream-continuation-miss" {
		t.Fatalf("expected provider body and headers to remain unchanged, got body=%q headers=%v", apiErr.RawBody, apiErr.ResponseHeaders)
	}
	if apiErr.Param != "provider_previous_response_id" {
		t.Fatalf("expected provider error fields to remain unchanged, got %+v", apiErr.OpenAIError)
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

func TestExecuteRelayAttemptsRevalidatesProviderRequestAfterRetrySelection(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("requestStartTime", time.Now())

	relay := &retryProviderValidationRelay{
		retryProviderSetupFailureRelay: retryProviderSetupFailureRelay{
			c: ctx,
			provider: &mainTestProvider{BaseProvider: providersBase.BaseProvider{
				Channel: &model.Channel{Id: 20, Name: "retry", Type: config.ChannelTypeCodex},
			}},
		},
		validationErr: newCapabilityGateError("n", "the selected channel cannot preserve multiple Chat choices"),
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
	relayHandlerFunc = func(RelayBaseInterface) (*types.OpenAIErrorWithStatusCode, bool) {
		handlerCalls++
		return &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{Code: "upstream_unavailable", Message: "provider unavailable", Type: "server_error"},
			StatusCode:  http.StatusBadGateway,
		}, false
	}
	processChannelRelayErrorFunc = func(context.Context, int, string, *types.OpenAIErrorWithStatusCode, int) {}
	shouldRetryFunc = func(*gin.Context, *types.OpenAIErrorWithStatusCode, int) bool { return true }
	shouldCooldownsFunc = func(*gin.Context, *model.Channel, *types.OpenAIErrorWithStatusCode) {}

	apiErr := executeRelayAttempts(relay)
	if apiErr == nil {
		t.Fatal("expected retry provider request validation error")
	}
	if apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != unsupportedCapabilityCode || apiErr.Param != "n" || !apiErr.LocalError {
		t.Fatalf("expected local unsupported capability error, got %+v", apiErr)
	}
	if relay.validationCalls != 1 {
		t.Fatalf("expected retry provider request to be validated once, got %d", relay.validationCalls)
	}
	if handlerCalls != 1 {
		t.Fatalf("invalid retry request must not be sent upstream, got %d handler calls", handlerCalls)
	}
}

func TestExecuteRelayAttemptsPreservesLastProviderErrorWhenRetryDeadlineElapsed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("requestStartTime", time.Now().Add(-time.Second))

	relay := &retryProviderSetupFailureRelay{
		c: ctx,
		provider: &mainTestProvider{BaseProvider: providersBase.BaseProvider{
			Channel: &model.Channel{Id: 18, Name: "primary", Type: config.ChannelTypeOpenAI},
		}},
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

	providerErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "upstream_unavailable", Message: "provider unavailable", Type: "server_error"},
		StatusCode:  http.StatusServiceUnavailable,
	}
	config.RetryTimes = 1
	config.RetryTimeOut = 0
	relayHandlerFunc = func(RelayBaseInterface) (*types.OpenAIErrorWithStatusCode, bool) { return providerErr, false }
	processChannelRelayErrorFunc = func(context.Context, int, string, *types.OpenAIErrorWithStatusCode, int) {}
	shouldRetryFunc = func(*gin.Context, *types.OpenAIErrorWithStatusCode, int) bool { return true }
	shouldCooldownsFunc = func(*gin.Context, *model.Channel, *types.OpenAIErrorWithStatusCode) {}

	apiErr := executeRelayAttempts(relay)
	if apiErr != providerErr {
		t.Fatalf("retry deadline must preserve the last provider response, got %+v", apiErr)
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

func TestInputTokensObservationCanRetryAmbiguousDeliveryWithoutContinuation(t *testing.T) {
	originalShouldRetry := shouldRetryFunc
	t.Cleanup(func() { shouldRetryFunc = originalShouldRetry })
	shouldRetryFunc = func(*gin.Context, *types.OpenAIErrorWithStatusCode, int) bool { return true }

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", nil)
	relay := &relayResponses{
		relayBase: relayBase{c: ctx},
		operation: responsesOperationInputTokens,
		responsesRequest: types.OpenAIResponsesRequest{
			Model: "gpt-5",
		},
	}
	ambiguous := &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusBadGateway, UpstreamAmbiguous: true}
	if !relayAttemptShouldRetry(relay, ambiguous, config.ChannelTypeOpenAI) {
		t.Fatal("side-effect-free input_tokens observation should allow retry after ambiguous delivery")
	}

	relay.responsesRequest.PreviousResponseID = "resp_owner"
	relay.strictOwnerRoute = true
	if relayAttemptShouldRetry(relay, ambiguous, config.ChannelTypeOpenAI) {
		t.Fatal("owner-bound input_tokens continuation must not switch channels")
	}
	workAction := &retryProviderSetupFailureRelay{c: ctx}
	if relayAttemptShouldRetry(workAction, ambiguous, config.ChannelTypeOpenAI) {
		t.Fatal("ambiguous Work Action must not regain submission rights")
	}
}

func TestClaimedProviderFailureStillUpdatesHealthWithoutReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("requestStartTime", time.Now())
	ctx.Set("new_model", "gpt-health")
	channel := &model.Channel{Id: 98761, Name: "rate-limited", Type: config.ChannelTypeOpenAI}
	relay := &retryProviderSetupFailureRelay{
		c: ctx,
		provider: &mainTestProvider{BaseProvider: providersBase.BaseProvider{
			Channel: channel,
		}},
	}

	originalHandler := relayHandlerFunc
	originalProcess := processChannelRelayErrorFunc
	originalRetryTimes := config.RetryTimes
	originalCooldownSeconds := config.RetryCooldownSeconds
	processed := make(chan int, 1)
	config.RetryTimes = 3
	config.RetryCooldownSeconds = 60
	relayHandlerFunc = func(RelayBaseInterface) (*types.OpenAIErrorWithStatusCode, bool) {
		return &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusTooManyRequests}, true
	}
	processChannelRelayErrorFunc = func(_ context.Context, channelID int, _ string, _ *types.OpenAIErrorWithStatusCode, _ int) {
		processed <- channelID
	}
	t.Cleanup(func() {
		relayHandlerFunc = originalHandler
		processChannelRelayErrorFunc = originalProcess
		config.RetryTimes = originalRetryTimes
		config.RetryCooldownSeconds = originalCooldownSeconds
	})

	apiErr := executeRelayAttempts(relay)
	if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected provider 429, got %+v", apiErr)
	}
	if !model.ChannelGroup.IsInCooldown(channel.Id, "gpt-health") {
		t.Fatal("claimed provider 429 did not enter model cooldown")
	}
	select {
	case channelID := <-processed:
		if channelID != channel.Id {
			t.Fatalf("health processing used channel %d, want %d", channelID, channel.Id)
		}
	case <-time.After(time.Second):
		t.Fatal("claimed provider failure skipped shared health processing")
	}
}

func TestProviderOpenRetryRequiresExplicitNoWriteEvidence(t *testing.T) {
	if !providerOpenCanRetry(&types.OpenAIErrorWithStatusCode{UpstreamNotAttempted: true, ProviderOpenRetrySafe: true}) {
		t.Fatal("pre-write handshake failure should remain retryable")
	}
	for _, apiErr := range []*types.OpenAIErrorWithStatusCode{
		{StatusCode: http.StatusServiceUnavailable},
		{UpstreamNotAttempted: true, UpstreamAmbiguous: true},
		{UpstreamNotAttempted: true, UpstreamAccepted: true},
		{UpstreamNotAttempted: true},
	} {
		if providerOpenCanRetry(apiErr) {
			t.Fatalf("unsafe provider open disposition became retryable: %+v", apiErr)
		}
	}
}
