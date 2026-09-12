package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/logger"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/internal/requesthints"
	"one-api/middleware"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/relay/relay_util"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type relayResponses struct {
	relayBase
	responsesRequest    types.OpenAIResponsesRequest
	preparedChatRequest *types.ChatCompletionRequest
	rawEnvelope         *commonresponses.RawEnvelope
	operation           responsesOperation
	strictOwnerRoute    bool
	selectedDataPath    providersBase.DataPath
}

const responsesPreviousResponseRecoveredContextKey = "responses_previous_response_recovered"

type responsesContinuationMissHandlingPlan struct {
	recoveryCandidateMeta map[string]any
}

func NewRelayResponses(c *gin.Context) *relayResponses {
	relay := &relayResponses{}
	relay.c = c
	relay.operation = detectResponsesOperation(c.Request.URL.Path)
	return relay
}

func (r *relayResponses) setProvider(modelName string) error {
	refreshPrincipal := middleware.RefreshAuthenticatedLongLivedPrincipal
	if r.operation == responsesOperationInputTokens {
		refreshPrincipal = middleware.RefreshLongLivedPrincipal
	}
	if apiErr := refreshPrincipal(r.c); apiErr != nil {
		return apiErr
	}
	if err := r.relayBase.setProvider(modelName); err != nil {
		return err
	}
	if apiErr := middleware.AdmitAuthenticatedChannelWork(r.c, modelName, r.provider.GetChannel().Id); apiErr != nil {
		return apiErr
	}
	return nil
}

func (r *relayResponses) setRequest() error {
	raw, err := common.CacheRequestBody(r.c)
	if err != nil {
		return err
	}
	envelope, err := commonresponses.ParseRawEnvelope(raw)
	if err != nil {
		return err
	}
	r.rawEnvelope = envelope
	r.responsesRequest = envelope.Projection
	if err := validateResponsesSupportedSurface(&r.responsesRequest, envelope.Object.Fields, r.operation); err != nil {
		return err
	}
	if strings.TrimSpace(r.responsesRequest.Model) == "" {
		return fmt.Errorf("field Model is required")
	}
	r.setOriginalModel(r.responsesRequest.Model)
	requireStored := r.operation == responsesOperationCreate && (r.responsesRequest.Store == nil || *r.responsesRequest.Store)
	operation := r.providerOperation()
	setRequestChannelCapability(r.c, requireResponsesRequestCompatibility(operation, requireStored, envelope.Object.Fields, r.responsesRequest.Model))
	if r.usesChannelAffinity() {
		prepareResponsesChannelAffinity(r.c, &r.responsesRequest)
	}
	continuationRoute, err := prepareResponsesContinuationOwnership(r.c, &r.responsesRequest)
	if err != nil {
		return err
	}
	r.strictOwnerRoute = continuationRoute.Strict
	return nil
}

func (r *relayResponses) WrapSetupError(_ string, err error) *types.OpenAIErrorWithStatusCode {
	var apiErr *types.OpenAIErrorWithStatusCode
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return responsesOwnershipAPIError(err)
}

func (r *relayResponses) HandleJsonError(apiErr *types.OpenAIErrorWithStatusCode) {
	if replayProviderRawResponse(r.c, apiErr, providerresponse.Policy{
		Operation:        r.providerOperation(),
		DataPath:         providerresponse.DataPathExactWire,
		BodyUnmodified:   true,
		PreserveRedirect: true,
	}) {
		return
	}
	r.relayBase.HandleJsonError(apiErr)
}

func (r *relayResponses) validateSelectedProviderRequest() error {
	if r == nil || r.provider == nil || r.provider.GetChannel() == nil {
		return nil
	}
	r.preparedChatRequest = nil
	r.selectedDataPath = ""
	operation := r.providerOperation()
	path, supported := providers.ResolveAdapterSupport(r.provider.GetChannel()).DataPath(operation)
	if !supported {
		return &capabilityGateError{message: fmt.Sprintf("channel adapter cannot relay operation %s", operation), status: http.StatusServiceUnavailable}
	}
	r.selectedDataPath = path
	if r.rawEnvelope != nil && r.rawEnvelope.ProjectionError != nil && path == providersBase.DataPathCrossProtocol {
		return newCapabilityGateError("request", "request contains fields that cannot be represented by the selected cross-protocol adapter")
	}
	if r.operation != responsesOperationCreate || path != providersBase.DataPathCrossProtocol {
		return nil
	}
	fields := map[string]json.RawMessage(nil)
	if r.rawEnvelope != nil && r.rawEnvelope.Object != nil {
		fields = r.rawEnvelope.Object.Fields
	}
	if err := validateResponsesToChatRepresentability(&r.responsesRequest, fields); err != nil {
		return err
	}
	chatRequest, err := r.responsesRequest.ToChatCompletionRequest()
	if err != nil {
		return newCapabilityGateError("request", err.Error())
	}
	chatRequest.Model = r.modelName
	chatRequest, err = materializeResponsesChatPreAdd(r.provider.GetChannel(), r.modelName, chatRequest)
	if err != nil {
		return err
	}
	r.preparedChatRequest = chatRequest
	return nil
}

func materializeResponsesChatPreAdd(channel *model.Channel, modelName string, request *types.ChatCompletionRequest) (*types.ChatCompletionRequest, error) {
	if channel == nil || request == nil {
		return request, nil
	}
	customParams, err := channel.GetCustomParameterMap()
	if err != nil {
		return nil, &capabilityGateError{message: "channel has invalid request transform configuration", status: http.StatusServiceUnavailable}
	}
	preAdd, _ := customParams["pre_add"].(bool)
	if !preAdd {
		return request, nil
	}
	// Cross-protocol pre_add targets the converted Chat request immediately
	// before provider mapping; it never mutates the source Responses envelope.
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	prepared, _, err := effectiveChatRequestForChannel(channel, modelName, request, fields)
	return prepared, err
}

func (r *relayResponses) prepareSelectedProviderRemoteMedia() error {
	if r == nil || r.preparedChatRequest == nil {
		return nil
	}
	fetcher := r.remoteMedia
	if fetcher == nil {
		fetcher = newRequestRemoteMediaFetcher(r.c.Request.Context())
	}
	if err := providers.PrepareChatRemoteMedia(r.provider, r.preparedChatRequest, fetcher); err != nil {
		return remoteMediaCapabilityGateError(err)
	}
	return r.publishPreparedChatBody()
}

// publishPreparedChatBody materializes the explicit Responses -> Chat adapter
// output for provider code that starts from the current canonical request body.
// rawEnvelope remains the immutable Responses source for retries and native
// Responses providers.
func (r *relayResponses) publishPreparedChatBody() error {
	if r == nil || r.c == nil || r.preparedChatRequest == nil {
		return nil
	}
	body, err := json.Marshal(r.preparedChatRequest)
	if err != nil {
		return fmt.Errorf("marshal prepared Chat request: %w", err)
	}
	common.SetReusableRequestBody(r.c, body)
	return nil
}

func (r *relayResponses) providerOperation() providersBase.Operation {
	if r == nil {
		return providersBase.OperationResponsesCreate
	}
	switch r.operation {
	case responsesOperationCompact:
		return providersBase.OperationResponsesCompact
	case responsesOperationInputTokens:
		return providersBase.OperationResponsesInputTokens
	default:
		return providersBase.OperationResponsesCreate
	}
}

func (r *relayResponses) getRequest() interface{} {
	return &r.responsesRequest
}

func (r *relayResponses) IsStream() bool {
	if r.operation != responsesOperationCreate {
		return false
	}
	return r.responsesRequest.Stream
}

func (r *relayResponses) getPromptTokens() (int, error) {
	channel := r.provider.GetChannel()
	if r.preparedChatRequest != nil {
		return common.CountTokenMessages(r.preparedChatRequest.Messages, r.modelName, channel.PreCost), nil
	}
	return common.CountTokenInputMessages(r.responsesRequest.Input, r.modelName, channel.PreCost), nil
}

func (r *relayResponses) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	err, done = r.sendCurrentProvider()
	if err == nil && r.usesChannelAffinity() {
		if channel := r.provider.GetChannel(); channel != nil {
			recordCurrentChannelAffinity(r.c, channelAffinityKindResponses, channel.Id)
		}
		if r.c != nil && r.c.GetBool(responsesPreviousResponseRecoveredContextKey) {
			mergeChannelAffinityMeta(r.c, map[string]any{
				"channel_affinity_previous_response_recovered": true,
			})
		}
	}

	return
}

func (r *relayResponses) usesChannelAffinity() bool {
	return r != nil && r.operation != responsesOperationInputTokens
}

func (r *relayResponses) sendCurrentProvider() (err *types.OpenAIErrorWithStatusCode, done bool) {
	switch r.operation {
	case responsesOperationInputTokens:
		if r.responsesRequest.Stream {
			return common.StringErrorWrapperLocal("streaming is not supported for /responses/input_tokens", "invalid_request_error", http.StatusBadRequest), true
		}
		provider, ok := r.provider.(providersBase.ResponsesInputTokensInterface)
		if !ok {
			return common.StringErrorWrapperLocal("channel does not support responses input token counting", unsupportedCapabilityCode, http.StatusServiceUnavailable), true
		}
		if err = r.requireRawEnvelope(); err != nil {
			return err, true
		}
		r.responsesRequest.Model = r.modelName
		var response *http.Response
		response, err = provider.CountResponsesInputTokens(r.c.Request.Context(), r.providerRequest(commonresponses.ResponsesInputTokens))
		if err != nil {
			return err, false
		}
		if err = responseMultipart(r.c, response, providerResponsePolicyForChannel(r.provider.GetChannel(), providersBase.OperationResponsesInputTokens, true)); err != nil {
			return err, true
		}
		if channel := r.provider.GetChannel(); channel != nil {
			recordZeroQuotaResponsesAudit(r.c, channel.Id, "responses input_tokens")
		}
		return nil, false
	case responsesOperationCompact:
		if r.responsesRequest.Stream {
			err = common.StringErrorWrapperLocal("streaming not supported for /responses/compact", "invalid_request_error", http.StatusBadRequest)
			done = true
			return
		}

		r.responsesRequest.Model = r.modelName
		responsesProvider, ok := r.provider.(providersBase.ResponsesInterface)
		if !ok || r.selectedDataPath == providersBase.DataPathCrossProtocol {
			err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
			done = true
			return
		}
		if err = r.requireRawEnvelope(); err != nil {
			done = true
			return
		}
		var response *types.OpenAIResponsesResponses
		response, err = responsesProvider.CompactResponses(r.c.Request.Context(), r.providerRequest(commonresponses.ResponsesCompact))
		if err != nil {
			done = err.ReplayRawResponse
			return
		}
		if channel := r.provider.GetChannel(); channel != nil {
			recordResponsesChannelAffinity(r.c, channel.Id, response)
		}
		openErr := responseJsonClient(r.c, response)
		if openErr != nil {
			err = openErr
		}
	default:
		r.responsesRequest.Model = r.modelName
		channel := r.provider.GetChannel()
		responsesProvider, ok := r.provider.(providersBase.ResponsesInterface)

		if r.selectedDataPath == providersBase.DataPathCrossProtocol {
			chatProvider, chatOK := r.provider.(providersBase.ChatInterface)
			if !chatOK {
				err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
				done = true
				return
			}
			return r.compatibleSend(chatProvider)
		}
		if !ok {
			err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
			done = true
			return
		}
		if err = r.requireRawEnvelope(); err != nil {
			done = true
			return
		}

		if r.responsesRequest.Stream {
			ioOwner, ioErr := newResponsesHTTPIO(r.c)
			if ioErr != nil {
				return common.ErrorWrapperLocal(ioErr, "response_write_deadline_unsupported", http.StatusInternalServerError), true
			}
			defer ioOwner.Close()
			r.c.Set(responsesHTTPIOContextKey, ioOwner)
			var response commonresponses.EventStream
			response, err = responsesProvider.CreateResponsesStream(ioOwner.ctx, r.providerRequest(commonresponses.ResponsesCreate))
			if err != nil {
				return
			}

			observer := commonresponses.NewStreamObserver()
			if channel != nil && !responseRequiresDurableOwner(&r.responsesRequest) {
				observer.SetResponseIDObserver(func(responseID string) {
					recordResponsesEphemeralProof(r.c, responseID, channel.Id)
				})
			}
			var firstResponseTime time.Time
			var streamErr *types.OpenAIErrorWithStatusCode
			if responseRequiresDurableOwner(&r.responsesRequest) {
				firstResponseTime, streamErr = responseStoredResponsesStreamClient(r.c, response, observer, channel.Id)
			} else {
				firstResponseTime, streamErr = responseNativeResponsesStreamClient(r.c, response, observer)
			}
			r.SetFirstResponseTime(firstResponseTime)
			if streamErr != nil {
				if streamErr.LocalError && r.c.Writer.Written() {
					// RelayHandler 的 panic guard 先按已取得证据结算，net/http 再 abort/reset。
					panic(http.ErrAbortHandler)
				}
				// Only an error before any accepted non-error payload establishes
				// provider rejection; a missing response ID alone proves nothing.
				providerRejected := observer.ProviderRejected()
				if !providerRejected {
					streamErr.UpstreamAccepted = true
				}
				return streamErr, true
			}
			if channel := r.provider.GetChannel(); channel != nil {
				finalResponse := observer.FinalResponse()
				recordResponsesChannelAffinity(r.c, channel.Id, finalResponse)
			}
		} else {
			var response *types.OpenAIResponsesResponses
			response, err = responsesProvider.CreateResponses(r.c.Request.Context(), r.providerRequest(commonresponses.ResponsesCreate))
			if err != nil {
				done = err.ReplayRawResponse
				return
			}
			if channel := r.provider.GetChannel(); channel != nil {
				if responseRequiresDurableOwner(&r.responsesRequest) {
					if ownerErr := persistStoredResponseOwner(r.c, response.ID, channel.Id); ownerErr != nil {
						ownerErr.UpstreamAccepted = true
						return ownerErr, true
					}
				} else {
					recordResponsesEphemeralProof(r.c, response.ID, channel.Id)
				}
				recordResponsesChannelAffinity(r.c, channel.Id, response)
			}
			openErr := responseJsonClient(r.c, response)

			if openErr != nil {
				err = openErr
			}
		}
	}
	return
}

func (r *relayResponses) providerRequest(operation commonresponses.Operation) *commonresponses.Request {
	channelID := 0
	if r.provider != nil && r.provider.GetChannel() != nil {
		channelID = r.provider.GetChannel().Id
	}
	headers := requestctx.HeaderSnapshot{}
	principal := requestctx.Principal{}
	if r.c != nil {
		if r.c.Request != nil {
			headers = requestctx.NewHeaderSnapshot(r.c.Request.Header)
		}
		principal = requestctx.PrincipalFromGin(r.c)
	}
	body := r.rawEnvelope
	rawQuery := ""
	if r.c != nil && r.c.Request != nil && r.c.Request.URL != nil {
		rawQuery = r.c.Request.URL.RawQuery
	}
	return &commonresponses.Request{
		Operation: operation,
		Headers:   headers,
		RawQuery:  rawQuery,
		Body:      body,
		Control: commonresponses.Control{
			DownstreamDialect: commonresponses.DownstreamResponses,
			Stream:            r.responsesRequest.Stream,
		},
		Policy:    r.responsesPolicyInput(),
		Principal: principal,
		ChannelID: channelID,
		Model:     r.modelName,
	}
}

func (r *relayResponses) responsesPolicyInput() commonresponses.PolicyInput {
	policy := commonresponses.PolicyInput{}
	if r == nil {
		return policy
	}
	if key := strings.TrimSpace(r.responsesRequest.PromptCacheKey); key != "" {
		policy.PromptCache = &commonresponses.PromptCacheDecision{
			Key:    key,
			Source: commonresponses.PromptCacheClientBody,
		}
		return policy
	}
	if key := requesthints.Get(r.c, requesthints.ResponsesPromptCacheKey); key != "" {
		policy.PromptCache = &commonresponses.PromptCacheDecision{
			Key:    key,
			Source: commonresponses.PromptCacheRouteHint,
		}
	}
	return policy
}

func (r *relayResponses) requireRawEnvelope() *types.OpenAIErrorWithStatusCode {
	if r != nil && r.rawEnvelope != nil {
		return nil
	}
	return common.StringErrorWrapperLocal("responses raw request body is required", "invalid_request_error", http.StatusBadRequest)
}

func (r *relayResponses) clearStalePreviousResponseAffinity() {
	if r == nil {
		return
	}

	ownerChannelID := 0
	if r.provider != nil && r.provider.GetChannel() != nil {
		ownerChannelID = r.provider.GetChannel().Id
	}
	if ownerChannelID <= 0 {
		ownerChannelID = currentPreferredChannelID(r.c)
	}
	clearResponsesEphemeralProof(r.c, r.responsesRequest.PreviousResponseID, ownerChannelID)
	clearCurrentChannelAffinityBindings(r.c)
	if r.usesChannelAffinity() {
		prepareResponsesChannelAffinity(r.c, &r.responsesRequest)
	}
}

func (r *relayResponses) stalePreviousResponseHandlingPlan(apiErr *types.OpenAIErrorWithStatusCode) *responsesContinuationMissHandlingPlan {
	if r == nil || !shouldRecoverStalePreviousResponse(apiErr) {
		return nil
	}
	if strings.TrimSpace(r.responsesRequest.PreviousResponseID) == "" {
		return nil
	}

	return &responsesContinuationMissHandlingPlan{
		recoveryCandidateMeta: map[string]any{
			"responses_continuation_miss":               true,
			"responses_continuation_recovery_candidate": true,
			"responses_continuation_recovery_strategy":  "manual_replay_required",
			"responses_continuation_error_code":         openAIErrorCodeString(apiErr.Code, "previous_response_not_found"),
		},
	}
}

func shouldRecoverStalePreviousResponse(apiErr *types.OpenAIErrorWithStatusCode) bool {
	if apiErr == nil {
		return false
	}
	// Only client-visible missing-resource statuses can prove that the
	// continuation target is stale. Rewriting a throttling or server failure by
	// message text would hide the provider status, clear valid affinity, and turn
	// a retryable failure into a local 400.
	if apiErr.StatusCode != http.StatusBadRequest && apiErr.StatusCode != http.StatusNotFound {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(openAIErrorCodeString(apiErr.Code, "")), "previous_response_not_found") {
		return true
	}
	// Message matching is only a compatibility fallback for providers that omit
	// a structured code. The parameter keeps an unrelated 400/404 containing
	// similar prose from clearing valid response affinity.
	if !strings.EqualFold(strings.TrimSpace(apiErr.Param), "previous_response_id") {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(apiErr.Message))
	if message == "" {
		return false
	}
	return strings.Contains(message, "previous_response_not_found") ||
		(strings.Contains(message, "previous response") && strings.Contains(message, "not found"))
}

func (r *relayResponses) compatibleSend(chatProvider providersBase.ChatInterface) (errWithCode *types.OpenAIErrorWithStatusCode, done bool) {
	if errWithCode = r.statefulCompatibilityFallbackError(); errWithCode != nil {
		return errWithCode, false
	}

	chatReq := r.preparedChatRequest
	if chatReq == nil {
		return &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{
				Message: "responses compatibility request was not finalized",
				Type:    "internal_error",
				Code:    "provider_request_not_finalized",
			},
			StatusCode: http.StatusInternalServerError,
			LocalError: true,
		}, true
	}

	if r.responsesRequest.Stream {
		var response requester.StreamReaderInterface[string]
		response, errWithCode = chatProvider.CreateChatCompletionStream(chatReq)
		if errWithCode != nil {
			return
		}
		var finalResponse *types.OpenAIResponsesResponses
		var firstResponseTime time.Time
		firstResponseTime, finalResponse, errWithCode = r.chatToResponseStreamClient(response)
		r.SetFirstResponseTime(firstResponseTime)
		if errWithCode != nil {
			errWithCode.UpstreamAccepted = true
			return errWithCode, true
		}
		if channel := r.provider.GetChannel(); channel != nil {
			recordResponsesChannelAffinity(r.c, channel.Id, finalResponse)
		}
	} else {
		var response *types.ChatCompletionResponse
		response, errWithCode = chatProvider.CreateChatCompletion(chatReq)
		if errWithCode != nil {
			return
		}

		// Captured native responses may have only a partial observation DTO.
		// A protocol conversion must retain its strict representability boundary.
		if raw := response.ReplayProviderRawJSON(); len(raw) > 0 {
			if runtimesession.OpenAIErrorEnvelopeFromPayload(raw) != nil {
				apiErr := common.StringErrorWrapperLocal("provider error cannot be converted to a successful Responses result", "invalid_provider_response", http.StatusBadGateway)
				apiErr.UpstreamAccepted = true
				return apiErr, true
			}
			var mapped types.ChatCompletionResponse
			if decodeErr := json.Unmarshal(raw, &mapped); decodeErr != nil {
				apiErr := common.ErrorWrapperLocal(decodeErr, "invalid_provider_response", http.StatusBadGateway)
				apiErr.UpstreamAccepted = true
				return apiErr, true
			}
		}
		responseResp := response.ToResponses(&r.responsesRequest)
		if channel := r.provider.GetChannel(); channel != nil {
			recordResponsesChannelAffinity(r.c, channel.Id, responseResp)
		}
		errWithCode = responseJsonClient(r.c, responseResp)
	}

	if errWithCode != nil {
		done = true
	}

	return
}

// Fail closed instead of silently degrading stateful Responses requests to Chat
// Completions. store omitted/true, previous_response_id, and conversation all depend on
// response-native state semantics that cannot be preserved across compatibility
// fallback, especially once multi-channel routing may move follow-up requests to
// a different upstream account or region.
func (r *relayResponses) statefulCompatibilityFallbackError() *types.OpenAIErrorWithStatusCode {
	if r == nil {
		return nil
	}

	param, description := responsesStatefulFallbackRequirement(&r.responsesRequest)
	if param == "" {
		return nil
	}

	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Message: fmt.Sprintf("%s requires native /v1/responses support on the selected channel; one-hub will not degrade this request to /v1/chat/completions because that would change response state semantics.", description),
			Type:    "channel_error",
			Param:   param,
			Code:    "responses_native_support_required",
		},
		StatusCode: http.StatusServiceUnavailable,
	}
}

func responsesStatefulFallbackRequirement(request *types.OpenAIResponsesRequest) (param string, description string) {
	if request == nil {
		return "", ""
	}

	if strings.TrimSpace(request.PreviousResponseID) != "" {
		return "previous_response_id", "responses request with previous_response_id"
	}

	if hasMeaningfulResponsesConversation(request.Conversation) {
		return "conversation", "responses request with conversation state"
	}

	if request.Store == nil || *request.Store {
		return "store", "responses request with stored response semantics"
	}

	return "", ""
}

func hasMeaningfulResponsesConversation(conversation any) bool {
	switch value := conversation.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(value) != ""
	case map[string]any:
		return len(value) > 0
	default:
		return true
	}
}

// 将chat转换成兼容的responses流处理
func (r *relayResponses) chatToResponseStreamClient(stream requester.StreamReaderInterface[string]) (firstResponseTime time.Time, finalResponse *types.OpenAIResponsesResponses, errWithCode *types.OpenAIErrorWithStatusCode) {
	requester.SetEventStreamHeaders(r.c)
	dataChan, errChan := stream.Recv()
	rawSSEEvents := requester.IsRawSSEEventStream(stream)

	defer requester.CloseAndDrainStream(stream)
	streamWriter := relay_util.NewBufferedStreamWriter(r.c.Writer, 0)
	defer streamWriter.Close()
	var isFirstResponse bool

	credentials := requestctx.ProviderCredentials(r.c)
	writeEvent := func(event string, payload []byte) error {
		// 转换后的正文也保持原值；仅处理明确的错误事件。
		safe := payload
		if event == "error" || event == "response.failed" || event == "response.incomplete" {
			safe = redactProviderErrorPayload(payload, credentials...)
		}
		_, err := streamWriter.WriteString("event: " + event + "\ndata: " + string(safe) + "\n\n")
		return err
	}
	converter := relay_util.NewOpenAIResponsesStreamConverter(writeEvent, &r.responsesRequest, r.provider.GetUsage())
	streamFailure := func(err error, code string) *types.OpenAIErrorWithStatusCode {
		if r.c.Writer.Written() {
			r.c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
		}
		var failure *types.OpenAIErrorWithStatusCode
		if errors.As(err, &failure) && failure != nil && failure.LocalError {
			return failure
		}
		failure = common.ErrorWrapper(err, code, http.StatusBadGateway)
		failure.UpstreamAccepted = true
		return failure
	}
	dataOpen := dataChan != nil
	errOpen := errChan != nil

	handleData := func(data string) *types.OpenAIErrorWithStatusCode {
		if r.c.Request.Context().Err() != nil {
			return responsesStreamClientCanceledError()
		}
		if !isFirstResponse {
			firstResponseTime = time.Now()
			isFirstResponse = true
		}
		if rawSSEEvents {
			payload, hasData := commonresponses.SSEDataPayload(data)
			if !hasData || strings.TrimSpace(payload) == "" || strings.TrimSpace(payload) == "[DONE]" {
				return nil
			}
			if providerErr := runtimesession.OpenAIErrorEnvelopeFromPayload([]byte(payload)); providerErr != nil {
				if writeErr := converter.ProcessStreamError(); writeErr != nil {
					return streamFailure(writeErr, "stream_write_failed")
				}
				if r.c.Writer.Written() {
					r.c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
				}
				providerErr.UpstreamAccepted = true
				return providerErr
			}
			data = payload
		}

		if err := converter.ProcessStreamData(data); err != nil {
			return streamFailure(err, relay_util.ResponsesStreamFailureCode(err))
		}
		return nil
	}

	handleEOF := func() error {
		// EOF/terminal has been dequeued, but the producer may still be returning
		// from its handler. Drain it before the converter reads provider-owned usage.
		requester.CloseAndDrainStream(stream)
		return converter.ProcessStreamData("[DONE]")
	}

	handleError := func(err error) error {
		if isStreamTerminalEOF(err) {
			return handleEOF()
		}
		logger.LogError(r.c.Request.Context(), "Stream err:"+err.Error())
		select {
		case <-r.c.Request.Context().Done():
		default:
			if writeErr := converter.ProcessStreamError(); writeErr != nil {
				return writeErr
			}
			if r.c.Writer.Written() {
				r.c.Set(responsesStreamErrorAlreadyRenderedContextKey, true)
			}
		}
		return err
	}

	for dataOpen || errOpen {
		if dataOpen {
			select {
			case data, ok := <-dataChan:
				if !ok {
					dataOpen = false
					dataChan = nil
					continue
				}
				if errWithCode := handleData(data); errWithCode != nil {
					return firstResponseTime, converter.FinalResponse(), errWithCode
				}
				continue
			default:
			}
		}

		select {
		case <-r.c.Request.Context().Done():
			return firstResponseTime, converter.FinalResponse(), responsesStreamClientCanceledError()
		case data, ok := <-dataChan:
			if !ok {
				dataOpen = false
				dataChan = nil
				continue
			}
			if errWithCode := handleData(data); errWithCode != nil {
				return firstResponseTime, converter.FinalResponse(), errWithCode
			}
		case err, ok := <-errChan:
			if !ok {
				errOpen = false
				errChan = nil
				continue
			}
			if failure := handleError(err); failure != nil {
				code := "stream_read_failed"
				if isStreamTerminalEOF(err) {
					code = relay_util.ResponsesStreamFailureCode(failure)
				}
				return firstResponseTime, converter.FinalResponse(), streamFailure(failure, code)
			}
			return firstResponseTime, converter.FinalResponse(), nil
		}
	}

	if err := handleEOF(); err != nil {
		return firstResponseTime, converter.FinalResponse(), streamFailure(err, relay_util.ResponsesStreamFailureCode(err))
	}
	return firstResponseTime, converter.FinalResponse(), nil
}

type responsesOperation int

const (
	responsesOperationCreate responsesOperation = iota
	responsesOperationCompact
	responsesOperationInputTokens
)

func detectResponsesOperation(path string) responsesOperation {
	if strings.HasSuffix(path, "/input_tokens") {
		return responsesOperationInputTokens
	}
	if strings.HasSuffix(path, "/compact") {
		return responsesOperationCompact
	}
	return responsesOperationCreate
}

func (r *relayResponses) skipQuotaSettlement() bool {
	return r != nil && r.operation == responsesOperationInputTokens
}

func (r *relayResponses) allowsSideEffectFreeObservationRetry() bool {
	return r != nil && r.operation == responsesOperationInputTokens &&
		!r.strictOwnerRoute && strings.TrimSpace(r.responsesRequest.PreviousResponseID) == ""
}
