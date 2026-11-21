package openai

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"net/http"
	"reflect"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/jsonobject"
	"one-api/providers/base"
	"one-api/types"
)

func nativeOpenAIJSONRelayMode(relayMode int) bool {
	switch relayMode {
	case config.RelayModeChatCompletions,
		config.RelayModeCompletions,
		config.RelayModeAudioSpeech,
		config.RelayModeEmbeddings,
		config.RelayModeModerations,
		config.RelayModeImagesGenerations:
		return true
	default:
		return false
	}
}

func shouldForceProviderStreamUsage(p *OpenAIProvider, stream bool) bool {
	return p != nil && stream && p.SupportStreamOptions
}

// 开启额外字段透传时，最终请求从原始 stream_options 合入渠道参数，
// typed 投影不覆盖未知选项；无原始字段的内部调用继续使用自身的选项。
func (p *OpenAIProvider) streamOptionsForTypedBody(options *types.StreamOptions, forceUsage bool) *types.StreamOptions {
	if !p.ProviderRawJSONReplay && !forceUsage {
		return nil
	}
	if options != nil && p.Channel != nil && p.Channel.AllowExtraBody && p.Context != nil {
		fields, err := common.GetReusableBodyMap(p.Context)
		if _, exists := fields["stream_options"]; err == nil && exists {
			return nil
		}
	}
	return options
}

func rejectUnsupportedImageStream(body []byte, contentType string) *types.OpenAIErrorWithStatusCode {
	err := ValidateImageStreamRequestBody(body, contentType)
	if err == nil {
		return nil
	}
	var requestErr *base.RequestCapabilityError
	if !errors.As(err, &requestErr) {
		requestErr = &base.RequestCapabilityError{Param: "stream", Message: err.Error()}
	}
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Code:    "unsupported_capability",
			Message: requestErr.Message,
			Param:   requestErr.Param,
			Type:    "invalid_request_error",
		},
		StatusCode: http.StatusBadRequest,
		LocalError: true,
	}
}

func rejectUnsupportedImageStreamRequest(req *http.Request) *types.OpenAIErrorWithStatusCode {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	body, err := requestBodyCopy(req)
	if err != nil {
		return common.ErrorWrapperLocal(err, "read_request_body_failed", http.StatusInternalServerError)
	}
	return rejectUnsupportedImageStream(body, req.Header.Get("Content-Type"))
}

func requestBodyCopy(req *http.Request) ([]byte, error) {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		defer body.Close()
		return io.ReadAll(body)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	restored := io.NopCloser(bytes.NewReader(body))
	req.Body = restored
	return body, nil
}

// patchNativeJSONModel changes only the proxy-owned routing field. Edits do not
// inherit the custom-parameter behavior of the general native JSON planner.
func patchNativeJSONModel(raw []byte, model string) ([]byte, error) {
	object, err := jsonobject.Parse(raw)
	if err != nil {
		return nil, err
	}
	if err := object.SetJSON("model", model); err != nil {
		return nil, err
	}
	return object.MarshalJSON()
}

// planNativeJSONBody starts from the accepted downstream body and changes only
// fields owned by this proxy. A body without an effective model/custom-parameter
// patch is returned byte-for-byte.
func (p *OpenAIProvider) planNativeJSONBody(modelName string) ([]byte, bool, error) {
	raw, exists, err := p.nativeRawBody()
	if err != nil || !exists {
		return nil, exists, err
	}

	customParams, err := p.CustomParameterHandler()
	if err != nil {
		return nil, true, err
	}
	modelPatch := p.OriginalModel != "" && p.OriginalModel != modelName
	customPatchPossible := len(customParams) > 0
	if preAdd, _ := customParams["pre_add"].(bool); preAdd {
		// pre_add is materialized before provider selection. Applying it here
		// would give the same configuration a second owner.
		customPatchPossible = false
	}
	if !customPatchPossible {
		if !modelPatch {
			return raw, true, nil
		}
		patched, err := patchNativeJSONModel(raw, modelName)
		return patched, true, err
	}

	object, err := jsonobject.Parse(raw)
	if err != nil {
		return nil, true, err
	}
	before, ok, err := p.GetRawBodyMap()
	if err != nil {
		return nil, true, err
	}
	if !ok || before == nil {
		return nil, true, errors.New("request body must be a JSON object")
	}
	after, ok, err := p.GetRawBodyMap()
	if err != nil {
		return nil, true, err
	}
	if !ok || after == nil {
		return nil, true, errors.New("request body must be a JSON object")
	}
	if modelPatch {
		after["model"] = modelName
	}
	if customPatchPossible {
		after = base.ApplyCustomParams(after, customParams, modelName, false)
	}
	out := object.Clone()
	changed := false
	for name, beforeValue := range before {
		afterValue, exists := after[name]
		if !exists {
			out.Delete(name)
			changed = true
			continue
		}
		if reflect.DeepEqual(beforeValue, afterValue) {
			continue
		}
		if err := out.SetJSON(name, afterValue); err != nil {
			return nil, true, err
		}
		changed = true
	}
	for name, value := range after {
		if _, exists := before[name]; exists {
			continue
		}
		if err := out.SetJSON(name, value); err != nil {
			return nil, true, err
		}
		changed = true
	}
	if !changed {
		return raw, true, nil
	}
	patched, err := out.MarshalJSON()
	return patched, true, err
}

// 返回有改动的正文和最终是否已知开启采集。nil 表示沿用现有 HTTP 请求体。
func mergeProviderStreamUsage(body []byte) (patched []byte, enabled bool) {
	var stream bool
	var options []byte
	var optionsAt int
	insertAt := visitStreamOptionMembers(body, func(key string, at int, value []byte) {
		switch key {
		case "stream":
			stream = bytes.Equal(value, []byte("true"))
		case "stream_options":
			options, optionsAt = value, at
		}
	})
	if insertAt < 0 || !stream {
		return nil, false
	}

	var start, end int
	var replacement string
	switch {
	case options == nil:
		start, end = insertAt, insertAt
		replacement = `,"stream_options":{"include_usage":true}`
	case bytes.Equal(options, []byte("null")):
		start, end = optionsAt, optionsAt+len(options)
		replacement = `{"include_usage":true}`
	default:
		var usage []byte
		var usageAt int
		replacement = `"include_usage":true`
		insertAt = visitStreamOptionMembers(options, func(key string, at int, value []byte) {
			replacement = `,"include_usage":true`
			if key == "include_usage" {
				usage, usageAt = value, at
			}
		})
		if insertAt < 0 {
			return nil, false
		}
		start, end = optionsAt+insertAt, optionsAt+insertAt
		if usage != nil {
			switch string(usage) {
			case "true":
				return nil, true
			case "false", "null":
				start, end = optionsAt+usageAt, optionsAt+usageAt+len(usage)
				replacement = "true"
			default:
				return nil, false
			}
		}
	}
	patched = make([]byte, 0, len(body)-(end-start)+len(replacement))
	patched = append(patched, body[:start]...)
	patched = append(patched, replacement...)
	return append(patched, body[end:]...), true
}

// 返回最后一个成员值后的插入位置，语法错误或当前层重名时返回 -1。
// 只借用入参切片；未知成员内部的键和值完全由上游解释。
func visitStreamOptionMembers(raw []byte, visit func(key string, at int, value []byte)) int {
	decoder := jsontext.NewDecoder(bytes.NewBuffer(raw), jsontext.AllowDuplicateNames(true))
	if token, err := decoder.ReadToken(); err != nil || token.Kind() != '{' {
		return -1
	}
	seen := make(map[string]struct{})
	for decoder.PeekKind() != '}' {
		token, err := decoder.ReadToken()
		if err != nil {
			return -1
		}
		key := token.String()
		if _, duplicate := seen[key]; duplicate {
			return -1
		}
		seen[key] = struct{}{}
		value, err := decoder.ReadValue()
		if err != nil {
			return -1
		}
		end := int(decoder.InputOffset())
		at := end - len(value)
		visit(key, at, raw[at:end])
	}
	insertAt := int(decoder.InputOffset())
	if _, err := decoder.ReadToken(); err != nil {
		return -1
	}
	if _, err := decoder.ReadToken(); err != io.EOF {
		return -1
	}
	return insertAt
}

func replaceRequestBody(req *http.Request, body []byte) {
	if req == nil {
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

func (p *OpenAIProvider) nativeRawBody() ([]byte, bool, error) {
	if p == nil || p.Context == nil || p.Context.Request == nil {
		return nil, false, nil
	}
	if body, ok := common.GetCanonicalRequestBody(p.Context); ok {
		if len(body) == 0 {
			return nil, false, nil
		}
		return body, true, nil
	}
	if p.Context.Request.Body == nil || p.Context.Request.Body == http.NoBody {
		return nil, false, nil
	}
	body, err := common.CacheRequestBody(p.Context)
	if err != nil {
		return nil, true, err
	}
	if len(body) == 0 {
		return nil, false, nil
	}
	return body, true, nil
}
