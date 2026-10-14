package claude

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeContentPreservesSignatureAndUnknownFields(t *testing.T) {
	const input = `{"type":"tool_use","id":"call_a","name":"lookup","input":{"count":9007199254740993,"scale":1e+09},"signature":"native-signature","future":null}`
	t.Run("request", func(t *testing.T) {
		var block MessageContent
		if err := json.Unmarshal([]byte(input), &block); err != nil {
			t.Fatal(err)
		}
		if string(block.ExtraFields["signature"]) != `"native-signature"` {
			t.Fatalf("signature not available as a raw extension: %#v", block.ExtraFields)
		}
		block.Name = "search"
		block.Input.(map[string]any)["other"] = true
		assertClaudeRawFields(t, block, `"name":"search"`, `"other":true`, `"signature":"native-signature"`, `"future":null`, `9007199254740993`, `1e+09`)
	})
	t.Run("response", func(t *testing.T) {
		var block ResContent
		if err := json.Unmarshal([]byte(input), &block); err != nil {
			t.Fatal(err)
		}
		block.Signature = "updated-signature"
		assertClaudeRawFields(t, block, `"signature":"updated-signature"`, `"future":null`, `9007199254740993`, `1e+09`)
	})
	t.Run("stream", func(t *testing.T) {
		var block ContentBlock
		if err := json.Unmarshal([]byte(input), &block); err != nil {
			t.Fatal(err)
		}
		block.Id = "call_b"
		assertClaudeRawFields(t, block, `"id":"call_b"`, `"signature":"native-signature"`, `"future":null`, `9007199254740993`, `1e+09`)
	})
}

func TestClaudeMediaPreparationPreservesBlockAndSourceExtensions(t *testing.T) {
	var block MessageContent
	if err := json.Unmarshal([]byte(`{"type":"image","source":{"type":"url","url":"https://example.invalid/image","media_type":null,"future":9007199254740993},"cache_control":null,"future":null}`), &block); err != nil {
		t.Fatal(err)
	}
	block.Source.Type = "base64"
	block.Source.Url = ""
	block.Source.Data = "aA=="
	encoded := assertClaudeRawFields(t, block, `"type":"base64"`, `"data":"aA=="`, `"media_type":null`, `"cache_control":null`, `"future":null`, `9007199254740993`)
	if strings.Contains(encoded, `"url"`) {
		t.Fatalf("old URL survived the media patch: %s", encoded)
	}
}

func TestClaudeStreamDeltaPreservesNullAndSignatureExtensions(t *testing.T) {
	var delta Delta
	if err := json.Unmarshal([]byte(`{"type":"signature_delta","signature":"native-signature","text":null,"future":{"number":9007199254740993}}`), &delta); err != nil {
		t.Fatal(err)
	}
	delta.Signature = "next-signature"
	delta.ExtraFields["next"] = json.RawMessage(`null`)
	assertClaudeRawFields(t, delta, `"signature":"next-signature"`, `"text":null`, `"future":{"number":9007199254740993}`, `"next":null`)
}

func assertClaudeRawFields(t *testing.T, value any, fragments ...string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range fragments {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("lost %s: %s", fragment, encoded)
		}
	}
	return string(encoded)
}
