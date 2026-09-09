package openai

import (
	"encoding/json"
	"net/http"

	"one-api/types"
)

type providerRawJSONCaptureEnabler interface {
	EnableProviderRawJSONCapture()
}

func (p *OpenAIProvider) sendUnaryJSON(req *http.Request, response providerRawJSONCaptureEnabler) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	var (
		httpResponse *http.Response
		apiErr       *types.OpenAIErrorWithStatusCode
	)
	if p.ProviderRawJSONReplay {
		response.EnableProviderRawJSONCapture()
		httpResponse, apiErr = p.Requester.SendRequestPreservingRedirect(req, response, false)
	} else {
		httpResponse, apiErr = p.Requester.SendRequest(req, response, false)
	}
	if apiErr == nil {
		p.captureProviderResponseHeaders(httpResponse, p.ProviderRawJSONReplay)
	}
	return httpResponse, apiErr
}

// Captured JSON remains the delivery source; a future response union must not
// make independent billing/attribution observations unavailable.
func (r *OpenAIProviderChatResponse) DecodeCapturedProviderJSON(raw []byte) error {
	var decoded OpenAIProviderChatResponse
	if json.Unmarshal(raw, &decoded) == nil {
		*r = decoded
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*r = OpenAIProviderChatResponse{}
	types.DecodeOptionalRawField(fields, "id", &r.ID)
	types.DecodeOptionalRawField(fields, "object", &r.Object)
	types.DecodeOptionalRawField(fields, "created", &r.Created)
	types.DecodeOptionalRawField(fields, "model", &r.Model)
	types.DecodeOptionalRawField(fields, "service_tier", &r.ServiceTier)
	types.DecodeOptionalRawField(fields, "usage", &r.Usage)
	types.DecodeOptionalRawField(fields, "choices", &r.Choices)
	types.DecodeOptionalRawField(fields, "error", &r.Error)
	return nil
}

func (r *OpenAIProviderCompletionResponse) DecodeCapturedProviderJSON(raw []byte) error {
	var decoded OpenAIProviderCompletionResponse
	if json.Unmarshal(raw, &decoded) == nil {
		*r = decoded
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*r = OpenAIProviderCompletionResponse{}
	types.DecodeOptionalRawField(fields, "id", &r.ID)
	types.DecodeOptionalRawField(fields, "object", &r.Object)
	types.DecodeOptionalRawField(fields, "created", &r.Created)
	types.DecodeOptionalRawField(fields, "model", &r.Model)
	types.DecodeOptionalRawField(fields, "usage", &r.Usage)
	types.DecodeOptionalRawField(fields, "choices", &r.Choices)
	types.DecodeOptionalRawField(fields, "error", &r.Error)
	return nil
}

func (r *OpenAIProviderEmbeddingsResponse) DecodeCapturedProviderJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*r = OpenAIProviderEmbeddingsResponse{}
	types.DecodeOptionalRawField(fields, "object", &r.Object)
	types.DecodeOptionalRawField(fields, "model", &r.Model)
	types.DecodeOptionalRawField(fields, "usage", &r.Usage)
	types.DecodeOptionalRawField(fields, "error", &r.Error)
	return nil
}

func (r *OpenAIProviderModerationResponse) DecodeCapturedProviderJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*r = OpenAIProviderModerationResponse{}
	types.DecodeOptionalRawField(fields, "id", &r.ID)
	types.DecodeOptionalRawField(fields, "model", &r.Model)
	types.DecodeOptionalRawField(fields, "error", &r.Error)
	return nil
}

func (r *OpenAIProviderImageResponse) DecodeCapturedProviderJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*r = OpenAIProviderImageResponse{}
	types.DecodeOptionalRawField(fields, "created", &r.Created)
	types.DecodeOptionalRawField(fields, "model", &r.Model)
	types.DecodeOptionalRawField(fields, "usage", &r.Usage)
	types.DecodeOptionalRawField(fields, "error", &r.Error)
	if rawData, ok := fields["data"]; ok {
		var data []json.RawMessage
		if json.Unmarshal(rawData, &data) == nil && data != nil {
			count := len(data)
			r.providerDataCount = &count
		}
	}
	return nil
}
