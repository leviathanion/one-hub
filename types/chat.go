package types

import (
	"encoding/json"
	"strings"
)

const (
	ContentTypeText     = "text"
	ContentTypeImageURL = "image_url"
	ContentTypeFile     = "file"
)

const (
	FinishReasonStop          = "stop"
	FinishReasonLength        = "length"
	FinishReasonFunctionCall  = "function_call"
	FinishReasonToolCalls     = "tool_calls"
	FinishReasonContentFilter = "content_filter"
	FinishReasonNull          = "null"
)

const (
	ChatMessageRoleSystem    = "system"
	ChatMessageRoleDeveloper = "developer"
	ChatMessageRoleUser      = "user"
	ChatMessageRoleAssistant = "assistant"
	ChatMessageRoleFunction  = "function"
	ChatMessageRoleTool      = "tool"
)

const (
	ToolChoiceTypeFunction = "function"
	ToolChoiceTypeCustom   = "custom"
	ToolChoiceTypeAuto     = "auto"
	ToolChoiceTypeNone     = "none"
	ToolChoiceTypeRequired = "required"
)

type ChatCompletionToolCallsFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type ChatCompletionToolCallsCustom struct {
	Name  string `json:"name,omitempty"`
	Input string `json:"input"`
}

func (f *ChatCompletionToolCallsFunction) UnmarshalJSON(data []byte) error {
	type chatCompletionToolCallsFunctionAlias ChatCompletionToolCallsFunction
	type chatCompletionToolCallsFunctionPayload struct {
		*chatCompletionToolCallsFunctionAlias
		Arguments json.RawMessage `json:"arguments"`
	}

	alias := chatCompletionToolCallsFunctionAlias(*f)
	payload := chatCompletionToolCallsFunctionPayload{chatCompletionToolCallsFunctionAlias: &alias}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	*f = ChatCompletionToolCallsFunction(alias)
	if payload.Arguments != nil {
		arguments, _, err := responsesArgumentsString(payload.Arguments)
		if err != nil {
			return err
		}
		f.Arguments = arguments
	}
	return nil
}

type ChatCompletionToolCalls struct {
	Id       string                           `json:"id,omitempty"`
	Type     string                           `json:"type,omitempty"`
	Function *ChatCompletionToolCallsFunction `json:"function,omitempty"`
	Custom   *ChatCompletionToolCallsCustom   `json:"custom,omitempty"`
	Index    int                              `json:"index"`
}

type ChatCompletionMessage struct {
	Role             string                           `json:"role"`
	Content          any                              `json:"content,omitempty"`
	Refusal          string                           `json:"refusal,omitempty"`
	ReasoningContent string                           `json:"reasoning_content,omitempty"`
	Reasoning        string                           `json:"reasoning,omitempty"`
	Name             *string                          `json:"name,omitempty"`
	FunctionCall     *ChatCompletionToolCallsFunction `json:"function_call,omitempty"`
	ToolCalls        []*ChatCompletionToolCalls       `json:"tool_calls,omitempty"`
	ToolCallID       string                           `json:"tool_call_id,omitempty"`
	Audio            any                              `json:"audio,omitempty"`
	Annotations      any                              `json:"annotations,omitempty"`
	Image            []MultimediaData                 `json:"image,omitempty"`
	Images           []ChatMessagePart                `json:"images,omitempty"`
	CacheControl     any                              `json:"cache_control,omitempty"`
}

func (m ChatCompletionMessage) StringContent() string {
	content, ok := m.Content.(string)
	if ok {
		return content
	}
	contentList, ok := m.Content.([]any)
	if ok {
		var contentStr string
		for _, contentItem := range contentList {
			contentMap, ok := contentItem.(map[string]any)
			if !ok {
				continue
			}

			if subStr, ok := contentMap["text"].(string); ok && subStr != "" {
				contentStr += subStr
			}
		}
		return contentStr
	}
	return ""
}

func (m ChatCompletionMessage) ParseContent() []ChatMessagePart {
	var contentList []ChatMessagePart
	content, ok := m.Content.(string)
	if ok {
		contentList = append(contentList, ChatMessagePart{
			Type: ContentTypeText,
			Text: content,
		})
		return contentList
	}
	msgJson, err := json.Marshal(m.Content)
	if err != nil {
		return contentList
	}

	json.Unmarshal(msgJson, &contentList)
	return contentList
}

// 将FunctionCall转换为ToolCalls
func (m *ChatCompletionMessage) FuncToToolCalls() {
	if m.ToolCalls != nil {
		return
	}
	if m.FunctionCall != nil {
		m.ToolCalls = []*ChatCompletionToolCalls{
			{
				Id:       m.FunctionCall.Name,
				Type:     ChatMessageRoleFunction,
				Function: m.FunctionCall,
			},
		}
		m.FunctionCall = nil
	}
}

// 将ToolCalls转换为FunctionCall
func (m *ChatCompletionMessage) ToolToFuncCalls() {
	if m.FunctionCall != nil {
		return
	}
	if len(m.ToolCalls) > 0 && m.ToolCalls[0] != nil && m.ToolCalls[0].Function != nil {
		m.FunctionCall = &ChatCompletionToolCallsFunction{
			Name:      m.ToolCalls[0].Function.Name,
			Arguments: m.ToolCalls[0].Function.Arguments,
		}
		m.ToolCalls = nil
	}
}

func (m *ChatCompletionMessage) IsSystemRole() bool {
	return m.Role == ChatMessageRoleSystem || m.Role == ChatMessageRoleDeveloper
}

type ChatMessageImageURL struct {
	URL    string `json:"url,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type ChatMessagePart struct {
	Type       string               `json:"type,omitempty"`
	Text       string               `json:"text,omitempty"`
	ImageURL   *ChatMessageImageURL `json:"image_url,omitempty"`
	InputAudio *InputAudio          `json:"input_audio,omitempty"`
	Refusal    string               `json:"refusal,omitempty"`

	File *ChatMessageFile `json:"file,omitempty"`
}

type InputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type ChatMessageFile struct {
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

type ChatCompletionResponseFormat struct {
	Type       string            `json:"type,omitempty"`
	JsonSchema *FormatJsonSchema `json:"json_schema,omitempty"`
}

type FormatJsonSchema struct {
	Description string `json:"description,omitempty"`
	Name        string `json:"name"`
	Schema      any    `json:"schema,omitempty"`
	Strict      any    `json:"strict,omitempty"`
}

type ChatCompletionRequest struct {
	Model               string                        `json:"model" binding:"required"`
	Messages            []ChatCompletionMessage       `json:"messages" binding:"required"`
	System              any                           `json:"system,omitempty"`
	MaxTokens           int                           `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                           `json:"max_completion_tokens,omitempty"`
	Temperature         *float64                      `json:"temperature,omitempty"`
	TopP                *float64                      `json:"top_p,omitempty"`
	TopK                *float64                      `json:"top_k,omitempty"`
	N                   *int                          `json:"n,omitempty"`
	Stream              bool                          `json:"stream,omitempty"`
	StreamOptions       *StreamOptions                `json:"stream_options,omitempty"`
	Stop                any                           `json:"stop,omitempty"`
	PresencePenalty     *float64                      `json:"presence_penalty,omitempty"`
	ResponseFormat      *ChatCompletionResponseFormat `json:"response_format,omitempty"`
	Seed                *int                          `json:"seed,omitempty"`
	FrequencyPenalty    *float64                      `json:"frequency_penalty,omitempty"`
	LogitBias           any                           `json:"logit_bias,omitempty"`
	LogProbs            *bool                         `json:"logprobs,omitempty"`
	TopLogProbs         int                           `json:"top_logprobs,omitempty"`
	User                string                        `json:"user,omitempty"`
	Functions           []*ChatCompletionFunction     `json:"functions,omitempty"`
	FunctionCall        any                           `json:"function_call,omitempty"`
	Tools               []*ChatCompletionTool         `json:"tools,omitempty"`
	ToolChoice          any                           `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool                         `json:"parallel_tool_calls,omitempty"`
	Modalities          []string                      `json:"modalities,omitempty"`
	Audio               *ChatAudio                    `json:"audio,omitempty"`
	ReasoningEffort     *string                       `json:"reasoning_effort,omitempty"`
	Prediction          any                           `json:"prediction,omitempty"`
	WebSearchOptions    *WebSearchOptions             `json:"web_search_options,omitempty"`
	ServiceTier         string                        `json:"service_tier,omitempty"`
	ProcessingClass     string                        `json:"processing_class,omitempty"`
	SafetyIdentifier    string                        `json:"safety_identifier,omitempty"`
	PromptCacheKey      any                           `json:"prompt_cache_key,omitempty"`
	Verbosity           string                        `json:"verbosity,omitempty"`    // 用于控制输出的详细程度
	Store               *bool                         `json:"store,omitempty"`        // ChatGPT 是否存储对话（Codex 要求设置为 false）
	Instructions        *string                       `json:"instructions,omitempty"` // Codex CLI 系统提示词

	Reasoning *ChatReasoning `json:"reasoning,omitempty"`
}

type ChatReasoning struct {
	MaxTokens int     `json:"-"`
	Effort    string  `json:"effort,omitempty"`
	Summary   *string `json:"summary,omitempty"`

	maxTokensPresent bool
}

// HasMaxTokens reports whether max_tokens was explicitly supplied. The
// distinction matters because some provider dialects use zero as a real value.
func (r *ChatReasoning) HasMaxTokens() bool {
	return r != nil && (r.maxTokensPresent || r.MaxTokens != 0)
}

// SetMaxTokens records a programmatically supplied max_tokens value, including
// zero. JSON decoding records the same presence information automatically.
func (r *ChatReasoning) SetMaxTokens(maxTokens int) {
	if r == nil {
		return
	}
	r.MaxTokens = maxTokens
	r.maxTokensPresent = true
}

func (r ChatReasoning) MarshalJSON() ([]byte, error) {
	type chatReasoningWire struct {
		MaxTokens *int    `json:"max_tokens,omitempty"`
		Effort    string  `json:"effort,omitempty"`
		Summary   *string `json:"summary,omitempty"`
	}

	var maxTokens *int
	if r.HasMaxTokens() {
		value := r.MaxTokens
		maxTokens = &value
	}
	return json.Marshal(chatReasoningWire{
		MaxTokens: maxTokens,
		Effort:    r.Effort,
		Summary:   r.Summary,
	})
}

func (r *ChatReasoning) UnmarshalJSON(data []byte) error {
	type chatReasoningWire struct {
		MaxTokens *int    `json:"max_tokens"`
		Effort    string  `json:"effort"`
		Summary   *string `json:"summary"`
	}

	var wire chatReasoningWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*r = ChatReasoning{Effort: wire.Effort, Summary: wire.Summary}
	if wire.MaxTokens != nil {
		r.SetMaxTokens(*wire.MaxTokens)
	}
	return nil
}

// NormalizeReasoning 归一化 ReasoningEffort 和 Reasoning 字段，确保两者一致。
// 优先级：Reasoning > ReasoningEffort
// - 如果 Reasoning 存在，以 Reasoning 为准，并回填 ReasoningEffort
// - 如果 Reasoning 不存在但 ReasoningEffort 存在，从 ReasoningEffort 构造 Reasoning
func (r *ChatCompletionRequest) NormalizeReasoning() {
	if r.Reasoning != nil {
		if r.Reasoning.Effort != "" && r.ReasoningEffort == nil {
			r.ReasoningEffort = &r.Reasoning.Effort
		}
		return
	}

	r.Reasoning = r.EffectiveReasoning()
}

// EffectiveReasoning returns the adapter-facing reasoning configuration
// without changing the Chat wire representation.
func (r *ChatCompletionRequest) EffectiveReasoning() *ChatReasoning {
	if r == nil {
		return nil
	}
	if r.Reasoning != nil {
		return r.Reasoning
	}
	if r.ReasoningEffort == nil {
		return nil
	}
	return &ChatReasoning{Effort: *r.ReasoningEffort}
}

type WebSearchOptions struct {
	SearchContextSize string `json:"search_context_size,omitempty"`
	UserLocation      any    `json:"user_location,omitempty"`
}

func (r ChatCompletionRequest) ParseToolChoice() (toolType, toolFunc string) {
	if choice, ok := r.ToolChoice.(map[string]any); ok {
		if function, ok := choice["function"].(map[string]any); ok {
			toolType = ToolChoiceTypeFunction
			toolFunc = function["name"].(string)
		}
	} else if toolChoiceType, ok := r.ToolChoice.(string); ok {
		toolType = toolChoiceType
	}

	if toolType == "" {
		toolType = ToolChoiceTypeAuto
	}

	return
}

func (r ChatCompletionRequest) GetFunctionCate() string {
	if r.Tools != nil {
		return "tool"
	} else if r.Functions != nil {
		return "function"
	}
	return ""
}

func (r *ChatCompletionRequest) GetFunctions() []*ChatCompletionFunction {
	if r.Tools == nil && r.Functions == nil {
		return nil
	}

	if r.Tools != nil {
		var functions []*ChatCompletionFunction
		for _, tool := range r.Tools {
			functions = append(functions, &tool.Function)
		}
		return functions
	}

	return r.Functions
}

type ChatCompletionFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
	Strict      *bool  `json:"strict,omitempty"`
}

type ChatCompletionTool struct {
	Type          string                 `json:"type"`
	Function      ChatCompletionFunction `json:"function,omitzero"`
	ResponsesTool ResponsesTools         `json:"-"`
}

func (t *ChatCompletionTool) UnmarshalJSON(data []byte) error {
	type chatCompletionToolPayload struct {
		Type     string                 `json:"type"`
		Function ChatCompletionFunction `json:"function"`
	}

	var payload chatCompletionToolPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	t.Type = payload.Type
	t.Function = payload.Function

	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFields); err != nil {
		return err
	}

	if _, hasFunction := rawFields["function"]; hasFunction {
		t.ResponsesTool = ResponsesTools{Type: payload.Type}
		return nil
	}

	var responsesTool ResponsesTools
	if err := json.Unmarshal(data, &responsesTool); err != nil {
		return err
	}
	if responsesTool.Type == "" {
		responsesTool.Type = payload.Type
	}

	t.ResponsesTool = responsesTool
	return nil
}

func (t ChatCompletionTool) MarshalJSON() ([]byte, error) {
	if t.Type == "function" || t.Function.Name != "" || t.Function.Description != "" || t.Function.Parameters != nil || t.Function.Strict != nil {
		type chatCompletionToolPayload struct {
			Type     string                 `json:"type"`
			Function ChatCompletionFunction `json:"function"`
		}

		return json.Marshal(chatCompletionToolPayload{
			Type:     t.Type,
			Function: t.Function,
		})
	}

	responsesTool := t.ResponsesTool
	if responsesTool.Type == "" {
		responsesTool.Type = t.Type
	}

	return json.Marshal(responsesTool)
}

type ChatCompletionChoice struct {
	Index                int                   `json:"index"`
	Message              ChatCompletionMessage `json:"message"`
	LogProbs             any                   `json:"logprobs,omitempty"`
	FinishReason         string                `json:"finish_reason,omitempty"`
	ContentFilterResults any                   `json:"content_filter_results,omitempty"`
	FinishDetails        any                   `json:"finish_details,omitempty"`
}

func (c *ChatCompletionChoice) CheckChoice(request *ChatCompletionRequest) {
	if request.Functions != nil && c.Message.ToolCalls != nil {
		c.Message.ToolToFuncCalls()
		c.FinishReason = FinishReasonFunctionCall
	}
}

type ChatCompletionResponse struct {
	ID                     string                 `json:"id"`
	Object                 string                 `json:"object"`
	Created                any                    `json:"created"`
	Model                  string                 `json:"model"`
	Choices                []ChatCompletionChoice `json:"choices"`
	Usage                  *Usage                 `json:"usage,omitempty"`
	SystemFingerprint      string                 `json:"system_fingerprint,omitempty"`
	ServiceTier            string                 `json:"service_tier,omitempty"`
	PromptFilterResults    any                    `json:"prompt_filter_results,omitempty"`
	rawProviderJSON        []byte                 `json:"-"`
	captureProviderRawJSON bool                   `json:"-"`
	replayProviderRawJSON  bool                   `json:"-"`
}

// chatCompletionUsage is the Chat Completions wire projection of Usage.
// Usage also carries provider-specific accounting details that must remain
// available internally but are not part of the downstream Chat contract.
// 字段以官方 CompletionUsage schema 为准（prompt/completion/legacy completions 共用）。
type chatCompletionUsage struct {
	PromptTokens            int                                   `json:"prompt_tokens"`
	CompletionTokens        int                                   `json:"completion_tokens"`
	TotalTokens             int                                   `json:"total_tokens"`
	PromptTokensDetails     chatCompletionPromptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails chatCompletionCompletionTokensDetails `json:"completion_tokens_details"`
}

type chatCompletionPromptTokensDetails struct {
	AudioTokens      int `json:"audio_tokens,omitempty"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
	TextTokens       int `json:"text_tokens,omitempty"`
	ImageTokens      int `json:"image_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// chatCompletionCompletionTokensDetails 不包含内部的 image_tokens：官方
// CompletionTokensDetails 只有以下五个字段。
type chatCompletionCompletionTokensDetails struct {
	AudioTokens              int `json:"audio_tokens,omitempty"`
	TextTokens               int `json:"text_tokens,omitempty"`
	ReasoningTokens          int `json:"reasoning_tokens"`
	AcceptedPredictionTokens int `json:"accepted_prediction_tokens"`
	RejectedPredictionTokens int `json:"rejected_prediction_tokens"`
}

func projectChatCompletionUsage(usage *Usage) *chatCompletionUsage {
	if usage == nil {
		return nil
	}
	return &chatCompletionUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		PromptTokensDetails: chatCompletionPromptTokensDetails{
			AudioTokens:      usage.PromptTokensDetails.AudioTokens,
			CachedTokens:     usage.PromptTokensDetails.CachedTokens,
			TextTokens:       usage.PromptTokensDetails.TextTokens,
			ImageTokens:      usage.PromptTokensDetails.ImageTokens,
			CacheWriteTokens: usage.PromptTokensDetails.CacheWriteTokens,
		},
		CompletionTokensDetails: chatCompletionCompletionTokensDetails{
			AudioTokens:              usage.CompletionTokensDetails.AudioTokens,
			TextTokens:               usage.CompletionTokensDetails.TextTokens,
			ReasoningTokens:          usage.CompletionTokensDetails.ReasoningTokens,
			AcceptedPredictionTokens: usage.CompletionTokensDetails.AcceptedPredictionTokens,
			RejectedPredictionTokens: usage.CompletionTokensDetails.RejectedPredictionTokens,
		},
	}
}

func (r ChatCompletionResponse) MarshalJSON() ([]byte, error) {
	type responseAlias ChatCompletionResponse
	return json.Marshal(struct {
		responseAlias
		Usage *chatCompletionUsage `json:"usage,omitempty"`
	}{
		responseAlias: responseAlias(r),
		Usage:         projectChatCompletionUsage(r.Usage),
	})
}

func (r *ChatCompletionResponse) SetProviderRawJSON(raw []byte) {
	if r == nil {
		return
	}
	r.rawProviderJSON = append(r.rawProviderJSON[:0], raw...)
}

func (r *ChatCompletionResponse) ProviderRawJSON() []byte {
	if r == nil {
		return nil
	}
	return append([]byte(nil), r.rawProviderJSON...)
}

func (r *ChatCompletionResponse) EnableProviderRawJSONCapture() {
	if r != nil {
		r.captureProviderRawJSON = true
	}
}

func (r *ChatCompletionResponse) CaptureProviderRawJSON() bool {
	return r != nil && r.captureProviderRawJSON
}

func (r *ChatCompletionResponse) EnableProviderRawJSONReplay() {
	if r != nil {
		r.replayProviderRawJSON = true
	}
}

func (r *ChatCompletionResponse) ReplayProviderRawJSON() []byte {
	if r == nil || !r.replayProviderRawJSON {
		return nil
	}
	return r.ProviderRawJSON()
}

func (cc *ChatCompletionResponse) GetContent() string {
	var content string
	for _, choice := range cc.Choices {
		content += choice.Message.StringContent()
	}
	return content
}

func (c ChatCompletionStreamChoice) ConvertOpenaiStream() []ChatCompletionStreamChoice {
	var choices []ChatCompletionStreamChoice
	var stopFinish string
	if c.Delta.FunctionCall != nil {
		stopFinish = FinishReasonFunctionCall
		choices = c.Delta.FunctionCall.Split(&c, stopFinish, 0)
	} else {
		stopFinish = FinishReasonToolCalls
		for index, tool := range c.Delta.ToolCalls {
			choices = append(choices, tool.Function.Split(&c, stopFinish, index)...)
		}
	}

	choices = append(choices, ChatCompletionStreamChoice{
		Index:        c.Index,
		Delta:        ChatCompletionStreamChoiceDelta{},
		FinishReason: stopFinish,
	})

	return choices
}

func (f *ChatCompletionToolCallsFunction) Split(c *ChatCompletionStreamChoice, stopFinish string, index int) []ChatCompletionStreamChoice {
	var functions []*ChatCompletionToolCallsFunction
	var choices []ChatCompletionStreamChoice
	functions = append(functions, &ChatCompletionToolCallsFunction{
		Name:      f.Name,
		Arguments: "",
	})

	if f.Arguments == "" || f.Arguments == "{}" {
		functions = append(functions, &ChatCompletionToolCallsFunction{
			Arguments: "{}",
		})
	} else {
		functions = append(functions, &ChatCompletionToolCallsFunction{
			Arguments: f.Arguments,
		})
	}

	for fIndex, function := range functions {
		choice := ChatCompletionStreamChoice{
			Index: c.Index,
			Delta: ChatCompletionStreamChoiceDelta{
				Role: c.Delta.Role,
			},
		}
		if stopFinish == FinishReasonFunctionCall {
			choice.Delta.FunctionCall = function
		} else {
			toolCalls := &ChatCompletionToolCalls{
				// Id:       c.Delta.ToolCalls[0].Id,
				Index:    index,
				Type:     ChatMessageRoleFunction,
				Function: function,
			}

			if fIndex == 0 {
				toolCalls.Id = c.Delta.ToolCalls[0].Id
			}
			choice.Delta.ToolCalls = []*ChatCompletionToolCalls{toolCalls}
		}

		choices = append(choices, choice)
	}

	return choices
}

type ChatCompletionStreamChoiceDelta struct {
	Content          string                           `json:"content,omitempty"`
	Refusal          string                           `json:"refusal,omitempty"`
	Role             string                           `json:"role,omitempty"`
	FunctionCall     *ChatCompletionToolCallsFunction `json:"function_call,omitempty"`
	ToolCalls        []*ChatCompletionToolCalls       `json:"tool_calls,omitempty"`
	ReasoningContent string                           `json:"reasoning_content,omitempty"`
	Reasoning        string                           `json:"reasoning,omitempty"`
	Image            []MultimediaData                 `json:"image,omitempty"`
	Images           []ChatMessagePart                `json:"images,omitempty"`
}

func (m *ChatCompletionStreamChoiceDelta) ToolToFuncCalls() {
	if m.FunctionCall != nil {
		return
	}
	if len(m.ToolCalls) > 0 && m.ToolCalls[0] != nil && m.ToolCalls[0].Function != nil {
		m.FunctionCall = &ChatCompletionToolCallsFunction{
			Name:      m.ToolCalls[0].Function.Name,
			Arguments: m.ToolCalls[0].Function.Arguments,
		}
		m.ToolCalls = nil
	}
}

type ChatCompletionStreamChoice struct {
	Index                int                             `json:"index"`
	Delta                ChatCompletionStreamChoiceDelta `json:"delta"`
	FinishReason         any                             `json:"finish_reason"`
	ContentFilterResults any                             `json:"content_filter_results,omitempty"`
	Usage                *Usage                          `json:"usage,omitempty"`
}

func (c ChatCompletionStreamChoice) MarshalJSON() ([]byte, error) {
	type choiceAlias ChatCompletionStreamChoice
	return json.Marshal(struct {
		choiceAlias
		Usage *chatCompletionUsage `json:"usage,omitempty"`
	}{
		choiceAlias: choiceAlias(c),
		Usage:       projectChatCompletionUsage(c.Usage),
	})
}

func (c *ChatCompletionStreamChoice) CheckChoice(request *ChatCompletionRequest) {
	if request.Functions != nil && c.Delta.ToolCalls != nil {
		c.Delta.ToolToFuncCalls()
		c.FinishReason = FinishReasonToolCalls
	}
}

type ChatCompletionStreamResponse struct {
	ID                string                       `json:"id"`
	Object            string                       `json:"object"`
	Created           any                          `json:"created"`
	Model             string                       `json:"model"`
	Choices           []ChatCompletionStreamChoice `json:"choices"`
	PromptAnnotations any                          `json:"prompt_annotations,omitempty"`
	Usage             *Usage                       `json:"usage,omitempty"`
	ServiceTier       string                       `json:"service_tier,omitempty"`
}

func (r ChatCompletionStreamResponse) MarshalJSON() ([]byte, error) {
	type responseAlias ChatCompletionStreamResponse
	return json.Marshal(struct {
		responseAlias
		Usage *chatCompletionUsage `json:"usage,omitempty"`
	}{
		responseAlias: responseAlias(r),
		Usage:         projectChatCompletionUsage(r.Usage),
	})
}

type ChatAudio struct {
	Voice  string `json:"voice"`
	Format string `json:"format"`
}

type MultimediaData struct {
	Data       string `json:"data"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	ID         string `json:"id,omitempty"`
	Transcript string `json:"transcript,omitempty"`
}

func (c *ChatCompletionRequest) ToResponsesRequest() *OpenAIResponsesRequest {
	res := &OpenAIResponsesRequest{
		Model:            c.Model,
		MaxOutputTokens:  c.MaxCompletionTokens,
		Stream:           c.Stream,
		Temperature:      c.Temperature,
		ToolChoice:       c.ToolChoice,
		TopP:             c.TopP,
		ServiceTier:      c.ServiceTier,
		ProcessingClass:  c.ProcessingClass,
		SafetyIdentifier: c.SafetyIdentifier,
		PromptCacheKey:   c.PromptCacheKeyString(),
	}
	// Chat stream options control the downstream Chat stream contract. They are
	// not forwarded across this protocol conversion.
	if c.ParallelToolCalls != nil {
		value := *c.ParallelToolCalls
		res.ParallelToolCalls = &value
	}
	// Chat Completions does not create retained application state by default,
	// while Responses does. Preserve the Chat contract across the protocol
	// boundary by making the otherwise different default explicit.
	store := false
	if c.Store != nil {
		store = *c.Store
	}
	res.Store = &store
	if c.Instructions != nil {
		res.Instructions = *c.Instructions
	}

	if c.ResponseFormat != nil {
		res.Text = &ResponsesText{}

		if c.ResponseFormat.Type != "" {
			res.Text.Format = &ResponsesTextFormat{
				Type: c.ResponseFormat.Type,
			}
		}

		if c.ResponseFormat.JsonSchema != nil && res.Text.Format != nil {
			res.Text.Format.Name = c.ResponseFormat.JsonSchema.Name
			res.Text.Format.Schema = c.ResponseFormat.JsonSchema.Schema
			res.Text.Format.Description = c.ResponseFormat.JsonSchema.Description
			res.Text.Format.Strict = c.ResponseFormat.JsonSchema.Strict
		}
	}

	if c.Verbosity != "" {
		if res.Text == nil {
			res.Text = &ResponsesText{}
		}

		res.Text.Verbosity = c.Verbosity
	}

	if c.Reasoning != nil {
		res.Reasoning = &ReasoningEffort{
			Summary: c.Reasoning.Summary,
		}

		if c.Reasoning.Effort != "" {
			res.Reasoning.Effort = &c.Reasoning.Effort
		}
	}

	if c.ReasoningEffort != nil && res.Reasoning == nil {
		res.Reasoning = &ReasoningEffort{
			Effort: c.ReasoningEffort,
		}
	}

	if len(c.Tools) > 0 {
		resTools := make([]ResponsesTools, 0)
		for _, tool := range c.Tools {
			if tool.Type == "function" && tool.Function.Name != "" {
				resTools = append(resTools, ResponsesTools{
					Type:        tool.Type,
					Name:        tool.Function.Name,
					Description: tool.Function.Description,
					Parameters:  tool.Function.Parameters,
					Strict:      tool.Function.Strict,
				})
				continue
			}

			responsesTool := tool.ResponsesTool
			responsesTool.Type = tool.Type
			if tool.Type == "custom" {
				if nested, ok := responsesTool.rawFields["custom"]; ok {
					var customTool ResponsesTools
					if json.Unmarshal(nested, &customTool) == nil {
						customTool.Type = tool.Type
						responsesTool = customTool
					}
				}
			}
			resTools = append(resTools, responsesTool)
		}

		if len(resTools) > 0 {
			res.Tools = resTools
		}
	}

	inputs := make([]InputResponses, 0)
	toolCallTypes := make(map[string]string)
	for _, msg := range c.Messages {
		// Chat allows assistant content and tool calls on the same message. Responses
		// represents them as adjacent input items, with the message content first.
		if len(msg.ToolCalls) > 0 {
			if input, ok := chatMessageToResponsesInput(msg); ok {
				inputs = append(inputs, input)
			}
			for _, tool := range msg.ToolCalls {
				if tool == nil {
					continue
				}
				if tool.Type == ToolChoiceTypeCustom && tool.Custom != nil {
					inputs = append(inputs, InputResponses{
						Type:   InputTypeCustomToolCall,
						CallID: tool.Id,
						Name:   tool.Custom.Name,
						Input:  tool.Custom.Input,
					})
					toolCallTypes[tool.Id] = ToolChoiceTypeCustom
					continue
				}
				if tool.Function != nil {
					inputs = append(inputs, InputResponses{
						Type:      InputTypeFunctionCall,
						CallID:    tool.Id,
						Name:      tool.Function.Name,
						Arguments: tool.Function.Arguments,
					})
					toolCallTypes[tool.Id] = ToolChoiceTypeFunction
				}
			}

			continue
		}

		if msg.ToolCallID != "" {
			outputType := InputTypeFunctionCallOutput
			if toolCallTypes[msg.ToolCallID] == ToolChoiceTypeCustom {
				outputType = InputTypeCustomToolCallOutput
			}
			inputs = append(inputs, InputResponses{
				Type:   outputType,
				CallID: msg.ToolCallID,
				Output: chatToolOutputToResponses(msg.Content),
			})

			continue
		}

		if input, ok := chatMessageToResponsesInput(msg); ok {
			inputs = append(inputs, input)
		}
	}

	if len(inputs) > 0 {
		res.Input = inputs
	}
	res.ToolChoice = flattenChatToolChoice(c.ToolChoice)

	return res
}

// PromptCacheKeyString returns the client key only when its wire value is a
// string. Chat exact-wire paths leave other current or future upstream-owned
// shapes untouched; cross-protocol adapters can reject them as unrepresentable.
func (c *ChatCompletionRequest) PromptCacheKeyString() string {
	if c == nil {
		return ""
	}
	key, _ := c.PromptCacheKey.(string)
	return strings.TrimSpace(key)
}

func chatMessageToResponsesInput(msg ChatCompletionMessage) (InputResponses, bool) {
	input := InputResponses{
		Type: InputTypeMessage,
		Role: msg.Role,
	}
	inputContent := make([]ContentResponses, 0)

	for _, part := range msg.ParseContent() {
		switch part.Type {
		case ContentTypeImageURL:
			if part.ImageURL == nil {
				continue
			}
			inputContent = append(inputContent, ContentResponses{
				Type:     ContentTypeInputImage,
				ImageUrl: part.ImageURL.URL,
				Detail:   part.ImageURL.Detail,
			})
		case ContentTypeFile:
			if part.File == nil {
				continue
			}
			inputContent = append(inputContent, ContentResponses{
				Type:     ContentTypeInputFile,
				FileId:   part.File.FileID,
				FileData: part.File.FileData,
				FileName: part.File.Filename,
			})
		case ContentTypeText:
			roleType := ContentTypeInputText
			if msg.Role == ChatMessageRoleAssistant {
				roleType = ContentTypeOutputText
			}
			inputContent = append(inputContent, ContentResponses{
				Type: roleType,
				Text: part.Text,
			})
		}
	}

	if len(inputContent) == 0 {
		return InputResponses{}, false
	}
	input.Content = inputContent
	return input, true
}

func chatToolOutputToResponses(output any) any {
	if _, ok := output.(string); ok {
		return output
	}
	raw, err := json.Marshal(output)
	if err != nil {
		return output
	}
	var parts []ChatMessagePart
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
		return output
	}
	responsesParts := make([]ContentResponses, 0, len(parts))
	for _, part := range parts {
		if part.Type != ContentTypeText {
			return output
		}
		responsesParts = append(responsesParts, ContentResponses{Type: ContentTypeInputText, Text: part.Text})
	}
	return responsesParts
}

func flattenChatToolChoice(choice any) any {
	object, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	typeName, _ := object["type"].(string)
	if typeName != ToolChoiceTypeFunction && typeName != ToolChoiceTypeCustom {
		return choice
	}
	nested, ok := object[typeName].(map[string]any)
	if !ok {
		return choice
	}
	flattened := make(map[string]any, len(nested)+1)
	flattened["type"] = typeName
	for key, value := range nested {
		flattened[key] = value
	}
	return flattened
}
