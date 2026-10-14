package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/common/surface"
	"one-api/model"
	"one-api/providers"
	"one-api/providers/gemini"
	"one-api/safty"
	"one-api/types"
	"strings"

	"github.com/gin-gonic/gin"
)

type relayGeminiOnly struct {
	relayBase
	geminiRequest *gemini.NativeRequest
}

func NewRelayGeminiOnly(c *gin.Context) *relayGeminiOnly {
	relay := &relayGeminiOnly{
		relayBase: relayBase{
			allowHeartbeat: true,
			c:              c,
			contract:       surface.GeminiContract(),
		},
	}

	return relay
}

func (r *relayGeminiOnly) setRequest() error {
	modelAction := r.c.Param("model")

	if modelAction == "" {
		return errors.New("model is required")
	}

	modelList := strings.Split(modelAction, ":")
	if len(modelList) != 2 {
		return errors.New("model error")
	}

	isStream := false
	switch modelList[1] {
	case "generateContent":
	case "streamGenerateContent":
		isStream = true
	default:
		return fmt.Errorf("Gemini action %q is not supported", modelList[1])
	}

	raw, err := common.CacheRequestBody(r.c)
	if err != nil {
		return err
	}
	r.geminiRequest, err = gemini.ParseNativeRequest(raw, modelList[0], isStream)
	if err != nil {
		return err
	}
	r.setOriginalModel(r.geminiRequest.Model)
	setRequestChannelCapability(r.c, func(channel *model.Channel) error {
		modelName, err := mappedModelForChannel(channel, modelList[0])
		if err != nil {
			return err
		}
		_, fields, err := effectiveNativeRequestBody(channel, modelName, raw, false)
		if err != nil {
			return err
		}
		return providerCapabilityGateError(providers.AssessGeminiRequest(channel, modelName, fields))
	})

	return nil
}

func (r *relayGeminiOnly) prepareSelectedProviderRemoteMedia() error {
	raw, ok := common.GetOriginalRequestBody(r.c)
	if !ok {
		raw = r.geminiRequest.Raw
	}
	body, _, err := effectiveNativeRequestBody(r.provider.GetChannel(), r.modelName, raw, false)
	if err != nil {
		return err
	}
	r.geminiRequest.Raw = body
	r.geminiRequest.Model = r.modelName
	common.SetReusableRequestBody(r.c, body)
	return nil
}

func (r *relayGeminiOnly) getRequest() interface{} {
	return r.geminiRequest
}

func (r *relayGeminiOnly) IsStream() bool {
	return r.geminiRequest.Stream
}

func (r *relayGeminiOnly) getPromptTokens() (int, error) {
	channel := r.provider.GetChannel()
	projection, _ := r.geminiRequest.Projection()
	return CountGeminiTokenMessages(projection, channel.PreCost)
}

func (r *relayGeminiOnly) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	chatProvider, ok := r.provider.(gemini.GeminiChatInterface)
	if !ok {
		return common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable), true
	}

	// 内容策略从有效 raw 提取解码文本，不能依赖 typed 投影或 JSON 转义表示。
	var body map[string]json.RawMessage
	_ = json.Unmarshal(r.geminiRequest.Raw, &body)
	if result, _ := safty.CheckContent(body["contents"]); !result.IsSafe {
		return common.StringErrorWrapperLocal(result.Reason, result.Code, http.StatusBadRequest), true
	}

	r.geminiRequest.Model = r.modelName

	if r.geminiRequest.Stream {
		var response requester.StreamReaderInterface[string]
		response, err = chatProvider.CreateGeminiChatStream(r.geminiRequest)
		if err != nil {
			return
		}

		if r.heartbeat != nil {
			r.heartbeat.Stop()
		}

		providerErrorDelivered := false
		onDelivered := func(event string) {
			providerErrorDelivered = providerErrorDelivered || sseEventContainsError([]byte(event))
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
				_, _ = r.c.Writer.Write([]byte("data: {\"error\":{\"code\":502,\"message\":\"stream interrupted\",\"status\":\"INVALID_PROVIDER_RESPONSE\"}}\n\n"))
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
		var response *gemini.GeminiChatResponse
		response, err = chatProvider.CreateGeminiChat(r.geminiRequest)
		if err != nil {
			return
		}

		if r.heartbeat != nil {
			r.heartbeat.Stop()
		}

		err = responseJsonClient(r.c, response)
	}

	if err != nil {
		done = true
	}

	return
}

func (r *relayGeminiOnly) GetError(err *types.OpenAIErrorWithStatusCode) (int, any) {
	newErr := surface.NormalizeOpenAIError(r.c, err)
	geminiErr := gemini.OpenaiErrToGeminiErr(&newErr)

	return newErr.StatusCode, geminiErr.GeminiErrorResponse
}

func (r *relayGeminiOnly) HandleJsonError(err *types.OpenAIErrorWithStatusCode) {
	r.relayBase.HandleJsonError(err)
}

func (r *relayGeminiOnly) HandleStreamError(err *types.OpenAIErrorWithStatusCode) {
	r.relayBase.HandleStreamError(err)
}

func CountGeminiTokenMessages(request *gemini.GeminiChatRequest, preCostType int) (int, error) {
	if preCostType == config.PreContNotAll {
		return 0, nil
	}

	tokenEncoder := common.GetTokenEncoder(request.Model)

	tokenNum := 0
	tokensPerMessage := 4
	var textMsg strings.Builder

	for _, message := range request.Contents {
		tokenNum += tokensPerMessage
		for _, part := range message.Parts {
			if part.Text != "" {
				textMsg.WriteString(part.Text)
			}

			if part.InlineData != nil {
				// 其他类型的，暂时按200个token计算
				tokenNum += 200
			}
		}
	}

	if textMsg.Len() > 0 {
		tokenNum += common.GetTokenNum(tokenEncoder, textMsg.String())
	}
	return tokenNum, nil
}
