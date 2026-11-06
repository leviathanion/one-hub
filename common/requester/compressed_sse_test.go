package requester

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func integrityTestStream(t *testing.T, source io.ReadCloser, encoding string, limit int64, terminal func([]byte) bool, tailLimit ...int64) *streamReader[string] {
	t.Helper()
	resp := &http.Response{Body: source, Header: http.Header{"Content-Encoding": {encoding}}}
	decodeCompressedResponse(resp)
	options := StreamReadOptions{SSELines: true, MaxLineBytes: limit, SSETerminalEvent: terminal}
	if len(tailLimit) > 0 {
		options.MaxSSETerminalTailBytes = tailLimit[0]
	}
	stream, apiErr := RequestNoTrimStreamWithEmitterOptions[string](nil, resp, func(line *[]byte, emitter StreamEmitter[string]) {
		// 即使完整性校验按事件暂存，handler 仍只收到一条物理行。
		if _, rest := SplitSSELine(string(*line)); rest != "" {
			t.Errorf("handler received multiple physical lines: %q", *line)
		}
		emitter.SendData(string(*line))
	}, options)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	t.Cleanup(func() { CloseAndDrainStream(stream) })
	return stream
}

func testSSETerminal(event []byte) bool { return bytes.Contains(event, []byte("data: end")) }

func collectIntegrityStream(t *testing.T, stream *streamReader[string]) (string, error) {
	t.Helper()
	data, failures := stream.Recv()
	var body strings.Builder
	var failure error
	deadline := time.After(3 * time.Second)
	for data != nil || failures != nil {
		select {
		case line, ok := <-data:
			if !ok {
				data = nil
			} else {
				body.WriteString(line)
			}
		case err, ok := <-failures:
			if !ok {
				failures = nil
			} else if err != nil {
				failure = err
			}
		case <-deadline:
			t.Fatal("stream did not finish")
		}
	}
	return body.String(), failure
}

func TestCompressedSSEIntegrityPreservesWireAndUnterminatedTail(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd"} {
		for _, delimiter := range []string{"\n", "\r\n", "\r"} {
			t.Run(encoding+"/"+strings.ReplaceAll(delimiter, "\r", "CR"), func(t *testing.T) {
				plain := strings.ReplaceAll(": prefix\n\ndata: {\ndata: \"future\":9007199254740993}\n\nevent: done\ndata: end\n\n: trailer\n\ndata: [DONE]\n\ndata: unfinished", "\n", delimiter)
				wire := compressedFixture(t, encoding, []byte(plain))
				stream := integrityTestStream(t, io.NopCloser(bytes.NewReader(wire)), encoding, 1024, testSSETerminal)
				got, err := collectIntegrityStream(t, stream)
				if got != plain || !errors.Is(err, io.EOF) {
					t.Fatalf("wire=%q want=%q err=%v", got, plain, err)
				}
			})
		}
	}
}

func TestCompressedSSEIntegrityBoundsEventsAndTerminalTail(t *testing.T) {
	const eventLimit = 128
	const tailLimit = 64
	const prefix = "data: prefix\n\n"
	const terminal = "data: end\n\n"
	fullTerminal := "data: end" + strings.Repeat(" ", eventLimit-len(terminal)) + "\n\n"
	for _, encoding := range []string{"gzip", "zstd"} {
		for _, test := range []struct {
			name      string
			body      string
			tailLimit int64
			wantErr   error
		}{
			{"terminal at event limit with done", prefix + fullTerminal + "data: [DONE]\n\n", tailLimit, io.EOF},
			{"terminal at event limit with comment", prefix + fullTerminal + ": trailer\n\n", tailLimit, io.EOF},
			{"event and tail at independent limits", prefix + fullTerminal + strings.Repeat(" ", tailLimit), tailLimit, io.EOF},
			{"terminal tail over limit", prefix + terminal + strings.Repeat(" ", tailLimit+1), tailLimit, ErrSSETerminalTailTooLarge},
			{"terminal event over limit", prefix + "data: end " + strings.Repeat(" ", eventLimit-len(terminal)) + "\n\n", tailLimit, ErrSSEEventTooLarge},
			{"multiline event over limit", prefix + "data: " + strings.Repeat("x", 70) + "\ndata: " + strings.Repeat("y", 70) + "\n\n", tailLimit, ErrSSEEventTooLarge},
			{"default tail budget independent of event limit", prefix + fullTerminal + strings.Repeat(": trailer\n\n", 32), 0, io.EOF},
		} {
			t.Run(encoding+"/"+test.name, func(t *testing.T) {
				wire := compressedFixture(t, encoding, []byte(test.body))
				stream := integrityTestStream(t, io.NopCloser(bytes.NewReader(wire)), encoding, eventLimit, testSSETerminal, test.tailLimit)
				got, err := collectIntegrityStream(t, stream)
				want := prefix
				if errors.Is(test.wantErr, io.EOF) {
					want = test.body
				}
				if got != want || !errors.Is(err, test.wantErr) {
					t.Fatalf("delivered=%q want=%q err=%v wantErr=%v", got, want, err, test.wantErr)
				}
			})
		}
	}
}

func TestCompressedSSETrailerWaitCanBeCanceled(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	terminalSeen := make(chan struct{})
	stream := integrityTestStream(t, reader, "gzip", 1024, func(event []byte) bool {
		if testSSETerminal(event) {
			close(terminalSeen)
			return true
		}
		return false
	})
	go func() {
		compressed := gzip.NewWriter(writer)
		_, _ = io.WriteString(compressed, "data: end\n\n")
		_ = compressed.Flush()
	}()
	stream.Recv()
	select {
	case <-terminalSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not reach terminal")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := stream.CloseAndDrainContext(ctx); err != nil {
		t.Fatalf("close did not interrupt trailer verification: %v", err)
	}
}

func TestUncompressedSSETerminalDoesNotWaitForEOF(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	stream := integrityTestStream(t, reader, "", 1024, func([]byte) bool {
		t.Error("uncompressed stream invoked integrity gate")
		return true
	})
	go func() { _, _ = io.WriteString(writer, "data: end\n\n") }()
	data, _ := stream.Recv()
	select {
	case line := <-data:
		if line != "data: end\n" {
			t.Fatalf("terminal changed: %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("uncompressed terminal waited for EOF")
	}
}
