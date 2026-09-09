package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	providersBase "one-api/providers/base"
	"one-api/types"
	"strings"
)

type ClaudeRelayStreamHandler struct {
	Usage            *types.Usage
	ModelName        string
	Prefix           string
	AccumulatedUsage Usage

	AddEvent           bool
	framer             *requester.SSEEventFramer
	ErrorSeen          bool
	ProviderCredential string
}

func (p *ClaudeProvider) CreateClaudeChat(request *ClaudeRequest) (*ClaudeResponse, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.getNativeClaudeRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	claudeResponse := &ClaudeResponse{}
	claudeResponse.EnableProviderRawJSONCapture()
	// 发送请求
	httpResponse, errWithCode := p.Requester.SendRequestPreservingNativeDialect(req, claudeResponse, false)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if p.Context != nil && httpResponse != nil {
		p.Context.Set(requestctx.ProviderResponseHeadersContextKey, requestctx.SafeProviderResponseHeaders(httpResponse.Header))
		p.Context.Set(requestctx.ProviderResponseStatusContextKey, httpResponse.StatusCode)
	}

	usage := p.GetUsage()

	isOk := ClaudeUsageToOpenaiUsage(&claudeResponse.Usage, usage)
	if !isOk {
		usage.CompletionTokens = ClaudeOutputUsage(claudeResponse)
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	usage.MergeProviderAttribution(claudeResponse.Model, "")
	claudeResponse.EnableProviderRawJSONReplay()

	return claudeResponse, nil
}

func (p *ClaudeProvider) CreateClaudeChatStream(request *ClaudeRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.getNativeClaudeRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	chatHandler := &ClaudeRelayStreamHandler{
		Usage:              p.Usage,
		ModelName:          request.Model,
		Prefix:             `data: {"type"`,
		framer:             requester.NewSSEEventFramer(16 << 20),
		ProviderCredential: p.Channel.Key,
	}

	// 发送请求
	streamRequester := p.Requester.ForHTTPProfile(requester.HTTPProfileLongStream)
	resp, errWithCode := streamRequester.SendRequestRawCheckedNativeDialect(req, providerresponse.OperationUnknown)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if p.Context != nil && resp != nil {
		p.Context.Set(requestctx.ProviderResponseHeadersContextKey, requestctx.SafeProviderResponseHeaders(resp.Header))
		p.Context.Set(requestctx.ProviderResponseStatusContextKey, resp.StatusCode)
	}

	stream, errWithCode := requester.RequestNoTrimStreamWithEmitterOptions(streamRequester, resp, chatHandler.HandlerStreamWithEmitter, requester.StreamReadOptions{RequireProtocolTerminal: true})
	if errWithCode != nil {
		return nil, errWithCode
	}

	return stream, nil
}

func (h *ClaudeRelayStreamHandler) HandlerStreamWithEmitter(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
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
	payload := claudeSSEPayload(event)
	var claudeResponse ClaudeStreamResponse
	if json.Unmarshal(payload, &claudeResponse) != nil {
		// Observation failure cannot replace a valid same-dialect raw event.
		safe, _ := common.RedactCredentialValuesText(string(event), h.ProviderCredential)
		emitter.SendData(safe)
		return
	}
	switch claudeResponse.Type {
	case "message_start":
		h.Usage.MergeProviderAttribution("", claudeResponse.Message.Usage.ServiceTier)
		ClaudeUsageMerge(&h.AccumulatedUsage, &claudeResponse.Message.Usage)
		ClaudeUsageToOpenaiUsage(&h.AccumulatedUsage, h.Usage)
		h.Usage.MergeProviderAttribution(claudeResponse.Message.Model, "")
	case "message_delta":
		h.Usage.MergeProviderAttribution("", claudeResponse.Usage.ServiceTier)
		ClaudeUsageMerge(&h.AccumulatedUsage, &claudeResponse.Usage)
		ClaudeUsageToOpenaiUsage(&h.AccumulatedUsage, h.Usage)
	}
	safeEvent, _ := common.RedactCredentialValuesText(string(event), h.ProviderCredential)
	if !emitter.SendData(safeEvent) {
		return
	}
	if claudeResponse.Error != nil || claudeResponse.Type == "error" {
		h.ErrorSeen = true
		*rawLine = requester.StreamClosed
		if claudeResponse.Error != nil {
			emitter.SendError(claudeResponse.Error)
		} else {
			emitter.SendError(common.StringErrorWrapper("provider Claude stream failed", "upstream_error", http.StatusBadGateway))
		}
		return
	}
	if claudeResponse.Type == "message_stop" {
		*rawLine = requester.StreamClosed
		emitter.SendError(io.EOF)
	}
}

func claudeSSEPayload(event []byte) []byte {
	data := make([]string, 0, 1)
	for _, line := range strings.Split(string(event), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return []byte(strings.Join(data, "\n"))
}

func (p *ClaudeProvider) getNativeClaudeRequest(request *ClaudeRequest) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatCompletions)
	if errWithCode != nil {
		return nil, errWithCode
	}
	fullRequestURL := p.GetFullRequestURL(url)
	if fullRequestURL == "" {
		return nil, common.StringErrorWrapperLocal("invalid Claude provider URL", "invalid_claude_config", http.StatusInternalServerError)
	}
	headers := p.GetRequestHeaders()
	if request.Stream {
		headers["Accept"] = "text/event-stream"
	}
	body, exists := p.GetRawBody()
	if !exists {
		return nil, common.StringErrorWrapperLocal("request body not found", "request_body_not_found", http.StatusInternalServerError)
	}
	customParams, err := p.CustomParameterHandler()
	if err != nil {
		return nil, common.ErrorWrapper(err, "custom_parameter_error", http.StatusInternalServerError)
	}
	modelPatch := p.GetOriginalModel() != "" && p.GetOriginalModel() != request.Model
	requestBody := any(body)
	customPatch := false
	var bodyMap map[string]interface{}
	if modelPatch || len(customParams) > 0 {
		beforeMap, ok, err := p.GetRawBodyMap()
		if err != nil || !ok || beforeMap == nil {
			return nil, common.ErrorWrapperLocal(err, "decode_request_failed", http.StatusInternalServerError)
		}
		bodyMap = beforeMap
		if len(customParams) > 0 {
			afterMap, afterOK, afterErr := p.GetRawBodyMap()
			if afterErr != nil || !afterOK || afterMap == nil {
				return nil, common.ErrorWrapperLocal(afterErr, "decode_request_failed", http.StatusInternalServerError)
			}
			afterMap = providersBase.ApplyCustomParams(afterMap, customParams, request.Model, false)
			customPatch = !nativeClaudeJSONMapsEqual(beforeMap, afterMap)
		}
		if modelPatch {
			bodyMap["model"] = request.Model
		}
		if modelPatch || customPatch {
			requestBody = bodyMap
		}
	}
	var req *http.Request
	if customPatch {
		// Keep native requests opaque while reusing the shared custom-parameter
		// order and validation. ApplyCustomParams runs inside this builder once.
		req, errWithCode = p.BuildRequestWithMerge(requestBody, fullRequestURL, headers, request.Model)
		if errWithCode != nil {
			return nil, errWithCode
		}
	} else {
		req, err = p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(requestBody), p.Requester.WithHeader(headers))
	}
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	if p.Context != nil && p.Context.Request != nil {
		for _, value := range p.Context.Request.Header.Values("anthropic-beta") {
			if strings.ContainsAny(value, "\r\n") {
				return nil, common.StringErrorWrapperLocal("anthropic-beta contains an invalid value", "invalid_request_header", http.StatusBadRequest)
			}
			req.Header.Add("anthropic-beta", value)
		}
	}
	return req, nil
}

// nativeClaudeJSONMapsEqual compares the post-transform JSON without changing
// the original map's json.Number values.
func nativeClaudeJSONMapsEqual(left, right map[string]interface{}) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func (h *ClaudeRelayStreamHandler) HandlerStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	rawStr := string(*rawLine)
	// 如果rawLine 前缀不为data:，则直接返回
	if !strings.HasPrefix(rawStr, h.Prefix) {
		dataChan <- rawStr
		return
	}

	if h.AddEvent {
		rawStr = fmt.Sprintf("data: %s\n", rawStr)
	}

	noSpaceLine := bytes.TrimSpace(*rawLine)
	if strings.HasPrefix(string(noSpaceLine), "data: ") {
		// 去除前缀
		noSpaceLine = noSpaceLine[6:]
	}

	var claudeResponse ClaudeStreamResponse
	err := json.Unmarshal(noSpaceLine, &claudeResponse)
	if err != nil {
		errChan <- ErrorToClaudeErr(err)
		return
	}

	if claudeResponse.Error != nil {
		if h.AddEvent {
			event := "event: error\n"
			dataChan <- event
		}

		errChan <- claudeResponse.Error
		return
	}

	if h.AddEvent {
		event := fmt.Sprintf("event: %s\n", claudeResponse.Type)
		dataChan <- event
	}

	switch claudeResponse.Type {
	case "message_start":
		h.Usage.MergeProviderAttribution("", claudeResponse.Message.Usage.ServiceTier)
		ClaudeUsageMerge(&h.AccumulatedUsage, &claudeResponse.Message.Usage)
		ClaudeUsageToOpenaiUsage(&h.AccumulatedUsage, h.Usage)
	case "message_delta":
		h.Usage.MergeProviderAttribution("", claudeResponse.Usage.ServiceTier)
		ClaudeUsageMerge(&h.AccumulatedUsage, &claudeResponse.Usage)
		ClaudeUsageToOpenaiUsage(&h.AccumulatedUsage, h.Usage)
	}

	dataChan <- rawStr

	if h.AddEvent {
		event := "\n"
		dataChan <- event
	}
}
