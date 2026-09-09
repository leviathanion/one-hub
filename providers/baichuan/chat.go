package baichuan

import (
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
	"strings"
)

func (p *BaichuanProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (openaiResponse *types.ChatCompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	if apiErr := p.rejectUnpricedSearch(request.Model); apiErr != nil {
		return nil, apiErr
	}
	requestBody := p.getChatRequestBody(request)
	req, errWithCode := p.GetRequestTextBody(config.RelayModeChatCompletions, request.Model, requestBody)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	response := &BaichuanChatResponse{}
	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, response, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// 检测是否错误
	openaiErr := openai.ErrorHandle(&response.OpenAIErrorResponse)
	if openaiErr != nil {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *openaiErr,
			StatusCode:  http.StatusBadRequest,
		}
		return nil, errWithCode
	}

	if response.Usage != nil {
		response.Usage.MarkProviderReported()
		response.Usage.MergeProviderAttribution(response.Model, response.ServiceTier)
		*p.Usage = *response.Usage
	}

	return &response.ChatCompletionResponse, nil
}

func (p *BaichuanProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	if apiErr := p.rejectUnpricedSearch(request.Model); apiErr != nil {
		return nil, apiErr
	}
	streamOptions := request.StreamOptions
	// 如果支持流式返回Usage 则需要更改配置：
	if p.SupportStreamOptions {
		request.StreamOptions = &types.StreamOptions{
			IncludeUsage: true,
		}
	} else {
		// 避免误传导致报错
		request.StreamOptions = nil
	}

	req, errWithCode := p.GetRequestTextBody(config.RelayModeChatCompletions, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// 恢复原来的配置
	request.StreamOptions = streamOptions

	// 发送请求
	streamRequester := p.Requester.ForHTTPProfile(requester.HTTPProfileLongStream)
	resp, errWithCode := streamRequester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	chatHandler := openai.OpenAIStreamHandler{
		Usage:     p.Usage,
		ModelName: request.Model,
	}

	return requester.RequestStreamWithOptions[string](streamRequester, resp, chatHandler.HandlerChatStream, requester.StreamReadOptions{
		RequireProtocolTerminal: true,
	})
}

func (p *BaichuanProvider) rejectUnpricedSearch(modelName string) *types.OpenAIErrorWithStatusCode {
	if err := validateBaichuanSearchCapability(p.Channel, modelName); err != nil {
		if _, ok := err.(*base.RequestCapabilityError); !ok {
			return common.ErrorWrapperLocal(err, "custom_parameter_error", http.StatusInternalServerError)
		}
		return common.StringErrorWrapperLocal(err.Error(), "baichuan_search_billing_unsupported", http.StatusBadRequest)
	}
	return nil
}

// 获取聊天请求体
func (p *BaichuanProvider) getChatRequestBody(request *types.ChatCompletionRequest) *BaichuanChatRequest {
	messages := make([]BaichuanMessage, 0, len(request.Messages))
	for i := 0; i < len(request.Messages); i++ {
		message := request.Messages[i]
		if message.Role == "system" || message.Role == "assistant" {
			message.Role = "assistant"
		} else {
			message.Role = "user"
		}
		messages = append(messages, BaichuanMessage{
			Content: message.StringContent(),
			Role:    strings.ToLower(message.Role),
		})
	}

	return &BaichuanChatRequest{
		Model:       request.Model,
		Messages:    messages,
		Stream:      request.Stream,
		Temperature: request.Temperature,
		TopP:        request.TopP,
		TopK:        request.N,
	}
}
