package openai

import (
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/types"
)

func (p *OpenAIProvider) CreateImageGenerations(request *types.ImageRequest) (*types.ImageResponse, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.GetRequestTextBody(config.RelayModeImagesGenerations, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	response := &OpenAIProviderImageResponse{}
	_, errWithCode = p.sendUnaryJSON(req, response)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// 检测是否错误
	openaiErr := ErrorHandle(&response.OpenAIErrorResponse)
	if openaiErr != nil {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *openaiErr,
			StatusCode:  http.StatusBadRequest,
		}
		return nil, errWithCode
	}

	applyImageEvidence(p.Usage, &response.ImageResponse, response.confirmedDataCount(p.ProviderRawJSONReplay))
	if p.ProviderRawJSONReplay {
		response.EnableProviderRawJSONReplay()
	}

	return &response.ImageResponse, nil
}

func ApplyImageEvidence(target *types.Usage, response *types.ImageResponse) {
	applyImageEvidence(target, response, imageDataCount(response))
}

func imageDataCount(response *types.ImageResponse) *int {
	if response == nil || response.Data == nil {
		return nil
	}
	count := len(response.Data)
	return &count
}

func applyImageEvidence(target *types.Usage, response *types.ImageResponse, dataCount *int) {
	if target == nil || response == nil {
		return
	}
	if response.Usage != nil {
		response.Usage.MarkProviderReported()
		if tokenUsage := response.Usage.ToOpenAIUsage(); tokenUsage != nil {
			tokenUsage.RequireTokenExtraEvidence(
				config.UsageExtraInputTextTokens,
				config.UsageExtraInputImageTokens,
				config.UsageExtraOutputTextTokens,
				config.UsageExtraOutputImageTokens,
			)
			// Images usage requires both token sides.  total_tokens is a
			// redundant provider assertion: if it is present, validate it below;
			// if omitted, preserve the input/output evidence without inventing a
			// provider field.
			if tokenUsage.ProviderTokenFields["prompt_tokens"] &&
				tokenUsage.ProviderTokenFields["completion_tokens"] {
				tokenUsage.SetTokenExtraEvidenceGroups(
					[]string{config.UsageExtraInputTextTokens, config.UsageExtraInputImageTokens},
					[]string{config.UsageExtraOutputTextTokens, config.UsageExtraOutputImageTokens},
				)
				if tokenUsage.ProviderTokenFields["total_tokens"] &&
					tokenUsage.PromptTokens+tokenUsage.CompletionTokens != tokenUsage.TotalTokens {
					tokenUsage.ProviderTokenConflict = true
				}
				*target = *tokenUsage
			}
		}
	}
	if dataCount != nil {
		target.MarkProviderOperationUnits(*dataCount)
	}
	target.MergeProviderAttribution(response.Model, "")
}

func (r *OpenAIProviderImageResponse) confirmedDataCount(exactWire bool) *int {
	if r == nil {
		return nil
	}
	if exactWire {
		return r.providerDataCount
	}
	return imageDataCount(&r.ImageResponse)
}

func IsWithinRange(element string, value int) bool {
	if _, ok := common.DalleGenerationImageAmounts[element]; !ok {
		return true
	}
	minCount := common.DalleGenerationImageAmounts[element][0]
	maxCount := common.DalleGenerationImageAmounts[element][1]

	return value >= minCount && value <= maxCount
}
