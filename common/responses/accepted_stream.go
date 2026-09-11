package responses

import (
	"context"
	"errors"
	"strings"

	"one-api/common/requester"
)

// SSEChunkFramer turns arbitrary transport chunks into complete SSE events.
// Both an unterminated physical line and the accumulated event share maxBytes.
type SSEChunkFramer struct {
	framer      *requester.SSEEventFramer
	pendingLine strings.Builder
	maxBytes    int
	afterCR     bool
}

func NewSSEChunkFramer(maxBytes int) *SSEChunkFramer {
	return &SSEChunkFramer{
		framer:   requester.NewSSEEventFramer(maxBytes),
		maxBytes: maxBytes,
	}
}

func (f *SSEChunkFramer) PushChunk(chunk string, visit func(string) (bool, error)) (bool, error) {
	if f == nil || f.framer == nil {
		return false, errors.New("Responses SSE event framer is required")
	}
	for len(chunk) > 0 {
		if f.afterCR {
			f.afterCR = false
			if chunk[0] == '\n' {
				chunk = chunk[1:]
				// CR 已完成分帧；跨 chunk 的 LF 只补交 wire 字节。
				if f.framer.BufferedLen() == 0 {
					if stop, err := visit("\n"); stop || err != nil {
						f.Reset()
						return stop, err
					}
				} else {
					if f.wouldExceedLimit(1) {
						f.Reset()
						return false, requester.ErrSSEEventTooLarge
					}
					f.pendingLine.WriteByte('\n')
				}
				continue
			}
		}
		segment, rest := requester.SplitSSELine(chunk)
		if !strings.HasSuffix(segment, "\r") && !strings.HasSuffix(segment, "\n") {
			if f.wouldExceedLimit(len(chunk)) {
				f.Reset()
				return false, requester.ErrSSEEventTooLarge
			}
			_, _ = f.pendingLine.WriteString(chunk)
			break
		}

		chunk = rest
		f.afterCR = strings.HasSuffix(segment, "\r")
		if f.wouldExceedLimit(len(segment)) {
			f.Reset()
			return false, requester.ErrSSEEventTooLarge
		}
		line := segment
		if f.pendingLine.Len() > 0 {
			_, _ = f.pendingLine.WriteString(segment)
			line = f.pendingLine.String()
		}
		event, complete, err := f.framer.PushLine([]byte(line))
		f.pendingLine.Reset()
		if err != nil {
			f.Reset()
			return false, err
		}
		if !complete {
			continue
		}
		stop, err := visit(string(event))
		if err != nil || stop {
			f.Reset()
			return stop, err
		}
	}
	return false, nil
}

func (f *SSEChunkFramer) wouldExceedLimit(additionalBytes int) bool {
	if f == nil || f.maxBytes <= 0 {
		return false
	}
	bufferedBytes := f.pendingLine.Len()
	if f.framer != nil {
		bufferedBytes += f.framer.BufferedLen()
	}
	return additionalBytes > f.maxBytes-bufferedBytes
}

func (f *SSEChunkFramer) HasPending() bool {
	return f != nil && (f.pendingLine.Len() > 0 || f.framer != nil && f.framer.BufferedLen() > 0)
}

func (f *SSEChunkFramer) Reset() {
	if f != nil {
		f.pendingLine.Reset()
		f.afterCR = false
		if f.framer != nil {
			f.framer.Reset()
		}
	}
}

func (f *SSEChunkFramer) TakePending() string {
	if f == nil {
		return ""
	}
	raw := f.framer.TakePending() + f.pendingLine.String()
	f.Reset()
	return raw
}

// EventStream 将原始传输与请求内证据观察接线；观察不授予交付许可。
type EventStream interface {
	requester.StreamReaderInterface[string]
	ObserveResponsesEvent(rawEvent string) error
}

func IgnoreResponsesEvent(string) error { return nil }

type streamWithObserver struct {
	requester.StreamReaderInterface[string]
	observe func(string) error
}

func NewEventStream(stream requester.StreamReaderInterface[string], observe func(string) error) EventStream {
	if stream == nil {
		return nil
	}
	return &streamWithObserver{
		StreamReaderInterface: stream,
		observe:               observe,
	}
}

func (s *streamWithObserver) ObserveResponsesEvent(rawEvent string) error {
	if s == nil || s.observe == nil {
		return errors.New("Responses stream evidence observer is required")
	}
	return s.observe(rawEvent)
}

func (s *streamWithObserver) CloseAndDrain() {
	if s != nil {
		requester.CloseAndDrainStream(s.StreamReaderInterface)
	}
}

func (s *streamWithObserver) CloseAndDrainContext(ctx context.Context) error {
	return requester.CloseAndDrainStreamContext(ctx, s.StreamReaderInterface)
}

func (s *streamWithObserver) ReadContext() context.Context {
	return requester.StreamReadContext(s.StreamReaderInterface)
}

// SSEDataPayload returns the logical data payload of one complete SSE event.
// It preserves the SSE rule that multiple data fields are joined with a LF.
func SSEDataPayload(rawEvent string) (string, bool) {
	var payload strings.Builder
	hasData := false
	for len(rawEvent) > 0 {
		line, rest := requester.SplitSSELine(rawEvent)
		rawEvent = rest
		line = requester.SSELineContent(line)
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		field, value, hasColon := strings.Cut(line, ":")
		if !hasColon {
			field = line
			value = ""
		}
		if field != "data" {
			continue
		}
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		if hasData {
			payload.WriteByte('\n')
		}
		payload.WriteString(value)
		hasData = true
	}
	return payload.String(), hasData
}
