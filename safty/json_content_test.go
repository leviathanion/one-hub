package safty

import (
	"encoding/json"
	"testing"
)

func TestJSONContentTextDecodesValuesWithoutSchema(t *testing.T) {
	for _, test := range []struct {
		name, raw, want string
	}{
		{"escaped text", `[{"text":"\u0062locked"},{"future":{"text":"\u4e2d\u6587"}}]`, "blocked\n中文"},
		{"escaped controls", `{"text":"quote: \" slash: \\ newline:\n tab:\t"}`, "quote: \" slash: \\ newline:\n tab:\t"},
		{"surrogate pair", `"\ud83d\ude00"`, "😀"},
		{"structure and numbers", `{"blocked":true,"n":1e+10000,"large":9007199254740993,"null":null,"text":"hello"}`, "hello"},
		{"duplicate values", `{"text":"\u0062locked","text":"hello"}`, "blocked\nhello"},
		{"nested arrays", `[null,[false,{},"hello"],"world"]`, "hello\nworld"},
		{"empty field", ``, ""},
		{"null field", `null`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := json.RawMessage(test.raw)
			got, err := convertToString(raw)
			if err != nil || got != test.want {
				t.Fatalf("解码文本错误：got=%q want=%q err=%v", got, test.want, err)
			}
			if string(raw) != test.raw {
				t.Fatal("内容观察修改了原始字节")
			}
		})
	}
}

func TestJSONContentTextRejectsMalformedJSON(t *testing.T) {
	for _, raw := range []string{`{"text":`, `["hello"`, `{"text" "hello"}`, `{"text":"hello"} {}`, `true false`, `"\uZZZZ"`} {
		if _, err := convertToString(json.RawMessage(raw)); err == nil {
			t.Fatalf("损坏的 JSON 未报告解析错误：%s", raw)
		}
	}
}
