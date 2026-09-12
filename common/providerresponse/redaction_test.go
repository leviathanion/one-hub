package providerresponse

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestErrorRedactionPreservesNormalBody(t *testing.T) {
	for _, raw := range []string{
		`{ "choices":[{"message":{"content":"provider-secret","error":{"token":"model-owned"}}}],"account_id":"provider-secret","future":1e999 }`,
		`{"type":"response.output_text.delta","delta":"provider-secret"}`,
		`{"type":"response.completed","response":{"id":"provider-secret","output":[{"token":"provider-secret"}],"metadata":{"account_id":"provider-secret"}}}`,
		`{"code":"bad_input","message":"provider-secret"}`,
		`{"error":null,"text":"provider-secret"}`,
		`{"error":"invalid_api_key","error_description":"provider-secret"}`,
	} {
		safe, changed := SanitizeErrorPayload([]byte(raw), "provider-secret")
		if changed || string(safe) != raw {
			t.Fatalf("正文改变: %s", safe)
		}
	}
}

func TestErrorRedactionOnlyChangesDiagnosticValues(t *testing.T) {
	for _, raw := range []string{
		`{ "error":{"message":"provider\u002dsecret","code":"provider-secret","param":"provider-secret","id_token":{"nested":"private"}}, "output":[{"text":"provider-secret"}], "metadata":{"account_id":"provider-secret"}, "future":9007199254740993 }`,
		`{ "type":"response.failed","response":{"error":{"message":"provider\u002dsecret","code":"provider-secret","param":"provider-secret","id_token":{"nested":"private"}},"output":[{"text":"provider-secret"}],"metadata":{"account_id":"provider-secret"}}, "future":9007199254740993 }`,
	} {
		want := strings.ReplaceAll(strings.ReplaceAll(raw, `"message":"provider\u002dsecret"`, `"message":"[redacted]"`), `"id_token":{"nested":"private"}`, `"id_token":"[redacted]"`)
		input := []byte(raw)
		safe, changed := SanitizeErrorPayload(input, "provider-secret")
		if !changed || string(safe) != want || string(input) != raw {
			t.Fatalf("诊断范围/原始字节错误: %s", safe)
		}
	}
}

func TestErrorRedactionBestEffortFallback(t *testing.T) {
	for _, raw := range []string{
		`{"error":{"message":"provider-secret","message":"again"}}`,
		`{"error":{"message":"provider-secret"}`, `{"error":{"message":"provider-secret"}} {}`,
		"{\"error\":{\"message\":\"\xff\"}}",
		`{"error":{"message":"provider-secret","details":` + strings.Repeat("[", 70) + `0` + strings.Repeat("]", 70) + `}}`,
		`{"error":{"message":"provider-secret","details":"` + strings.Repeat("x", 1<<20) + `"}}`,
	} {
		safe, changed := SanitizeErrorPayload([]byte(raw), "provider-secret")
		if changed || string(safe) != raw {
			t.Fatalf("无法处理时没有返回原文: %.160s", safe)
		}
	}
}

func TestErrorRedactionKnownFlatErrorAndProtocolExtensions(t *testing.T) {
	raw := []byte(`{ "code":{"detail":"provider-secret"}, "type":"invalid_request_error","message":"provider-secret", "details":{"access_token":"hidden"},"param":"session","future":1e999 }`)
	safe, changed := SanitizeErrorResponse(raw, "provider-secret")
	want := `{ "code":{"detail":"provider-secret"}, "type":"invalid_request_error","message":"[redacted]", "details":{"access_token":"[redacted]"},"param":"session","future":1e999 }`
	if !changed || string(safe) != want {
		t.Fatalf("已知错误改写错误: %s", safe)
	}
}

func TestErrorRedactionDoesNotRegenerateSecret(t *testing.T) {
	for _, test := range []struct{ secret, message string }{{"red", "rreded"}, {"x[redacted]y", "xx[redacted]yy"}} {
		safe := SanitizeErrorText(test.message, test.secret)
		if strings.Contains(safe, test.secret) {
			t.Fatalf("替换重组秘密: %q", safe)
		}
	}
}

func FuzzSanitizeErrorPayload(f *testing.F) {
	for _, raw := range []string{`{"error":{"message":"provider-secret"}}`, `{"type":"error","account_id":{"nested":"secret"},"future":9007199254740993}`, `{"error":{"message":"one","message":"two"}}`, `{"response":{"error":{"message":"provider-secret"},"output":[{"text":"provider-secret"}]}}`, `{"error":{"message":"provider-secret"}} trailing`} {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			return
		}
		original := bytes.Clone(raw)
		safe, changed := SanitizeErrorPayload(raw, "provider-secret")
		if !bytes.Equal(raw, original) {
			t.Fatal("输入被修改")
		}
		if !changed {
			if !bytes.Equal(safe, raw) {
				t.Fatal("回退改变了原文")
			}
			return
		}
		if !json.Valid(safe) {
			t.Fatalf("产生非法 JSON: %q", safe)
		}
		again, changedAgain := SanitizeErrorPayload(safe, "provider-secret")
		if changedAgain || !bytes.Equal(again, safe) {
			t.Fatalf("重复执行不稳定: %s -> %s", safe, again)
		}
	})
}
