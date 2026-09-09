package cloudflareAI

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/storage"
	"one-api/common/utils"
	"one-api/types"
	"time"
)

const (
	maxCloudflareImageResponseBytes int64  = 64 << 20
	maxCloudflareImagePixels        uint64 = 8 << 20
	maxCloudflareImageDimension     uint64 = 16384
	maxCloudflareImageDecodeBytes   uint64 = 64 << 20
)

func (p *CloudflareAIProvider) CreateImageGenerations(request *types.ImageRequest) (*types.ImageResponse, *types.OpenAIErrorWithStatusCode) {
	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(request.Model)
	if fullRequestURL == "" {
		return nil, common.ErrorWrapper(nil, "invalid_cloudflare_ai_config", http.StatusInternalServerError)
	}

	// 获取请求头
	headers := p.GetRequestHeaders()
	cfRequest := convertFromIamgeOpenai(request)

	// 创建请求
	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(cfRequest), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	defer req.Body.Close()

	resp, errWithCode := p.Requester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "image/png" {
		apiErr := common.StringErrorWrapper("invalid_image_response", "invalid_image_response", http.StatusInternalServerError)
		apiErr.UpstreamAccepted = true
		return nil, apiErr
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCloudflareImageResponseBytes+1))
	if err != nil {
		apiErr := common.ErrorWrapper(err, "read_response_failed", http.StatusInternalServerError)
		apiErr.UpstreamAccepted = true
		return nil, apiErr
	}
	if int64(len(body)) > maxCloudflareImageResponseBytes {
		apiErr := common.StringErrorWrapper("image response exceeds bounded limit", "invalid_image_response", http.StatusInternalServerError)
		apiErr.UpstreamAccepted = true
		return nil, apiErr
	}
	if err := validateCloudflarePNG(body); err != nil {
		apiErr := common.StringErrorWrapper(fmt.Sprintf("invalid image response: %v", err), "invalid_image_response", http.StatusInternalServerError)
		apiErr.UpstreamAccepted = true
		return nil, apiErr
	}

	url := ""
	if request.ResponseFormat == "" || request.ResponseFormat == "url" {
		url = storage.Upload(body, utils.GetUUID()+".png")
	}

	openaiResponse := &types.ImageResponse{
		Created: time.Now().Unix(),
	}

	if url == "" {
		base64Image := base64.StdEncoding.EncodeToString(body)
		openaiResponse.Data = []types.ImageResponseDataInner{{B64JSON: base64Image}}
	} else {
		openaiResponse.Data = []types.ImageResponseDataInner{{URL: url}}
	}

	if p.Usage != nil {
		p.Usage.MarkProviderOperationUnits(1)
		p.Usage.MergeProviderAttribution(request.Model, "")
	}

	return openaiResponse, nil
}

func validateCloudflarePNG(body []byte) error {
	if len(body) == 0 {
		return fmt.Errorf("empty image body")
	}
	config, err := png.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return err
	}
	width, height := uint64(config.Width), uint64(config.Height)
	if width == 0 || height == 0 {
		return fmt.Errorf("image dimensions must be positive")
	}
	if width > maxCloudflareImageDimension || height > maxCloudflareImageDimension {
		return fmt.Errorf("image dimensions exceed decoding budget")
	}
	if height > maxCloudflareImagePixels/width {
		return fmt.Errorf("image pixel count exceeds decoding budget")
	}
	pixels := width * height
	// RGBA64 is the largest image buffer png.Decode may materialize for a
	// supported PNG, so reserve eight bytes per pixel before decoding it.
	if pixels > maxCloudflareImagePixels || pixels > maxCloudflareImageDecodeBytes/8 {
		return fmt.Errorf("image decode allocation exceeds budget")
	}
	if _, err := png.Decode(bytes.NewReader(body)); err != nil {
		return err
	}
	return nil
}

func convertFromIamgeOpenai(request *types.ImageRequest) *ImageRequest {
	return &ImageRequest{
		Prompt: request.Prompt,
	}
}
