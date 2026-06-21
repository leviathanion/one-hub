package responsesws

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common/requester"
	"one-api/types"
)

type bridgeTestOpener struct {
	stream         requester.StreamReaderInterface[string]
	err            *types.OpenAIErrorWithStatusCode
	prepareErr     error
	panic          bool
	observeDefault func(string)
}

func (o bridgeTestOpener) OpenBridgeStream(ctx context.Context, req BridgeStreamRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode, error) {
	if o.panic {
		panic("bridge open panic")
	}
	if o.observeDefault != nil {
		o.observeDefault(req.DefaultPreviousResponseID)
	}
	return o.stream, o.err, o.prepareErr
}

type bridgeTestStream struct {
	dataCh     chan string
	errCh      chan error
	closed     chan struct{}
	closeOnce  sync.Once
	panic      bool
	closeErr   error
	afterClose func()
}

func newBridgeTestStream() *bridgeTestStream {
	return &bridgeTestStream{
		dataCh: make(chan string, 4),
		errCh:  make(chan error, 4),
		closed: make(chan struct{}),
	}
}

func (s *bridgeTestStream) Recv() (<-chan string, <-chan error) {
	if s.panic {
		panic("bridge stream panic")
	}
	return s.dataCh, s.errCh
}

func (s *bridgeTestStream) Close() {
	s.closeOnce.Do(func() {
		if s.closeErr != nil {
			s.errCh <- s.closeErr
		}
		if s.afterClose != nil {
			s.afterClose()
		}
		close(s.closed)
	})
}

type bridgeCountingCloseStream struct {
	dataCh      chan string
	errCh       chan error
	recvStarted chan struct{}
	closeCh     chan struct{}
	closeErr    error
	recvOnce    sync.Once
	mu          sync.Mutex
	closes      int
}

func newBridgeCountingCloseStream(closeErr error) *bridgeCountingCloseStream {
	return &bridgeCountingCloseStream{
		dataCh:      make(chan string, 4),
		errCh:       make(chan error, 4),
		recvStarted: make(chan struct{}),
		closeCh:     make(chan struct{}, 8),
		closeErr:    closeErr,
	}
}

func (s *bridgeCountingCloseStream) Recv() (<-chan string, <-chan error) {
	s.recvOnce.Do(func() {
		close(s.recvStarted)
	})
	return s.dataCh, s.errCh
}

func (s *bridgeCountingCloseStream) Close() {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
	if s.closeErr != nil {
		select {
		case s.errCh <- s.closeErr:
		default:
		}
	}
	select {
	case s.closeCh <- struct{}{}:
	default:
	}
}

func (s *bridgeCountingCloseStream) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

func waitBridgeCountingStreamRecv(t *testing.T, stream *bridgeCountingCloseStream) {
	t.Helper()
	select {
	case <-stream.recvStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bridge test stream Recv")
	}
}

func waitBridgeCloseCountAtLeast(t *testing.T, stream *bridgeCountingCloseStream, want int) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		if got := stream.closeCount(); got >= want {
			return
		}
		select {
		case <-stream.closeCh:
		case <-deadline:
			t.Fatalf("timed out waiting for bridge stream close count >= %d, got %d", want, stream.closeCount())
		}
	}
}

type bridgeBlockingOpener struct {
	ctxCh chan context.Context
}

func (o bridgeBlockingOpener) OpenBridgeStream(ctx context.Context, _ BridgeStreamRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode, error) {
	o.ctxCh <- ctx
	<-ctx.Done()
	return nil, nil, nil
}

type bridgeBlockingDefaultOpener struct {
	ctxCh            chan context.Context
	previousDefaults chan string
}

func (o bridgeBlockingDefaultOpener) OpenBridgeStream(ctx context.Context, req BridgeStreamRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode, error) {
	o.previousDefaults <- req.DefaultPreviousResponseID
	o.ctxCh <- ctx
	<-ctx.Done()
	return nil, nil, nil
}

type bridgeNonCooperativeBlockingOpener struct {
	started chan struct{}
}

func (o bridgeNonCooperativeBlockingOpener) OpenBridgeStream(context.Context, BridgeStreamRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode, error) {
	close(o.started)
	select {}
}

type bridgeLateStreamOpener struct {
	started chan struct{}
	release chan struct{}
	stream  *bridgeTestStream
}

func (o bridgeLateStreamOpener) OpenBridgeStream(context.Context, BridgeStreamRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode, error) {
	close(o.started)
	<-o.release
	return o.stream, nil, nil
}

func TestBridgeSessionOpenSuccessReturnsAttemptedAndStreamsProviderEvents(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected bridge stream open to be attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamOpened || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProvider {
		t.Fatalf("expected bridge_stream_opened provider evidence, got %+v", event)
	}

	stream.dataCh <- `data: {"type":"response.completed","event_id":"evt_1","response":{"id":"resp_1","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`
	event, err = session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv provider stream event: %v", err)
	}
	if event.Frame == nil || string(event.Frame.Payload()) != `{"type":"response.completed","event_id":"evt_1","response":{"id":"resp_1","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}` || event.DetailOrigin != RecvDetailOriginProviderStream || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProvider {
		t.Fatalf("expected provider_stream text frame, got %+v", event)
	}
	if event.Usage == nil || event.Usage.InputTokens != 2 || event.Usage.OutputTokens != 3 || event.Usage.TotalTokens != 5 || event.Usage.ResponseID != "resp_1" || event.Usage.ProviderEventID != "evt_1" {
		t.Fatalf("expected provider_stream usage event, got %+v", event.Usage)
	}
}

func TestBridgeSessionIgnoresSSEControlLines(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected bridge stream open to be attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	stream.dataCh <- "event: response.created\nid: evt_ignored\nretry: 1000\n"
	select {
	case event := <-session.recvCh:
		t.Fatalf("expected SSE control lines to be ignored, got %+v", event)
	case <-time.After(50 * time.Millisecond):
	}

	stream.dataCh <- `data: {"type":"response.completed","response":{"id":"resp_after_control","status":"completed"}}`
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv provider stream event: %v", err)
	}
	if event.Frame == nil || !strings.Contains(string(event.Frame.Payload()), "resp_after_control") {
		t.Fatalf("expected provider stream frame after ignored control lines, got %+v", event)
	}
}

func TestBridgeSessionAssemblesMultilineSSEData(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})
	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected bridge stream open to be attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	stream.dataCh <- "event: response.completed\r\n"
	stream.dataCh <- "data: {\"type\":\"response.completed\",\r\n"
	stream.dataCh <- "data: \"response\":{\"id\":\"resp_multiline\",\"status\":\"completed\"}}\r\n"
	select {
	case event := <-session.recvCh:
		t.Fatalf("expected multiline SSE event to wait for blank line, got %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
	stream.dataCh <- "\r\n"

	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv assembled stream event: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginProviderStream || event.Frame == nil {
		t.Fatalf("expected provider stream event, got %+v", event)
	}
	if !strings.Contains(string(event.Frame.Payload()), "resp_multiline") {
		t.Fatalf("expected assembled payload to contain response id, got %q", string(event.Frame.Payload()))
	}
}

func TestBridgeSessionSSEEventTooLargeClosesAsBridgeStreamError(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{MaxStreamEventBytes: 16})
	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected bridge stream open to be attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	stream.dataCh <- `data: {"type":"response.completed","response":{"id":"resp_too_large"}}`
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv oversized stream event: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamError || !errors.Is(event.Err, requester.ErrStreamLineTooLarge) {
		t.Fatalf("expected oversized SSE to become bridge stream error, got %+v", event)
	}
}

func TestBridgeSSEAssemblerDoesNotCarryNonDataBytesIntoNextEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "raw passthrough", raw: strings.Repeat("x", 128)},
		{name: "comment", raw: ": " + strings.Repeat("x", 128) + "\n"},
		{name: "control", raw: "event: response.completed\nid: " + strings.Repeat("x", 128) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assembler := newBridgeSSEAssembler(64)
			if _, err := assembler.Consume(tc.raw); err != nil {
				t.Fatalf("expected non-data input not to fail size limit, got %v", err)
			}
			events, err := assembler.Consume("data: {\"type\":\"response.completed\"}\n\n")
			if err != nil {
				t.Fatalf("expected later small data event not to inherit prior bytes, got %v", err)
			}
			if len(events) != 1 || !strings.Contains(events[0], "response.completed") {
				t.Fatalf("expected one assembled data event, got %#v", events)
			}
		})
	}
}

func TestBridgeSessionMalformedSSEDataClosesAsBridgeStreamError(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected bridge stream open to be attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	stream.dataCh <- `data: not-json`
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv malformed stream data: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamError || !errors.Is(event.Err, ErrInvalidBridgeStreamPayload) || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal {
		t.Fatalf("expected malformed SSE data to become bridge_stream_error, got %+v", event)
	}
}

func TestBridgeSessionSchemaInvalidSSEDataClosesAsBridgeStreamError(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected bridge stream open to be attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	stream.dataCh <- `data: {"foo":1}`
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv schema-invalid stream data: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamError || !errors.Is(event.Err, ErrInvalidBridgeStreamPayload) || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal || event.Frame != nil {
		t.Fatalf("expected schema-invalid SSE data to become bridge_stream_error, got %+v", event)
	}
}

func TestBridgeSessionKnownTerminalBadResponseShapeClosesAsBridgeStreamError(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected bridge stream open to be attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	stream.dataCh <- `data: {"type":"response.completed","response":"opaque"}`
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv bad terminal stream data: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamError || !errors.Is(event.Err, ErrInvalidBridgeStreamPayload) || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal || event.Frame != nil {
		t.Fatalf("expected bad terminal SSE data to become bridge_stream_error, got %+v", event)
	}
}

func TestBridgeSessionControlCancelCancelsOpeningStream(t *testing.T) {
	opener := bridgeBlockingOpener{ctxCh: make(chan context.Context, 1)}
	session := NewBridgeSession(opener, BridgeSessionOptions{})
	resultCh := make(chan ResponsesWSTransportSendResult, 1)
	sendCtx := context.Background()
	attemptID := "attempt-opening-cancel"
	go func() {
		resultCh <- session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	}()

	var openCtx context.Context
	select {
	case openCtx = <-opener.ctxCh:
	case <-time.After(time.Second):
		t.Fatal("expected bridge opener to be called")
	}

	cancelResult := session.SendControl(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if cancelResult.Status != ResponsesWSTransportSendAttempted || cancelResult.Err != nil {
		t.Fatalf("expected opening cancel control to be attempted, got %+v", cancelResult)
	}
	select {
	case <-openCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected opening context to be canceled")
	}

	select {
	case createResult := <-resultCh:
		if createResult.Status != ResponsesWSTransportSendAmbiguous || !errors.Is(createResult.Err, ErrBridgeOpenCancelled) {
			t.Fatalf("expected canceled opening create to be ambiguous, got %+v", createResult)
		}
	case <-time.After(time.Second):
		t.Fatal("expected opening create send to return after cancel")
	}
}

func TestBridgeSessionOpenTimeoutReturnsAmbiguousAndCancelsOpeningContext(t *testing.T) {
	opener := bridgeBlockingOpener{ctxCh: make(chan context.Context, 1)}
	session := NewBridgeSession(opener, BridgeSessionOptions{OpenTimeout: 20 * time.Millisecond})
	resultCh := make(chan ResponsesWSTransportSendResult, 1)
	go func() {
		resultCh <- session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-open-timeout", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	}()

	var openCtx context.Context
	select {
	case openCtx = <-opener.ctxCh:
	case <-time.After(time.Second):
		t.Fatal("expected bridge opener to be called")
	}

	select {
	case result := <-resultCh:
		if result.Status != ResponsesWSTransportSendAmbiguous {
			t.Fatalf("expected open timeout to be ambiguous, got %+v", result)
		}
		var apiErr *types.OpenAIErrorWithStatusCode
		if !errors.As(result.Err, &apiErr) || !apiErr.LocalError || apiErr.StatusCode != http.StatusGatewayTimeout {
			t.Fatalf("expected local 504 open timeout error, got %+v", result.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected opening create send to return after timeout")
	}

	select {
	case <-openCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected opening context to be canceled")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv open timeout event: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamError || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal {
		t.Fatalf("expected local bridge stream error event, got %+v", event)
	}
	payload := string(ClientPayloadFromError(event.Err))
	if !strings.Contains(payload, `"status":504`) || !strings.Contains(payload, `bridge stream opening timed out`) {
		t.Fatalf("expected safe timeout client payload, got %s", payload)
	}
}

func TestBridgeSessionOpenTimeoutDoesNotAddCleanupWaiterForNonCooperativeOpener(t *testing.T) {
	before := runtime.NumGoroutine()
	opener := bridgeNonCooperativeBlockingOpener{started: make(chan struct{})}
	session := NewBridgeSession(opener, BridgeSessionOptions{OpenTimeout: 20 * time.Millisecond})
	resultCh := make(chan ResponsesWSTransportSendResult, 1)
	go func() {
		resultCh <- session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-non-cooperative-timeout", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	}()

	select {
	case <-opener.started:
	case <-time.After(time.Second):
		t.Fatal("expected bridge opener to be called")
	}
	select {
	case result := <-resultCh:
		if result.Status != ResponsesWSTransportSendAmbiguous {
			t.Fatalf("expected open timeout to be ambiguous, got %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("expected opening create send to return after timeout")
	}

	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	if extra := after - before; extra > 1 {
		t.Fatalf("expected timeout path not to add a permanent cleanup waiter, goroutines before=%d after=%d extra=%d", before, after, extra)
	}
}

func TestBridgeSessionOpenTimeoutClosesLateReturnedStream(t *testing.T) {
	stream := newBridgeTestStream()
	opener := bridgeLateStreamOpener{
		started: make(chan struct{}),
		release: make(chan struct{}),
		stream:  stream,
	}
	session := NewBridgeSession(opener, BridgeSessionOptions{OpenTimeout: 20 * time.Millisecond})
	resultCh := make(chan ResponsesWSTransportSendResult, 1)
	go func() {
		resultCh <- session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-late-stream-timeout", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	}()

	select {
	case <-opener.started:
	case <-time.After(time.Second):
		t.Fatal("expected bridge opener to be called")
	}
	select {
	case result := <-resultCh:
		if result.Status != ResponsesWSTransportSendAmbiguous {
			t.Fatalf("expected open timeout to be ambiguous, got %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("expected opening create send to return after timeout")
	}

	close(opener.release)
	select {
	case <-stream.closed:
	case <-time.After(time.Second):
		t.Fatal("expected late stream returned after open timeout to be closed")
	}
}

func TestBridgeSessionOpenTimeoutResultChannelIsUnbuffered(t *testing.T) {
	source, err := os.ReadFile("bridge_session.go")
	if err != nil {
		t.Fatalf("read bridge session source: %v", err)
	}
	text := string(source)
	if !strings.Contains(text, "resultCh := make(chan bridgeOpenResult)") {
		t.Fatal("open timeout result channel must stay unbuffered so late streams are closed after timeout")
	}
	if strings.Contains(text, "resultCh := make(chan bridgeOpenResult,") {
		t.Fatal("open timeout result channel must not be buffered")
	}
}

func TestBridgeSessionOpenTimeoutDisabledKeepsOpeningCancelable(t *testing.T) {
	opener := bridgeBlockingOpener{ctxCh: make(chan context.Context, 1)}
	session := NewBridgeSession(opener, BridgeSessionOptions{OpenTimeout: 0})
	resultCh := make(chan ResponsesWSTransportSendResult, 1)
	sendCtx := context.Background()
	attemptID := "attempt-open-timeout-disabled"
	go func() {
		resultCh <- session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	}()

	select {
	case <-opener.ctxCh:
	case <-time.After(time.Second):
		t.Fatal("expected bridge opener to be called")
	}
	select {
	case result := <-resultCh:
		t.Fatalf("expected disabled open timeout not to complete without cancel, got %+v", result)
	case <-time.After(40 * time.Millisecond):
	}

	cancelResult := session.SendControl(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if cancelResult.Status != ResponsesWSTransportSendAttempted || cancelResult.Err != nil {
		t.Fatalf("expected opening cancel control to be attempted, got %+v", cancelResult)
	}
	select {
	case result := <-resultCh:
		if result.Status != ResponsesWSTransportSendAmbiguous || !errors.Is(result.Err, ErrBridgeOpenCancelled) {
			t.Fatalf("expected canceled opening create to be ambiguous, got %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("expected opening create send to return after cancel")
	}
}

func TestBridgeSessionOpenTimeoutDoesNotCancelOpenedStream(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{OpenTimeout: 10 * time.Millisecond})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected bridge stream open to be attempted, got %+v", result)
	}
	time.Sleep(30 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamOpened {
		t.Fatalf("expected stream opened event, got %+v", event)
	}

	stream.dataCh <- `data: {"type":"response.completed","response":{"id":"resp_after_open_timeout","status":"completed"}}`
	event, err = session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv provider stream event after open timeout: %v", err)
	}
	if event.Frame == nil || !strings.Contains(string(event.Frame.Payload()), "resp_after_open_timeout") {
		t.Fatalf("expected opened stream to continue after open timeout window, got %+v", event)
	}
}

func TestBridgeSessionProviderOpenErrorReturnsRejectedBeforeStreamAfterEventEnqueued(t *testing.T) {
	providerErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "rate_limit", Type: "provider_error", Message: "provider busy"},
		StatusCode:  http.StatusTooManyRequests,
	}
	session := NewBridgeSession(bridgeTestOpener{err: providerErr}, BridgeSessionOptions{})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendRejectedBeforeStream || result.Err != nil {
		t.Fatalf("expected provider rejection before stream after event enqueue, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv provider open error: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeOpenProviderError || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProvider {
		t.Fatalf("expected bridge_open_provider_error provider event, got %+v", event)
	}
	var bridgeErr *BridgeOpenProviderError
	if !errors.As(event.Err, &bridgeErr) || ClientPayloadFromError(event.Err) == nil {
		t.Fatalf("expected bridge_open_provider_error client payload, got %+v", event)
	}
}

func TestBridgeSessionPrepareErrorIsNotProviderRejectionEvidence(t *testing.T) {
	prepareErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "custom_parameter_error", Type: "one_hub_error", Message: "invalid custom parameter"},
		StatusCode:  http.StatusInternalServerError,
	}
	session := NewBridgeSession(bridgeTestOpener{prepareErr: prepareErr}, BridgeSessionOptions{})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, prepareErr) {
		t.Fatalf("expected pre-send prepare error to be not_attempted, got %+v", result)
	}
	select {
	case event := <-session.recvCh:
		t.Fatalf("expected pre-send prepare error not to enqueue provider evidence, got %+v", event)
	default:
	}
}

func TestBridgeSessionLocalOpenErrorAfterRequestIsAmbiguous(t *testing.T) {
	localErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "http_request_failed", Type: "one_hub_error", Message: "dial failed"},
		StatusCode:  http.StatusInternalServerError,
		LocalError:  true,
	}
	session := NewBridgeSession(bridgeTestOpener{err: localErr}, BridgeSessionOptions{})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAmbiguous || result.Err == nil {
		t.Fatalf("expected local open error after request to be ambiguous, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv local open error: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamError || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal {
		t.Fatalf("expected bridge_stream_error proxy-local event, got %+v", event)
	}
}

func TestBridgeSessionMarkedHTTPTransportOpenErrorIsAmbiguous(t *testing.T) {
	transportErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "http_request_failed", Type: "one_hub_error", Message: "dial failed"},
		StatusCode:  http.StatusInternalServerError,
	}
	session := NewBridgeSession(bridgeTestOpener{err: MarkHTTPBridgeTransportError(transportErr)}, BridgeSessionOptions{})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAmbiguous || result.Err == nil {
		t.Fatalf("expected marked transport open error to be ambiguous, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv marked transport open error: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamError || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal {
		t.Fatalf("expected bridge_stream_error proxy-local event, got %+v", event)
	}
}

func TestBridgeSessionProviderOpenErrorQueueFullIsAmbiguous(t *testing.T) {
	providerErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "rate_limit", Type: "provider_error", Message: "provider busy"},
		StatusCode:  http.StatusTooManyRequests,
	}
	session := NewBridgeSession(bridgeTestOpener{err: providerErr}, BridgeSessionOptions{RecvQueueSize: 1})
	session.recvCh <- UpstreamEvent{DetailOrigin: RecvDetailOriginProxyLocal}

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAmbiguous || !errors.Is(result.Err, ErrNativeQueueFull) {
		t.Fatalf("expected provider rejection enqueue failure to be ambiguous, got %+v", result)
	}
}

func TestBridgeSessionNotAttemptedSendResults(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := session.SendClientWithResult(ctx, SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("expected canceled context before request to be not_attempted, got %+v", result)
	}

	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted ||
		result.Err != nil ||
		result.Reason != ResponsesWSTransportSendReasonNoActiveBridgeCancel {
		t.Fatalf("expected no-active cancel to be not_attempted, got %+v", result)
	}

	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected first create to be attempted, got %+v", result)
	}
	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, ErrBridgeBusy) {
		t.Fatalf("expected active stream busy create to be not_attempted, got %+v", result)
	}

	session.Abort("test_closed")
	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, ErrUpstreamClosed) {
		t.Fatalf("expected closed bridge session to be not_attempted, got %+v", result)
	}
}

func TestBridgeSessionContainsOpenAndPumpPanics(t *testing.T) {
	var diagnostics []BridgeDiagnostic
	options := BridgeSessionOptions{
		ProviderName: "test-provider",
		ChannelID:    42,
		Transport:    "responses-http-bridge",
		Diagnostics: func(diag BridgeDiagnostic) {
			diagnostics = append(diagnostics, diag)
		},
	}
	session := NewBridgeSession(bridgeTestOpener{panic: true}, options)
	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, ErrAdapterPanic) {
		t.Fatalf("expected bridge open panic to be not_attempted adapter panic, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv bridge open panic event: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginAdapterPanic || event.DetailPhase != RecvDetailPhasePrepareClientFrame || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal || !errors.Is(event.Err, ErrAdapterPanic) {
		t.Fatalf("expected bridge open panic to emit adapter_panic event, got %+v", event)
	}
	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, ErrUpstreamClosed) {
		t.Fatalf("expected bridge open panic to fail-close session, got %+v", result)
	}
	if len(diagnostics) != 1 || diagnostics[0].Code != "adapter_panic" || diagnostics[0].Provider != "test-provider" || diagnostics[0].ChannelID != 42 || diagnostics[0].Transport != "responses-http-bridge" || diagnostics[0].Phase != RecvDetailPhasePrepareClientFrame || diagnostics[0].StackHash == "" || diagnostics[0].PanicClass != "string" {
		t.Fatalf("expected safe open panic diagnostic metadata, got %+v", diagnostics)
	}

	stream := newBridgeTestStream()
	stream.panic = true
	diagnostics = nil
	session = NewBridgeSession(bridgeTestOpener{stream: stream}, options)
	sendCtx := context.Background()
	result = session.SendClientWithResult(sendCtx, SendRequest{AttemptID: "attempt-pump", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected bridge stream open to be attempted before pump panic, got %+v", result)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}
	event, err = session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv adapter panic event: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginAdapterPanic || event.DetailPhase != RecvDetailPhaseHandleProviderFrame || !errors.Is(event.Err, ErrAdapterPanic) {
		t.Fatalf("expected bridge pump panic to be contained, got %+v", event)
	}
	if event.AttemptID != "attempt-pump" {
		t.Fatalf("expected bridge panic event to carry attempt id, got %+v", event)
	}
	if len(diagnostics) != 1 || diagnostics[0].Provider != "test-provider" || diagnostics[0].ChannelID != 42 || diagnostics[0].Phase != RecvDetailPhaseHandleProviderFrame || diagnostics[0].StackHash == "" || diagnostics[0].PanicClass != "string" {
		t.Fatalf("expected safe pump panic diagnostic metadata, got %+v", diagnostics)
	}
}

func TestBridgeSessionCancelClosesActiveStreamAndEmitsSyntheticEvent(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})
	sendCtx := context.Background()
	attemptID := "attempt-cancel"
	result := session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected create attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	result = session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected active cancel to be attempted, got %+v", result)
	}
	select {
	case <-stream.closed:
	case <-time.After(time.Second):
		t.Fatal("expected cancel to close active bridge stream")
	}
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv synthetic cancel: %v", err)
	}
	if event.Frame == nil || event.DetailOrigin != RecvDetailOriginSyntheticBridge || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal {
		t.Fatalf("expected proxy-local synthetic cancel event, got %+v", event)
	}
	if event.AttemptID != "attempt-cancel" {
		t.Fatalf("expected synthetic cancel to carry attempt id, got %+v", event)
	}

	result = session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted ||
		result.Err != nil ||
		result.Reason != ResponsesWSTransportSendReasonNoActiveBridgeCancel {
		t.Fatalf("expected no-active cancel to be not_attempted, got %+v", result)
	}
}

func TestBridgeSessionCancelAndPumpCloseActiveStreamOnce(t *testing.T) {
	stream := newBridgeCountingCloseStream(io.ErrClosedPipe)
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})
	sendCtx := context.Background()
	attemptID := "attempt-cancel-once"
	result := session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected create attempted, got %+v", result)
	}
	waitBridgeCountingStreamRecv(t, stream)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	result = session.SendControl(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected active cancel to be attempted, got %+v", result)
	}
	waitBridgeCloseCountAtLeast(t, stream, 1)
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv synthetic cancel: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginSyntheticBridge {
		t.Fatalf("expected synthetic cancel event, got %+v", event)
	}
	event, err = session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv cancel eof: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamEOF {
		t.Fatalf("expected cancel EOF event, got %+v", event)
	}
	time.Sleep(50 * time.Millisecond)
	if got := stream.closeCount(); got != 1 {
		t.Fatalf("expected active bridge stream Close once across cancel and pump defer, got %d", got)
	}
}

func TestBridgeSessionAbortAndPumpCloseActiveStreamOnce(t *testing.T) {
	stream := newBridgeCountingCloseStream(nil)
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})
	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected create attempted, got %+v", result)
	}
	waitBridgeCountingStreamRecv(t, stream)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	session.Abort("test_abort_close_once")
	waitBridgeCloseCountAtLeast(t, stream, 1)
	time.Sleep(50 * time.Millisecond)
	if got := stream.closeCount(); got != 1 {
		t.Fatalf("expected active bridge stream Close once across abort and pump defer, got %d", got)
	}
}

func TestBridgeSessionCancelWithStaleAttemptDoesNotCloseActiveStream(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})
	sendCtx := context.Background()
	currentAttemptID := "attempt-current"
	result := session.SendClientWithResult(sendCtx, SendRequest{AttemptID: currentAttemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected create attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	result = session.SendControl(context.Background(), SendRequest{AttemptID: "attempt-stale", Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted ||
		result.Err != nil ||
		result.Reason != ResponsesWSTransportSendReasonStaleBridgeCancel {
		t.Fatalf("expected stale cancel to be a no-op, got %+v", result)
	}
	select {
	case <-stream.closed:
		t.Fatal("stale cancel must not close current active stream")
	default:
	}
	select {
	case event := <-session.recvCh:
		t.Fatalf("expected no event from stale cancel, got %+v", event)
	default:
	}

	result = session.SendControl(sendCtx, SendRequest{AttemptID: currentAttemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected matching cancel to be attempted, got %+v", result)
	}
	select {
	case <-stream.closed:
	case <-time.After(time.Second):
		t.Fatal("expected matching cancel to close active stream")
	}
}

func TestBridgeSessionCancelWithoutAttemptDoesNotCrossTurn(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		stream := newBridgeTestStream()
		session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})
		sendCtx := context.Background()
		result := session.SendClientWithResult(sendCtx, SendRequest{AttemptID: "attempt-current", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
		if result.Status != ResponsesWSTransportSendAttempted {
			t.Fatalf("expected create attempted, got %+v", result)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := session.Recv(ctx); err != nil {
			t.Fatalf("recv stream opened: %v", err)
		}

		result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
		if result.Status != ResponsesWSTransportSendNotAttempted ||
			result.Err != nil ||
			result.Reason != ResponsesWSTransportSendReasonStaleBridgeCancel {
			t.Fatalf("expected empty-attempt active cancel to be stale no-op, got %+v", result)
		}
		select {
		case <-stream.closed:
			t.Fatal("empty-attempt cancel must not close active stream")
		default:
		}
	})

	t.Run("opening", func(t *testing.T) {
		opener := bridgeBlockingOpener{ctxCh: make(chan context.Context, 1)}
		session := NewBridgeSession(opener, BridgeSessionOptions{})
		resultCh := make(chan ResponsesWSTransportSendResult, 1)
		sendCtx := context.Background()
		attemptID := "attempt-opening"
		go func() {
			resultCh <- session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
		}()

		var openCtx context.Context
		select {
		case openCtx = <-opener.ctxCh:
		case <-time.After(time.Second):
			t.Fatal("expected bridge opener to be called")
		}

		result := session.SendControl(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
		if result.Status != ResponsesWSTransportSendNotAttempted ||
			result.Err != nil ||
			result.Reason != ResponsesWSTransportSendReasonStaleBridgeCancel {
			t.Fatalf("expected empty-attempt opening cancel to be stale no-op, got %+v", result)
		}
		select {
		case <-openCtx.Done():
			t.Fatal("empty-attempt cancel must not cancel opening stream")
		default:
		}

		result = session.SendControl(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
		if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
			t.Fatalf("expected matching opening cancel to be attempted, got %+v", result)
		}
		select {
		case createResult := <-resultCh:
			if createResult.Status != ResponsesWSTransportSendAmbiguous || !errors.Is(createResult.Err, ErrBridgeOpenCancelled) {
				t.Fatalf("expected canceled opening create to be ambiguous, got %+v", createResult)
			}
		case <-time.After(time.Second):
			t.Fatal("expected opening create send to return after matching cancel")
		}
	})
}

func TestBridgeSessionCancelQueuesSyntheticBeforeCloseEOF(t *testing.T) {
	stream := newBridgeTestStream()
	stream.closeErr = io.EOF
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})
	stream.afterClose = func() {
		deadline := time.After(time.Second)
		for {
			if len(session.recvCh) > 0 {
				return
			}
			select {
			case <-deadline:
				return
			default:
				time.Sleep(time.Millisecond)
			}
		}
	}

	sendCtx := context.Background()
	attemptID := "attempt-cancel-order"
	result := session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected create attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	result = session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected active cancel to be attempted, got %+v", result)
	}
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv first cancel-side event: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginSyntheticBridge {
		t.Fatalf("expected synthetic cancel before close-induced EOF, got %+v", event)
	}
}

func TestBridgeSessionCancelQueueFullAllowsBufferedProviderTerminalToWin(t *testing.T) {
	stream := newBridgeTestStream()
	stream.afterClose = func() {
		stream.dataCh <- `data: {"type":"response.completed","event_id":"evt_terminal","response":{"id":"resp_terminal","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`
	}
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{RecvQueueSize: 1})
	sendCtx := context.Background()
	attemptID := "attempt-terminal-wins"
	result := session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected create attempted, got %+v", result)
	}

	result = session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected cancel under backpressure to be attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamOpened {
		t.Fatalf("expected bridge_stream_opened first, got %+v", event)
	}
	event, err = session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv provider terminal: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginProviderStream || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProvider || event.Frame == nil {
		t.Fatalf("expected provider terminal to win cancel race, got %+v", event)
	}
	if event.Usage == nil || event.Usage.TotalTokens != 3 || event.Usage.ResponseID != "resp_terminal" {
		t.Fatalf("expected terminal usage to be preserved, got %+v", event.Usage)
	}
	time.Sleep(defaultBridgeDrainTimeout + 20*time.Millisecond)
	select {
	case event := <-session.recvCh:
		t.Fatalf("expected no synthetic cancel after provider terminal won, got %+v", event)
	default:
	}
}

func TestBridgeSessionCancelQueueFullAbortCleansBackpressureState(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{RecvQueueSize: 1})
	sendCtx := context.Background()
	attemptID := "attempt-cancel-abort"
	result := session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected create attempted, got %+v", result)
	}

	result = session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected cancel under backpressure to be attempted, got %+v", result)
	}
	session.mu.Lock()
	if len(session.backpressureCancelStreamIDs) != 1 {
		t.Fatalf("expected backpressure cancel marker before abort, got %d", len(session.backpressureCancelStreamIDs))
	}
	session.mu.Unlock()

	session.Abort("test_abort_after_backpressure_cancel")
	deadline := time.After(time.Second)
	for {
		session.mu.Lock()
		cancelledCount := len(session.cancelledStreamIDs)
		backpressureCount := len(session.backpressureCancelStreamIDs)
		finishedCount := len(session.finishedStreamIDs)
		session.mu.Unlock()
		if cancelledCount == 0 && backpressureCount == 0 && finishedCount == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("expected stream id maps to be cleaned, cancelled=%d backpressure=%d finished=%d", cancelledCount, backpressureCount, finishedCount)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestBridgeSessionCancelQueueFullClosesActiveStreamAndPreservesActorOrder(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{RecvQueueSize: 1})
	sendCtx := context.Background()
	attemptID := "attempt-cancel-backpressure"
	result := session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected create attempted, got %+v", result)
	}
	// The synchronous bridge_stream_opened event fills recvCh. Cancel still has
	// to release the provider stream immediately; the actor observes cancel then
	// EOF only after it drains the earlier bridge evidence.
	result = session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected cancel under backpressure to be attempted, got %+v", result)
	}
	select {
	case <-stream.closed:
	case <-time.After(time.Second):
		t.Fatal("expected backpressured cancel to close active stream")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamOpened {
		t.Fatalf("expected bridge_stream_opened first, got %+v", event)
	}
	event, err = session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv synthetic cancel: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginSyntheticBridge || event.AttemptID != "attempt-cancel-backpressure" {
		t.Fatalf("expected synthetic cancel for active attempt, got %+v", event)
	}
	event, err = session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv expected cancel EOF: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginBridgeStreamEOF || event.AttemptID != "attempt-cancel-backpressure" {
		t.Fatalf("expected bridge EOF after synthetic cancel, got %+v", event)
	}

	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || result.Reason != ResponsesWSTransportSendReasonNoActiveBridgeCancel {
		t.Fatalf("expected repeated cancel to be no-active, got %+v", result)
	}
	select {
	case event := <-session.recvCh:
		t.Fatalf("expected no event from repeated no-active cancel, got %+v", event)
	default:
	}
}

func TestBridgeSessionLateProviderFrameAfterCancelKeepsOriginalAttemptID(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})
	sendCtx := context.Background()
	attemptID := "attempt-before-cancel"
	result := session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected create attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	result = session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected active cancel to be attempted, got %+v", result)
	}
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv synthetic cancel: %v", err)
	}

	stream.dataCh <- `data: {"type":"response.completed","response":{"id":"resp_late"}}`
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv late provider frame: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginProviderStream || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProvider || event.Frame == nil {
		t.Fatalf("expected late provider stream event, got %+v", event)
	}
	if event.AttemptID != "attempt-before-cancel" {
		t.Fatalf("expected late provider frame to keep original attempt id, got %+v", event)
	}
}

func TestBridgeSessionStreamEOFAndErrorUseBridgeOrigins(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		origin RecvDetailOrigin
	}{
		{name: "eof", err: io.EOF, origin: RecvDetailOriginBridgeStreamEOF},
		{name: "context canceled without local cancel", err: context.Canceled, origin: RecvDetailOriginBridgeStreamError},
		{name: "read error", err: errors.New("read failed"), origin: RecvDetailOriginBridgeStreamError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := newBridgeTestStream()
			session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})
			result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
			if result.Status != ResponsesWSTransportSendAttempted {
				t.Fatalf("expected attempted open, got %+v", result)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := session.Recv(ctx); err != nil {
				t.Fatalf("recv stream opened: %v", err)
			}
			stream.errCh <- tc.err
			event, err := session.Recv(ctx)
			if err != nil {
				t.Fatalf("recv stream terminal event: %v", err)
			}
			if event.DetailOrigin != tc.origin || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal {
				t.Fatalf("expected %s proxy-local event, got %+v", tc.origin, event)
			}
			if tc.err == io.EOF && event.Err != nil {
				t.Fatalf("expected EOF event without err, got %+v", event)
			}
		})
	}
}

func TestBridgeSessionCancelExpectedLocalReadErrorMapsToEOF(t *testing.T) {
	for _, tc := range []struct {
		name      string
		closeErr  error
		want      RecvDetailOrigin
		wantError bool
	}{
		{name: "context canceled", closeErr: context.Canceled, want: RecvDetailOriginBridgeStreamEOF},
		{name: "closed response body", closeErr: errors.New("http: read on closed response body"), want: RecvDetailOriginBridgeStreamEOF},
		{name: "line too large remains error", closeErr: requester.ErrStreamLineTooLarge, want: RecvDetailOriginBridgeStreamError, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := newBridgeTestStream()
			stream.closeErr = tc.closeErr
			session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})
			sendCtx := context.Background()
			attemptID := "attempt-local-cancel"
			result := session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
			if result.Status != ResponsesWSTransportSendAttempted {
				t.Fatalf("expected create attempted, got %+v", result)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := session.Recv(ctx); err != nil {
				t.Fatalf("recv stream opened: %v", err)
			}
			result = session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
			if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
				t.Fatalf("expected cancel attempted, got %+v", result)
			}
			synthetic, err := session.Recv(ctx)
			if err != nil {
				t.Fatalf("recv synthetic cancel: %v", err)
			}
			if synthetic.DetailOrigin != RecvDetailOriginSyntheticBridge {
				t.Fatalf("expected synthetic cancel first, got %+v", synthetic)
			}
			terminal, err := session.Recv(ctx)
			if err != nil {
				t.Fatalf("recv close terminal: %v", err)
			}
			if terminal.DetailOrigin != tc.want {
				t.Fatalf("expected terminal origin %s, got %+v", tc.want, terminal)
			}
			if tc.wantError && terminal.Err == nil {
				t.Fatalf("expected terminal error, got %+v", terminal)
			}
			if !tc.wantError && terminal.Err != nil {
				t.Fatalf("expected expected close without error, got %+v", terminal)
			}
		})
	}
}

func TestBridgeSessionCancelFallbackEmitsEOFWhenCloseErrorIsSuppressed(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})
	sendCtx := context.Background()
	attemptID := "attempt-fallback-eof"
	result := session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected create attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	result = session.SendClientWithResult(sendCtx, SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected cancel attempted, got %+v", result)
	}
	synthetic, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv synthetic cancel: %v", err)
	}
	if synthetic.DetailOrigin != RecvDetailOriginSyntheticBridge {
		t.Fatalf("expected synthetic cancel first, got %+v", synthetic)
	}
	terminal, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv fallback EOF: %v", err)
	}
	if terminal.DetailOrigin != RecvDetailOriginBridgeStreamEOF || terminal.Err != nil || terminal.AttemptID != "attempt-fallback-eof" {
		t.Fatalf("expected fallback bridge EOF for original attempt, got %+v", terminal)
	}

	stream.dataCh <- `data: {"type":"response.completed","response":{"id":"resp_late_after_fallback"}}`
	select {
	case event := <-session.recvCh:
		t.Fatalf("expected late provider frame after fallback EOF to be suppressed, got %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBridgeSessionScheduledCancelFallbackPanicEmitsDiagnostic(t *testing.T) {
	diagnostics := make(chan BridgeDiagnostic, 2)
	session := NewBridgeSession(bridgeTestOpener{}, BridgeSessionOptions{
		ProviderName: "test-provider",
		ChannelID:    42,
		Transport:    "responses-http-bridge",
		Diagnostics: func(diag BridgeDiagnostic) {
			diagnostics <- diag
		},
	})
	streamID := uint64(90210)
	session.backpressureCancelStreamIDs = map[uint64]struct{}{streamID: {}}
	close(session.recvCh)

	session.scheduleBackpressureCancelFallback(NewTextFrame([]byte(`{"type":"response.cancel"}`)), "attempt-backpressure-panic", streamID)

	diag := waitBridgeDiagnostic(t, diagnostics)
	if diag.Code != "bridge_backpressure_cancel_fallback_panic" || diag.Provider != "test-provider" || diag.ChannelID != 42 || diag.Transport != "responses-http-bridge" || diag.StackHash == "" || diag.PanicClass == "" {
		t.Fatalf("expected backpressure fallback panic diagnostic, got %+v", diag)
	}
}

func TestBridgeSessionScheduledExpectedCancelEOFPanicEmitsDiagnostic(t *testing.T) {
	diagnostics := make(chan BridgeDiagnostic, 2)
	session := NewBridgeSession(bridgeTestOpener{}, BridgeSessionOptions{
		ProviderName: "test-provider",
		ChannelID:    42,
		Transport:    "responses-http-bridge",
		Diagnostics: func(diag BridgeDiagnostic) {
			diagnostics <- diag
		},
	})
	close(session.recvCh)

	session.scheduleExpectedCancelEOF(newBridgeTestStream(), "attempt-eof-panic", 90211)

	diag := waitBridgeDiagnostic(t, diagnostics)
	if diag.Code != "bridge_expected_cancel_eof_panic" || diag.Provider != "test-provider" || diag.ChannelID != 42 || diag.Transport != "responses-http-bridge" || diag.StackHash == "" || diag.PanicClass == "" {
		t.Fatalf("expected expected-cancel EOF panic diagnostic, got %+v", diag)
	}
}

func waitBridgeDiagnostic(t *testing.T, diagnostics <-chan BridgeDiagnostic) BridgeDiagnostic {
	t.Helper()
	select {
	case diag := <-diagnostics:
		return diag
	case <-time.After(defaultBridgeDrainTimeout + time.Second):
		t.Fatal("timed out waiting for bridge diagnostic")
		return BridgeDiagnostic{}
	}
}

func TestBridgeSessionAllowsSequentialCreateAfterStreamEOF(t *testing.T) {
	first := newBridgeTestStream()
	second := newBridgeTestStream()
	opener := &bridgeSequentialOpener{streams: []*bridgeTestStream{first, second}}
	session := NewBridgeSession(opener, BridgeSessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected first create attempted, got %+v", result)
	}
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv first stream opened: %v", err)
	}
	first.errCh <- io.EOF
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv first eof: %v", err)
	}

	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected second create attempted after EOF, got %+v", result)
	}
	if opener.opens != 2 {
		t.Fatalf("expected two bridge stream opens, got %d", opener.opens)
	}
}

func TestBridgeSessionInitialPreviousResponseIDIsFirstCreateOnly(t *testing.T) {
	first := newBridgeTestStream()
	second := newBridgeTestStream()
	opener := &bridgeSequentialOpener{streams: []*bridgeTestStream{first, second}}
	session := NewBridgeSession(opener, BridgeSessionOptions{InitialPreviousResponseID: "resp_initial"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected first create attempted, got %+v", result)
	}
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv first stream opened: %v", err)
	}
	first.errCh <- io.EOF
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv first eof: %v", err)
	}

	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected second create attempted, got %+v", result)
	}
	if got, want := opener.previousDefaults, []string{"resp_initial", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected initial previous_response_id to be first-create only, got %#v want %#v", got, want)
	}
}

func TestBridgeSessionInitialPreviousResponseIDSurvivesPrepareFailure(t *testing.T) {
	stream := newBridgeTestStream()
	opener := &bridgePrepareThenStreamOpener{stream: stream}
	session := NewBridgeSession(opener, BridgeSessionOptions{InitialPreviousResponseID: "resp_initial"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, ErrInvalidFrame) {
		t.Fatalf("expected prepare failure to be not_attempted, got %+v", result)
	}

	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected retry create to be attempted, got %+v", result)
	}
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}
	if got, want := opener.previousDefaults, []string{"resp_initial", "resp_initial"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected prepare failure not to consume initial previous_response_id, got %#v want %#v", got, want)
	}
}

func TestBridgeSessionInitialPreviousResponseIDConsumedAfterLocalOpenError(t *testing.T) {
	var previousDefaults []string
	localErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "ws_request_failed", Message: "dial failed"},
		StatusCode:  http.StatusBadGateway,
		LocalError:  true,
	}
	session := NewBridgeSession(bridgeTestOpener{
		err: localErr,
		observeDefault: func(value string) {
			previousDefaults = append(previousDefaults, value)
		},
	}, BridgeSessionOptions{InitialPreviousResponseID: "resp_initial"})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAmbiguous || result.Err == nil {
		t.Fatalf("expected local open error to be ambiguous, got %+v", result)
	}
	if got, want := previousDefaults, []string{"resp_initial"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected local open error to send initial previous_response_id once, got %#v want %#v", got, want)
	}
	if got := bridgeSessionInitialPreviousResponseIDForTest(t, session); got != "" {
		t.Fatalf("expected local open error to consume initial previous_response_id, got %q", got)
	}
}

func TestBridgeSessionInitialPreviousResponseIDConsumedAfterOpeningCancel(t *testing.T) {
	opener := bridgeBlockingDefaultOpener{
		ctxCh:            make(chan context.Context, 1),
		previousDefaults: make(chan string, 1),
	}
	session := NewBridgeSession(opener, BridgeSessionOptions{InitialPreviousResponseID: "resp_initial"})
	resultCh := make(chan ResponsesWSTransportSendResult, 1)
	attemptID := "attempt-opening-cancel-previous"
	go func() {
		resultCh <- session.SendClientWithResult(context.Background(), SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	}()

	select {
	case got := <-opener.previousDefaults:
		if got != "resp_initial" {
			t.Fatalf("expected opening create to use initial previous_response_id, got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected bridge opener to receive default previous_response_id")
	}
	select {
	case <-opener.ctxCh:
	case <-time.After(time.Second):
		t.Fatal("expected bridge opener to be called")
	}

	cancelResult := session.SendControl(context.Background(), SendRequest{AttemptID: attemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if cancelResult.Status != ResponsesWSTransportSendAttempted || cancelResult.Err != nil {
		t.Fatalf("expected opening cancel control to be attempted, got %+v", cancelResult)
	}
	select {
	case result := <-resultCh:
		if result.Status != ResponsesWSTransportSendAmbiguous || !errors.Is(result.Err, ErrBridgeOpenCancelled) {
			t.Fatalf("expected canceled opening create to be ambiguous, got %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("expected opening create send to return after cancel")
	}
	if got := bridgeSessionInitialPreviousResponseIDForTest(t, session); got != "" {
		t.Fatalf("expected opening cancel to consume initial previous_response_id, got %q", got)
	}
}

func bridgeSessionInitialPreviousResponseIDForTest(t *testing.T, session *BridgeSession) string {
	t.Helper()
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.initialPreviousResponseID
}

func TestBridgeSessionClosesReturnedStreamOnPrepareFailure(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream, prepareErr: ErrInvalidFrame}, BridgeSessionOptions{})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, ErrInvalidFrame) {
		t.Fatalf("expected prepare failure to be not_attempted, got %+v", result)
	}
	select {
	case <-stream.closed:
	case <-time.After(time.Second):
		t.Fatal("expected stream returned with prepare error to be closed")
	}
}

func TestBridgeSessionClosesReturnedStreamOnOpenError(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{
		stream: stream,
		err: &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{Code: "http_request_failed", Type: "one_hub_error", Message: "dial failed"},
			StatusCode:  http.StatusInternalServerError,
			LocalError:  true,
		},
	}, BridgeSessionOptions{})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAmbiguous || result.Err == nil {
		t.Fatalf("expected local open error to be ambiguous, got %+v", result)
	}
	select {
	case <-stream.closed:
	case <-time.After(time.Second):
		t.Fatal("expected stream returned with open error to be closed")
	}
}

func TestBridgeSessionInitialPreviousResponseIDConsumedAfterProviderRejection(t *testing.T) {
	stream := newBridgeTestStream()
	providerErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "rate_limit_exceeded", Message: "rate limited"},
		StatusCode:  http.StatusTooManyRequests,
	}
	opener := &bridgeProviderRejectThenStreamOpener{stream: stream, err: providerErr}
	session := NewBridgeSession(opener, BridgeSessionOptions{InitialPreviousResponseID: "resp_initial"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendRejectedBeforeStream || result.Err != nil {
		t.Fatalf("expected provider rejection before stream, got %+v", result)
	}
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv provider rejection event: %v", err)
	}

	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected retry create to be attempted, got %+v", result)
	}
	if got, want := opener.previousDefaults, []string{"resp_initial", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected provider rejection to consume initial previous_response_id, got %#v want %#v", got, want)
	}
}

func TestBridgeSessionDynamicPreviousResponseIDOverridesInitialDefault(t *testing.T) {
	first := newBridgeTestStream()
	second := newBridgeTestStream()
	opener := &bridgeSequentialOpener{streams: []*bridgeTestStream{first, second}}
	session := NewBridgeSession(opener, BridgeSessionOptions{InitialPreviousResponseID: "resp_initial"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	sendCtx := context.Background()
	result := session.SendClientWithResult(sendCtx, SendRequest{AttemptID: "attempt-test", DefaultPreviousResponseID: "resp_dynamic", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected first create attempted, got %+v", result)
	}
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv first stream opened: %v", err)
	}
	first.errCh <- io.EOF
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv first eof: %v", err)
	}

	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected second create attempted, got %+v", result)
	}
	if got, want := opener.previousDefaults, []string{"resp_dynamic", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected dynamic default to override initial default, got %#v want %#v", got, want)
	}
}

func TestBridgeSessionCloseAndDrainUnblocksStreamReaderSendAfterClose(t *testing.T) {
	stream := &bridgeTestStream{
		dataCh: make(chan string),
		errCh:  make(chan error),
		closed: make(chan struct{}),
	}
	sent := make(chan struct{})
	stream.afterClose = func() {
		go func() {
			stream.dataCh <- `data: {"type":"response.created","response":{"id":"resp_drain"}}`
			close(sent)
			close(stream.dataCh)
			close(stream.errCh)
		}()
	}
	session := NewBridgeSession(bridgeTestOpener{}, BridgeSessionOptions{})

	session.closeAndDrain(stream, stream.dataCh, stream.errCh)

	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("expected closeAndDrain to consume a blocked stream reader send")
	}
}

func TestBridgeSessionProviderTerminalDrainsDoneMarkerAfterClose(t *testing.T) {
	stream := &bridgeTestStream{
		dataCh: make(chan string),
		errCh:  make(chan error),
		closed: make(chan struct{}),
	}
	doneSent := make(chan struct{})
	stream.afterClose = func() {
		go func() {
			stream.dataCh <- `data: [DONE]`
			close(doneSent)
			close(stream.dataCh)
			close(stream.errCh)
		}()
	}
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected create attempted, got %+v", result)
	}
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}

	go func() {
		stream.dataCh <- `data: {"type":"response.completed","response":{"id":"resp_terminal","status":"completed"}}`
	}()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv terminal frame: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginProviderStream || event.Frame == nil {
		t.Fatalf("expected provider terminal stream frame, got %+v", event)
	}

	select {
	case <-doneSent:
	case <-time.After(time.Second):
		t.Fatal("expected bridge pump to drain [DONE] after provider terminal")
	}
}

func TestBridgeSessionCancelDoesNotConsumeInitialPreviousResponseID(t *testing.T) {
	stream := newBridgeTestStream()
	opener := &bridgeSequentialOpener{streams: []*bridgeTestStream{stream}}
	session := NewBridgeSession(opener, BridgeSessionOptions{InitialPreviousResponseID: "resp_initial"})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted ||
		result.Err != nil ||
		result.Reason != ResponsesWSTransportSendReasonNoActiveBridgeCancel {
		t.Fatalf("expected no-active cancel to be not_attempted, got %+v", result)
	}
	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected create attempted, got %+v", result)
	}
	if got, want := opener.previousDefaults, []string{"resp_initial"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expected cancel not to consume initial previous_response_id, got %#v want %#v", got, want)
	}
}

func TestBridgeSessionRejectsDuplicateKeyCancelControlFrame(t *testing.T) {
	stream := newBridgeTestStream()
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{})

	result := session.SendControl(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create","type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, ErrInvalidFrame) {
		t.Fatalf("expected duplicate-key cancel control to be rejected as invalid frame, got %+v", result)
	}
}

func TestBridgeSessionSequentialCreateKeepsLateCancelledStreamAttemptIDs(t *testing.T) {
	first := newBridgeTestStream()
	second := newBridgeTestStream()
	opener := &bridgeSequentialOpener{streams: []*bridgeTestStream{first, second}}
	session := NewBridgeSession(opener, BridgeSessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	firstCtx := context.Background()
	firstAttemptID := "attempt-first"
	result := session.SendClientWithResult(firstCtx, SendRequest{AttemptID: firstAttemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected first create attempted, got %+v", result)
	}
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv first stream opened: %v", err)
	}
	result = session.SendClientWithResult(firstCtx, SendRequest{AttemptID: firstAttemptID, Frame: NewTextFrame([]byte(`{"type":"response.cancel"}`))})
	if result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected first cancel attempted, got %+v", result)
	}
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv synthetic cancel: %v", err)
	}

	secondCtx := context.Background()
	secondAttemptID := "attempt-second"
	result = session.SendClientWithResult(secondCtx, SendRequest{AttemptID: secondAttemptID, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected second create attempted, got %+v", result)
	}
	secondOpened, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv second stream opened: %v", err)
	}
	if secondOpened.DetailOrigin != RecvDetailOriginBridgeStreamOpened || secondOpened.AttemptID != "attempt-second" {
		t.Fatalf("expected second stream opened with second attempt id, got %+v", secondOpened)
	}

	first.dataCh <- `data: {"type":"response.completed","response":{"id":"resp_first_late"}}`
	late, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv first late provider frame: %v", err)
	}
	if late.DetailOrigin != RecvDetailOriginProviderStream || PayloadOriginForDetailOrigin(late.DetailOrigin) != PayloadOriginProvider || late.Frame == nil {
		t.Fatalf("expected first late provider stream event, got %+v", late)
	}
	if late.AttemptID != "attempt-first" {
		t.Fatalf("expected first late provider frame to keep first attempt id, got %+v", late)
	}
}

func TestBridgeSessionForgetStreamIDClearsFinishedState(t *testing.T) {
	session := NewBridgeSession(bridgeTestOpener{}, BridgeSessionOptions{})
	if !session.markStreamFinished(42) {
		t.Fatal("expected stream finish marker to be recorded")
	}
	session.forgetStreamID(42)
	if session.streamIsFinished(42) {
		t.Fatal("expected finished stream marker to be forgotten")
	}
}

type bridgeSequentialOpener struct {
	streams          []*bridgeTestStream
	opens            int
	previousDefaults []string
}

func (o *bridgeSequentialOpener) OpenBridgeStream(_ context.Context, req BridgeStreamRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode, error) {
	if o.opens >= len(o.streams) {
		return nil, nil, ErrUpstreamClosed
	}
	o.previousDefaults = append(o.previousDefaults, req.DefaultPreviousResponseID)
	stream := o.streams[o.opens]
	o.opens++
	return stream, nil, nil
}

type bridgePrepareThenStreamOpener struct {
	stream           *bridgeTestStream
	calls            int
	previousDefaults []string
}

func (o *bridgePrepareThenStreamOpener) OpenBridgeStream(_ context.Context, req BridgeStreamRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode, error) {
	o.previousDefaults = append(o.previousDefaults, req.DefaultPreviousResponseID)
	o.calls++
	if o.calls == 1 {
		return nil, nil, ErrInvalidFrame
	}
	return o.stream, nil, nil
}

type bridgeProviderRejectThenStreamOpener struct {
	stream           *bridgeTestStream
	err              *types.OpenAIErrorWithStatusCode
	calls            int
	previousDefaults []string
}

func (o *bridgeProviderRejectThenStreamOpener) OpenBridgeStream(_ context.Context, req BridgeStreamRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode, error) {
	o.previousDefaults = append(o.previousDefaults, req.DefaultPreviousResponseID)
	o.calls++
	if o.calls == 1 {
		return nil, o.err, nil
	}
	return o.stream, nil, nil
}

func TestBridgeSessionTerminalQueueFullAbortsInsteadOfDropping(t *testing.T) {
	cases := []struct {
		name    string
		trigger func(*bridgeTestStream)
	}{
		{
			name: "done marker",
			trigger: func(stream *bridgeTestStream) {
				stream.dataCh <- "data: [DONE]"
			},
		},
		{
			name: "malformed payload",
			trigger: func(stream *bridgeTestStream) {
				stream.dataCh <- "data: not-json"
			},
		},
		{
			name: "eof error channel",
			trigger: func(stream *bridgeTestStream) {
				stream.errCh <- io.EOF
			},
		},
		{
			name: "read error channel",
			trigger: func(stream *bridgeTestStream) {
				stream.errCh <- errors.New("read failed")
			},
		},
		{
			name: "both channels closed",
			trigger: func(stream *bridgeTestStream) {
				close(stream.dataCh)
				close(stream.errCh)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := newBridgeTestStream()
			session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{RecvQueueSize: 1})

			result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
			if result.Status != ResponsesWSTransportSendAttempted {
				t.Fatalf("expected bridge stream open to be attempted, got %+v", result)
			}
			// recvCh(容量 1) 已被同步入队的 bridge_stream_opened 占满；不读它以制造背压。

			tc.trigger(stream)

			// Abort 会关闭 active stream，以此作为 Abort 已发生的同步点。
			select {
			case <-stream.closed:
			case <-time.After(2 * time.Second):
				t.Fatal("expected terminal-queue-full to Abort and close the bridge stream")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			// 第一次 Recv 读走积压的 bridge_stream_opened。
			if _, err := session.Recv(ctx); err != nil {
				t.Fatalf("expected buffered bridge_stream_opened event, got %v", err)
			}
			// done 已被 Abort 关闭，再次 Recv 必须立即返回 ErrUpstreamClosed，而非阻塞到 ctx 超时。
			if _, err := session.Recv(ctx); !errors.Is(err, ErrUpstreamClosed) {
				t.Fatalf("expected ErrUpstreamClosed after terminal-queue-full abort, got %v", err)
			}
		})
	}
}

func TestBridgeSessionQueueFullCloseDrainsStreamReader(t *testing.T) {
	cases := []struct {
		name    string
		trigger func(*bridgeTestStream)
	}{
		{
			name: "provider data",
			trigger: func(stream *bridgeTestStream) {
				stream.dataCh <- `data: {"type":"response.created","response":{"id":"resp_queue_full"}}`
			},
		},
		{
			name: "done marker",
			trigger: func(stream *bridgeTestStream) {
				stream.dataCh <- "data: [DONE]"
			},
		},
		{
			name: "malformed payload",
			trigger: func(stream *bridgeTestStream) {
				stream.dataCh <- "data: not-json"
			},
		},
		{
			name: "stream error",
			trigger: func(stream *bridgeTestStream) {
				stream.errCh <- errors.New("read failed")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := &bridgeTestStream{
				dataCh: make(chan string),
				errCh:  make(chan error),
				closed: make(chan struct{}),
			}
			drained := make(chan struct{})
			stream.afterClose = func() {
				go func() {
					stream.dataCh <- `data: {"type":"response.output_text.delta","delta":"tail"}`
					close(drained)
					close(stream.dataCh)
					close(stream.errCh)
				}()
			}
			session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{RecvQueueSize: 1})

			result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
			if result.Status != ResponsesWSTransportSendAttempted {
				t.Fatalf("expected bridge stream open to be attempted, got %+v", result)
			}
			// recvCh is still full with bridge_stream_opened.
			triggered := make(chan struct{})
			go func() {
				tc.trigger(stream)
				close(triggered)
			}()

			select {
			case <-triggered:
			case <-time.After(time.Second):
				t.Fatal("expected bridge pump to consume queue-full trigger")
			}
			select {
			case <-stream.closed:
			case <-time.After(time.Second):
				t.Fatal("expected queue-full path to close stream")
			}
			select {
			case <-drained:
			case <-time.After(time.Second):
				t.Fatal("expected queue-full close path to drain stream reader channels")
			}
		})
	}
}

func TestBridgeSessionPumpPanicQueueFullClosesStream(t *testing.T) {
	stream := newBridgeTestStream()
	stream.panic = true
	session := NewBridgeSession(bridgeTestOpener{stream: stream}, BridgeSessionOptions{RecvQueueSize: 1})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAttempted {
		t.Fatalf("expected bridge stream open to be attempted, got %+v", result)
	}
	// recvCh is full with bridge_stream_opened, so the recovered panic event
	// cannot be enqueued and the pump must abort/close instead of leaking.
	select {
	case <-stream.closed:
	case <-time.After(time.Second):
		t.Fatal("expected panic enqueue failure to close stream")
	}
}
