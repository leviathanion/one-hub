package openai

import (
	"bytes"
	"encoding/json"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
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
	responsesDataPrefix  = []byte("data:")
	responsesDonePayload = []byte("[DONE]")
	trackedUsageEvents   = map[string]struct{}{
		"response.created":           {},
		"response.output_text.delta": {},
		"response.output_item.added": {},
		"response.completed":         {},
		"response.failed":            {},
		"response.incomplete":        {},
	}
	responsesRequestJSONFields = collectJSONFieldNames(reflect.TypeOf(types.OpenAIResponsesRequest{}))
)

type responsesUsageEvent struct {
	Type     string                          `json:"type"`
	Delta    json.RawMessage                 `json:"delta,omitempty"`
	Item     *types.ResponsesOutput          `json:"item,omitempty"`
	Response *types.OpenAIResponsesResponses `json:"response,omitempty"`
}

func shouldTrackResponsesUsageByType(eventType string) bool {
	_, ok := trackedUsageEvents[eventType]
	return ok
}

func (p *OpenAIProvider) buildResponsesOperationRequest(pathSuffix string, modelName string, request any) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	basePath, errWithCode := p.GetSupportedAPIUri(config.RelayModeResponses)
	if errWithCode != nil {
		return nil, errWithCode
	}

	fullPath := joinURLPath(basePath, pathSuffix)
	fullRequestURL := p.GetFullRequestURL(fullPath, modelName)
	headers := p.GetRequestHeaders()

	return p.BuildRequestWithMerge(request, fullRequestURL, headers, modelName)
}

func joinURLPath(basePath string, suffix string) string {
	basePath = strings.TrimRight(basePath, "/")
	suffix = strings.TrimLeft(suffix, "/")
	if suffix == "" {
		return basePath
	}
	return basePath + "/" + suffix
}

func (p *OpenAIProvider) CreateResponses(request *types.OpenAIResponsesRequest) (openaiResponse *types.OpenAIResponsesResponses, errWithCode *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.GetRequestTextBody(config.RelayModeResponses, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	response := &types.OpenAIResponsesResponses{}
	// 发送请求
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
		// // 那么需要计算
		response.Usage.OutputTokens = common.CountTokenText(response.GetContent(), request.Model)
		response.Usage.TotalTokens = response.Usage.InputTokens + response.Usage.OutputTokens
	}

	*p.Usage = *response.Usage.ToOpenAIUsage()

	getResponsesExtraBilling(response, p.Usage)

	return response, nil
}

func (p *OpenAIProvider) CreateResponsesStream(request *types.OpenAIResponsesRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.GetRequestTextBody(config.RelayModeResponses, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

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
		return requester.RequestStream(p.Requester, resp, chatHandler.HandlerChatStream)
	}

	return requester.RequestNoTrimStream(p.Requester, resp, chatHandler.HandlerResponsesStream)
}

func (p *OpenAIProvider) CompactResponses(request *types.OpenAIResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.buildCompactResponsesRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
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

func (p *OpenAIProvider) buildCompactResponsesRequest(request *types.OpenAIResponsesRequest) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	basePath, errWithCode := p.GetSupportedAPIUri(config.RelayModeResponses)
	if errWithCode != nil {
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
	rawStr := string(*rawLine)

	// 如果rawLine 前缀不为data:，则直接返回
	if !strings.HasPrefix(rawStr, h.Prefix) {
		dataChan <- rawStr
		return
	}

	noSpaceLine := bytes.TrimSpace(*rawLine)
	if !bytes.HasPrefix(noSpaceLine, responsesDataPrefix) {
		dataChan <- rawStr
		return
	}

	payload := bytes.TrimSpace(noSpaceLine[len(responsesDataPrefix):])

	if len(payload) == 0 || bytes.Equal(payload, responsesDonePayload) {
		dataChan <- rawStr
		return
	}

	var eventMeta struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &eventMeta); err != nil || eventMeta.Type == "" {
		dataChan <- rawStr
		return
	}

	if !shouldTrackResponsesUsageByType(eventMeta.Type) {
		dataChan <- rawStr
		return
	}

	var openaiResponse responsesUsageEvent
	if err := json.Unmarshal(payload, &openaiResponse); err != nil {
		// Usage tracking should not break stream passthrough.
		dataChan <- rawStr
		return
	}

	switch openaiResponse.Type {
	case "response.created":
		if openaiResponse.Response != nil && len(openaiResponse.Response.Tools) > 0 {
			for _, tool := range openaiResponse.Response.Tools {
				if types.IsResponsesWebSearchToolType(tool.Type) {
					h.searchType = "medium"
					if tool.SearchContextSize != "" {
						h.searchType = tool.SearchContextSize
					}
				}
			}
		}
	case "response.output_text.delta":
		var delta string
		if len(openaiResponse.Delta) > 0 && json.Unmarshal(openaiResponse.Delta, &delta) == nil {
			h.Usage.TextBuilder.WriteString(delta)
		}
	case "response.output_item.added":
		if openaiResponse.Item != nil {
			switch openaiResponse.Item.Type {
			case types.InputTypeWebSearchCall:
				if h.searchType == "" {
					h.searchType = "medium"
				}
				h.Usage.IncExtraBilling(types.APIToolTypeWebSearchPreview, h.searchType)
			case types.InputTypeCodeInterpreterCall:
				h.Usage.IncExtraBilling(types.APIToolTypeCodeInterpreter, "")
			case types.InputTypeFileSearchCall:
				h.Usage.IncExtraBilling(types.APIToolTypeFileSearch, "")
			}
		}
	default:
		if openaiResponse.Response != nil && openaiResponse.Response.Usage != nil {
			usage := openaiResponse.Response.Usage
			*h.Usage = *usage.ToOpenAIUsage()
			getResponsesExtraBilling(openaiResponse.Response, h.Usage)
		}
	}

	dataChan <- rawStr
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
		}
		if len(openaiResponse.Response.Tools) > 0 {
			for _, tool := range openaiResponse.Response.Tools {
				if types.IsResponsesWebSearchToolType(tool.Type) {
					h.searchType = "medium"
					if tool.SearchContextSize != "" {
						h.searchType = tool.SearchContextSize
					}
				}
			}
		}
		chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
			Index: 0,
			Delta: types.ChatCompletionStreamChoiceDelta{},
		})
		needOutput = true
	case "response.output_text.delta": // 处理文本输出的增量
		delta, ok := openaiResponse.Delta.(string)
		if ok {
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
		delta, ok := openaiResponse.Delta.(string)
		if ok {
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
		delta, ok := openaiResponse.Delta.(string)
		if ok {
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
			switch openaiResponse.Item.Type {
			case types.InputTypeWebSearchCall:
				if h.searchType == "" {
					h.searchType = "medium"
				}
				h.Usage.IncExtraBilling(types.APIToolTypeWebSearchPreview, h.searchType)
			case types.InputTypeCodeInterpreterCall:
				h.Usage.IncExtraBilling(types.APIToolTypeCodeInterpreter, "")
			case types.InputTypeFileSearchCall:
				h.Usage.IncExtraBilling(types.APIToolTypeFileSearch, "")

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
			usage := openaiResponse.Response.Usage
			*h.Usage = *usage.ToOpenAIUsage()

			getResponsesExtraBilling(openaiResponse.Response, h.Usage)
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
