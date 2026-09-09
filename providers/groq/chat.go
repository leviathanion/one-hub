package groq

import (
	"bytes"
	"encoding/json"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/common/utils"
	"one-api/providers/openai"
	"one-api/types"
	"strings"
)

func (p *GroqProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (openaiResponse *types.ChatCompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	if err := validateGroqCompoundCapability(request.Model); err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "groq_compound_billing_unsupported", http.StatusBadRequest)
	}
	p.getChatRequestBody(request)

	req, errWithCode := p.GetRequestTextBody(config.RelayModeChatCompletions, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	response := &openai.OpenAIProviderChatResponse{}
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

func (p *GroqProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	if err := validateGroqCompoundCapability(request.Model); err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "groq_compound_billing_unsupported", http.StatusBadRequest)
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
	p.getChatRequestBody(request)
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

	return requester.RequestStreamWithOptions[string](streamRequester, resp, p.streamHandler(chatHandler), requester.StreamReadOptions{
		RequireProtocolTerminal: true,
	})
}

func isCompoundModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "groq/compound" || model == "groq/compound-mini" || model == "compound" || model == "compound-mini"
}

func (p *GroqProvider) streamHandler(baseHandler openai.OpenAIStreamHandler) requester.HandlerPrefix[string] {
	return func(rawLine *[]byte, dataChan chan string, errChan chan error) {
		trimmed := bytes.TrimSpace(*rawLine)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
			if !bytes.Equal(payload, []byte("[DONE]")) {
				var envelope struct {
					Model       string          `json:"model"`
					ServiceTier string          `json:"service_tier"`
					Usage       json.RawMessage `json:"usage"`
					XGroq       *struct {
						Usage *types.Usage `json:"usage"`
					} `json:"x_groq"`
				}
				if json.Unmarshal(payload, &envelope) == nil && len(envelope.Usage) == 0 && envelope.XGroq != nil && envelope.XGroq.Usage != nil && p.Usage != nil {
					previousModel := p.Usage.ResponseModel
					previousTier := p.Usage.ServiceTier
					previousConflict := p.Usage.AttributionConflict
					envelope.XGroq.Usage.MarkProviderReported()
					*p.Usage = *envelope.XGroq.Usage
					p.Usage.MergeProviderAttribution(previousModel, previousTier)
					p.Usage.AttributionConflict = p.Usage.AttributionConflict || previousConflict
					p.Usage.MergeProviderAttribution(envelope.Model, envelope.ServiceTier)
				}
			}
		}
		baseHandler.HandlerChatStream(rawLine, dataChan, errChan)
	}
}

// 获取聊天请求体
func (p *GroqProvider) getChatRequestBody(request *types.ChatCompletionRequest) {
	if request.Tools != nil {
		request.Tools = nil
	}

	if request.ToolChoice != nil {
		request.ToolChoice = nil
	}

	if request.ResponseFormat != nil {
		request.ResponseFormat = nil
	}

	if request.N != nil && *request.N > 1 {
		request.N = utils.GetPointer(1)
	}
}
