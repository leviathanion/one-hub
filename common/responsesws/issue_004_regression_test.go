package responsesws

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/types"
)

func TestIssue004CompletedThenProviderClosePreservesWireOrder(t *testing.T) {
	client, server := wstest.Pair(t)
	t.Cleanup(func() {
		client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
		server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
	})
	usage := &types.UsageEvent{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}
	session := NewNativeSession(client, nativeTestAdapter{
		handle: func(_ context.Context, frame Frame) ProviderFrameResult {
			return ProviderFrameResult{
				EmitFrame: &frame,
				Usage:     usage,
				Origin:    RecvDetailOriginProviderFrame,
			}
		},
	}, NativeSessionOptions{RecvQueueSize: 2})

	completed := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp-close","status":"completed"}}`)
	if err := server.WriteMessage(wsconn.TextMessage, completed); err != nil {
		t.Fatalf("write completed provider frame: %v", err)
	}
	server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Code: wsconn.CloseNormalClosure, Reason: "provider_done"})

	first := issue004Recv(t, session)
	if first.Frame == nil || string(first.Frame.Payload()) != string(completed) {
		t.Fatalf("provider close crossed completed frame: first=%+v", first)
	}
	if first.Usage != usage {
		t.Fatalf("completed usage evidence was not kept with its frame: got=%+v want=%+v", first.Usage, usage)
	}

	second := issue004Recv(t, session)
	if second.ProviderClose == nil || second.ProviderClose.Code != int(wsconn.CloseNormalClosure) {
		t.Fatalf("expected provider close after completed frame, second=%+v", second)
	}
}

func TestIssue004CompletedThenInjectAcknowledgementPreservesWireOrder(t *testing.T) {
	session := NewNativeSession(nil, nativeTestAdapter{}, NativeSessionOptions{RecvQueueSize: 2})
	completed := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp-inject","status":"completed"}}`)
	ack := []byte(`{"type":"response.inject.created","sequence_number":2,"response_id":"resp-inject"}`)

	// 模拟 read pump 先接纳两个 provider 帧，relay actor 随后才开始接收。
	session.handleProviderMessage(context.Background(), wsconn.TextMessage, completed)
	session.handleProviderMessage(context.Background(), wsconn.TextMessage, ack)

	first := issue004Recv(t, session)
	second := issue004Recv(t, session)
	if first.Frame == nil || string(first.Frame.Payload()) != string(completed) {
		t.Fatalf("inject acknowledgement crossed completed frame: first=%+v", first)
	}
	if second.Frame == nil || string(second.Frame.Payload()) != string(ack) {
		t.Fatalf("expected inject acknowledgement after completed frame: second=%+v", second)
	}
	if firstResult, secondResult := ClassifyResponsesWSEvent(first.Frame.Payload()), ClassifyResponsesWSEvent(second.Frame.Payload()); !firstResult.HasSequenceNumber || !secondResult.HasSequenceNumber || firstResult.SequenceNumber >= secondResult.SequenceNumber {
		t.Fatalf("provider sequence regressed across completed/inject acknowledgement: first=%+v second=%+v", firstResult, secondResult)
	}
}

func TestIssue004FullQueueKeepsAcceptedCompletedBeforeAckAndClose(t *testing.T) {
	client, server := wstest.Pair(t)
	t.Cleanup(func() {
		client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
		server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
	})
	enqueued := make(chan UpstreamEvent, 4)
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{
		RecvQueueSize: 1,
		EventEnqueued: func(event UpstreamEvent) {
			if event.Frame == nil && event.ProviderClose == nil {
				return
			}
			select {
			case enqueued <- event:
			default:
			}
		},
	})
	ctxCanceled, cancel := context.WithCancel(context.Background())
	cancel()
	// 取消的这次 Recv 只启动真实 read pump，不消费 NativeSession 队列；
	// 下面的入队回调确认两个事件都已积压后，才发送会触发背压的 ack。
	if _, err := session.Recv(ctxCanceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled Recv to start the native read pump without consuming events, got %v", err)
	}
	ordinary := []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"resp-full","status":"in_progress"}}`)
	completed := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp-full","status":"completed"}}`)
	ack := []byte(`{"type":"response.inject.created","sequence_number":2,"response_id":"resp-full"}`)

	if err := server.WriteMessage(wsconn.TextMessage, ordinary); err != nil {
		t.Fatalf("write ordinary provider frame: %v", err)
	}
	if err := server.WriteMessage(wsconn.TextMessage, completed); err != nil {
		t.Fatalf("write completed provider frame: %v", err)
	}
	accepted := make([]UpstreamEvent, 0, 2)
	deadline := time.NewTimer(time.Second)
	for len(accepted) < 2 {
		select {
		case event := <-enqueued:
			accepted = append(accepted, event)
		case <-deadline.C:
			t.Fatalf("timed out waiting for native read pump to accept ordinary and completed events: got=%d", len(accepted))
		}
	}
	deadline.Stop()
	if accepted[0].Frame == nil || string(accepted[0].Frame.Payload()) != string(ordinary) ||
		accepted[1].Frame == nil || string(accepted[1].Frame.Payload()) != string(completed) {
		t.Fatalf("native read pump changed accepted event order before consumer start: %+v", accepted)
	}
	// 普通槽已满，已接纳的 completed 占用保留终态槽；后续 ack 和 close
	// 不能挤掉这两个已接纳事件，也不能让 completed 消失。
	if err := server.WriteMessage(wsconn.TextMessage, ack); err != nil {
		t.Fatalf("write inject acknowledgement that should trigger backpressure: %v", err)
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("expected native read pump to close on full queue")
	}
	server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Code: wsconn.CloseNormalClosure, Reason: "provider_done_after_backpressure"})

	first := issue004Recv(t, session)
	second := issue004Recv(t, session)
	if first.Frame == nil || string(first.Frame.Payload()) != string(ordinary) || second.Frame == nil || string(second.Frame.Payload()) != string(completed) {
		t.Fatalf("full queue changed accepted event order: first=%+v second=%+v", first, second)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); !errors.Is(err, ErrUpstreamClosed) {
		t.Fatalf("expected backpressure close after rejecting later acknowledgement, got %v", err)
	}
}

func TestIssue004AcceptedCompletedSurvivesCancellationAndRepeatedClose(t *testing.T) {
	client, _ := wstest.Pair(t)
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{})
	completed := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp-cancel","status":"completed"}}`)
	session.handleProviderMessage(context.Background(), wsconn.TextMessage, completed)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-start
		session.Abort("abort_first")
	}()
	go func() {
		defer wg.Done()
		<-start
		session.close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Reason: "close_second"})
	}()
	go func() {
		defer wg.Done()
		<-start
		session.close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "close_third"})
	}()
	close(start)
	wg.Wait()

	event, err := session.Recv(ctx)
	if err != nil || event.Frame == nil || string(event.Frame.Payload()) != string(completed) {
		t.Fatalf("accepted completed evidence was lost during cancel/close race: event=%+v err=%v", event, err)
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for repeated close race to terminate transport")
	}
	if info := client.CloseInfo(); info.Reason == "" {
		t.Fatalf("close race did not retain one close reason: %+v", info)
	}
}

func issue004Recv(t *testing.T, session *NativeSession) UpstreamEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv Responses WS event: %v", err)
	}
	return event
}
