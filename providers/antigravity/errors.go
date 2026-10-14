package antigravity

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"one-api/common"
	"one-api/common/providerresponse"
	"one-api/providers/antigravity/wire"
	"one-api/providers/base"
	"one-api/types"
)

func capabilityError(err error) error {
	var failure *wire.CapabilityError
	if errors.As(err, &failure) {
		return &base.RequestCapabilityError{Param: failure.Param, Message: failure.Message}
	}
	return err
}

func requestError(err error) *types.OpenAIErrorWithStatusCode {
	result := common.ErrorWrapperLocal(err, "request_not_representable", http.StatusBadRequest)
	var failure *wire.CapabilityError
	if errors.As(err, &failure) {
		result.Param = failure.Param
	}
	var policyFailure *base.RequestCapabilityError
	if errors.As(err, &policyFailure) {
		result.Param = policyFailure.Param
	}
	result.UpstreamNotAttempted = true
	return result
}

func (p *AntigravityProvider) handleHTTPError(resp *http.Response) *types.OpenAIError {
	// requester 已对错误正文限长，仍显式限定此解析器直接调用的边界。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil
	}
	return googleError(raw)
}

func googleError(raw []byte) *types.OpenAIError {
	var fields map[string]json.RawMessage
	var errorFields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields["error"], &errorFields) != nil || errorFields == nil {
		return nil
	}
	var envelope struct {
		Error struct {
			Message string          `json:"message"`
			Status  string          `json:"status"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	// 投影失败不取消原生错误的交付资格；未知诊断联合类型仍保留在 raw 中。
	_ = json.Unmarshal(raw, &envelope)
	if envelope.Error.Message == "" {
		envelope.Error.Message = "Antigravity 上游返回错误"
	}
	return &types.OpenAIError{Message: envelope.Error.Message, Type: "upstream_error", Param: envelope.Error.Status, Code: envelope.Error.Code}
}

func (p *AntigravityProvider) responseError(err error, native bool) *types.OpenAIErrorWithStatusCode {
	var existing *types.OpenAIErrorWithStatusCode
	if errors.As(err, &existing) {
		return providerresponse.SanitizeAPIError(existing, p.credentialValues()...)
	}
	result := common.StringErrorWrapper("Antigravity 响应读取或协议转换失败", "upstream_response_error", http.StatusBadGateway)
	result.UpstreamAccepted = true
	var upstream *wire.UpstreamError
	if errors.As(err, &upstream) {
		if native {
			result.RawBody, result.ReplayRawResponse = upstream.Raw, true
		}
		if mapped := googleError(upstream.Raw); mapped != nil {
			result.OpenAIError = *mapped
			var code int
			if rawCode, ok := mapped.Code.(json.RawMessage); ok && json.Unmarshal(rawCode, &code) == nil && code >= 400 && code <= 599 {
				result.StatusCode = code
			}
			result.ProviderAuthRejected = result.StatusCode == http.StatusUnauthorized
			result.ProviderRateLimited = result.StatusCode == http.StatusTooManyRequests
		}
	}
	return providerresponse.SanitizeAPIError(result, p.credentialValues()...)
}
