package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/providerresponse"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
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

	sseFramer           *requester.SSEEventFramer
	sseStarted          bool
	sseLeadingBOM       bool
	sseSent             bool
	usageFilter         streamUsageFilter
	beforeChatDelivery  func([]byte) error
	formatProviderUsage bool
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
	commitChatResources, errWithCode := p.chatResourceRequestCommit(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	response := &OpenAIProviderChatResponse{}
	captureSameDialect := p.ProviderRawJSONReplay || p.usesNativeChatWire()
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

	formatUsage := false
	if response.Usage != nil {
		response.Usage.MarkProviderReported()
		if p.UsageHandler != nil {
			formatUsage = p.UsageHandler(response.Usage)
		}
		response.Usage.MergeProviderAttribution(response.Model, response.ServiceTier)
		*p.Usage = *response.Usage
	}

	// 错误与计费证据独立：同一响应中的 usage 仍属于本次上游执行。
	openaiErr := ErrorHandle(&response.OpenAIErrorResponse)
	if openaiErr != nil && !p.ProviderRawJSONReplay {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *openaiErr,
			StatusCode:  http.StatusBadRequest,
		}
		return nil, errWithCode
	}

	bodyUnmodified := false
	if p.ProviderRawJSONReplay {
		raw := response.ProviderRawJSON()
		response.EnableProviderRawJSONReplay()
		bodyUnmodified = len(raw) > 0 && bytes.Equal(raw, response.ReplayProviderRawJSON())
	} else if captureSameDialect {
		originalRaw := response.ProviderRawJSON()
		replay, replayErr := safeCompatibleChatResponseReplay(originalRaw, &response.ChatCompletionResponse, formatUsage)
		if replayErr != nil {
			// 局部用量投影失败不替换已经成功的上游响应；计费仍使用独立提取的证据。
			replay = originalRaw
		}
		response.SetProviderRawJSON(replay)
		response.EnableProviderRawJSONReplay()
		bodyUnmodified = len(originalRaw) > 0 && bytes.Equal(originalRaw, replay)
	}
	p.captureProviderResponseHeaders(httpResponse, bodyUnmodified)
	if commitChatResources != nil {
		if err := commitChatResources(response.ProviderRawJSON()); err != nil {
			var apiErr *types.OpenAIErrorWithStatusCode
			if !errors.As(err, &apiErr) {
				apiErr = common.ErrorWrapperLocal(err, "resource_owner_commit_failed", http.StatusServiceUnavailable)
				apiErr.UpstreamAccepted = true
			}
			return nil, apiErr
		}
	}

	return &response.ChatCompletionResponse, nil
}

func safeCompatibleChatResponseReplay(raw []byte, response *types.ChatCompletionResponse, forceUsagePatch bool) ([]byte, error) {
	if len(raw) == 0 || response == nil {
		return nil, nil
	}
	if !forceUsagePatch || response.Usage == nil {
		return raw, nil
	}
	return patchChatPayloadUsage(raw, response.Usage)
}

func (p *OpenAIProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return p.createChatCompletionStream(request, false)
}

func (p *OpenAIProvider) SupportsChatStreamConversion() bool {
	return p.usesNativeOpenAIWire() && !p.StreamEscapeJSON
}

func (p *OpenAIProvider) CreateChatCompletionStreamForConversion(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	if !p.SupportsChatStreamConversion() {
		return nil, common.StringErrorWrapperLocal("selected adapter does not implement native Chat stream conversion", "unsupported_capability", http.StatusBadRequest)
	}
	return p.createChatCompletionStream(request, true)
}

func (p *OpenAIProvider) createChatCompletionStream(request *types.ChatCompletionRequest, forConversion bool) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	if err := p.validateChatRequestPolicy(request); err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "chat_search_billing_evidence_unavailable", http.StatusBadRequest)
	}
	if p.RequestHandleBefore != nil {
		errWithCode := p.RequestHandleBefore(request)
		if errWithCode != nil {
			return nil, errWithCode
		}
	}

	forceProviderUsage := shouldForceProviderStreamUsage(p, request.Stream)
	req, providerUsageEnabled, errWithCode := p.getRequestTextBody(config.RelayModeChatCompletions, request.Model, func() any {
		body := *request
		body.StreamOptions = p.streamOptionsForTypedBody(body.StreamOptions, forceProviderUsage)
		return &body
	}, forceProviderUsage)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()
	commitChatResources, errWithCode := p.chatResourceRequestCommit(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

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
		Usage:      p.Usage,
		ModelName:  request.Model,
		isAzure:    p.IsAzure,
		EscapeJSON: p.StreamEscapeJSON,

		UsageHandler:        p.UsageHandler,
		ExposeProviderUsage: clientUsage || !clientUsageKnown,
		beforeChatDelivery:  commitChatResources,
	}
	options := requester.StreamReadOptions{
		RequireProtocolTerminal: p.RequireOpenAIStreamTerminal,
	}
	if p.ProviderRawJSONReplay || (p.usesNativeChatWire() && !p.StreamEscapeJSON) {
		chatHandler.formatProviderUsage = !p.ProviderRawJSONReplay
		chatHandler.usageFilter = p.rawStreamUsageFilter(providerUsageEnabled, chatHandler.ExposeProviderUsage)
		chatHandler.sseFramer = requester.NewSSEEventFramer(openAIExactSSEMaxEventBytes)
		if forConversion {
			return requester.RequestRawSSEEventStreamWithEmitterOptions(streamRequester, resp, chatHandler.HandleChatConversionSSE, options)
		}
		options.RequireProtocolTerminal = false
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

	usageOnly := h.observeChatStreamResponse(&openaiResponse)
	aiError := ErrorHandle(&openaiResponse.OpenAIErrorResponse)
	if aiError != nil {
		errChan <- aiError
		return
	}

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

// HandleExactChatSSE 保留上游事件边界，按客户端开关过滤可识别的 usage。
// JSON 投影失败的事件仍可按原文交付。
func (h *OpenAIStreamHandler) HandleExactChatSSE(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
	h.handleExactSSE(rawLine, emitter, h.observeExactChatStreamPayload, false)
}

// HandleChatConversionSSE is a source-protocol boundary for Chat→Responses.
// Its terminal closes the conversion input, not a native passthrough stream.
func (h *OpenAIStreamHandler) HandleChatConversionSSE(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
	h.handleExactSSE(rawLine, emitter, h.observeExactChatStreamPayload, true)
}

func (h *OpenAIStreamHandler) handleExactSSE(rawLine *[]byte, emitter requester.StreamEmitter[string], observe func(*streamUsageFrame) bool, forConversion bool) {
	if h == nil || rawLine == nil || len(*rawLine) == 0 {
		return
	}
	if h.sseFramer == nil {
		h.sseFramer = requester.NewSSEEventFramer(openAIExactSSEMaxEventBytes)
	}
	line := *rawLine
	if !h.sseStarted {
		h.sseStarted = true
		line, h.sseLeadingBOM = bytes.CutPrefix(line, []byte(requester.SSEUTF8BOM))
	}
	event, complete, err := h.sseFramer.PushLine(line)
	*rawLine = nil
	if err != nil {
		*rawLine = requester.StreamClosed
		emitter.SendError(err)
		return
	}
	if !complete {
		return
	}

	safeEvent := string(event)
	payload, hasData := commonresponses.SSEDataPayload(safeEvent)
	if hasData && h.beforeChatDelivery != nil {
		if err := h.beforeChatDelivery([]byte(payload)); err != nil {
			*rawLine = requester.StreamClosed
			emitter.SendError(err)
			return
		}
	}
	var conversionTerminal error
	if hasData && strings.TrimSpace(payload) == "[DONE]" {
		if forConversion {
			conversionTerminal = io.EOF
		}
	} else if hasData {
		filter := h.usageFilter
		if filter != keepStreamUsage && !isChatMessageEvent(safeEvent) {
			filter = keepStreamUsage
		}
		frame := readStreamUsageFrame([]byte(payload), filter)
		formatted := observe(&frame)
		if forConversion && frame.providerError != nil {
			conversionTerminal = frame.providerError
		}
		if frame.hide {
			var drop bool
			safeEvent, drop = frame.hideUsage(safeEvent)
			if drop {
				return
			}
		} else if formatted && h.formatProviderUsage {
			patched, err := patchChatPayloadUsage(frame.raw, frame.chatUsage())
			if err == nil && !bytes.Equal(patched, frame.raw) {
				safeEvent = rewriteChatSSEData(safeEvent, patched)
			}
		}
	}
	if !forConversion && !h.sseSent {
		h.sseSent = true
		// 保留流首 BOM。过滤把带 U+FEFF 的字段移到流首时，加一个前导标记以保留该字段。
		if h.sseLeadingBOM || strings.HasPrefix(safeEvent, requester.SSEUTF8BOM) {
			safeEvent = requester.SSEUTF8BOM + safeEvent
		}
	}
	if !emitter.SendData(safeEvent) {
		return
	}
	if conversionTerminal != nil {
		*rawLine = requester.StreamClosed
		emitter.SendError(conversionTerminal)
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
		h.mergeCumulativeStreamUsage(usage, response.Model, response.ServiceTier)
	} else if h.Usage.TotalTokens == 0 {
		h.Usage.TotalTokens = h.Usage.PromptTokens
	}
	if usage == nil {
		h.Usage.MergeProviderAttribution(response.Model, response.ServiceTier)
	}
	return usageOnly
}

func (h *OpenAIStreamHandler) observeExactChatStreamPayload(frame *streamUsageFrame) bool {
	model := rawJSONString(frame.fields["model"])
	tier := rawJSONString(frame.fields["service_tier"])
	if usage := frame.chatUsage(); usage != nil {
		return h.mergeCumulativeStreamUsage(usage, model, tier)
	}
	if h.Usage != nil {
		h.Usage.MergeProviderAttribution(model, tier)
	}
	return false
}

func (h *OpenAIStreamHandler) mergeCumulativeStreamUsage(usage *types.Usage, model, serviceTier string) bool {
	if h == nil || h.Usage == nil || usage == nil {
		return false
	}
	previousModel := h.Usage.ResponseModel
	previousTier := h.Usage.ServiceTier
	previousConflict := h.Usage.AttributionConflict
	usage.MarkProviderReported()
	formatted := h.UsageHandler != nil && h.UsageHandler(usage)
	if formatted {
		h.EscapeJSON = true
	}
	if h.Usage.HasProviderBaseUsage() && (!usage.ProviderTokenFields["prompt_tokens"] || !usage.ProviderTokenFields["completion_tokens"]) {
		// 累计 token 快照需要完整的基础字段，稀疏观察不覆盖旧快照，也不拼接计数。
		// 已由 provider 授权的独立用量仍按累计值更新。
		for key, billing := range usage.ExtraBilling {
			if usage.HasProviderExtraBilling(key) {
				h.Usage.SetProviderExtraBilling(types.ResolveExtraBillingServiceType(key, billing), types.ResolveExtraBillingType(key, billing), billing.CallCount)
			}
		}
		h.Usage.MergeBillingDiagnostics(usage.BillingDiagnostics)
		h.Usage.MergeProviderAttribution(model, serviceTier)
		return formatted
	}
	*h.Usage = *usage
	h.Usage.MergeProviderAttribution(previousModel, previousTier)
	h.Usage.AttributionConflict = h.Usage.AttributionConflict || previousConflict
	h.Usage.MergeProviderAttribution(model, serviceTier)
	return formatted
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
