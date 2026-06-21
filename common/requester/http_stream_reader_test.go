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
