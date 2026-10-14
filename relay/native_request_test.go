package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"one-api/common"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
)

func TestNativePreAddPreservesUntouchedRawSubtrees(t *testing.T) {
	raw := []byte(`{ "contents": { "future_union": true }, "large":9007199254740993, "future":1e+3, "temperature":0.5 }`)
	params := `{"pre_add":true,"overwrite":true,"temperature":0.2}`
	body, fields, err := effectiveNativeRequestBody(&model.Channel{CustomParameter: &params}, "mapped-model", raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(fields["temperature"]) != "0.2" || string(fields["large"]) != "9007199254740993" || string(fields["future"]) != "1e+3" || !bytes.Contains(body, []byte(`{ "future_union": true }`)) {
		t.Fatalf("pre_add changed unrelated raw semantics: %s", body)
	}
}

func TestChatPreMappingPreservesLargeSchemaNumbersAndUnknownFields(t *testing.T) {
	raw := `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"const":900719925474099312345,"future":null}}}]}`
	params := `{"pre_add":true,"temperature":0.2}`
	provider := &openai.OpenAIProvider{BaseProvider: base.BaseProvider{Channel: &model.Channel{CustomParameter: &params}}}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
	changed, err := applyPreMappingForProvider(ctx, "m", provider)
	if err != nil || !changed {
		t.Fatalf("pre_add未应用：changed=%t err=%v", changed, err)
	}
	body, ok := common.GetCanonicalRequestBody(ctx)
	var fields map[string]json.RawMessage
	if !ok || json.Unmarshal(body, &fields) != nil || !bytes.Contains(fields["tools"], []byte(`"const":900719925474099312345`)) || !bytes.Contains(fields["tools"], []byte(`"future":null`)) || string(fields["temperature"]) != "0.2" {
		t.Fatalf("发送前准备与raw评估语义不一致：%s", body)
	}
}

func TestNativePreAddNoopKeepsOriginalBytes(t *testing.T) {
	raw := []byte(`{ "contents": { "future_union": true }, "future":1e+3 }`)
	params := `{"pre_add":true}`
	body, _, err := effectiveNativeRequestBody(&model.Channel{CustomParameter: &params}, "model", raw, false)
	if err != nil || !bytes.Equal(body, raw) {
		t.Fatalf("no-op changed raw: body=%s err=%v", body, err)
	}
}
