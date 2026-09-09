package openrouter

import (
	"encoding/json"
	"net/http"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/providers/openai"
	"one-api/types"
	"strings"
)

func (p *OpenRouterProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (openaiResponse *types.ChatCompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	orRequest := &ChatCompletionRequest{
		ChatCompletionRequest: *request,
	}

	modelProvider := strings.Split(request.Model, "/")[0]

	p.ConvertFromChatOpenai(orRequest, modelProvider)

	req, errWithCode := p.GetRequestTextBody(config.RelayModeChatCompletions, request.Model, orRequest)
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
		p.observeOpenRouterUsage(response.Usage)
		*p.Usage = *response.Usage
	}

	for index, choices := range response.Choices {
		if choices.Message.ReasoningContent == "" && choices.Message.Reasoning != "" {
			response.Choices[index].Message.ReasoningContent = choices.Message.Reasoning
			response.Choices[index].Message.Reasoning = ""
		}
	}

	return &response.ChatCompletionResponse, nil
}

func (p *OpenRouterProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	orRequest := &ChatCompletionRequest{
		ChatCompletionRequest: *request,
	}

	modelProvider := strings.Split(request.Model, "/")[0]
	p.ConvertFromChatOpenai(orRequest, modelProvider)

	streamOptions := orRequest.StreamOptions
	// 如果支持流式返回Usage 则需要更改配置：
	orRequest.StreamOptions = &types.StreamOptions{
		IncludeUsage: true,
	}

	req, errWithCode := p.GetRequestTextBody(config.RelayModeChatCompletions, orRequest.Model, orRequest)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// 恢复原来的配置
	orRequest.StreamOptions = streamOptions

	// 发送请求
	streamRequester := p.Requester.ForHTTPProfile(requester.HTTPProfileLongStream)
	resp, errWithCode := streamRequester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	chatHandler := openai.OpenAIStreamHandler{
		Usage:      p.Usage,
		ModelName:  request.Model,
		EscapeJSON: p.StreamEscapeJSON,

		ReasoningHandler: p.ReasoningHandler,
		UsageHandler:     p.observeOpenRouterUsage,
	}

	return requester.RequestStreamWithOptions(streamRequester, resp, chatHandler.HandlerChatStream, requester.StreamReadOptions{
		RequireProtocolTerminal: true,
	})
}

// observeOpenRouterUsage is called only at the OpenRouter provider boundary.
// The generic OpenAI handler keeps server_tool_use as opaque usage metadata;
// this provider-specific hook authorizes the documented search counter.
func (p *OpenRouterProvider) observeOpenRouterUsage(usage *types.Usage) bool {
	if usage == nil || usage.ProviderServerToolUse == nil {
		return false
	}
	if usage.ProviderServerToolUse.WebSearchRequests > 0 {
		usage.SetProviderExtraBilling(types.APIToolTypeWebSearch, "", usage.ProviderServerToolUse.WebSearchRequests)
	}
	return false
}

func (p *OpenRouterProvider) ConvertFromChatOpenai(request *ChatCompletionRequest, modelProvider string) {
	if p.Channel.Plugin != nil {
		plugin := p.Channel.Plugin.Data()
		if pOther, ok := plugin["other"]; ok {
			if provider, ok := pOther["provider"].(string); ok && provider != "" {
				var orProvider map[string]orProvider
				err := json.Unmarshal([]byte(provider), &orProvider)
				if err == nil {
					if _, ok := orProvider[modelProvider]; ok {
						request.Provider = orProvider[modelProvider]
					}
				}
			}
		}
	}

}
