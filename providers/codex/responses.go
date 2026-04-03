package codex

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/requester"
	"one-api/providers/openai"
	"one-api/types"
)

const codexReasoningEncryptedContentInclude = "reasoning.encrypted_content"

// CodexResponsesStreamHandler handles Codex Responses streaming.
type CodexResponsesStreamHandler struct {
	Usage       *types.Usage
	eventBuffer strings.Builder
	eventType   string
	accumulator *codexTurnUsageAccumulator
}

type codexResponsesUsageEvent struct {
	Type        string                          `json:"type"`
	Delta       json.RawMessage                 `json:"delta,omitempty"`
	Item        *types.ResponsesOutput          `json:"item,omitempty"`
	OutputIndex *int                            `json:"output_index,omitempty"`
	Response    *types.OpenAIResponsesResponses `json:"response,omitempty"`
}

func cloneCodexExtraBilling(extraBilling map[string]types.ExtraBilling) map[string]types.ExtraBilling {
	if len(extraBilling) == 0 {
		return nil
	}

	cloned := make(map[string]types.ExtraBilling, len(extraBilling))
	for key, value := range extraBilling {
		cloned[key] = value
	}
	return cloned
}

func safeCountCodexResponseTokens(content string, modelName string) (tokens int) {
	defer func() {
		if recover() != nil {
			tokens = 0
		}
	}()
	return common.CountTokenText(content, modelName)
}

func applyResolvedCodexUsage(target *types.Usage, resolved *types.Usage) {
	if target == nil || resolved == nil {
		return
	}

	existingText := ""
	if target.TextBuilder.Len() > 0 {
		existingText = target.TextBuilder.String()
	}

	*target = *resolved
	if existingText != "" {
		target.TextBuilder.WriteString(existingText)
	}
}

func resolveCodexResponsesUsage(seed *types.Usage, accumulator *codexTurnUsageAccumulator, response *types.OpenAIResponsesResponses, modelName string, allowContentFallback bool) *types.Usage {
	if response == nil {
		return nil
	}
	if accumulator == nil {
		accumulator = newCodexTurnUsageAccumulator()
	}
	accumulator.SeedFromUsage(seed)
	return accumulator.ResolveUsage(response, modelName, allowContentFallback)
}

func finalizeCodexResponsesUsage(usage *types.Usage, response *types.OpenAIResponsesResponses, modelName string, allowContentFallback bool) {
	resolved := resolveCodexResponsesUsage(usage, nil, response, modelName, allowContentFallback)
	if usage == nil || resolved == nil {
		return
	}
	applyResolvedCodexUsage(usage, resolved)
}

func codexResponsesSearchType(response *types.OpenAIResponsesResponses) string {
	if response == nil || len(response.Tools) == 0 {
		return ""
	}

	for _, tool := range response.Tools {
		if !types.IsResponsesWebSearchToolType(tool.Type) {
			continue
		}
		if searchType := strings.TrimSpace(tool.SearchContextSize); searchType != "" {
			return searchType
		}
		return "medium"
	}

	return ""
}

func applyCodexResponsesAddedToolBilling(usage *types.Usage, item *types.ResponsesOutput, searchType string) {
	if usage == nil || item == nil {
		return
	}

	switch item.Type {
	case types.InputTypeWebSearchCall:
		if searchType == "" {
			searchType = "medium"
		}
		usage.IncExtraBilling(types.APIToolTypeWebSearchPreview, searchType)
	case types.InputTypeCodeInterpreterCall:
		usage.IncExtraBilling(types.APIToolTypeCodeInterpreter, "")
	case types.InputTypeFileSearchCall:
		usage.IncExtraBilling(types.APIToolTypeFileSearch, "")
	case types.InputTypeImageGenerationCall:
		usage.IncExtraBilling(types.APIToolTypeImageGeneration, item.Quality+"-"+item.Size)
	}
}

func codexResponsesUsageHandlerEventType(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.created", "response.output_text.delta", "response.output_item.added", "response.completed", "response.failed", "response.incomplete", "response.done":
		return true
	default:
		return false
	}
}

func (h *CodexResponsesStreamHandler) observeUsageEvent(dataLine string) {
	if h == nil {
		return
	}

	var event codexResponsesUsageEvent
	if err := json.Unmarshal([]byte(dataLine), &event); err != nil || !codexResponsesUsageHandlerEventType(event.Type) {
		return
	}

	if h.accumulator != nil {
		h.accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
			Type:        event.Type,
			Delta:       event.Delta,
			Item:        event.Item,
			OutputIndex: event.OutputIndex,
			Response:    event.Response,
		})
	}

	switch event.Type {
	case "response.output_text.delta":
		if h.Usage != nil {
			var delta string
			if len(event.Delta) > 0 && json.Unmarshal(event.Delta, &delta) == nil {
				h.Usage.TextBuilder.WriteString(delta)
			}
		}
	case "response.output_item.added":
		if h.Usage != nil {
			searchType := ""
			if h.accumulator != nil {
				searchType = h.accumulator.searchType
			}
			applyCodexResponsesAddedToolBilling(h.Usage, event.Item, searchType)
		}
	case "response.completed", "response.failed", "response.incomplete", "response.done":
		if resolved := resolveCodexResponsesUsage(h.Usage, h.accumulator, event.Response, "", false); resolved != nil {
			applyResolvedCodexUsage(h.Usage, resolved)
		}
	}
}

func newCodexResponsesStreamHandler(usage *types.Usage) *CodexResponsesStreamHandler {
	accumulator := newCodexTurnUsageAccumulator()
	accumulator.SeedFromUsage(usage)
	return &CodexResponsesStreamHandler{
		Usage:       usage,
		accumulator: accumulator,
	}
}

// CreateResponses builds a non-streamed response via streaming.
func (p *CodexProvider) CreateResponses(request *types.OpenAIResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	// Apply Codex-specific settings.
	p.prepareCodexRequest(request)

	// Codex requires streaming.
	request.Stream = true

	// Build request.
	req, errWithCode := p.getResponsesRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// Send streaming request.
	resp, errWithCode := p.Requester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// Create stream handler.
	handler := newCodexResponsesStreamHandler(p.Usage)

	// Get stream response.
	stream, errWithCode := requester.RequestNoTrimStream(p.Requester, resp, handler.HandlerResponsesStream)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// Aggregate full response.
	response, errWithCode := p.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		return nil, errWithCode
	}

	if p.Usage == nil {
		p.Usage = &types.Usage{}
	}
	if resolved := resolveCodexResponsesUsage(p.Usage, handler.accumulator, response, request.Model, true); resolved != nil {
		applyResolvedCodexUsage(p.Usage, resolved)
	}
	backfillCodexResponsePromptCacheKey(response, request)
	return response, nil
}

// CreateResponsesStream streams Responses.
func (p *CodexProvider) CreateResponsesStream(request *types.OpenAIResponsesRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	// Apply Codex-specific settings.
	p.prepareCodexRequest(request)

	// Force stream (Codex requirement).
	request.Stream = true

	// Build request.
	req, errWithCode := p.getResponsesRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// Send request.
	resp, errWithCode := p.Requester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// Create stream handler.
	handler := newCodexResponsesStreamHandler(p.Usage)

	// Convert Responses SSE to ChatCompletion stream when requested.
	if request.ConvertChat {
		chatHandler := openai.OpenAIResponsesStreamHandler{
			Usage:  &types.Usage{},
			Prefix: "data: ",
			Model:  request.Model,
		}

		bridgeHandler := func(rawLine *[]byte, dataChan chan string, errChan chan error) {
			if rawLine == nil || len(*rawLine) == 0 {
				return
			}

			rawStr := strings.TrimSpace(string(*rawLine))
			if !strings.HasPrefix(rawStr, "data:") {
				return
			}

			// Normalize "data:{...}" and "data: {...}" to the expected "data: {...}".
			dataLine := strings.TrimSpace(strings.TrimPrefix(rawStr, "data:"))
			if dataLine == "" || dataLine == "[DONE]" {
				return
			}
			handler.observeUsageEvent(dataLine)

			normalized := []byte("data: " + dataLine)
			chatHandler.HandlerChatStream(&normalized, dataChan, errChan)
		}

		return requester.RequestStream(p.Requester, resp, bridgeHandler)
	}

	// Use RequestNoTrimStream to preserve event lines.
	return requester.RequestNoTrimStream(p.Requester, resp, handler.HandlerResponsesStream)
}

func (p *CodexProvider) CompactResponses(request *types.OpenAIResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	p.prepareCodexRequest(request)
	request.Stream = false

	req, errWithCode := p.getResponsesOperationRequest(request, "compact")
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	response := &types.OpenAIResponsesResponses{}
	_, errWithCode = p.Requester.SendRequest(req, response, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	if p.Usage == nil {
		p.Usage = &types.Usage{}
	}

	finalizeCodexResponsesUsage(p.Usage, response, request.Model, false)
	backfillCodexResponsePromptCacheKey(response, request)
	return response, nil
}

// prepareCodexRequest prepares Codex request fields.
func (p *CodexProvider) prepareCodexRequest(request *types.OpenAIResponsesRequest) {
	request.Model = normalizeCodexModelName(request.Model)

	// Codex requires store=false.
	storeFalse := false
	request.Store = &storeFalse

	// Prefer temperature over top_p when both set.
	if request.Temperature != nil && request.TopP != nil {
		request.TopP = nil
	}

	// Codex upstream currently rejects OpenAI context-management and truncation controls.
	request.ContextManagement = nil
	request.Truncation = ""

	ensureStablePromptCacheKey(request, p.Context, p.getPromptCacheKeyStrategy())
	ensureCodexIncludes(request)
	normalizeCodexBuiltinTools(request)

	// Adapt to Codex CLI format.
	p.adaptCodexCLI(request)
}

func ensureCodexIncludes(request *types.OpenAIResponsesRequest) {
	if request == nil {
		return
	}

	switch include := request.Include.(type) {
	case nil:
		request.Include = []string{codexReasoningEncryptedContentInclude}
	case []string:
		request.Include = appendUniqueStrings(include, codexReasoningEncryptedContentInclude)
	case []any:
		values := make([]string, 0, len(include)+1)
		for _, item := range include {
			if str, ok := item.(string); ok && strings.TrimSpace(str) != "" {
				values = append(values, str)
			}
		}
		request.Include = appendUniqueStrings(values, codexReasoningEncryptedContentInclude)
	case string:
		request.Include = appendUniqueStrings([]string{include}, codexReasoningEncryptedContentInclude)
	default:
		raw, err := json.Marshal(include)
		if err != nil {
			request.Include = []string{codexReasoningEncryptedContentInclude}
			return
		}

		var values []string
		if err := json.Unmarshal(raw, &values); err != nil {
			request.Include = []string{codexReasoningEncryptedContentInclude}
			return
		}
		request.Include = appendUniqueStrings(values, codexReasoningEncryptedContentInclude)
	}
}

func appendUniqueStrings(items []string, extra string) []string {
	result := make([]string, 0, len(items)+1)
	hasExtra := false
	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		if trimmed == extra {
			hasExtra = true
		}
		result = append(result, trimmed)
	}
	if !hasExtra {
		result = append(result, extra)
	}
	return result
}

func normalizeCodexBuiltinTools(request *types.OpenAIResponsesRequest) {
	if request == nil {
		return
	}

	for i := range request.Tools {
		request.Tools[i].Type = normalizeCodexBuiltinToolType(request.Tools[i].Type)
	}

	if request.ToolChoice == nil {
		return
	}

	normalized, ok := normalizeCodexToolChoiceValue(request.ToolChoice)
	if ok {
		request.ToolChoice = normalized
	}
}

func normalizeCodexToolChoiceValue(value any) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return normalizeCodexToolChoiceMap(typed), true
	case []any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			if normalized, ok := normalizeCodexToolChoiceValue(item); ok {
				items = append(items, normalized)
			} else {
				items = append(items, item)
			}
		}
		return items, true
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return value, false
		}

		var mapped map[string]any
		if err := json.Unmarshal(raw, &mapped); err != nil {
			return value, false
		}
		return normalizeCodexToolChoiceMap(mapped), true
	}
}

func normalizeCodexToolChoiceMap(value map[string]any) map[string]any {
	if value == nil {
		return value
	}

	if toolType, ok := value["type"].(string); ok {
		value["type"] = normalizeCodexBuiltinToolType(toolType)
	}

	if tools, ok := value["tools"].([]any); ok {
		for i, tool := range tools {
			if toolMap, ok := tool.(map[string]any); ok {
				tools[i] = normalizeCodexToolChoiceMap(toolMap)
			}
		}
		value["tools"] = tools
	}

	return value
}

func normalizeCodexBuiltinToolType(toolType string) string {
	if strings.TrimSpace(toolType) == "" {
		return toolType
	}

	if normalized := types.NormalizeResponsesWebSearchToolType(toolType); normalized == types.APIToolTypeWebSearch {
		return normalized
	}

	return toolType
}

func (p *CodexProvider) getPromptCacheKeyStrategy() string {
	if options := p.getChannelOptions(); options != nil {
		return normalizePromptCacheStrategy(options.PromptCacheKeyStrategy)
	}
	return codexPromptCacheStrategyOff
}

func backfillCodexResponsePromptCacheKey(response *types.OpenAIResponsesResponses, request *types.OpenAIResponsesRequest) {
	if response == nil || request == nil {
		return
	}
	if strings.TrimSpace(response.PromptCacheKey) != "" {
		return
	}
	if strings.TrimSpace(request.PromptCacheKey) == "" {
		return
	}
	response.PromptCacheKey = request.PromptCacheKey
}

// adaptCodexCLI adapts for Codex CLI.
func (p *CodexProvider) adaptCodexCLI(request *types.OpenAIResponsesRequest) {
	// Detect Codex CLI requests via instructions.
	isCodexCLI := false
	if request.Instructions != "" {
		instructions := request.Instructions
		isCodexCLI = len(instructions) > 50 && (len(instructions) >= len("You are a coding agent running in the Codex CLI") &&
			instructions[:len("You are a coding agent running in the Codex CLI")] == "You are a coding agent running in the Codex CLI" ||
			len(instructions) >= len("You are Codex") &&
				instructions[:len("You are Codex")] == "You are Codex")
	}

	// Apply defaults for non-CLI requests.
	if !isCodexCLI {
		// Remove incompatible fields.
		request.Temperature = nil
		request.TopP = nil
		request.MaxOutputTokens = 0

		// Codex backend rejects system/developer roles in input messages.
		mergeSystemInputMessagesForCodex(request)

		// Set default Codex CLI instructions.
		request.Instructions = CodexCLIInstructions
	}
}

func mergeSystemInputMessagesForCodex(request *types.OpenAIResponsesRequest) {
	inputs, err := request.ParseInput()
	if err != nil || len(inputs) == 0 {
		return
	}

	merged := make([]types.InputResponses, 0, len(inputs))
	pendingSystemText := make([]string, 0, 2)

	for _, input := range inputs {
		if isSystemInputMessage(input) {
			systemText := strings.TrimSpace(extractInputMessageText(input))
			if systemText != "" {
				pendingSystemText = append(pendingSystemText, systemText)
			}
			continue
		}

		if len(pendingSystemText) > 0 && isMergeableInputMessage(input) {
			input = prependSystemTextToInputMessage(input, strings.Join(pendingSystemText, "\n\n"))
			pendingSystemText = pendingSystemText[:0]
		}

		merged = append(merged, input)
	}

	// If no following message exists, keep system content as a user message.
	if len(pendingSystemText) > 0 {
		merged = append(merged, types.InputResponses{
			Type: types.InputTypeMessage,
			Role: types.ChatMessageRoleUser,
			Content: []types.ContentResponses{
				{
					Type: types.ContentTypeInputText,
					Text: strings.Join(pendingSystemText, "\n\n"),
				},
			},
		})
	}

	request.Input = merged
}

func isSystemInputMessage(input types.InputResponses) bool {
	if input.Type != "" && input.Type != types.InputTypeMessage {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(input.Role)) {
	case types.ChatMessageRoleSystem, types.ChatMessageRoleDeveloper:
		return true
	default:
		return false
	}
}

func isMergeableInputMessage(input types.InputResponses) bool {
	if input.Type != "" && input.Type != types.InputTypeMessage {
		return false
	}

	return strings.ToLower(strings.TrimSpace(input.Role)) == types.ChatMessageRoleUser
}

func extractInputMessageText(input types.InputResponses) string {
	if input.Content == nil {
		return ""
	}
	if content, ok := input.Content.(string); ok {
		return content
	}

	contentList, err := input.ParseContent()
	if err != nil || len(contentList) == 0 {
		return ""
	}

	textParts := make([]string, 0, len(contentList))
	for _, content := range contentList {
		if content.Type == types.ContentTypeInputText || content.Type == types.ContentTypeOutputText || content.Type == "" {
			if strings.TrimSpace(content.Text) != "" {
				textParts = append(textParts, content.Text)
			}
		}
	}

	return strings.Join(textParts, "\n")
}

func prependSystemTextToInputMessage(input types.InputResponses, systemText string) types.InputResponses {
	systemText = strings.TrimSpace(systemText)
	if systemText == "" {
		return input
	}

	if content, ok := input.Content.(string); ok {
		if strings.TrimSpace(content) == "" {
			input.Content = systemText
		} else {
			input.Content = systemText + "\n\n" + content
		}
		return input
	}

	contentList, err := input.ParseContent()
	if err != nil || len(contentList) == 0 {
		input.Content = systemText
		return input
	}

	if contentList[0].Type == types.ContentTypeInputText || contentList[0].Type == types.ContentTypeOutputText || contentList[0].Type == "" {
		if strings.TrimSpace(contentList[0].Text) == "" {
			contentList[0].Text = systemText
		} else {
			contentList[0].Text = systemText + "\n\n" + contentList[0].Text
		}
	} else {
		contentList = append([]types.ContentResponses{
			{
				Type: types.ContentTypeInputText,
				Text: systemText,
			},
		}, contentList...)
	}

	input.Content = contentList
	return input
}

// collectResponsesStreamResponse aggregates stream to a response.
func (p *CodexProvider) collectResponsesStreamResponse(stream requester.StreamReaderInterface[string]) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	var response *types.OpenAIResponsesResponses

	dataChan, errChan := stream.Recv()

	for {
		select {
		case data, ok := <-dataChan:
			if !ok {
				goto buildResponse
			}

			if strings.TrimSpace(data) == "" {
				continue
			}

			// Extract JSON payload from SSE.
			jsonData := extractJSONFromSSE(data)
			if jsonData == "" {
				continue
			}

			// Parse stream payload.
			var streamResp types.OpenAIResponsesStreamResponses
			if err := json.Unmarshal([]byte(jsonData), &streamResp); err != nil {
				continue
			}

			// Capture terminal response event.
			if (streamResp.Type == "response.completed" || streamResp.Type == "response.failed" || streamResp.Type == "response.incomplete" || streamResp.Type == "response.done") && streamResp.Response != nil {
				response = streamResp.Response
			}

		case err, ok := <-errChan:
			if !ok {
				continue
			}
			if err != nil {
				// EOF is normal end-of-stream.
				if errors.Is(err, io.EOF) {
					goto buildResponse
				}
				return nil, common.ErrorWrapper(err, "stream_read_failed", http.StatusInternalServerError)
			}
		}
	}

buildResponse:
	if response == nil {
		return nil, common.StringErrorWrapperLocal("no response received", "no_response", http.StatusInternalServerError)
	}
	if p.Usage == nil {
		p.Usage = &types.Usage{}
	}
	finalizeCodexResponsesUsage(p.Usage, response, "", false)
	return response, nil
}

// extractJSONFromSSE extracts JSON payload from SSE data.
func extractJSONFromSSE(sseData string) string {
	// SSE format example:
	// event: response.created
	//
	// data: {"type":"response.created",...}
	//
	// Extract JSON after data: prefix.

	lines := strings.Split(sseData, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			return payload
		}
	}
	return ""
}

// getResponsesRequest builds the Responses request.
func (p *CodexProvider) getResponsesRequest(request *types.OpenAIResponsesRequest) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	return p.getResponsesOperationRequestWithSession(request, "", "")
}

func (p *CodexProvider) getResponsesRequestWithSession(request *types.OpenAIResponsesRequest, sessionID string) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	return p.getResponsesOperationRequestWithSession(request, "", sessionID)
}

func (p *CodexProvider) getResponsesOperationRequest(request *types.OpenAIResponsesRequest, pathSuffix string) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	return p.getResponsesOperationRequestWithSession(request, pathSuffix, "")
}

func (p *CodexProvider) getResponsesOperationRequestWithSession(request *types.OpenAIResponsesRequest, pathSuffix, sessionID string) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	ensureStablePromptCacheKey(request, p.Context, p.getPromptCacheKeyStrategy())

	requestPath := strings.TrimRight(p.Config.Responses, "/")
	if pathSuffix != "" {
		requestPath += "/" + strings.TrimLeft(pathSuffix, "/")
	}

	// Build full request URL.
	fullRequestURL := p.GetFullRequestURL(requestPath, request.Model)

	// Build headers with token error handling.
	headers, err := p.getRequestHeaderBag()
	if err != nil {
		return nil, p.handleTokenError(err)
	}

	applyCodexExecutionSessionHeader(headers, resolveCodexExecutionSessionID(headers, sessionID))

	// Reuse prompt cache identity as the Codex conversation/session identifier.
	if strings.TrimSpace(request.PromptCacheKey) != "" {
		headers.Set("Conversation_id", request.PromptCacheKey)
		headers.Set("session_id", request.PromptCacheKey)
	}

	// Apply Codex default headers.
	p.applyDefaultHeaders(headers)

	if request.Stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}

	// Create request via requester.
	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(request), p.Requester.WithHeader(headers.Map()))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}

	return req, nil
}

// HandlerResponsesStream handles Responses streaming (passthrough).
func (h *CodexResponsesStreamHandler) HandlerResponsesStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	rawStr := string(*rawLine)

	// Handle SSE event lines.
	if strings.HasPrefix(rawStr, "event: ") {
		// Start new event and capture event type.
		h.eventType = strings.TrimPrefix(rawStr, "event: ")
		h.eventBuffer.Reset()
		h.eventBuffer.WriteString(rawStr)
		h.eventBuffer.WriteString("\n")
		return
	}

	// Buffer non-data lines when inside an event.
	if !strings.HasPrefix(rawStr, "data:") {
		if h.eventBuffer.Len() > 0 {
			h.eventBuffer.WriteString(rawStr)
			h.eventBuffer.WriteString("\n")
		} else {
			// No event type: forward as-is.
			dataChan <- rawStr
		}
		return
	}

	// Handle data line.
	dataLine := strings.TrimPrefix(rawStr, "data:")
	dataLine = strings.TrimSpace(dataLine)

	// Skip [DONE].
	if dataLine == "[DONE]" {
		// Flush buffered event.
		if h.eventBuffer.Len() > 0 {
			dataChan <- h.eventBuffer.String()
			h.eventBuffer.Reset()
			h.eventType = ""
		}
		return
	}

	h.observeUsageEvent(dataLine)

	// Passthrough: buffer or forward raw data.
	if h.eventBuffer.Len() > 0 {
		// Buffer data line within event.
		h.eventBuffer.WriteString(rawStr)
		h.eventBuffer.WriteString("\n")

		// Send event when complete (blank line).
		if strings.HasSuffix(h.eventBuffer.String(), "\n\n") {
			// Send complete event.
			dataChan <- h.eventBuffer.String()
			h.eventBuffer.Reset()
			h.eventType = ""
		}
	} else {
		// No event type: forward data line.
		dataChan <- rawStr
	}
}
