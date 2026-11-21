package base

import (
	"encoding/json"
	"testing"
)

func TestChatStreamOptionsProjectionUsesAdapterBooleanFields(t *testing.T) {
	for _, test := range []struct {
		options           string
		conversion, typed bool
	}{
		{`null`, true, true},
		{`{}`, true, true},
		{`{"include_usage":true}`, true, true},
		{`{"include_usage":false}`, true, true},
		{`{"include_usage":null}`, true, true},
		{`{"include_obfuscation":true}`, false, true},
		{`{"include_obfuscation":false}`, false, true},
		{`{"include_obfuscation":null}`, true, true},
		{`"future"`, false, false},
		{`[]`, false, false},
		{`{"include_usage":{"future":true}}`, false, false},
		{`{"include_usage":"true"}`, false, false},
		{`{"include_obfuscation":"future"}`, false, false},
		{`{"future":true}`, false, false},
		{`{"future":null}`, false, false},
	} {
		t.Run(test.options, func(t *testing.T) {
			raw := json.RawMessage(test.options)
			if err := ValidateChatStreamOptionsProjection(raw); (err == nil) != test.conversion {
				t.Fatalf("conversion allowed=%v error=%v", test.conversion, err)
			}
			if err := ValidateChatStreamOptionsProjection(raw, "include_obfuscation"); (err == nil) != test.typed {
				t.Fatalf("typed OpenAI allowed=%v error=%v", test.typed, err)
			}
		})
	}
}
