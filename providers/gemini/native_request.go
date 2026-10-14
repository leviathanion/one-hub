package gemini

import (
	"encoding/json"
	"one-api/common/jsonobject"
	"one-api/types"
)

// NativeRequest 保留原生请求字节；模型和流式控制来自 URL，不混入客户端 body。
type NativeRequest struct {
	Raw    []byte
	Model  string
	Stream bool
}

func ParseNativeRequest(raw []byte, model string, stream bool) (*NativeRequest, error) {
	object, err := jsonobject.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &NativeRequest{Raw: object.Raw, Model: model, Stream: stream}, nil
}

// Projection 仅供观察与预估；无法投影的未来字段不改变 Raw 的转发资格。
func (r *NativeRequest) Projection() (*GeminiChatRequest, error) {
	projection := &GeminiChatRequest{}
	err := json.Unmarshal(r.Raw, projection)
	projection.Model, projection.Stream = r.Model, r.Stream
	return projection, err
}

func (r *NativeRequest) UsesGoogleSearch() bool { return rawGeminiGoogleSearch(r.Raw) }

func (r *NativeRequest) UsesCachedContent() bool {
	var projection struct {
		CachedContent      string `json:"cachedContent"`
		CachedContentAlias string `json:"cached_content"`
	}
	_ = json.Unmarshal(r.Raw, &projection)
	return projection.CachedContent != "" || projection.CachedContentAlias != ""
}

func (r *NativeRequest) UsesInputImages() bool {
	projection, _ := r.Projection()
	return projection.UsesInputImages()
}

func (r *NativeRequest) ApplyUsageRequirements(usage *types.Usage) {
	applyGeminiUsageRequirements(usage, r.UsesCachedContent(), r.UsesInputImages())
}
