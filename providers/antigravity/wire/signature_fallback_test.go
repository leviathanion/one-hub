package wire

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestCrossProtocolGeminiBypassOnlyChangesUnsignedFunctionParts(t *testing.T) {
	input := []byte(`{"messages":[{"role":"assistant","content":[
		{"type":"text","text":"checking"},
		{"type":"thinking","thinking":"unsigned thought"},
		{"type":"thinking","thinking":"signed thought","signature":"native-thought-signature"},
		{"type":"tool_use","id":"a","name":"lookup","input":{}},
		{"type":"tool_use","id":"b","name":"lookup","input":{},"signature":"native-tool-signature"}
	]}]}`)
	req, err := ConvertRequest(DialectClaude, input, "gemini-3-pro")
	if err != nil {
		t.Fatal(err)
	}
	// 固定目标样本独立于生产响应编码器，完整比较 part 内容、顺序和签名归属。
	wantRaw := []byte(`[
		{"text":"checking"},
		{"text":"unsigned thought","thought":true},
		{"text":"signed thought","thought":true,"thoughtSignature":"native-thought-signature"},
		{"functionCall":{"id":"a","name":"lookup","args":{}},"thoughtSignature":"skip_thought_signature_validator"},
		{"functionCall":{"id":"b","name":"lookup","args":{}},"thoughtSignature":"native-tool-signature"}
	]`)
	gotRaw, err := json.Marshal(requestParts(t, req.Body, 0))
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(gotRaw, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed parts changed: got=%s want=%s", gotRaw, wantRaw)
	}
}

func TestCrossProtocolGeminiUnsignedToolsUseSignatureBypass(t *testing.T) {
	for _, tt := range []struct {
		dialect Dialect
		input   string
	}{
		{DialectChat, `{"messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"lookup","arguments":"{\"n\":9007199254740993123}"}},{"id":"b","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":"ok"},{"role":"tool","tool_call_id":"b","content":"ok"}]}`},
		{DialectClaude, `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"lookup","input":{"n":9007199254740993123}},{"type":"tool_use","id":"b","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"ok"},{"type":"tool_result","tool_use_id":"b","content":"ok"}]}]}`},
		{DialectResponses, `{"store":false,"input":[{"type":"function_call","call_id":"a","name":"lookup","arguments":"{\"n\":9007199254740993123}"},{"type":"function_call","call_id":"b","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"ok"},{"type":"function_call_output","call_id":"b","output":"ok"}]}`},
	} {
		t.Run(string(tt.dialect), func(t *testing.T) {
			for _, model := range []string{"gemini-3-pro", "GeMiNi-3-flash"} {
				req, err := ConvertRequest(tt.dialect, []byte(tt.input), model)
				if err != nil {
					t.Fatal(err)
				}
				parts := requestParts(t, req.Body, 0)
				if len(parts) != 2 {
					t.Fatalf("parallel calls changed: %s", req.Body)
				}
				for _, part := range parts {
					if !bytes.Contains(part, []byte(`"thoughtSignature":"skip_thought_signature_validator"`)) {
						t.Fatalf("unsigned call lacks bypass: %s", part)
					}
				}
				if !bytes.Contains(parts[0], []byte(`9007199254740993123`)) {
					t.Fatalf("tool argument precision lost: %s", parts[0])
				}
				for _, part := range requestParts(t, req.Body, 1) {
					if bytes.Contains(part, []byte(`thoughtSignature`)) {
						t.Fatalf("tool result acquired signature: %s", part)
					}
				}
			}
		})
	}
}

func TestCrossProtocolGeminiPreservesRestoredToolSignatures(t *testing.T) {
	for _, dialect := range []Dialect{DialectChat, DialectClaude, DialectResponses} {
		for _, streaming := range []bool{false, true} {
			t.Run(string(dialect)+map[bool]string{false: "/aggregate", true: "/stream"}[streaming], func(t *testing.T) {
				history := responseHistoryForRoundTrip(t, dialect, []string{`[{"functionCall":{"id":"a","name":"lookup","args":{}},"thoughtSignature":"native-opaque-signature"}]`}, streaming)
				req, err := ConvertRequest(dialect, history, "gemini-3-pro")
				if err != nil {
					t.Fatal(err)
				}
				parts := requestParts(t, req.Body, 0)
				if len(parts) != 1 || !bytes.Contains(parts[0], []byte(`"thoughtSignature":"native-opaque-signature"`)) || bytes.Contains(req.Body, []byte(`skip_thought_signature_validator`)) {
					t.Fatalf("real signature was replaced: %s", req.Body)
				}
			})
		}
	}
}

func TestCrossProtocolGeminiEmptyToolSignatureUsesBypass(t *testing.T) {
	for _, signature := range []string{"", " \t\n"} {
		input := marshal(map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "a", "name": "lookup", "input": map[string]any{}, "signature": signature}}}}})
		req, err := ConvertRequest(DialectClaude, input, "gemini-3-pro")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(requestParts(t, req.Body, 0)[0], []byte(`"thoughtSignature":"skip_thought_signature_validator"`)) {
			t.Fatalf("empty signature lacks bypass: %s", req.Body)
		}
	}
}

func TestToolSignatureBypassDoesNotChangeNativeOrOtherModels(t *testing.T) {
	raw := []byte(` { "contents":[{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"n":9007199254740993123}},"future":{"union":null}}]}], "unknown":true } `)
	req, err := ConvertRequest(DialectGemini, raw, "gemini-3-pro")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(req.Body, raw) {
		t.Fatalf("native wire changed: %s", req.Body)
	}
	for _, model := range []string{"claude-sonnet-4-5", "future-model"} {
		req, err := ConvertRequest(DialectChat, []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`), model)
		if err != nil {
			t.Fatal(err)
		}
		var part map[string]json.RawMessage
		if err := json.Unmarshal(requestParts(t, req.Body, 0)[0], &part); err != nil {
			t.Fatal(err)
		}
		if _, exists := part["thoughtSignature"]; exists {
			t.Fatalf("non-Gemini target acquired signature: %s", req.Body)
		}
	}
}
