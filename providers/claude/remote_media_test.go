package claude

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"one-api/providers/base"
)

type nativeMediaFetcher struct {
	calls int
	err   error
	mime  string
}

func (f *nativeMediaFetcher) Fetch(string) (string, []byte, error) {
	f.calls++
	if f.err != nil {
		return "", nil, f.err
	}
	mime := f.mime
	if mime == "" {
		mime = "image/png"
	}
	return mime, []byte("remote-image"), nil
}

func nativeClaudeMediaRequest(blocks ...any) *ClaudeRequest {
	return &ClaudeRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: 16,
		Messages:  []Message{{Role: "user", Content: blocks}},
	}
}

func TestMaterializeNativeRemoteMediaHandlesNestedToolResultWithoutMutatingInput(t *testing.T) {
	request := nativeClaudeMediaRequest(
		map[string]any{
			"type": ContentTypeToolResult,
			"content": []any{
				map[string]any{"type": "text", "text": "look"},
				map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}},
			},
		},
		map[string]any{
			"type":  "tool_use",
			"input": map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://must-not-fetch.example/a.png"}},
		},
	)
	fetcher := &nativeMediaFetcher{}
	prepared, err := MaterializeNativeRemoteMedia(request, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("fetch calls=%d, want 1", fetcher.calls)
	}
	originalNested := request.Messages[0].Content.([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)
	if originalNested["source"].(map[string]any)["type"] != "url" {
		t.Fatalf("input request was mutated: %#v", originalNested)
	}
	preparedNested := prepared.Messages[0].Content.([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)
	source := preparedNested["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != base64.StdEncoding.EncodeToString([]byte("remote-image")) {
		t.Fatalf("unexpected prepared source: %#v", source)
	}
}

func TestMaterializeNativeRemoteMediaValidatesLocalDataBeforeNetwork(t *testing.T) {
	request := nativeClaudeMediaRequest(
		map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}},
		map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "data:image/png;base64,not-valid!"}},
	)
	fetcher := &nativeMediaFetcher{}
	if _, err := MaterializeNativeRemoteMedia(request, fetcher); err == nil {
		t.Fatal("expected invalid data URI to fail")
	}
	if fetcher.calls != 0 {
		t.Fatalf("network started before local validation: %d", fetcher.calls)
	}
}

func TestMaterializeNativeRemoteMediaDataURINeverFetches(t *testing.T) {
	raw := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("local-image"))
	request := nativeClaudeMediaRequest(map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": raw}})
	fetcher := &nativeMediaFetcher{err: errors.New("must not fetch")}
	prepared, err := MaterializeNativeRemoteMedia(request, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 0 {
		t.Fatalf("data URI triggered %d fetches", fetcher.calls)
	}
	source := prepared.Messages[0].Content.([]any)[0].(map[string]any)["source"].(map[string]any)
	if source["type"] != "base64" || source["data"] != base64.StdEncoding.EncodeToString([]byte("local-image")) {
		t.Fatalf("unexpected prepared data source: %#v", source)
	}
}

func TestValidateMaterializableNativeRemoteMediaRejectsDocumentsAndCount(t *testing.T) {
	document := nativeClaudeMediaRequest(map[string]any{"type": "document", "source": map[string]any{"type": "url", "url": "https://example.com/a.pdf"}})
	if err := ValidateMaterializableNativeRemoteMedia(document, SummarizeNativeRemoteMedia(document)); err == nil || !strings.Contains(err.Error(), "document URL") {
		t.Fatalf("expected document URL rejection, got %v", err)
	}

	blocks := make([]any, base.MaxChatRemoteMediaItems+1)
	for index := range blocks {
		blocks[index] = map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}}
	}
	tooMany := nativeClaudeMediaRequest(blocks...)
	if err := ValidateMaterializableNativeRemoteMedia(tooMany, SummarizeNativeRemoteMedia(tooMany)); err == nil || !strings.Contains(err.Error(), "more than 16") {
		t.Fatalf("expected count rejection, got %v", err)
	}
}

func TestMaterializeNativeRemoteMediaRejectsUnrepresentableSourceFieldsBeforeFetch(t *testing.T) {
	request := nativeClaudeMediaRequest(map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":          "url",
			"url":           "https://example.com/a.png",
			"future_option": true,
		},
	})
	fetcher := &nativeMediaFetcher{}
	if _, err := MaterializeNativeRemoteMedia(request, fetcher); err == nil || !strings.Contains(err.Error(), "future_option") {
		t.Fatalf("expected source extension rejection, got %v", err)
	}
	if fetcher.calls != 0 {
		t.Fatalf("source extension rejection fetched %d URLs", fetcher.calls)
	}
}

func TestMaterializeNativeRemoteMediaRejectsNonRasterFetcherMIME(t *testing.T) {
	request := nativeClaudeMediaRequest(map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}})
	fetcher := &nativeMediaFetcher{mime: "application/pdf"}
	if _, err := MaterializeNativeRemoteMedia(request, fetcher); err == nil || !strings.Contains(err.Error(), "application/pdf") {
		t.Fatalf("expected non-raster MIME rejection, got %v", err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("fetch calls=%d, want 1", fetcher.calls)
	}
}
