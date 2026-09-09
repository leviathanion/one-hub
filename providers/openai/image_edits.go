package openai

import (
	"bytes"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/types"
)

func (p *OpenAIProvider) CreateImageEdits(request *types.ImageEditRequest) (*types.ImageResponse, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.getRequestImageBody(config.RelayModeImagesEdits, request.Model, request)
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

func (p *OpenAIProvider) getRequestImageBody(relayMode int, ModelName string, request *types.ImageEditRequest) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(relayMode)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if errWithCode := p.validateAzureClassicAPIVersionForRequest(); errWithCode != nil {
		return nil, errWithCode
	}
	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url, ModelName)

	// 获取请求头
	headers := p.GetRequestHeaders()
	body, exists, rawErr := p.nativeRawBody()
	if rawErr != nil {
		return nil, common.ErrorWrapperLocal(rawErr, "read_request_body_failed", http.StatusInternalServerError)
	}
	var req *http.Request
	var err error
	if exists {
		if p.Context == nil || p.Context.Request == nil {
			return nil, common.StringErrorWrapperLocal("request body not found", "request_body_not_found", http.StatusInternalServerError)
		}
		contentType := p.Context.Request.Header.Get("Content-Type")
		if p.OriginalModel != "" && p.OriginalModel != request.Model {
			body, contentType, rawErr = rewriteMultipartModel(body, contentType, request.Model)
			if rawErr != nil {
				return nil, common.ErrorWrapperLocal(rawErr, "invalid_multipart_request", http.StatusBadRequest)
			}
		}
		if errWithCode := rejectUnsupportedImageStream(body, contentType); errWithCode != nil {
			return nil, errWithCode
		}
		req, err = p.Requester.NewRequest(
			http.MethodPost,
			fullRequestURL,
			p.Requester.WithBody(body),
			p.Requester.WithHeader(headers),
			p.Requester.WithContentType(contentType))
		if req != nil {
			req.ContentLength = int64(len(body))
		}
	} else {
		var formBody bytes.Buffer
		builder := p.Requester.CreateFormBuilder(&formBody)
		if err := imagesEditsMultipartForm(request, builder); err != nil {
			return nil, common.ErrorWrapper(err, "create_form_builder_failed", http.StatusInternalServerError)
		}
		req, err = p.Requester.NewRequest(
			http.MethodPost,
			fullRequestURL,
			p.Requester.WithBody(&formBody),
			p.Requester.WithHeader(headers),
			p.Requester.WithContentType(builder.FormDataContentType()))
		if req != nil {
			req.ContentLength = int64(formBody.Len())
		}
	}

	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	if exists && p.usesNativeOpenAIWire() && p.Context != nil && p.Context.Request != nil {
		if err := p.applyOpenAIHTTPHeaders(req.Header, requestctx.NewHeaderSnapshot(p.Context.Request.Header)); err != nil {
			return nil, common.ErrorWrapperLocal(err, "invalid_request_header", http.StatusBadRequest)
		}
	}

	return req, nil
}

func imagesEditsMultipartForm(request *types.ImageEditRequest, b requester.FormBuilder) (err error) {
	if request.Image != nil {
		err = b.CreateFormFile("image", request.Image)
		if err != nil {
			return fmt.Errorf("creating form image: %w", err)
		}
	}

	if request.Images != nil {
		for _, image := range request.Images {
			err = b.CreateFormFile("image[]", image)
			if err != nil {
				return fmt.Errorf("creating form images: %w", err)
			}
		}
	}

	err = b.WriteField("prompt", request.Prompt)
	if err != nil {
		return fmt.Errorf("writing prompt: %w", err)
	}

	err = b.WriteField("model", request.Model)
	if err != nil {
		return fmt.Errorf("writing model name: %w", err)
	}

	if request.Mask != nil {
		err = b.CreateFormFile("mask", request.Mask)
		if err != nil {
			return fmt.Errorf("writing mask: %w", err)
		}
	}

	if request.ResponseFormat != "" {
		err = b.WriteField("response_format", request.ResponseFormat)
		if err != nil {
			return fmt.Errorf("writing format: %w", err)
		}
	}

	if request.N != 0 {
		err = b.WriteField("n", fmt.Sprintf("%d", request.N))
		if err != nil {
			return fmt.Errorf("writing n: %w", err)
		}
	}

	if request.Size != "" {
		err = b.WriteField("size", request.Size)
		if err != nil {
			return fmt.Errorf("writing size: %w", err)
		}
	}

	if request.User != "" {
		err = b.WriteField("user", request.User)
		if err != nil {
			return fmt.Errorf("writing user: %w", err)
		}
	}

	return b.Close()
}
