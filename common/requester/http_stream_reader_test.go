package requester

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestStreamDoesNotSurfaceEOFBeforeChunkIsConsumed(t *testing.T) {
	handlerStarted := make(chan struct{})
	stream, errWithCode := RequestStream[string](nil, &http.Response{
		Body: io.NopCloser(bytes.NewBufferString("first chunk\n")),
	}, func(rawLine *[]byte, dataChan chan string, _ chan error) {
		close(handlerStarted)
		dataChan <- string(*rawLine)
	})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	dataChan, errChan := stream.Recv()
	defer stream.Close()

	<-handlerStarted

	select {
	case err := <-errChan:
		t.Fatalf("unexpected early stream termination before chunk consumption: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	select {
	case data := <-dataChan:
		if data != "first chunk" {
			t.Fatalf("unexpected stream chunk: got %q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream chunk")
	}

	select {
	case err := <-errChan:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected EOF after chunk delivery, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EOF")
	}
	assertStreamChannelClosed(t, "data", dataChan)
	assertStreamChannelClosed(t, "error", errChan)
}

func TestRequestStreamDeliversFinalFragmentBeforeEOF(t *testing.T) {
	stream, errWithCode := RequestNoTrimStream[string](nil, &http.Response{
		Body: io.NopCloser(bytes.NewBufferString("data: final")),
	}, func(rawLine *[]byte, dataChan chan string, _ chan error) {
		dataChan <- string(*rawLine)
	})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	dataChan, errChan := stream.Recv()
	defer stream.Close()

	select {
	case data := <-dataChan:
		if data != "data: final" {
			t.Fatalf("unexpected final fragment: got %q", data)
		}
	case err := <-errChan:
		t.Fatalf("expected final fragment before EOF, got error %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for final fragment")
	}

	select {
	case err := <-errChan:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected EOF after final fragment, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EOF")
	}
	assertStreamChannelClosed(t, "data", dataChan)
	assertStreamChannelClosed(t, "error", errChan)
}

func TestRequestStreamWithReadLimitDeliversFinalFragmentBeforeEOF(t *testing.T) {
	stream, errWithCode := RequestNoTrimStreamWithOptions[string](nil, &http.Response{
		Body: io.NopCloser(bytes.NewBufferString("data: final")),
	}, func(rawLine *[]byte, dataChan chan string, _ chan error) {
		dataChan <- string(*rawLine)
	}, StreamReadOptions{MaxLineBytes: 32})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	dataChan, errChan := stream.Recv()
	defer stream.Close()

	select {
	case data := <-dataChan:
		if data != "data: final" {
			t.Fatalf("unexpected final fragment: got %q", data)
		}
	case err := <-errChan:
		t.Fatalf("expected final fragment before EOF, got error %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for final fragment")
	}

	select {
	case err := <-errChan:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected EOF after final fragment, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EOF")
	}
	assertStreamChannelClosed(t, "data", dataChan)
	assertStreamChannelClosed(t, "error", errChan)
}

func TestRequestStreamUsesBoundedDefaultLineSize(t *testing.T) {
	stream, errWithCode := RequestNoTrimStream[string](nil, &http.Response{
		Body: io.NopCloser(strings.NewReader("")),
	}, func(*[]byte, chan string, chan error) {})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}
	defer stream.Close()
	if stream.options.MaxLineBytes != defaultProviderStreamMaxLineBytes {
		t.Fatalf("expected default line limit %d, got %d", defaultProviderStreamMaxLineBytes, stream.options.MaxLineBytes)
	}
}

func TestRequestStreamRequiredTerminalRejectsTransportEOF(t *testing.T) {
	stream, errWithCode := RequestStreamWithOptions[string](nil, &http.Response{
		Body: io.NopCloser(bytes.NewBufferString("partial chunk\n")),
	}, func(rawLine *[]byte, dataChan chan string, _ chan error) {
		dataChan <- string(*rawLine)
	}, StreamReadOptions{RequireProtocolTerminal: true})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	dataChan, errChan := stream.Recv()
	defer stream.Close()
	if got := <-dataChan; got != "partial chunk" {
		t.Fatalf("unexpected stream chunk: %q", got)
	}
	if err := <-errChan; !errors.Is(err, ErrStreamProtocolTerminalMissing) {
		t.Fatalf("error=%v, want ErrStreamProtocolTerminalMissing", err)
	}
}

func TestRequestStreamRequiredTerminalPredicateAcceptsTransportEOF(t *testing.T) {
	predicateCalls := 0
	stream, errWithCode := RequestStreamWithOptions[string](nil, &http.Response{
		Body: io.NopCloser(bytes.NewBufferString("complete\n")),
	}, func(rawLine *[]byte, dataChan chan string, _ chan error) {
		dataChan <- string(*rawLine)
	}, StreamReadOptions{
		RequireProtocolTerminal: true,
		ProtocolTerminalPredicate: func() bool {
			predicateCalls++
			return true
		},
	})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	dataChan, errChan := stream.Recv()
	defer stream.Close()
	if got := <-dataChan; got != "complete" {
		t.Fatalf("unexpected stream chunk: %q", got)
	}
	if err := <-errChan; !errors.Is(err, io.EOF) || errors.Is(err, ErrStreamProtocolTerminalMissing) {
		t.Fatalf("error=%v, want accepted transport EOF", err)
	}
	if predicateCalls != 1 {
		t.Fatalf("terminal predicate called %d times, want once", predicateCalls)
	}
}

func TestRequestStreamRequiredTerminalPredicateDoesNotConvertReadTimeout(t *testing.T) {
	readErr := &streamReadTimeoutError{}
	stream, errWithCode := RequestStreamWithOptions[string](nil, &http.Response{
		Body: &streamReadTimeoutBody{err: readErr},
	}, func(*[]byte, chan string, chan error) {}, StreamReadOptions{
		RequireProtocolTerminal: true,
		ProtocolTerminalPredicate: func() bool {
			t.Fatal("terminal predicate must not run for a read timeout")
			return true
		},
	})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	_, errChan := stream.Recv()
	defer stream.Close()
	if err := <-errChan; err != readErr {
		t.Fatalf("read error=%v, want original timeout %v", err, readErr)
	}
}

func TestRequestStreamRequiredTerminalAcceptsHandlerEOF(t *testing.T) {
	stream, errWithCode := RequestStreamWithOptions[string](nil, &http.Response{
		Body: io.NopCloser(bytes.NewBufferString("[DONE]\n")),
	}, func(rawLine *[]byte, _ chan string, errChan chan error) {
		*rawLine = StreamClosed
		errChan <- io.EOF
	}, StreamReadOptions{RequireProtocolTerminal: true})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	_, errChan := stream.Recv()
	defer stream.Close()
	if err := <-errChan; !errors.Is(err, io.EOF) || errors.Is(err, ErrStreamProtocolTerminalMissing) {
		t.Fatalf("error=%v, want protocol io.EOF", err)
	}
}

func TestRequestNoTrimStreamWithOptionsRejectsOversizedLine(t *testing.T) {
	stream, errWithCode := RequestNoTrimStreamWithOptions[string](nil, &http.Response{
		Body: io.NopCloser(bytes.NewBufferString("data: 123456789\n")),
	}, func(rawLine *[]byte, dataChan chan string, _ chan error) {
		dataChan <- string(*rawLine)
	}, StreamReadOptions{MaxLineBytes: 8})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	dataChan, errChan := stream.Recv()
	defer stream.Close()

	select {
	case data := <-dataChan:
		t.Fatalf("expected oversized line to be rejected before data delivery, got %q", data)
	case err := <-errChan:
		if !errors.Is(err, ErrStreamLineTooLarge) {
			t.Fatalf("expected ErrStreamLineTooLarge, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for oversized line error")
	}
}

func TestStreamReaderRecvIsIdempotent(t *testing.T) {
	body := newGatedReadCloser("data: once\n")
	stream, errWithCode := RequestNoTrimStream[string](nil, &http.Response{
		Body: body,
	}, func(rawLine *[]byte, dataChan chan string, _ chan error) {
		dataChan <- string(*rawLine)
	})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	dataChan, errChan := stream.Recv()
	dataChanAgain, errChanAgain := stream.Recv()
	defer stream.Close()
	if dataChan != dataChanAgain || errChan != errChanAgain {
		t.Fatal("expected repeated Recv calls to return the same channels")
	}

	time.Sleep(50 * time.Millisecond)
	if body.maxActive.Load() > 1 {
		t.Fatalf("expected only one reader goroutine, observed %d concurrent reads", body.maxActive.Load())
	}
	body.Release()

	select {
	case data := <-dataChan:
		if data != "data: once\n" {
			t.Fatalf("unexpected stream chunk: got %q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream chunk")
	}
	select {
	case err := <-errChan:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected EOF after chunk delivery, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EOF")
	}
	assertStreamChannelClosed(t, "data", dataChan)
	assertStreamChannelClosed(t, "error", errChan)
}

func TestStreamReaderPanicRecoveryDoesNotSurfaceRecoveredValue(t *testing.T) {
	stream, errWithCode := RequestNoTrimStream[string](nil, &http.Response{
		Body: io.NopCloser(bytes.NewBufferString("data: panic\n")),
	}, func(_ *[]byte, _ chan string, _ chan error) {
		panic("raw secret panic")
	})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	_, errChan := stream.Recv()
	defer stream.Close()

	select {
	case err := <-errChan:
		if err == nil {
			t.Fatal("expected panic recovery error")
		}
		if strings.Contains(err.Error(), "raw secret panic") {
			t.Fatalf("expected recovered value not to surface, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for panic recovery error")
	}
	assertStreamChannelClosed(t, "error", errChan)
}

func TestStreamReaderCloseClosesRecvChannels(t *testing.T) {
	body := newBlockingReadCloser()
	stream, errWithCode := RequestNoTrimStream[string](nil, &http.Response{
		Body: body,
	}, func(rawLine *[]byte, dataChan chan string, _ chan error) {
		dataChan <- string(*rawLine)
	})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	dataChan, errChan := stream.Recv()
	stream.Close()

	assertStreamChannelClosed(t, "data", dataChan)
	assertStreamChannelClosed(t, "error", errChan)
}

func TestStreamEmitterSendDataUnblocksOnClose(t *testing.T) {
	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	stream, errWithCode := RequestNoTrimStreamWithEmitterOptions[string](nil, &http.Response{
		Body: io.NopCloser(bytes.NewBufferString("data: blocked\n")),
	}, func(rawLine *[]byte, emitter StreamEmitter[string]) {
		close(handlerStarted)
		emitter.SendData(string(*rawLine))
		close(handlerDone)
	}, StreamReadOptions{})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	dataChan, errChan := stream.Recv()
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream handler")
	}

	stream.Close()

	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("expected emitter send to unblock after Close")
	}
	assertStreamChannelClosed(t, "data", dataChan)
	assertStreamChannelClosed(t, "error", errChan)
}

func TestCloseAndDrainStreamUnblocksLegacyHandlerSend(t *testing.T) {
	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	stream, errWithCode := RequestNoTrimStream[string](nil, &http.Response{
		Body: io.NopCloser(bytes.NewBufferString("data: blocked\n")),
	}, func(rawLine *[]byte, dataChan chan string, _ chan error) {
		close(handlerStarted)
		dataChan <- string(*rawLine)
		close(handlerDone)
	})
	if errWithCode != nil {
		t.Fatalf("unexpected stream construction error: %v", errWithCode)
	}

	dataChan, errChan := stream.Recv()
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for legacy handler")
	}

	closed := make(chan struct{})
	go func() {
		CloseAndDrainStream[string](stream)
		close(closed)
	}()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("legacy handler send remained blocked after close")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close-and-drain did not finish")
	}
	assertStreamChannelClosed(t, "data", dataChan)
	assertStreamChannelClosed(t, "error", errChan)
}

func TestSendStreamErrorClosesStreamWhenErrorChannelBlocked(t *testing.T) {
	originalTimeout := streamErrorSendTimeout
	streamErrorSendTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		streamErrorSendTimeout = originalTimeout
	})

	stream := &streamReader[string]{
		DataChan: make(chan string),
		ErrChan:  make(chan error, 1),
		done:     make(chan struct{}),
	}
	stream.ErrChan <- errors.New("first error already queued")

	if sendStreamError(stream, errors.New("second error")) {
		t.Fatal("expected blocked stream error send to fail")
	}
	select {
	case <-stream.done:
	case <-time.After(time.Second):
		t.Fatal("expected blocked stream error send to close the stream")
	}
}

func assertStreamChannelClosed[T any](t *testing.T, name string, ch <-chan T) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("expected %s channel to be closed", name)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s channel to close", name)
	}
}

type gatedReadCloser struct {
	line      []byte
	release   chan struct{}
	once      sync.Once
	reads     atomic.Int32
	active    atomic.Int32
	maxActive atomic.Int32
}

func newGatedReadCloser(line string) *gatedReadCloser {
	return &gatedReadCloser{
		line:    []byte(line),
		release: make(chan struct{}),
	}
}

func (r *gatedReadCloser) Read(p []byte) (int, error) {
	active := r.active.Add(1)
	for {
		maxActive := r.maxActive.Load()
		if active <= maxActive || r.maxActive.CompareAndSwap(maxActive, active) {
			break
		}
	}
	defer r.active.Add(-1)

	<-r.release
	if r.reads.Add(1) > 1 {
		return 0, io.EOF
	}
	n := copy(p, r.line)
	return n, nil
}

func (r *gatedReadCloser) Close() error {
	r.Release()
	return nil
}

func (r *gatedReadCloser) Release() {
	r.once.Do(func() {
		close(r.release)
	})
}

type blockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

type streamReadTimeoutError struct{}

func (*streamReadTimeoutError) Error() string   { return "stream read timeout" }
func (*streamReadTimeoutError) Timeout() bool   { return true }
func (*streamReadTimeoutError) Temporary() bool { return true }

type streamReadTimeoutBody struct {
	err error
}

func (b *streamReadTimeoutBody) Read([]byte) (int, error) { return 0, b.err }
func (*streamReadTimeoutBody) Close() error               { return nil }

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{closed: make(chan struct{})}
}

func (r *blockingReadCloser) Read(_ []byte) (int, error) {
	<-r.closed
	return 0, errors.New("closed response body")
}

func (r *blockingReadCloser) Close() error {
	r.once.Do(func() {
		close(r.closed)
	})
	return nil
}
