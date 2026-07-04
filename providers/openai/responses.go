package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
	"one-api/common/utils"
	providersBase "one-api/providers/base"
	"one-api/types"
	"reflect"
	"strings"
)

type OpenAIResponsesStreamHandler struct {
	Usage     *types.Usage
	Prefix    string
	Model     string
	MessageID string

	searchType  string
	toolIndex   int
	hasToolCall bool
}

var (
	responsesDataPrefix        = []byte("data:")
	responsesDonePayload       = []byte("[DONE]")
	responsesRequestJSONFields = collectJSONFieldNames(reflect.TypeOf(types.OpenAIResponsesRequest{}))
)

func joinURLPath(basePath string, suffix string) string {
	basePath = strings.TrimRight(basePath, "/")
	suffix = strings.TrimLeft(suffix, "/")
	if suffix == "" {
		return basePath
	}
	return basePath + "/" + suffix
}

func (p *OpenAIProvider) CreateResponses(ctx context.Context, rawReq *commonresponses.Request) (openaiResponse *types.OpenAIResponsesResponses, errWithCode *types.OpenAIErrorWithStatusCode) {
	request := responsesRequestProjection(rawReq)
	httpReq, errWithCode := p.buildResponsesCreateRequest(rawReq, request, false)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if ctx != nil {
		httpReq = p.Requester.WithRequestContext(httpReq, ctx)
	}
	defer httpReq.Body.Close()

	response := &types.OpenAIResponsesResponses{}
	// 发送请求
	_, errWithCode = p.Requester.SendRequest(httpReq, response, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	if response.Usage == nil || response.Usage.OutputTokens == 0 {
		response.Usage = &types.ResponsesUsage{
			InputTokens:  p.Usage.PromptTokens,
			OutputTokens: 0,
			TotalTokens:  0,
		}
		// // 那么需要计算
		response.Usage.OutputTokens = common.CountTokenText(response.GetContent(), request.Model)
		response.Usage.TotalTokens = response.Usage.InputTokens + response.Usage.OutputTokens
	}

	*p.Usage = *response.Usage.ToOpenAIUsage()

	getResponsesExtraBilling(response, p.Usage)

	return response, nil
}

func (p *OpenAIProvider) CreateResponsesStream(ctx context.Context, rawReq *commonresponses.Request) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	request := responsesRequestProjection(rawReq)
	req, errWithCode := p.buildResponsesCreateRequest(rawReq, request, true)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if ctx != nil {
		req = p.Requester.WithRequestContext(req, ctx)
	}
	defer req.Body.Close()

	return p.createResponsesStreamFromRequest(req, request)
}

func (p *OpenAIProvider) CreateResponsesStreamRaw(ctx context.Context, model string, body map[string]json.RawMessage, request *types.OpenAIResponsesRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	fullRequestURL, errWithCode := p.responsesHTTPBridgeRequestURL(model)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if err := p.validateResponsesWSHTTPBridgeURL(ctx, fullRequestURL); err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "ws_request_failed", requester.UpstreamResponsesHTTPURLStatusCode(err))
	}
	headers := p.requestHeaders(openAIRequestAuthBearer)
	req, errWithCode := p.buildResponsesHTTPBridgeRequest(body, fullRequestURL, headers, model)
	if errWithCode != nil {
		return nil, markHTTPBridgePreSendLocalError(errWithCode)
	}
	if ctx != nil {
		req = p.Requester.WithRequestContext(req, ctx)
	}
	defer req.Body.Close()

	stream, errWithCode := p.createResponsesHTTPBridgeStreamFromRequestWithOptions(req, request, responsesHTTPBridgeStreamReadOptions())
	if errWithCode != nil {
		return nil, responsesws.MarkHTTPBridgeTransportError(errWithCode)
	}
	return stream, nil
}

func markHTTPBridgePreSendLocalError(errWithStatus *types.OpenAIErrorWithStatusCode) *types.OpenAIErrorWithStatusCode {
	if errWithStatus != nil {
		errWithStatus.LocalError = true
	}
	return errWithStatus
}

func (p *OpenAIProvider) responsesHTTPBridgeRequestURL(model string) (string, *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(config.RelayModeResponses)
	if errWithCode != nil {
		return "", errWithCode
	}
	if errWithCode := p.validateAzureClassicAPIVersionForRequest(); errWithCode != nil {
		return "", errWithCode
	}
	return p.GetFullRequestURL(url, model), nil
}

func responsesHTTPBridgeStreamReadOptions() requester.StreamReadOptions {
	return requester.StreamReadOptions{MaxLineBytes: config.RealtimeWebsocketReadLimit()}
}

func (p *OpenAIProvider) buildResponsesHTTPBridgeRequest(body map[string]json.RawMessage, fullRequestURL string, headers map[string]string, model string) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	requestMap, err := rawMessageBodyToInterfaceMap(body)
	if err != nil {
		return nil, common.ErrorWrapper(err, "decode_request_failed", http.StatusInternalServerError)
	}
	if requestMap == nil {
		requestMap = make(map[string]interface{})
	}
	customParams, err := p.CustomParameterHandler()
	if err != nil {
		return nil, common.ErrorWrapper(err, "custom_parameter_error", http.StatusInternalServerError)
	}
	if customParams != nil {
		requestMap = p.MergeCustomParams(requestMap, customParams, model)
	}
	if err := responsesws.NormalizeResponsesHTTPBridgeRequestMap(requestMap); err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "unsupported_responses_ws_bridge_field", http.StatusBadRequest)
	}
	requestBytes, err := json.Marshal(requestMap)
	if err != nil {
		return nil, common.ErrorWrapper(err, "marshal_request_failed", http.StatusInternalServerError)
	}
	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(requestBytes), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	return req, nil
}

func rawMessageBodyToInterfaceMap(body map[string]json.RawMessage) (map[string]interface{}, error) {
	if body == nil {
		return nil, nil
	}
	converted := make(map[string]interface{}, len(body))
	for key, value := range body {
		var decoded interface{}
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return nil, err
		}
		converted[key] = decoded
	}
	return converted, nil
}

func (p *OpenAIProvider) createResponsesStreamFromRequest(req *http.Request, request *types.OpenAIResponsesRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return p.createResponsesStreamFromRequestWithOptions(req, request, requester.StreamReadOptions{})
}

func (p *OpenAIProvider) createResponsesStreamFromRequestWithOptions(req *http.Request, request *types.OpenAIResponsesRequest, options requester.StreamReadOptions) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	// 发送请求
	resp, errWithCode := p.Requester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	chatHandler := OpenAIResponsesStreamHandler{
		Usage:  p.Usage,
		Prefix: `data: `,
		Model:  request.Model,
	}

	if request.ConvertChat {
		return requester.RequestStreamWithOptions(p.Requester, resp, chatHandler.HandlerChatStream, options)
	}

	return requester.RequestNoTrimStreamWithEmitterOptions(p.Requester, resp, chatHandler.HandlerResponsesStreamWithEmitter, options)
}

func (p *OpenAIProvider) createResponsesHTTPBridgeStreamFromRequestWithOptions(req *http.Request, request *types.OpenAIResponsesRequest, options requester.StreamReadOptions) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	resp, errWithCode := p.Requester.SendResponsesHTTPBridgeRaw(req, p.responsesHTTPBridgeSecurity())
	if errWithCode != nil {
		return nil, errWithCode
	}

	chatHandler := OpenAIResponsesStreamHandler{
		Usage:  p.Usage,
		Prefix: `data: `,
		Model:  request.Model,
	}

	if request.ConvertChat {
		return requester.RequestStreamWithOptions(p.Requester, resp, chatHandler.HandlerChatStream, options)
	}

	return requester.RequestNoTrimStreamWithEmitterOptions(p.Requester, resp, chatHandler.HandlerResponsesStreamWithEmitter, options)
}

func (p *OpenAIProvider) CompactResponses(ctx context.Context, rawReq *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	request := responsesRequestProjection(rawReq)
	req, errWithCode := p.buildCompactResponsesRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if ctx != nil {
		req = p.Requester.WithRequestContext(req, ctx)
	}
	defer req.Body.Close()

	response := &types.OpenAIResponsesResponses{}
	_, errWithCode = p.Requester.SendRequest(req, response, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	if response.Usage == nil || response.Usage.OutputTokens == 0 {
		response.Usage = &types.ResponsesUsage{
			InputTokens:  p.Usage.PromptTokens,
			OutputTokens: 0,
			TotalTokens:  0,
		}
		response.Usage.OutputTokens = common.CountTokenText(response.GetContent(), request.Model)
		response.Usage.TotalTokens = response.Usage.InputTokens + response.Usage.OutputTokens
	}

	*p.Usage = *response.Usage.ToOpenAIUsage()
	getResponsesExtraBilling(response, p.Usage)

	return response, nil
}

func responsesRequestProjection(req *commonresponses.Request) *types.OpenAIResponsesRequest {
	return commonresponses.ProjectRequest(req, nil)
}

func (p *OpenAIProvider) buildResponsesCreateRequest(rawReq *commonresponses.Request, request *types.OpenAIResponsesRequest, stream bool) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	if request == nil {
		request = &types.OpenAIResponsesRequest{}
	}
	basePath, errWithCode := p.GetSupportedAPIUri(config.RelayModeResponses)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if errWithCode := p.validateAzureClassicAPIVersionForRequest(); errWithCode != nil {
		return nil, errWithCode
	}

	fullRequestURL := p.GetFullRequestURL(basePath, request.Model)
	headers := p.GetRequestHeaders()

	bodyMap, errWithCode := p.buildResponsesCreateBody(rawReq, request, stream)
	if errWithCode != nil {
		return nil, errWithCode
	}

	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(bodyMap), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	return req, nil
}

func (p *OpenAIProvider) buildResponsesCreateBody(rawReq *commonresponses.Request, request *types.OpenAIResponsesRequest, stream bool) (map[string]interface{}, *types.OpenAIErrorWithStatusCode) {
	bodyMap, err := rawResponsesBodyMap(rawReq)
	if err != nil {
		return nil, common.ErrorWrapper(err, "decode_request_failed", http.StatusInternalServerError)
	}
	if bodyMap == nil {
		bodyMap = make(map[string]interface{})
	}

	if _, exists := bodyMap["prompt_cache_key"]; !exists && rawReq != nil && rawReq.Policy.PromptCache != nil {
		if key := strings.TrimSpace(rawReq.Policy.PromptCache.Key); key != "" {
			bodyMap["prompt_cache_key"] = key
		}
	}

	customParams, err := p.CustomParameterHandler()
	if err != nil {
		return nil, common.ErrorWrapper(err, "custom_parameter_error", http.StatusInternalServerError)
	}
	if customParams != nil {
		bodyMap = providersBase.ApplyCustomParams(bodyMap, customParams, request.Model, true)
	}
	if model := strings.TrimSpace(request.Model); model != "" {
		bodyMap["model"] = model
	}
	if stream {
		bodyMap["stream"] = true
	}
	return bodyMap, nil
}

func rawResponsesBodyMap(req *commonresponses.Request) (map[string]interface{}, error) {
	if req == nil || req.Body == nil || req.Body.Object == nil {
		return nil, nil
	}
	return rawMessageBodyToInterfaceMap(req.Body.Object.Fields)
}

func (p *OpenAIProvider) buildCompactResponsesRequest(request *types.OpenAIResponsesRequest) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	basePath, errWithCode := p.GetSupportedAPIUri(config.RelayModeResponses)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if errWithCode := p.validateAzureClassicAPIVersionForRequest(); errWithCode != nil {
		return nil, errWithCode
	}

	fullRequestURL := p.GetFullRequestURL(joinURLPath(basePath, "compact"), request.Model)
	headers := p.GetRequestHeaders()

	bodyMap, errWithCode := p.buildCompactRequestBody(request)
	if errWithCode != nil {
		return nil, errWithCode
	}

	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(bodyMap), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}

	return req, nil
}

func (p *OpenAIProvider) buildCompactRequestBody(request *types.OpenAIResponsesRequest) (map[string]interface{}, *types.OpenAIErrorWithStatusCode) {
	// Trade-off: `/responses/compact` has a materially narrower structured
	// request schema than ordinary `/responses`. Start from the documented
	// compact-safe fields only, then reattach unknown extra-body fields and let
	// `custom_parameter` override last. This keeps normal typed request fields
	// like `store`/`include` from leaking into compact while preserving operator
	// passthrough semantics for intentionally-added custom keys.
	bodyMap := make(map[string]interface{}, 6)
	bodyMap["model"] = request.Model
	if request.Input != nil {
		bodyMap["input"] = request.Input
	}
	if request.Instructions != "" {
		bodyMap["instructions"] = request.Instructions
	}
	if request.PreviousResponseID != "" {
		bodyMap["previous_response_id"] = request.PreviousResponseID
	}
	if request.PromptCacheKey != "" {
		bodyMap["prompt_cache_key"] = request.PromptCacheKey
	}
	if request.PromptCacheRetention != "" {
		bodyMap["prompt_cache_retention"] = request.PromptCacheRetention
	}

	if p.Channel.AllowExtraBody {
		rawMap, ok, err := p.GetRawBodyMap()
		if err != nil {
			return nil, common.ErrorWrapper(err, "unmarshal_request_failed", http.StatusInternalServerError)
		}
		if ok && rawMap != nil {
			for key, value := range rawMap {
				if responsesRequestJSONFields[key] {
					continue
				}
				bodyMap[key] = value
			}
		}
	}

	customParams, err := p.CustomParameterHandler()
	if err != nil {
		return nil, common.ErrorWrapper(err, "custom_parameter_error", http.StatusInternalServerError)
	}
	if customParams != nil {
		bodyMap = providersBase.ApplyCustomParams(bodyMap, customParams, request.Model, true)
	}

	return bodyMap, nil
}

func collectJSONFieldNames(t reflect.Type) map[string]bool {
	fields := make(map[string]bool)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return fields
	}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}

		tag := strings.TrimSpace(field.Tag.Get("json"))
		name := field.Name
		if tag != "" {
			parts := strings.Split(tag, ",")
			switch parts[0] {
			case "-":
				continue
			case "":
			default:
				name = parts[0]
			}
		}

		if field.Anonymous && (tag == "" || tag == ",omitempty") {
			for nested := range collectJSONFieldNames(field.Type) {
				fields[nested] = true
			}
			continue
		}

		fields[name] = true
	}

	return fields
}

func (h *OpenAIResponsesStreamHandler) HandlerResponsesStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	h.handleResponsesStream(rawLine, func(data string) bool {
		dataChan <- data
		return true
	})
}

func (h *OpenAIResponsesStreamHandler) HandlerResponsesStreamWithEmitter(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
	h.handleResponsesStream(rawLine, emitter.SendData)
}

func (h *OpenAIResponsesStreamHandler) handleResponsesStream(rawLine *[]byte, sendData func(string) bool) {
	rawStr := string(*rawLine)

	// 如果rawLine 前缀不为data:，则直接返回
	if !strings.HasPrefix(rawStr, h.Prefix) {
		sendData(rawStr)
		return
	}

	noSpaceLine := bytes.TrimSpace(*rawLine)
	if !bytes.HasPrefix(noSpaceLine, responsesDataPrefix) {
		sendData(rawStr)
		return
	}

	payload := bytes.TrimSpace(noSpaceLine[len(responsesDataPrefix):])

	if len(payload) == 0 || bytes.Equal(payload, responsesDonePayload) {
		sendData(rawStr)
		return
	}

	openaiResponse, ok := commonresponses.ParseStreamUsageEvent(payload)
	if !ok {
		// Usage tracking should not break stream passthrough.
		sendData(rawStr)
		return
	}

	switch openaiResponse.Type {
	case "response.created":
		if searchType := commonresponses.ResponsesSearchType(openaiResponse.Response); searchType != "" {
			h.searchType = searchType
		}
	case "response.output_text.delta", "response.reasoning_summary_text.delta":
		if h.Usage != nil {
			delta, ok := commonresponses.StreamEventDeltaString(openaiResponse.Delta)
			if ok {
				h.Usage.TextBuilder.WriteString(delta)
			}
		}
	case "response.output_item.added":
		commonresponses.ApplyResponsesOutputItemBilling(h.Usage, openaiResponse.Item, h.searchType)
	default:
		commonresponses.ApplyResponsesUsage(h.Usage, openaiResponse.Response)
	}

	sendData(rawStr)
}

func openAIStreamDeltaString(delta any) (string, bool) {
	text, ok := delta.(string)
	return text, ok
}

func (h *OpenAIResponsesStreamHandler) HandlerChatStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	// 如果rawLine 前缀不为data:，则直接返回
	if !strings.HasPrefix(string(*rawLine), h.Prefix) {
		*rawLine = nil
		return
	}

	// 去除前缀
	*rawLine = (*rawLine)[6:]

	var openaiResponse types.OpenAIResponsesStreamResponses
	err := json.Unmarshal(*rawLine, &openaiResponse)
	if err != nil {
		errChan <- common.ErrorToOpenAIError(err)
		return
	}

	chatRes := types.ChatCompletionStreamResponse{
		ID:      h.MessageID,
		Object:  "chat.completion.chunk",
		Created: utils.GetTimestamp(),
		Model:   h.Model,
		Choices: make([]types.ChatCompletionStreamChoice, 0),
	}
	needOutput := false

	switch openaiResponse.Type {
	case "response.created":
		h.hasToolCall = false
		h.toolIndex = 0
		if openaiResponse.Response != nil {
			if h.MessageID == "" {
				h.MessageID = openaiResponse.Response.ID
				chatRes.ID = h.MessageID
			}
			if searchType := commonresponses.ResponsesSearchType(openaiResponse.Response); searchType != "" {
				h.searchType = searchType
			}
		}
		chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
			Index: 0,
			Delta: types.ChatCompletionStreamChoiceDelta{},
		})
		needOutput = true
	case "response.output_text.delta": // 处理文本输出的增量
		delta, ok := openAIStreamDeltaString(openaiResponse.Delta)
		if ok && h.Usage != nil {
			h.Usage.TextBuilder.WriteString(delta)
		}
		chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
			Index: 0,
			Delta: types.ChatCompletionStreamChoiceDelta{
				Content: delta,
			},
		})
		needOutput = true
	case "response.reasoning_summary_text.delta": // 处理文本输出的增量
		delta, ok := openAIStreamDeltaString(openaiResponse.Delta)
		if ok && h.Usage != nil {
			h.Usage.TextBuilder.WriteString(delta)
		}
		chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
			Index: 0,
			Delta: types.ChatCompletionStreamChoiceDelta{
				ReasoningContent: delta,
			},
		})
		needOutput = true
	case "response.function_call_arguments.delta": // 处理函数调用参数的增量
		h.hasToolCall = true
		delta, ok := openAIStreamDeltaString(openaiResponse.Delta)
		if ok && h.Usage != nil {
			h.Usage.TextBuilder.WriteString(delta)
		}
		chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
			Index: 0,
			Delta: types.ChatCompletionStreamChoiceDelta{
				Role: types.ChatMessageRoleAssistant,
				ToolCalls: []*types.ChatCompletionToolCalls{
					{
						Index: h.toolIndex,
						Function: &types.ChatCompletionToolCallsFunction{
							Arguments: delta,
						},
					},
				},
			},
		})
		needOutput = true
	case "response.function_call_arguments.done":
		h.hasToolCall = true
		h.toolIndex++
	case "response.output_item.added":
		if openaiResponse.Item != nil {
			commonresponses.ApplyResponsesOutputItemBilling(h.Usage, openaiResponse.Item, h.searchType)
			switch openaiResponse.Item.Type {
			case types.InputTypeMessage, types.InputTypeReasoning:
				chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
					Index: 0,
					Delta: types.ChatCompletionStreamChoiceDelta{
						Role:    types.ChatMessageRoleAssistant,
						Content: "",
					},
				})
				needOutput = true
			case types.InputTypeFunctionCall:
				h.hasToolCall = true
				arguments := ""
				if openaiResponse.Item.Arguments != nil {
					arguments = *openaiResponse.Item.Arguments
				}

				chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
					Index: 0,
					Delta: types.ChatCompletionStreamChoiceDelta{
						Role: types.ChatMessageRoleAssistant,
						ToolCalls: []*types.ChatCompletionToolCalls{
							{
								Index: h.toolIndex,
								Id:    openaiResponse.Item.CallID,
								Type:  "function",
								Function: &types.ChatCompletionToolCallsFunction{
									Name:      openaiResponse.Item.Name,
									Arguments: arguments,
								},
							},
						},
					},
				})
				needOutput = true
			}
		}
	case "response.output_item.done":
		if openaiResponse.Item != nil && openaiResponse.Item.Type == types.InputTypeFunctionCall {
			h.hasToolCall = true
		}
	default:
		if openaiResponse.Response != nil && openaiResponse.Response.Usage != nil {
			commonresponses.ApplyResponsesUsage(h.Usage, openaiResponse.Response)
			finishReason := types.ConvertResponsesStatusToChat(openaiResponse.Response.Status)
			if finishReason == types.FinishReasonStop && shouldUseToolCallsFinishReason(openaiResponse.Response, h.hasToolCall) {
				finishReason = types.FinishReasonToolCalls
			}
			chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
				Index:        0,
				Delta:        types.ChatCompletionStreamChoiceDelta{},
				FinishReason: finishReason,
			})
			needOutput = true
		}
	}

	if needOutput {
		jsonData, err := json.Marshal(chatRes)
		if err != nil {
			errChan <- common.ErrorToOpenAIError(err)
			return
		}
		dataChan <- string(jsonData)

		return
	}

	*rawLine = nil
}

func shouldUseToolCallsFinishReason(response *types.OpenAIResponsesResponses, hasToolCall bool) bool {
	if hasToolCall {
		return true
	}

	if response == nil {
		return false
	}

	for _, output := range response.Output {
		if output.Type == types.InputTypeFunctionCall {
			return true
		}
	}

	return false
}

func getResponsesExtraBilling(response *types.OpenAIResponsesResponses, usage *types.Usage) {
	if usage == nil {
		return
	}
	usage.MergeExtraBilling(types.GetResponsesExtraBilling(response))
}
