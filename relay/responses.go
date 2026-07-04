package relay

import (
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/logger"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/internal/requesthints"
	providersBase "one-api/providers/base"
	"one-api/relay/relay_util"
	"one-api/types"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type relayResponses struct {
	relayBase
	responsesRequest types.OpenAIResponsesRequest
	rawEnvelope      *commonresponses.RawEnvelope
	operation        responsesOperation
}

const responsesPreviousResponseRecoveredContextKey = "responses_previous_response_recovered"

type responsesContinuationMissHandlingPlan struct {
	recoveryCandidateMeta map[string]any
	clientError           *types.OpenAIErrorWithStatusCode
}

func NewRelayResponses(c *gin.Context) *relayResponses {
	relay := &relayResponses{}
	relay.c = c
	relay.operation = detectResponsesOperation(c.Request.URL.Path)
	return relay
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
	if strings.TrimSpace(r.responsesRequest.Model) == "" {
		return fmt.Errorf("field Model is required")
	}
	r.setOriginalModel(r.responsesRequest.Model)
	prepareResponsesChannelAffinity(r.c, &r.responsesRequest)
	return nil
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
	return common.CountTokenInputMessages(r.responsesRequest.Input, r.modelName, channel.PreCost), nil
}

func (r *relayResponses) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	err, done = r.sendCurrentProvider()
	if err == nil {
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

func (r *relayResponses) sendCurrentProvider() (err *types.OpenAIErrorWithStatusCode, done bool) {
	switch r.operation {
	case responsesOperationCompact:
		if r.responsesRequest.Stream {
			err = common.StringErrorWrapperLocal("streaming not supported for /responses/compact", "invalid_request_error", http.StatusBadRequest)
			done = true
			return
		}

		r.responsesRequest.Model = r.modelName
		responsesProvider, ok := r.provider.(providersBase.ResponsesInterface)
		canNative := ok && r.provider.GetSupportedResponse()
		if !canNative {
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
		canNative := ok && r.provider.GetSupportedResponse()

		if !canNative {
			if !channel.CompatibleResponse {
				err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
				done = true
				return
			}

			// 做一层Chat的兼容
			chatProvider, ok := r.provider.(providersBase.ChatInterface)
			if !ok {
				err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
				done = true
				return
			}

			return r.compatibleSend(chatProvider)
		}
		if err = r.requireRawEnvelope(); err != nil {
			done = true
			return
		}

		if r.responsesRequest.Stream {
			var response requester.StreamReaderInterface[string]
			response, err = responsesProvider.CreateResponsesStream(r.c.Request.Context(), r.providerRequest(commonresponses.ResponsesCreate))
			if err != nil {
				return
			}

			doneStr := func() string {
				return ""
			}

			observer := relay_util.NewOpenAIResponsesStreamObserver()
			firstResponseTime := responseGeneralStreamClientWithObserver(r.c, response, doneStr, observer.ObserveRawLine)
			r.SetFirstResponseTime(firstResponseTime)
			if channel := r.provider.GetChannel(); channel != nil {
				recordResponsesChannelAffinity(r.c, channel.Id, observer.FinalResponse())
			}
		} else {
			var response *types.OpenAIResponsesResponses
			response, err = responsesProvider.CreateResponses(r.c.Request.Context(), r.providerRequest(commonresponses.ResponsesCreate))
			if err != nil {
				return
			}
			if channel := r.provider.GetChannel(); channel != nil {
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
	return &commonresponses.Request{
		Operation: operation,
		Headers:   headers,
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

	clearCurrentChannelAffinityBindings(r.c)
	prepareResponsesChannelAffinity(r.c, &r.responsesRequest)
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
		clientError: &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{
				Code:    "previous_response_not_found",
				Type:    "invalid_request_error",
				Param:   "previous_response_id",
				Message: "previous_response_id is stale. one-hub cannot safely recover this responses request without replay; resend the request with full context.",
			},
			StatusCode: http.StatusConflict,
			LocalError: true,
		},
	}
}

func shouldRecoverStalePreviousResponse(apiErr *types.OpenAIErrorWithStatusCode) bool {
	if apiErr == nil {
		return false
	}
	if openAIErrorCodeString(apiErr.Code, "") == "previous_response_not_found" {
		return true
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

	chatReq, err := r.responsesRequest.ToChatCompletionRequest()
	if err != nil {
		return common.ErrorWrapperLocal(err, "invalid_claude_config", http.StatusInternalServerError), true
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
		if errWithCode != nil {
			return
		}
		r.SetFirstResponseTime(firstResponseTime)
		if channel := r.provider.GetChannel(); channel != nil {
			recordResponsesChannelAffinity(r.c, channel.Id, finalResponse)
		}
	} else {
		var response *types.ChatCompletionResponse
		response, errWithCode = chatProvider.CreateChatCompletion(chatReq)
		if errWithCode != nil {
			return
		}

		responseResp := response.ToResponses(&r.responsesRequest)
		if channel := r.provider.GetChannel(); channel != nil {
			recordResponsesChannelAffinity(r.c, channel.Id, responseResp)
		}
		responseJsonClient(r.c, responseResp)
	}

	if errWithCode != nil {
		done = true
	}

	return
}

// Fail closed instead of silently degrading stateful Responses requests to Chat
// Completions. store=true, previous_response_id, and conversation all depend on
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

	if request.Store != nil && *request.Store {
		return "store", "responses request with store=true"
	}

	if strings.TrimSpace(request.PreviousResponseID) != "" {
		return "previous_response_id", "responses request with previous_response_id"
	}

	if hasMeaningfulResponsesConversation(request.Conversation) {
		return "conversation", "responses request with conversation state"
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

	defer stream.Close()
	streamWriter := relay_util.NewBufferedStreamWriter(r.c.Writer, 0)
	relay_util.SetStreamWriter(r.c, streamWriter)
	defer func() {
		_ = streamWriter.Close()
		relay_util.ClearStreamWriter(r.c)
	}()
	var isFirstResponse bool

	converter := relay_util.NewOpenAIResponsesStreamConverter(r.c, &r.responsesRequest, r.provider.GetUsage())
	dataOpen := dataChan != nil
	errOpen := errChan != nil

	handleData := func(data string) {
		if !isFirstResponse {
			firstResponseTime = time.Now()
			isFirstResponse = true
		}

		select {
		case <-r.c.Request.Context().Done():
		default:
			converter.ProcessStreamData(data)
		}
	}

	handleEOF := func() {
		converter.ProcessStreamData("[DONE]")
	}

	handleError := func(err error) {
		if isStreamTerminalEOF(err) {
			handleEOF()
			return
		}
		select {
		case <-r.c.Request.Context().Done():
		default:
			converter.ProcessStreamError()
		}

		logger.LogError(r.c.Request.Context(), "Stream err:"+common.RedactSensitiveText(err.Error()))
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
				handleData(data)
				continue
			default:
			}
		}

		select {
		case data, ok := <-dataChan:
			if !ok {
				dataOpen = false
				dataChan = nil
				continue
			}
			handleData(data)
		case err, ok := <-errChan:
			if !ok {
				errOpen = false
				errChan = nil
				continue
			}
			handleError(err)
			if !isStreamTerminalEOF(err) {
				return firstResponseTime, converter.FinalResponse(), common.ErrorWrapper(err, "stream_read_failed", http.StatusInternalServerError)
			}
			return firstResponseTime, converter.FinalResponse(), nil
		}
	}

	handleEOF()
	return firstResponseTime, converter.FinalResponse(), nil
}

type responsesOperation int

const (
	responsesOperationCreate responsesOperation = iota
	responsesOperationCompact
)

func detectResponsesOperation(path string) responsesOperation {
	if strings.HasSuffix(path, "/compact") {
		return responsesOperationCompact
	}
	return responsesOperationCreate
}
