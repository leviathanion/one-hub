package replicate

import (
	"encoding/json"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/common/utils"
	"one-api/types"
	"strings"
)

type ReplicateStreamHandler struct {
	Usage           *types.Usage
	ModelName       string
	ID              string
	framer          *requester.SSEEventFramer
	fetchPrediction func() *ReplicateResponse[[]string]
}

func (p *ReplicateProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (response *types.ChatCompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatCompletions)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url, request.Model)
	if fullRequestURL == "" {
		return nil, common.ErrorWrapper(nil, "invalid_recraft_config", http.StatusInternalServerError)
	}

	// 获取请求头
	headers := p.GetRequestHeaders()

	replicateRequest := convertFromChatOpenai(request)
	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(replicateRequest), p.Requester.WithHeader(headers))

	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}

	replicateResponse := &ReplicateResponse[[]string]{}

	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, replicateResponse, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	replicateResponse, err = getPrediction(p, replicateResponse)
	if err != nil {
		return nil, common.ErrorWrapper(err, "prediction_failed", http.StatusInternalServerError)
	}

	return p.convertToChatOpenai(replicateResponse)
}

func convertFromChatOpenai(request *types.ChatCompletionRequest) *ReplicateRequest[ReplicateChatRequest] {
	systemPrompt := ""
	prompt := ""

	for _, msg := range request.Messages {
		if msg.Role == "system" {
			systemPrompt += msg.StringContent() + "\n"
			continue
		}

		prompt += msg.Role + ": \n"
		openaiContent := msg.ParseContent()
		for _, content := range openaiContent {
			if content.Type != types.ContentTypeText {
				continue
			}
			prompt += content.Text
		}
		prompt += "\n"
	}

	prompt += "assistant: \n"

	return &ReplicateRequest[ReplicateChatRequest]{
		Stream: request.Stream,
		Input: ReplicateChatRequest{
			TopP:             request.TopP,
			MaxTokens:        request.MaxCompletionTokens,
			MinTokens:        0,
			Temperature:      request.Temperature,
			SystemPrompt:     systemPrompt,
			Prompt:           prompt,
			PresencePenalty:  request.PresencePenalty,
			FrequencyPenalty: request.FrequencyPenalty,
		},
	}
}

func (p *ReplicateProvider) convertToChatOpenai(response *ReplicateResponse[[]string]) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	responseText := ""
	if response.Output != nil {
		for _, text := range response.Output {
			responseText += text
		}
	}

	choice := types.ChatCompletionChoice{
		Index: 0,
		Message: types.ChatCompletionMessage{
			Role:    types.ChatMessageRoleAssistant,
			Content: responseText,
		},
		FinishReason: types.FinishReasonStop,
	}

	openaiResponse := &types.ChatCompletionResponse{
		ID:      response.ID,
		Object:  "chat.completion",
		Created: utils.GetTimestamp(),
		Choices: []types.ChatCompletionChoice{choice},
		Model:   response.Model,
		Usage: &types.Usage{
			CompletionTokens: 0,
			PromptTokens:     0,
			TotalTokens:      0,
		},
	}

	applyReplicateSucceededEvidence(p.Usage, response.Model, response.Metrics)
	openaiResponse.Usage = p.Usage

	return openaiResponse, nil
}

func (p *ReplicateProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatCompletions)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url, request.Model)
	if fullRequestURL == "" {
		return nil, common.ErrorWrapper(nil, "invalid_recraft_config", http.StatusInternalServerError)
	}

	// 获取请求头
	headers := p.GetRequestHeaders()

	replicateRequest := convertFromChatOpenai(request)
	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(replicateRequest), p.Requester.WithHeader(headers))

	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}

	replicateResponse := &ReplicateResponse[[]string]{}

	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, replicateResponse, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	headers["Accept"] = "text/event-stream"
	req, err = p.Requester.NewRequest(http.MethodGet, replicateResponse.Urls.Stream, p.Requester.WithHeader(headers))

	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}

	// 只有打开 SSE 的 GET 使用长流策略；创建 POST 和终态轮询保持原策略。
	streamRequester := p.Requester.ForHTTPProfile(requester.HTTPProfileLongStream)
	resp, errWithCode := streamRequester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	chatHandler := ReplicateStreamHandler{
		Usage:     p.Usage,
		ModelName: request.Model,
		ID:        replicateResponse.ID,
		framer:    requester.NewSSEEventFramer(16 << 20),
	}
	chatHandler.fetchPrediction = func() *ReplicateResponse[[]string] {
		return getPredictionResponse[[]string](p, replicateResponse.ID)
	}

	return requester.RequestNoTrimStreamWithEmitterOptions(streamRequester, resp, chatHandler.HandlerChatStreamWithEmitter, requester.StreamReadOptions{RequireProtocolTerminal: true})
}

func (h *ReplicateStreamHandler) HandlerChatStreamWithEmitter(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
	if h == nil || rawLine == nil {
		return
	}
	if h.framer == nil {
		h.framer = requester.NewSSEEventFramer(16 << 20)
	}
	event, complete, err := h.framer.PushLine(*rawLine)
	*rawLine = nil
	if err != nil {
		*rawLine = requester.StreamClosed
		emitter.SendError(err)
		return
	}
	if !complete {
		return
	}
	eventType, data := replicateSSEEvent(event)
	switch eventType {
	case "error":
		*rawLine = requester.StreamClosed
		apiErr := common.StringErrorWrapper(strings.TrimSpace(data), "prediction_failed", http.StatusBadGateway)
		if strings.TrimSpace(apiErr.Message) == "" {
			apiErr.Message = "prediction failed"
		}
		apiErr.UpstreamAccepted = true
		emitter.SendError(apiErr)
		return
	case "done":
		var prediction *ReplicateResponse[[]string]
		if h.fetchPrediction != nil {
			prediction = h.fetchPrediction()
		}
		if prediction == nil {
			*rawLine = requester.StreamClosed
			apiErr := common.StringErrorWrapper("prediction terminal state is unavailable", "prediction_failed", http.StatusBadGateway)
			apiErr.UpstreamAccepted = true
			emitter.SendError(apiErr)
			return
		}
		if _, terminalErr := replicateTerminalResult(prediction); terminalErr != nil {
			*rawLine = requester.StreamClosed
			apiErr := common.ErrorWrapper(terminalErr, "prediction_failed", http.StatusBadGateway)
			apiErr.UpstreamAccepted = true
			emitter.SendError(apiErr)
			return
		}
		applyReplicateSucceededEvidence(h.Usage, prediction.Model, prediction.Metrics)
		choice := types.ChatCompletionStreamChoice{Index: 0, Delta: types.ChatCompletionStreamChoiceDelta{Role: types.ChatMessageRoleAssistant}, FinishReason: types.FinishReasonStop}
		if !emitter.SendData(getStreamResponse(h.ID, choice, h.ModelName)) {
			return
		}
		*rawLine = requester.StreamClosed
		emitter.SendError(io.EOF)
		return
	default:
		if data == "" {
			return
		}
		choice := types.ChatCompletionStreamChoice{Index: 0, Delta: types.ChatCompletionStreamChoiceDelta{Role: types.ChatMessageRoleAssistant, Content: data}}
		emitter.SendData(getStreamResponse(h.ID, choice, h.ModelName))
	}
}

func applyReplicateSucceededEvidence(usage *types.Usage, modelName string, metrics ReplicateMetrics) {
	if usage == nil {
		return
	}
	usage.MarkProviderOperationUnits(1)
	usage.MergeProviderAttribution(modelName, "")
	if metrics.inputTokenCountPresent && metrics.outputTokenCountPresent {
		usage.PromptTokens = metrics.InputTokenCount
		usage.CompletionTokens = metrics.OutputTokenCount
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		usage.MarkProviderReported()
	}
}

func replicateSSEEvent(event []byte) (eventType, data string) {
	dataLines := make([]string, 0, 1)
	for _, line := range strings.Split(string(event), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		field, value := line, ""
		if colon := strings.IndexByte(line, ':'); colon >= 0 {
			field, value = line[:colon], line[colon+1:]
			if strings.HasPrefix(value, " ") {
				value = value[1:]
			}
		}
		switch field {
		case "event":
			// 事件名是控制字段；保留原有宽松的名称匹配语义。
			eventType = strings.ToLower(strings.TrimSpace(value))
		case "data":
			// SSE 只允许去掉冒号后的一个可选分隔空格。其余空格、tab
			// 和空 data 行都属于模型载荷，必须原样保留。
			dataLines = append(dataLines, value)
		}
	}
	return eventType, strings.Join(dataLines, "\n")
}

func getStreamResponse(id string, choice types.ChatCompletionStreamChoice, modelName string) string {
	chatCompletion := types.ChatCompletionStreamResponse{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: utils.GetTimestamp(),
		Model:   modelName,
		Choices: []types.ChatCompletionStreamChoice{choice},
	}

	responseBody, _ := json.Marshal(chatCompletion)

	return string(responseBody)
}
