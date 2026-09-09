package claude

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/requester"
)

func decodeI011RequestBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("上游收到的 Claude body 不是 JSON object: %v; body=%q", err, body)
	}
	return decoded
}

func drainI011ClaudeStream(t *testing.T, stream requester.StreamReaderInterface[string]) {
	t.Helper()
	data, errs := stream.Recv()
	defer stream.Close()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	var terminalErr error
	for data != nil || errs != nil {
		select {
		case _, ok := <-data:
			if !ok {
				data = nil
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				if terminalErr != nil {
					t.Fatalf("Claude stream 收到多个终态错误: %v 和 %v", terminalErr, err)
				}
				terminalErr = err
			}
		case <-timeout.C:
			t.Fatal("读取 Claude stream 超时")
		}
	}
	if !errors.Is(terminalErr, io.EOF) {
		t.Fatalf("Claude stream 终态=%v，期望 io.EOF", terminalErr)
	}
}

func TestI011NativeClaudeOverwriteReachesUnaryAndStreamUpstream(t *testing.T) {
	for _, streamMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "unary", true: "stream"}[streamMode], func(t *testing.T) {
			var received atomic.Int32
			var body []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				body, _ = io.ReadAll(r.Body)
				if streamMode {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":1}}}\n\n")
					_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\",\"usage\":{\"output_tokens\":1}}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_i011","type":"message","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			raw := `{"model":"claude-test","max_tokens":64,"messages":[],"future":{"nested":{"kept":true}}}`
			if streamMode {
				raw = `{"model":"claude-test","max_tokens":64,"messages":[],"stream":true,"future":{"nested":{"kept":true}}}`
			}
			provider, request := newNativeClaudeProviderForTest(t, server, raw, nil)
			custom := `{"overwrite":true,"max_tokens":256}`
			provider.Channel.CustomParameter = &custom

			if streamMode {
				stream, apiErr := provider.CreateClaudeChatStream(request)
				if apiErr != nil {
					t.Fatalf("创建 Claude stream 失败: %+v", apiErr)
				}
				drainI011ClaudeStream(t, stream)
			} else if _, apiErr := provider.CreateClaudeChat(request); apiErr != nil {
				t.Fatalf("创建 Claude unary 请求失败: %+v", apiErr)
			}

			if received.Load() != 1 {
				t.Fatalf("上游收到 %d 次请求，期望1次", received.Load())
			}
			decoded := decodeI011RequestBody(t, body)
			if got := decoded["max_tokens"]; got != json.Number("256") {
				t.Fatalf("渠道 overwrite 未到达 %s 上游: max_tokens=%v (%T), body=%s", map[bool]string{false: "unary", true: "stream"}[streamMode], got, got, body)
			}
			future, ok := decoded["future"].(map[string]any)
			if !ok || future["nested"].(map[string]any)["kept"] != true {
				t.Fatalf("未知嵌套字段未保留: %#v", decoded)
			}
		})
	}
}

func TestI011NativeClaudeRemoveAndMappedPerModelPatchKeepUnknownFields(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_i011","type":"message","model":"claude-mapped","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	raw := `{"model":"public-claude","max_tokens":64,"messages":[],"metadata":{"drop":true,"keep":{"future":true}},"future_integer":9007199254740993}`
	provider, request := newNativeClaudeProviderForTest(t, server, raw, nil)
	mapping := `{"public-claude":"claude-mapped"}`
	provider.Channel.ModelMapping = &mapping
	mappedModel, err := provider.ModelMappingHandler(request.Model)
	if err != nil {
		t.Fatalf("模型映射失败: %v", err)
	}
	request.Model = mappedModel
	custom := `{"overwrite":true,"per_model":true,"public-claude":{"max_tokens":111},"claude-mapped":{"max_tokens":256,"temperature":0.3,"remove_params":["metadata.drop"]}}`
	provider.Channel.CustomParameter = &custom
	if _, apiErr := provider.CreateClaudeChat(request); apiErr != nil {
		t.Fatalf("mapped Claude unary 请求失败: %+v", apiErr)
	}
	decoded := decodeI011RequestBody(t, body)
	if decoded["model"] != "claude-mapped" || decoded["max_tokens"] != json.Number("256") || decoded["temperature"] != json.Number("0.3") {
		t.Fatalf("模型映射后的 per_model patch 未按顺序应用: %#v", decoded)
	}
	metadata := decoded["metadata"].(map[string]any)
	if _, exists := metadata["drop"]; exists || metadata["keep"].(map[string]any)["future"] != true {
		t.Fatalf("remove_params 或未知嵌套字段错误: %#v", decoded)
	}
	if got := decoded["future_integer"]; got != json.Number("9007199254740993") {
		t.Fatalf("未知大整数被改写: %v (%T)", got, got)
	}
}

func TestI011NativeClaudeInvalidCustomParameterFailsBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"unexpected","type":"message","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	provider, request := newNativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":64,"messages":[]}`, nil)
	invalid := `{"overwrite":true,"max_tokens":`
	provider.Channel.CustomParameter = &invalid
	if _, apiErr := provider.CreateClaudeChat(request); apiErr == nil {
		t.Fatal("无效 custom_parameter 未在上游请求前失败")
	} else if apiErr.Code != "custom_parameter_error" {
		t.Fatalf("无效 custom_parameter 错误码=%v，期望 custom_parameter_error", apiErr.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("无效 custom_parameter 仍触发了 %d 次上游请求", calls.Load())
	}
}

func TestI011NativeClaudeNoPatchKeepsRawWire(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_i011","type":"message","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	raw := `{ "model":"claude-test", "max_tokens":64, "messages":[], "future": { "kept": true } }`
	provider, request := newNativeClaudeProviderForTest(t, server, raw, nil)
	if _, apiErr := provider.CreateClaudeChat(request); apiErr != nil {
		t.Fatalf("无补丁 Claude unary 请求失败: %+v", apiErr)
	}
	if string(body) != raw {
		t.Fatalf("无补丁 raw wire 被重建:\nwant %s\n got %s", raw, body)
	}
}

func TestI011NativeClaudeAllowExtraBodyWithoutPatchKeepsRawWire(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_i011","type":"message","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	raw := `{  "model":"claude-test", "max_tokens":64, "messages":[], "future": { "kept": true }  }`
	provider, request := newNativeClaudeProviderForTest(t, server, raw, nil)
	provider.Channel.AllowExtraBody = true
	if _, apiErr := provider.CreateClaudeChat(request); apiErr != nil {
		t.Fatalf("AllowExtraBody 无补丁请求失败: %+v", apiErr)
	}
	if string(body) != raw {
		t.Fatalf("AllowExtraBody 无补丁 raw wire 被无意义重建:\nwant %s\n got %s", raw, body)
	}
}

func TestI011NativeClaudeEmptyCustomParameterWithoutPatchKeepsRawWire(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_i011","type":"message","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	raw := `{  "model":"claude-test", "max_tokens":64, "messages":[], "future": { "kept": true }  }`
	provider, request := newNativeClaudeProviderForTest(t, server, raw, nil)
	empty := `{}`
	provider.Channel.CustomParameter = &empty
	if _, apiErr := provider.CreateClaudeChat(request); apiErr != nil {
		t.Fatalf("空 custom_parameter 请求失败: %+v", apiErr)
	}
	if string(body) != raw {
		t.Fatalf("空 custom_parameter raw wire 被无意义重建:\nwant %s\n got %s", raw, body)
	}
}

func TestI011NativeClaudeEffectiveNoopPatchesKeepRawWireUnaryAndStream(t *testing.T) {
	cases := []struct {
		name   string
		custom string
	}{
		{name: "controls-only", custom: `{"overwrite":true}`},
		{name: "per-model-miss", custom: `{"overwrite":true,"per_model":true,"other-model":{"max_tokens":256}}`},
		{name: "remove-missing", custom: `{"remove_params":["missing.path"]}`},
		{name: "non-overwrite-existing", custom: `{"temperature":0.3}`},
		{name: "overwrite-same-number", custom: `{"overwrite":true,"max_tokens":64}`},
	}

	for _, streamMode := range []bool{false, true} {
		modeName := map[bool]string{false: "unary", true: "stream"}[streamMode]
		for _, test := range cases {
			t.Run(test.name+"/"+modeName, func(t *testing.T) {
				var received []byte
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					received, _ = io.ReadAll(r.Body)
					if streamMode {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":1}}}\n\n")
						_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\",\"usage\":{\"output_tokens\":1}}\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"msg_i011","type":"message","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
				}))
				t.Cleanup(server.Close)
				previousClient := requester.HTTPClient
				requester.HTTPClient = server.Client()
				t.Cleanup(func() { requester.HTTPClient = previousClient })

				raw := `{ "model":"claude-test", "max_tokens":64, "messages":[], "temperature":0.2, "future_integer":9007199254740993, "future": { "kept": true } }`
				if streamMode {
					raw = `{ "model":"claude-test", "max_tokens":64, "messages":[], "temperature":0.2, "stream":true, "future_integer":9007199254740993, "future": { "kept": true } }`
				}
				provider, request := newNativeClaudeProviderForTest(t, server, raw, nil)
				provider.Channel.CustomParameter = &test.custom
				if streamMode {
					stream, apiErr := provider.CreateClaudeChatStream(request)
					if apiErr != nil {
						t.Fatalf("创建 Claude stream 失败: %+v", apiErr)
					}
					drainI011ClaudeStream(t, stream)
				} else if _, apiErr := provider.CreateClaudeChat(request); apiErr != nil {
					t.Fatalf("创建 Claude unary 请求失败: %+v", apiErr)
				}
				if string(received) != raw {
					t.Fatalf("有效结果为空的 custom_parameter 重建了 raw wire:\nwant %s\n got %s", raw, received)
				}
			})
		}
	}
}
