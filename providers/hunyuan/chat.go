package hunyuan

import (
	"encoding/json"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/types"
	"strings"
)

type tunyuanStreamHandler struct {
	Usage   *types.Usage
	Request *types.ChatCompletionRequest
}

func (p *HunyuanProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.getChatRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	tunyuanChatResponse := &ChatCompletionsResponse{}
	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, tunyuanChatResponse, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	return p.convertToChatOpenai(tunyuanChatResponse, request)
}

func (p *HunyuanProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.getChatRequest(request)
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

	chatHandler := &tunyuanStreamHandler{
		Usage:   p.Usage,
		Request: request,
	}

	return requester.RequestStream[string](streamRequester, resp, chatHandler.handlerStream)
}

func (p *HunyuanProvider) getChatRequest(request *types.ChatCompletionRequest) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	action, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatCompletions)
	if errWithCode != nil {
		return nil, errWithCode
	}

	tunyuanRequest := convertFromChatOpenai(request)
	req, errWithCode := p.sign(tunyuanRequest, action, http.MethodPost)
	if errWithCode != nil {
		return nil, errWithCode
	}

	return req, nil
}

func (p *HunyuanProvider) convertToChatOpenai(response *ChatCompletionsResponse, request *types.ChatCompletionRequest) (openaiResponse *types.ChatCompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	aiError := errorHandle(&response.Response.HunyuanResponseError)
	if aiError != nil {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *aiError,
			StatusCode:  http.StatusBadRequest,
		}
		return
	}

	txResponse := response.Response

	openaiResponse = &types.ChatCompletionResponse{
		Object:  "chat.completion",
		Created: txResponse.Created,
		Usage:   hunyuanUsageToOpenAI(txResponse.Usage),
		Model:   request.Model,
	}

	for _, choice := range txResponse.Choices {
		openaiResponse.Choices = append(openaiResponse.Choices, types.ChatCompletionChoice{
			Index:        0,
			Message:      types.ChatCompletionMessage{Role: choice.Message.Role, Content: choice.Message.Content},
			FinishReason: choice.FinishReason,
		})
	}

	if openaiResponse.Usage != nil {
		*p.Usage = *openaiResponse.Usage
	}

	return
}

func convertFromChatOpenai(request *types.ChatCompletionRequest) *ChatCompletionsRequest {
	messages := make([]*Message, 0, len(request.Messages))
	for _, message := range request.Messages {
		messages = append(messages, &Message{
			Content: message.StringContent(),
			Role:    message.Role,
		})
	}

	return &ChatCompletionsRequest{
		Model:       request.Model,
		Messages:    messages,
		Stream:      request.Stream,
		TopP:        request.TopP,
		Temperature: request.Temperature,
	}
}

// 转换为OpenAI聊天流式请求体
func (h *tunyuanStreamHandler) handlerStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	// 如果rawLine 前缀不为data:，则直接返回
	if !strings.HasPrefix(string(*rawLine), "data:") {
		*rawLine = nil
		return
	}

	// 去除前缀
	*rawLine = (*rawLine)[5:]

	var tunyuanChatResponse ChatCompletionsResponseParams
	err := json.Unmarshal(*rawLine, &tunyuanChatResponse)
	if err != nil {
		errChan <- common.ErrorToOpenAIError(err)
		return
	}

	aiError := errorHandle(&tunyuanChatResponse.HunyuanResponseError)
	if aiError != nil {
		errChan <- aiError
		return
	}

	h.convertToOpenaiStream(&tunyuanChatResponse, dataChan)
}

func (h *tunyuanStreamHandler) convertToOpenaiStream(tunyuanChatResponse *ChatCompletionsResponseParams, dataChan chan string) {
	streamResponse := types.ChatCompletionStreamResponse{
		Object:  "chat.completion.chunk",
		Created: tunyuanChatResponse.Created,
		Model:   h.Request.Model,
	}

	for _, choice := range tunyuanChatResponse.Choices {
		streamResponse.Choices = append(streamResponse.Choices, types.ChatCompletionStreamChoice{
			FinishReason: choice.FinishReason,
			Delta: types.ChatCompletionStreamChoiceDelta{
				Role:    choice.Delta.Role,
				Content: choice.Delta.Content,
			},
			Index: 0,
		})
	}

	responseBody, _ := json.Marshal(streamResponse)
	dataChan <- string(responseBody)

	terminal := false
	for _, choice := range tunyuanChatResponse.Choices {
		if strings.TrimSpace(choice.FinishReason) != "" {
			terminal = true
			break
		}
	}
	if terminal {
		if usage := hunyuanUsageToOpenAI(tunyuanChatResponse.Usage); usage != nil {
			*h.Usage = *usage
		}
	}
}

func hunyuanUsageToOpenAI(providerUsage *HunyuanUsage) *types.Usage {
	if providerUsage == nil {
		return nil
	}
	usage := &types.Usage{
		PromptTokens:     providerUsage.PromptTokens,
		CompletionTokens: providerUsage.CompletionTokens,
		TotalTokens:      providerUsage.TotalTokens,
	}
	if !providerUsage.promptTokensPresent || !providerUsage.completionTokensPresent || !providerUsage.totalTokensPresent {
		return usage
	}
	if providerUsage.PromptTokensDetails.CachedTokens != nil {
		usage.SetExtraTokens(config.UsageExtraCache, *providerUsage.PromptTokensDetails.CachedTokens)
	}
	if usage.PromptTokens+usage.CompletionTokens != usage.TotalTokens {
		usage.ProviderTokenConflict = true
	}
	usage.MarkProviderReported()
	return usage
}
