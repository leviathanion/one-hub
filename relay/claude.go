package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/jsonobject"
	"one-api/common/requester"
	"one-api/common/surface"
	"one-api/model"
	"one-api/providers"
	"one-api/providers/claude"
	"one-api/safty"
	"one-api/types"
	"strings"

	"github.com/gin-gonic/gin"
)

type relayClaudeOnly struct {
	relayBase
	claudeRequest         *claude.ClaudeRequest
	preparedClaudeRequest *claude.ClaudeRequest
}

func NewRelayClaudeOnly(c *gin.Context) *relayClaudeOnly {
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
	raw, err := common.CacheRequestBody(r.c)
	if err != nil {
		return err
	}
	r.claudeRequest, err = projectNativeClaudeRequest(raw)
	if err != nil {
		return err
	}
	r.setOriginalModel(r.claudeRequest.Model)
	setRequestChannelCapability(r.c, requireNativeClaudeRequestCompatibility(r.claudeRequest.Model, raw))
	return nil
}

func projectNativeClaudeRequest(raw []byte) (*claude.ClaudeRequest, error) {
	object, err := jsonobject.Parse(raw)
	if err != nil {
		return nil, err
	}
	projection := &claude.ClaudeRequest{}
	_ = json.Unmarshal(raw, projection)
	if err := json.Unmarshal(object.Fields["model"], &projection.Model); err != nil {
		return nil, fmt.Errorf("model must be a string")
	}
	if stream, ok := object.Fields["stream"]; ok {
		if err := json.Unmarshal(stream, &projection.Stream); err != nil {
			return nil, fmt.Errorf("stream must be a boolean")
		}
	}
	return projection, nil
}

func requireNativeClaudeRequestCompatibility(modelName string, raw []byte) requestChannelCapability {
	return func(channel *model.Channel) error {
		canonicalModel, err := mappedModelForChannel(channel, modelName)
		if err != nil {
			return &capabilityGateError{message: "channel has invalid model mapping configuration", status: http.StatusServiceUnavailable}
		}
		body, fields, err := effectiveNativeRequestBody(channel, canonicalModel, raw, true)
		if err != nil {
			return err
		}
		if err := providers.AssessClaudeRequest(channel, canonicalModel, fields); err != nil {
			return providerCapabilityGateError(err)
		}
		projection, err := projectNativeClaudeRequest(body)
		if err != nil {
			return err
		}
		_, _, err = providers.AssessNativeClaudeRemoteMedia(channel, projection)
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
	raw, ok := common.GetOriginalRequestBody(r.c)
	if !ok {
		var err error
		raw, err = common.CacheRequestBody(r.c)
		if err != nil {
			return fmt.Errorf("read native Claude request body: %w", err)
		}
	}
	modelName := r.modelName
	if modelName == "" {
		modelName = r.claudeRequest.Model
	}
	effective, _, err := effectiveNativeRequestBody(r.provider.GetChannel(), modelName, raw, false)
	if err != nil {
		return err
	}
	if current, ok := common.GetCanonicalRequestBody(r.c); ok && bytes.Equal(current, effective) {
		return nil
	}
	return r.restoreNativeClaudeRaw(effective)
}

func (r *relayClaudeOnly) restoreNativeClaudeRaw(rawBody []byte) error {
	updatedRequest, err := projectNativeClaudeRequest(rawBody)
	if err != nil {
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
	// 内容策略从有效 raw 提取解码文本，不能依赖 typed 投影或 JSON 转义表示。
	var content any = request.Messages
	if raw, ok := common.GetCanonicalRequestBody(r.c); ok {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) == nil {
			content = fields["messages"]
		}
	}
	if result, _ := safty.CheckContent(content); !result.IsSafe {
		return common.StringErrorWrapperLocal(result.Reason, result.Code, http.StatusBadRequest), true
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
		onDelivered := func(event string) {
			eventName, payloadType, _ := audioSSEFacts([]byte(event))
			providerErrorDelivered = providerErrorDelivered || eventName == "error" || payloadType == "error"
		}
		firstResponseTime, streamErr := responseGeneralStreamClientWithObserverResult(r.c, response, nil, onDelivered, providerSSETransform(r.c), false)
		r.SetFirstResponseTime(firstResponseTime)
		if streamErr != nil {
			var providerErr *types.OpenAIErrorWithStatusCode
			if errors.As(streamErr, &providerErr) && providerErr != nil && providerErr.LocalError {
				return providerErr, true
			}
			if providerErrorDelivered {
				r.c.Set(streamErrorAlreadyRenderedContextKey, true)
			} else if r.c.Request.Context().Err() == nil {
				_, _ = r.c.Writer.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"stream interrupted\"}}\n\n"))
				r.c.Writer.Flush()
				r.c.Set(streamErrorAlreadyRenderedContextKey, true)
			}
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
