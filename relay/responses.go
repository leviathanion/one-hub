package relay

import (
	"errors"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/logger"
	"one-api/common/requester"
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
	switch r.operation {
	case responsesOperationCompact:
		if err := common.UnmarshalBodyReusable(r.c, &r.responsesRequest); err != nil {
			return err
		}
		r.setOriginalModel(r.responsesRequest.Model)
		prepareResponsesChannelAffinity(r.c, &r.responsesRequest)
		return nil
	default:
		if err := common.UnmarshalBodyReusable(r.c, &r.responsesRequest); err != nil {
			return err
		}

		r.setOriginalModel(r.responsesRequest.Model)
		prepareResponsesChannelAffinity(r.c, &r.responsesRequest)
		return nil
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
		channel := r.provider.GetChannel()
		responsesProvider, ok := r.provider.(providersBase.ResponsesInterface)
		if !ok || channel.CompatibleResponse || !r.provider.GetSupportedResponse() {
			err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
			done = true
			return
		}
		var response *types.OpenAIResponsesResponses
		response, err = responsesProvider.CompactResponses(&r.responsesRequest)
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
		if !ok || channel.CompatibleResponse || !r.provider.GetSupportedResponse() {
			// 做一层Chat的兼容
			chatProvider, ok := r.provider.(providersBase.ChatInterface)
			if !ok {
				err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
				done = true
				return
			}

			return r.compatibleSend(chatProvider)
		}

		if r.responsesRequest.Stream {
			var response requester.StreamReaderInterface[string]
			response, err = responsesProvider.CreateResponsesStream(&r.responsesRequest)
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
			response, err = responsesProvider.CreateResponses(&r.responsesRequest)
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
		firstResponseTime, finalResponse := r.chatToResponseStreamClient(response)
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

// 将chat转换成兼容的responses流处理
func (r *relayResponses) chatToResponseStreamClient(stream requester.StreamReaderInterface[string]) (firstResponseTime time.Time, finalResponse *types.OpenAIResponsesResponses) {
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

	for {
		select {
		case data, ok := <-dataChan:
			if !ok {
				return firstResponseTime, converter.FinalResponse()
			}

			if !isFirstResponse {
				firstResponseTime = time.Now()
				isFirstResponse = true
			}

			select {
			case <-r.c.Request.Context().Done():
			default:
				converter.ProcessStreamData(data)
			}
			continue
		default:
		}

		select {
		case data, ok := <-dataChan:
			if !ok {
				return firstResponseTime, converter.FinalResponse()
			}

			if !isFirstResponse {
				firstResponseTime = time.Now()
				isFirstResponse = true
			}

			select {
			case <-r.c.Request.Context().Done():
			default:
				converter.ProcessStreamData(data)
			}
		case err := <-errChan:
			if !errors.Is(err, io.EOF) {
				select {
				case <-r.c.Request.Context().Done():
				default:
					converter.ProcessError(err.Error())
				}

				logger.LogError(r.c.Request.Context(), "Stream err:"+err.Error())
			} else {
				converter.ProcessStreamData("[DONE]")
			}
			return firstResponseTime, converter.FinalResponse()
		}
	}
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
