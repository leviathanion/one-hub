package jsonobject

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProjectionRetainsOpenContentWhenOtherFieldsChange(t *testing.T) {
	type wire struct {
		Role    string `json:"role"`
		Content any    `json:"content,omitempty"`
		Text    string `json:"text,omitempty"`
	}
	for _, content := range []string{`null`, `false`, `9007199254740993`, `1e+09`, `{"future":[null,9007199254740993]}`} {
		t.Run(content, func(t *testing.T) {
			var value wire
			state, extra, err := DecodeProjection([]byte(`{"role":"assistant","content":`+content+`,"text":null,"extension":null}`), &value)
			if err != nil {
				t.Fatal(err)
			}
			value.Role = "user"
			encoded, err := MarshalProjection(value, extra, state)
			if err != nil {
				t.Fatal(err)
			}
			for _, fragment := range []string{`"role":"user"`, `"content":` + content, `"text":null`, `"extension":null`} {
				if !strings.Contains(string(encoded), fragment) {
					t.Fatalf("lost %s: %s", fragment, encoded)
				}
			}
			value.Content = "updated"
			encoded, err = MarshalProjection(value, extra, state)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), `"content":"updated"`) {
				t.Fatalf("typed content did not override original: %s", encoded)
			}
		})
	}
}

func TestProjectionRejectsInvalidExtensionJSON(t *testing.T) {
	type wire struct {
		Role string `json:"role"`
	}
	_, err := MarshalProjection(wire{Role: "assistant"}, map[string]json.RawMessage{"extension": json.RawMessage(`{`)}, nil)
	if err == nil {
		t.Fatal("invalid extension JSON was accepted")
	}
}
