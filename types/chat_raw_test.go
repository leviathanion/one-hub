package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatMessageRoundTripPreservesWireFields(t *testing.T) {
	const input = `{"role":"assistant","content":null,"tool_calls":[{"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{}","future_number":9007199254740993},"future_null":null}],"extra_content":{"state":"opaque","number":9007199254740993,"scale":1e+09},"future":null}`
	var message ChatCompletionMessage
	if err := json.Unmarshal([]byte(input), &message); err != nil {
		t.Fatal(err)
	}
	message.Role = "tool"
	message.ToolCalls[0].Id = "call_b"
	message.ToolCalls[0].Function.Name = "search"
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`"role":"tool"`, `"content":null`, `"id":"call_b"`, `"name":"search"`,
		`"future_number":9007199254740993`, `"future_null":null`, `"number":9007199254740993`, `"scale":1e+09`, `"future":null`,
	} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("lost %s: %s", fragment, encoded)
		}
	}
	if strings.Contains(string(encoded), `"index"`) {
		t.Fatalf("inserted index absent from the original call: %s", encoded)
	}
	message.ToolCalls = nil
	message.Content = "completed"
	encoded, err = json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"tool_calls"`) || !strings.Contains(string(encoded), `"content":"completed"`) {
		t.Fatalf("typed changes did not replace original fields: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"extra_content"`) {
		t.Fatalf("lost extension when removing a known field: %s", encoded)
	}
}

func TestChatStreamDeltaPreservesExtensionsAndExplicitNull(t *testing.T) {
	var delta ChatCompletionStreamChoiceDelta
	if err := json.Unmarshal([]byte(`{"content":null,"tool_calls":null,"extra_content":{"state":"opaque"},"future":9007199254740993}`), &delta); err != nil {
		t.Fatal(err)
	}
	delta.ReasoningContent = "reasoning"
	delta.ExtraFields["another_extension"] = json.RawMessage(`null`)
	delta.ExtraFields["content"] = json.RawMessage(`"cannot override typed field"`)
	encoded, err := json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"content":null`, `"tool_calls":null`, `"reasoning_content":"reasoning"`, `"future":9007199254740993`, `"extra_content":{"state":"opaque"}`, `"another_extension":null`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("lost %s: %s", fragment, encoded)
		}
	}
	delete(delta.ExtraFields, "future")
	delta.Content = "answer"
	encoded, err = json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"future"`) || !strings.Contains(string(encoded), `"content":"answer"`) {
		t.Fatalf("field edits were ignored: %s", encoded)
	}
}

func TestChatFunctionDefinitionPreservesSchemaAndExtensions(t *testing.T) {
	var tool ChatCompletionTool
	if err := json.Unmarshal([]byte(`{"type":"function","future":null,"function":{"name":"lookup","description":null,"parameters":{"maximum":9007199254740993,"scale":1e+09},"strict":null,"future":{"key":null}}}`), &tool); err != nil {
		t.Fatal(err)
	}
	tool.Function.Name = "search"
	tool.Function.Parameters.(map[string]any)["minimum"] = json.Number("9007199254740991")
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"name":"search"`, `"description":null`, `"strict":null`, `"maximum":9007199254740993`, `"scale":1e+09`, `"minimum":9007199254740991`, `"future":null`, `"future":{"key":null}`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("lost %s: %s", fragment, encoded)
		}
	}
}

func TestChatToolCallPreservesCustomAndFunctionExtensions(t *testing.T) {
	for _, input := range []string{
		`{"type":"function","function":{"name":"run","arguments":{"count":9007199254740993},"future":null},"future":1e+09}`,
		`{"type":"custom","custom":{"name":"run","input":"command","future":null},"future":9007199254740993}`,
	} {
		var call ChatCompletionToolCalls
		if err := json.Unmarshal([]byte(input), &call); err != nil {
			t.Fatal(err)
		}
		call.Id = "call_a"
		encoded, err := json.Marshal(call)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"future":null`) || !strings.Contains(string(encoded), `9007199254740993`) {
			t.Fatalf("lost nested extension or numeric representation: %s", encoded)
		}
		if call.Function != nil {
			call.Function.Arguments = `{"count":2}`
			encoded, err = json.Marshal(call)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), `"arguments":"{\"count\":2}"`) {
				t.Fatalf("changed arguments did not override raw value: %s", encoded)
			}
		}
	}
}

func TestChatContentPartsPreserveMediaExtensions(t *testing.T) {
	var message ChatCompletionMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/image","detail":null,"future":9007199254740993},"future":null}]}`), &message); err != nil {
		t.Fatal(err)
	}
	parts := message.ParseContent()
	if len(parts) != 1 || parts[0].ImageURL == nil {
		t.Fatalf("unexpected parts: %#v", parts)
	}
	parts[0].ImageURL.URL = "data:image/png;base64,aA=="
	message.Content = parts
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`data:image/png;base64,aA==`, `"future":9007199254740993`, `"detail":null`, `"future":null`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("lost %s after media preparation: %s", fragment, encoded)
		}
	}
}

func TestChatGeneratedExtensionsAreSerialized(t *testing.T) {
	message := ChatCompletionMessage{Role: "assistant", ExtraFields: map[string]json.RawMessage{"extra_content": json.RawMessage(`{"state":"opaque"}`)}}
	delta := ChatCompletionStreamChoiceDelta{ExtraFields: message.ExtraFields}
	for _, value := range []any{message, delta} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"extra_content":{"state":"opaque"}`) || strings.Contains(string(encoded), `"tool_calls"`) {
			t.Fatalf("unexpected generated extension: %s", encoded)
		}
	}
}

func TestChatWireDecodeReplacesPreviousExtensions(t *testing.T) {
	var message ChatCompletionMessage
	if err := json.Unmarshal([]byte(`{"role":"assistant","future":true}`), &message); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"role":"user","content":"next"}`), &message); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "future") {
		t.Fatalf("leaked previous object's fields: %s", encoded)
	}
}
