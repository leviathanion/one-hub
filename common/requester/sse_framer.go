package requester

import (
	"bytes"
	"errors"
)

var ErrSSEEventTooLarge = errors.New("SSE event exceeds configured limit")

// SSEEventFramer is a protocol-neutral framing leaf. It preserves every byte
// of a complete event while keeping lifecycle interpretation in the caller.
type SSEEventFramer struct {
	buffer   bytes.Buffer
	maxBytes int
}

func NewSSEEventFramer(maxBytes int) *SSEEventFramer {
	return &SSEEventFramer{maxBytes: maxBytes}
}

func (f *SSEEventFramer) PushLine(line []byte) ([]byte, bool, error) {
	if f == nil {
		return nil, false, errors.New("SSE framer is nil")
	}
	if f.maxBytes > 0 && f.buffer.Len()+len(line) > f.maxBytes {
		f.buffer.Reset()
		return nil, false, ErrSSEEventTooLarge
	}
	if len(line) > 0 {
		_, _ = f.buffer.Write(line)
	}
	if len(bytes.TrimRight(line, "\r\n")) != 0 {
		return nil, false, nil
	}
	if f.buffer.Len() == 0 || len(line) == 0 {
		return nil, false, nil
	}
	event := append([]byte(nil), f.buffer.Bytes()...)
	f.buffer.Reset()
	return event, true, nil
}

func (f *SSEEventFramer) Reset() {
	if f != nil {
		f.buffer.Reset()
	}
}

func (f *SSEEventFramer) BufferedLen() int {
	if f == nil {
		return 0
	}
	return f.buffer.Len()
}
