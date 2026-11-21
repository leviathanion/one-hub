package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"one-api/common/config"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/types"
)

func TestChatSSEUsageFormattingPreservesExtensionsAndEventMetadata(t *testing.T) {
	wire := ": provider comment\r\nid: usage-1\r\nretry: 1200\r\nevent: usage\r\n" +
		`data: {"choices":[],"future_chunk":true,"usage":{"prompt_tokens":2,"completion_tokens":1,` + "\r\n" +
		`data: "total_tokens":6,"completion_tokens_details":{"reasoning_tokens":3,"text_tokens":1,"future_details":{"n":9007199254740993}},"future_usage":true}}` + "\r\n\r\n"
	for _, test := range []struct {
		name, want                  string
		compatible, formatted, hide bool
	}{
		{name: "same dialect formatting", compatible: true, formatted: true, want: "4"},
		{name: "named event when usage is hidden", compatible: true, formatted: true, hide: true, want: "4"},
		{name: "internal evidence only", compatible: true, formatted: false, want: "1"},
		{name: "exact wire", compatible: false, formatted: true, want: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &OpenAIStreamHandler{
				Usage: &types.Usage{}, formatProviderUsage: test.compatible,
				UsageHandler: func(usage *types.Usage) bool {
					usage.CompletionTokens = 4
					usage.CompletionTokensDetails.TextTokens = 2
					return test.formatted
				},
			}
			if test.hide {
				handler.usageFilter = hideCollectedUsage
			}
			stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{Body: io.NopCloser(strings.NewReader(wire))}, handler.HandleExactChatSSE, requester.StreamReadOptions{})
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			output, err := collectNativeSSE(t, stream)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("读取 usage stream：%v", err)
			}
			if !test.compatible || !test.formatted {
				if output != wire {
					t.Fatalf("未授权的 wire 格式化：%q", output)
				}
			}
			if !strings.HasPrefix(output, ": provider comment\r\nid: usage-1\r\nretry: 1200\r\nevent: usage\r\n") || !strings.HasSuffix(output, "\r\n\r\n") {
				t.Fatalf("SSE 元字段或事件分隔符改变：%q", output)
			}
			payload, _ := commonresponses.SSEDataPayload(output)
			var object struct {
				Usage  map[string]json.RawMessage `json:"usage"`
				Future bool                       `json:"future_chunk"`
			}
			if json.Unmarshal([]byte(payload), &object) != nil || !object.Future || string(object.Usage["completion_tokens"]) != test.want || string(object.Usage["future_usage"]) != "true" {
				t.Fatalf("usage 字段改写错误：%s", payload)
			}
			var detail map[string]json.RawMessage
			_ = json.Unmarshal(object.Usage["completion_tokens_details"], &detail)
			if string(detail["future_details"]) != `{"n":9007199254740993}` || detail["audio_tokens"] != nil || object.Usage["prompt_tokens_details"] != nil {
				t.Fatalf("嵌套扩展丢失或补入伪造零值：%s", payload)
			}
			if handler.Usage.CompletionTokens != 4 {
				t.Fatalf("内部计费证据未规范化：%+v", handler.Usage)
			}
		})
	}
}

func TestCompatibleChatUsageFormattingPreservesNestedExtensions(t *testing.T) {
	raw := []byte(`{"choices":[],"usage":{"completion_tokens":1,"completion_tokens_details":{"reasoning_tokens":3,"text_tokens":1,"future_detail":true},"future_usage":{"n":9007199254740993}}}`)
	var response types.ChatCompletionResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	response.Usage.CompletionTokens = 4
	response.Usage.CompletionTokensDetails.TextTokens = 2
	patched, err := safeCompatibleChatResponseReplay(raw, &response, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, preserved := range []string{`"completion_tokens":4`, `"text_tokens":2`, `"future_detail":true`, `"future_usage":{"n":9007199254740993}`} {
		if !strings.Contains(string(patched), preserved) {
			t.Fatalf("usage 规范化丢失字段 %s：%s", preserved, patched)
		}
	}
	if strings.Contains(string(patched), `"prompt_tokens"`) || strings.Contains(string(patched), `"total_tokens"`) || strings.Contains(string(patched), `"audio_tokens"`) {
		t.Fatalf("usage 规范化补入默认零值：%s", patched)
	}
}

func TestCompatibleChatUsageFormattingKeepsChoiceUsageLocation(t *testing.T) {
	raw := []byte(`{"choices":[{"delta":{"future_delta":true},"usage":{"completion_tokens":1,"future_usage":true}}]}`)
	patched, err := patchChatPayloadUsage(raw, &types.Usage{CompletionTokens: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patched), `"usage":{"completion_tokens":4,"future_usage":true}`) || !strings.Contains(string(patched), `"future_delta":true`) {
		t.Fatalf("choice usage 位置或未知字段改变：%s", patched)
	}
}

func TestChatSSEObservesUsageInErrorFrameWithoutChangingErrorDelivery(t *testing.T) {
	for _, flat := range []bool{false, true} {
		for _, hasUsage := range []bool{false, true} {
			for _, compatible := range []bool{false, true} {
				name := fmt.Sprintf("flat=%v usage=%v compatible=%v", flat, hasUsage, compatible)
				t.Run(name, func(t *testing.T) {
					detail := `{"code":"execution_failed","message":"stopped","type":"upstream_error"}`
					fields := `"error":` + detail
					if flat {
						fields = `"code":"execution_failed","error":"stopped"`
					}
					if hasUsage {
						fields += `,"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2},"future_usage":true}`
					}
					payload := `{"model":"model","choices":[],` + fields + `}`
					wire := ": diagnostic\r\nid: error-1\r\ndata: " + payload + "\r\n\r\ndata: [DONE]\r\n\r\n"
					handler := &OpenAIStreamHandler{Usage: &types.Usage{}, usageFilter: hideCollectedUsage, formatProviderUsage: compatible, UsageHandler: normalizeReasoningUsageForTest}
					stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{Body: io.NopCloser(strings.NewReader(wire))}, handler.HandleExactChatSSE, requester.StreamReadOptions{})
					if apiErr != nil {
						t.Fatal(apiErr)
					}
					output, err := collectNativeSSE(t, stream)
					if !errors.Is(err, io.EOF) {
						t.Fatalf("原始错误交付改变：%v", err)
					}
					if !compatible || !hasUsage {
						if output != wire {
							t.Fatalf("错误事件原文改变：%s", output)
						}
					}
					if handler.Usage.HasProviderBaseUsage() != hasUsage {
						t.Fatalf("错误帧usage未独立观察：%+v", handler.Usage)
					}
					if !strings.Contains(output, `"error":`+detail) && !strings.Contains(output, `"error":"stopped"`) {
						t.Fatalf("错误诊断改变：%s", output)
					}
					if !strings.Contains(output, `"code":"execution_failed"`) {
						t.Fatalf("错误码丢失：%s", output)
					}
					if hasUsage {
						if handler.Usage.CompletionTokens != 7 {
							t.Fatalf("内部usage未规范化：%+v", handler.Usage)
						}
						want := `"completion_tokens":5`
						if compatible {
							want = `"completion_tokens":7`
						}
						if !strings.Contains(output, want) || !strings.Contains(output, `"future_usage":true`) {
							t.Fatalf("错误帧usage投影错误：%s", output)
						}
					}
				})
			}
		}
	}
}

func TestUsageFilteringPreservesContentExtensionsAndErrorSemantics(t *testing.T) {
	usage := `"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}`
	for _, test := range []struct {
		name, event, payload, want string
	}{
		{name: "ordinary usage", payload: `{"choices":[],` + usage + `}`},
		{name: "default message event", event: "message", payload: `{"choices":[],` + usage + `}`},
		{name: "chat usage object", payload: `{"object":"chat.completion.chunk","choices":[],` + usage + `}`},
		{name: "usage with provider padding", payload: `{"object":"chat.completion.chunk","choices":[],"obfuscation":"ab",` + usage + `}`},
		{name: "future padding shape", payload: `{"choices":[],"obfuscation":{"future":true},` + usage + `}`, want: `{"choices":[],"obfuscation":{"future":true}}`},
		{name: "completion usage object", payload: `{"object":"text_completion","choices":[],` + usage + `}`},
		{name: "extension fields", payload: `{"future":{"n":9007199254740993},"choices":[],` + usage + `}`, want: `{"future":{"n":9007199254740993},"choices":[]}`},
		{name: "mixed content", payload: `{"choices":[{"index":0,"delta":{"content":"ok","future":9007199254740993}}],` + usage + `}`, want: `{"choices":[{"index":0,"delta":{"content":"ok","future":9007199254740993}}]}`},
		{name: "choice usage", payload: `{"choices":[{"delta":{"content":"ok","future":9007199254740993},` + usage + `}]}`, want: `{"choices":[{"delta":{"content":"ok","future":9007199254740993}}]}`},
		{name: "named error", event: "error", payload: `{"choices":[],` + usage + `}`, want: "original"},
		{name: "error envelope", payload: `{"error":{"code":"execution_failed","message":"stopped"},"choices":[],` + usage + `}`, want: "original"},
		{name: "named extension", event: "vendor.notice", payload: `{"choices":[],` + usage + `}`, want: "original"},
		{name: "error type", payload: `{"type":"error","choices":[],` + usage + `}`, want: "original"},
		{name: "future type", payload: `{"type":{"kind":"future"},"choices":[],` + usage + `}`, want: "original"},
		{name: "future object", payload: `{"object":"vendor.notice","choices":[],` + usage + `}`, want: "original"},
		{name: "future choices union", payload: `{"choices":{"kind":"future"},` + usage + `}`, want: "original"},
	} {
		t.Run(test.name, func(t *testing.T) {
			prefix := ""
			if test.event != "" {
				prefix = "event: " + test.event + "\r\n"
			}
			done := "data: [DONE]\r\n\r\n"
			wire := prefix + "id: event-1\r\ndata: " + test.payload + "\r\n\r\n" + done
			handler := &OpenAIStreamHandler{Usage: &types.Usage{}, usageFilter: hideCollectedUsage}
			stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{Body: io.NopCloser(strings.NewReader(wire))}, handler.HandleExactChatSSE, requester.StreamReadOptions{})
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			output, err := collectNativeSSE(t, stream)
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			want := prefix + "id: event-1\r\n\r\n" + done
			if test.want == "original" {
				want = wire
			} else if test.want != "" {
				want = prefix + "id: event-1\r\ndata: " + test.want + "\r\n\r\n" + done
			}
			if output != want {
				t.Fatalf("usage展示过滤改变事件语义：want=%q got=%q", want, output)
			}
			if !handler.Usage.HasProviderBaseUsage() {
				t.Fatalf("过滤清除了内部usage：%+v", handler.Usage)
			}
		})
	}
}

func TestUsageFilteringHidesNullPlaceholdersOnOrdinaryChunks(t *testing.T) {
	for _, test := range []struct {
		payload, want string
	}{
		{`{"object":"chat.completion.chunk","choices":[{"delta":{"content":"ok"}}],"usage":null}`, `{"object":"chat.completion.chunk","choices":[{"delta":{"content":"ok"}}]}`},
		{`{"object":"text_completion","choices":[{"text":"ok","usage":null}]}`, `{"object":"text_completion","choices":[{"text":"ok"}]}`},
		{`{"choices":[],"usage":null}`, `{"choices":[]}`},
		{`{"choices":[{"text":"ok"}],"\u0075sage":null}`, `{"choices":[{"text":"ok"}]}`},
		{`{"choices":[],"usage":null,"\u0075sage":null}`, `{"choices":[],"usage":null,"\u0075sage":null}`},
		{`{"choices":[{"usage":null,"usage":null}],"usage":null}`, `{"choices":[{"usage":null,"usage":null}],"usage":null}`},
		{`{"choices":[{"text":"ok"}],"usage":[1]}`, `{"choices":[{"text":"ok"}],"usage":[1]}`},
		{`{"error":{"message":"stopped"},"choices":[],"usage":null}`, `{"error":{"message":"stopped"},"choices":[],"usage":null}`},
	} {
		event := "data: " + test.payload + "\n\n"
		frame := readStreamUsageFrame([]byte(test.payload), hideCollectedUsage)
		got, drop := event, false
		if frame.hide {
			got, drop = frame.hideUsage(event)
		}
		if want := "data: " + test.want + "\n\n"; drop || got != want {
			t.Errorf("流帧过滤错误：want=%q got=%q drop=%t", want, got, drop)
		}
	}
}

func TestHiddenUsagePreservesSSEMetadataWithoutDispatchingEmptyData(t *testing.T) {
	for _, completion := range []bool{false, true} {
		for _, test := range []struct{ name, prefix string }{
			{name: "no metadata"},
			{name: "event id", prefix: "id: usage-1\r\n"},
			{name: "empty id resets cursor", prefix: "id:\r\n"},
			{name: "id without colon", prefix: "id\r\n"},
			{name: "reconnect delay", prefix: "retry: 1200\r\n"},
			{name: "comment", prefix: ": heartbeat\r\n"},
			{name: "future field", prefix: "future-sse-field: opaque\r\n"},
			{name: "combined metadata", prefix: ": provider comment\r\nid: usage-1\r\nretry: 1200\r\nevent: message\r\nfuture-sse-field: opaque\r\n"},
		} {
			t.Run(fmt.Sprintf("completion=%t/%s", completion, test.name), func(t *testing.T) {
				usage := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\r\ndata: \"completion_tokens\":3,\"total_tokens\":5}}\r\n\r\n"
				next := "data: {\"choices\":[{\"delta\":{\"content\":\"after usage\"}}]}\r\n\r\ndata: [DONE]\r\n\r\n"
				wire := test.prefix + usage + next
				handler := &OpenAIStreamHandler{Usage: &types.Usage{}, usageFilter: hideCollectedUsage}
				handle := handler.HandleExactChatSSE
				if completion {
					handle = handler.handleExactCompletionSSE
				}
				stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{Body: io.NopCloser(strings.NewReader(wire))}, handle, requester.StreamReadOptions{})
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				got, err := collectNativeSSE(t, stream)
				want := next
				if test.prefix != "" {
					want = test.prefix + "\r\n" + next
				}
				if got != want || !errors.Is(err, io.EOF) {
					t.Fatalf("hidden usage lost metadata or created an extra data event: want=%q got=%q err=%v", want, got, err)
				}
				if !handler.Usage.HasProviderBaseUsage() || handler.Usage.TotalTokens != 5 {
					t.Fatalf("usage evidence lost during metadata-only delivery: %+v", handler.Usage)
				}
			})
		}
	}
}

func TestUsageFilteringPreservesSSEFramesWithMixedLineEndings(t *testing.T) {
	const done = "data: [DONE]\r\n\r\n"
	for _, completion := range []bool{false, true} {
		for _, test := range []struct {
			name, input, want string
		}{
			{"content", "data: {\"choices\":[{\"text\":\"ok\"}],\rdata: \"usage\":null}\n\n", "data: {\"choices\":[{\"text\":\"ok\"}]}\r\r\n"},
			{"metadata", "id: usage-1\rdata: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n", "id: usage-1\r\r\n"},
		} {
			t.Run(fmt.Sprintf("completion=%t/%s", completion, test.name), func(t *testing.T) {
				handler := &OpenAIStreamHandler{Usage: &types.Usage{}, usageFilter: hideCollectedUsage}
				handle := handler.HandleExactChatSSE
				if completion {
					handle = handler.handleExactCompletionSSE
				}
				response := &http.Response{Body: io.NopCloser(iotest.OneByteReader(strings.NewReader(test.input + done)))}
				stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, response, handle, requester.StreamReadOptions{})
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				got, err := collectNativeSSE(t, stream)
				if want := test.want + done; got != want || !errors.Is(err, io.EOF) {
					t.Fatalf("SSE 事件边界变化：got=%q want=%q err=%v", got, want, err)
				}
			})
		}
	}
}

func TestTypedChatStreamObservesErrorFrameUsage(t *testing.T) {
	for _, hasUsage := range []bool{false, true} {
		payload := `{"error":{"code":"execution_failed","message":"stopped"},"choices":[]`
		if hasUsage {
			payload += `,"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2}}`
		}
		payload += "}"
		line := []byte("data: " + payload)
		data, errs := make(chan string, 1), make(chan error, 1)
		handler := &OpenAIStreamHandler{Usage: &types.Usage{}, UsageHandler: normalizeReasoningUsageForTest}
		handler.HandlerChatStream(&line, data, errs)
		select {
		case err := <-errs:
			apiErr, ok := err.(*types.OpenAIError)
			if !ok || apiErr.Code != "execution_failed" || apiErr.Message != "stopped" {
				t.Fatalf("typed错误语义改变：%v", err)
			}
		default:
			t.Fatal("typed错误信号丢失")
		}
		if len(data) != 0 || handler.Usage.HasProviderBaseUsage() != hasUsage {
			t.Fatalf("typed错误交付或usage观察改变：%+v", handler.Usage)
		}
		if hasUsage && handler.Usage.CompletionTokens != 7 {
			t.Fatalf("typed错误usage未规范化：%+v", handler.Usage)
		}
	}
}

func TestChatCompletionObservesUsageBeforeReturningBodyError(t *testing.T) {
	for _, hasUsage := range []bool{false, true} {
		t.Run(fmt.Sprintf("usage=%v", hasUsage), func(t *testing.T) {
			payload := `{"error":{"code":"execution_failed","message":"stopped"},"model":"model","choices":[]`
			if hasUsage {
				payload += `,"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2}}`
			}
			payload += "}"
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, payload)
			}))
			defer server.Close()
			previous := requester.HTTPClient
			requester.HTTPClient = server.Client()
			defer func() { requester.HTTPClient = previous }()
			proxy := ""
			p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeCustom, Proxy: &proxy, Plugin: model.NewCustomEndpointPlugin()}, server.URL)
			p.Usage = &types.Usage{}
			p.UsageHandler = normalizeReasoningUsageForTest
			response, apiErr := p.CreateChatCompletion(&types.ChatCompletionRequest{Model: "model", Messages: []types.ChatCompletionMessage{{Role: "user", Content: "Hi"}}})
			if response != nil || apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "execution_failed" || apiErr.Message != "stopped" {
				t.Fatalf("body错误语义改变：response=%+v err=%+v", response, apiErr)
			}
			if calls.Load() != 1 || p.Usage.HasProviderBaseUsage() != hasUsage {
				t.Fatalf("错误响应被重复执行或usage未观察：calls=%d usage=%+v", calls.Load(), p.Usage)
			}
			if hasUsage && p.Usage.CompletionTokens != 7 {
				t.Fatalf("错误usage未规范化：%+v", p.Usage)
			}
		})
	}
}

func normalizeReasoningUsageForTest(usage *types.Usage) bool {
	usage.CompletionTokens += usage.CompletionTokensDetails.ReasoningTokens
	return usage.CompletionTokensDetails.ReasoningTokens != 0
}

func TestChatSSEUsageObservationWithoutProviderErrorReturnsNil(t *testing.T) {
	for _, payload := range []string{
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`{"future":{"kind":"extension"}}`,
		`{malformed`,
	} {
		handler := &OpenAIStreamHandler{Usage: &types.Usage{}}
		frame := readStreamUsageFrame([]byte(payload), keepStreamUsage)
		handler.observeExactChatStreamPayload(&frame)
		if err := frame.providerError; err != nil {
			t.Fatalf("非错误帧生成错误控制信号：%v", err)
		}
	}
}

func TestChatStreamKeepsCompleteCumulativeUsageAcrossSparseObservations(t *testing.T) {
	for _, test := range []struct {
		name, tail                string
		prompt, completion, total int
		base, conflict            bool
	}{
		{"empty usage", `"model":"model","usage":{}`, 3, 5, 8, true, false},
		{"prompt only", `"model":"model","usage":{"prompt_tokens":99}`, 3, 5, 8, true, false},
		{"completion only", `"model":"model","usage":{"completion_tokens":99}`, 3, 5, 8, true, false},
		{"new complete snapshot", `"model":"model","usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}`, 4, 6, 10, true, false},
		{"explicit zero snapshot", `"model":"model","usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, 0, 0, 0, true, false},
		{"invalid complete snapshot", `"model":"model","usage":{"prompt_tokens":3,"completion_tokens":-1,"total_tokens":2}`, 3, -1, 2, false, false},
		{"sparse model conflict", `"model":"other","usage":{}`, 3, 5, 8, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire := `data: {"model":"model","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}` + "\n\n" + `data: {"choices":[],` + test.tail + `,"future":true}` + "\n\ndata: [DONE]\n\n"
			h := &OpenAIStreamHandler{Usage: &types.Usage{}}
			stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{Body: io.NopCloser(strings.NewReader(wire))}, h.HandleExactChatSSE, requester.StreamReadOptions{})
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			output, err := collectNativeSSE(t, stream)
			if !errors.Is(err, io.EOF) || output != wire {
				t.Fatalf("usage观察改变原始交付：%q %v", output, err)
			}
			if u := h.Usage; u.PromptTokens != test.prompt || u.CompletionTokens != test.completion || u.TotalTokens != test.total || u.HasProviderBaseUsage() != test.base || u.AttributionConflict != test.conflict {
				t.Fatalf("累计快照选择错误：%+v", u)
			}
		})
	}
}

func TestChatSparseUsageKeepsIndependentCumulativeBillingEvidence(t *testing.T) {
	h := &OpenAIStreamHandler{Usage: &types.Usage{}, UsageHandler: func(u *types.Usage) bool {
		if u.ProviderServerToolUse != nil {
			u.SetProviderExtraBilling(types.APIToolTypeWebSearch, "", u.ProviderServerToolUse.WebSearchRequests)
		}
		return false
	}}
	for _, payload := range []string{
		`{"model":"model","usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8,"server_tool_use":{"web_search_requests":1}}}`,
		`{"model":"model","usage":{"server_tool_use":{"web_search_requests":2}}}`,
		`{"model":"model","usage":{"server_tool_use":{"web_search_requests":2}}}`,
	} {
		frame := readStreamUsageFrame([]byte(payload), keepStreamUsage)
		h.observeExactChatStreamPayload(&frame)
		if err := frame.providerError; err != nil {
			t.Fatal(err)
		}
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "")
	if !h.Usage.HasProviderBaseUsage() || h.Usage.CompletionTokens != 5 || !h.Usage.HasProviderExtraBilling(key) || h.Usage.ExtraBilling[key].CallCount != 2 {
		t.Fatalf("独立累计用量被丢弃或重复累加：%+v", h.Usage)
	}
}

func TestExactChatCompletionReplaysSuccessfulHTTPErrorEnvelope(t *testing.T) {
	for _, hasUsage := range []bool{false, true} {
		t.Run(fmt.Sprintf("usage=%v", hasUsage), func(t *testing.T) {
			payload := `{"error":{"code":"execution_failed","message":"stopped","future_error":true},"future_response":9007199254740993`
			if hasUsage {
				payload += `,"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}`
			}
			payload += "}"
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, payload)
			}))
			defer server.Close()
			previous := requester.HTTPClient
			requester.HTTPClient = server.Client()
			defer func() { requester.HTTPClient = previous }()
			proxy := ""
			p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, server.URL)
			p.SetProviderRawJSONReplay(true)
			p.Usage = &types.Usage{}
			response, apiErr := p.CreateChatCompletion(&types.ChatCompletionRequest{Model: "model", Messages: []types.ChatCompletionMessage{{Role: "user", Content: "Hi"}}})
			if apiErr != nil || response == nil {
				t.Fatalf("exact-wire改写上游200错误正文：%+v", apiErr)
			}
			if got := string(response.ReplayProviderRawJSON()); got != payload {
				t.Fatalf("exact-wire丢失错误字段：%s", got)
			}
			if calls.Load() != 1 || p.Usage.HasProviderBaseUsage() != hasUsage {
				t.Fatalf("重复执行或错误usage归属：calls=%d usage=%+v", calls.Load(), p.Usage)
			}
		})
	}
}
