package openai

import (
	"net/http"
	"one-api/common/config"
	"one-api/types"
)

func (p *OpenAIProvider) CreateImageVariations(request *types.ImageEditRequest) (*types.ImageResponse, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.getRequestImageBody(config.RelayModeImagesVariations, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	response := &OpenAIProviderImageResponse{}
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

	applyImageEvidence(p.Usage, &response.ImageResponse, response.confirmedDataCount(p.ProviderRawJSONReplay))
	if p.ProviderRawJSONReplay {
		response.EnableProviderRawJSONReplay()
	}

	return &response.ImageResponse, nil
}
