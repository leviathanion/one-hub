package codex

import (
	"encoding/json"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/requester"
	"one-api/types"
)

const codexRealtimeBridgeReasoningEncryptedContentInclude = "reasoning.encrypted_content"

// prepareCodexRealtimeBridgeRequest is intentionally scoped to the legacy
// /v1/realtime HTTP bridge. Codex Official /v1/responses and ResponsesWS native
// paths must use providers/codex/wire instead of this typed repair path.
func (p *CodexProvider) prepareCodexRealtimeBridgeRequest(request *types.OpenAIResponsesRequest) {
	p.prepareCodexRealtimeBridgeRequestWithReasoning(request, true)
}

func (p *CodexProvider) prepareCodexRealtimeBridgeRequestWithReasoning(request *types.OpenAIResponsesRequest, includeReasoning bool) {
	request.Model = normalizeCodexModelName(request.Model)

	if includeReasoning {
		storeFalse := false
		request.Store = &storeFalse
	} else {
		request.Store = nil
	}

	if request.Temperature != nil && request.TopP != nil {
		request.TopP = nil
	}
	request.ContextManagement = nil
	request.Truncation = ""

	ensureStablePromptCacheKey(request, p.Context, p.getPromptCacheKeyStrategy())
	if includeReasoning {
		ensureCodexRealtimeBridgeIncludes(request)
	} else {
		request.Include = nil
	}
	normalizeCodexRealtimeBridgeBuiltinTools(request)
	p.adaptCodexRealtimeBridgeCLI(request)
}

func ensureCodexRealtimeBridgeIncludes(request *types.OpenAIResponsesRequest) {
	if request == nil {
		return
	}

	switch include := request.Include.(type) {
	case nil:
		request.Include = []string{codexRealtimeBridgeReasoningEncryptedContentInclude}
	case []string:
		request.Include = appendUniqueRealtimeBridgeStrings(include, codexRealtimeBridgeReasoningEncryptedContentInclude)
	case []any:
		values := make([]string, 0, len(include)+1)
		for _, item := range include {
			if str, ok := item.(string); ok && strings.TrimSpace(str) != "" {
				values = append(values, str)
			}
		}
		request.Include = appendUniqueRealtimeBridgeStrings(values, codexRealtimeBridgeReasoningEncryptedContentInclude)
	case string:
		request.Include = appendUniqueRealtimeBridgeStrings([]string{include}, codexRealtimeBridgeReasoningEncryptedContentInclude)
	default:
		raw, err := json.Marshal(include)
		if err != nil {
			request.Include = []string{codexRealtimeBridgeReasoningEncryptedContentInclude}
			return
		}

		var values []string
		if err := json.Unmarshal(raw, &values); err != nil {
			request.Include = []string{codexRealtimeBridgeReasoningEncryptedContentInclude}
			return
		}
		request.Include = appendUniqueRealtimeBridgeStrings(values, codexRealtimeBridgeReasoningEncryptedContentInclude)
	}
}

func appendUniqueRealtimeBridgeStrings(items []string, extra string) []string {
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

func normalizeCodexRealtimeBridgeBuiltinTools(request *types.OpenAIResponsesRequest) {
	if request == nil {
		return
	}

	for i := range request.Tools {
		request.Tools[i].Type = normalizeCodexRealtimeBridgeBuiltinToolType(request.Tools[i].Type)
	}

	if request.ToolChoice == nil {
		return
	}

	normalized, ok := normalizeCodexRealtimeBridgeToolChoiceValue(request.ToolChoice)
	if ok {
		request.ToolChoice = normalized
	}
}

func normalizeCodexRealtimeBridgeToolChoiceValue(value any) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return normalizeCodexRealtimeBridgeToolChoiceMap(typed), true
	case []any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			if normalized, ok := normalizeCodexRealtimeBridgeToolChoiceValue(item); ok {
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
		return normalizeCodexRealtimeBridgeToolChoiceMap(mapped), true
	}
}

func normalizeCodexRealtimeBridgeToolChoiceMap(value map[string]any) map[string]any {
	if value == nil {
		return value
	}

	if toolType, ok := value["type"].(string); ok {
		value["type"] = normalizeCodexRealtimeBridgeBuiltinToolType(toolType)
	}

	if tools, ok := value["tools"].([]any); ok {
		for i, tool := range tools {
			if toolMap, ok := tool.(map[string]any); ok {
				tools[i] = normalizeCodexRealtimeBridgeToolChoiceMap(toolMap)
			}
		}
		value["tools"] = tools
	}

	return value
}

func normalizeCodexRealtimeBridgeBuiltinToolType(toolType string) string {
	if strings.TrimSpace(toolType) == "" {
		return toolType
	}

	if normalized := types.NormalizeResponsesWebSearchToolType(toolType); normalized == types.APIToolTypeWebSearch {
		return normalized
	}

	return toolType
}

func (p *CodexProvider) adaptCodexRealtimeBridgeCLI(request *types.OpenAIResponsesRequest) {
	isCodexCLI := false
	if request.Instructions != "" {
		instructions := request.Instructions
		isCodexCLI = len(instructions) > 50 && (len(instructions) >= len("You are a coding agent running in the Codex CLI") &&
			instructions[:len("You are a coding agent running in the Codex CLI")] == "You are a coding agent running in the Codex CLI" ||
			len(instructions) >= len("You are Codex") &&
				instructions[:len("You are Codex")] == "You are Codex")
	}

	if !isCodexCLI {
		request.Temperature = nil
		request.TopP = nil
		request.MaxOutputTokens = 0
		mergeRealtimeBridgeSystemInputMessages(request)
		request.Instructions = CodexCLIInstructions
	}
}

func mergeRealtimeBridgeSystemInputMessages(request *types.OpenAIResponsesRequest) {
	inputs, err := request.ParseInput()
	if err != nil || len(inputs) == 0 {
		return
	}

	merged := make([]types.InputResponses, 0, len(inputs))
	pendingSystemText := make([]string, 0, 2)

	for _, input := range inputs {
		if isRealtimeBridgeSystemInputMessage(input) {
			systemText := strings.TrimSpace(extractRealtimeBridgeInputMessageText(input))
			if systemText != "" {
				pendingSystemText = append(pendingSystemText, systemText)
			}
			continue
		}

		if len(pendingSystemText) > 0 && isRealtimeBridgeMergeableInputMessage(input) {
			input = prependRealtimeBridgeSystemTextToInputMessage(input, strings.Join(pendingSystemText, "\n\n"))
			pendingSystemText = pendingSystemText[:0]
		}

		merged = append(merged, input)
	}

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

func isRealtimeBridgeSystemInputMessage(input types.InputResponses) bool {
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

func isRealtimeBridgeMergeableInputMessage(input types.InputResponses) bool {
	if input.Type != "" && input.Type != types.InputTypeMessage {
		return false
	}

	return strings.ToLower(strings.TrimSpace(input.Role)) == types.ChatMessageRoleUser
}

func extractRealtimeBridgeInputMessageText(input types.InputResponses) string {
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

func prependRealtimeBridgeSystemTextToInputMessage(input types.InputResponses, systemText string) types.InputResponses {
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

func (p *CodexProvider) getCodexRealtimeBridgeResponsesRequestWithSession(request *types.OpenAIResponsesRequest, sessionID string) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	ensureStablePromptCacheKey(request, p.Context, p.getPromptCacheKeyStrategy())

	requestPath := strings.TrimRight(p.Config.Responses, "/")
	fullRequestURL := p.GetFullRequestURL(requestPath, request.Model)

	headers, err := p.getRequestHeaderBag()
	if err != nil {
		return nil, p.handleTokenError(err)
	}
	applyCodexExecutionSessionHeader(headers, resolveCodexExecutionSessionID(headers, sessionID))
	p.applyDefaultHeaders(headers)
	headers.Set("Accept", "text/event-stream")

	httpRequester := p.codexRequester()
	if httpRequester == nil {
		return nil, common.StringErrorWrapperLocal("requester is not configured", "channel_error", http.StatusServiceUnavailable)
	}
	req, err := httpRequester.NewRequest(http.MethodPost, fullRequestURL, httpRequester.WithBody(request), httpRequester.WithHeader(headers.Map()))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	return req, nil
}

func (p *CodexProvider) responsesHTTPBridgeSecurity() requester.ResponsesHTTPBridgeSecurity {
	return requester.ResponsesHTTPBridgeSecurity{
		AllowSelfHosted: p.codexResponsesWSSelfHosted(),
		ProxyAddr:       channelProxyValue(p.codexChannel()),
	}
}
