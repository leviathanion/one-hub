package openai

import (
	"net/http"
	"one-api/common/config"
	"one-api/types"
)

func (p *OpenAIProvider) CreateEmbeddings(request *types.EmbeddingRequest) (*types.EmbeddingResponse, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.GetRequestTextBody(config.RelayModeEmbeddings, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	response := &OpenAIProviderEmbeddingsResponse{}
	_, errWithCode = p.sendUnaryJSON(req, response)
	if errWithCode != nil {
		return nil, errWithCode
	}

	openaiErr := ErrorHandle(&response.OpenAIErrorResponse)
	if openaiErr != nil {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *openaiErr,
			StatusCode:  http.StatusBadRequest,
		}
		return nil, errWithCode
	}

	applyOpenAIEmbeddingUsage(p.Usage, response.Usage, response.Model)
	if p.ProviderRawJSONReplay {
		response.EnableProviderRawJSONReplay()
	}

	return &response.EmbeddingResponse, nil
}

func applyOpenAIEmbeddingUsage(target, providerUsage *types.Usage, actualModel string) {
	if target == nil || providerUsage == nil || !providerUsage.ProviderTokenFields["prompt_tokens"] || providerUsage.PromptTokens < 0 {
		return
	}
	if providerUsage.ProviderTokenFields["completion_tokens"] && providerUsage.CompletionTokens != 0 {
		return
	}
	candidate := *providerUsage
	candidate.CompletionTokens = 0
	candidate.TotalTokens = candidate.PromptTokens
	candidate.ProviderTokenFields = map[string]bool{
		"prompt_tokens":     true,
		"completion_tokens": true,
		"total_tokens":      true,
	}
	candidate.MarkProviderReported()
	candidate.MergeProviderAttribution(actualModel, "")
	*target = candidate
}
