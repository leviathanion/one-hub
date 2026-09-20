package types

import (
	"encoding/json"
	"net/http"
)

// ImageResponseWrapper lets a native adapter choose delivery from the actual
// upstream content type. Existing unary providers keep their original contract.
type ImageResponseWrapper struct {
	JSON                 *ImageResponse
	Stream               *http.Response
	ObserveProviderEvent func([]byte)
}

// UnmarshalJSON projects routing and accounting fields only. JSON image/mask
// values belong to the upstream union, not multipart.FileHeader.
func (r *ImageEditRequest) UnmarshalJSON(data []byte) error {
	var fields struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              int    `json:"n"`
		Size           string `json:"size"`
		ResponseFormat string `json:"response_format"`
		User           string `json:"user"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*r = ImageEditRequest{Model: fields.Model, Prompt: fields.Prompt, N: fields.N, Size: fields.Size, ResponseFormat: fields.ResponseFormat, User: fields.User}
	return nil
}
