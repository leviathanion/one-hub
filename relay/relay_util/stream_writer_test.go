package relay_util

import (
	"bytes"
	"testing"
)

type testFlushTarget struct {
	body      bytes.Buffer
	flushes   int
	snapshots []string
}

func (t *testFlushTarget) Write(p []byte) (int, error) {
	return t.body.Write(p)
}

func (t *testFlushTarget) Flush() {
	t.flushes++
	t.snapshots = append(t.snapshots, t.body.String())
}

func TestBufferedStreamWriterFlushesOnlyAfterCompleteSSEFrame(t *testing.T) {
	target := &testFlushTarget{}
	writer := NewBufferedStreamWriter(target, 0)

	parts := []string{"event: ", "response.delta", "\ndata: ", `{"delta":"ok"}`, "\n\n"}
	for i, part := range parts {
		if _, err := writer.WriteString(part); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
		if i < len(parts)-1 && target.flushes != 0 {
			t.Fatalf("expected no flush before SSE frame is complete, got %d", target.flushes)
		}
	}

	if target.flushes != 1 {
		t.Fatalf("expected a single flush after completing the SSE frame, got %d", target.flushes)
	}

	expected := "event: response.delta\ndata: {\"delta\":\"ok\"}\n\n"
	if got := target.body.String(); got != expected {
		t.Fatalf("unexpected flushed body: got %q want %q", got, expected)
	}
}

func TestBufferedStreamWriterFlushesWhenBlankLineArrivesSeparately(t *testing.T) {
	target := &testFlushTarget{}
	writer := NewBufferedStreamWriter(target, 0)

	parts := []string{
		"event: response.delta\n",
		"data: {\"delta\":\"ok\"}\n",
		"\n",
	}

	for i, part := range parts {
		if _, err := writer.WriteString(part); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
		if i < len(parts)-1 && target.flushes != 0 {
			t.Fatalf("expected no flush before blank line, got %d", target.flushes)
		}
	}

	if target.flushes != 1 {
		t.Fatalf("expected a single flush after blank line, got %d", target.flushes)
	}

	expected := "event: response.delta\ndata: {\"delta\":\"ok\"}\n\n"
	if got := target.body.String(); got != expected {
		t.Fatalf("unexpected flushed body: got %q want %q", got, expected)
	}
}

func TestBufferedStreamWriterFlushesAcrossCRLFBoundaryWithoutCombiningBuffers(t *testing.T) {
	target := &testFlushTarget{}
	writer := NewBufferedStreamWriter(target, 0)

	parts := []string{
		"event: response.completed\r\n",
		"data: {\"ok\":true}\r",
		"\n\r\n",
	}

	for i, part := range parts {
		if _, err := writer.WriteString(part); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
		if i < len(parts)-1 && target.flushes != 0 {
			t.Fatalf("expected no flush before CRLF delimiter is complete, got %d", target.flushes)
		}
	}

	if target.flushes != 1 {
		t.Fatalf("expected a single flush after CRLF delimiter, got %d", target.flushes)
	}
}
