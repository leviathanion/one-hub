package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/providers/antigravity/wire"
	"one-api/types"
)

type antigravityStreamBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *antigravityStreamBody) Close() error { b.closed.Store(true); return nil }

type antigravityFailingReader struct{ err error }

func (r antigravityFailingReader) Read([]byte) (int, error) { return 0, r.err }

func antigravitySSE(payload string) string { return "data: " + payload + "\n\n" }

func antigravityChatDecoder() *wire.ResponseDecoder {
	return wire.NewResponseDecoder(&wire.Context{Dialect: wire.DialectChat, Model: "model", ExpectedCandidates: 1, IncludeUsage: true}, "chatcmpl-test", true)
}

func collectAntigravityStream(t *testing.T, stream requester.RawSSEEventStream) ([]string, []error) {
	t.Helper()
	defer requester.CloseAndDrainStream(stream)
	data, errs := stream.Recv()
	var frames []string
	var failures []error
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for data != nil || errs != nil {
		select {
		case <-timer.C:
			t.Fatal("流未及时结束")
		case frame, ok := <-data:
			if !ok {
				data = nil
			} else {
				frames = append(frames, frame)
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
			} else {
				failures = append(failures, err)
			}
		}
	}
	return frames, failures
}

func TestAntigravityStreamReadsTrailingUsageBeforeTerminal(t *testing.T) {
	input := antigravitySSE(`{"response":{"candidates":[{"index":0,"content":{"parts":[{"text":"hello"}]}}]}}`) +
		antigravitySSE(`{"response":{"candidates":[{"index":0,"finishReason":"STOP"}]}}`) +
		antigravitySSE(`{"response":{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}}`)
	body := &antigravityStreamBody{Reader: strings.NewReader(input)}
	var usage json.RawMessage
	stream, apiErr := newAntigravityStream(nil, &http.Response{Body: body}, antigravityChatDecoder(), func(raw json.RawMessage, _ string) { usage = raw }, nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if !requester.IsRawSSEEventStream(stream) {
		t.Fatal("缺少完整 SSE 事件标记")
	}
	frames, failures := collectAntigravityStream(t, stream)
	if len(failures) != 1 || !errors.Is(failures[0], io.EOF) {
		t.Fatalf("结束错误 = %v", failures)
	}
	joined := strings.Join(frames, "")
	if strings.Count(joined, "data: [DONE]") != 1 || !strings.Contains(joined, `"total_tokens":8`) || !strings.Contains(joined, `"content":"hello"`) {
		t.Fatalf("缺少正文、尾部用量或唯一终态: %s", joined)
	}
	if !strings.Contains(string(usage), `"totalTokenCount":8`) || !body.closed.Load() {
		t.Fatalf("用量未观察或 body 未关闭: %s, %t", usage, body.closed.Load())
	}
	if !errors.Is(context.Cause(requester.StreamReadContext(stream)), io.EOF) {
		t.Fatal("正常 EOF 未保留读取事实")
	}
}

func TestAntigravityStreamFailureDoesNotSynthesizeSuccess(t *testing.T) {
	complete := antigravitySSE(`{"response":{"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"STOP"}]}}`)
	transportErr := errors.New("connection reset after candidate finish")
	tests := []struct {
		name string
		body io.Reader
		want error
	}{
		{"missing_terminal", strings.NewReader(antigravitySSE(`{"response":{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}}`)), wire.ErrTerminalMissing},
		{"partial_event_after_terminal", strings.NewReader(complete + "data: {\"response\":"), io.ErrUnexpectedEOF},
		{"transport_error_after_terminal", io.MultiReader(strings.NewReader(complete), antigravityFailingReader{transportErr}), transportErr},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := &antigravityStreamBody{Reader: test.body}
			stream, apiErr := newAntigravityStream(nil, &http.Response{Body: body}, antigravityChatDecoder(), nil, nil)
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			frames, failures := collectAntigravityStream(t, stream)
			if len(failures) != 1 || !errors.Is(failures[0], test.want) {
				t.Fatalf("错误 = %v, want %v", failures, test.want)
			}
			joined := strings.Join(frames, "")
			if strings.Contains(joined, "[DONE]") || strings.Contains(joined, `"finish_reason":"stop"`) {
				t.Fatalf("失败后合成了成功终态: %s", joined)
			}
			if !body.closed.Load() {
				t.Fatal("失败后没有关闭 body")
			}
		})
	}
}

func TestAntigravityStreamObservesUsageInLateError(t *testing.T) {
	input := antigravitySSE(`{"response":{"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"STOP"}]}}`) +
		antigravitySSE(`{"response":{"error":{"message":"secret","code":429},"usageMetadata":{"promptTokenCount":6,"candidatesTokenCount":2}}}`)
	var usage json.RawMessage
	sanitized := errors.New("sanitized upstream error")
	stream, apiErr := newAntigravityStream(nil, &http.Response{Body: io.NopCloser(strings.NewReader(input))}, antigravityChatDecoder(), func(raw json.RawMessage, _ string) { usage = raw }, func(err error) error {
		var upstream *wire.UpstreamError
		if !errors.As(err, &upstream) {
			t.Errorf("收到非上游错误: %v", err)
		}
		return sanitized
	})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	frames, failures := collectAntigravityStream(t, stream)
	if len(failures) != 1 || !errors.Is(failures[0], sanitized) {
		t.Fatalf("错误 = %v", failures)
	}
	if !strings.Contains(string(usage), `"promptTokenCount":6`) || strings.Contains(strings.Join(frames, ""), "[DONE]") {
		t.Fatalf("尾部错误丢失用量或错误合成为成功: usage=%s, frames=%v", usage, frames)
	}
}

func TestAntigravityNativeStreamPreservesUnknownFieldsAndSSEMetadata(t *testing.T) {
	inner := "{\n\"future\":9007199254740993,\"candidates\":[{\"finishReason\":\"FUTURE_REASON\"}]\n}"
	input := ": keepalive\r\n\r\n" + "id: upstream-id\nevent: message\ndata: {\"response\":\ndata: " + strings.ReplaceAll(inner, "\n", "\ndata: ") + "\ndata: }\n\n"
	stream, apiErr := newAntigravityNativeStream(nil, &http.Response{Body: io.NopCloser(strings.NewReader(input))}, wire.NewNativeDecoder(1), nil, nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	frames, failures := collectAntigravityStream(t, stream)
	if len(failures) != 1 || !errors.Is(failures[0], io.EOF) {
		t.Fatalf("错误 = %v", failures)
	}
	var payload string
	for _, frame := range frames {
		if value, present := commonresponses.SSEDataPayload(frame); present {
			payload = value
		}
	}
	if payload != inner {
		t.Fatalf("内层 JSON 被改写: got %q, want %q", payload, inner)
	}
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, ": keepalive\r\n\r\n") || !strings.Contains(joined, "id: upstream-id\nevent: message\n") {
		t.Fatalf("SSE 元数据未保留: %q", joined)
	}
}

func TestAntigravityNativeStreamDeliversSanitizedErrorBeforeFailure(t *testing.T) {
	input := antigravitySSE(`{"error":{"code":403,"message":"access-token-secret","details":[{"reason":"DENIED"}]}}`)
	safe := &types.OpenAIErrorWithStatusCode{RawBody: []byte(`{"error":{"code":403,"message":"[REDACTED]","details":[{"reason":"DENIED"}]}}`), StatusCode: 403}
	stream, apiErr := newAntigravityNativeStream(nil, &http.Response{Body: io.NopCloser(strings.NewReader(input))}, wire.NewNativeDecoder(1), nil, func(error) error { return safe })
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	frames, failures := collectAntigravityStream(t, stream)
	if len(frames) != 1 || len(failures) != 1 || failures[0] != safe {
		t.Fatalf("错误事件或错误事实缺失: %v, %v", frames, failures)
	}
	payload, present := commonresponses.SSEDataPayload(frames[0])
	if !present || payload != string(safe.RawBody) || strings.Contains(payload, "access-token-secret") {
		t.Fatalf("原生错误协议未安全保留: %s", payload)
	}
}

func TestAntigravityStreamCancellationStopsBlockedReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/", nil)
	if err != nil {
		t.Fatal(err)
	}
	httpRequester := requester.NewHTTPRequester("", nil)
	httpRequester.Context = ctx
	stream, apiErr := newAntigravityStream(httpRequester, &http.Response{Body: reader, Request: req}, antigravityChatDecoder(), nil, nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	stream.Recv()
	cancel()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
	defer drainCancel()
	if err = requester.CloseAndDrainStreamContext(drainCtx, stream); err != nil {
		t.Fatalf("取消后未能排空: %v", err)
	}
	if !errors.Is(context.Cause(requester.StreamReadContext(stream)), context.Canceled) {
		t.Fatalf("取消错误被改写: %v", context.Cause(requester.StreamReadContext(stream)))
	}
}

func TestAntigravityStreamCloseBeforeRecvDoesNotFinalize(t *testing.T) {
	body := &antigravityStreamBody{Reader: strings.NewReader(antigravitySSE(`{"response":{"candidates":[{"finishReason":"STOP"}]}}`))}
	stream, apiErr := newAntigravityStream(nil, &http.Response{Body: body}, antigravityChatDecoder(), nil, nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	stream.Close()
	frames, failures := collectAntigravityStream(t, stream)
	if len(frames) != 0 || len(failures) != 0 || !body.closed.Load() {
		t.Fatalf("关闭后仍解码: %v, %v, closed=%t", frames, failures, body.closed.Load())
	}
}

func TestAntigravityStreamEnforcesEventBudget(t *testing.T) {
	line := ":" + strings.Repeat("x", wire.MaxRetainedBytes/2) + "\n"
	stream, apiErr := newAntigravityNativeStream(nil, &http.Response{Body: io.NopCloser(strings.NewReader(line + line + "\n"))}, wire.NewNativeDecoder(1), nil, nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	frames, failures := collectAntigravityStream(t, stream)
	if len(frames) != 0 || len(failures) != 1 || !errors.Is(failures[0], requester.ErrSSEEventTooLarge) {
		t.Fatalf("未执行事件容量约束: frames=%d, errors=%v", len(frames), failures)
	}
}

func TestAntigravityAggregateWaitsForUsageAndRejectsTransportFailure(t *testing.T) {
	input := antigravitySSE(`{"response":{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}]}}`) +
		antigravitySSE(`{"response":{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}}`)
	for _, failed := range []bool{false, true} {
		name := "complete"
		var reader io.Reader = strings.NewReader(input)
		if failed {
			name = "transport_failed"
			reader = io.MultiReader(reader, antigravityFailingReader{io.ErrUnexpectedEOF})
		}
		t.Run(name, func(t *testing.T) {
			body := &antigravityStreamBody{Reader: reader}
			decoder := wire.NewResponseDecoder(&wire.Context{Dialect: wire.DialectChat, Model: "model", ExpectedCandidates: 1}, "chatcmpl-test", false)
			var usage json.RawMessage
			result, apiErr := newAntigravityAggregate(nil, &http.Response{Body: body}, decoder, func(raw json.RawMessage, _ string) { usage = raw }, nil)
			if failed {
				if len(result) != 0 || apiErr == nil || !apiErr.UpstreamAccepted {
					t.Fatalf("传输失败被聚合为成功: %s, %v", result, apiErr)
				}
			} else if apiErr != nil || !strings.Contains(string(result), `"content":"hello"`) || !strings.Contains(string(result), `"total_tokens":8`) {
				t.Fatalf("聚合响应不完整: %s, %v", result, apiErr)
			}
			if !strings.Contains(string(usage), `"totalTokenCount":8`) || !body.closed.Load() {
				t.Fatalf("聚合丢失用量或生命周期未结束: usage=%s, closed=%t", usage, body.closed.Load())
			}
		})
	}
}

func TestAntigravityHTTPBodyEOFDoesNotCancelDownstreamDelivery(t *testing.T) {
	payload := antigravitySSE(`{"response":{"candidates":[{"content":{"parts":[{"text":"complete body"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = io.WriteString(w, payload)
	}))
	defer server.Close()
	previousClient := requester.HTTPClient
	transport := http.DefaultTransport.(*http.Transport).Clone()
	requester.HTTPClient = &http.Client{Transport: transport}
	t.Cleanup(func() {
		requester.HTTPClient = previousClient
		transport.CloseIdleConnections()
	})
	for _, mode := range []string{"chat_stream", "native_stream", "chat_aggregate"} {
		t.Run(mode, func(t *testing.T) {
			callerCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			httpRequester := requester.NewHTTPRequester("", nil).ForHTTPProfile(requester.HTTPProfileLongStream)
			httpRequester.Context = callerCtx
			req, err := httpRequester.NewRequest(http.MethodPost, server.URL, httpRequester.WithBody([]byte(`{}`)))
			if err != nil {
				t.Fatal(err)
			}
			resp, apiErr := httpRequester.SendRequestRawCheckedNoRedirect(req)
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			defer resp.Body.Close()
			var output string
			if mode == "chat_aggregate" {
				decoder := wire.NewResponseDecoder(&wire.Context{Dialect: wire.DialectChat, Model: "model", ExpectedCandidates: 1}, "chatcmpl-test", false)
				body, aggregateErr := newAntigravityAggregate(httpRequester, resp, decoder, nil, nil)
				if aggregateErr != nil {
					t.Fatal(aggregateErr)
				}
				output = string(body)
			} else {
				var stream requester.RawSSEEventStream
				if mode == "native_stream" {
					stream, apiErr = newAntigravityNativeStream(httpRequester, resp, wire.NewNativeDecoder(1), nil, nil)
				} else {
					stream, apiErr = newAntigravityStream(httpRequester, resp, antigravityChatDecoder(), nil, nil)
				}
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				frames, failures := collectAntigravityStream(t, stream)
				if len(failures) != 1 || !errors.Is(failures[0], io.EOF) {
					t.Fatalf("正常 HTTP EOF 被当成取消: %v", failures)
				}
				output = strings.Join(frames, "")
				if mode == "chat_stream" && strings.Count(output, "data: [DONE]") != 1 {
					t.Fatalf("EOF 后未交付成功终态: %s", output)
				}
			}
			if !strings.Contains(output, "complete body") {
				t.Fatalf("EOF 后未交付正文: %s", output)
			}
			if callerCtx.Err() != nil || !errors.Is(resp.Request.Context().Err(), context.Canceled) {
				t.Fatalf("未覆盖 transport 独立回收场景: caller=%v, transport=%v", callerCtx.Err(), resp.Request.Context().Err())
			}
		})
	}
}
