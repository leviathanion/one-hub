package ali

import (
	"encoding/json"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/common/utils"
	"one-api/types"
	"strings"
)

type aliStreamHandler struct {
	Usage              *types.Usage
	Request            *types.ChatCompletionRequest
	lastStreamResponse string
}

func (p *AliProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	if p.UseOpenaiAPI {
		if errWithCode := p.validateEffectiveSearch(request); errWithCode != nil {
			return nil, errWithCode
		}
		return p.OpenAIProvider.CreateChatCompletion(request)
	}

	req, errWithCode := p.getAliChatRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	aliResponse := &AliChatResponse{}
	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, aliResponse, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	return p.convertToChatOpenai(aliResponse, request)
}

func (p *AliProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	if p.UseOpenaiAPI {
		if errWithCode := p.validateEffectiveSearch(request); errWithCode != nil {
			return nil, errWithCode
		}
		return p.OpenAIProvider.CreateChatCompletionStream(request)
	}

	req, errWithCode := p.getAliChatRequest(request)
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

	chatHandler := &aliStreamHandler{
		Usage:   p.Usage,
		Request: request,
	}

	return requester.RequestStream[string](streamRequester, resp, chatHandler.handlerStream)
}

func (p *AliProvider) getAliChatRequest(request *types.ChatCompletionRequest) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	if errWithCode := p.validateEffectiveSearch(request); errWithCode != nil {
		return nil, errWithCode
	}
	url, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatCompletions)
	if errWithCode != nil {
		return nil, errWithCode
	}
	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url, request.Model)

	// 获取请求头
	headers := p.GetRequestHeaders()
	if request.Stream {
		headers["Accept"] = "text/event-stream"
		headers["X-DashScope-SSE"] = "enable"
	}

	aliRequest := p.convertFromChatOpenai(request)
	// 使用通用的 BuildRequestWithMerge 处理 AllowExtraBody 和 CustomParameter 参数透传
	req, errWithCode := p.BuildRequestWithMerge(aliRequest, fullRequestURL, headers, request.Model)
	if errWithCode != nil {
		return nil, errWithCode
	}

	return req, nil
}

func (p *AliProvider) validateEffectiveSearch(request *types.ChatCompletionRequest) *types.OpenAIErrorWithStatusCode {
	if p == nil || request == nil {
		return nil
	}
	raw, _, err := p.GetRawBodyMap()
	if err != nil {
		return common.StringErrorWrapperLocal("Chat request cannot be evaluated for DashScope search billing evidence", "ali_search_billing_unsupported", http.StatusBadRequest)
	}
	if err := validateAliSearchPlan(p.Channel, request.Model, request, raw); err != nil {
		return common.StringErrorWrapperLocal(err.Error(), "ali_search_billing_unsupported", http.StatusBadRequest)
	}
	return nil
}

// 转换为OpenAI聊天请求体
func (p *AliProvider) convertToChatOpenai(response *AliChatResponse, request *types.ChatCompletionRequest) (openaiResponse *types.ChatCompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	aiError := errorHandle(&response.AliError)
	if aiError != nil {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *aiError,
			StatusCode:  http.StatusBadRequest,
		}
		return
	}

	openaiResponse = &types.ChatCompletionResponse{
		ID:      response.RequestId,
		Object:  "chat.completion",
		Created: utils.GetTimestamp(),
		Model:   request.Model,
		Choices: response.Output.ToChatCompletionChoices(),
		Usage:   aliUsageToOpenAI(&response.Usage),
	}
	if openaiResponse.Usage != nil {
		applyAliUsageRequirements(openaiResponse.Usage, request)
		*p.Usage = *openaiResponse.Usage
	}

	return
}

// 阿里云聊天请求体
func (p *AliProvider) convertFromChatOpenai(request *types.ChatCompletionRequest) *AliChatRequest {
	messages := make([]AliMessage, 0, len(request.Messages))
	for i := 0; i < len(request.Messages); i++ {
		message := request.Messages[i]
		modelKeywords := strings.Split(VisionModelKeywords, ",")
		isVisionModel := false
		for _, keyword := range modelKeywords {
			if strings.Contains(request.Model, keyword) {
				isVisionModel = true
				break
			}
		}
		if !isVisionModel {
			messages = append(messages, AliMessage{
				Content: message.StringContent(),
				Role:    strings.ToLower(message.Role),
			})
		} else {
			openaiContent := message.ParseContent()
			var parts []AliMessagePart
			for _, part := range openaiContent {
				if part.Type == types.ContentTypeText {
					parts = append(parts, AliMessagePart{
						Text: part.Text,
					})
				} else if part.Type == types.ContentTypeImageURL {
					parts = append(parts, AliMessagePart{
						Image: part.ImageURL.URL,
					})
				}
			}
			messages = append(messages, AliMessage{
				Content: parts,
				Role:    strings.ToLower(message.Role),
			})
		}
	}

	aliChatRequest := &AliChatRequest{
		Model: request.Model,
		Input: AliInput{
			Messages: messages,
		},
		Parameters: AliParameters{
			ResultFormat:      "message",
			IncrementalOutput: request.Stream,
		},
	}

	p.pluginHandle(aliChatRequest)

	return aliChatRequest
}

func (p *AliProvider) pluginHandle(request *AliChatRequest) {
	request.Parameters.EnableSearch = aliSearchEnabled(p.Channel, request.Model)
}

// 转换为OpenAI聊天流式请求体
func (h *aliStreamHandler) handlerStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	// 如果rawLine 前缀不为data:，则直接返回
	if !strings.HasPrefix(string(*rawLine), "data:") {
		*rawLine = nil
		return
	}

	// 去除前缀
	*rawLine = (*rawLine)[5:]

	var aliResponse AliChatResponse
	err := json.Unmarshal(*rawLine, &aliResponse)
	if err != nil {
		errChan <- common.ErrorToOpenAIError(err)
		return
	}

	aiError := errorHandle(&aliResponse.AliError)
	if aiError != nil {
		errChan <- aiError
		return
	}

	h.convertToOpenaiStream(&aliResponse, dataChan)
}

func (h *aliStreamHandler) convertToOpenaiStream(aliResponse *AliChatResponse, dataChan chan string) {
	content := aliResponse.Output.Choices[0].Message.StringContent()
	reasoningContent := aliResponse.Output.Choices[0].Message.ReasoningContent
	var choice types.ChatCompletionStreamChoice
	choice.Index = aliResponse.Output.Choices[0].Index
	choice.Delta.Content = strings.TrimPrefix(content, h.lastStreamResponse)
	choice.Delta.ReasoningContent = reasoningContent
	if aliResponse.Output.Choices[0].FinishReason != "" {
		if aliResponse.Output.Choices[0].FinishReason != "null" {
			finishReason := aliResponse.Output.Choices[0].FinishReason
			choice.FinishReason = &finishReason
		}
	}

	if aliResponse.Output.FinishReason != "" {
		if aliResponse.Output.FinishReason != "null" {
			finishReason := aliResponse.Output.FinishReason
			choice.FinishReason = &finishReason
		}
	}

	h.lastStreamResponse = content
	streamResponse := types.ChatCompletionStreamResponse{
		ID:      aliResponse.RequestId,
		Object:  "chat.completion.chunk",
		Created: utils.GetTimestamp(),
		Model:   h.Request.Model,
		Choices: []types.ChatCompletionStreamChoice{choice},
	}

	if usage := aliUsageToOpenAI(&aliResponse.Usage); usage != nil {
		applyAliUsageRequirements(usage, h.Request)
		*h.Usage = *usage
	}

	responseBody, _ := json.Marshal(streamResponse)
	dataChan <- string(responseBody)
}

func aliUsageToOpenAI(providerUsage *AliUsage) *types.Usage {
	if providerUsage == nil || !providerUsage.present {
		return nil
	}
	usage := &types.Usage{
		PromptTokens:     providerUsage.InputTokens,
		CompletionTokens: providerUsage.OutputTokens,
		TotalTokens:      providerUsage.TotalTokens,
	}
	if !providerUsage.inputTokensPresent || !providerUsage.outputTokensPresent || !providerUsage.totalTokensPresent {
		return usage
	}
	if usage.PromptTokens+usage.CompletionTokens != usage.TotalTokens {
		usage.ProviderTokenConflict = true
	}
	setAliTokenDetail := func(value *int, key string) {
		if value != nil {
			usage.SetExtraTokens(key, *value)
		}
	}
	setAliTokenDetail(providerUsage.InputTokenDetails.CachedTokens, config.UsageExtraCache)
	setAliTokenDetail(providerUsage.InputTokenDetails.TextTokens, config.UsageExtraInputTextTokens)
	setAliTokenDetail(providerUsage.InputTokenDetails.ImageTokens, config.UsageExtraInputImageTokens)
	setAliTokenDetail(providerUsage.InputTokenDetails.AudioTokens, config.UsageExtraInputAudio)
	setAliTokenDetail(providerUsage.InputTokenDetails.VideoTokens, config.UsageExtraInputVideoTokens)
	setAliTokenDetail(providerUsage.OutputTokenDetails.TextTokens, config.UsageExtraOutputTextTokens)
	setAliTokenDetail(providerUsage.OutputTokenDetails.ImageTokens, config.UsageExtraOutputImageTokens)
	setAliTokenDetail(providerUsage.OutputTokenDetails.AudioTokens, config.UsageExtraOutputAudio)
	setAliTokenDetail(providerUsage.OutputTokenDetails.VideoTokens, config.UsageExtraOutputVideoTokens)
	setAliTokenDetail(providerUsage.OutputTokenDetails.ReasoningTokens, config.UsageExtraReasoning)
	usage.MarkProviderReported()
	return usage
}

func applyAliUsageRequirements(usage *types.Usage, request *types.ChatCompletionRequest) {
	if usage == nil || request == nil {
		return
	}
	for _, message := range request.Messages {
		for _, part := range message.ParseContent() {
			if part.Type == types.ContentTypeImageURL {
				usage.RequireTokenExtraEvidence(config.UsageExtraInputImageTokens)
				return
			}
		}
	}
}
