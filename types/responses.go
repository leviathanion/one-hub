package types

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"one-api/common/utils"
	"reflect"
	"strconv"
	"strings"
)

const (
	APIToolTypeWebSearchPreview = "web_search_preview"
	APIToolTypeWebSearch        = "web_search"
	APIToolTypeFileSearch       = "file_search"
	APIToolTypeCodeInterpreter  = "code_interpreter"
	APIToolTypeImageGeneration  = "image_generation"
	APIToolTypeShell            = "shell"
	APIToolTypeLocalShell       = "local_shell"
)

const apiToolTypeWebSearchPreview20250311 = "web_search_preview_2025_03_11"

func IsResponsesWebSearchToolType(toolType string) bool {
	switch strings.TrimSpace(toolType) {
	case APIToolTypeWebSearchPreview, APIToolTypeWebSearch, apiToolTypeWebSearchPreview20250311:
		return true
	default:
		return false
	}
}

func NormalizeResponsesWebSearchToolType(toolType string) string {
	trimmed := strings.TrimSpace(toolType)
	if trimmed == "" {
		return ""
	}
	if IsResponsesWebSearchToolType(trimmed) {
		return APIToolTypeWebSearch
	}
	return trimmed
}

// message / file_search_call / computer_call / web_search_call / computer_call_output / function_call / function_call_output / reasoning / image_generation_call / code_interpreter_call / local_shell_call / local_shell_call_output / mcp_list_tools / mcp_approval_request / mcp_approval_response / mcp_call

const (
	InputTypeMessage              = "message"
	InputTypeFileSearchCall       = "file_search_call"
	InputTypeComputerCall         = "computer_call"
	InputTypeWebSearchCall        = "web_search_call"
	InputTypeComputerCallOutput   = "computer_call_output"
	InputTypeFunctionCall         = "function_call"
	InputTypeFunctionCallOutput   = "function_call_output"
	InputTypeCustomToolCall       = "custom_tool_call"
	InputTypeCustomToolCallOutput = "custom_tool_call_output"
	InputTypeReasoning            = "reasoning"
	InputTypeImageGenerationCall  = "image_generation_call"
	InputTypeCodeInterpreterCall  = "code_interpreter_call"
	InputTypeShellCall            = "shell_call"
	InputTypeShellCallOutput      = "shell_call_output"
	InputTypeLocalShellCall       = "local_shell_call"
	InputTypeLocalShellCallOutput = "local_shell_call_output"
	InputTypeMCPListTools         = "mcp_list_tools"
	InputTypeMCPApprovalRequest   = "mcp_approval_request"
	InputTypeMCPApprovalResponse  = "mcp_approval_response"
	InputTypeMCPCall              = "mcp_call"
)

// input_text / input_image / input_file / output_text / refusal
const (
	ContentTypeInputText     = "input_text"
	ContentTypeInputImage    = "input_image"
	ContentTypeInputFile     = "input_file"
	ContentTypeOutputText    = "output_text"
	ContentTypeReasoningText = "reasoning_text"
	ContentTypeSummaryText   = "summary_text"
	ContentTypeRefusal       = "refusal"
)

// completed, failed, in_progress, cancelled, queued, or incomplete
const (
	ResponseStatusCompleted  = "completed"
	ResponseStatusFailed     = "failed"
	ResponseStatusInProgress = "in_progress"
	ResponseStatusCancelled  = "cancelled"
	ResponseStatusQueued     = "queued"
	ResponseStatusIncomplete = "incomplete"
)

type OpenAIResponsesRequest struct {
	Input                any               `json:"input,omitempty"`
	Model                string            `json:"model" binding:"required"`
	Background           *bool             `json:"background,omitempty"`
	Conversation         any               `json:"conversation,omitempty"`
	Include              any               `json:"include,omitempty"`
	Instructions         string            `json:"instructions,omitempty"`
	MaxOutputTokens      int               `json:"max_output_tokens,omitempty"`
	MaxToolCalls         *int              `json:"max_tool_calls,omitempty"`
	Metadata             map[string]string `json:"metadata,omitempty"`
	ParallelToolCalls    *bool             `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID   string            `json:"previous_response_id,omitempty"`
	Prompt               any               `json:"prompt,omitempty"`
	PromptCacheKey       string            `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string            `json:"prompt_cache_retention,omitempty"`
	Reasoning            *ReasoningEffort  `json:"reasoning,omitempty"`
	SafetyIdentifier     string            `json:"safety_identifier,omitempty"`
	ServiceTier          string            `json:"service_tier,omitempty"`
	ProcessingClass      string            `json:"processing_class,omitempty"`
	Store                *bool             `json:"store,omitempty"` // 是否存储响应结果
	Stream               bool              `json:"stream,omitempty"`
	StreamOptions        any               `json:"stream_options,omitempty"`
	Temperature          *float64          `json:"temperature,omitempty"`
	Text                 *ResponsesText    `json:"text,omitempty"`
	ToolChoice           any               `json:"tool_choice,omitempty"`
	Tools                []ResponsesTools  `json:"tools,omitempty"`
	TopLogProbs          any               `json:"top_logprobs,omitempty"` // The number of top log probabilities to return for each token in the response.
	TopP                 *float64          `json:"top_p,omitempty"`
	Truncation           string            `json:"truncation,omitempty"`
	ContextManagement    any               `json:"context_management,omitempty"`

	ConvertChat bool `json:"-"`
}

type ResponsesText struct {
	Format    *ResponsesTextFormat `json:"format,omitempty"`
	Verbosity string               `json:"verbosity,omitempty"`
}

type ResponsesTextFormat struct {
	Type        string `json:"type,omitempty"`
	Name        string `json:"name,omitempty"` // The name of the text format. This is used to identify the text format in the response.
	Schema      any    `json:"schema,omitempty"`
	Description string `json:"description,omitempty"`
	Strict      any    `json:"strict,omitempty"`
}

func (r *OpenAIResponsesRequest) ToChatCompletionRequest() (*ChatCompletionRequest, error) {
	chat := &ChatCompletionRequest{
		Model:               r.Model,
		MaxCompletionTokens: r.MaxOutputTokens,
		Stream:              r.Stream,
		Temperature:         r.Temperature,
		ServiceTier:         r.ServiceTier,
		ProcessingClass:     r.ProcessingClass,
		SafetyIdentifier:    r.SafetyIdentifier,
		Store:               r.Store,
		// ResponseFormat:    r.Text,
		ToolChoice: responsesToolChoiceToChat(r.ToolChoice),
		TopP:       r.TopP,
	}
	if r.ParallelToolCalls != nil {
		value := *r.ParallelToolCalls
		chat.ParallelToolCalls = &value
	}

	if r.Reasoning != nil {
		if r.Reasoning.Summary != nil || r.Reasoning.GenerateSummary != nil {
			return nil, errors.New("reasoning summary cannot be represented by Chat Completions")
		}
		if r.Reasoning.Effort != nil {
			effort := *r.Reasoning.Effort
			chat.ReasoningEffort = &effort
		}
	}

	if r.Text != nil && r.Text.Format != nil {
		chat.ResponseFormat = &ChatCompletionResponseFormat{
			Type: r.Text.Format.Type,
		}

		if r.Text.Format.Type == "json_schema" {
			chat.ResponseFormat.JsonSchema = &FormatJsonSchema{
				Description: r.Text.Format.Description,
				Name:        r.Text.Format.Name,
				Schema:      r.Text.Format.Schema,
				Strict:      r.Text.Format.Strict,
			}
		}
	}

	if r.Text != nil && r.Text.Verbosity != "" {
		chat.Verbosity = r.Text.Verbosity
	}

	if len(r.Tools) > 0 {
		chatTools := make([]*ChatCompletionTool, 0)
		for _, tool := range r.Tools {
			if tool.Type != "function" {
				continue
			}
			chatTools = append(chatTools, &ChatCompletionTool{
				Type: tool.Type,
				Function: ChatCompletionFunction{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  tool.Parameters,
					Strict:      tool.Strict,
				},
			})
		}

		if len(chatTools) > 0 {
			chat.Tools = chatTools
		}
	}

	var err error
	chat.Messages, err = r.InputToMessages()
	if err != nil {
		return nil, err
	}

	return chat, nil
}

func responsesToolChoiceToChat(choice any) any {
	object, ok := choice.(map[string]any)
	if !ok || strings.TrimSpace(responsesStringValue(object["type"])) != "function" {
		return choice
	}
	name := strings.TrimSpace(responsesStringValue(object["name"]))
	if name == "" {
		return choice
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": name,
		},
	}
}

func responsesStringValue(value any) string {
	text, _ := value.(string)
	return text
}

func (r *OpenAIResponsesRequest) ParseInput() ([]InputResponses, error) {
	inputs := make([]InputResponses, 0)
	if input, ok := r.Input.(string); ok {
		inputs = append(inputs, InputResponses{
			Role:    "user",
			Content: input,
			Type:    InputTypeMessage,
		})
		return inputs, nil
	}

	contentBytes, err := json.Marshal(r.Input)
	if err != nil {
		return nil, errors.New("failed to marshal input")
	}

	err = json.Unmarshal(contentBytes, &inputs)
	if err != nil {
		return nil, errors.New("failed to unmarshal input")
	}

	return inputs, nil
}

func (r *OpenAIResponsesRequest) InputToMessages() ([]ChatCompletionMessage, error) {
	messages := make([]ChatCompletionMessage, 0)

	if r.Instructions != "" {
		messages = append(messages, ChatCompletionMessage{
			Role:    "system",
			Content: r.Instructions,
		})
	}

	inputs, err := r.ParseInput()
	if err != nil {
		return nil, err
	}

	for _, item := range inputs {
		switch item.Type {
		case InputTypeMessage, "":
			if item.Content == nil {
				return nil, errors.New("message content is nil")
			}

			contents, err := item.ParseContent()
			if err != nil {
				return nil, err
			}

			msg := ChatCompletionMessage{
				Role: item.Role,
			}
			msgContents := make([]ChatMessagePart, 0)
			for _, contentItem := range contents {
				msgContent, err := contentItem.ToChatContent()
				if err != nil {
					return nil, err
				}

				if msgContent != nil {
					msgContents = append(msgContents, *msgContent)
				}
			}

			if len(msgContents) == 0 {
				return nil, errors.New("message contents cannot be empty")
			}

			msg.Content = msgContents
			messages = append(messages, msg)

		case InputTypeFunctionCall:
			messages = append(messages, ChatCompletionMessage{
				Role: "assistant",
				ToolCalls: []*ChatCompletionToolCalls{
					{
						Id:   item.CallID,
						Type: "function",
						Function: &ChatCompletionToolCallsFunction{
							Name:      item.Name,
							Arguments: item.Arguments,
						},
					},
				},
			})

		case InputTypeFunctionCallOutput:
			content, err := responsesToolOutputToChat(item.Output)
			if err != nil {
				return nil, fmt.Errorf("convert function_call_output: %w", err)
			}
			messages = append(messages, ChatCompletionMessage{
				Role:       "tool",
				ToolCallID: item.CallID,
				Content:    content,
			})

		case InputTypeCustomToolCall:
			input, ok := item.Input.(string)
			if !ok {
				return nil, errors.New("custom_tool_call input must be a string")
			}
			messages = append(messages, ChatCompletionMessage{
				Role: "assistant",
				ToolCalls: []*ChatCompletionToolCalls{
					{
						Id:   item.CallID,
						Type: ToolChoiceTypeCustom,
						Custom: &ChatCompletionToolCallsCustom{
							Name:  item.Name,
							Input: input,
						},
					},
				},
			})

		case InputTypeCustomToolCallOutput:
			content, err := responsesToolOutputToChat(item.Output)
			if err != nil {
				return nil, fmt.Errorf("convert custom_tool_call_output: %w", err)
			}
			messages = append(messages, ChatCompletionMessage{
				Role:       "tool",
				ToolCallID: item.CallID,
				Content:    content,
			})

		default:
			continue
		}
	}

	return messages, nil
}

func responsesToolOutputToChat(output any) (any, error) {
	if text, ok := output.(string); ok {
		return text, nil
	}

	raw, err := json.Marshal(output)
	if err != nil {
		return nil, errors.New("tool output cannot be represented as Chat content")
	}
	var rawParts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawParts); err != nil || len(rawParts) == 0 {
		return nil, errors.New("tool output must be a string or a non-empty input_text array")
	}

	chatParts := make([]ChatMessagePart, 0, len(rawParts))
	for index, rawPart := range rawParts {
		for field, value := range rawPart {
			if field != "type" && field != "text" && !isResponsesJSONNull(value) {
				return nil, fmt.Errorf("tool output part %d field %q cannot be represented as Chat content", index, field)
			}
		}
		var part ContentResponses
		partJSON, err := json.Marshal(rawPart)
		if err != nil || json.Unmarshal(partJSON, &part) != nil || part.Type != ContentTypeInputText {
			return nil, fmt.Errorf("tool output part %d cannot be represented as Chat content", index)
		}
		chatParts = append(chatParts, ChatMessagePart{Type: ContentTypeText, Text: part.Text})
	}
	return chatParts, nil
}

func isResponsesJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

type InputResponses struct {
	Role    string `json:"role,omitempty"`
	Content any    `json:"content,omitempty"` // string or ContentResponses
	Type    string `json:"type,omitempty"`    // message / file_search_call / computer_call / web_search_call / computer_call_output / function_call / function_call_output / reasoning / image_generation_call / code_interpreter_call / local_shell_call / local_shell_call_output / mcp_list_tools / mcp_approval_request / mcp_approval_response / mcp_call
	Status  string `json:"status,omitempty"`  // The status of item. One of in_progress, completed, or incomplete. Populated when items are returned via API.

	// file_search_call
	ID      string `json:"id,omitempty"`
	Queries any    `json:"queries,omitempty"`
	Results any    `json:"results,omitempty"` // The results of the file search call.

	// computer_call
	Action              any    `json:"action,omitempty"`
	CallID              string `json:"call_id,omitempty"`
	PendingSafetyChecks any    `json:"pending_safety_checks,omitempty"`

	// computer_call_output
	Output                   any `json:"output,omitempty"`
	AcknowledgedSafetyChecks any `json:"acknowledged_safety_checks,omitempty"`

	// function_call
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// reasoning
	Summary          SummaryResponsesList `json:"summary,omitempty"`
	EncryptedContent *string              `json:"encrypted_content,omitempty"`

	// image_generation_call
	Result any `json:"result,omitempty"`

	// code_interpreter_call
	Code        string `json:"code,omitempty"`
	ContainerID string `json:"container_id,omitempty"`

	// mcp_list_tools
	ServerLabel string `json:"server_label,omitempty"`
	Tools       any    `json:"tools,omitempty"`
	Error       string `json:"error,omitempty"`

	// mcp_approval_request
	ApprovalRequestID string `json:"approval_request_id,omitempty"`
	Approve           bool   `json:"approve,omitempty"`
	Reason            string `json:"reason,omitempty"`

	Input any `json:"input,omitempty"` // The input to the tool call. This can be a string or a list of ContentResponses.
}

func (i *InputResponses) UnmarshalJSON(data []byte) error {
	type inputResponsesAlias InputResponses
	type inputResponsesPayload struct {
		*inputResponsesAlias
		Arguments json.RawMessage `json:"arguments"`
	}

	alias := inputResponsesAlias(*i)
	payload := inputResponsesPayload{inputResponsesAlias: &alias}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	*i = InputResponses(alias)
	if payload.Arguments != nil {
		arguments, _, err := responsesArgumentsString(payload.Arguments)
		if err != nil {
			return fmt.Errorf("decode responses input arguments: %w", err)
		}
		i.Arguments = arguments
	}
	return nil
}

func (i InputResponses) MarshalJSON() ([]byte, error) {
	type inputResponsesAlias InputResponses

	raw, err := json.Marshal(inputResponsesAlias(i))
	if err != nil {
		return nil, err
	}

	if i.Type != InputTypeReasoning {
		return raw, nil
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}

	payload["summary"] = summaryResponsesForMarshal(i.Summary)
	return json.Marshal(payload)
}

func (i *InputResponses) ParseContent() ([]ContentResponses, error) {
	contents := make([]ContentResponses, 0)

	if i.Content == nil {
		return nil, errors.New("content is nil")
	}

	if content, ok := i.Content.(string); ok {
		contents = append(contents, ContentResponses{
			Type: ContentTypeInputText,
			Text: content,
		})
		return contents, nil
	}

	contentBytes, err := json.Marshal(i.Content)
	if err != nil {
		return nil, errors.New("failed to marshal content")
	}

	err = json.Unmarshal(contentBytes, &contents)
	if err != nil {
		return nil, errors.New("failed to unmarshal content")
	}

	return contents, nil
}

type ContentResponses struct {
	Type string `json:"type"` // input_text / input_image / input_file / output_text / refusal
	//input_text
	Text string `json:"text,omitempty"`

	//input_image
	Detail   string `json:"detail,omitempty"`    // Image: low/high/original/auto. PDF input_file: low/high/auto.
	FileId   string `json:"file_id,omitempty"`   // The ID of the file to be sent to the model.
	ImageUrl string `json:"image_url,omitempty"` // The URL of the image to be sent to the model. A fully qualified URL or base64 encoded image in a data URL.

	// input_file
	FileData string `json:"file_data,omitempty"` // The content of the file to be sent to the model.
	FileUrl  string `json:"file_url,omitempty"`  // The URL of the file to be sent to the model.
	FileName string `json:"filename,omitempty"`  // The name of the file to be sent to the model.

	// output_text
	Annotations []Annotations `json:"annotations,omitempty"` // The annotations of the text output.
	Logprobs    any           `json:"logprobs,omitempty"`

	// refusal
	Refusal string `json:"refusal,omitempty"`

	rawFields map[string]json.RawMessage `json:"-"`
}

var contentResponsesJSONFields = collectJSONFieldNames(reflect.TypeOf(ContentResponses{}))

func (c *ContentResponses) UnmarshalJSON(data []byte) error {
	type contentResponsesAlias ContentResponses
	var alias contentResponsesAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFields); err != nil {
		return err
	}
	*c = ContentResponses(alias)
	c.rawFields = rawFields
	return nil
}

func (c ContentResponses) MarshalJSON() ([]byte, error) {
	type contentResponsesAlias ContentResponses
	knownJSON, err := json.Marshal(contentResponsesAlias(c))
	if err != nil {
		return nil, err
	}
	var knownFields map[string]json.RawMessage
	if err := json.Unmarshal(knownJSON, &knownFields); err != nil {
		return nil, err
	}
	fields := make(map[string]json.RawMessage, len(c.rawFields)+len(knownFields))
	for field, raw := range c.rawFields {
		fields[field] = raw
	}
	for field := range contentResponsesJSONFields {
		if _, projected := knownFields[field]; !projected && !isExplicitJSONEmpty(c.rawFields[field]) {
			delete(fields, field)
		}
	}
	for field, raw := range knownFields {
		fields[field] = raw
	}
	return json.Marshal(fields)
}

func (c *ContentResponses) ToChatContent() (*ChatMessagePart, error) {
	switch c.Type {
	case ContentTypeInputText, ContentTypeOutputText:
		return &ChatMessagePart{
			Type: "text",
			Text: c.Text,
		}, nil
	case ContentTypeInputImage:
		if c.FileId == "" && c.ImageUrl == "" {
			return nil, errors.New("input_image must have either file_id or image_url")
		}
		return &ChatMessagePart{
			Type: "image_url",
			ImageURL: &ChatMessageImageURL{
				URL:    c.ImageUrl,
				Detail: c.Detail,
			},
		}, nil
	case ContentTypeInputFile:
		if c.FileData == "" && c.FileName == "" {
			return nil, errors.New("input_file must have either file_data or filename")
		}
		return &ChatMessagePart{
			Type: "file",
			File: &ChatMessageFile{
				Filename: c.FileName,
				FileData: c.FileData,
				FileID:   c.FileId,
			},
		}, nil
	default:
		return nil, nil
	}
}

type Annotations struct {
	Type string `json:"type"` // file_citation / url_citation / container_file_citation / file_path
	// file_citation
	FileId string `json:"file_id,omitempty"` // The ID of the file that is cited.
	Index  int    `json:"index,omitempty"`   // The index of the file that is cited.

	// url_citation
	Url        string `json:"url,omitempty"`         // The URL of the web resource.
	Title      string `json:"title,omitempty"`       // The title of the web resource.
	StartIndex int    `json:"start_index,omitempty"` // The index of the first character of the URL citation in the message.
	EndIndex   int    `json:"end_index,omitempty"`   // The index of the last character of the URL citation in the message.

	// container_file_citation
	ContainerId string `json:"container_id,omitempty"` // The ID of the container file.

}

type SummaryResponses struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type SummaryResponsesList []SummaryResponses

func (s *SummaryResponsesList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = nil
		return nil
	}

	if trimmed[0] == '[' {
		var list []SummaryResponses
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		*s = SummaryResponsesList(list)
		return nil
	}

	var item SummaryResponses
	if err := json.Unmarshal(trimmed, &item); err != nil {
		return err
	}
	*s = SummaryResponsesList{item}
	return nil
}

func (s SummaryResponsesList) MarshalJSON() ([]byte, error) {
	return json.Marshal([]SummaryResponses(s))
}

func summaryResponsesForMarshal(summary SummaryResponsesList) []SummaryResponses {
	if len(summary) == 0 {
		return make([]SummaryResponses, 0)
	}
	return []SummaryResponses(summary)
}

type ResponsesTools struct {
	Type string `json:"type"`
	// Web Search
	UserLocation      any    `json:"user_location,omitempty"`
	SearchContextSize string `json:"search_context_size,omitempty"`
	// File Search
	VectorStoreIds []string `json:"vector_store_ids,omitempty"`
	MaxNumResults  uint     `json:"max_num_results,omitempty"`
	Filters        any      `json:"filters,omitempty"`
	RankingOptions any      `json:"ranking_options,omitempty"`
	// Computer Use
	DisplayWidth  uint            `json:"display_width,omitempty"`
	DisplayHeight uint            `json:"display_height,omitempty"`
	Environment   json.RawMessage `json:"environment,omitempty"`
	// Function
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`
	Execution   string `json:"execution,omitempty"`

	//MCP
	ServerLabel     string           `json:"server_label,omitempty"`
	ServerURL       string           `json:"server_url,omitempty"`
	AllowedTools    any              `json:"allowed_tools,omitempty"`
	Headers         any              `json:"headers,omitempty"`
	RequireApproval any              `json:"require_approval,omitempty"`
	Tools           []ResponsesTools `json:"tools,omitempty"`

	// Code interpreter
	Container any `json:"container,omitempty"`
	// Image generation tool
	Background        any    `json:"background,omitempty"`
	InputImageMask    any    `json:"input_image_mask,omitempty"`
	Model             string `json:"model,omitempty"`
	Moderation        any    `json:"moderation,omitempty"`
	OutputCompression any    `json:"output_compression,omitempty"`
	OutputFormat      any    `json:"output_format,omitempty"`
	PartialImages     any    `json:"partial_images,omitempty"`
	Quality           string `json:"quality,omitempty"`
	Size              string `json:"size,omitempty"`

	rawFields map[string]json.RawMessage `json:"-"`
}

var responsesToolsJSONFields = collectJSONFieldNames(reflect.TypeOf(ResponsesTools{}))

func (t *ResponsesTools) UnmarshalJSON(data []byte) error {
	type responsesToolAlias ResponsesTools

	var alias responsesToolAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}

	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFields); err != nil {
		return err
	}

	*t = ResponsesTools(alias)
	t.rawFields = rawFields
	return nil
}

func (t ResponsesTools) MarshalJSON() ([]byte, error) {
	type responsesToolAlias ResponsesTools

	knownFieldsJSON, err := json.Marshal(responsesToolAlias(t))
	if err != nil {
		return nil, fmt.Errorf("marshal responses tool: %w", err)
	}

	var knownFields map[string]json.RawMessage
	if err := json.Unmarshal(knownFieldsJSON, &knownFields); err != nil {
		return nil, fmt.Errorf("decode marshaled responses tool: %w", err)
	}

	rawFields := make(map[string]json.RawMessage, len(t.rawFields)+len(knownFields))
	for key, value := range t.rawFields {
		rawFields[key] = value
	}

	for key := range responsesToolsJSONFields {
		delete(rawFields, key)
	}

	for key, value := range knownFields {
		rawFields[key] = value
	}

	if t.stripsDescription() {
		delete(rawFields, "description")
	}

	return json.Marshal(rawFields)
}

func (t ResponsesTools) stripsDescription() bool {
	toolType := strings.TrimSpace(t.Type)
	// Preserve description for unknown/custom tools so rawFields remains forward-compatible;
	// strip it only for known hosted Responses tools that do not accept the field.
	if IsResponsesWebSearchToolType(toolType) {
		return true
	}

	switch toolType {
	case "tool_search":
		return strings.TrimSpace(t.Execution) != "client"
	case APIToolTypeFileSearch, APIToolTypeCodeInterpreter, APIToolTypeImageGeneration, "computer_use_preview", "mcp":
		return true
	default:
		return false
	}
}

func collectJSONFieldNames(t reflect.Type) map[string]struct{} {
	fieldNames := make(map[string]struct{}, t.NumField())

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}

		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}

		name := tag
		if idx := strings.IndexByte(name, ','); idx >= 0 {
			name = name[:idx]
		}
		if name == "" {
			name = field.Name
		}

		fieldNames[name] = struct{}{}
	}

	return fieldNames
}

type ReasoningEffort struct {
	Effort          *string `json:"effort,omitempty"`
	GenerateSummary *string `json:"generate_summary,omitempty"` // Deprecated
	Summary         *string `json:"summary,omitempty"`

	rawFields map[string]json.RawMessage `json:"-"`
}

var reasoningEffortJSONFields = collectJSONFieldNames(reflect.TypeOf(ReasoningEffort{}))

func (r *ReasoningEffort) UnmarshalJSON(data []byte) error {
	type reasoningEffortAlias ReasoningEffort
	var alias reasoningEffortAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFields); err != nil {
		return err
	}
	*r = ReasoningEffort(alias)
	r.rawFields = rawFields
	return nil
}

func (r ReasoningEffort) MarshalJSON() ([]byte, error) {
	type reasoningEffortAlias ReasoningEffort
	knownJSON, err := json.Marshal(reasoningEffortAlias(r))
	if err != nil {
		return nil, err
	}
	var knownFields map[string]json.RawMessage
	if err := json.Unmarshal(knownJSON, &knownFields); err != nil {
		return nil, err
	}
	fields := make(map[string]json.RawMessage, len(r.rawFields)+len(knownFields))
	for field, raw := range r.rawFields {
		fields[field] = raw
	}
	for field := range reasoningEffortJSONFields {
		if _, projected := knownFields[field]; !projected && !isExplicitJSONEmpty(r.rawFields[field]) {
			delete(fields, field)
		}
	}
	for field, raw := range knownFields {
		fields[field] = raw
	}
	return json.Marshal(fields)
}

type OpenAIResponsesResponses struct {
	Background           *bool             `json:"background,omitempty"`
	CreatedAt            any               `json:"created_at,omitempty"`
	Conversation         any               `json:"conversation,omitempty"`
	Error                *OpenAIError      `json:"error,omitempty"`
	ID                   string            `json:"id,omitempty"`
	IncompleteDetail     *IncompleteDetail `json:"incomplete_details,omitempty"`
	Instructions         any               `json:"instructions,omitempty"`
	MaxOutputTokens      int               `json:"max_output_tokens,omitempty"`
	MaxToolCalls         *int              `json:"max_tool_calls,omitempty"`
	Metadata             map[string]string `json:"metadata,omitempty"`
	Model                string            `json:"model,omitempty"`
	Object               string            `json:"object"`
	Output               []ResponsesOutput `json:"output,omitempty"`
	ParallelToolCalls    *bool             `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID   string            `json:"previous_response_id,omitempty"`
	Prompt               any               `json:"prompt,omitempty"`
	PromptCacheKey       string            `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string            `json:"prompt_cache_retention,omitempty"`
	Reasoning            *ReasoningEffort  `json:"reasoning,omitempty"`
	SafetyIdentifier     string            `json:"safety_identifier,omitempty"`
	ServiceTier          string            `json:"service_tier,omitempty"`
	ProcessingClass      string            `json:"processing_class,omitempty"`
	Status               string            `json:"status,omitempty"`
	Store                *bool             `json:"store,omitempty"`
	Temperature          *float64          `json:"temperature,omitempty"`
	Text                 any               `json:"text,omitempty"`
	ToolChoice           any               `json:"tool_choice,omitempty"`
	Tools                []ResponsesTools  `json:"tools,omitempty"`
	TopP                 *float64          `json:"top_p,omitempty"`
	Truncation           string            `json:"truncation,omitempty"`

	Usage *ResponsesUsage `json:"usage,omitempty"`

	rawFields                map[string]json.RawMessage   `json:"-"`
	originalKnownFieldHashes map[string][sha256.Size]byte `json:"-"`
	rawProviderJSON          []byte                       `json:"-"`
	captureProviderRawJSON   bool                         `json:"-"`
	replayProviderRawJSON    bool                         `json:"-"`
}

func (r *OpenAIResponsesResponses) SetProviderRawJSON(raw []byte) {
	if r == nil {
		return
	}
	r.rawProviderJSON = append(r.rawProviderJSON[:0], raw...)
}

// ApplyUsageAttribution 保留前后冲突和无法解析的计价维度，避免回退默认值收费。
func (r *OpenAIResponsesResponses) ApplyUsageAttribution(usage *Usage) {
	if r == nil || usage == nil {
		return
	}
	usage.MergeProviderAttribution(r.Model, r.ServiceTier)
	for _, field := range []string{"model", "service_tier"} {
		raw := r.rawFields[field]
		if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil {
			usage.AttributionConflict = true
			usage.AddBillingDiagnostic("responses_" + field + "_uninterpretable")
		}
	}
}

// DecodeCapturedProviderJSON prefers the complete current DTO, then falls
// back to the stable response evidence used by ownership and billing. The raw
// JSON remains the exact-wire delivery truth in either case.
func (r *OpenAIResponsesResponses) DecodeCapturedProviderJSON(raw []byte) error {
	if r == nil {
		return errors.New("responses response is required")
	}
	if err := json.Unmarshal(raw, r); err == nil {
		return nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("provider response must be a JSON object")
	}

	*r = OpenAIResponsesResponses{rawFields: fields}
	DecodeOptionalRawField(fields, "id", &r.ID)
	DecodeOptionalRawField(fields, "model", &r.Model)
	DecodeOptionalRawField(fields, "object", &r.Object)
	DecodeOptionalRawField(fields, "status", &r.Status)
	DecodeOptionalRawField(fields, "service_tier", &r.ServiceTier)
	DecodeOptionalRawField(fields, "store", &r.Store)
	DecodeOptionalRawField(fields, "created_at", &r.CreatedAt)
	DecodeOptionalRawField(fields, "error", &r.Error)
	DecodeOptionalRawField(fields, "incomplete_details", &r.IncompleteDetail)
	DecodeOptionalRawField(fields, "output", &r.Output)
	DecodeOptionalRawField(fields, "tools", &r.Tools)
	DecodeOptionalRawField(fields, "usage", &r.Usage)
	return nil
}

// DecodeOptionalRawField stages one observation field. Unknown or malformed
// observations do not block raw delivery and never publish a partial value.
func DecodeOptionalRawField(fields map[string]json.RawMessage, name string, destination any) bool {
	raw, ok := fields[name]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	target := reflect.ValueOf(destination)
	if target.Kind() != reflect.Pointer || target.IsNil() || target.Elem().Kind() == reflect.Invalid {
		return false
	}
	temporary := reflect.New(target.Elem().Type())
	if err := json.Unmarshal(raw, temporary.Interface()); err == nil {
		target.Elem().Set(temporary.Elem())
		return true
	}
	return false
}

func (r *OpenAIResponsesResponses) ProviderRawJSON() []byte {
	if r == nil {
		return nil
	}
	return append([]byte(nil), r.rawProviderJSON...)
}

func (r *OpenAIResponsesResponses) EnableProviderRawJSONCapture() {
	if r != nil {
		r.captureProviderRawJSON = true
	}
}

func (r *OpenAIResponsesResponses) CaptureProviderRawJSON() bool {
	return r != nil && r.captureProviderRawJSON
}

func (r *OpenAIResponsesResponses) EnableProviderRawJSONReplay() {
	if r != nil {
		r.replayProviderRawJSON = true
	}
}

func (r *OpenAIResponsesResponses) ReplayProviderRawJSON() []byte {
	if r == nil || !r.replayProviderRawJSON {
		return nil
	}
	return r.ProviderRawJSON()
}

var openAIResponsesResponseJSONFields = collectJSONFieldNames(reflect.TypeOf(OpenAIResponsesResponses{}))

func (r *OpenAIResponsesResponses) UnmarshalJSON(data []byte) error {
	type responseAlias OpenAIResponsesResponses
	var alias responseAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFields); err != nil {
		return err
	}
	*r = OpenAIResponsesResponses(alias)
	r.rawFields = rawFields
	knownJSON, err := json.Marshal(alias)
	if err != nil {
		return err
	}
	var originalKnownFields map[string]json.RawMessage
	if err := json.Unmarshal(knownJSON, &originalKnownFields); err != nil {
		return err
	}
	r.originalKnownFieldHashes = make(map[string][sha256.Size]byte, len(originalKnownFields))
	for field, raw := range originalKnownFields {
		r.originalKnownFieldHashes[field] = sha256.Sum256(raw)
	}
	return nil
}

func (r OpenAIResponsesResponses) MarshalJSON() ([]byte, error) {
	type responseAlias OpenAIResponsesResponses
	knownJSON, err := json.Marshal(responseAlias(r))
	if err != nil {
		return nil, err
	}
	var knownFields map[string]json.RawMessage
	if err := json.Unmarshal(knownJSON, &knownFields); err != nil {
		return nil, err
	}
	fields := make(map[string]json.RawMessage, len(r.rawFields)+len(knownFields))
	for field, raw := range r.rawFields {
		fields[field] = raw
	}
	for field := range openAIResponsesResponseJSONFields {
		if _, projected := knownFields[field]; !projected && !isExplicitJSONEmpty(r.rawFields[field]) {
			delete(fields, field)
		}
	}
	for field, raw := range knownFields {
		// A same-dialect response owns no provider response fields. If the typed
		// projection is unchanged, retain the original JSON for the whole field so
		// future nested unions, extension members, and JSON numbers survive without
		// teaching every nested DTO about them. Error objects deliberately remain on
		// the typed path because provider-account details have a stricter exposure
		// boundary than ordinary response extensions.
		originalHash, originallyKnown := r.originalKnownFieldHashes[field]
		if field != "error" && originallyKnown && sha256.Sum256(raw) == originalHash {
			if _, present := r.rawFields[field]; present {
				continue
			}
		}
		fields[field] = raw
	}
	return json.Marshal(fields)
}

type TextResponses struct {
	Format struct {
		Type string `json:"type"`
	} `json:"format"`
}

func (cc *OpenAIResponsesResponses) GetContent() string {
	var content strings.Builder
	for _, output := range cc.Output {
		content.WriteString(output.StringContent())
	}
	return content.String()
}

func (m ResponsesOutput) StringContent() string {
	text, _ := m.messageContentStrings()
	return text
}

func (m ResponsesOutput) messageContentStrings() (string, string) {
	if m.Type != "message" {
		return "", ""
	}

	content, ok := m.Content.(string)
	if ok {
		return content, ""
	}
	contentItems, ok := m.Content.([]ContentResponses)
	if ok {
		var text strings.Builder
		var refusal strings.Builder
		for _, contentItem := range contentItems {
			if contentItem.Text != "" {
				text.WriteString(contentItem.Text)
			}
			if contentItem.Type == ContentTypeRefusal && contentItem.Refusal != "" {
				refusal.WriteString(contentItem.Refusal)
			}
		}
		return text.String(), refusal.String()
	}
	contentList, ok := m.Content.([]any)
	if ok {
		var text strings.Builder
		var refusal strings.Builder
		for _, contentItem := range contentList {
			contentMap, ok := contentItem.(map[string]any)
			if !ok {
				continue
			}

			if subStr, ok := contentMap["text"].(string); ok && subStr != "" {
				text.WriteString(subStr)
			}
			if contentType, _ := contentMap["type"].(string); contentType == ContentTypeRefusal {
				if subStr, ok := contentMap["refusal"].(string); ok && subStr != "" {
					refusal.WriteString(subStr)
				}
			}
		}
		return text.String(), refusal.String()
	}
	return "", ""
}

func (m ResponsesOutput) GetSummaryString() string {
	if m.Type != InputTypeReasoning {
		return ""
	}

	var summary strings.Builder
	for _, item := range m.Summary {
		if item.Type == ContentTypeSummaryText {
			summary.WriteString(item.Text)
		}
	}
	return summary.String()
}

type IncompleteDetail struct {
	Reason string `json:"reason,omitempty"`
}

type ResponsesOutput struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Status  string `json:"status"`
	Role    string `json:"role,omitempty"`
	Content any    `json:"content,omitempty"`

	Queries             any                  `json:"queries,omitempty"`
	Results             any                  `json:"results,omitempty"`
	Arguments           *string              `json:"arguments,omitempty"`
	CallID              string               `json:"call_id,omitempty"`
	Name                string               `json:"name,omitempty"`
	Input               string               `json:"input,omitempty"`
	Action              any                  `json:"action,omitempty"`
	PendingSafetyChecks any                  `json:"pending_safety_checks,omitempty"`
	Summary             SummaryResponsesList `json:"summary,omitempty"`
	EncryptedContent    *string              `json:"encrypted_content,omitempty"`

	Code        any    `json:"code,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
	Outputs     any    `json:"outputs,omitempty"`
	ServerLabel any    `json:"server_label,omitempty"`
	Error       any    `json:"error,omitempty"`
	Output      any    `json:"output,omitempty"` // The output of the tool call.
	Tools       any    `json:"tools,omitempty"`  // The tools available for the tool call.

	Background    any    `json:"background,omitempty"`
	OutputFormat  any    `json:"output_format,omitempty"`
	Quality       string `json:"quality,omitempty"`
	Result        any    `json:"result,omitempty"`         // The result of the image generation call.
	Size          string `json:"size,omitempty"`           // The size of the image to be generated.
	RevisedPrompt any    `json:"revised_prompt,omitempty"` // The revised prompt for the image generation call.

	rawFields map[string]json.RawMessage `json:"-"`
}

var responsesOutputJSONFields = collectJSONFieldNames(reflect.TypeOf(ResponsesOutput{}))

func (m *ResponsesOutput) UnmarshalJSON(data []byte) error {
	type responsesOutputAlias ResponsesOutput
	type responsesOutputPayload struct {
		*responsesOutputAlias
		Arguments json.RawMessage `json:"arguments"`
		Quality   json.RawMessage `json:"quality"`
	}

	alias := responsesOutputAlias(*m)
	payload := responsesOutputPayload{responsesOutputAlias: &alias}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	*m = ResponsesOutput(alias)
	if err := json.Unmarshal(data, &m.rawFields); err != nil {
		return err
	}
	if payload.Arguments != nil {
		arguments, present, err := responsesArgumentsString(payload.Arguments)
		if err != nil {
			return fmt.Errorf("decode responses output arguments: %w", err)
		}
		if present {
			m.Arguments = &arguments
		} else {
			m.Arguments = nil
		}
	}
	if payload.Quality != nil {
		var quality string
		if err := json.Unmarshal(payload.Quality, &quality); err == nil {
			m.Quality = quality
		} else {
			// 保留未来 union 的原始字段；本地仅将已知字符串质量用于计费。
			m.Quality = ""
		}
	}
	return nil
}

func (m ResponsesOutput) MarshalJSON() ([]byte, error) {
	type responsesOutputAlias ResponsesOutput

	knownJSON, err := json.Marshal(responsesOutputAlias(m))
	if err != nil {
		return nil, err
	}
	if !isKnownResponsesOutputType(m.Type) && len(m.rawFields) > 0 {
		return json.Marshal(m.rawFields)
	}
	var knownFields map[string]json.RawMessage
	if err := json.Unmarshal(knownJSON, &knownFields); err != nil {
		return nil, err
	}
	fields := make(map[string]json.RawMessage, len(m.rawFields)+len(knownFields))
	for field, raw := range m.rawFields {
		fields[field] = raw
	}
	for field := range responsesOutputJSONFields {
		if _, projected := knownFields[field]; !projected && !isExplicitJSONEmpty(m.rawFields[field]) {
			if field == "quality" && isUnknownResponsesOutputQuality(m.rawFields[field]) {
				continue
			}
			delete(fields, field)
		}
	}
	for field, raw := range knownFields {
		fields[field] = raw
	}
	if m.Type == InputTypeReasoning {
		summary, err := json.Marshal(summaryResponsesForMarshal(m.Summary))
		if err != nil {
			return nil, err
		}
		fields["summary"] = summary
	}
	if m.Type == InputTypeCustomToolCall {
		input, err := json.Marshal(m.Input)
		if err != nil {
			return nil, err
		}
		fields["input"] = input
	}
	return json.Marshal(fields)
}

func isUnknownResponsesOutputQuality(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] != '"' && !bytes.Equal(trimmed, []byte("null"))
}

func isExplicitJSONEmpty(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return !typed
	case string:
		return typed == ""
	case json.Number:
		number, err := typed.Float64()
		return err == nil && number == 0
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func isKnownResponsesOutputType(outputType string) bool {
	switch strings.TrimSpace(outputType) {
	case InputTypeMessage,
		InputTypeFileSearchCall,
		InputTypeComputerCall,
		InputTypeWebSearchCall,
		InputTypeComputerCallOutput,
		InputTypeFunctionCall,
		InputTypeFunctionCallOutput,
		InputTypeCustomToolCall,
		InputTypeCustomToolCallOutput,
		InputTypeReasoning,
		InputTypeImageGenerationCall,
		InputTypeCodeInterpreterCall,
		InputTypeShellCall,
		InputTypeShellCallOutput,
		InputTypeLocalShellCall,
		InputTypeLocalShellCallOutput,
		InputTypeMCPListTools,
		InputTypeMCPApprovalRequest,
		InputTypeMCPApprovalResponse,
		InputTypeMCPCall:
		return true
	default:
		return false
	}
}

func responsesArgumentsString(data json.RawMessage) (string, bool, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", false, nil
	}

	if trimmed[0] != '"' {
		return string(trimmed), true, nil
	}

	var arguments string
	if err := json.Unmarshal(trimmed, &arguments); err != nil {
		return "", false, err
	}
	return arguments, true, nil
}

type ResponsesOutputToolCall struct {
	ID string `json:"id"`
}

type OpenAIResponsesStreamResponses struct {
	Type           string `json:"type"`            // 始终存在
	SequenceNumber int    `json:"sequence_number"` // 始终存在
	// response.created  第一条数据
	// response.in_progress 第二条数据
	// response.completed  最后一条数据
	// response.failed 内容过滤返回
	// response.incomplete 内容不完整返回
	Response *OpenAIResponsesResponses `json:"response,omitempty"`

	// response.output_item.added response.output_item.done 在新项目的前后, 函数调用时，会在added先输出需要调用的函数名， done 会输出完整的项目数据
	OutputIndex *int             `json:"output_index,omitempty"` // 当前项目的索引，从0开始
	Item        *ResponsesOutput `json:"item,omitempty"`

	ItemID string `json:"item_id,omitempty"` // 项目的ID，和response.output_item 中的Item.ID一致

	// response.content_part.added response.content_part.done 在output_text的前后，done 会输出完整的output_text
	ContentIndex *int              `json:"content_index,omitempty"`
	Part         *ContentResponses `json:"part,omitempty"`

	// response.output_text.delta  response.output_text.done 文本输出
	Delta any     `json:"delta,omitempty"` // 仅在response.output_text.delta / response.refusal.delta / response.function_call_arguments.delta存在
	Text  *string `json:"text,omitempty"`  // 仅在response.output_text.done存在

	// response.refusal.delta response.refusal.done 拒绝输出
	Refusal *string `json:"refusal,omitempty"` // 仅在response.refusal.done存在

	// response.function_call_arguments.delta response.function_call_arguments.done
	Arguments any `json:"arguments,omitempty"` // 仅在response.function_call_arguments.done存在

	// response.reasoning_summary_part.added response.reasoning_summary_part.done 在reasoning_summary_text的前后，done 会输出完整的reasoning_summary_text
	SummaryIndex *int `json:"summary_index,omitempty"` // 当前摘要的索引，从0开始

	// response.reasoning_summary_text.delta response.reasoning_summary_text.done

	//  response.image_generation_call.completed response.image_generation_call.generating response.image_generation_call.in_progress response.image_generation_call.partial_image
	PartialImageIndex *int    `json:"partial_image_index,omitempty"` // 当前图片的索引，从0开始
	PartialImageB64   *string `json:"partial_image_b64,omitempty"`   // 仅在response.image_generation_call.partial_image存在

	// response.mcp_call.arguments.delta 时 Delta 是对象  response.mcp_call.arguments.done 时 arguments 是对象

	// response.reasoning.delta 时 delta 是对象 {"text": "reasoning text"} response.reasoning.done 在text中显示完整的推理内容

	// response.reasoning_summary.delta response.reasoning_summary.done

	// error
	Code    *string `json:"code,omitempty"`    // 错误代码
	Message *string `json:"message,omitempty"` // 错误信息
	Param   *any    `json:"param,omitempty"`   // 错误参数
}

type ResponsesUsage struct {
	InputTokens          int                                `json:"input_tokens"`
	OutputTokens         int                                `json:"output_tokens"`
	TotalTokens          int                                `json:"total_tokens"`
	OutputTokensDetails  *ResponsesUsageOutputTokensDetails `json:"output_tokens_details"`
	InputTokensDetails   *ResponsesUsageInputTokensDetails  `json:"input_tokens_details"`
	ProviderReported     bool                               `json:"-"`
	ProviderTokenFields  map[string]bool                    `json:"-"`
	providerWireObserved bool                               `json:"-"`
}

func (u *ResponsesUsage) UnmarshalJSON(data []byte) error {
	type responsesUsageAlias ResponsesUsage
	var decoded responsesUsageAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = ResponsesUsage(decoded)
	u.providerWireObserved = true
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) == nil {
		u.ProviderTokenFields = make(map[string]bool)
		for _, field := range []string{"input_tokens", "output_tokens", "total_tokens"} {
			if validUsageInteger(fields[field]) {
				u.ProviderTokenFields[field] = true
			}
		}
		markUsageDetailPresence(u.ProviderTokenFields, fields["input_tokens_details"], true)
		markUsageDetailPresence(u.ProviderTokenFields, fields["output_tokens_details"], false)
	}
	return nil
}

func (u *ResponsesUsage) MarkProviderReported() {
	if u == nil {
		return
	}
	u.ProviderReported = true
	if len(u.ProviderTokenFields) == 0 && !u.providerWireObserved {
		u.ProviderTokenFields = map[string]bool{"input_tokens": true, "output_tokens": true, "total_tokens": true}
	}
}

type ResponsesUsageOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
	TextTokens      int `json:"text_tokens,omitempty"`
	ImageTokens     int `json:"image_tokens,omitempty"`
}

type ResponsesUsageInputTokensDetails struct {
	AudioTokens       int `json:"audio_tokens,omitempty"`
	CachedTokens      int `json:"cached_tokens"`
	CachedReadTokens  int `json:"cached_read_tokens,omitempty"`
	CacheWriteTokens  int `json:"cache_write_tokens,omitempty"`
	CachedWriteTokens int `json:"cached_write_tokens,omitempty"`
	TextTokens        int `json:"text_tokens,omitempty"`
	ImageTokens       int `json:"image_tokens,omitempty"`
}

// MarshalJSON keeps provider-specific cache evidence internal while preserving
// the cache fields defined by the public Responses wire. Exact-wire relays
// preserve provider JSON before this typed representation is involved.
func (d ResponsesUsageInputTokensDetails) MarshalJSON() ([]byte, error) {
	type responsesUsageInputTokensDetailsWire struct {
		AudioTokens      int `json:"audio_tokens,omitempty"`
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
		TextTokens       int `json:"text_tokens,omitempty"`
		ImageTokens      int `json:"image_tokens,omitempty"`
	}
	return json.Marshal(responsesUsageInputTokensDetailsWire{
		AudioTokens:      d.AudioTokens,
		CachedTokens:     d.CachedTokens,
		CacheWriteTokens: d.CacheWriteTokens,
		TextTokens:       d.TextTokens,
		ImageTokens:      d.ImageTokens,
	})
}

func GetResponsesExtraBilling(response *OpenAIResponsesResponses) map[string]ExtraBilling {
	if response == nil || len(response.Output) == 0 {
		return nil
	}

	usage := &Usage{}
	ApplyResponsesExtraBilling(response, usage)
	return cloneExtraBillingMap(usage.ExtraBilling)
}

func GetResponsesBillingDiagnostics(response *OpenAIResponsesResponses) map[string]bool {
	if response == nil || len(response.Output) == 0 {
		return nil
	}
	usage := &Usage{}
	ApplyResponsesExtraBilling(response, usage)
	return cloneBillingDiagnostics(usage.BillingDiagnostics)
}

func ApplyResponsesExtraBilling(response *OpenAIResponsesResponses, usage *Usage) {
	ApplyResponsesExtraBillingWithImagePartialCounts(response, usage, nil)
}

// ApplyResponsesExtraBillingWithImagePartialCounts applies tool billing from a
// completed Responses object. Image partials are streaming output evidence, so
// callers that observed the stream can supply the actual distinct count for
// each output item. A nil resolver means that no partial image was observed.
func ApplyResponsesExtraBillingWithImagePartialCounts(response *OpenAIResponsesResponses, usage *Usage, imagePartialCount func(output *ResponsesOutput, outputIndex int) int) {
	if response == nil || usage == nil {
		return
	}
	imageGenerationType := ResponsesImageGenerationBillingType(response)
	searchServiceType, searchType := ResponsesWebSearchBilling(response)
	if searchServiceType == "" {
		searchServiceType = APIToolTypeWebSearchPreview
	}
	if searchType == "" {
		searchType = "medium"
	}
	for outputIndex := range response.Output {
		output := &response.Output[outputIndex]
		switch output.Type {
		case InputTypeWebSearchCall:
			bill, unknownAction := ShouldBillResponsesWebSearch(*output)
			if unknownAction {
				usage.AddBillingDiagnostic("web_search_action_unknown")
			}
			if !bill {
				continue
			}
			usage.IncProviderExtraBilling(searchServiceType, searchType)
		case InputTypeImageGenerationCall:
			if ShouldBillResponsesImageGenerationResponse(response) {
				applyResponsesImageGenerationOutputBilling(usage, output, outputIndex, imageGenerationType, imagePartialCount)
			}
		}
	}
}

func ResponsesWebSearchBilling(response *OpenAIResponsesResponses) (serviceType, billingType string) {
	if response == nil {
		return "", ""
	}
	for _, tool := range response.Tools {
		toolType := strings.TrimSpace(tool.Type)
		if !IsResponsesWebSearchToolType(toolType) {
			continue
		}
		toolBillingType := strings.TrimSpace(tool.SearchContextSize)
		if toolBillingType == "" {
			toolBillingType = "medium"
		}
		if toolType == APIToolTypeWebSearch {
			if serviceType == "" {
				serviceType = APIToolTypeWebSearch
				billingType = toolBillingType
			}
			continue
		}
		// Preview dominates an ambiguous mixed tool list because output events do
		// not identify which search declaration produced the call.
		if serviceType != APIToolTypeWebSearchPreview {
			billingType = toolBillingType
		}
		serviceType = APIToolTypeWebSearchPreview
	}
	return serviceType, billingType
}

func ApplyResponsesImageGenerationBillingWithPartialCounts(response *OpenAIResponsesResponses, usage *Usage, imagePartialCount func(output *ResponsesOutput, outputIndex int) int) {
	if response == nil || usage == nil || !ShouldBillResponsesImageGenerationResponse(response) {
		return
	}
	imageGenerationType := ResponsesImageGenerationBillingType(response)
	for outputIndex := range response.Output {
		output := &response.Output[outputIndex]
		if output.Type == InputTypeImageGenerationCall {
			applyResponsesImageGenerationOutputBilling(usage, output, outputIndex, imageGenerationType, imagePartialCount)
		}
	}
}

func applyResponsesImageGenerationOutputBilling(usage *Usage, output *ResponsesOutput, outputIndex int, imageGenerationType string, imagePartialCount func(output *ResponsesOutput, outputIndex int) int) {
	if usage == nil || output == nil || !ShouldBillResponsesImageGeneration(*output) {
		return
	}
	partialImages := 0
	if imagePartialCount != nil {
		partialImages = imagePartialCount(output, outputIndex)
		if partialImages < 0 {
			return
		}
	}
	usage.IncProviderExtraBilling(APIToolTypeImageGeneration, ResponsesImageGenerationOutputBillingType(imageGenerationType, output, partialImages))
}

// ResponsesImageGenerationBillingType captures the pricing-relevant image tool
// configuration carried by a Responses object. The output item can later refine
// quality and size when the provider resolves an "auto" request.
func ResponsesImageGenerationBillingType(response *OpenAIResponsesResponses) string {
	if response == nil {
		return ""
	}
	for _, tool := range response.Tools {
		if tool.Type != APIToolTypeImageGeneration {
			continue
		}
		// partial_images is a request limit, not evidence that the provider
		// emitted that many partial image events.
		return BuildResponsesImageGenerationBillingType(tool.Model, tool.Quality, tool.Size, 0)
	}
	return ""
}

func BuildResponsesImageGenerationBillingType(model, quality, size string, partialImages int) string {
	if partialImages < 0 {
		partialImages = 0
	}
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(model)),
		strings.ToLower(strings.TrimSpace(quality)),
		strings.ToLower(strings.TrimSpace(size)),
		strconv.Itoa(partialImages),
	}, "|")
}

func ResponsesImageGenerationOutputBillingType(configuredType string, output *ResponsesOutput, observedPartialImages ...int) string {
	model, quality, size, partialImages := "", "", "", 0
	parts := strings.Split(configuredType, "|")
	if len(parts) == 4 {
		model, quality, size = parts[0], parts[1], parts[2]
		if parsed, err := strconv.Atoi(parts[3]); err == nil && parsed > 0 {
			partialImages = parsed
		}
	}
	if output != nil {
		if value := strings.TrimSpace(output.Quality); value != "" {
			quality = value
		}
		if value := strings.TrimSpace(output.Size); value != "" {
			size = value
		}
	}
	if len(observedPartialImages) > 0 {
		partialImages = observedPartialImages[0]
		if partialImages < 0 {
			partialImages = 0
		}
	}
	if configuredType == "" && model == "" && partialImages == 0 && quality != "" && size != "" {
		return strings.ToLower(strings.TrimSpace(quality)) + "-" + strings.ToLower(strings.TrimSpace(size))
	}
	return BuildResponsesImageGenerationBillingType(model, quality, size, partialImages)
}

func ShouldBillResponsesImageGenerationResponse(response *OpenAIResponsesResponses) bool {
	if response == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(response.Status)) {
	case "failed", "cancelled", "canceled", "incomplete":
		return false
	default:
		return true
	}
}

func ShouldBillResponsesImageGeneration(output ResponsesOutput) bool {
	switch strings.ToLower(strings.TrimSpace(output.Status)) {
	case "completed":
		return true
	case "failed", "cancelled", "canceled", "incomplete":
		return false
	default:
		return output.Result != nil
	}
}

func ShouldBillResponsesWebSearch(output ResponsesOutput) (bill bool, unknownAction bool) {
	if !strings.EqualFold(strings.TrimSpace(output.Status), "completed") {
		return false, false
	}
	actionType := responsesActionType(output.Action)
	switch actionType {
	case "search":
		return true, false
	case "open_page", "find_in_page":
		return false, false
	default:
		// A completed provider-originated web_search_call proves one billable
		// action even when a future/omitted subtype cannot be classified.
		return true, true
	}
}

func responsesActionType(action any) string {
	if action == nil {
		return ""
	}
	if object, ok := action.(map[string]any); ok {
		if actionType, ok := object["type"].(string); ok {
			return strings.ToLower(strings.TrimSpace(actionType))
		}
	}
	raw, err := json.Marshal(action)
	if err != nil {
		return ""
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(envelope.Type))
}

func (u *ResponsesUsage) ToOpenAIUsage() *Usage {
	if u == nil {
		return nil
	}
	usage := &Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
		ProviderTokenFields: map[string]bool{
			"prompt_tokens":     u.ProviderTokenFields["input_tokens"],
			"completion_tokens": u.ProviderTokenFields["output_tokens"],
			"total_tokens":      u.ProviderTokenFields["total_tokens"],
		},
		providerWireObserved: u.providerWireObserved,
	}
	for key, present := range u.ProviderTokenFields {
		if key != "input_tokens" && key != "output_tokens" && key != "total_tokens" {
			usage.ProviderTokenFields[key] = present
		}
	}

	if u.OutputTokensDetails != nil {
		usage.CompletionTokensDetails.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
		usage.CompletionTokensDetails.TextTokens = u.OutputTokensDetails.TextTokens
		usage.CompletionTokensDetails.ImageTokens = u.OutputTokensDetails.ImageTokens
	}

	if u.InputTokensDetails != nil {
		usage.PromptTokensDetails.AudioTokens = u.InputTokensDetails.AudioTokens
		usage.PromptTokensDetails.CachedTokens = u.InputTokensDetails.CachedTokens
		usage.PromptTokensDetails.CachedReadTokens = u.InputTokensDetails.CachedReadTokens
		usage.PromptTokensDetails.CacheWriteTokens = u.InputTokensDetails.CacheWriteTokens
		usage.PromptTokensDetails.CachedWriteTokens = u.InputTokensDetails.CachedWriteTokens
		usage.PromptTokensDetails.TextTokens = u.InputTokensDetails.TextTokens
		usage.PromptTokensDetails.ImageTokens = u.InputTokensDetails.ImageTokens
	}
	if u.ProviderReported {
		usage.MarkProviderReported()
	}

	return usage
}

func (u *Usage) ToResponsesUsage() *ResponsesUsage {
	if u == nil {
		return nil
	}

	responsesUsage := &ResponsesUsage{
		InputTokens:      u.PromptTokens,
		OutputTokens:     u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		ProviderReported: u.ProviderReported,
	}

	if u.CompletionTokensDetails.ReasoningTokens > 0 {
		responsesUsage.OutputTokensDetails = &ResponsesUsageOutputTokensDetails{
			ReasoningTokens: u.CompletionTokensDetails.ReasoningTokens,
		}
	}

	responsesUsage.InputTokensDetails = &ResponsesUsageInputTokensDetails{
		AudioTokens:       u.PromptTokensDetails.AudioTokens,
		CachedTokens:      u.PromptTokensDetails.CachedTokens,
		CachedReadTokens:  u.PromptTokensDetails.CachedReadTokens,
		CacheWriteTokens:  u.PromptTokensDetails.CacheWriteTokens,
		CachedWriteTokens: u.PromptTokensDetails.CachedWriteTokens,
		TextTokens:        u.PromptTokensDetails.TextTokens,
		ImageTokens:       u.PromptTokensDetails.ImageTokens,
	}

	return responsesUsage
}

func ConvertResponsesStatusToChat(status string) string {
	switch status {
	case ResponseStatusFailed:
		return FinishReasonContentFilter
	case ResponseStatusIncomplete:
		return FinishReasonLength
	default:
		return FinishReasonStop
	}
}

func ConvertChatStatusToResponses(status string) string {
	switch status {
	case FinishReasonContentFilter:
		return ResponseStatusFailed
	case FinishReasonLength:
		return ResponseStatusIncomplete
	default:
		return ResponseStatusCompleted
	}
}

func (cc *ChatCompletionResponse) ToResponses(request *OpenAIResponsesRequest) *OpenAIResponsesResponses {
	text := any(TextResponses{
		Format: struct {
			Type string `json:"type"`
		}{
			Type: "text",
		},
	})
	if request.Text != nil {
		text = request.Text
	}

	res := &OpenAIResponsesResponses{
		CreatedAt:            cc.Created,
		ID:                   cc.ID,
		Model:                cc.Model,
		Object:               "response",
		Usage:                cc.Usage.ToResponsesUsage(),
		Text:                 text,
		MaxOutputTokens:      request.MaxOutputTokens,
		MaxToolCalls:         request.MaxToolCalls,
		Background:           request.Background,
		Conversation:         request.Conversation,
		Instructions:         request.Instructions,
		Metadata:             request.Metadata,
		ParallelToolCalls:    request.ParallelToolCalls,
		PreviousResponseID:   request.PreviousResponseID,
		Prompt:               request.Prompt,
		PromptCacheKey:       request.PromptCacheKey,
		PromptCacheRetention: request.PromptCacheRetention,
		Reasoning:            request.Reasoning,
		Temperature:          request.Temperature,
		SafetyIdentifier:     request.SafetyIdentifier,
		ServiceTier:          cc.ServiceTier,
		ProcessingClass:      request.ProcessingClass,
		Store:                request.Store,
		ToolChoice:           request.ToolChoice,
		TopP:                 request.TopP,
		Truncation:           request.Truncation,
		Tools:                request.Tools,
	}

	status := ResponseStatusCompleted

	outputs := make([]ResponsesOutput, 0, len(cc.Choices))
	for _, choice := range cc.Choices {
		status = ConvertChatStatusToResponses(choice.FinishReason)

		// Chat responses may carry assistant content and tool calls together.
		// finish_reason describes why generation stopped; it is not a union tag.
		if choice.Message.Audio == nil {
			content := make([]ContentResponses, 0)

			if choice.Message.Refusal != "" {
				content = append(content, ContentResponses{
					Type:    ContentTypeRefusal,
					Refusal: choice.Message.Refusal,
				})
			}

			if choice.Message.ReasoningContent != "" {
				outputs = append(outputs, ResponsesOutput{
					Type:   InputTypeReasoning,
					ID:     fmt.Sprintf("msg_%s", utils.GetRandomString(48)),
					Status: ResponseStatusCompleted,
					Summary: SummaryResponsesList{
						{
							Type: "summary_text",
							Text: choice.Message.ReasoningContent,
						},
					},
				})
			}

			chatContent, ok := choice.Message.Content.(string)
			if ok && chatContent != "" {
				content = append(content, ContentResponses{
					Type: ContentTypeOutputText,
					Text: chatContent,
				})
			}

			if len(content) > 0 {
				outputs = append(outputs, ResponsesOutput{
					Type:    InputTypeMessage,
					ID:      fmt.Sprintf("msg_%s", utils.GetRandomString(48)),
					Role:    ChatMessageRoleAssistant,
					Status:  status,
					Content: content,
				})
			}
		}

		for _, tool := range choice.Message.ToolCalls {
			if tool == nil {
				continue
			}
			if tool.Type == ToolChoiceTypeCustom && tool.Custom != nil {
				outputs = append(outputs, ResponsesOutput{
					Type:   InputTypeCustomToolCall,
					ID:     fmt.Sprintf("ctc_%s", utils.GetRandomString(48)),
					Status: ResponseStatusCompleted,
					CallID: tool.Id,
					Name:   tool.Custom.Name,
					Input:  tool.Custom.Input,
				})
				continue
			}
			if tool.Function == nil {
				continue
			}
			outputs = append(outputs, ResponsesOutput{
				Type:      InputTypeFunctionCall,
				ID:        fmt.Sprintf("fc_%s", utils.GetRandomString(48)),
				Status:    ResponseStatusCompleted,
				CallID:    tool.Id,
				Name:      tool.Function.Name,
				Arguments: &tool.Function.Arguments,
			})
		}
	}

	res.Status = status
	res.Output = outputs

	return res
}

func (r *OpenAIResponsesResponses) ToChat() *ChatCompletionResponse {
	resp := &ChatCompletionResponse{
		Created:     r.CreatedAt,
		ID:          r.ID,
		Model:       r.Model,
		Object:      "chat.completion",
		ServiceTier: r.ServiceTier,
		Usage:       r.Usage.ToOpenAIUsage(),
		Choices:     make([]ChatCompletionChoice, 0),
	}

	choice := ChatCompletionChoice{
		Message: ChatCompletionMessage{
			Role: ChatMessageRoleAssistant,
		},
		FinishReason: FinishReasonStop,
	}

	for _, output := range r.Output {
		switch output.Type {
		case InputTypeMessage:
			choice.Message.Content, choice.Message.Refusal = output.messageContentStrings()
		case InputTypeReasoning:
			choice.Message.ReasoningContent = output.GetSummaryString()
		case InputTypeFunctionCall:
			if choice.Message.ToolCalls == nil {
				choice.Message.ToolCalls = make([]*ChatCompletionToolCalls, 0)
			}
			arguments := ""
			if output.Arguments != nil {
				arguments = *output.Arguments
			}
			choice.Message.ToolCalls = append(choice.Message.ToolCalls, &ChatCompletionToolCalls{
				Id:   output.CallID,
				Type: "function",
				Function: &ChatCompletionToolCallsFunction{
					Name:      output.Name,
					Arguments: arguments,
				},
			})
			choice.FinishReason = FinishReasonToolCalls
		case InputTypeCustomToolCall:
			if choice.Message.ToolCalls == nil {
				choice.Message.ToolCalls = make([]*ChatCompletionToolCalls, 0)
			}
			choice.Message.ToolCalls = append(choice.Message.ToolCalls, &ChatCompletionToolCalls{
				Id:   output.CallID,
				Type: ToolChoiceTypeCustom,
				Custom: &ChatCompletionToolCallsCustom{
					Name:  output.Name,
					Input: output.Input,
				},
			})
			choice.FinishReason = FinishReasonToolCalls
		}

		if output.Status == ResponseStatusFailed || output.Status == ResponseStatusIncomplete {
			choice.FinishReason = ConvertResponsesStatusToChat(output.Status)
		}
	}

	resp.Choices = append(resp.Choices, choice)

	return resp
}
