package relay_util

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	GinStreamWriterKey      = "relay_stream_writer"
	defaultStreamFlushBytes = 4 * 1024
	sseDelimiterTailBytes   = 3
)

type StreamWriter interface {
	Write([]byte) (int, error)
	WriteString(string) (int, error)
	Flush() error
	Close() error
}

type streamFlushTarget interface {
	io.Writer
	http.Flusher
}

type directStreamWriter struct {
	target streamFlushTarget
}

func (w *directStreamWriter) Write(p []byte) (int, error) {
	n, err := w.target.Write(p)
	if err != nil {
		return n, err
	}
	w.target.Flush()
	return n, nil
}

func (w *directStreamWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *directStreamWriter) Flush() error {
	w.target.Flush()
	return nil
}

func (w *directStreamWriter) Close() error {
	return w.Flush()
}

type BufferedStreamWriter struct {
	target     streamFlushTarget
	buffer     *bufio.Writer
	pending    int
	flushBytes int
	tail       []byte
}

func NewBufferedStreamWriter(target streamFlushTarget, flushBytes int) *BufferedStreamWriter {
	if flushBytes <= 0 {
		flushBytes = defaultStreamFlushBytes
	}

	return &BufferedStreamWriter{
		target:     target,
		buffer:     bufio.NewWriterSize(target, flushBytes),
		flushBytes: flushBytes,
	}
}

func SetStreamWriter(c *gin.Context, writer StreamWriter) {
	c.Set(GinStreamWriterKey, writer)
}

func ClearStreamWriter(c *gin.Context) {
	c.Set(GinStreamWriterKey, nil)
}

func GetStreamWriter(c *gin.Context) StreamWriter {
	if c != nil {
		if cached, exists := c.Get(GinStreamWriterKey); exists {
			if writer, ok := cached.(StreamWriter); ok && writer != nil {
				return writer
			}
		}
	}

	return &directStreamWriter{target: c.Writer}
}

func (w *BufferedStreamWriter) Write(p []byte) (int, error) {
	n, err := w.buffer.Write(p)
	w.pending += n
	if err != nil {
		return n, err
	}

	if w.shouldFlushAfterWrite(p[:n]) {
		if flushErr := w.Flush(); flushErr != nil {
			return n, flushErr
		}
	}

	return n, nil
}

func (w *BufferedStreamWriter) WriteString(s string) (int, error) {
	n, err := w.buffer.WriteString(s)
	w.pending += n
	if err != nil {
		return n, err
	}

	if w.shouldFlushAfterWriteString(s[:n]) {
		if flushErr := w.Flush(); flushErr != nil {
			return n, flushErr
		}
	}

	return n, nil
}

func (w *BufferedStreamWriter) shouldFlushAfterWrite(p []byte) bool {
	if w.pending >= w.flushBytes {
		w.updateTail(p)
		return true
	}

	shouldFlush := bytes.Contains(p, []byte("\n\n")) ||
		bytes.Contains(p, []byte("\r\n\r\n")) ||
		hasSSEDelimiterAcrossBoundaryBytes(w.tail, p)
	w.updateTail(p)

	return shouldFlush
}

func (w *BufferedStreamWriter) shouldFlushAfterWriteString(s string) bool {
	if w.pending >= w.flushBytes {
		w.updateTailString(s)
		return true
	}

	shouldFlush := strings.Contains(s, "\n\n") ||
		strings.Contains(s, "\r\n\r\n") ||
		hasSSEDelimiterAcrossBoundaryString(w.tail, s)
	w.updateTailString(s)

	return shouldFlush
}

func (w *BufferedStreamWriter) updateTail(p []byte) {
	if len(p) > sseDelimiterTailBytes {
		p = p[len(p)-sseDelimiterTailBytes:]
	}

	w.tail = append(w.tail[:0], p...)
}

func (w *BufferedStreamWriter) updateTailString(s string) {
	if len(s) > sseDelimiterTailBytes {
		s = s[len(s)-sseDelimiterTailBytes:]
	}

	if cap(w.tail) < len(s) {
		w.tail = make([]byte, len(s))
	} else {
		w.tail = w.tail[:len(s)]
	}
	copy(w.tail, s)
}

func hasSSEDelimiterAcrossBoundaryBytes(tail []byte, p []byte) bool {
	return hasDelimiterAcrossBoundaryBytes(tail, p, []byte("\n\n")) ||
		hasDelimiterAcrossBoundaryBytes(tail, p, []byte("\r\n\r\n"))
}

func hasDelimiterAcrossBoundaryBytes(tail []byte, p []byte, delimiter []byte) bool {
	for split := 1; split < len(delimiter); split++ {
		if len(tail) < split || len(p) < len(delimiter)-split {
			continue
		}
		if bytes.Equal(tail[len(tail)-split:], delimiter[:split]) && bytes.HasPrefix(p, delimiter[split:]) {
			return true
		}
	}
	return false
}

func hasSSEDelimiterAcrossBoundaryString(tail []byte, s string) bool {
	return hasDelimiterAcrossBoundaryString(tail, s, "\n\n") ||
		hasDelimiterAcrossBoundaryString(tail, s, "\r\n\r\n")
}

func hasDelimiterAcrossBoundaryString(tail []byte, s string, delimiter string) bool {
	for split := 1; split < len(delimiter); split++ {
		if len(tail) < split || len(s) < len(delimiter)-split {
			continue
		}
		if !strings.HasPrefix(s, delimiter[split:]) {
			continue
		}
		matched := true
		start := len(tail) - split
		for i := 0; i < split; i++ {
			if tail[start+i] != delimiter[i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func (w *BufferedStreamWriter) Flush() error {
	if w.pending == 0 {
		return nil
	}

	if err := w.buffer.Flush(); err != nil {
		return err
	}

	w.target.Flush()
	w.pending = 0
	w.tail = w.tail[:0]
	return nil
}

func (w *BufferedStreamWriter) Close() error {
	return w.Flush()
}
