package claude

import (
	"encoding/json"

	"one-api/types"
)

func (r *ClaudeResponse) DecodeCapturedProviderJSON(raw []byte) error {
	var decoded ClaudeResponse
	if json.Unmarshal(raw, &decoded) == nil {
		*r = decoded
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*r = ClaudeResponse{}
	types.DecodeOptionalRawField(fields, "id", &r.Id)
	types.DecodeOptionalRawField(fields, "type", &r.Type)
	types.DecodeOptionalRawField(fields, "model", &r.Model)
	types.DecodeOptionalRawField(fields, "content", &r.Content)
	types.DecodeOptionalRawField(fields, "usage", &r.Usage)
	types.DecodeOptionalRawField(fields, "stop_reason", &r.StopReason)
	types.DecodeOptionalRawField(fields, "error", &r.Error)
	return nil
}
