package requester

import (
	"net/http"

	"one-api/types"
)

// RawSSEEventStream marks a string stream whose data items are complete SSE
// events rather than logical data-field payloads. The requester owns this
// delivery-shape contract; protocol interpretation remains with the provider
// handler and the downstream consumer.
type RawSSEEventStream interface {
	StreamReaderInterface[string]
	rawSSEEventStream()
}

type rawSSEEventStream struct {
	*streamReader[string]
}

func (*rawSSEEventStream) rawSSEEventStream() {}

// IsRawSSEEventStream reports whether each data item is one complete raw SSE
// event. Consumers that expect logical JSON payloads must explicitly unwrap
// the event data before interpreting it.
func IsRawSSEEventStream(stream StreamReaderInterface[string]) bool {
	_, ok := stream.(RawSSEEventStream)
	return ok
}

// RequestRawSSEEventStreamWithEmitterOptions atomically couples the raw-event
// marker with a no-trim line reader. The handler must emit only complete SSE
// events; using a dedicated constructor prevents a marker/reader mismatch.
func RequestRawSSEEventStreamWithEmitterOptions(
	httpRequester *HTTPRequester,
	resp *http.Response,
	handler HandlerPrefixWithEmitter[string],
	options StreamReadOptions,
) (RawSSEEventStream, *types.OpenAIErrorWithStatusCode) {
	stream, errWithCode := RequestNoTrimStreamWithEmitterOptions(httpRequester, resp, handler, options)
	if errWithCode != nil {
		return nil, errWithCode
	}
	return &rawSSEEventStream{streamReader: stream}, nil
}
