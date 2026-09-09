package vertexai

import (
	"encoding/json"
	"io"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/claude"
	"one-api/types"
)

type vertexNativeMediaFetcher struct{ calls int }

func (f *vertexNativeMediaFetcher) Fetch(string) (string, []byte, error) {
	f.calls++
	return "image/png", []byte("vertex-image"), nil
}

func TestVertexAIKeyConfigReadsJSONOther(t *testing.T) {
	provider := &VertexAIProvider{
		BaseProvider: base.BaseProvider{
			Channel: &model.Channel{Other: `{"region":"us-central1","project_id":"project-a"}`},
		},
	}

	getKeyConfig(provider)
	if provider.Region != "us-central1" || provider.ProjectID != "project-a" {
		t.Fatalf("expected JSON Other region/project_id, got region=%q project=%q", provider.Region, provider.ProjectID)
	}
}

func TestVertexAIRemoteMediaPolicyMaterializesOnlySupportedChatCategories(t *testing.T) {
	factory := VertexAIProviderFactory{}
	for _, test := range []struct {
		model string
		want  base.RemoteMediaMode
	}{
		{model: "claude-sonnet-4", want: base.RemoteMediaMaterialize},
		{model: "gemini-2.5-pro", want: base.RemoteMediaMaterialize},
		{model: "imagen-3", want: base.RemoteMediaReject},
	} {
		mode, err := factory.AssessChatRemoteMedia(nil, &types.ChatCompletionRequest{Model: test.model}, base.ChatRemoteMediaSummary{Items: 1, RemoteURLs: 1})
		if err != nil || mode != test.want {
			t.Fatalf("Vertex model %q remote media policy = %v, %v; want %v", test.model, mode, err, test.want)
		}
	}
}

func TestVertexAINativeClaudeMediaPolicyUsesMappedClaudeModel(t *testing.T) {
	factory := VertexAIProviderFactory{}
	request := &claude.ClaudeRequest{
		Model: "claude-sonnet-4",
		Messages: []claude.Message{{Content: []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}},
		}}},
	}
	mode, err := factory.AssessNativeClaudeRemoteMedia(nil, request, claude.SummarizeNativeRemoteMedia(request))
	if err != nil || mode != base.RemoteMediaMaterialize {
		t.Fatalf("Vertex native Claude image policy=%v err=%v", mode, err)
	}
	request.Model = "gemini-2.5-pro"
	if mode, err = factory.AssessNativeClaudeRemoteMedia(nil, request, claude.SummarizeNativeRemoteMedia(request)); err != nil || mode != base.RemoteMediaReject {
		t.Fatalf("Vertex non-Claude native policy=%v err=%v, want Reject", mode, err)
	}
}

func TestVertexAINativeClaudeRequestSerializesPreparedMixedMedia(t *testing.T) {
	cache.InitCacheManager()
	const projectID = "native-media-project"
	cacheKey := TokenCacheKey + ":" + projectID
	if err := cache.SetCache(cacheKey, "test-token", time.Minute); err != nil {
		t.Fatalf("seed Vertex token cache: %v", err)
	}
	t.Cleanup(func() { _ = cache.DeleteCache(cacheKey) })
	proxy := ""
	provider := (VertexAIProviderFactory{}).Create(&model.Channel{
		Type:  config.ChannelTypeVertexAI,
		Key:   `{}`,
		Proxy: &proxy,
		Other: `{"region":"us-central1","project_id":"` + projectID + `"}`,
	}).(*VertexAIProvider)
	request := &claude.ClaudeRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: 16,
		Messages: []claude.Message{{Role: "user", Content: []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": "ZXhpc3Rpbmc=", "future": true}},
			map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}},
			map[string]any{"type": claude.ContentTypeToolResult, "content": []any{
				map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/b.png"}},
			}},
		}}},
	}
	fetcher := &vertexNativeMediaFetcher{}
	prepared, err := provider.MaterializeNativeClaudeRemoteMedia(request, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 2 {
		t.Fatalf("fetch calls=%d, want 2", fetcher.calls)
	}
	req, apiErr := provider.getClaudeRequest(prepared)
	if apiErr != nil {
		t.Fatalf("build Vertex Claude request: %v", apiErr)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode Vertex wire: %v", err)
	}
	if wire["model"] != nil || wire["anthropic_version"] == nil {
		t.Fatalf("unexpected Vertex wrapper: %#v", wire)
	}
	content := wire["messages"].([]any)[0].(map[string]any)["content"].([]any)
	existing := content[0].(map[string]any)["source"].(map[string]any)
	materialized := content[1].(map[string]any)["source"].(map[string]any)
	nested := content[2].(map[string]any)["content"].([]any)[0].(map[string]any)["source"].(map[string]any)
	if existing["type"] != "base64" || existing["data"] != "ZXhpc3Rpbmc=" || existing["future"] != true {
		t.Fatalf("existing base64 source changed: %#v", existing)
	}
	if materialized["type"] != "base64" || nested["type"] != "base64" {
		t.Fatalf("URL sources were not materialized: top=%#v nested=%#v", materialized, nested)
	}
}

func TestVertexAIImageUsageUsesProviderPredictionCount(t *testing.T) {
	usage := &types.Usage{}
	applyVertexAIImageUsage(usage, "imagen-3", 2)
	if usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 2 || usage.ResponseModel != "imagen-3" {
		t.Fatalf("Vertex image prediction count did not become provider operation evidence: %+v", usage)
	}
}
