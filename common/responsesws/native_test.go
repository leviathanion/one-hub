package responsesws

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
)

type nativeTestAdapter struct {
	prepareErr error
	binary     bool
	prepare    func(context.Context, Frame) (Frame, error)
	handle     func(context.Context, Frame) ProviderFrameResult
	closeMap   func(context.Context, ProviderCloseInfo) ProviderCloseResult
}

type nativeTestContextKey string

func (a nativeTestAdapter) PrepareClientFrame(ctx context.Context, frame Frame) (Frame, error) {
	if a.prepare != nil {
		return a.prepare(ctx, frame)
	}
	if a.prepareErr != nil {
		return Frame{}, a.prepareErr
	}
	return frame, nil
}

func (a nativeTestAdapter) HandleProviderFrame(ctx context.Context, frame Frame) ProviderFrameResult {
	if a.handle != nil {
		return a.handle(ctx, frame)
	}
	return ProviderFrameResult{EmitFrame: &frame, Origin: RecvDetailOriginProviderFrame}
}

func (a nativeTestAdapter) MapProviderClose(ctx context.Context, info ProviderCloseInfo) ProviderCloseResult {
	if a.closeMap != nil {
		return a.closeMap(ctx, info)
	}
	return ProviderCloseResult{
		ProviderClose: &ProviderClose{Code: info.Code, Reason: info.Reason, Err: info.Err},
		Origin:        RecvDetailOriginNativeProviderClose,
	}
}

func (a nativeTestAdapter) SupportsBinaryProviderFrames() bool {
	return a.binary
}

func TestNativeSessionSendClientWithResultClassifiesAttemptedAndNotAttempted(t *testing.T) {
	client, _ := wstest.Pair(t)
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{})

	if result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))}); result.Status != ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected successful write to be attempted, got %+v", result)
	}

	prepareErr := errors.New("prepare failed")
	session = NewNativeSession(client, nativeTestAdapter{prepareErr: prepareErr}, NativeSessionOptions{})
	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, prepareErr) {
		t.Fatalf("expected prepare failure to be not_attempted, got %+v", result)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result = session.SendClientWithResult(ctx, SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("expected canceled context before write to be not_attempted, got %+v", result)
	}

	client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_closed"})
	<-client.Done()
	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, ErrUpstreamClosed) {
		t.Fatalf("expected closed connection before write to be not_attempted, got %+v", result)
	}
}

func TestNativeSessionSendClientWithResultClassifiesWriteErrorAfterEntryAsAmbiguous(t *testing.T) {
	client, _ := wstest.Pair(t)
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{})
	writeErr := errors.New("write entered and failed")
	session.writeMessage = func(wsconn.MessageType, []byte) error { return writeErr }

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendAmbiguous || !errors.Is(result.Err, writeErr) {
		t.Fatalf("expected write-entered failure to be ambiguous, got %+v", result)
	}
}

func TestNativeSessionRecvProviderFrameAndCloseOrigins(t *testing.T) {
	client, server := wstest.Pair(t)
	closeInfos := make(chan ProviderCloseInfo, 1)
	session := NewNativeSession(client, nativeTestAdapter{
		closeMap: func(_ context.Context, info ProviderCloseInfo) ProviderCloseResult {
			closeInfos <- info
			return ProviderCloseResult{
				ProviderClose: &ProviderClose{Code: info.Code, Reason: info.Reason, Err: info.Err},
				Origin:        RecvDetailOriginNativeProviderClose,
			}
		},
	}, NativeSessionOptions{})

	if err := server.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.created"}`)); err != nil {
		t.Fatalf("write provider frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv provider frame: %v", err)
	}
	if event.Frame == nil || string(event.Frame.Payload()) != `{"type":"response.created"}` || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProvider || event.DetailOrigin != RecvDetailOriginProviderFrame {
		t.Fatalf("unexpected provider frame event: %+v", event)
	}

	server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Code: wsconn.CloseNormalClosure, Reason: "done"})
	event, err = session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv provider close: %v", err)
	}
	if event.ProviderClose == nil || event.DetailOrigin != RecvDetailOriginNativeProviderClose || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProvider {
		t.Fatalf("unexpected provider close event: %+v", event)
	}
	select {
	case info := <-closeInfos:
		if info.Kind != ProviderCloseKindPeerClose {
			t.Fatalf("expected adapter to receive peer close kind, got %+v", info)
		}
	case <-time.After(time.Second):
		t.Fatal("expected adapter close mapping to run")
	}
}

func TestNativeSessionReadPumpUsesOptionContext(t *testing.T) {
	client, server := wstest.Pair(t)
	key := nativeTestContextKey("trace")
	baseCtx := context.WithValue(context.Background(), key, "trace-1")
	seen := make(chan any, 1)
	session := NewNativeSession(client, nativeTestAdapter{
		handle: func(ctx context.Context, frame Frame) ProviderFrameResult {
			seen <- ctx.Value(key)
			return ProviderFrameResult{EmitFrame: &frame, Origin: RecvDetailOriginProviderFrame}
		},
	}, NativeSessionOptions{Context: baseCtx})

	if err := server.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.created"}`)); err != nil {
		t.Fatalf("write provider frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv provider frame: %v", err)
	}
	select {
	case got := <-seen:
		if got != "trace-1" {
			t.Fatalf("expected read pump to pass option context value, got %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for adapter context")
	}
}

func TestNativeSessionReadPumpSurvivesCanceledOpenContext(t *testing.T) {
	client, server := wstest.Pair(t)
	openCtx, cancelOpen := context.WithCancel(context.Background())
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{Context: openCtx})

	cancelOpen()
	if err := server.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.created"}`)); err != nil {
		t.Fatalf("write provider frame after open context cancel: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv provider frame after open context cancel: %v", err)
	}
	if event.Frame == nil || string(event.Frame.Payload()) != `{"type":"response.created"}` {
		t.Fatalf("expected provider frame after open context cancel, got %+v", event)
	}
}

func TestNativeSessionRejectsBinaryProviderFrameByDefault(t *testing.T) {
	client, server := wstest.Pair(t)
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{})

	if err := server.WriteMessage(wsconn.BinaryMessage, []byte{1, 2, 3}); err != nil {
		t.Fatalf("write provider binary frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv malformed provider frame: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginProviderMalformed || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal || !errors.Is(event.Err, ErrNativeProtocol) {
		t.Fatalf("expected binary frame to become provider_malformed proxy-local event, got %+v", event)
	}
}

func TestNativeSessionAllowsAdapterDeclaredBinaryProviderFrame(t *testing.T) {
	client, server := wstest.Pair(t)
	session := NewNativeSession(client, nativeTestAdapter{
		binary: true,
		handle: func(_ context.Context, frame Frame) ProviderFrameResult {
			if frame.Kind() != FrameKindBinary {
				t.Fatalf("expected binary frame, got %d", frame.Kind())
			}
			return ProviderFrameResult{
				EmitFrame: &frame,
				Origin:    RecvDetailOriginProviderFrame,
			}
		},
	}, NativeSessionOptions{})

	if err := server.WriteMessage(wsconn.BinaryMessage, []byte{1, 2, 3}); err != nil {
		t.Fatalf("write provider binary frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv provider binary frame: %v", err)
	}
	if event.Frame == nil || event.Frame.Kind() != FrameKindBinary || string(event.Frame.Payload()) != string([]byte{1, 2, 3}) ||
		event.DetailOrigin != RecvDetailOriginProviderFrame || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProvider {
		t.Fatalf("expected adapter-declared binary provider frame, got %+v", event)
	}
}

func TestNativeSessionFiltersBootstrapWithoutDownstreamFrameOrUsage(t *testing.T) {
	client, server := wstest.Pair(t)
	handled := make(chan struct{})
	var handledOnce sync.Once
	session := NewNativeSession(client, nativeTestAdapter{
		handle: func(context.Context, Frame) ProviderFrameResult {
			handledOnce.Do(func() {
				close(handled)
			})
			return ProviderFrameResult{
				Filtered: true,
				Origin:   RecvDetailOriginProviderFrame,
			}
		},
	}, NativeSessionOptions{})
	type recvResult struct {
		event UpstreamEvent
		err   error
	}
	recvCtx, recvCancel := context.WithCancel(context.Background())
	defer recvCancel()
	recvCh := make(chan recvResult, 1)
	go func() {
		event, err := session.Recv(recvCtx)
		recvCh <- recvResult{event: event, err: err}
	}()

	if err := server.WriteMessage(wsconn.TextMessage, []byte(`{"type":"session.created"}`)); err != nil {
		t.Fatalf("write provider bootstrap frame: %v", err)
	}
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("expected adapter to handle provider bootstrap frame")
	}
	select {
	case got := <-recvCh:
		t.Fatalf("expected filtered bootstrap frame to produce no recv event, event=%+v err=%v", got.event, got.err)
	case <-time.After(50 * time.Millisecond):
	}
	recvCancel()
	select {
	case <-recvCh:
	case <-time.After(time.Second):
		t.Fatal("expected pending Recv to exit after context cancellation")
	}
}

func TestNativeSessionProviderEOFOrigin(t *testing.T) {
	session := NewNativeSession(nil, nil, NativeSessionOptions{})
	session.handleProviderClose(context.Background(), wsconn.CloseInfo{Kind: wsconn.CloseKindReadError, Err: io.EOF})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv provider eof event: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginNativeProviderEOF || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProvider {
		t.Fatalf("expected native provider EOF event, got %+v", event)
	}
}

func TestNativeSessionLocalWriteCloseDoesNotBecomeProviderClose(t *testing.T) {
	writeErr := errors.New("write failed")
	session := NewNativeSession(nil, nativeTestAdapter{
		closeMap: func(context.Context, ProviderCloseInfo) ProviderCloseResult {
			t.Fatal("local write close must not be delegated to provider close mapper")
			return ProviderCloseResult{}
		},
	}, NativeSessionOptions{})
	session.handleProviderClose(context.Background(), wsconn.CloseInfo{Kind: wsconn.CloseKindWriteError, Reason: "write_message_failed", Err: writeErr})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv local write close event: %v", err)
	}
	if event.ProviderClose != nil || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal || event.DetailOrigin != RecvDetailOriginNativeReadError || !errors.Is(event.Err, writeErr) {
		t.Fatalf("expected local write close to remain proxy-local transport error, got %+v", event)
	}
}

func TestNativeSessionRecvBackpressureClosesTransport(t *testing.T) {
	client, server := wstest.Pair(t)
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{RecvQueueSize: 1})
	session.startReadPump()

	if err := server.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.created","n":1}`)); err != nil {
		t.Fatalf("write provider frame 1: %v", err)
	}
	if err := server.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.created","n":2}`)); err != nil {
		t.Fatalf("write provider frame 2: %v", err)
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("expected native session to close client connection on recv backpressure")
	}
	info := client.CloseInfo()
	if info.Kind != wsconn.CloseKindBackpressure || !errors.Is(info.Err, ErrNativeQueueFull) {
		t.Fatalf("expected backpressure close, got %+v", info)
	}
	_ = session
}

func TestNativeSessionRecvDrainsBufferedProviderEventBeforeTerminal(t *testing.T) {
	session := NewNativeSession(nil, nativeTestAdapter{}, NativeSessionOptions{RecvQueueSize: 1})
	frame := NewTextFrame([]byte(`{"type":"response.created"}`))
	if !session.enqueue(UpstreamEvent{
		Frame:        &frame,
		DetailOrigin: RecvDetailOriginProviderFrame,
	}) {
		t.Fatal("expected provider frame to enqueue")
	}
	if !session.enqueueTerminal(UpstreamEvent{
		DetailOrigin: RecvDetailOriginNativeBackpressure,
		Err:          ErrNativeQueueFull,
	}) {
		t.Fatal("expected terminal event to enqueue")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv provider event: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginProviderFrame || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProvider || event.Frame == nil {
		t.Fatalf("expected buffered provider frame before terminal event, got %+v", event)
	}

	terminal, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv terminal event: %v", err)
	}
	if terminal.DetailOrigin != RecvDetailOriginNativeBackpressure || !errors.Is(terminal.Err, ErrNativeQueueFull) {
		t.Fatalf("expected backpressure terminal after provider frame, got %+v", terminal)
	}
}

func TestNativeSessionAbortIsIdempotentAndWakesRecv(t *testing.T) {
	client, _ := wstest.Pair(t)
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{})

	done := make(chan UpstreamEvent, 1)
	go func() {
		event, err := session.Recv(context.Background())
		if err != nil {
			t.Errorf("recv after abort: %v", err)
		}
		done <- event
	}()

	session.Abort("first")
	session.Abort("second")

	select {
	case event := <-done:
		if event.DetailOrigin != RecvDetailOriginNativeLocalAbort || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal || !errors.Is(event.Err, ErrUpstreamClosed) {
			t.Fatalf("expected Recv to wake with local abort event, got %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Recv to wake after Abort")
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for client connection to close after Abort")
	}
	if info := client.CloseInfo(); info.Kind != wsconn.CloseKindAbort || info.Reason != "first" {
		t.Fatalf("expected first abort reason to win closeOnce, got %+v", info)
	}
}

func TestNativeSessionAbortConcurrentWithProviderCloseStopsReadPump(t *testing.T) {
	client, server := wstest.Pair(t)
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = session.Recv(context.Background())
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		session.Abort("client_abort")
	}()
	go func() {
		defer wg.Done()
		server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Code: wsconn.CloseNormalClosure, Reason: "provider_done"})
	}()
	wg.Wait()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Recv/read pump to stop after abort/provider close race")
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for client connection to close after abort/provider close race")
	}
}

func TestNativeSessionLocalCloseConcurrentWithProviderCloseDoesNotPanic(t *testing.T) {
	for i := 0; i < 100; i++ {
		t.Run("abort", func(t *testing.T) {
			t.Parallel()
			assertNativeSessionLocalCloseConcurrentWithProviderClose(t, func(session *NativeSession) {
				session.Abort("client_abort")
			})
		})
		t.Run("detach", func(t *testing.T) {
			t.Parallel()
			assertNativeSessionLocalCloseConcurrentWithProviderClose(t, func(session *NativeSession) {
				session.Detach("client_detach")
			})
		})
	}
}

func assertNativeSessionLocalCloseConcurrentWithProviderClose(t *testing.T, closeLocal func(*NativeSession)) {
	t.Helper()
	client, server := wstest.Pair(t)
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{})

	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		_, _ = session.Recv(context.Background())
	}()

	start := make(chan struct{})
	panicCh := make(chan any, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				panicCh <- recovered
			}
		}()
		<-start
		closeLocal(session)
	}()
	go func() {
		defer wg.Done()
		<-start
		server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Code: wsconn.CloseNormalClosure, Reason: "provider_done"})
	}()
	close(start)
	wg.Wait()

	select {
	case recovered := <-panicCh:
		t.Fatalf("local close panicked while provider close raced: %v", recovered)
	default:
	}
	select {
	case <-recvDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Recv to stop after provider/local close race")
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for client connection to close after provider/local close race")
	}
}

func TestNativeSessionCloseOnceKeepsFirstCloseReason(t *testing.T) {
	client, _ := wstest.Pair(t)
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		session.close(wsconn.CloseInfo{Kind: wsconn.CloseKindBackpressure, Reason: "first", Err: ErrNativeQueueFull})
	}()
	go func() {
		defer wg.Done()
		session.close(wsconn.CloseInfo{Kind: wsconn.CloseKindPeerClose, Reason: "second"})
	}()
	wg.Wait()

	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for close")
	}
	info := client.CloseInfo()
	if info.Reason != "first" && info.Reason != "second" {
		t.Fatalf("expected one close reason to win closeOnce, got %+v", info)
	}
	winner := info.Reason
	session.close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "third"})
	if got := client.CloseInfo().Reason; got != winner {
		t.Fatalf("expected closeOnce to keep first close reason %q, got %q", winner, got)
	}
}

func TestNativeSessionContainsAdapterPanicAtPrepareBoundary(t *testing.T) {
	client, _ := wstest.Pair(t)
	var diagnostics []NativeDiagnostic
	session := NewNativeSession(client, nativeTestAdapter{
		prepare: func(context.Context, Frame) (Frame, error) {
			panic("raw secret panic")
		},
	}, NativeSessionOptions{
		Diagnostics: func(diag NativeDiagnostic) {
			diagnostics = append(diagnostics, diag)
		},
	})

	result := session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, ErrAdapterPanic) {
		t.Fatalf("expected prepare panic to become not_attempted adapter panic, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv prepare panic event: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginAdapterPanic || event.DetailPhase != RecvDetailPhasePrepareClientFrame || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal || !errors.Is(event.Err, ErrAdapterPanic) {
		t.Fatalf("expected prepare panic to emit adapter_panic event, got %+v", event)
	}
	result = session.SendClientWithResult(context.Background(), SendRequest{AttemptID: "attempt-test", Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, ErrUpstreamClosed) {
		t.Fatalf("expected prepare panic to fail-close native session, got %+v", result)
	}
	if len(diagnostics) != 1 || diagnostics[0].Code != "adapter_panic" || diagnostics[0].Phase != RecvDetailPhasePrepareClientFrame || diagnostics[0].StackHash == "" || diagnostics[0].PanicClass != "string" {
		t.Fatalf("expected safe diagnostic summary, got %+v", diagnostics)
	}
	if strings.Contains(result.Err.Error(), "raw secret panic") || strings.Contains(diagnostics[0].DetailError, "raw secret panic") {
		t.Fatalf("expected recovered value not to be exposed, err=%v diag=%+v", result.Err, diagnostics[0])
	}
}

func TestNativeSessionAdapterPanicForceEnqueuesWhenQueueFull(t *testing.T) {
	client, _ := wstest.Pair(t)
	session := NewNativeSession(client, nativeTestAdapter{
		prepare: func(context.Context, Frame) (Frame, error) {
			panic("raw secret panic")
		},
	}, NativeSessionOptions{RecvQueueSize: 1})

	frame := NewTextFrame([]byte(`{"type":"response.created"}`))
	if !session.enqueue(UpstreamEvent{Frame: &frame, DetailOrigin: RecvDetailOriginProviderFrame}) {
		t.Fatal("expected seed provider frame to fill queue")
	}

	result := session.SendClientWithResult(context.Background(), SendRequest{
		AttemptID: "attempt-panic",
		Frame:     NewTextFrame([]byte(`{"type":"response.create"}`)),
	})
	if result.Status != ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, ErrAdapterPanic) {
		t.Fatalf("expected adapter panic send result, got %+v", result)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv forced panic event: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginAdapterPanic || event.DetailPhase != RecvDetailPhasePrepareClientFrame || event.AttemptID != "attempt-panic" || !errors.Is(event.Err, ErrAdapterPanic) {
		t.Fatalf("expected adapter panic evidence to replace old buffered event, got %+v", event)
	}
}

func TestNativeSessionInvalidProviderFrameResultPreservesAdapterError(t *testing.T) {
	client, server := wstest.Pair(t)
	adapterErr := errors.New("adapter context")
	session := NewNativeSession(client, nativeTestAdapter{
		handle: func(context.Context, Frame) ProviderFrameResult {
			frame := NewTextFrame([]byte(`{"type":"response.created"}`))
			return ProviderFrameResult{
				Origin:    RecvDetailOriginProviderFrame,
				EmitFrame: &frame,
				Err:       adapterErr,
			}
		},
	}, NativeSessionOptions{})

	if err := server.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.created"}`)); err != nil {
		t.Fatalf("write provider frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv invalid provider frame result: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginProviderMalformed || !errors.Is(event.Err, ErrInvalidProviderFrameResult) || !errors.Is(event.Err, adapterErr) {
		t.Fatalf("expected invalid result to preserve adapter error context, got %+v", event)
	}
}

func TestNativeSessionContainsAdapterPanicAtProviderFrameBoundary(t *testing.T) {
	client, server := wstest.Pair(t)
	var diagnostics []NativeDiagnostic
	session := NewNativeSession(client, nativeTestAdapter{
		handle: func(context.Context, Frame) ProviderFrameResult {
			panic("raw provider payload")
		},
	}, NativeSessionOptions{
		Diagnostics: func(diag NativeDiagnostic) {
			diagnostics = append(diagnostics, diag)
		},
	})

	if err := server.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.created"}`)); err != nil {
		t.Fatalf("write provider frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv adapter panic event: %v", err)
	}
	if event.DetailOrigin != RecvDetailOriginAdapterPanic || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal || !errors.Is(event.Err, ErrAdapterPanic) {
		t.Fatalf("expected adapter panic event, got %+v", event)
	}
	if len(diagnostics) != 1 || diagnostics[0].Phase != RecvDetailPhaseHandleProviderFrame || diagnostics[0].StackHash == "" || diagnostics[0].PanicClass != "string" {
		t.Fatalf("expected provider frame panic diagnostic, got %+v", diagnostics)
	}
	if strings.Contains(event.Err.Error(), "raw provider payload") {
		t.Fatalf("expected recovered value not to be exposed, err=%v", event.Err)
	}
}

func TestNativeSessionContainsAdapterPanicAtMapCloseBoundary(t *testing.T) {
	client, server := wstest.Pair(t)
	var diagnostics []NativeDiagnostic
	session := NewNativeSession(client, nativeTestAdapter{
		closeMap: func(context.Context, ProviderCloseInfo) ProviderCloseResult {
			panic("raw close panic")
		},
	}, NativeSessionOptions{
		Diagnostics: func(diag NativeDiagnostic) {
			diagnostics = append(diagnostics, diag)
		},
	})

	server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Code: wsconn.CloseNormalClosure, Reason: "done"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv map close panic event: %v", err)
	}
	if event.ProviderClose == nil || event.DetailOrigin != RecvDetailOriginNativeProviderClose || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProvider || !errors.Is(event.Err, ErrAdapterPanic) {
		t.Fatalf("expected map close adapter panic event, got %+v", event)
	}
	if len(diagnostics) != 1 || diagnostics[0].Phase != RecvDetailPhaseMapProviderClose || diagnostics[0].StackHash == "" || diagnostics[0].PanicClass != "string" {
		t.Fatalf("expected map close panic diagnostic, got %+v", diagnostics)
	}
	if strings.Contains(event.Err.Error(), "raw close panic") {
		t.Fatalf("expected recovered value not to be exposed, err=%v", event.Err)
	}
}

func TestNativeSessionContainsReadPumpPanic(t *testing.T) {
	client, server := wstest.Pair(t)
	server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Reason: "force_terminal_read_state"})
	_, _, _ = client.ReadInitial(context.Background())

	var diagnostics []NativeDiagnostic
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{
		Diagnostics: func(diag NativeDiagnostic) {
			diagnostics = append(diagnostics, diag)
		},
	})

	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("read pump panic escaped containment: %v", recovered)
			}
		}()
		session.runReadPump()
	}()

	event, ok := session.recvBufferedEvent()
	if !ok {
		t.Fatal("expected contained read pump panic event")
	}
	if event.DetailOrigin != RecvDetailOriginNativeReadError || PayloadOriginForDetailOrigin(event.DetailOrigin) != PayloadOriginProxyLocal || !errors.Is(event.Err, ErrNativeReadPumpPanic) {
		t.Fatalf("expected native read error panic event, got %+v", event)
	}
	if len(diagnostics) != 1 || diagnostics[0].Code != "native_read_pump_panic" || diagnostics[0].StackHash == "" || diagnostics[0].PanicClass == "" {
		t.Fatalf("expected safe read pump panic diagnostic, got %+v", diagnostics)
	}
}
