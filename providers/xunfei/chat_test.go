package xunfei

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"one-api/common/requester"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/types"
)

type xunfeiFakeStream struct {
	dataChan chan XunfeiChatResponse
	errChan  chan error
	closed   chan struct{}
}

func (s *xunfeiFakeStream) Recv() (<-chan XunfeiChatResponse, <-chan error) {
	return s.dataChan, s.errChan
}

func (s *xunfeiFakeStream) Close() {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
}

func TestSendXunfeiWSJSONRequestWritesTextFrame(t *testing.T) {
	client, server := wstest.Pair(t)
	defer client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
	defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})

	stream, apiErr := sendXunfeiWSJSONRequest[string](client, map[string]string{"type": "test"}, func(*[]byte, chan string, chan error) {})
	if apiErr != nil {
		t.Fatalf("sendXunfeiWSJSONRequest apiErr=%v", apiErr)
	}
	if stream == nil {
		t.Fatalf("stream is nil")
	}

	mt, payload, err := server.ReadInitial(context.Background())
	if err != nil {
		t.Fatalf("ReadInitial err=%v", err)
	}
	if mt != wsconn.TextMessage {
		t.Fatalf("message type=%v, want text", mt)
	}
	var got map[string]string
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload is not json: %v", err)
	}
	if got["type"] != "test" {
		t.Fatalf("payload type=%q, want test", got["type"])
	}
}

func TestXunfeiWSConfigUsesZeroLiveness(t *testing.T) {
	cfg := xunfeiWSConfig()
	if cfg.Label != "xunfei-chat-upstream" {
		t.Fatalf("Label=%q, want xunfei-chat-upstream", cfg.Label)
	}
	if cfg.ReadLimit <= 0 {
		t.Fatalf("ReadLimit=%d, want configured positive limit", cfg.ReadLimit)
	}
	if cfg.PingInterval != 0 || cfg.PongMissTimeout != 0 || cfg.InboundActivityTimeout != nil {
		t.Fatalf("liveness config not zero: ping=%s pong=%s inbound_set=%v", cfg.PingInterval, cfg.PongMissTimeout, cfg.InboundActivityTimeout != nil)
	}
}

func TestXunfeiWSReaderProcessesFramesOutsidePumpHandle(t *testing.T) {
	client, server := wstest.Pair(t)
	defer client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
	defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})

	stream, apiErr := sendXunfeiWSJSONRequest[string](client, map[string]string{"type": "test"}, func(raw *[]byte, data chan string, errCh chan error) {
		data <- string(*raw)
		*raw = requester.StreamClosed
		errCh <- io.EOF
	})
	if apiErr != nil {
		t.Fatalf("sendXunfeiWSJSONRequest apiErr=%v", apiErr)
	}
	if _, _, err := server.ReadInitial(context.Background()); err != nil {
		t.Fatalf("server initial read err=%v", err)
	}

	dataCh, errCh := stream.Recv()
	if err := server.WriteMessage(wsconn.TextMessage, []byte("chunk")); err != nil {
		t.Fatalf("server write chunk: %v", err)
	}

	select {
	case got := <-dataCh:
		if got != "chunk" {
			t.Fatalf("data=%q, want chunk", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for data")
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("err=%v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for EOF")
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for stream completed close")
	}
	if info := client.CloseInfo(); info.Kind != wsconn.CloseKindNormal || info.Reason != "stream completed" {
		t.Fatalf("CloseInfo=%+v, want normal stream completed", info)
	}
}

func TestXunfeiWSReaderRecoversHandlerPanic(t *testing.T) {
	client, server := wstest.Pair(t)
	defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})

	stream, apiErr := sendXunfeiWSJSONRequest[string](client, map[string]string{"type": "test"}, func(*[]byte, chan string, chan error) {
		panic("secret panic detail")
	})
	if apiErr != nil {
		t.Fatalf("sendXunfeiWSJSONRequest apiErr=%v", apiErr)
	}
	if _, _, err := server.ReadInitial(context.Background()); err != nil {
		t.Fatalf("server initial read err=%v", err)
	}

	_, errCh := stream.Recv()
	if err := server.WriteMessage(wsconn.TextMessage, []byte("chunk")); err != nil {
		t.Fatalf("server write chunk: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil || err.Error() != "xunfei websocket handler failed" {
			t.Fatalf("err=%v, want generic handler error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for handler panic error")
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for client close after handler panic")
	}
	info := client.CloseInfo()
	if info.Kind != wsconn.CloseKindHandlerPanic || info.Reason != "xunfei_handler_panic" {
		t.Fatalf("CloseInfo=%+v, want handler_panic/xunfei_handler_panic", info)
	}
}

func TestConvertToChatOpenaiClosesStream(t *testing.T) {
	stream := &xunfeiFakeStream{
		dataChan: make(chan XunfeiChatResponse, 1),
		errChan:  make(chan error, 1),
		closed:   make(chan struct{}),
	}
	var upstream XunfeiChatResponse
	upstream.Payload.Choices.Text = []XunfeiChatResponseTextItem{{Content: "hello"}}
	stream.dataChan <- upstream
	stream.errChan <- io.EOF

	handler := &xunfeiHandler{Request: &types.ChatCompletionRequest{Model: "spark"}}
	response, apiErr := handler.convertToChatOpenai(stream)
	if apiErr != nil {
		t.Fatalf("convertToChatOpenai apiErr=%v", apiErr)
	}
	if response == nil || len(response.Choices) != 1 || response.Choices[0].Message.Content != "hello" {
		t.Fatalf("unexpected response: %+v", response)
	}
	select {
	case <-stream.closed:
	case <-time.After(time.Second):
		t.Fatal("expected convertToChatOpenai to close stream")
	}
}
