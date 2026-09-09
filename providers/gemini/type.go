package gemini

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/storage"
	"one-api/common/utils"
	"one-api/providers/base"
	"one-api/types"
	"strings"
)

const GeminiImageSymbol = "![one-hub-gemini-image]"

const geminiImageUploadError = "image upload err"

const (
	ModalityTEXT  = "TEXT"
	ModalityAUDIO = "AUDIO"
	ModalityIMAGE = "IMAGE"
	ModalityVIDEO = "VIDEO"
)

type GeminiChatRequest struct {
	Model             string                     `json:"-"`
	Stream            bool                       `json:"-"`
	Contents          []GeminiChatContent        `json:"contents"`
	SafetySettings    []GeminiChatSafetySettings `json:"safetySettings,omitempty"`
	GenerationConfig  GeminiChatGenerationConfig `json:"generationConfig,omitempty"`
	Tools             []GeminiChatTools          `json:"tools,omitempty"`
	ToolConfig        *GeminiToolConfig          `json:"toolConfig,omitempty"`
	SystemInstruction any                        `json:"systemInstruction,omitempty"`
	CachedContent     string                     `json:"cachedContent,omitempty"`

	JsonRaw []byte `json:"-"`
}

func (r *GeminiChatRequest) UsesCachedContent() bool {
	return r != nil && strings.TrimSpace(r.CachedContent) != ""
}

func (r *GeminiChatRequest) UsesInputImages() bool {
	if r == nil {
		return false
	}
	for _, content := range r.Contents {
		for _, part := range content.Parts {
			mimeType := ""
			if part.InlineData != nil {
				mimeType = part.InlineData.MimeType
			} else if part.FileData != nil {
				mimeType = part.FileData.MimeType
			}
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/") {
				return true
			}
		}
	}
	return false
}

func (r *GeminiChatRequest) ApplyUsageRequirements(usage *types.Usage) {
	applyGeminiUsageRequirements(usage, r.UsesCachedContent(), r.UsesInputImages())
}

func (r *GeminiChatRequest) UsesGoogleSearch() bool {
	if r == nil {
		return false
	}
	for _, tool := range r.Tools {
		if tool.GoogleSearch != nil {
			return true
		}
	}
	return false
}

// UsesGoogleSearchInRaw recognizes both ProtoJSON spellings of the native
// Gemini grounding tool. Native relay requests keep their original body for
// the provider, so this check intentionally observes the raw envelope without
// rewriting or narrowing it.
func (r *GeminiChatRequest) UsesGoogleSearchInRaw(raw []byte) bool {
	if r != nil && r.UsesGoogleSearch() {
		return true
	}
	return rawGeminiGoogleSearch(raw)
}

func rawGeminiGoogleSearch(raw []byte) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var envelope struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false
	}
	for _, tool := range envelope.Tools {
		for _, field := range []string{"googleSearch", "google_search"} {
			value, ok := tool[field]
			if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				continue
			}
			return true
		}
	}
	return false
}

type GeminiToolConfig struct {
	FunctionCallingConfig *GeminiFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type GeminiFunctionCallingConfig struct {
	Model                string `json:"model,omitempty"`
	AllowedFunctionNames any    `json:"allowedFunctionNames,omitempty"`
}
type GeminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type GeminiFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileUri  string `json:"fileUri,omitempty"`
}

type GeminiPart struct {
	FunctionCall        *GeminiFunctionCall            `json:"functionCall,omitempty"`
	FunctionResponse    *GeminiFunctionResponse        `json:"functionResponse,omitempty"`
	Text                string                         `json:"text,omitempty"`
	InlineData          *GeminiInlineData              `json:"inlineData,omitempty"`
	FileData            *GeminiFileData                `json:"fileData,omitempty"`
	ExecutableCode      *GeminiPartExecutableCode      `json:"executableCode,omitempty"`
	CodeExecutionResult *GeminiPartCodeExecutionResult `json:"codeExecutionResult,omitempty"`
	Thought             bool                           `json:"thought,omitempty"` // 是否是思考内容
	ThoughtSignature    json.RawMessage                `json:"thoughtSignature,omitempty"`
	MediaResolution     json.RawMessage                `json:"mediaResolution,omitempty"`
	VideoMetadata       json.RawMessage                `json:"videoMetadata,omitempty"`
}

type GeminiPartExecutableCode struct {
	Language string `json:"language,omitempty"`
	Code     string `json:"code,omitempty"`
}

type GeminiPartCodeExecutionResult struct {
	Outcome string `json:"outcome,omitempty"`
	Output  string `json:"output,omitempty"`
}

type GeminiFunctionCall struct {
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

var emptyGeminiToolArgs = json.RawMessage("{}")

func normalizeGeminiToolArgs(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return emptyGeminiToolArgs
	}

	if trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return emptyGeminiToolArgs
	}

	if !json.Valid([]byte(trimmed)) {
		return emptyGeminiToolArgs
	}

	return json.RawMessage(trimmed)
}

func (candidate *GeminiChatCandidate) ToOpenAIStreamChoice(request *types.ChatCompletionRequest) types.ChatCompletionStreamChoice {
	choice := types.ChatCompletionStreamChoice{
		Index: int(candidate.Index),
		Delta: types.ChatCompletionStreamChoiceDelta{
			Role: types.ChatMessageRoleAssistant,
		},
	}

	if candidate.FinishReason != nil {
		choice.FinishReason = ConvertFinishReason(*candidate.FinishReason)
	}

	var content []string
	isTools := false
	images := make([]types.MultimediaData, 0)
	reasoningContent := make([]string, 0)

	for _, part := range candidate.Content.Parts {
		if part.FunctionCall != nil {
			if choice.Delta.ToolCalls == nil {
				choice.Delta.ToolCalls = make([]*types.ChatCompletionToolCalls, 0)
			}
			isTools = true
			choice.Delta.ToolCalls = append(choice.Delta.ToolCalls, part.FunctionCall.ToOpenAITool())
		} else if part.InlineData != nil {
			if strings.HasPrefix(part.InlineData.MimeType, "image/") {
				images = append(images, types.MultimediaData{
					Data: part.InlineData.Data,
				})
				url := ""
				imageData, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
				if err == nil {
					url = storage.Upload(imageData, utils.GetUUID()+".png")
				}
				if url == "" {
					url = geminiImageUploadError
				}
				content = append(content, fmt.Sprintf("%s(%s)", GeminiImageSymbol, url))
			}
			//  else if strings.HasPrefix(part.InlineData.MimeType, "audio/") {
			// 	choice.Message.Audio = types.MultimediaData{
			// 		Data: part.InlineData.Data,
			// 	}
			// }
		} else {
			if part.ExecutableCode != nil {
				content = append(content, "```"+part.ExecutableCode.Language+"\n"+part.ExecutableCode.Code+"\n```")
			} else if part.CodeExecutionResult != nil {
				content = append(content, "```output\n"+part.CodeExecutionResult.Output+"\n```")
			} else if part.Thought {
				reasoningContent = append(reasoningContent, part.Text)
			} else {
				content = append(content, part.Text)
			}
		}
	}

	if len(images) > 0 {
		choice.Delta.Image = images
	}

	// Add grounding metadata as markdown citations
	if candidate.GroundingMetadata != nil && showGoogleSearchMeta(request) {
		groundingMarkdown := formatGroundingMetadataAsMarkdown(candidate.GroundingMetadata)
		if groundingMarkdown != "" {
			content = append(content, "\n\n"+groundingMarkdown)
		}
	}

	choice.Delta.Content = strings.Join(content, "\n")

	if len(reasoningContent) > 0 {
		choice.Delta.ReasoningContent = strings.Join(reasoningContent, "\n")
	}

	if isTools {
		choice.FinishReason = types.FinishReasonToolCalls
	}
	choice.CheckChoice(request)

	return choice
}

func (candidate *GeminiChatCandidate) ToOpenAIChoice(request *types.ChatCompletionRequest) types.ChatCompletionChoice {
	choice := types.ChatCompletionChoice{
		Index: int(candidate.Index),
		Message: types.ChatCompletionMessage{
			Role: "assistant",
		},
		// FinishReason: types.FinishReasonStop,
	}

	if candidate.FinishReason != nil {
		choice.FinishReason = ConvertFinishReason(*candidate.FinishReason)
	}

	if len(candidate.Content.Parts) == 0 {
		choice.Message.Content = ""
		return choice
	}

	var content []string
	useTools := false
	images := make([]types.MultimediaData, 0)
	reasoningContent := make([]string, 0)

	for _, part := range candidate.Content.Parts {
		if part.FunctionCall != nil {
			if choice.Message.ToolCalls == nil {
				choice.Message.ToolCalls = make([]*types.ChatCompletionToolCalls, 0)
			}
			useTools = true
			choice.Message.ToolCalls = append(choice.Message.ToolCalls, part.FunctionCall.ToOpenAITool())
		} else if part.InlineData != nil {
			if strings.HasPrefix(part.InlineData.MimeType, "image/") {
				images = append(images, types.MultimediaData{
					Data: part.InlineData.Data,
				})
				url := ""
				imageData, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
				if err == nil {
					url = storage.Upload(imageData, utils.GetUUID()+".png")
				}
				if url == "" {
					url = geminiImageUploadError
				}
				content = append(content, fmt.Sprintf("%s(%s)", GeminiImageSymbol, url))
			}
			//  else if strings.HasPrefix(part.InlineData.MimeType, "audio/") {
			// 	choice.Message.Audio = types.MultimediaData{
			// 		Data: part.InlineData.Data,
			// 	}
			// }
		} else {
			if part.ExecutableCode != nil {
				content = append(content, "```"+part.ExecutableCode.Language+"\n"+part.ExecutableCode.Code+"\n```")
			} else if part.CodeExecutionResult != nil {
				content = append(content, "```output\n"+part.CodeExecutionResult.Output+"\n```")
			} else if part.Thought {
				reasoningContent = append(reasoningContent, part.Text)
			} else {
				content = append(content, part.Text)
			}
		}
	}

	choice.Message.Content = strings.Join(content, "\n")

	// Add grounding metadata as markdown citations
	if candidate.GroundingMetadata != nil && showGoogleSearchMeta(request) {
		groundingMarkdown := formatGroundingMetadataAsMarkdown(candidate.GroundingMetadata)
		if groundingMarkdown != "" {
			if contentStr, ok := choice.Message.Content.(string); ok && contentStr != "" {
				choice.Message.Content = contentStr + "\n\n" + groundingMarkdown
			} else {
				choice.Message.Content = groundingMarkdown
			}
		}
	}

	if len(reasoningContent) > 0 {
		choice.Message.ReasoningContent = strings.Join(reasoningContent, "\n")
	}

	if len(images) > 0 {
		choice.Message.Image = images
	}

	if useTools {
		choice.FinishReason = types.FinishReasonToolCalls
	}

	choice.CheckChoice(request)

	return choice
}

type GeminiFunctionResponse struct {
	Name         string          `json:"name,omitempty"`
	Response     any             `json:"response,omitempty"`
	WillContinue json.RawMessage `json:"willContinue,omitempty"`
	Scheduling   json.RawMessage `json:"scheduling,omitempty"`
	Parts        json.RawMessage `json:"parts,omitempty"`
	ID           json.RawMessage `json:"id,omitempty"`
}

type GeminiFunctionResponseContent struct {
	Name    string `json:"name,omitempty"`
	Content string `json:"content,omitempty"`
}

func (g *GeminiFunctionCall) ToOpenAITool() *types.ChatCompletionToolCalls {
	args, _ := json.Marshal(g.Args)

	return &types.ChatCompletionToolCalls{
		Id:    "call_" + utils.GetRandomString(24),
		Type:  types.ChatMessageRoleFunction,
		Index: 0,
		Function: &types.ChatCompletionToolCallsFunction{
			Name:      g.Name,
			Arguments: string(args),
		},
	}
}

type GeminiChatContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts,omitempty"`
}

type GeminiChatSafetySettings struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

type GeminiChatTools struct {
	FunctionDeclarations  []types.ChatCompletionFunction `json:"functionDeclarations,omitempty"`
	CodeExecution         *GeminiCodeExecution           `json:"codeExecution,omitempty"`
	GoogleSearch          any                            `json:"googleSearch,omitempty"`
	UrlContext            any                            `json:"urlContext,omitempty"`
	GoogleSearchRetrieval any                            `json:"googleSearchRetrieval,omitempty"`
}

type GeminiCodeExecution struct {
}

type GeminiChatGenerationConfig struct {
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"topP,omitempty"`
	TopK               *float64        `json:"topK,omitempty"`
	MaxOutputTokens    int             `json:"maxOutputTokens,omitempty"`
	CandidateCount     int             `json:"candidateCount,omitempty"`
	StopSequences      []string        `json:"stopSequences,omitempty"`
	ResponseMimeType   string          `json:"responseMimeType,omitempty"`
	ResponseSchema     any             `json:"responseSchema,omitempty"`
	ResponseModalities []string        `json:"responseModalities,omitempty"`
	ThinkingConfig     *ThinkingConfig `json:"thinkingConfig,omitempty"`
}

type ThinkingConfig struct {
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
}

type GeminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func (e *GeminiError) Error() string {
	bytes, _ := json.Marshal(e)
	return string(bytes) + "\n"
}

type GeminiErrorResponse struct {
	ErrorInfo *GeminiError `json:"error,omitempty"`
}

func (e *GeminiErrorResponse) Error() string {
	bytes, _ := json.Marshal(e)
	return string(bytes) + "\n"
}

type GeminiChatResponse struct {
	Candidates     []GeminiChatCandidate     `json:"candidates"`
	PromptFeedback *GeminiChatPromptFeedback `json:"promptFeedback,omitempty"`
	UsageMetadata  *GeminiUsageMetadata      `json:"usageMetadata,omitempty"`
	ModelVersion   string                    `json:"modelVersion,omitempty"`
	Model          string                    `json:"model,omitempty"`
	ResponseId     string                    `json:"responseId,omitempty"`
	GeminiErrorResponse

	rawProviderJSON        []byte
	replayProviderRawJSON  bool
	captureProviderRawJSON bool
}

func (r *GeminiChatResponse) SetProviderRawJSON(raw []byte) {
	if r != nil {
		r.rawProviderJSON = append(r.rawProviderJSON[:0], raw...)
	}
}

func (r *GeminiChatResponse) ProviderRawJSON() []byte {
	if r == nil {
		return nil
	}
	return append([]byte(nil), r.rawProviderJSON...)
}

func (r *GeminiChatResponse) EnableProviderRawJSONCapture() {
	if r != nil {
		r.captureProviderRawJSON = true
	}
}

func (r *GeminiChatResponse) CaptureProviderRawJSON() bool {
	return r != nil && r.captureProviderRawJSON
}

func (r *GeminiChatResponse) EnableProviderRawJSONReplay() {
	if r != nil {
		r.replayProviderRawJSON = true
	}
}

func (r *GeminiChatResponse) ReplayProviderRawJSON() []byte {
	if r == nil || !r.replayProviderRawJSON {
		return nil
	}
	return r.ProviderRawJSON()
}

type GeminiUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
	ToolUsePromptTokenCount int `json:"toolUsePromptTokenCount,omitempty"`

	PromptTokensDetails        []GeminiUsageMetadataDetails `json:"promptTokensDetails,omitempty"`
	CandidatesTokensDetails    []GeminiUsageMetadataDetails `json:"candidatesTokensDetails,omitempty"`
	CacheTokensDetails         []GeminiUsageMetadataDetails `json:"cacheTokensDetails,omitempty"`
	ToolUsePromptTokensDetails []GeminiUsageMetadataDetails `json:"toolUsePromptTokensDetails,omitempty"`
	ServiceTier                string                       `json:"serviceTier,omitempty"`

	promptTokenCountPresent        bool
	candidatesTokenCountPresent    bool
	totalTokenCountPresent         bool
	cachedContentPresent           bool
	toolUsePromptTokenCountPresent bool
	invalidTokenCountField         bool
}

func (u *GeminiUsageMetadata) UnmarshalJSON(data []byte) error {
	type usageAlias GeminiUsageMetadata
	var decoded usageAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = GeminiUsageMetadata(decoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err == nil {
		for _, key := range []string{
			"promptTokenCount",
			"candidatesTokenCount",
			"totalTokenCount",
			"cachedContentTokenCount",
			"thoughtsTokenCount",
			"toolUsePromptTokenCount",
		} {
			if raw, exists := fields[key]; exists && !geminiUsageIntegerPresent(raw) {
				u.invalidTokenCountField = true
			}
		}
		for _, key := range []string{"promptTokensDetails", "candidatesTokensDetails", "cacheTokensDetails", "toolUsePromptTokensDetails"} {
			if raw, exists := fields[key]; exists && !geminiUsageDetailsValid(raw) {
				u.invalidTokenCountField = true
			}
		}
		u.promptTokenCountPresent = geminiUsageIntegerPresent(fields["promptTokenCount"])
		u.candidatesTokenCountPresent = geminiUsageIntegerPresent(fields["candidatesTokenCount"])
		u.totalTokenCountPresent = geminiUsageIntegerPresent(fields["totalTokenCount"])
		u.cachedContentPresent = geminiUsageIntegerPresent(fields["cachedContentTokenCount"])
		u.toolUsePromptTokenCountPresent = geminiUsageIntegerPresent(fields["toolUsePromptTokenCount"])
	}
	return nil
}

func geminiUsageIntegerPresent(raw json.RawMessage) bool {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return false
	}
	var value int
	return json.Unmarshal(raw, &value) == nil
}

func geminiUsageDetailsValid(raw json.RawMessage) bool {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return false
	}
	var details []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &details); err != nil {
		return false
	}
	for _, detail := range details {
		if detail == nil {
			return false
		}
		value, exists := detail["tokenCount"]
		if exists && !geminiUsageIntegerPresent(value) {
			return false
		}
	}
	return true
}

type GeminiUsageMetadataDetails struct {
	Modality   string `json:"modality"`
	TokenCount int    `json:"tokenCount"`
}

type GeminiChatCandidate struct {
	Content               GeminiChatContent        `json:"content"`
	FinishReason          *string                  `json:"finishReason,omitempty"`
	Index                 int64                    `json:"index"`
	SafetyRatings         []GeminiChatSafetyRating `json:"safetyRatings"`
	CitationMetadata      any                      `json:"citationMetadata,omitempty"`
	TokenCount            int                      `json:"tokenCount,omitempty"`
	GroundingAttributions []any                    `json:"groundingAttributions,omitempty"`
	GroundingMetadata     *GeminiGroundingMetadata `json:"groundingMetadata,omitempty"`
	AvgLogprobs           any                      `json:"avgLogprobs,omitempty"`
}

type GeminiChatSafetyRating struct {
	Category    string `json:"category"`
	Probability string `json:"probability"`
}

type GeminiChatPromptFeedback struct {
	BlockReason   string                   `json:"blockReason"`
	SafetyRatings []GeminiChatSafetyRating `json:"safetyRatings"`
}

// NormalizeAssistantImageHistory 只识别项目公开的 assistant 图片表示，将标记
// 转成标准 image_url part，交给现有媒体准备校验 data URI 或安全物化远程 URL。
// 普通用户文本和相似 Markdown 不会被改写。
func NormalizeAssistantImageHistory(request *types.ChatCompletionRequest) error {
	if request == nil {
		return nil
	}
	for messageIndex := range request.Messages {
		message := &request.Messages[messageIndex]
		if message.Role != types.ChatMessageRoleAssistant || len(message.Image) == 0 {
			continue
		}

		parts := message.ParseContent()
		markerCount := 0
		for _, part := range parts {
			if part.Type == types.ContentTypeText {
				markerCount += strings.Count(part.Text, GeminiImageSymbol)
			}
		}
		if markerCount == 0 {
			// 已规范化的消息可能再次经过准备；显式 image_url 已位于安全边界。
			for _, part := range parts {
				if part.Type == types.ContentTypeImageURL && part.ImageURL != nil {
					markerCount++
				}
			}
			if markerCount > 0 {
				continue
			}
			return fmt.Errorf("messages[%d] assistant image requires %q marker", messageIndex, GeminiImageSymbol)
		}
		if markerCount != len(message.Image) {
			return fmt.Errorf("messages[%d] assistant image marker count %d does not match image count %d", messageIndex, markerCount, len(message.Image))
		}

		imageIndex := 0
		rewritten := make([]types.ChatMessagePart, 0, len(parts)+len(message.Image))
		for _, part := range parts {
			if part.Type != types.ContentTypeText || !strings.Contains(part.Text, GeminiImageSymbol) {
				rewritten = append(rewritten, part)
				continue
			}
			expanded, consumed, err := expandAssistantImageMarkers(part.Text, message.Image[imageIndex:])
			if err != nil {
				return fmt.Errorf("messages[%d] assistant image: %w", messageIndex, err)
			}
			if consumed > len(message.Image)-imageIndex {
				return fmt.Errorf("messages[%d] assistant image has too many markers", messageIndex)
			}
			rewritten = append(rewritten, expanded...)
			imageIndex += consumed
		}
		if imageIndex != len(message.Image) {
			return fmt.Errorf("messages[%d] assistant image marker count does not match image count", messageIndex)
		}
		message.Content = rewritten
	}
	return nil
}

func expandAssistantImageMarkers(text string, images []types.MultimediaData) ([]types.ChatMessagePart, int, error) {
	parts := make([]types.ChatMessagePart, 0, 2)
	consumed := 0
	for {
		markerPos := strings.Index(text, GeminiImageSymbol)
		if markerPos < 0 {
			if text != "" {
				parts = append(parts, types.ChatMessagePart{Type: types.ContentTypeText, Text: text})
			}
			return parts, consumed, nil
		}
		if markerPos > 0 {
			parts = append(parts, types.ChatMessagePart{Type: types.ContentTypeText, Text: text[:markerPos]})
		}
		remainder := text[markerPos+len(GeminiImageSymbol):]
		if !strings.HasPrefix(remainder, "(") {
			return nil, consumed, fmt.Errorf("marker must be followed by a media URL")
		}
		closePos := strings.IndexByte(remainder[1:], ')')
		if closePos < 0 {
			return nil, consumed, fmt.Errorf("marker media URL is not closed")
		}
		closePos++
		mediaURL := strings.TrimSpace(remainder[1:closePos])
		if mediaURL == "" {
			return nil, consumed, fmt.Errorf("marker media URL is empty")
		}
		if consumed >= len(images) {
			return nil, consumed, fmt.Errorf("marker count exceeds image count")
		}
		mediaURL, err := assistantImageMediaURL(images[consumed], mediaURL)
		if err != nil {
			return nil, consumed, err
		}
		parts = append(parts, types.ChatMessagePart{
			Type:     types.ContentTypeImageURL,
			ImageURL: &types.ChatMessageImageURL{URL: mediaURL},
		})
		consumed++
		text = remainder[closePos+1:]
	}
}

func assistantImageMediaURL(image types.MultimediaData, mediaURL string) (string, error) {
	rawData := strings.TrimSpace(image.Data)
	if rawData == "" {
		return "", fmt.Errorf("image data is empty")
	}
	var body []byte
	if strings.HasPrefix(strings.ToLower(rawData), "data:") {
		_, decoded, err := base.DecodeChatMediaDataURI(rawData)
		if err != nil {
			return "", fmt.Errorf("image data URI is invalid: %w", err)
		}
		body = decoded
	} else {
		decoded, err := base.DecodeBase64Bounded(rawData, base.MaxChatRemoteMediaItemBytes)
		if err != nil {
			if errors.Is(err, base.ErrBase64PayloadTooLarge) {
				return "", fmt.Errorf("image data exceeds %d bytes", base.MaxChatRemoteMediaItemBytes)
			}
			return "", fmt.Errorf("image data is not valid base64")
		}
		body = decoded
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaURL)), "data:") {
		_, markerBody, err := base.DecodeChatMediaDataURI(mediaURL)
		if err != nil {
			return "", fmt.Errorf("marker data URI is invalid: %w", err)
		}
		if !bytes.Equal(markerBody, body) {
			return "", fmt.Errorf("image data does not match marker data URI")
		}
	}
	if mediaURL == geminiImageUploadError {
		// 图床失败不改变已生成的图片；内联数据仍经过共享 MIME 和资源限额检查。
		mediaURL = "data:" + http.DetectContentType(body) + ";base64," + base64.StdEncoding.EncodeToString(body)
	}
	return mediaURL, nil
}

// NormalizeChatRemoteMedia 实现共享媒体准备前的可选 hook。OpenAI 兼容 Gemini
// 端点拥有不同的 URL 合同，因此保留原有直通行为。
func (p *GeminiProvider) NormalizeChatRemoteMedia(request *types.ChatCompletionRequest) error {
	if p == nil || p.UseOpenaiAPI {
		return nil
	}
	return NormalizeAssistantImageHistory(request)
}

func OpenAIToGeminiChatContent(openaiContents []types.ChatCompletionMessage) ([]GeminiChatContent, string, *types.OpenAIErrorWithStatusCode) {
	contents := make([]GeminiChatContent, 0)
	// useToolName := ""
	var systemContent []string
	toolCallId := make(map[string]string)

	for _, openaiContent := range openaiContents {
		if openaiContent.IsSystemRole() {
			systemContent = append(systemContent, openaiContent.StringContent())
			continue
		}

		content := GeminiChatContent{
			Role:  ConvertRole(openaiContent.Role),
			Parts: make([]GeminiPart, 0),
		}
		openaiContent.FuncToToolCalls()

		if openaiContent.ToolCalls != nil {
			for _, toolCall := range openaiContent.ToolCalls {
				toolCallId[toolCall.Id] = toolCall.Function.Name

				args := normalizeGeminiToolArgs(toolCall.Function.Arguments)

				content.Parts = append(content.Parts, GeminiPart{
					FunctionCall: &GeminiFunctionCall{
						Name: toolCall.Function.Name,
						Args: args,
					},
				})
			}
			text := openaiContent.StringContent()
			if text != "" {
				contents = append(contents, createSystemResponse(text))
			}
		} else if openaiContent.Role == types.ChatMessageRoleFunction || openaiContent.Role == types.ChatMessageRoleTool {
			if openaiContent.Name == nil {
				if toolName, exists := toolCallId[openaiContent.ToolCallID]; exists {
					openaiContent.Name = &toolName
				}
			}

			functionPart := GeminiPart{
				FunctionResponse: &GeminiFunctionResponse{
					Name: *openaiContent.Name,
					Response: GeminiFunctionResponseContent{
						Name:    *openaiContent.Name,
						Content: openaiContent.StringContent(),
					},
				},
			}

			if len(contents) > 0 && contents[len(contents)-1].Role == "function" {
				contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, functionPart)
			} else {
				contents = append(contents, GeminiChatContent{
					Role:  "function",
					Parts: []GeminiPart{functionPart},
				})
			}

			continue
		} else {
			openaiMessagePart := openaiContent.ParseContent()
			for _, openaiPart := range openaiMessagePart {
				if openaiPart.Type == types.ContentTypeText {
					if openaiPart.Text == "" {
						continue
					}
					content.Parts = append(content.Parts, GeminiPart{Text: openaiPart.Text})
				} else if openaiPart.Type == types.ContentTypeImageURL {
					mimeType, body, err := base.DecodeChatMediaDataURI(openaiPart.ImageURL.URL)
					if err != nil {
						return nil, "", common.ErrorWrapper(err, "image_url_invalid", http.StatusBadRequest)
					}
					content.Parts = append(content.Parts, GeminiPart{
						InlineData: &GeminiInlineData{
							MimeType: mimeType,
							Data:     base64.StdEncoding.EncodeToString(body),
						},
					})
				}
			}
		}
		contents = append(contents, content)
	}

	return contents, strings.Join(systemContent, "\n"), nil
}

func createSystemResponse(text string) GeminiChatContent {
	return GeminiChatContent{
		Role: "model",
		Parts: []GeminiPart{
			{
				Text: text,
			},
		},
	}
}

type ModelListResponse struct {
	Models []ModelDetails `json:"models"`
}

type ModelDetails struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

type GeminiErrorWithStatusCode struct {
	GeminiErrorResponse
	StatusCode int  `json:"status_code"`
	LocalError bool `json:"-"`
}

func (e *GeminiErrorWithStatusCode) ToOpenAiError() *types.OpenAIErrorWithStatusCode {
	return &types.OpenAIErrorWithStatusCode{
		StatusCode: e.StatusCode,
		OpenAIError: types.OpenAIError{
			Code:    e.ErrorInfo.Code,
			Type:    e.ErrorInfo.Status,
			Message: e.ErrorInfo.Message,
		},
		LocalError: e.LocalError,
	}
}

type GeminiErrors []*GeminiErrorResponse

func (e *GeminiErrors) Error() *GeminiErrorResponse {
	return (*e)[0]
}

type GeminiImageRequest struct {
	Instances  []GeminiImageInstance `json:"instances"`
	Parameters GeminiImageParameters `json:"parameters"`
}

type GeminiImageInstance struct {
	Prompt string `json:"prompt"`
}

type GeminiImageParameters struct {
	PersonGeneration string `json:"personGeneration,omitempty"`
	AspectRatio      string `json:"aspectRatio,omitempty"`
	SampleCount      int    `json:"sampleCount,omitempty"`
}

type GeminiImageResponse struct {
	Predictions []GeminiImagePrediction `json:"predictions"`
}

type GeminiImagePrediction struct {
	BytesBase64Encoded string `json:"bytesBase64Encoded"`
	MimeType           string `json:"mimeType"`
	RaiFilteredReason  string `json:"raiFilteredReason,omitempty"`
	SafetyAttributes   any    `json:"safetyAttributes,omitempty"`
}

func isEmptyOrOnlyNewlines(s string) bool {
	trimmed := strings.TrimSpace(s)
	return trimmed == ""
}

type GeminiGroundingMetadata struct {
	GroundingChunks  []GeminiGroundingChunk `json:"groundingChunks,omitempty"`
	WebSearchQueries []string               `json:"webSearchQueries,omitempty"`
}

type GeminiGroundingChunk struct {
	Web *GeminiGroundingChunkWeb `json:"web,omitempty"`
}

type GeminiGroundingChunkWeb struct {
	Uri   string `json:"uri,omitempty"`
	Title string `json:"title,omitempty"`
}

// checks if googleSearch tool has "show" parameter
func showGoogleSearchMeta(request *types.ChatCompletionRequest) bool {
	functions := request.GetFunctions()
	if functions == nil {
		return false
	}

	for _, function := range functions {
		if function.Name == "googleSearch" && function.Parameters != nil {
			if paramStr, ok := function.Parameters.(string); ok && paramStr == "show" {
				return true
			}
		}
	}

	return false
}

// formats grounding metadata as markdown citation
func formatGroundingMetadataAsMarkdown(metadata *GeminiGroundingMetadata) string {
	if metadata == nil || len(metadata.GroundingChunks) == 0 {
		return ""
	}
	var result strings.Builder
	// Add search queries
	if len(metadata.WebSearchQueries) > 0 {
		result.WriteString("> Searched ")
		for i, query := range metadata.WebSearchQueries {
			if i > 0 {
				result.WriteString(" and ")
			}
			result.WriteString(fmt.Sprintf(`"%s"`, query))
		}
		result.WriteString("\n")
	}
	// Add grounding chunks as numbered list
	linkCount := 0
	for _, chunk := range metadata.GroundingChunks {
		if chunk.Web != nil && chunk.Web.Uri != "" {
			linkCount++
			title := chunk.Web.Title
			if title == "" {
				title = chunk.Web.Uri
			}
			result.WriteString(fmt.Sprintf("> %d. [%s](%s)\n", linkCount, title, chunk.Web.Uri))
		}
	}
	return result.String()
}
