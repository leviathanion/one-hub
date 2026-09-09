package gemini

import (
	"encoding/json"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/common/utils"
	"one-api/providers/base"
	"one-api/types"
	"strings"
)

const (
	GeminiVisionMaxImageNum = 16
)

type GeminiStreamHandler struct {
	Usage                *types.Usage
	Request              *types.ChatCompletionRequest
	RequireCachedContent bool
	RequireInputImage    bool

	key string
}

type OpenAIStreamHandler struct {
	Usage     *types.Usage
	ModelName string
}

func (p *GeminiProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	if p.UseOpenaiAPI {
		return p.OpenAIProvider.CreateChatCompletion(request)
	}

	geminiRequest, errWithCode := ConvertFromChatOpenai(request)
	if errWithCode != nil {
		return nil, errWithCode
	}

	req, errWithCode := p.getChatRequest(geminiRequest, false)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	geminiChatResponse := &GeminiChatResponse{}
	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, geminiChatResponse, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	response, conversionErr := ConvertToChatOpenai(p, geminiChatResponse, request)
	applyGeminiUsageRequirements(p.Usage, geminiRequest.UsesCachedContent(), geminiRequest.UsesInputImages())
	return response, conversionErr
}

func (p *GeminiProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	channel := p.GetChannel()
	if p.UseOpenaiAPI {
		return p.OpenAIProvider.CreateChatCompletionStream(request)
	}

	geminiRequest, errWithCode := ConvertFromChatOpenai(request)
	if errWithCode != nil {
		return nil, errWithCode
	}

	req, errWithCode := p.getChatRequest(geminiRequest, false)
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

	chatHandler := &GeminiStreamHandler{
		Usage:                p.Usage,
		Request:              request,
		RequireCachedContent: geminiRequest.UsesCachedContent(),
		RequireInputImage:    geminiRequest.UsesInputImages(),

		key: channel.Key,
	}

	return requester.RequestStream(streamRequester, resp, chatHandler.HandlerStream)
}

func (p *GeminiProvider) getChatRequest(geminiRequest *GeminiChatRequest, isRelay bool) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	url := "generateContent"
	if geminiRequest.Stream {
		url = "streamGenerateContent?alt=sse"
	}
	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url, geminiRequest.Model)

	// 获取请求头
	headers := p.GetRequestHeaders()
	if geminiRequest.Stream {
		headers["Accept"] = "text/event-stream"
	}

	var body any
	if isRelay {
		var exists bool
		body, exists = p.GetRawBody()
		if !exists {
			return nil, common.StringErrorWrapperLocal("request body not found", "request_body_not_found", http.StatusInternalServerError)
		}
	} else {
		p.pluginHandle(geminiRequest)
		body = geminiRequest
	}

	// 创建请求
	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(body), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}

	return req, nil
}

func ConvertFromChatOpenai(request *types.ChatCompletionRequest) (*GeminiChatRequest, *types.OpenAIErrorWithStatusCode) {
	if err := ValidateNativeChatRequest(request.Model, request, "Gemini Chat"); err != nil {
		code := "unsupported_capability"
		if capabilityErr, ok := err.(*base.RequestCapabilityError); ok {
			switch capabilityErr.Param {
			case "service_tier":
				code = "gemini_service_tier_unsupported"
			case "audio":
				code = "gemini_audio_output_unsupported"
			case "tools":
				code = "gemini_grounding_billing_unsupported"
			}
		}
		return nil, common.StringErrorWrapperLocal(err.Error(), code, http.StatusBadRequest)
	}
	threshold := "BLOCK_NONE"
	reasoning := request.EffectiveReasoning()

	// if strings.HasPrefix(request.Model, "gemini-2.0") && !strings.Contains(request.Model, "thinking") {
	// 	threshold = "OFF"
	// }

	geminiRequest := GeminiChatRequest{
		Contents: make([]GeminiChatContent, 0, len(request.Messages)),
		SafetySettings: []GeminiChatSafetySettings{
			{
				Category:  "HARM_CATEGORY_HARASSMENT",
				Threshold: threshold,
			},
			{
				Category:  "HARM_CATEGORY_HATE_SPEECH",
				Threshold: threshold,
			},
			{
				Category:  "HARM_CATEGORY_SEXUALLY_EXPLICIT",
				Threshold: threshold,
			},
			{
				Category:  "HARM_CATEGORY_DANGEROUS_CONTENT",
				Threshold: threshold,
			},
			{
				Category:  "HARM_CATEGORY_CIVIC_INTEGRITY",
				Threshold: threshold,
			},
		},
		GenerationConfig: GeminiChatGenerationConfig{
			Temperature:        request.Temperature,
			TopP:               request.TopP,
			TopK:               request.TopK,
			MaxOutputTokens:    request.MaxCompletionTokens,
			ResponseModalities: request.Modalities,
		},
	}
	if request.Stop != nil {
		encodedStop, err := json.Marshal(request.Stop)
		if err != nil {
			return nil, common.ErrorWrapperLocal(err, "invalid_stop", http.StatusBadRequest)
		}
		var stopSequences []string
		if err := json.Unmarshal(encodedStop, &stopSequences); err != nil {
			var stop string
			if stringErr := json.Unmarshal(encodedStop, &stop); stringErr != nil {
				return nil, common.ErrorWrapperLocal(err, "invalid_stop", http.StatusBadRequest)
			}
			stopSequences = []string{stop}
		}
		geminiRequest.GenerationConfig.StopSequences = stopSequences
	}

	if strings.HasPrefix(request.Model, "gemini-2.0-flash-exp") || strings.HasPrefix(request.Model, "gemini-2.5-flash-image-preview") {
		geminiRequest.GenerationConfig.ResponseModalities = []string{"Text", "Image"}
	}

	if strings.HasSuffix(request.Model, "-tts") {
		geminiRequest.GenerationConfig.ResponseModalities = []string{"AUDIO"}
	}

	if reasoning != nil {
		geminiRequest.GenerationConfig.ThinkingConfig = geminiThinkingConfig(request.Model, reasoning)
	}

	if config.RuntimeGeminiOpenThinkFromSnapshot(config.GlobalOption.RuntimeSnapshot(), request.Model) {
		if geminiRequest.GenerationConfig.ThinkingConfig == nil {
			geminiRequest.GenerationConfig.ThinkingConfig = &ThinkingConfig{}
		}
		geminiRequest.GenerationConfig.ThinkingConfig.IncludeThoughts = true
	}

	functions := request.GetFunctions()

	if functions != nil {
		var geminiChatTools GeminiChatTools
		codeExecution := false
		urlContext := false
		for _, function := range functions {
			if function.Name == "codeExecution" {
				codeExecution = true
				continue
			}
			if function.Name == "urlContext" {
				urlContext = true
				continue
			}

			if params, ok := function.Parameters.(map[string]interface{}); ok {
				if properties, ok := params["properties"].(map[string]interface{}); ok && len(properties) == 0 {
					function.Parameters = nil
				}
			}

			geminiChatTools.FunctionDeclarations = append(geminiChatTools.FunctionDeclarations, *function)
		}

		if codeExecution && len(geminiRequest.Tools) == 0 {
			geminiRequest.Tools = append(geminiRequest.Tools, GeminiChatTools{
				CodeExecution: &GeminiCodeExecution{},
			})
		}
		if urlContext && len(geminiRequest.Tools) == 0 {
			geminiRequest.Tools = append(geminiRequest.Tools, GeminiChatTools{
				UrlContext: &GeminiCodeExecution{},
			})
		}

		if len(geminiRequest.Tools) == 0 {
			geminiRequest.Tools = append(geminiRequest.Tools, geminiChatTools)
		}
	}

	geminiContent, systemContent, err := OpenAIToGeminiChatContent(request.Messages)
	if err != nil {
		return nil, err
	}

	if systemContent != "" {
		geminiRequest.SystemInstruction = &GeminiChatContent{
			Parts: []GeminiPart{
				{Text: systemContent},
			},
		}
	}

	geminiRequest.Contents = geminiContent
	geminiRequest.Stream = request.Stream
	geminiRequest.Model = request.Model

	if request.ResponseFormat != nil && (request.ResponseFormat.Type == "json_schema" || request.ResponseFormat.Type == "json_object") {
		geminiRequest.GenerationConfig.ResponseMimeType = "application/json"

		if request.ResponseFormat.JsonSchema != nil && request.ResponseFormat.JsonSchema.Schema != nil {
			cleanedSchema := removeAdditionalPropertiesWithDepth(request.ResponseFormat.JsonSchema.Schema, 0)
			geminiRequest.GenerationConfig.ResponseSchema = cleanedSchema
		}
	}

	return &geminiRequest, nil
}

func geminiThinkingConfig(model string, reasoning *types.ChatReasoning) *ThinkingConfig {
	if reasoning == nil {
		return nil
	}
	if reasoning.HasMaxTokens() {
		budget := reasoning.MaxTokens
		return &ThinkingConfig{ThinkingBudget: &budget}
	}

	effort := strings.ToLower(strings.TrimSpace(reasoning.Effort))
	if effort == "" {
		return nil
	}
	if strings.Contains(strings.ToLower(model), "gemini-2.5") {
		budgetByEffort := map[string]int{
			"none":    0,
			"minimal": 1024,
			"low":     1024,
			"medium":  8192,
			"high":    24576,
		}
		budget, ok := budgetByEffort[effort]
		if !ok {
			return nil
		}
		return &ThinkingConfig{ThinkingBudget: &budget}
	}

	levelByEffort := map[string]string{
		"minimal": "MINIMAL",
		"low":     "LOW",
		"medium":  "MEDIUM",
		"high":    "HIGH",
	}
	level := levelByEffort[effort]
	if level == "" {
		return nil
	}
	return &ThinkingConfig{ThinkingLevel: level}
}

func removeAdditionalPropertiesWithDepth(schema interface{}, depth int) interface{} {
	if depth >= 5 {
		return schema
	}

	v, ok := schema.(map[string]interface{})
	if !ok || len(v) == 0 {
		return schema
	}

	// 如果type不为object和array，则直接返回
	if typeVal, exists := v["type"]; !exists || (typeVal != "object" && typeVal != "array") {
		return schema
	}

	delete(v, "title")

	switch v["type"] {
	case "object":
		delete(v, "additionalProperties")
		// 处理 properties
		if properties, ok := v["properties"].(map[string]interface{}); ok {
			for key, value := range properties {
				properties[key] = removeAdditionalPropertiesWithDepth(value, depth+1)
			}
		}
		for _, field := range []string{"allOf", "anyOf", "oneOf"} {
			if nested, ok := v[field].([]interface{}); ok {
				for i, item := range nested {
					nested[i] = removeAdditionalPropertiesWithDepth(item, depth+1)
				}
			}
		}
	case "array":
		if items, ok := v["items"].(map[string]interface{}); ok {
			v["items"] = removeAdditionalPropertiesWithDepth(items, depth+1)
		}
	}

	return v
}

func ConvertToChatOpenai(provider base.ProviderInterface, response *GeminiChatResponse, request *types.ChatCompletionRequest) (openaiResponse *types.ChatCompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	openaiResponse = &types.ChatCompletionResponse{
		ID:      response.ResponseId,
		Object:  "chat.completion",
		Created: utils.GetTimestamp(),
		Model:   request.Model,
		Choices: make([]types.ChatCompletionChoice, 0, len(response.Candidates)),
	}

	usage := provider.GetUsage()
	*usage = ConvertOpenAIUsage(response.UsageMetadata, firstNonEmpty(response.ModelVersion, response.Model))
	openaiResponse.Usage = usage

	if len(response.Candidates) == 0 {
		errWithCode = common.StringErrorWrapper("no candidates", "no_candidates", http.StatusInternalServerError)
		return
	}

	for _, candidate := range response.Candidates {
		openaiResponse.Choices = append(openaiResponse.Choices, candidate.ToOpenAIChoice(request))
	}

	return
}

// 转换为OpenAI聊天流式请求体
func (h *GeminiStreamHandler) HandlerStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	// 如果rawLine 前缀不为data:，则直接返回
	if !strings.HasPrefix(string(*rawLine), "data: ") {
		*rawLine = nil
		return
	}

	// 去除前缀
	*rawLine = (*rawLine)[6:]

	var geminiResponse GeminiChatResponse
	err := json.Unmarshal(*rawLine, &geminiResponse)
	if err != nil {
		errChan <- common.ErrorToOpenAIError(err)
		return
	}

	aiError := errorHandle(&geminiResponse.GeminiErrorResponse, h.key)
	if aiError != nil {
		errChan <- aiError
		return
	}

	h.convertToOpenaiStream(&geminiResponse, dataChan)
}

func (h *GeminiStreamHandler) convertToOpenaiStream(geminiResponse *GeminiChatResponse, dataChan chan string) {
	streamResponse := types.ChatCompletionStreamResponse{
		ID:      geminiResponse.ResponseId,
		Object:  "chat.completion.chunk",
		Created: utils.GetTimestamp(),
		Model:   h.Request.Model,
		// Choices: choices,
	}

	choices := make([]types.ChatCompletionStreamChoice, 0, len(geminiResponse.Candidates))

	isStop := false
	for _, candidate := range geminiResponse.Candidates {
		if candidate.FinishReason != nil && *candidate.FinishReason == "STOP" {
			isStop = true
			candidate.FinishReason = nil
		}
		choices = append(choices, candidate.ToOpenAIStreamChoice(h.Request))
	}

	if len(choices) > 0 && (choices[0].Delta.ToolCalls != nil || choices[0].Delta.FunctionCall != nil) {
		choices := choices[0].ConvertOpenaiStream()
		for _, choice := range choices {
			chatCompletionCopy := streamResponse
			chatCompletionCopy.Choices = []types.ChatCompletionStreamChoice{choice}
			responseBody, _ := json.Marshal(chatCompletionCopy)
			dataChan <- string(responseBody)
		}
	} else {
		streamResponse.Choices = choices
		responseBody, _ := json.Marshal(streamResponse)
		dataChan <- string(responseBody)
	}

	if isStop {
		streamResponse.Choices = []types.ChatCompletionStreamChoice{
			{
				FinishReason: types.FinishReasonStop,
				Delta: types.ChatCompletionStreamChoiceDelta{
					Role: types.ChatMessageRoleAssistant,
				},
			},
		}
		responseBody, _ := json.Marshal(streamResponse)
		dataChan <- string(responseBody)
	}

	// 和ExecutableCode的tokens共用，所以跳过
	if geminiResponse.UsageMetadata == nil {
		return
	}

	usage := ConvertOpenAIUsage(geminiResponse.UsageMetadata, firstNonEmpty(geminiResponse.ModelVersion, geminiResponse.Model))

	usage.MergeProviderAttribution(h.Usage.ResponseModel, h.Usage.ServiceTier)
	applyGeminiUsageRequirements(&usage, h.RequireCachedContent, h.RequireInputImage)
	*h.Usage = usage
}

// func adjustTokenCounts(modelName string, usage *GeminiUsageMetadata) {
// 	if usage.PromptTokenCount <= tokenThreshold && usage.CandidatesTokenCount <= tokenThreshold {
// 		return
// 	}

// 	currentRatio := 1
// 	for model, r := range modelAdjustRatios {
// 		if strings.HasPrefix(modelName, model) {
// 			currentRatio = r
// 			break
// 		}
// 	}

// 	if currentRatio == 1 {
// 		return
// 	}

// 	adjustTokenCount := func(count int) int {
// 		if count > tokenThreshold {
// 			return tokenThreshold + (count-tokenThreshold)*currentRatio
// 		}
// 		return count
// 	}

// 	if usage.PromptTokenCount > tokenThreshold {
// 		usage.PromptTokenCount = adjustTokenCount(usage.PromptTokenCount)
// 	}

// 	if usage.CandidatesTokenCount > tokenThreshold {
// 		usage.CandidatesTokenCount = adjustTokenCount(usage.CandidatesTokenCount)
// 	}

// 	usage.TotalTokenCount = usage.PromptTokenCount + usage.CandidatesTokenCount
// }

func ConvertOpenAIUsage(geminiUsage *GeminiUsageMetadata, actualModel string) types.Usage {
	if geminiUsage == nil {
		return types.Usage{}
	}

	usage := types.Usage{
		PromptTokens:     geminiUsage.PromptTokenCount + geminiUsage.ToolUsePromptTokenCount,
		CompletionTokens: geminiUsage.CandidatesTokenCount + geminiUsage.ThoughtsTokenCount,
		TotalTokens:      geminiUsage.TotalTokenCount,

		CompletionTokensDetails: types.CompletionTokensDetails{
			ReasoningTokens: geminiUsage.ThoughtsTokenCount,
		},
	}
	usage.MergeProviderAttribution(actualModel, geminiUsage.ServiceTier)
	if !geminiUsageTokenCountsNonNegative(geminiUsage) {
		return usage
	}
	candidatesTokenCountPresent := geminiUsage.candidatesTokenCountPresent
	if !candidatesTokenCountPresent {
		candidatesTokenCountPresent = geminiUsageCanProveZeroCandidates(geminiUsage)
	}
	if !geminiUsage.promptTokenCountPresent || !candidatesTokenCountPresent || !geminiUsage.totalTokenCountPresent {
		return usage
	}
	if !geminiUsageTokenTotalsMatch(geminiUsage) {
		usage.ProviderTokenConflict = true
		usage.BillingDiagnostics = map[string]bool{"gemini_token_total_conflict": true}
		return usage
	}
	usage.PromptTokensDetails.CachedTokens = geminiUsage.CachedContentTokenCount
	if geminiUsage.cachedContentPresent {
		usage.SetExtraTokens(config.UsageExtraCache, geminiUsage.CachedContentTokenCount)
	}
	if geminiUsage.ToolUsePromptTokenCount > 0 {
		usage.SetExtraTokens(config.UsageExtraToolUsePrompt, geminiUsage.ToolUsePromptTokenCount)
	}

	for _, p := range geminiUsage.PromptTokensDetails {
		switch p.Modality {
		case "TEXT":
			usage.PromptTokensDetails.TextTokens = p.TokenCount
		case "AUDIO":
			usage.PromptTokensDetails.AudioTokens = p.TokenCount
		case "IMAGE":
			usage.PromptTokensDetails.ImageTokens = p.TokenCount
			usage.SetExtraTokens(config.UsageExtraInputImageTokens, p.TokenCount)
		}
	}

	for _, c := range geminiUsage.CandidatesTokensDetails {
		switch c.Modality {
		case "TEXT":
			usage.CompletionTokensDetails.TextTokens = c.TokenCount
		case "AUDIO":
			usage.CompletionTokensDetails.AudioTokens = c.TokenCount
		case "IMAGE":
			usage.CompletionTokensDetails.ImageTokens = c.TokenCount
			usage.SetExtraTokens(config.UsageExtraOutputImageTokens, c.TokenCount)
		}
	}
	usage.MarkProviderReported()

	return usage
}

func geminiUsageTokenCountsNonNegative(geminiUsage *GeminiUsageMetadata) bool {
	if geminiUsage == nil || geminiUsage.invalidTokenCountField || geminiUsage.PromptTokenCount < 0 || geminiUsage.CandidatesTokenCount < 0 || geminiUsage.TotalTokenCount < 0 || geminiUsage.ThoughtsTokenCount < 0 || geminiUsage.ToolUsePromptTokenCount < 0 || geminiUsage.CachedContentTokenCount < 0 {
		return false
	}
	for _, details := range [][]GeminiUsageMetadataDetails{
		geminiUsage.PromptTokensDetails,
		geminiUsage.CandidatesTokensDetails,
		geminiUsage.CacheTokensDetails,
		geminiUsage.ToolUsePromptTokensDetails,
	} {
		for _, detail := range details {
			if detail.TokenCount < 0 {
				return false
			}
		}
	}
	return true
}

func geminiUsageCanProveZeroCandidates(geminiUsage *GeminiUsageMetadata) bool {
	// ProtoJSON may omit a scalar zero. Treat an omitted candidates count as
	// zero only when every other count that can contribute to total is known
	// and leaves no candidate remainder; this is not a total-based estimate.
	if geminiUsage == nil || geminiUsage.candidatesTokenCountPresent || !geminiUsage.promptTokenCountPresent || !geminiUsage.totalTokenCountPresent || !geminiUsageTokenCountsNonNegative(geminiUsage) {
		return false
	}
	if !geminiUsageKnownTokenDimensionsConsistent(geminiUsage) {
		return false
	}
	for _, detail := range geminiUsage.CandidatesTokensDetails {
		if detail.TokenCount != 0 {
			return false
		}
	}
	if geminiUsageToolDetailsConflict(geminiUsage) {
		return false
	}
	if geminiUsage.PromptTokenCount > geminiUsage.TotalTokenCount {
		return false
	}
	remaining := geminiUsage.TotalTokenCount - geminiUsage.PromptTokenCount
	if geminiUsage.ToolUsePromptTokenCount > remaining {
		return false
	}
	remaining -= geminiUsage.ToolUsePromptTokenCount
	return geminiUsage.ThoughtsTokenCount == remaining
}

func geminiUsageTokenTotalsMatch(geminiUsage *GeminiUsageMetadata) bool {
	if !geminiUsageTokenCountsNonNegative(geminiUsage) || geminiUsage == nil || !geminiUsage.promptTokenCountPresent || !geminiUsage.totalTokenCountPresent {
		return false
	}
	candidatesTokenCountPresent := geminiUsage.candidatesTokenCountPresent
	if !candidatesTokenCountPresent {
		candidatesTokenCountPresent = geminiUsageCanProveZeroCandidates(geminiUsage)
	}
	if !candidatesTokenCountPresent || geminiUsageToolDetailsConflict(geminiUsage) || !geminiUsageKnownTokenDimensionsConsistent(geminiUsage) {
		return false
	}
	if geminiUsage.PromptTokenCount > geminiUsage.TotalTokenCount {
		return false
	}
	remaining := geminiUsage.TotalTokenCount - geminiUsage.PromptTokenCount
	if geminiUsage.ToolUsePromptTokenCount > remaining {
		return false
	}
	remaining -= geminiUsage.ToolUsePromptTokenCount
	if geminiUsage.CandidatesTokenCount > remaining {
		return false
	}
	remaining -= geminiUsage.CandidatesTokenCount
	return geminiUsage.ThoughtsTokenCount == remaining
}

func geminiUsageToolDetailsConflict(geminiUsage *GeminiUsageMetadata) bool {
	if geminiUsage == nil || len(geminiUsage.ToolUsePromptTokensDetails) == 0 {
		return false
	}
	maxInt := int(^uint(0) >> 1)
	total := 0
	for _, detail := range geminiUsage.ToolUsePromptTokensDetails {
		if detail.TokenCount > maxInt-total {
			return true
		}
		total += detail.TokenCount
	}
	if !geminiUsage.toolUsePromptTokenCountPresent {
		return total != 0
	}
	return total != geminiUsage.ToolUsePromptTokenCount
}

func geminiUsageKnownTokenDimensionsConsistent(geminiUsage *GeminiUsageMetadata) bool {
	if geminiUsage == nil {
		return false
	}
	if geminiUsage.CachedContentTokenCount > geminiUsage.PromptTokenCount {
		return false
	}
	if geminiUsageTokenDetailsExceed(geminiUsage.PromptTokensDetails, geminiUsage.PromptTokenCount) {
		return false
	}
	if geminiUsageTokenDetailsExceed(geminiUsage.CacheTokensDetails, geminiUsage.PromptTokenCount) {
		return false
	}
	if geminiUsage.cachedContentPresent && geminiUsageTokenDetailsExceed(geminiUsage.CacheTokensDetails, geminiUsage.CachedContentTokenCount) {
		return false
	}
	return true
}

func geminiUsageTokenDetailsExceed(details []GeminiUsageMetadataDetails, limit int) bool {
	maxInt := int(^uint(0) >> 1)
	total := 0
	for _, detail := range details {
		if detail.TokenCount > maxInt-total {
			return true
		}
		total += detail.TokenCount
	}
	return total > limit
}

func applyGeminiUsageRequirements(usage *types.Usage, cachedContent, inputImage bool) {
	if usage == nil {
		return
	}
	required := append([]string(nil), usage.RequiredTokenExtraKeys...)
	if cachedContent {
		required = append(required, config.UsageExtraCache)
	}
	if inputImage {
		required = append(required, config.UsageExtraInputImageTokens)
	}
	usage.RequireTokenExtraEvidence(required...)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (p *GeminiProvider) pluginHandle(request *GeminiChatRequest) {
	if !p.UseCodeExecution {
		return
	}

	if len(request.Tools) > 0 {
		return
	}

	if p.Channel.Plugin == nil {
		return
	}

	request.Tools = append(request.Tools, GeminiChatTools{
		CodeExecution: &GeminiCodeExecution{},
	})
}
