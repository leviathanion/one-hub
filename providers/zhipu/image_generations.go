package zhipu

import (
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/types"
	"strings"
	"time"
)

func (p *ZhipuProvider) CreateImageGenerations(request *types.ImageRequest) (*types.ImageResponse, *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(config.RelayModeImagesGenerations)
	if errWithCode != nil {
		return nil, errWithCode
	}
	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url)
	if fullRequestURL == "" {
		return nil, common.ErrorWrapper(nil, "invalid_zhipu_config", http.StatusInternalServerError)
	}

	// 获取请求头
	headers := p.GetRequestHeaders()

	zhipuRequest := convertFromIamgeOpenai(request)
	// 创建请求
	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(zhipuRequest), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	defer req.Body.Close()

	zhipuResponse := &ZhipuImageGenerationResponse{}

	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, zhipuResponse, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	return p.convertToImageOpenai(zhipuResponse)
}

func (p *ZhipuProvider) convertToImageOpenai(response *ZhipuImageGenerationResponse) (openaiResponse *types.ImageResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	aiError := errorHandle(&response.Error)
	if aiError != nil {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *aiError,
			StatusCode:  http.StatusBadRequest,
		}
		return
	}

	openaiResponse = &types.ImageResponse{
		Created: time.Now().Unix(),
		Data:    response.Data,
	}

	if p.Usage != nil {
		count := zhipuImageResultCount(response.Data)
		if count > 0 {
			p.Usage.MarkProviderOperationUnits(count)
			p.Usage.MergeProviderAttribution(response.Model, "")
		}
	}

	return
}

func zhipuImageResultCount(data []types.ImageResponseDataInner) int {
	count := 0
	for _, item := range data {
		if strings.TrimSpace(item.URL) != "" || strings.TrimSpace(item.B64JSON) != "" {
			count++
		}
	}
	return count
}

func convertFromIamgeOpenai(request *types.ImageRequest) *ZhipuImageGenerationRequest {
	return &ZhipuImageGenerationRequest{
		Model:  request.Model,
		Prompt: request.Prompt,
	}
}
