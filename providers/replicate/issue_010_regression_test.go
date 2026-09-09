package replicate

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/common/requester"
	"one-api/types"
)

type replicateI010StreamResult struct {
	text   string
	errors []error
	chunks []types.ChatCompletionStreamResponse
}

func collectReplicateI010Stream(t *testing.T, wire string, prediction *ReplicateResponse[[]string]) replicateI010StreamResult {
	t.Helper()
	handler := &ReplicateStreamHandler{
		Usage:     &types.Usage{},
		ModelName: "replicate-test-model",
		ID:        "pred_i010",
		framer:    requester.NewSSEEventFramer(16 << 20),
		fetchPrediction: func() *ReplicateResponse[[]string] {
			return prediction
		},
	}
	stream, apiErr := requester.RequestNoTrimStreamWithEmitterOptions[string](nil, &http.Response{
		Body: io.NopCloser(strings.NewReader(wire)),
	}, handler.HandlerChatStreamWithEmitter, requester.StreamReadOptions{RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatalf("构造 Replicate 流失败: %v", apiErr)
	}
	defer stream.Close()

	dataChan, errorChan := stream.Recv()
	result := replicateI010StreamResult{}
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for dataChan != nil || errorChan != nil {
		select {
		case chunk, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}
			var decoded types.ChatCompletionStreamResponse
			if err := json.Unmarshal([]byte(chunk), &decoded); err != nil {
				t.Fatalf("客户端流块不是合法 ChatCompletion JSON: %v; raw=%q", err, chunk)
			}
			for _, choice := range decoded.Choices {
				result.text += choice.Delta.Content
			}
			result.chunks = append(result.chunks, decoded)
		case err, ok := <-errorChan:
			if !ok {
				errorChan = nil
				continue
			}
			if err != nil {
				result.errors = append(result.errors, err)
			}
		case <-timeout.C:
			t.Fatalf("读取 Replicate 流超时: data=%d errors=%d", len(result.chunks), len(result.errors))
		}
	}
	return result
}

func TestReplicateI010PreservesPayloadWhitespaceThroughReaderAndHandler(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want string
	}{
		{
			name: "word spacing",
			wire: "data: Hello\n\ndata:  world\n\nevent: done\ndata: {}\n\n",
			want: "Hello world",
		},
		{
			name: "code indentation",
			wire: "data:     if ready {\n\n" +
				"event: done\ndata: {}\n\n",
			want: "    if ready {",
		},
		{
			name: "space only",
			wire: "data:   \n\n" +
				"event: done\ndata: {}\n\n",
			want: "  ",
		},
		{
			name: "trailing spaces",
			wire: "data: text  \n\n" +
				"event: done\ndata: {}\n\n",
			want: "text  ",
		},
		{
			name: "multiline data and tab",
			wire: "data: first\ndata:  second\ndata:\ndata: \tthird\n\n" +
				"event: done\ndata: {}\n\n",
			want: "first\n second\n\n\tthird",
		},
		{
			name: "bare data line",
			wire: "data: first\ndata\ndata: last\n\n" +
				"event: done\ndata: {}\n\n",
			want: "first\n\nlast",
		},
		{
			name: "CRLF",
			wire: "data: Hello\r\n\r\ndata:  world\r\n\r\n" +
				"event: done\r\ndata: {}\r\n\r\n",
			want: "Hello world",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := collectReplicateI010Stream(t, test.wire, &ReplicateResponse[[]string]{Status: "succeeded"})
			if result.text != test.want {
				t.Fatalf("客户端收到的文本=%q，期望逐字节为 %q", result.text, test.want)
			}
			if len(result.errors) != 1 || !errors.Is(result.errors[0], io.EOF) {
				t.Fatalf("正常完成的终态错误=%v，期望单个 io.EOF", result.errors)
			}
			if len(result.chunks) < 2 {
				t.Fatalf("正常流缺少文本块或完成块: %+v", result.chunks)
			}
			last := result.chunks[len(result.chunks)-1]
			if len(last.Choices) != 1 || last.Choices[0].FinishReason != "stop" {
				t.Fatalf("完成事件未保留 stop: %+v", last)
			}
		})
	}
}

func TestReplicateI010ErrorEventPreservesProtocolHandling(t *testing.T) {
	result := collectReplicateI010Stream(t,
		"data: before error\n\nevent: error\ndata: provider failure\n\n",
		&ReplicateResponse[[]string]{Status: "succeeded"})
	if result.text != "before error" {
		t.Fatalf("错误事件前的文本被改变: %q", result.text)
	}
	if len(result.errors) != 1 {
		t.Fatalf("错误事件产生了 %d 个错误: %v", len(result.errors), result.errors)
	}
	apiErr, ok := result.errors[0].(*types.OpenAIErrorWithStatusCode)
	if !ok {
		t.Fatalf("错误事件类型=%T，期望 OpenAIErrorWithStatusCode", result.errors[0])
	}
	if apiErr.Code != "prediction_failed" || apiErr.StatusCode != http.StatusBadGateway || !apiErr.UpstreamAccepted || apiErr.Message != "provider failure" {
		t.Fatalf("错误事件协议语义错误: %+v", apiErr)
	}
}

func TestReplicateI010DisconnectReportsMissingTerminalAfterDeliveredText(t *testing.T) {
	result := collectReplicateI010Stream(t, "data: partial  \n\n", &ReplicateResponse[[]string]{Status: "succeeded"})
	if result.text != "partial  " {
		t.Fatalf("断流前文本空白被改变: %q", result.text)
	}
	if len(result.errors) != 1 || !errors.Is(result.errors[0], requester.ErrStreamProtocolTerminalMissing) {
		t.Fatalf("断流错误=%v，期望 ErrStreamProtocolTerminalMissing", result.errors)
	}
}

func TestReplicateI010DataPayloadCannotBecomeControlEvent(t *testing.T) {
	result := collectReplicateI010Stream(t, "data: event: done\n\n", &ReplicateResponse[[]string]{Status: "succeeded"})
	if result.text != "event: done" {
		t.Fatalf("正文控制字符串被误解释或改写: %q", result.text)
	}
	if len(result.errors) != 1 || !errors.Is(result.errors[0], requester.ErrStreamProtocolTerminalMissing) {
		t.Fatalf("正文控制字符串触发了错误=%v，期望仅报告真实断流", result.errors)
	}
}
