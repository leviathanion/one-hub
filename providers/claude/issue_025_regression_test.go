package claude

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/requester"
	"one-api/types"
)

func drainI025ClaudeStream(t *testing.T, stream requester.StreamReaderInterface[string]) {
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

func newI025NativeClaudeProviderForTest(t *testing.T, server *httptest.Server, raw string) (*ClaudeProvider, *ClaudeRequest) {
	t.Helper()
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	return newNativeClaudeProviderForTest(t, server, raw, nil)
}

func TestI025NativeClaudeUnaryPublishesProviderServiceTier(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_i025","type":"message","model":"claude-test","content":[],"usage":{"input_tokens":100,"output_tokens":20,"service_tier":"priority"}}`)
	}))
	t.Cleanup(server.Close)
	provider, request := newI025NativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":64,"messages":[]}`)
	if _, apiErr := provider.CreateClaudeChat(request); apiErr != nil {
		t.Fatalf("创建 Claude unary 请求失败: %+v", apiErr)
	}
	usage := provider.GetUsage()
	if usage.ServiceTier != "priority" || usage.PromptTokens != 100 || usage.CompletionTokens != 20 {
		t.Fatalf("Claude unary 未投影上游实际 service_tier/usage: %+v", usage)
	}
}

func TestI025NativeClaudeStreamPublishesAndMergesProviderServiceTier(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":100,\"output_tokens\":20,\"service_tier\":\"priority\"}}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"service_tier\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(server.Close)
	provider, request := newI025NativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":64,"messages":[],"stream":true}`)
	stream, apiErr := provider.CreateClaudeChatStream(request)
	if apiErr != nil {
		t.Fatalf("创建 Claude stream 失败: %+v", apiErr)
	}
	drainI025ClaudeStream(t, stream)
	usage := provider.GetUsage()
	if usage.ServiceTier != "priority" || usage.PromptTokens != 100 || usage.CompletionTokens != 20 || usage.AttributionConflict {
		t.Fatalf("Claude stream 未保留实际 service_tier/usage 或误报冲突: %+v", usage)
	}
}

func TestI025NativeClaudeStreamConflictingProviderServiceTierIsDiagnosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":100,\"output_tokens\":20,\"service_tier\":\"priority\"}}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"service_tier\":\"default\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(server.Close)
	provider, request := newI025NativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":64,"messages":[],"stream":true}`)
	stream, apiErr := provider.CreateClaudeChatStream(request)
	if apiErr != nil {
		t.Fatalf("创建 Claude stream 失败: %+v", apiErr)
	}
	drainI025ClaudeStream(t, stream)
	usage := provider.GetUsage()
	if usage.ServiceTier != "priority" || !usage.AttributionConflict || !usage.BillingDiagnostics["billing_tier_conflict"] {
		t.Fatalf("Claude stream tier 冲突未沿既有 attribution conflict 记录: %+v", usage)
	}
}

func TestI025ClaudeUsageMergeRetainsTierConflictForSharedAttribution(t *testing.T) {
	usage := decodeClaudeUsage(t, `{"input_tokens":100,"output_tokens":20,"service_tier":"priority"}`)
	ClaudeUsageMerge(usage, decodeClaudeUsage(t, `{"service_tier":"default"}`))
	converted := &types.Usage{}
	if !ClaudeUsageToOpenaiUsage(usage, converted) {
		t.Fatal("Claude usage conversion failed")
	}
	if converted.ServiceTier != "priority" || !converted.AttributionConflict || !converted.BillingDiagnostics["billing_tier_conflict"] {
		t.Fatalf("Claude usage merge did not preserve shared tier conflict: %+v", converted)
	}
}

func TestI025ClaudeSharedRelayStreamHandlersProjectProviderServiceTier(t *testing.T) {
	for _, test := range []struct {
		name      string
		prefix    string
		addEvent  bool
		startLine string
		deltaLine string
	}{
		{
			name:      "bedrock_eventstream",
			prefix:    `{"type"`,
			addEvent:  true,
			startLine: `{"type":"message_start","message":{"model":"claude-bedrock","usage":{"input_tokens":100,"output_tokens":20,"service_tier":"priority"}}}`,
			deltaLine: `{"type":"message_delta","usage":{"service_tier":"default"}}`,
		},
		{
			name:      "vertex_sse",
			prefix:    `data: {"type"`,
			startLine: `data: {"type":"message_start","message":{"model":"claude-vertex","usage":{"input_tokens":100,"output_tokens":20,"service_tier":"priority"}}}`,
			deltaLine: `data: {"type":"message_delta","usage":{"service_tier":"default"}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &ClaudeRelayStreamHandler{Usage: &types.Usage{}, Prefix: test.prefix, AddEvent: test.addEvent}
			data := make(chan string, 8)
			errors := make(chan error, 4)
			for _, line := range []string{test.startLine, test.deltaLine} {
				raw := []byte(line)
				handler.HandlerStream(&raw, data, errors)
			}
			if len(errors) != 0 {
				t.Fatalf("复用 Claude stream handler 返回错误: %v", <-errors)
			}
			usage := handler.Usage
			if usage.ServiceTier != "priority" || usage.PromptTokens != 100 || usage.CompletionTokens != 20 || !usage.AttributionConflict || !usage.BillingDiagnostics["billing_tier_conflict"] {
				t.Fatalf("Claude %s 复用入口未投影 tier/冲突: %+v", test.name, usage)
			}
		})
	}
}

func TestI025NativeClaudeRequestTierDoesNotAuthorizeActualTier(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_i025","type":"message","model":"claude-test","content":[],"usage":{"input_tokens":100,"output_tokens":20,"service_tier":"default"}}`)
	}))
	t.Cleanup(server.Close)
	provider, request := newI025NativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":64,"messages":[],"service_tier":"priority"}`)
	if _, apiErr := provider.CreateClaudeChat(request); apiErr != nil {
		t.Fatalf("创建 Claude unary 请求失败: %+v", apiErr)
	}
	if got := provider.GetUsage().ServiceTier; got != "default" {
		t.Fatalf("客户端请求 tier 不应冒充上游实际 tier: got %q", got)
	}
}

func TestI025ClaudeChatUnaryPublishesProviderServiceTier(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_i025_chat","type":"message","model":"claude-test","content":[],"usage":{"input_tokens":100,"output_tokens":20,"service_tier":"priority"}}`)
	}))
	t.Cleanup(server.Close)
	provider, _ := newI025NativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":64,"messages":[]}`)
	response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{Model: "claude-test", Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hello"}}})
	if apiErr != nil || response == nil {
		t.Fatalf("创建 Claude Chat unary 请求失败: response=%+v err=%+v", response, apiErr)
	}
	if usage := provider.GetUsage(); usage.ServiceTier != "priority" || usage.PromptTokens != 100 || usage.CompletionTokens != 20 {
		t.Fatalf("Claude Chat unary 未投影上游实际 service_tier/usage: %+v", usage)
	}
}

func TestI025ClaudeChatStreamPublishesProviderServiceTier(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":100,\"output_tokens\":20,\"service_tier\":\"priority\"}}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"service_tier\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(server.Close)
	provider, _ := newI025NativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":64,"messages":[],"stream":true}`)
	stream, apiErr := provider.CreateChatCompletionStream(&types.ChatCompletionRequest{Model: "claude-test", Stream: true, Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hello"}}})
	if apiErr != nil {
		t.Fatalf("创建 Claude Chat stream 失败: %+v", apiErr)
	}
	data, errs := stream.Recv()
	defer stream.Close()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
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
			if !errors.Is(err, io.EOF) && err != nil {
				t.Fatalf("Claude Chat stream 失败: %v", err)
			}
		case <-timeout.C:
			t.Fatal("读取 Claude Chat stream 超时")
		}
	}
	if usage := provider.GetUsage(); usage.ServiceTier != "priority" || usage.PromptTokens != 100 || usage.CompletionTokens != 20 {
		t.Fatalf("Claude Chat stream 未投影上游实际 service_tier/usage: %+v", usage)
	}
}
