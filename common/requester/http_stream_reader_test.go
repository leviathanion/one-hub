package requester

import (
	"bytes"
	"errors"
	"io"
	"net/http"
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
}
