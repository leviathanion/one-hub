package requester

import (
	"bytes"
	"errors"
	"strings"
)

var ErrSSEEventTooLarge = errors.New("SSE event exceeds configured limit")

// SplitSSELine 返回包含原始分隔符的一行；没有分隔符时返回全部尾部。
func SplitSSELine(raw string) (line, rest string) {
	end := strings.IndexAny(raw, "\r\n")
	if end < 0 {
		return raw, ""
	}
	end++
	if raw[end-1] == '\r' && end < len(raw) && raw[end] == '\n' {
		end++
	}
	return raw[:end], raw[end:]
}

func SSELineContent(line string) string {
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
}

// SSEEventFramer is a protocol-neutral framing leaf. It preserves every byte
// of a complete event while keeping lifecycle interpretation in the caller.
// A split CRLF after a delivered event can emit its LF as a delimiter-only
// continuation; concatenating deliveries always retains the original wire.
type SSEEventFramer struct {
	buffer   bytes.Buffer
	maxBytes int
	// A CR terminates its line immediately; a subsequent LF belongs to that
	// delimiter even when the transport delivers it in a later read.
	pendingLF        bool
	completedCRBytes int
}

func NewSSEEventFramer(maxBytes int) *SSEEventFramer {
	return &SSEEventFramer{maxBytes: maxBytes}
}

func (f *SSEEventFramer) PushLine(line []byte) ([]byte, bool, error) {
	if f == nil {
		return nil, false, errors.New("SSE framer is nil")
	}
	if len(line) == 0 {
		return nil, false, nil
	}
	continuation := f.pendingLF && len(line) == 1 && line[0] == '\n'
	f.pendingLF = len(line) > 0 && line[len(line)-1] == '\r'
	if continuation {
		if f.buffer.Len() == 0 {
			if f.maxBytes > 0 && f.completedCRBytes+1 > f.maxBytes {
				f.Reset()
				return nil, false, ErrSSEEventTooLarge
			}
			f.completedCRBytes = 0
			// The CR-only event was already delivered without waiting for more
			// network bytes. Forward its optional LF without a second event.
			return append([]byte(nil), line...), true, nil
		}
		if f.maxBytes > 0 && f.buffer.Len()+1 > f.maxBytes {
			f.Reset()
			return nil, false, ErrSSEEventTooLarge
		}
		f.buffer.WriteByte('\n')
		return nil, false, nil
	}
	f.completedCRBytes = 0
	if f.maxBytes > 0 && f.buffer.Len()+len(line) > f.maxBytes {
		f.Reset()
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
	if f.pendingLF {
		f.completedCRBytes = len(event)
	}
	f.buffer.Reset()
	return event, true, nil
}

func (f *SSEEventFramer) Reset() {
	if f != nil {
		f.buffer.Reset()
		f.pendingLF = false
		f.completedCRBytes = 0
	}
}

func (f *SSEEventFramer) BufferedLen() int {
	if f == nil {
		return 0
	}
	return f.buffer.Len()
}

// TakePending 只交还原始尾部，不把 EOF 当成事件分隔符。
func (f *SSEEventFramer) TakePending() string {
	if f == nil {
		return ""
	}
	raw := f.buffer.String()
	f.Reset()
	return raw
}
