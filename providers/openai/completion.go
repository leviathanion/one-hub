package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/providerresponse"
	"one-api/common/requester"
	"one-api/types"
)

func (p *OpenAIProvider) CreateCompletion(request *types.CompletionRequest) (openaiResponse *types.CompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	body, err := p.unaryCompletionBody(request)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "build_completion_request_failed", http.StatusInternalServerError)
	}
	req, errWithCode := p.GetRequestTextBody(config.RelayModeCompletions, request.Model, body)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	response := &OpenAIProviderCompletionResponse{}
	captureSameDialect := p.ProviderRawJSONReplay || p.usesNativeOpenAIWire()
	if captureSameDialect {
		response.EnableProviderRawJSONCapture()
	}
	// 发送请求
	var httpResponse *http.Response
	if p.ProviderRawJSONReplay {
		httpResponse, errWithCode = p.Requester.SendRequestPreservingRedirect(req, response, false)
	} else {
		httpResponse, errWithCode = p.Requester.SendRequest(req, response, false)
	}
	if errWithCode != nil {
		return nil, errWithCode
	}
	// Start with the safe header surface while the final response
	// representation is still undecided. The replay branch below recaptures the
	// provider headers after it has compared the actual bytes being delivered.
	p.captureProviderResponseHeaders(httpResponse, false)

	// 检测是否错误
	openaiErr := ErrorHandle(&response.OpenAIErrorResponse)
	if openaiErr != nil {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *openaiErr,
			StatusCode:  http.StatusBadRequest,
		}
		return nil, errWithCode
	}

	if response.Usage != nil {
		response.Usage.MarkProviderReported()
		response.Usage.MergeProviderAttribution(response.Model, "")
		*p.Usage = *response.Usage
	}
	bodyUnmodified := false
	if captureSameDialect {
		originalRaw := response.ProviderRawJSON()
		safeRaw := originalRaw
		response.SetProviderRawJSON(safeRaw)
		response.EnableProviderRawJSONReplay()
		bodyUnmodified = len(originalRaw) > 0 && bytes.Equal(originalRaw, safeRaw)
	}
	// Completion may redact provider metadata before relay sees the replay body;
	// recapture the final representation decision at that first rewrite point.
	p.captureProviderResponseHeaders(httpResponse, bodyUnmodified)

	return &response.CompletionResponse, nil
}

// 非流式 Completion 无需解释 stream_options。typed 路径仍按 DTO 选择
// 顶层字段，但这个已知字段使用原始 JSON，供上游判断其取值和组合。
func (p *OpenAIProvider) unaryCompletionBody(request *types.CompletionRequest) (any, error) {
	if p.usesNativeOpenAIWire() {
		return request, nil
	}
	raw, present, err := p.nativeRawBody()
	if err != nil || !present {
		return request, err
	}
	var envelope struct {
		StreamOptions json.RawMessage `json:"stream_options,case:strict"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.StreamOptions) == 0 {
		return request, nil
	}
	return struct {
		*types.CompletionRequest
		StreamOptions json.RawMessage `json:"stream_options"`
	}{request, envelope.StreamOptions}, nil
}

func (p *OpenAIProvider) CreateCompletionStream(request *types.CompletionRequest) (stream requester.StreamReaderInterface[string], errWithCode *types.OpenAIErrorWithStatusCode) {
	forceProviderUsage := shouldForceProviderStreamUsage(p, request.Stream)
	req, providerUsageEnabled, errWithCode := p.getRequestTextBody(config.RelayModeCompletions, request.Model, func() any {
		body := *request
		body.StreamOptions = p.streamOptionsForTypedBody(body.StreamOptions, forceProviderUsage)
		return &body
	}, forceProviderUsage)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// 发送请求
	streamRequester := p.Requester.ForHTTPProfile(requester.HTTPProfileLongStream)
	var resp *http.Response
	if p.ProviderRawJSONReplay {
		resp, errWithCode = streamRequester.SendRequestRawCheckedPreservingRedirect(req, providerresponse.OperationUnknown)
	} else {
		resp, errWithCode = streamRequester.SendRequestRaw(req)
	}
	if errWithCode != nil {
		return nil, errWithCode
	}
	p.captureProviderResponseHeaders(resp)

	clientUsage, clientUsageKnown := common.ClientStreamUsagePreference(p.Context, request.StreamOptions)
	chatHandler := OpenAIStreamHandler{
		Usage:               p.Usage,
		ModelName:           request.Model,
		ExposeProviderUsage: clientUsage || !clientUsageKnown,
	}
	options := requester.StreamReadOptions{
		RequireProtocolTerminal: p.RequireOpenAIStreamTerminal,
	}
	if p.ProviderRawJSONReplay || (p.usesNativeOpenAIWire() && !p.StreamEscapeJSON) {
		options.RequireProtocolTerminal = false
		chatHandler.usageFilter = p.rawStreamUsageFilter(providerUsageEnabled, chatHandler.ExposeProviderUsage)
		chatHandler.sseFramer = requester.NewSSEEventFramer(openAIExactSSEMaxEventBytes)
		return requester.RequestRawSSEEventStreamWithEmitterOptions(streamRequester, resp, chatHandler.handleExactCompletionSSE, options)
	}

	return requester.RequestStreamWithOptions[string](streamRequester, resp, chatHandler.handlerCompletionStream, options)
}

func (h *OpenAIStreamHandler) handlerCompletionStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	// 如果rawLine 前缀不为data:，则直接返回
	if !bytes.HasPrefix(*rawLine, []byte("data:")) {
		*rawLine = nil
		return
	}

	// 去除前缀
	*rawLine = bytes.TrimSpace((*rawLine)[5:])

	// 如果等于 DONE 则结束
	if string(*rawLine) == "[DONE]" {
		errChan <- io.EOF
		*rawLine = requester.StreamClosed
		return
	}

	var openaiResponse OpenAIProviderCompletionResponse
	err := json.Unmarshal(*rawLine, &openaiResponse)
	if err != nil {
		errChan <- common.ErrorToOpenAIError(err)
		return
	}

	aiError := ErrorHandle(&openaiResponse.OpenAIErrorResponse)
	emptyChoices := false
	if aiError == nil || openaiResponse.Usage != nil {
		emptyChoices = h.observeCompletionStreamResponse(&openaiResponse)
	}
	if aiError != nil {
		errChan <- aiError
		return
	}

	if emptyChoices && (openaiResponse.Usage == nil || !h.ExposeProviderUsage) {
		*rawLine = nil
		return
	}

	dataChan <- string(*rawLine)
}

func (h *OpenAIStreamHandler) handleExactCompletionSSE(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
	h.handleExactSSE(rawLine, emitter, h.observeExactCompletionStreamPayload, false)
}

func (h *OpenAIStreamHandler) observeCompletionStreamResponse(response *OpenAIProviderCompletionResponse) bool {
	if h == nil || response == nil || h.Usage == nil {
		return false
	}
	emptyChoices := len(response.Choices) == 0
	if emptyChoices && response.Usage == nil {
		return true
	}
	if response.Usage != nil {
		h.mergeCumulativeStreamUsage(response.Usage, response.Model, "")
	} else {
		h.Usage.MergeProviderAttribution(response.Model, "")
		if h.Usage.TotalTokens == 0 {
			h.Usage.TotalTokens = h.Usage.PromptTokens
		}
	}
	return emptyChoices
}

func (h *OpenAIStreamHandler) observeExactCompletionStreamPayload(frame *streamUsageFrame) bool {
	model := rawJSONString(frame.fields["model"])
	if frame.usage != nil {
		h.mergeCumulativeStreamUsage(frame.usage, model, "")
	} else if frame.providerError == nil && h.Usage != nil {
		h.Usage.MergeProviderAttribution(model, "")
	}
	return false
}
