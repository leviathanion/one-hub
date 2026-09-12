package relay_util

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
	"one-api/model"
	"one-api/providers/claude"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func collectI027ClaudeChatIntoResponses(t *testing.T, stream requester.StreamReaderInterface[string], converter *OpenAIResponsesStreamConverter) {
	t.Helper()
	data, errs := stream.Recv()
	defer stream.Close()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	terminal := false
	for data != nil || errs != nil {
		select {
		case chunk, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			if err := converter.ProcessStreamData(chunk); err != nil {
				t.Fatalf("Chat → Responses chunk 转换失败：%v；chunk=%s", err, chunk)
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err == nil {
				continue
			}
			if !errors.Is(err, io.EOF) {
				t.Fatalf("Claude Chat stream 失败：%v", err)
			}
			if !terminal {
				if err := converter.ProcessStreamData("[DONE]"); err != nil {
					t.Fatalf("Responses converter 完成失败：%v", err)
				}
				terminal = true
			}
		case <-timeout.C:
			t.Fatal("等待 Claude Chat stream 结束超时")
		}
	}
	if !terminal {
		t.Fatal("Claude Chat stream 未观察到 message_stop/EOF")
	}
}

func newI027ClaudeChatStreamProvider(t *testing.T, server *httptest.Server, responseWire string) (*claude.ClaudeProvider, *gin.Context, *types.ChatCompletionRequest, *atomic.Int32, *httptest.ResponseRecorder) {
	t.Helper()
	var requests atomic.Int32
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responseWire)
	})
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-i027","stream":true}`))
	proxy := ""
	provider := claude.CreateClaudeProvider(&model.Channel{Key: "i027-key", Proxy: &proxy}, server.URL)
	provider.SetContext(ctx)
	provider.SetUsage(&types.Usage{})
	effort := "high"
	request := &types.ChatCompletionRequest{
		Model:               "claude-i027",
		Stream:              true,
		MaxCompletionTokens: 1024,
		ReasoningEffort:     &effort,
		Messages:            []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hello"}},
	}
	return provider, ctx, request, &requests, recorder
}

func TestI027ClaudeThinkingThenTextReachesResponsesCompleted(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	wire := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_i027\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-i027\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"plan\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	provider, ctx, request, requests, recorder := newI027ClaudeChatStreamProvider(t, server, wire)
	stream, apiErr := provider.CreateChatCompletionStream(request)
	if apiErr != nil {
		t.Fatalf("创建 Claude Chat stream 失败：%+v", apiErr)
	}
	store := false
	converter := newTestResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: request.Model, Stream: true, Store: &store}, provider.GetUsage())
	collectI027ClaudeChatIntoResponses(t, stream, converter)
	if requests.Load() != 1 {
		t.Fatalf("上游请求次数=%d，期望1次", requests.Load())
	}
	events := parseSSEEvents(t, recorder.Body.String())
	completed := mustUnmarshalEvent(t, events, "response.completed")
	if completed.Response == nil || completed.Response.Status != types.ResponseStatusCompleted {
		t.Fatalf("thinking/text stream 未正常完成：%+v", completed)
	}
	if len(completed.Response.Output) != 2 || completed.Response.Output[0].Type != types.InputTypeReasoning || completed.Response.Output[1].Type != types.InputTypeMessage {
		t.Fatalf("Responses output 未按 reasoning→message 生成：%+v", completed.Response.Output)
	}
	if len(completed.Response.Output[0].Summary) != 1 || completed.Response.Output[0].Summary[0].Text != "plan" {
		t.Fatalf("reasoning summary 丢失：%+v", completed.Response.Output[0])
	}
	rawContent, err := json.Marshal(completed.Response.Output[1].Content)
	if err != nil {
		t.Fatalf("正文内容无法序列化：%v", err)
	}
	var text []types.ContentResponses
	if err := json.Unmarshal(rawContent, &text); err != nil || len(text) != 1 || text[0].Text != "hello world" {
		t.Fatalf("正文未完整拼接：%#v", completed.Response.Output[1].Content)
	}
}

func TestI027ClaudePlainTextStreamReachesResponsesCompleted(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	wire := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-i027\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"plain\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	provider, ctx, request, _, recorder := newI027ClaudeChatStreamProvider(t, server, wire)
	stream, apiErr := provider.CreateChatCompletionStream(request)
	if apiErr != nil {
		t.Fatalf("创建 Claude plain stream 失败：%+v", apiErr)
	}
	store := false
	converter := newTestResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: request.Model, Stream: true, Store: &store}, provider.GetUsage())
	collectI027ClaudeChatIntoResponses(t, stream, converter)
	completed := mustUnmarshalEvent(t, parseSSEEvents(t, recorder.Body.String()), "response.completed")
	if completed.Response == nil || completed.Response.Status != types.ResponseStatusCompleted || len(completed.Response.Output) != 1 || completed.Response.Output[0].Type != types.InputTypeMessage {
		t.Fatalf("普通 Claude 流未形成单 message completion：%+v", completed.Response)
	}
}

func TestI027ClaudeInterleavedTextAndToolsReachResponsesCompleted(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	wire := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_i027_tools\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-i027\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"before\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_one\",\"name\":\"lookup\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"1}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"text_delta\",\"text\":\"middle\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":3,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_two\",\"name\":\"search\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":3,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":3}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	provider, ctx, request, requests, recorder := newI027ClaudeChatStreamProvider(t, server, wire)
	stream, apiErr := provider.CreateChatCompletionStream(request)
	if apiErr != nil {
		t.Fatalf("创建 Claude interleaved stream 失败：%+v", apiErr)
	}
	store := false
	converter := newTestResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: request.Model, Stream: true, Store: &store}, provider.GetUsage())
	collectI027ClaudeChatIntoResponses(t, stream, converter)
	if requests.Load() != 1 {
		t.Fatalf("上游请求次数=%d，期望1次", requests.Load())
	}
	completed := mustUnmarshalEvent(t, parseSSEEvents(t, recorder.Body.String()), "response.completed")
	if completed.Response == nil || completed.Response.Status != types.ResponseStatusCompleted {
		t.Fatalf("交错 text/tool 流未正常完成：%+v", completed)
	}
	final := converter.FinalResponse()
	if final == nil || len(final.Output) != 4 {
		t.Fatalf("交错 text/tool 输出项数量错误：%+v", final)
	}
	wantTypes := []string{types.InputTypeMessage, types.InputTypeFunctionCall, types.InputTypeMessage, types.InputTypeFunctionCall}
	for i, output := range final.Output {
		if output.Type != wantTypes[i] || output.Status != types.ResponseStatusCompleted {
			t.Fatalf("输出项%d类型/状态错误：%+v", i, output)
		}
	}
	firstContent, err := json.Marshal(final.Output[0].Content)
	if err != nil || !strings.Contains(string(firstContent), `"text":"before"`) {
		t.Fatalf("第一个文本块丢失：%s，err=%v", firstContent, err)
	}
	if final.Output[1].CallID != "call_one" || final.Output[1].Name != "lookup" || final.Output[1].Arguments == nil || *final.Output[1].Arguments != `{"a":1}` {
		t.Fatalf("第一个工具调用归属或参数错误：%+v", final.Output[1])
	}
	secondContent, err := json.Marshal(final.Output[2].Content)
	if err != nil || !strings.Contains(string(secondContent), `"text":"middle"`) {
		t.Fatalf("第二个文本块丢失：%s，err=%v", secondContent, err)
	}
	if final.Output[3].CallID != "call_two" || final.Output[3].Name != "search" || final.Output[3].Arguments == nil || *final.Output[3].Arguments != `{"q":"x"}` {
		t.Fatalf("第二个工具调用归属或参数错误：%+v", final.Output[3])
	}
}

func TestI027ClaudeAdjacentToolsReachResponsesCompleted(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	wire := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_i027_adjacent_tools\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-i027\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_empty\",\"name\":\"lookup\",\"input\":{}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_next\",\"name\":\"search\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	provider, ctx, request, requests, recorder := newI027ClaudeChatStreamProvider(t, server, wire)
	stream, apiErr := provider.CreateChatCompletionStream(request)
	if apiErr != nil {
		t.Fatalf("创建 Claude adjacent tools stream 失败：%+v", apiErr)
	}
	store := false
	converter := newTestResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: request.Model, Stream: true, Store: &store}, provider.GetUsage())
	collectI027ClaudeChatIntoResponses(t, stream, converter)
	if requests.Load() != 1 {
		t.Fatalf("上游请求次数=%d，期望1次", requests.Load())
	}
	completed := mustUnmarshalEvent(t, parseSSEEvents(t, recorder.Body.String()), "response.completed")
	if completed.Response == nil || completed.Response.Status != types.ResponseStatusCompleted {
		t.Fatalf("相邻 tools 流未正常完成：%+v", completed)
	}
	final := converter.FinalResponse()
	if final == nil || len(final.Output) != 2 {
		t.Fatalf("相邻 tools 输出项数量错误：%+v", final)
	}
	first, second := final.Output[0], final.Output[1]
	if first.Type != types.InputTypeFunctionCall || first.Status != types.ResponseStatusCompleted || first.CallID != "call_empty" || first.Name != "lookup" || first.Arguments == nil || *first.Arguments != "{}" {
		t.Fatalf("首个空参数工具调用错误：%+v", first)
	}
	if second.Type != types.InputTypeFunctionCall || second.Status != types.ResponseStatusCompleted || second.CallID != "call_next" || second.Name != "search" || second.Arguments == nil || *second.Arguments != `{"q":"x"}` {
		t.Fatalf("第二个工具调用错误：%+v", second)
	}
}

func TestI027ResponsesConverterStillRejectsTrueMultipleChoices(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	converter := newTestResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: "claude-i027"}, &types.Usage{})
	err := converter.ProcessStreamData(`{"id":"chatcmpl_i027","choices":[{"index":0,"delta":{"content":"one"}},{"index":1,"delta":{"content":"two"}}]}`)
	if err == nil || !strings.Contains(err.Error(), "multiple choices") {
		t.Fatalf("真正多 choice 未被 Responses converter 拒绝：%v", err)
	}
	if strings.Contains(recorder.Body.String(), "response.completed") || converter.FinalResponse() != nil {
		t.Fatalf("真正多 choice 不应生成成功终态：%q", recorder.Body.String())
	}
}
