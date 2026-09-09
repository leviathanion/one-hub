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
	runtimesession "one-api/runtime/session"
	"one-api/types"
)

func (p *OpenAIProvider) CreateCompletion(request *types.CompletionRequest) (openaiResponse *types.CompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.GetRequestTextBody(config.RelayModeCompletions, request.Model, request)
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
		safeRaw, _ := common.RedactProviderMetadataJSON(originalRaw)
		response.SetProviderRawJSON(safeRaw)
		response.EnableProviderRawJSONReplay()
		bodyUnmodified = len(originalRaw) > 0 && bytes.Equal(originalRaw, safeRaw)
	}
	// Completion may redact provider metadata before relay sees the replay body;
	// recapture the final representation decision at that first rewrite point.
	p.captureProviderResponseHeaders(httpResponse, bodyUnmodified)

	return &response.CompletionResponse, nil
}

func (p *OpenAIProvider) CreateCompletionStream(request *types.CompletionRequest) (stream requester.StreamReaderInterface[string], errWithCode *types.OpenAIErrorWithStatusCode) {
	streamOptions := request.StreamOptions
	// 如果支持流式返回Usage 则需要更改配置：
	if p.ProviderRawJSONReplay {
		// Exact-wire requests retain the client's stream_options verbatim.
	} else if shouldForceProviderStreamUsage(p, request.Stream) {
		if streamOptions == nil {
			request.StreamOptions = &types.StreamOptions{IncludeUsage: true}
		} else {
			streamOptionsCopy := *streamOptions
			streamOptionsCopy.IncludeUsage = true
			request.StreamOptions = &streamOptionsCopy
		}
	} else {
		// 避免误传导致报错
		request.StreamOptions = nil
	}
	req, errWithCode := p.getRequestTextBody(config.RelayModeCompletions, request.Model, request, shouldForceProviderStreamUsage(p, request.Stream))
	request.StreamOptions = streamOptions
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

	chatHandler := OpenAIStreamHandler{
		Usage:               p.Usage,
		ModelName:           request.Model,
		ExposeProviderUsage: streamOptions != nil && streamOptions.IncludeUsage,
		ProviderCredential:  p.Channel.Key,
	}
	options := requester.StreamReadOptions{
		RequireProtocolTerminal: p.RequireOpenAIStreamTerminal,
	}
	if p.ProviderRawJSONReplay {
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
	if safe, changed := common.RedactCredentialValuesText(string(*rawLine), h.ProviderCredential); changed {
		*rawLine = []byte(safe)
	}

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
	if aiError != nil {
		errChan <- aiError
		return
	}

	if h.observeCompletionStreamResponse(&openaiResponse) && (openaiResponse.Usage == nil || !h.ExposeProviderUsage) {
		*rawLine = nil
		return
	}

	dataChan <- string(*rawLine)
}

func (h *OpenAIStreamHandler) handleExactCompletionSSE(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
	h.handleExactSSE(rawLine, emitter, h.observeExactCompletionStreamPayload)
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
		h.mergeCompletionStreamUsage(response.Usage, response.Model)
	} else {
		h.Usage.MergeProviderAttribution(response.Model, "")
		if h.Usage.TotalTokens == 0 {
			h.Usage.TotalTokens = h.Usage.PromptTokens
		}
	}
	return emptyChoices
}

func (h *OpenAIStreamHandler) observeExactCompletionStreamPayload(payload []byte) error {
	if h == nil {
		return nil
	}
	if providerErr := runtimesession.OpenAIErrorEnvelopeFromPayload(payload); providerErr != nil {
		return providerErr
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return nil
	}
	model := rawJSONString(fields["model"])
	if usage := decodeOpenAIStreamUsage(fields["usage"]); usage != nil {
		h.mergeCompletionStreamUsage(usage, model)
	} else if h.Usage != nil {
		h.Usage.MergeProviderAttribution(model, "")
	}
	return nil
}

func (h *OpenAIStreamHandler) mergeCompletionStreamUsage(usage *types.Usage, model string) {
	if h == nil || h.Usage == nil || usage == nil {
		return
	}
	previousModel := h.Usage.ResponseModel
	previousConflict := h.Usage.AttributionConflict
	usage.MarkProviderReported()
	*h.Usage = *usage
	h.Usage.MergeProviderAttribution(previousModel, "")
	h.Usage.AttributionConflict = h.Usage.AttributionConflict || previousConflict
	h.Usage.MergeProviderAttribution(model, "")
}
