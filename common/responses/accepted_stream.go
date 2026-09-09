package responses

import (
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
		lineEnd := strings.IndexByte(chunk, '\n')
		if lineEnd < 0 {
			if f.wouldExceedLimit(len(chunk)) {
				f.Reset()
				return false, requester.ErrSSEEventTooLarge
			}
			_, _ = f.pendingLine.WriteString(chunk)
			break
		}

		segment := chunk[:lineEnd+1]
		chunk = chunk[lineEnd+1:]
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
		if f.framer != nil {
			f.framer.Reset()
		}
	}
}

// EventStream couples Responses transport with its request-local accounting
// observer. Every consumer must accept an event before provider accounting can
// advance, so missing accounting cannot silently degrade to a plain stream.
type EventStream interface {
	requester.StreamReaderInterface[string]
	ObserveAcceptedResponsesEvent(rawEvent string) error
}

func IgnoreAcceptedResponsesEvent(string) error { return nil }

type streamWithAcceptedEventCommitter struct {
	requester.StreamReaderInterface[string]
	commit func(string) error
}

func NewEventStream(stream requester.StreamReaderInterface[string], observe func(string) error) EventStream {
	if stream == nil {
		return nil
	}
	return &streamWithAcceptedEventCommitter{
		StreamReaderInterface: stream,
		commit:                observe,
	}
}

func (s *streamWithAcceptedEventCommitter) ObserveAcceptedResponsesEvent(rawEvent string) error {
	if s == nil || s.commit == nil {
		return errors.New("Responses stream accepted-event observer is required")
	}
	return s.commit(rawEvent)
}

func (s *streamWithAcceptedEventCommitter) CloseAndDrain() {
	if s != nil {
		requester.CloseAndDrainStream(s.StreamReaderInterface)
	}
}

// SSEDataPayload returns the logical data payload of one complete SSE event.
// It preserves the SSE rule that multiple data fields are joined with a LF.
func SSEDataPayload(rawEvent string) (string, bool) {
	var payload strings.Builder
	hasData := false
	for len(rawEvent) > 0 {
		lineEnd := strings.IndexByte(rawEvent, '\n')
		line := rawEvent
		if lineEnd >= 0 {
			line = rawEvent[:lineEnd]
			rawEvent = rawEvent[lineEnd+1:]
		} else {
			rawEvent = ""
		}
		line = strings.TrimSuffix(line, "\r")
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
