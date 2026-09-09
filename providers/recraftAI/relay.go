package recraftAI

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"one-api/common"
	"one-api/types"
	"strings"
)

const maxRecraftNativeEvidenceBodyBytes int64 = 8 << 20

func (p *RecraftProvider) CreateRelay(url string) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url)
	if fullRequestURL == "" {
		return nil, common.ErrorWrapper(nil, "invalid_recraft_config", http.StatusInternalServerError)
	}

	// 获取请求头
	headers := p.GetRequestHeaders()
	body, exists := p.GetRawBody()
	if !exists {
		return nil, common.StringErrorWrapperLocal("request body not found", "request_body_not_found", http.StatusInternalServerError)
	}

	req, err := p.Requester.NewRequest(
		http.MethodPost,
		fullRequestURL,
		p.Requester.WithBody(body),
		p.Requester.WithHeader(headers),
		p.Requester.WithContentType(p.Context.Request.Header.Get("Content-Type")))
	req.ContentLength = p.Context.Request.ContentLength

	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}

	response, errWithCode := p.Requester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	if err := p.observeNativeOperationEvidence(url, response); err != nil {
		apiErr := common.ErrorWrapperLocal(err, "invalid_provider_response", http.StatusBadGateway)
		apiErr.UpstreamAccepted = true
		return nil, apiErr
	}

	return response, nil
}

// observeNativeOperationEvidence 有界缓存成功响应，既检查计量证据，也保留
// 原始 body 供 relay 原样交付。
func (p *RecraftProvider) observeNativeOperationEvidence(operationURL string, response *http.Response) error {
	if response == nil || response.Body == nil {
		return fmt.Errorf("provider response body is missing")
	}
	if !recraftNativeOperationNeedsEvidence(operationURL) {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRecraftNativeEvidenceBodyBytes+1))
	_ = response.Body.Close()
	if err != nil {
		return err
	}
	if int64(len(body)) > maxRecraftNativeEvidenceBodyBytes {
		return fmt.Errorf("provider response body exceeds %d bytes", maxRecraftNativeEvidenceBodyBytes)
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	if p.Usage != nil {
		if units := recraftNativeOperationUnits(operationURL, body); units > 0 {
			p.Usage.MarkProviderOperationUnits(units)
		}
	}
	return nil
}

func recraftNativeOperationNeedsEvidence(operationURL string) bool {
	switch strings.TrimSpace(operationURL) {
	case "/v1/images/vectorize", "/v1/images/removeBackground", "/v1/styles":
		return true
	default:
		return false
	}
}

func recraftNativeOperationUnits(operationURL string, body []byte) int {
	operationURL = strings.TrimSpace(operationURL)
	if len(bytes.TrimSpace(body)) == 0 {
		return 0
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return 0
	}
	switch operationURL {
	case "/v1/images/vectorize", "/v1/images/removeBackground":
		var image struct {
			URL string `json:"url"`
		}
		rawImage, ok := fields["image"]
		if !ok || json.Unmarshal(rawImage, &image) != nil {
			return 0
		}
		if strings.TrimSpace(image.URL) != "" {
			return 1
		}
	case "/v1/styles":
		var id string
		rawID, ok := fields["id"]
		if !ok || json.Unmarshal(rawID, &id) != nil {
			return 0
		}
		if strings.TrimSpace(id) != "" {
			return 1
		}
	}
	return 0
}
