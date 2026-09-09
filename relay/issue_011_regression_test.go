package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

type issue011NativeClaudeCapture struct {
	body  []byte
	calls atomic.Int32
}

func newI011NativeClaudeRelay(t *testing.T, stream bool, custom, mapping string) (*relayClaudeOnly, *issue011NativeClaudeCapture, []byte) {
	t.Helper()
	capture := &issue011NativeClaudeCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.calls.Add(1)
		capture.body, _ = io.ReadAll(r.Body)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"model-b\",\"usage\":{\"input_tokens\":1}}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\",\"usage\":{\"output_tokens\":1}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_i011","type":"message","model":"claude-3-5-sonnet","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	raw := `{"model":"model-a","max_tokens":64,"messages":[],"future_integer":9007199254740993,"future":{"original":true}}`
	if stream {
		raw = `{"model":"model-a","max_tokens":64,"messages":[],"stream":true,"future_integer":9007199254740993,"future":{"original":true}}`
	}
	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(channelGroupSnapshot) })
	weight := uint(1)
	proxy := ""
	baseURL := server.URL
	channel := &model.Channel{
		Id:              11011,
		Type:            config.ChannelTypeAnthropic,
		Status:          config.ChannelStatusEnabled,
		Group:           "default",
		Models:          "model-a",
		Weight:          &weight,
		Proxy:           &proxy,
		BaseURL:         &baseURL,
		Key:             "provider-key",
		CustomParameter: &custom,
		ModelMapping:    &mapping,
	}
	model.ChannelGroup = model.ChannelsChooser{
		Channels:   map[int]*model.ChannelChoice{channel.Id: {Channel: channel}},
		Rule:       map[string]map[string][][]int{"default": {"model-a": {{channel.Id}}}},
		ModelGroup: map[string]map[string]bool{"model-a": {"default": true}},
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/claude/v1/messages", strings.NewReader(raw))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("token_group", "default")
	relay := NewRelayClaudeOnly(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("解析原生 Claude 请求失败: %v", err)
	}
	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		t.Fatalf("选择原生 Claude provider 失败: %v", err)
	}
	relay.provider.SetUsage(&types.Usage{})
	return relay, capture, []byte(raw)
}

func decodeI011RelayBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("Claude 上游 body 不是 JSON object: %v; body=%q", err, body)
	}
	return decoded
}

func assertI011MappedPreAddBody(t *testing.T, body []byte, wantModel string) {
	t.Helper()
	decoded := decodeI011RelayBody(t, body)
	if decoded["model"] != wantModel || decoded["max_tokens"] != json.Number("222") {
		t.Fatalf("pre_add 未在映射模型 model-b 上按预期应用: want model=%s body=%#v", wantModel, decoded)
	}
	future, ok := decoded["future"].(map[string]any)
	if !ok || future["source"] != "B" {
		t.Fatalf("pre_add 的 per_model 未选择映射模型或未知字段丢失: %#v", decoded)
	}
}

func TestI011NativeClaudeRelayPreAddNoopKeepsCanonicalRawUnaryAndStream(t *testing.T) {
	previousDisableTokenEncoders := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() { config.DisableTokenEncoders = previousDisableTokenEncoders })

	cases := []struct {
		name   string
		custom string
	}{
		{name: "controls-only", custom: `{"pre_add":true,"overwrite":true}`},
		{name: "per-model-miss", custom: `{"pre_add":true,"overwrite":true,"per_model":true,"other-model":{"max_tokens":256}}`},
		{name: "remove-missing", custom: `{"pre_add":true,"remove_params":["missing.path"]}`},
		{name: "non-overwrite-existing", custom: `{"pre_add":true,"max_tokens":222}`},
		{name: "overwrite-same-number", custom: `{"pre_add":true,"overwrite":true,"max_tokens":64}`},
	}

	for _, stream := range []bool{false, true} {
		modeName := map[bool]string{false: "unary", true: "stream"}[stream]
		for _, test := range cases {
			t.Run(test.name+"/"+modeName, func(t *testing.T) {
				relay, capture, original := newI011NativeClaudeRelay(t, stream, test.custom, `{"model-a":"model-b"}`)
				if err := finalizeSelectedProviderRequest(relay); err != nil {
					t.Fatalf("原生 Claude no-op pre_add finalize 失败: %v", err)
				}
				canonical, ok := common.GetCanonicalRequestBody(relay.c)
				if !ok || !bytes.Equal(canonical, original) {
					t.Fatalf("无有效 pre_add 仍重建 canonical raw:\nwant %s\n got %s", original, canonical)
				}

				if apiErr, _ := relay.send(); apiErr != nil {
					t.Fatalf("原生 Claude no-op pre_add %s 发送失败: %+v", modeName, apiErr)
				}
				decoded := decodeI011RelayBody(t, capture.body)
				if decoded["model"] != "model-b" || decoded["max_tokens"] != json.Number("64") {
					t.Fatalf("模型映射或 no-op pre_add 改变了上游字段: %#v", decoded)
				}
				future, ok := decoded["future"].(map[string]any)
				if !ok || future["original"] != true {
					t.Fatalf("未知字段未保留: %#v", decoded)
				}
			})
		}
	}
}

func TestI011NativeClaudeRelayPreAddUsesMappedModelOnceUnaryAndStream(t *testing.T) {
	previousDisableTokenEncoders := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() { config.DisableTokenEncoders = previousDisableTokenEncoders })

	custom := `{"pre_add":true,"overwrite":true,"per_model":true,"model-a":{"max_tokens":111,"future":{"source":"A"}},"model-b":{"max_tokens":222,"future":{"source":"B"}}}`
	mapping := `{"model-a":"model-b"}`
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "unary", true: "stream"}[stream], func(t *testing.T) {
			relay, capture, original := newI011NativeClaudeRelay(t, stream, custom, mapping)
			if relay.modelName != "model-b" {
				t.Fatalf("测试未建立 model-a→model-b 映射: %q", relay.modelName)
			}
			if err := finalizeSelectedProviderRequest(relay); err != nil {
				t.Fatalf("原生 Claude relay 前置阶段失败: %v", err)
			}
			canonical, ok := common.GetCanonicalRequestBody(relay.c)
			if !ok {
				t.Fatal("前置阶段没有发布 canonical body")
			}
			assertI011MappedPreAddBody(t, canonical, "model-a")
			unchanged, ok := common.GetOriginalRequestBody(relay.c)
			if !ok || !bytes.Equal(unchanged, original) {
				t.Fatalf("pre_add 改写了原始请求基线: want=%q got=%q", original, unchanged)
			}

			if apiErr, _ := relay.send(); apiErr != nil {
				t.Fatalf("原生 Claude relay %s 发送失败: %+v", map[bool]string{false: "unary", true: "stream"}[stream], apiErr)
			}
			if capture.calls.Load() != 1 {
				t.Fatalf("上游收到 %d 次请求，期望1次", capture.calls.Load())
			}
			assertI011MappedPreAddBody(t, capture.body, "model-b")
		})
	}
}

func TestI011NativeClaudeRelayRetryRebuildsFromOriginalWhenPreAddDisappears(t *testing.T) {
	custom := `{"pre_add":true,"overwrite":true,"per_model":true,"model-a":{"max_tokens":111},"model-b":{"max_tokens":222}}`
	mapping := `{"model-a":"model-b"}`
	relay, _, original := newI011NativeClaudeRelay(t, false, custom, mapping)
	if err := finalizeSelectedProviderRequest(relay); err != nil {
		t.Fatalf("首次 native Claude finalize 失败: %v", err)
	}
	first, ok := common.GetCanonicalRequestBody(relay.c)
	if !ok || !strings.Contains(string(first), `"max_tokens":222`) {
		t.Fatalf("首次 finalize 未物化 pre_add: %s", first)
	}

	relay.provider.GetChannel().CustomParameter = nil
	if err := finalizeSelectedProviderRequest(relay); err != nil {
		t.Fatalf("重试 native Claude finalize 失败: %v", err)
	}
	rebuilt, ok := common.GetCanonicalRequestBody(relay.c)
	if !ok || !bytes.Equal(rebuilt, original) {
		t.Fatalf("无 pre_add 重试沿用了上一渠道 body: want=%q got=%q", original, rebuilt)
	}
}
