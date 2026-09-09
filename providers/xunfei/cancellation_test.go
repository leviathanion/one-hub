package xunfei

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestCanceledXunfeiRequestDoesNotPrepareOrDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx)
	// No channel is needed: cancellation must precede credential/URL preparation.
	p := &XunfeiProvider{BaseProvider: base.BaseProvider{Context: c}}
	conn, _, apiErr := p.getChatRequest(&types.ChatCompletionRequest{Model: "spark"})
	if conn != nil || apiErr == nil || !strings.Contains(apiErr.Message, context.Canceled.Error()) {
		t.Fatalf("canceled request proceeded: conn=%v err=%+v", conn, apiErr)
	}
}

func TestXunfeiIdleStreamCancellationDrainsProducer(t *testing.T) {
	for _, cancelBeforeRecv := range []bool{false, true} {
		t.Run(map[bool]string{false: "reading", true: "before_recv"}[cancelBeforeRecv], func(t *testing.T) {
			client, server := wstest.Pair(t)
			defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, apiErr := sendXunfeiWSJSONRequest[string](ctx, client, map[string]string{"type": "create"}, func(*[]byte, chan string, chan error) {})
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			defer stream.CloseAndDrain()
			if _, _, err := server.ReadInitial(t.Context()); err != nil {
				t.Fatal(err)
			}
			if cancelBeforeRecv {
				cancel()
			}
			_, errs := stream.Recv()
			cancel()
			select {
			case err := <-errs:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel cause lost: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("idle upstream ignored cancellation")
			}
			select {
			case <-stream.done:
			case <-time.After(time.Second):
				t.Fatal("producer did not finish")
			}
		})
	}
}

func TestXunfeiIdleUnaryCancellationClosesConnection(t *testing.T) {
	client, server := wstest.Pair(t)
	defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, apiErr := sendXunfeiWSJSONRequest[XunfeiChatResponse](ctx, client, map[string]string{"type": "create"}, func(*[]byte, chan XunfeiChatResponse, chan error) {})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	defer stream.CloseAndDrain()
	if _, _, err := server.ReadInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := make(chan *types.OpenAIErrorWithStatusCode, 1)
	go func() {
		handler := &xunfeiHandler{Request: &types.ChatCompletionRequest{Model: "spark"}}
		_, apiErr := handler.convertToChatOpenai(ctx, stream)
		done <- apiErr
	}()
	cancel()
	select {
	case apiErr := <-done:
		if apiErr == nil || !strings.Contains(apiErr.Message, context.Canceled.Error()) {
			t.Fatalf("unary cancel cause lost: %+v", apiErr)
		}
	case <-time.After(time.Second):
		t.Fatal("idle unary did not stop")
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("unary returned before connection cleanup")
	}
}

func TestXunfeiInitialRequestFailureClosesConnection(t *testing.T) {
	for _, kind := range []string{"canceled", "marshal", "write"} {
		t.Run(kind, func(t *testing.T) {
			client, server := wstest.Pair(t)
			defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var payload any = map[string]string{"type": "create"}
			switch kind {
			case "canceled":
				cancel()
			case "marshal":
				payload = make(chan string)
			case "write":
				client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
			}
			stream, apiErr := sendXunfeiWSJSONRequest[string](ctx, client, payload, func(*[]byte, chan string, chan error) {})
			if apiErr == nil || stream != nil {
				t.Fatalf("failed request returned reader: stream=%v err=%+v", stream, apiErr)
			}
			select {
			case <-client.Done():
			case <-time.After(time.Second):
				t.Fatal("failed request leaked connection")
			}
		})
	}
}

func TestXunfeiUnrepresentableToolsFailBeforeDial(t *testing.T) {
	for _, request := range []*types.ChatCompletionRequest{
		{Tools: []*types.ChatCompletionTool{nil}},
		{Messages: []types.ChatCompletionMessage{{ToolCalls: []*types.ChatCompletionToolCalls{nil}}}},
		{Messages: []types.ChatCompletionMessage{{ToolCalls: []*types.ChatCompletionToolCalls{{Type: "function"}}}}},
		{Messages: []types.ChatCompletionMessage{{ToolCalls: []*types.ChatCompletionToolCalls{{}, {}}}}},
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/chat/completions?api-version=v1.1", nil)
		p := &XunfeiProvider{BaseProvider: base.BaseProvider{Context: c, Channel: &model.Channel{Key: "app|secret|key"}, Config: getConfig()}}
		p.Config.BaseURL = "wss://127.0.0.1:1"
		conn, _, apiErr := p.getChatRequest(request)
		if conn != nil || apiErr == nil || !apiErr.LocalError || apiErr.Code != "unsupported_capability" {
			t.Fatalf("unrepresentable tool reached dial: conn=%v err=%+v", conn, apiErr)
		}
	}
	p := &XunfeiProvider{}
	converted, err := p.convertFromChatOpenai(&types.ChatCompletionRequest{Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello", ToolCalls: []*types.ChatCompletionToolCalls{}}}})
	if err != nil || len(converted.Payload.Message.Text) != 1 || converted.Payload.Message.Text[0].Content != "hello" {
		t.Fatalf("empty tool_calls changed ordinary message: result=%+v err=%v", converted, err)
	}
}

func TestXunfeiTerminalUsageRequiresActualEvidence(t *testing.T) {
	for _, test := range []struct {
		usage string
		want  bool
	}{
		{"", false},
		{`,"usage":{"text":null}`, false},
		{`,"usage":{"text":{}}`, false},
		{`,"usage":{"text":{"prompt_tokens":1}}`, false},
		{`,"usage":{"text":{"completion_tokens":1,"total_tokens":1}}`, false},
		{`,"usage":{"text":{"prompt_tokens":3,"completion_tokens":1}}`, true},
		{`,"usage":{"text":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`, true},
	} {
		handler := &xunfeiHandler{Usage: &types.Usage{PromptTokens: 8}}
		raw := []byte(`{"payload":{"choices":{"status":2}` + test.usage + `}}`)
		finished := false
		_, err := handler.handlerData(&raw, &finished)
		if err != nil || !finished || handler.Usage.HasProviderUsage() != test.want || handler.Usage.ProviderReported != test.want {
			t.Fatalf("terminal fabricated/lost evidence: err=%v finished=%t usage=%+v", err, finished, handler.Usage)
		}
	}
}

func TestXunfeiCanceledPrefixCannotAuthorizeTerminalUsage(t *testing.T) {
	client, server := wstest.Pair(t)
	defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := &xunfeiHandler{Usage: &types.Usage{PromptTokens: 8}, Request: &types.ChatCompletionRequest{Model: "spark"}}
	stream, apiErr := sendXunfeiWSJSONRequest[string](ctx, client, map[string]string{"type": "create"}, handler.handlerStream)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	defer stream.CloseAndDrain()
	if _, _, err := server.ReadInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, _ := stream.Recv()
	if err := server.WriteMessage(wsconn.TextMessage, []byte(`{"payload":{"choices":{"status":1,"text":[{"content":"hello"}]},"usage":{"text":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-data:
	case <-time.After(time.Second):
		t.Fatal("prefix not delivered")
	}
	cancel()
	stream.CloseAndDrain()
	if handler.Usage.ProviderReported || handler.Usage.HasProviderUsage() {
		t.Fatalf("nonterminal prefix/cancel authorized billing: %+v", handler.Usage)
	}
}
