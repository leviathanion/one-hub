package codex

import (
	"encoding/json"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/requester"
	"one-api/types"
)

// CodexResponsesStreamHandler handles Codex Responses streaming.
type CodexResponsesStreamHandler struct {
	Usage       *types.Usage
	eventBuffer strings.Builder
	eventType   string
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
	handler := &CodexResponsesStreamHandler{
		Usage: p.Usage,
	}

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
	handler := &CodexResponsesStreamHandler{
		Usage: p.Usage,
	}

	// Use RequestNoTrimStream to preserve event lines.
	return requester.RequestNoTrimStream(p.Requester, resp, handler.HandlerResponsesStream)
}

func (p *CodexProvider) CompactResponses(request *types.OpenAIResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return nil, common.StringErrorWrapperLocal("The API interface is not supported", "unsupported_api", http.StatusNotImplemented)
}

// prepareCodexRequest prepares Codex request fields.
func (p *CodexProvider) prepareCodexRequest(request *types.OpenAIResponsesRequest) {
	// Normalize gpt-5-* model names.
	if len(request.Model) > 6 && request.Model[:6] == "gpt-5-" && request.Model != "gpt-5-codex" {
		request.Model = "gpt-5"
	}

	// Codex requires store=false.
	storeFalse := false
	request.Store = &storeFalse

	// Prefer temperature over top_p when both set.
	if request.Temperature != nil && request.TopP != nil {
		request.TopP = nil
	}

	// Adapt to Codex CLI format.
	p.adaptCodexCLI(request)
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

			// Capture response.completed event.
			if streamResp.Type == "response.completed" && streamResp.Response != nil {
				response = streamResp.Response
				if response.Usage != nil {
					p.Usage.PromptTokens = response.Usage.InputTokens
					p.Usage.CompletionTokens = response.Usage.OutputTokens
					p.Usage.TotalTokens = response.Usage.TotalTokens
				}
			}

		case err, ok := <-errChan:
			if !ok {
				continue
			}
			if err != nil {
				// EOF is normal end-of-stream.
				if err.Error() == "EOF" {
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
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimPrefix(line, "data: ")
		}
	}
	return ""
}

// getResponsesRequest builds the Responses request.
func (p *CodexProvider) getResponsesRequest(request *types.OpenAIResponsesRequest) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	// Build full request URL.
	fullRequestURL := p.GetFullRequestURL(p.Config.Responses, request.Model)

	// Build headers with token error handling.
	headers, err := p.getRequestHeadersInternal()
	if err != nil {
		return nil, p.handleTokenError(err)
	}

	// Apply Codex default headers.
	p.applyDefaultHeaders(headers)

	if request.Stream {
		headers["Accept"] = "text/event-stream"
	} else {
		headers["Accept"] = "application/json"
	}

	// Create request via requester.
	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(request), p.Requester.WithHeader(headers))
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
	if !strings.HasPrefix(rawStr, "data: ") {
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
	dataLine := strings.TrimPrefix(rawStr, "data: ")
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

	// Parse JSON to extract usage (no mutation).
	var responsesEvent types.OpenAIResponsesStreamResponses
	if err := json.Unmarshal([]byte(dataLine), &responsesEvent); err == nil {
		// Extract usage info.
		if responsesEvent.Type == "response.completed" && responsesEvent.Response != nil {
			if responsesEvent.Response.Usage != nil {
				h.Usage.PromptTokens = responsesEvent.Response.Usage.InputTokens
				h.Usage.CompletionTokens = responsesEvent.Response.Usage.OutputTokens
				h.Usage.TotalTokens = responsesEvent.Response.Usage.TotalTokens
			}
		}
	}

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
