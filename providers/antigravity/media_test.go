package antigravity

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"one-api/providers/base"
	"one-api/providers/claude"
	"one-api/types"
)

func TestPatchClaudePreparedMediaKeepsCanonicalRequest(t *testing.T) {
	const raw = `{"model":"raw-model","future":{"number":9007199254740993,"scale":1e+09},"messages":[{"role":"user","future":null,"content":[{"type":"tool_result","tool_use_id":"call_a","future":9007199254740993,"content":[{"type":"image","source":{"type":"url","url":"https://example.invalid/image","future":null},"signature":"original-signature"},{"type":"future","value":9007199254740993}]}]}]}`
	prepared := decodeClaudeMediaRequest(t, raw)
	prepared.Model = "typed-model-must-not-replace-raw"
	result := prepared.Messages[0].Content.([]any)[0].(map[string]any)
	image := result["content"].([]any)[0].(map[string]any)
	image["source"] = map[string]any{"type": "base64", "media_type": "image/png", "data": "aA=="}
	image["signature"] = "typed-signature-must-not-replace-raw"
	patched, err := patchClaudePreparedMedia([]byte(raw), prepared)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"model":"raw-model"`, `9007199254740993`, `1e+09`, `"future":null`, `"type":"base64"`, `"media_type":"image/png"`, `"data":"aA=="`, `"signature":"original-signature"`, `"type":"future"`} {
		if !strings.Contains(string(patched), fragment) {
			t.Fatalf("lost %s: %s", fragment, patched)
		}
	}
	if strings.Contains(string(patched), `"url"`) || strings.Contains(string(patched), "typed-") {
		t.Fatalf("incorrect source patch or typed request rewrite: %s", patched)
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(patched, &request); err != nil {
		t.Fatal(err)
	}
	if string(request["future"]) != `{"number":9007199254740993,"scale":1e+09}` {
		t.Fatalf("root extension changed: %s", request["future"])
	}
}

func TestPatchChatPreparedMediaOnlyChangesImageURL(t *testing.T) {
	const raw = `{"model":"raw-model","future":9007199254740993,"messages":[{"role":"user","extra":null,"content":[{"type":"text","text":"inspect","future":9007199254740993},{"type":"image_url","image_url":{"url":"https://example.invalid/image","detail":null,"future":1e+09},"future":9007199254740993}]}]}`
	var prepared types.ChatCompletionRequest
	if err := json.Unmarshal([]byte(raw), &prepared); err != nil {
		t.Fatal(err)
	}
	parts := prepared.Messages[0].ParseContent()
	parts[0].Text = "typed text must not replace raw"
	parts[1].ImageURL.URL = "data:image/png;base64,aA=="
	parts[1].ImageURL.Detail = "typed detail must not replace raw"
	prepared.Messages[0].Content = parts
	patched, err := patchChatPreparedMedia([]byte(raw), &prepared)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"model":"raw-model"`, `9007199254740993`, `1e+09`, `"extra":null`, `"text":"inspect"`, `"detail":null`, `"url":"data:image/png;base64,aA=="`} {
		if !strings.Contains(string(patched), fragment) {
			t.Fatalf("lost %s: %s", fragment, patched)
		}
	}
	if strings.Contains(string(patched), "typed ") || strings.Contains(string(patched), "https://") {
		t.Fatalf("unexpected typed fields or unpatched URL: %s", patched)
	}
}

func TestPreparedMediaWithoutChangesReturnsOriginalBytes(t *testing.T) {
	for _, raw := range []string{
		" \n {\"model\":\"m\",\"future\":9007199254740993,\"messages\":[{\"role\":\"user\",\"content\":\"text\"}]} \t",
		`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aA=="}},{"type":"image_url","image_url":{"url":"data:image/png;base64,aA=="}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"future","image":{"url":"https://example.invalid/not-a-known-media-field"}}]}]}`,
	} {
		for _, patch := range []func([]byte) ([]byte, error){
			func(body []byte) ([]byte, error) { return patchClaudePreparedMedia(body, nil) },
			func(body []byte) ([]byte, error) { return patchChatPreparedMedia(body, nil) },
		} {
			out, err := patch([]byte(raw))
			if err != nil || !bytes.Equal(out, []byte(raw)) {
				t.Fatalf("no-media request changed: %s, err=%v", out, err)
			}
		}
	}
	const remote = `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.invalid/image"}}]}]}`
	out, err := patchClaudePreparedMedia([]byte(remote), decodeClaudeMediaRequest(t, remote))
	if err != nil || !bytes.Equal(out, []byte(remote)) {
		t.Fatalf("unchanged URL request changed: %s, err=%v", out, err)
	}
}

func TestPatchClaudePreparedMediaRejectsMisalignedPaths(t *testing.T) {
	const raw = `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":[{"type":"image","source":{"type":"url","url":"https://example.invalid/image"}}]}]}]}`
	for _, tc := range []struct {
		name   string
		mutate func(*claude.ClaudeRequest)
	}{
		{"message count", func(r *claude.ClaudeRequest) { r.Messages = nil }},
		{"message role", func(r *claude.ClaudeRequest) { r.Messages[0].Role = "assistant" }},
		{"content count", func(r *claude.ClaudeRequest) { r.Messages[0].Content = []any{} }},
		{"tool identity", func(r *claude.ClaudeRequest) {
			r.Messages[0].Content.([]any)[0].(map[string]any)["tool_use_id"] = "call_b"
		}},
		{"tool identity whitespace", func(r *claude.ClaudeRequest) {
			r.Messages[0].Content.([]any)[0].(map[string]any)["tool_use_id"] = "call_a "
		}},
		{"nested content count", func(r *claude.ClaudeRequest) { r.Messages[0].Content.([]any)[0].(map[string]any)["content"] = []any{} }},
		{"nested type", func(r *claude.ClaudeRequest) { claudeMediaTestImage(r)["type"] = "text" }},
		{"source missing", func(r *claude.ClaudeRequest) { delete(claudeMediaTestImage(r), "source") }},
		{"prepared data missing", func(r *claude.ClaudeRequest) {
			claudeMediaTestImage(r)["source"] = map[string]any{"type": "base64", "media_type": "image/png"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared := decodeClaudeMediaRequest(t, raw)
			tc.mutate(prepared)
			if _, err := patchClaudePreparedMedia([]byte(raw), prepared); err == nil {
				t.Fatal("misaligned media patch was accepted")
			}
		})
	}
}

func TestPatchChatPreparedMediaRejectsMisalignedPaths(t *testing.T) {
	const raw = `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/image"}}]}]}`
	for _, tc := range []struct {
		name   string
		mutate func(*types.ChatCompletionRequest)
	}{
		{"message count", func(r *types.ChatCompletionRequest) { r.Messages = nil }},
		{"content count", func(r *types.ChatCompletionRequest) { r.Messages[0].Content = []any{} }},
		{"block type", func(r *types.ChatCompletionRequest) {
			r.Messages[0].Content.([]any)[0].(map[string]any)["type"] = "text"
		}},
		{"source missing", func(r *types.ChatCompletionRequest) {
			delete(r.Messages[0].Content.([]any)[0].(map[string]any), "image_url")
		}},
		{"remote URL substitution", func(r *types.ChatCompletionRequest) {
			r.Messages[0].Content.([]any)[0].(map[string]any)["image_url"] = map[string]any{"url": "https://other.invalid/image"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var prepared types.ChatCompletionRequest
			if err := json.Unmarshal([]byte(raw), &prepared); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&prepared)
			if _, err := patchChatPreparedMedia([]byte(raw), &prepared); err == nil {
				t.Fatal("misaligned media patch was accepted")
			}
		})
	}
}

func TestPreparedMediaPatchesSharedMaterializerOutput(t *testing.T) {
	t.Run("Claude", func(t *testing.T) {
		const raw = `{"model":"claude-model","future":9007199254740993,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":[{"type":"image","source":{"type":"url","url":"https://example.invalid/image"},"future":9007199254740993,"signature":"keep"}]}]}]}`
		fetcher := &patchMediaFetcher{}
		prepared, err := claude.MaterializeNativeRemoteMedia(decodeClaudeMediaRequest(t, raw), fetcher)
		if err != nil {
			t.Fatal(err)
		}
		patched, err := patchClaudePreparedMedia([]byte(raw), prepared)
		if err != nil || fetcher.calls != 1 {
			t.Fatalf("patch error=%v, fetch calls=%d", err, fetcher.calls)
		}
		if bytes.Count(patched, []byte(`9007199254740993`)) != 2 || !bytes.Contains(patched, []byte(`"data":"aA=="`)) || !bytes.Contains(patched, []byte(`"signature":"keep"`)) {
			t.Fatalf("shared materialization lost raw fields: %s", patched)
		}
	})
	t.Run("Chat", func(t *testing.T) {
		const raw = `{"model":"chat-model","future":9007199254740993,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/image"},"future":9007199254740993}]}]}`
		var prepared types.ChatCompletionRequest
		if err := json.Unmarshal([]byte(raw), &prepared); err != nil {
			t.Fatal(err)
		}
		fetcher := &patchMediaFetcher{}
		if err := base.MaterializeChatRemoteMedia(&prepared, fetcher); err != nil {
			t.Fatal(err)
		}
		patched, err := patchChatPreparedMedia([]byte(raw), &prepared)
		if err != nil || fetcher.calls != 1 {
			t.Fatalf("patch error=%v, fetch calls=%d", err, fetcher.calls)
		}
		if bytes.Count(patched, []byte(`9007199254740993`)) != 2 || !bytes.Contains(patched, []byte(`"url":"data:image/png;base64,aA=="`)) {
			t.Fatalf("shared materialization lost raw fields: %s", patched)
		}
	})
}

type patchMediaFetcher struct{ calls int }

func (f *patchMediaFetcher) Fetch(string) (string, []byte, error) {
	f.calls++
	return "image/png", []byte("h"), nil
}

func decodeClaudeMediaRequest(t *testing.T, raw string) *claude.ClaudeRequest {
	t.Helper()
	var prepared claude.ClaudeRequest
	if err := json.Unmarshal([]byte(raw), &prepared); err != nil {
		t.Fatal(err)
	}
	return &prepared
}

func claudeMediaTestImage(request *claude.ClaudeRequest) map[string]any {
	return request.Messages[0].Content.([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
}
