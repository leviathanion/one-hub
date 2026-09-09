package gemini

import (
	"encoding/json"

	"one-api/types"
)

func (r *GeminiChatResponse) DecodeCapturedProviderJSON(raw []byte) error {
	var decoded GeminiChatResponse
	if json.Unmarshal(raw, &decoded) == nil {
		*r = decoded
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*r = GeminiChatResponse{}
	types.DecodeOptionalRawField(fields, "modelVersion", &r.ModelVersion)
	types.DecodeOptionalRawField(fields, "model", &r.Model)
	types.DecodeOptionalRawField(fields, "responseId", &r.ResponseId)
	types.DecodeOptionalRawField(fields, "usageMetadata", &r.UsageMetadata)
	types.DecodeOptionalRawField(fields, "error", &r.ErrorInfo)
	return nil
}
