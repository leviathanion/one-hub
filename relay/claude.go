package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/common/surface"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/providers/claude"
	"one-api/safty"
	"one-api/types"
	"strings"

	"github.com/gin-gonic/gin"
)

var AllowChannelType = []int{config.ChannelTypeAnthropic, config.ChannelTypeVertexAI, config.ChannelTypeBedrock, config.ChannelTypeCustom}

type relayClaudeOnly struct {
	relayBase
	claudeRequest         *claude.ClaudeRequest
	preparedClaudeRequest *claude.ClaudeRequest
}

func NewRelayClaudeOnly(c *gin.Context) *relayClaudeOnly {
	c.Set("allow_channel_type", AllowChannelType)
	relay := &relayClaudeOnly{
		relayBase: relayBase{
			allowHeartbeat: true,
			c:              c,
			contract:       surface.ClaudeContract(),
		},
	}

	return relay
}

func (r *relayClaudeOnly) setRequest() error {
	r.claudeRequest = &claude.ClaudeRequest{}
	if err := common.UnmarshalBodyReusable(r.c, r.claudeRequest); err != nil {
		return err
	}
	r.setOriginalModel(r.claudeRequest.Model)
	setRequestChannelCapability(r.c, requireNativeClaudeRemoteMedia(r.claudeRequest.Model, r.claudeRequest))
	return nil
}

func requireNativeClaudeRemoteMedia(modelName string, request *claude.ClaudeRequest) requestChannelCapability {
	return func(channel *model.Channel) error {
		canonicalModel, err := mappedModelForChannel(channel, modelName)
		if err != nil {
			return &capabilityGateError{message: "channel has invalid model mapping configuration", status: http.StatusServiceUnavailable}
		}
		effective := *request
		effective.Model = canonicalModel
		_, _, err = providers.AssessNativeClaudeRemoteMedia(channel, &effective)
		return remoteMediaCapabilityGateError(err)
	}
}

func (r *relayClaudeOnly) prepareSelectedProviderRemoteMedia() error {
	if r == nil || r.claudeRequest == nil || r.provider == nil {
		return nil
	}
	if err := r.materializeNativeClaudePreAdd(); err != nil {
		return err
	}
	effective := *r.claudeRequest
	effective.Model = r.modelName
	fetcher := r.remoteMedia
	if fetcher == nil {
		fetcher = newRequestRemoteMediaFetcher(r.c.Request.Context())
	}
	prepared, err := providers.PrepareNativeClaudeRemoteMedia(r.provider, &effective, fetcher)
	if err != nil {
		return remoteMediaCapabilityGateError(err)
	}
	r.preparedClaudeRequest = prepared
	return nil
}

// materializeNativeClaudePreAdd applies the channel's pre_add transform after
// provider model mapping, using the immutable request baseline. The native
// provider then observes the pre_add marker and skips it, so retries rebuild
// the same canonical body instead of accumulating transforms.
func (r *relayClaudeOnly) materializeNativeClaudePreAdd() error {
	if r == nil || r.c == nil || r.provider == nil {
		return nil
	}
	customParams, err := r.provider.CustomParameterHandler()
	if err != nil {
		return fmt.Errorf("channel custom parameters are invalid: %w", err)
	}
	rawBody, ok := common.GetOriginalRequestBody(r.c)
	if !ok {
		rawBody, err = common.CacheRequestBody(r.c)
		if err != nil {
			return fmt.Errorf("read native Claude request body: %w", err)
		}
	}
	preAdd, _ := customParams["pre_add"].(bool)
	if !preAdd {
		currentBody, currentOK := common.GetCanonicalRequestBody(r.c)
		if currentOK && bytes.Equal(currentBody, rawBody) {
			return nil
		}
		return r.restoreNativeClaudeRaw(rawBody)
	}

	originalMap, err := decodeNativeClaudeBodyMap(rawBody)
	if err != nil {
		return err
	}
	requestMap, err := decodeNativeClaudeBodyMap(rawBody)
	if err != nil {
		return err
	}

	modelName := r.modelName
	if modelName == "" {
		modelName = r.claudeRequest.Model
	}
	requestMap = providersBase.ApplyCustomParams(requestMap, customParams, modelName, true)
	if nativeClaudeJSONMapsEqual(originalMap, requestMap) {
		currentBody, currentOK := common.GetCanonicalRequestBody(r.c)
		if currentOK && bytes.Equal(currentBody, rawBody) {
			return nil
		}
		return r.restoreNativeClaudeRaw(rawBody)
	}
	materializedBody, err := json.Marshal(requestMap)
	if err != nil {
		return fmt.Errorf("marshal native Claude pre_add request: %w", err)
	}
	updatedRequest := &claude.ClaudeRequest{}
	if err := json.Unmarshal(materializedBody, updatedRequest); err != nil {
		return fmt.Errorf("decode native Claude pre_add request: %w", err)
	}

	common.SetReusableRequestBodyMap(r.c, materializedBody, requestMap)
	r.claudeRequest = updatedRequest
	r.preparedClaudeRequest = nil
	return nil
}

func decodeNativeClaudeBodyMap(rawBody []byte) (map[string]interface{}, error) {
	requestMap := make(map[string]interface{})
	decoder := json.NewDecoder(bytes.NewReader(rawBody))
	decoder.UseNumber()
	if err := decoder.Decode(&requestMap); err != nil {
		return nil, fmt.Errorf("decode native Claude request body: %w", err)
	}
	var extraValue interface{}
	if err := decoder.Decode(&extraValue); err != io.EOF {
		if err == nil {
			return nil, errors.New("native Claude request body contains multiple JSON values")
		}
		return nil, fmt.Errorf("decode native Claude request body: %w", err)
	}
	return requestMap, nil
}

// nativeClaudeJSONMapsEqual compares the post-transform JSON without changing
// the original map's json.Number values.
func nativeClaudeJSONMapsEqual(left, right map[string]interface{}) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func (r *relayClaudeOnly) restoreNativeClaudeRaw(rawBody []byte) error {
	updatedRequest := &claude.ClaudeRequest{}
	if err := json.Unmarshal(rawBody, updatedRequest); err != nil {
		return fmt.Errorf("decode native Claude request body: %w", err)
	}
	common.SetReusableRequestBody(r.c, rawBody)
	r.claudeRequest = updatedRequest
	r.preparedClaudeRequest = nil
	return nil
}

func (r *relayClaudeOnly) effectiveClaudeRequest() *claude.ClaudeRequest {
	if r != nil && r.preparedClaudeRequest != nil {
		return r.preparedClaudeRequest
	}
	if r == nil {
		return nil
	}
	return r.claudeRequest
}

func (r *relayClaudeOnly) getRequest() interface{} {
	return r.claudeRequest
}

func (r *relayClaudeOnly) IsStream() bool {
	return r.claudeRequest.Stream
}

func (r *relayClaudeOnly) getPromptTokens() (int, error) {
	channel := r.provider.GetChannel()
	return CountTokenMessages(r.effectiveClaudeRequest(), channel.PreCost)
}

func (r *relayClaudeOnly) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	chatProvider, ok := r.provider.(claude.ClaudeChatInterface)
	if !ok {
		err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
		done = true
		return
	}

	request := r.effectiveClaudeRequest()
	if request == nil {
		return common.StringErrorWrapperLocal("Claude request was not finalized", "provider_request_not_finalized", http.StatusInternalServerError), true
	}
	request.Model = r.modelName
	// 内容审查
	for _, message := range request.Messages {
		if message.Content != nil {
			CheckResult, _ := safty.CheckContent(message.Content)
			if !CheckResult.IsSafe {
				err = common.StringErrorWrapperLocal(CheckResult.Reason, CheckResult.Code, http.StatusBadRequest)
				done = true
				return
			}
		}
	}

	if request.Stream {
		var response requester.StreamReaderInterface[string]
		response, err = chatProvider.CreateClaudeChatStream(request)
		if err != nil {
			return
		}

		if r.heartbeat != nil {
			r.heartbeat.Stop()
		}

		providerErrorDelivered := false
		observe := func(event string) {
			eventName, payloadType, _ := audioSSEFacts([]byte(event))
			providerErrorDelivered = providerErrorDelivered || eventName == "error" || payloadType == "error"
		}
		firstResponseTime, streamErr := responseGeneralStreamClientWithObserverResult(r.c, response, nil, observe, sanitizeProviderSSEEvent, false)
		r.SetFirstResponseTime(firstResponseTime)
		if streamErr != nil {
			if providerErrorDelivered {
				r.c.Set(streamErrorAlreadyRenderedContextKey, true)
			} else if r.c.Request.Context().Err() == nil {
				_, _ = r.c.Writer.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"stream interrupted\"}}\n\n"))
				r.c.Writer.Flush()
				r.c.Set(streamErrorAlreadyRenderedContextKey, true)
			}
			var providerErr *types.OpenAIErrorWithStatusCode
			if errors.As(streamErr, &providerErr) && providerErr != nil {
				return providerErr, true
			}
			apiErr := common.ErrorWrapper(streamErr, "invalid_provider_response", http.StatusBadGateway)
			apiErr.UpstreamAccepted = true
			return apiErr, true
		}
	} else {
		var response *claude.ClaudeResponse
		response, err = chatProvider.CreateClaudeChat(request)
		if err != nil {
			return
		}

		if r.heartbeat != nil {
			r.heartbeat.Stop()
		}

		openErr := responseJsonClient(r.c, response)

		if openErr != nil {
			err = openErr
		}
	}

	if err != nil {
		done = true
	}
	return
}

func (r *relayClaudeOnly) GetError(err *types.OpenAIErrorWithStatusCode) (int, any) {
	newErr := surface.NormalizeOpenAIError(r.c, err)
	claudeErr := claude.OpenaiErrToClaudeErr(&newErr)

	return newErr.StatusCode, claudeErr.ClaudeError
}

func (r *relayClaudeOnly) HandleJsonError(err *types.OpenAIErrorWithStatusCode) {
	r.relayBase.HandleJsonError(err)
}

func (r *relayClaudeOnly) HandleStreamError(err *types.OpenAIErrorWithStatusCode) {
	r.relayBase.HandleStreamError(err)
}

func CountTokenMessages(request *claude.ClaudeRequest, preCostType int) (int, error) {
	if preCostType == config.PreContNotAll {
		return 0, nil
	}

	tokenEncoder := common.GetTokenEncoder(request.Model)

	tokenNum := 0

	tokensPerMessage := 4
	var textMsg strings.Builder

	for _, message := range request.Messages {
		tokenNum += tokensPerMessage
		switch v := message.Content.(type) {
		case string:
			textMsg.WriteString(v)
		case []any:
			for _, m := range v {
				content, ok := m.(map[string]any)
				if !ok {
					tokenNum += 50
					continue
				}
				switch content["type"] {
				case "text":
					text, ok := content["text"].(string)
					if !ok {
						tokenNum += 50
						continue
					}
					textMsg.WriteString(text)
				default:
					// 不算了  就只算他50吧
					tokenNum += 50
				}
			}
		}
	}

	if textMsg.Len() > 0 {
		tokenNum += common.GetTokenNum(tokenEncoder, textMsg.String())
	}

	return tokenNum, nil
}
