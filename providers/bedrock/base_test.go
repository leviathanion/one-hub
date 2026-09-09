package bedrock

import (
	"encoding/json"
	"io"
	"testing"

	"one-api/common/config"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/claude"
	"one-api/types"
)

type bedrockNativeMediaFetcher struct{ calls int }

func (f *bedrockNativeMediaFetcher) Fetch(string) (string, []byte, error) {
	f.calls++
	return "image/png", []byte("bedrock-image"), nil
}

func TestBedrockRemoteMediaPolicyMaterializesOnlyClaude(t *testing.T) {
	factory := BedrockProviderFactory{}
	for _, test := range []struct {
		model string
		want  base.RemoteMediaMode
	}{
		{model: "claude-sonnet-4-20250514", want: base.RemoteMediaMaterialize},
		{model: "anthropic.claude-3-5-sonnet-20241022-v2:0", want: base.RemoteMediaMaterialize},
		{model: "amazon.nova-pro-v1:0", want: base.RemoteMediaReject},
	} {
		mode, err := factory.AssessChatRemoteMedia(nil, &types.ChatCompletionRequest{Model: test.model}, base.ChatRemoteMediaSummary{Items: 1, RemoteURLs: 1})
		if err != nil || mode != test.want {
			t.Fatalf("Bedrock model %q remote media policy = %v, %v; want %v", test.model, mode, err, test.want)
		}
	}
}

func TestBedrockNativeClaudeMediaPolicyMaterializesImagesAndRejectsDocuments(t *testing.T) {
	factory := BedrockProviderFactory{}
	image := &claude.ClaudeRequest{Model: "claude-sonnet-4-20250514", Messages: []claude.Message{{Content: []any{
		map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}},
	}}}}
	mode, err := factory.AssessNativeClaudeRemoteMedia(nil, image, claude.SummarizeNativeRemoteMedia(image))
	if err != nil || mode != base.RemoteMediaMaterialize {
		t.Fatalf("Bedrock native image policy=%v err=%v", mode, err)
	}
	document := &claude.ClaudeRequest{Model: "claude-sonnet-4-20250514", Messages: []claude.Message{{Content: []any{
		map[string]any{"type": "document", "source": map[string]any{"type": "url", "url": "https://example.com/a.pdf"}},
	}}}}
	if mode, err = factory.AssessNativeClaudeRemoteMedia(nil, document, claude.SummarizeNativeRemoteMedia(document)); err == nil || mode != base.RemoteMediaReject {
		t.Fatalf("Bedrock native document policy=%v err=%v, want Reject", mode, err)
	}
}

func TestBedrockNativeClaudeRequestSerializesPreparedMixedMedia(t *testing.T) {
	proxy := ""
	provider := (BedrockProviderFactory{}).Create(&model.Channel{
		Type:  config.ChannelTypeBedrock,
		Key:   "us-east-1|test-token",
		Proxy: &proxy,
	}).(*BedrockProvider)
	request := &claude.ClaudeRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 16,
		Messages: []claude.Message{{Role: "user", Content: []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": "ZXhpc3Rpbmc=", "future": true}},
			map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}},
			map[string]any{"type": claude.ContentTypeToolResult, "content": []any{
				map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/b.png"}},
			}},
		}}},
	}
	fetcher := &bedrockNativeMediaFetcher{}
	prepared, err := provider.MaterializeNativeClaudeRemoteMedia(request, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 2 {
		t.Fatalf("fetch calls=%d, want 2", fetcher.calls)
	}
	req, apiErr := provider.getClaudeRequest(prepared)
	if apiErr != nil {
		t.Fatalf("build Bedrock Claude request: %v", apiErr)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode Bedrock wire: %v", err)
	}
	if wire["model"] != nil || wire["anthropic_version"] == nil {
		t.Fatalf("unexpected Bedrock wrapper: %#v", wire)
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
