package vertexai

import (
	"encoding/json"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/claude"
	"one-api/providers/gemini"
	"one-api/providers/vertexai/category"
	"one-api/types"
)

func (VertexAIProviderFactory) AssessChatRequest(_ *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	providerCategory, err := category.GetCategory(canonicalModel)
	if err != nil || providerCategory == nil || providerCategory.ChatComplete == nil || providerCategory.ResponseChatComplete == nil {
		return base.ChatRequestSupport{}, &base.RequestCapabilityError{Param: "model", Message: "model cannot be represented by the Vertex AI adapter"}
	}
	switch providerCategory.Category {
	case "claude":
		return base.ChatRequestSupport{}, claude.ValidateChatRequest(request, "Vertex Claude")
	case "gemini":
		return base.ChatRequestSupport{}, gemini.ValidateNativeChatRequest(canonicalModel, request, "Vertex Gemini")
	default:
		return base.ChatRequestSupport{}, &base.RequestCapabilityError{Param: "model", Message: "model cannot be represented by the Vertex AI adapter"}
	}
}

func (VertexAIProviderFactory) SupportsClaude(_ *model.Channel) bool { return true }
func (VertexAIProviderFactory) SupportsGemini(_ *model.Channel) bool { return true }

func (VertexAIProviderFactory) AssessClaudeRequest(_ *model.Channel, canonicalModel string, fields map[string]json.RawMessage) error {
	providerCategory, err := category.GetCategory(canonicalModel)
	if err != nil || providerCategory == nil || providerCategory.Category != "claude" {
		return &base.RequestCapabilityError{Param: "model", Message: "model cannot be represented by the Vertex Claude adapter"}
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	var projection claude.ClaudeRequest
	return json.Unmarshal(body, &projection)
}

func (VertexAIProviderFactory) AssessGeminiRequest(channel *model.Channel, canonicalModel string, fields map[string]json.RawMessage) error {
	providerCategory, err := category.GetCategory(canonicalModel)
	if err != nil || providerCategory == nil || providerCategory.Category != "gemini" {
		return &base.RequestCapabilityError{Param: "model", Message: "model cannot be represented by the Vertex Gemini adapter"}
	}
	return (gemini.GeminiProviderFactory{}).AssessGeminiRequest(channel, canonicalModel, fields)
}
