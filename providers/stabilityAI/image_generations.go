package stabilityAI

import (
	"bytes"
	"image/png"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/storage"
	"one-api/common/utils"
	providersBase "one-api/providers/base"
	"one-api/types"
	"strings"
	"time"
)

const (
	maxStabilityImageBytes            = providersBase.MaxChatRemoteMediaItemBytes
	maxStabilityImagePixels    uint64 = 8 << 20
	maxStabilityImageDimension uint64 = 16384
)

func convertModelName(modelName string) string {
	if modelName == "stable-image-core" {
		return "core"
	}

	return "sd3"
}

func (p *StabilityAIProvider) CreateImageGenerations(request *types.ImageRequest) (*types.ImageResponse, *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(config.RelayModeImagesGenerations)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url, convertModelName(request.Model))
	if fullRequestURL == "" {
		return nil, common.ErrorWrapper(nil, "invalid_stabilityAI_config", http.StatusInternalServerError)
	}

	// 获取请求头
	headers := p.GetRequestHeaders()
	headers["Accept"] = "application/json; type=image/png"

	var formBody bytes.Buffer
	builder := p.Requester.CreateFormBuilder(&formBody)
	builder.WriteField("prompt", request.Prompt)
	builder.WriteField("output_format", "png")
	if request.Model != "stable-image-core" {
		builder.WriteField("model", request.Model)
	}
	builder.Close()

	req, err := p.Requester.NewRequest(
		http.MethodPost,
		fullRequestURL,
		p.Requester.WithBody(&formBody),
		p.Requester.WithHeader(headers),
		p.Requester.WithContentType(builder.FormDataContentType()))
	req.ContentLength = int64(formBody.Len())

	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}

	stabilityAIResponse := &generateResponse{}

	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, stabilityAIResponse, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	imageBody, hasImage := decodeStabilityImage(stabilityAIResponse)

	openaiResponse := &types.ImageResponse{
		Created: time.Now().Unix(),
	}

	imgUrl := ""
	if hasImage && (request.ResponseFormat == "" || request.ResponseFormat == "url") {
		imgUrl = storage.Upload(imageBody, utils.GetUUID()+".png")
	}

	if imgUrl == "" {
		openaiResponse.Data = []types.ImageResponseDataInner{{B64JSON: stabilityAIResponse.Image}}
	} else {
		openaiResponse.Data = []types.ImageResponseDataInner{{URL: imgUrl}}
	}

	if hasImage && p.Usage != nil {
		p.Usage.MarkProviderOperationUnits(1)
	}

	return openaiResponse, nil
}

func decodeStabilityImage(response *generateResponse) ([]byte, bool) {
	if response == nil {
		return nil, false
	}
	finishReason := strings.ToUpper(strings.TrimSpace(response.FinishReason))
	if finishReason != "" && finishReason != "SUCCESS" {
		return nil, false
	}
	body, err := providersBase.DecodeBase64Bounded(response.Image, maxStabilityImageBytes)
	if err != nil || len(body) == 0 {
		return nil, false
	}
	config, err := png.DecodeConfig(bytes.NewReader(body))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return nil, false
	}
	width, height := uint64(config.Width), uint64(config.Height)
	if width > maxStabilityImageDimension || height > maxStabilityImageDimension || height > maxStabilityImagePixels/width {
		return nil, false
	}
	if _, err := png.Decode(bytes.NewReader(body)); err != nil {
		return nil, false
	}
	return body, true
}
