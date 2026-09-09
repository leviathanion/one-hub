package openai

import (
	"bytes"
	"encoding/json"
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
	return p != nil && stream && p.SupportStreamOptions && !p.ProviderRawJSONReplay
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
	if !modelPatch && !customPatchPossible {
		return raw, true, nil
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

// mergeProviderStreamUsage writes the provider-owned usage switch after the
// typed/native builder and channel custom parameters have both completed. It
// patches only the top-level stream_options field, preserving raw sibling and
// future fields in the rest of the request.
func mergeProviderStreamUsage(body []byte) ([]byte, error) {
	object, err := jsonobject.Parse(body)
	if err != nil {
		// Invalid provider-owned input remains upstream's responsibility. The
		// adapter cannot safely add a nested field to a malformed body.
		return body, nil
	}

	var options map[string]interface{}
	if rawOptions, exists := object.Fields["stream_options"]; exists {
		decoder := json.NewDecoder(bytes.NewReader(rawOptions))
		decoder.UseNumber()
		if err := decoder.Decode(&options); err != nil {
			return body, nil
		}
	}
	options = forceStreamOptionsMap(options)
	patchedOptions, err := json.Marshal(options)
	if err != nil {
		return nil, err
	}
	if rawOptions, exists := object.Fields["stream_options"]; exists && bytes.Equal(bytes.TrimSpace(rawOptions), patchedOptions) {
		return body, nil
	}
	if err := object.SetRaw("stream_options", patchedOptions); err != nil {
		return nil, err
	}
	return object.MarshalJSON()
}

func replaceRequestBody(req *http.Request, body []byte) {
	if req == nil {
		return
	}
	copyBody := append([]byte(nil), body...)
	req.Body = io.NopCloser(bytes.NewReader(copyBody))
	req.ContentLength = int64(len(copyBody))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(copyBody)), nil
	}
}

func forceStreamOptionsMap(options map[string]interface{}) map[string]interface{} {
	merged := make(map[string]interface{}, len(options)+1)
	for key, value := range options {
		merged[key] = value
	}
	merged["include_usage"] = true
	return merged
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
