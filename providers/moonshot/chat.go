package moonshot

import (
	"net/http"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/providers/openai"
	"one-api/types"
)

func (p *MoonshotProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (openaiResponse *types.ChatCompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
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

func (p *MoonshotProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	streamOptions := forceMoonshotProviderUsage(request)
	req, errWithCode := p.GetRequestTextBody(config.RelayModeChatCompletions, request.Model, request)
	request.StreamOptions = streamOptions
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// 发送请求
	streamRequester := p.Requester.ForHTTPProfile(requester.HTTPProfileLongStream)
	resp, errWithCode := streamRequester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	chatHandler := openai.OpenAIStreamHandler{
		Usage:               p.Usage,
		ModelName:           request.Model,
		ExposeProviderUsage: streamOptions != nil && streamOptions.IncludeUsage,
	}

	return requester.RequestStreamWithOptions(streamRequester, resp, chatHandler.HandlerChatStream, requester.StreamReadOptions{
		RequireProtocolTerminal: true,
	})
}

func forceMoonshotProviderUsage(request *types.ChatCompletionRequest) *types.StreamOptions {
	if request == nil {
		return nil
	}
	original := request.StreamOptions
	request.StreamOptions = &types.StreamOptions{IncludeUsage: true}
	return original
}
