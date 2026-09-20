package requester

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type sseChunkReader struct{ chunks []string }

func (r *sseChunkReader) Read(p []byte) (int, error) {
	for len(r.chunks) > 0 && r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	return n, nil
}

func framedSSETestStream(t *testing.T, body io.ReadCloser) RawSSEEventStream {
	t.Helper()
	framer := NewSSEEventFramer(1024)
	stream, apiErr := RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{Body: body}, func(line *[]byte, emitter StreamEmitter[string]) {
		event, ready, err := framer.PushLine(*line)
		*line = nil
		if err != nil {
			emitter.SendError(err)
			*line = StreamClosed
			return
		}
		if ready {
			emitter.SendData(string(event))
		}
	}, StreamReadOptions{})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	t.Cleanup(func() { CloseAndDrainStream(stream) })
	return stream
}

func TestRawSSESplitCRLFPreservesMultilineEventAndTail(t *testing.T) {
	cases := [][]string{
		{"event: extension\r", "\ndata: {\r", "\ndata: \"value\": 1}\r", "\n\r", "\n"},
		{"event: extension\r", "data: {\r", "data: \"value\": 1}\r", "\r"},
		{"event: extension\r\ndata: {\r\ndata: \"value\": 1}\r\n\r\n"},
	}
	for _, chunks := range cases {
		t.Run(strings.Join(chunks, "|"), func(t *testing.T) {
			want := strings.Join(chunks, "")
			stream := framedSSETestStream(t, io.NopCloser(&sseChunkReader{chunks: append([]string(nil), chunks...)}))
			data, errs := stream.Recv()
			var got strings.Builder
			observed := 0
			deadline := time.After(time.Second)
			for data != nil || errs != nil {
				select {
				case frame, ok := <-data:
					if !ok {
						data = nil
						continue
					}
					got.WriteString(frame)
					if strings.Contains(frame, "data:") {
						observed++
						if strings.Count(frame, "data:") != 2 {
							t.Fatalf("multiline event split early: %q", frame)
						}
					}
				case err, ok := <-errs:
					if !ok {
						errs = nil
						continue
					}
					if !errors.Is(err, io.EOF) {
						t.Fatal(err)
					}
				case <-deadline:
					t.Fatal("stream did not reach EOF")
				}
			}
			if got.String() != want || observed != 1 {
				t.Fatalf("wire=%q want=%q observations=%d", got.String(), want, observed)
			}
		})
	}
}

func TestRawSSECRCompletionDoesNotWaitForNextByte(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	stream := framedSSETestStream(t, reader)
	data, _ := stream.Recv()
	wire := "data: immediate\r\r"
	go func() { _, _ = io.WriteString(writer, wire) }()
	select {
	case frame := <-data:
		if frame != wire {
			t.Fatalf("wire=%q want=%q", frame, wire)
		}
	case <-time.After(time.Second):
		t.Fatal("CR-only event waited for another byte or EOF")
	}
}

func TestSSEFramerSplitLFDoesNotEndAnOpenEvent(t *testing.T) {
	f := NewSSEEventFramer(1024)
	for _, line := range []string{"data: first\r", "\n", "data: second\r", "\n"} {
		if frame, complete, err := f.PushLine([]byte(line)); err != nil || complete {
			t.Fatalf("premature event %q: %q %v", line, frame, err)
		}
	}
	frame, complete, err := f.PushLine([]byte("\r"))
	if err != nil || !complete || string(frame) != "data: first\r\ndata: second\r\n\r" {
		t.Fatalf("frame=%q complete=%v err=%v", frame, complete, err)
	}
	tail, complete, err := f.PushLine([]byte("\n"))
	if err != nil || !complete || string(tail) != "\n" || f.BufferedLen() != 0 {
		t.Fatalf("trailing LF lost: tail=%q complete=%v err=%v", tail, complete, err)
	}
}

func TestSSEFramerSplitDelimiterRespectsEventByteLimit(t *testing.T) {
	f := NewSSEEventFramer(len("data:x\r\n\r"))
	for _, line := range []string{"data:x\r\n", "\r"} {
		if _, _, err := f.PushLine([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.PushLine([]byte("\n")); !errors.Is(err, ErrSSEEventTooLarge) {
		t.Fatalf("late delimiter escaped event byte bound: %v", err)
	}
}
