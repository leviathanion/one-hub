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
	commonresponses "one-api/common/responses"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
)

const openAIExactSSEMaxEventBytes = 16 << 20

type OpenAIStreamHandler struct {
	Usage      *types.Usage
	ModelName  string
	isAzure    bool
	EscapeJSON bool

	ReasoningHandler bool
	UsageHandler     UsageHandler
	// ExposeProviderUsage records the downstream contract before the adapter
	// temporarily enables provider-side usage for settlement evidence.
	ExposeProviderUsage bool
	ProviderCredential  string
	sseFramer           *requester.SSEEventFramer
}

func (p *OpenAIProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (openaiResponse *types.ChatCompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	if err := p.validateChatRequestPolicy(request); err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "chat_search_billing_evidence_unavailable", http.StatusBadRequest)
	}
	if p.RequestHandleBefore != nil {
		errWithCode = p.RequestHandleBefore(request)
		if errWithCode != nil {
			return nil, errWithCode
		}
	}

	req, errWithCode := p.GetRequestTextBody(config.RelayModeChatCompletions, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	response := &OpenAIProviderChatResponse{}
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
		if p.UsageHandler != nil {
			p.UsageHandler(response.Usage)
		}
		response.Usage.MergeProviderAttribution(response.Model, response.ServiceTier)
		*p.Usage = *response.Usage
	}

	bodyUnmodified := false
	if p.ProviderRawJSONReplay {
		raw := response.ProviderRawJSON()
		response.EnableProviderRawJSONReplay()
		bodyUnmodified = len(raw) > 0 && bytes.Equal(raw, response.ReplayProviderRawJSON())
	} else if captureSameDialect {
		originalRaw := response.ProviderRawJSON()
		replay, replayErr := safeCompatibleChatResponseReplay(originalRaw, &response.ChatCompletionResponse, p.UsageHandler != nil)
		if replayErr != nil {
			return nil, common.ErrorWrapper(replayErr, "decode_response_failed", http.StatusInternalServerError)
		}
		response.SetProviderRawJSON(replay)
		response.EnableProviderRawJSONReplay()
		bodyUnmodified = len(originalRaw) > 0 && bytes.Equal(originalRaw, replay)
	}
	p.captureProviderResponseHeaders(httpResponse, bodyUnmodified)

	return &response.ChatCompletionResponse, nil
}

func safeCompatibleChatResponseReplay(raw []byte, response *types.ChatCompletionResponse, forceUsagePatch bool) ([]byte, error) {
	if len(raw) == 0 || response == nil {
		return nil, nil
	}
	safeRaw, _ := common.RedactProviderMetadataJSON(raw)
	var object map[string]json.RawMessage
	if err := json.Unmarshal(safeRaw, &object); err != nil {
		return nil, err
	}
	rawUsage, hasUsage := object["usage"]
	patchUsage := forceUsagePatch && hasUsage && !bytes.Equal(bytes.TrimSpace(rawUsage), []byte("null"))
	if !patchUsage {
		return safeRaw, nil
	}
	typed, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	var projection map[string]json.RawMessage
	if err := json.Unmarshal(typed, &projection); err != nil {
		return nil, err
	}
	if usage, ok := projection["usage"]; ok {
		if hasUsage && !bytes.Equal(bytes.TrimSpace(rawUsage), []byte("null")) {
			var rawUsageObject map[string]json.RawMessage
			var projectedUsageObject map[string]json.RawMessage
			if json.Unmarshal(rawUsage, &rawUsageObject) == nil && json.Unmarshal(usage, &projectedUsageObject) == nil {
				for key, value := range projectedUsageObject {
					rawUsageObject[key] = value
				}
				usage, err = json.Marshal(rawUsageObject)
				if err != nil {
					return nil, err
				}
			}
		}
		object["usage"] = usage
	}
	return json.Marshal(object)
}

func (p *OpenAIProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	if err := p.validateChatRequestPolicy(request); err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "chat_search_billing_evidence_unavailable", http.StatusBadRequest)
	}
	if p.RequestHandleBefore != nil {
		errWithCode := p.RequestHandleBefore(request)
		if errWithCode != nil {
			return nil, errWithCode
		}
	}

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
	req, errWithCode := p.getRequestTextBody(config.RelayModeChatCompletions, request.Model, request, shouldForceProviderStreamUsage(p, request.Stream))
	// Restore the downstream contract before any return; the temporary mutation
	// is only for non-exact adapters that need provider-side usage evidence.
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
		Usage:      p.Usage,
		ModelName:  request.Model,
		isAzure:    p.IsAzure,
		EscapeJSON: p.StreamEscapeJSON,

		UsageHandler:        p.UsageHandler,
		ExposeProviderUsage: streamOptions != nil && streamOptions.IncludeUsage,
		ProviderCredential:  p.Channel.Key,
	}
	options := requester.StreamReadOptions{
		RequireProtocolTerminal: p.RequireOpenAIStreamTerminal,
	}
	if p.ProviderRawJSONReplay {
		chatHandler.sseFramer = requester.NewSSEEventFramer(openAIExactSSEMaxEventBytes)
		return requester.RequestRawSSEEventStreamWithEmitterOptions(streamRequester, resp, chatHandler.HandleExactChatSSE, options)
	}

	return requester.RequestStreamWithOptions(streamRequester, resp, chatHandler.HandlerChatStream, options)
}

func (h *OpenAIStreamHandler) HandlerChatStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	// 如果rawLine 前缀不为data:，则直接返回
	if !strings.HasPrefix(string(*rawLine), "data:") {
		*rawLine = nil
		return
	}

	// 去除前缀
	*rawLine = (*rawLine)[5:]
	*rawLine = bytes.TrimSpace(*rawLine)
	if safe, changed := common.RedactCredentialValuesText(string(*rawLine), h.ProviderCredential); changed {
		*rawLine = []byte(safe)
	}

	// 如果等于 DONE 则结束
	if string(*rawLine) == "[DONE]" {
		errChan <- io.EOF
		*rawLine = requester.StreamClosed
		return
	}

	var openaiResponse OpenAIProviderChatStreamResponse
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

	usageOnly := h.observeChatStreamResponse(&openaiResponse)
	if usageOnly && !h.ExposeProviderUsage {
		*rawLine = nil
		return
	}

	if h.ReasoningHandler && len(openaiResponse.Choices) > 0 {
		for index, choices := range openaiResponse.Choices {
			if choices.Delta.ReasoningContent == "" && choices.Delta.Reasoning != "" {
				openaiResponse.Choices[index].Delta.ReasoningContent = choices.Delta.Reasoning
				openaiResponse.Choices[index].Delta.Reasoning = ""
			}
		}

		h.EscapeJSON = true
	}

	if h.EscapeJSON {
		if data, err := json.Marshal(openaiResponse.ChatCompletionStreamResponse); err == nil {
			dataChan <- string(data)
			return
		}
	}
	dataChan <- string(*rawLine)
}

// HandleExactChatSSE preserves each complete provider SSE event. JSON decoding
// is observation-only: future or malformed provider payloads do not replace a
// raw event that is otherwise valid at the SSE boundary.
func (h *OpenAIStreamHandler) HandleExactChatSSE(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
	h.handleExactSSE(rawLine, emitter, h.observeExactChatStreamPayload)
}

func (h *OpenAIStreamHandler) handleExactSSE(rawLine *[]byte, emitter requester.StreamEmitter[string], observe func([]byte) error) {
	if h == nil || rawLine == nil {
		return
	}
	if h.sseFramer == nil {
		h.sseFramer = requester.NewSSEEventFramer(openAIExactSSEMaxEventBytes)
	}
	event, complete, err := h.sseFramer.PushLine(*rawLine)
	*rawLine = nil
	if err != nil {
		*rawLine = requester.StreamClosed
		emitter.SendError(err)
		return
	}
	if !complete {
		return
	}

	safeEvent, _ := common.RedactCredentialValuesText(string(event), h.ProviderCredential)
	payload, hasData := commonresponses.SSEDataPayload(safeEvent)
	if hasData && strings.TrimSpace(payload) == "[DONE]" {
		if emitter.SendData(safeEvent) {
			emitter.SendError(io.EOF)
		}
		*rawLine = requester.StreamClosed
		return
	}

	var providerErr error
	if hasData && observe != nil {
		providerErr = observe([]byte(payload))
	}
	if !emitter.SendData(safeEvent) {
		return
	}
	if providerErr != nil {
		emitter.SendError(providerErr)
		*rawLine = requester.StreamClosed
	}
}

func (h *OpenAIStreamHandler) observeChatStreamResponse(response *OpenAIProviderChatStreamResponse) bool {
	if h == nil || response == nil || h.Usage == nil {
		return false
	}
	usageOnly := response.Usage != nil && len(response.Choices) == 0
	usage := response.Usage
	if usage == nil && len(response.Choices) > 0 {
		usage = response.Choices[0].Usage
	}
	if usage != nil {
		h.mergeChatStreamUsage(usage, response.Model, response.ServiceTier)
	} else if h.Usage.TotalTokens == 0 {
		h.Usage.TotalTokens = h.Usage.PromptTokens
	}
	if usage == nil {
		h.Usage.MergeProviderAttribution(response.Model, response.ServiceTier)
	}
	return usageOnly
}

func (h *OpenAIStreamHandler) observeExactChatStreamPayload(payload []byte) error {
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
	serviceTier := rawJSONString(fields["service_tier"])
	usage := decodeOpenAIStreamUsage(fields["usage"])
	if usage == nil {
		var choices []json.RawMessage
		if json.Unmarshal(fields["choices"], &choices) == nil && len(choices) > 0 {
			var choice map[string]json.RawMessage
			if json.Unmarshal(choices[0], &choice) == nil {
				usage = decodeOpenAIStreamUsage(choice["usage"])
			}
		}
	}
	if usage != nil {
		h.mergeChatStreamUsage(usage, model, serviceTier)
	} else if h.Usage != nil {
		h.Usage.MergeProviderAttribution(model, serviceTier)
	}
	return nil
}

func (h *OpenAIStreamHandler) mergeChatStreamUsage(usage *types.Usage, model, serviceTier string) {
	if h == nil || h.Usage == nil || usage == nil {
		return
	}
	previousModel := h.Usage.ResponseModel
	previousTier := h.Usage.ServiceTier
	previousConflict := h.Usage.AttributionConflict
	usage.MarkProviderReported()
	if h.UsageHandler != nil && h.UsageHandler(usage) {
		h.EscapeJSON = true
	}
	*h.Usage = *usage
	h.Usage.MergeProviderAttribution(previousModel, previousTier)
	h.Usage.AttributionConflict = h.Usage.AttributionConflict || previousConflict
	h.Usage.MergeProviderAttribution(model, serviceTier)
}

func decodeOpenAIStreamUsage(raw json.RawMessage) *types.Usage {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var usage types.Usage
	if json.Unmarshal(raw, &usage) != nil {
		return nil
	}
	return &usage
}
