package antigravity

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/providers/antigravity/wire"
)

func TestReplaceSSEDataPreservesLineEndingsAndMetadata(t *testing.T) {
	payload := "{\n\"text\":\"你好\"\n}"
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		input := "id: 17" + ending + ": keep" + ending + "event: message" + ending + "data: old" + ending + "retry: 50" + ending + "data: tail" + ending + ending
		want := "id: 17" + ending + ": keep" + ending + "event: message" + ending + "retry: 50" + ending + "data: " + strings.ReplaceAll(payload, "\n", ending+"data: ") + ending + ending
		got := string(replaceAntigravitySSEData(input, []byte(payload)))
		if got != want {
			t.Fatalf("ending=%q\ngot %q\nwant %q", ending, got, want)
		}
		if data, present := commonresponses.SSEDataPayload(got); !present || data != payload {
			t.Fatalf("payload changed: %q", data)
		}
	}
	// Non-data fields keep their bytes/order; payload occupies the last data line.
	if got := string(replaceAntigravitySSEData("id: 1\r\ndata: old\r: keep\ndata: tail\n\r", []byte("a\nb"))); got != "id: 1\r\n: keep\ndata: a\ndata: b\n\r" {
		t.Fatalf("mixed event changed: %q", got)
	}
	if got := string(replaceAntigravitySSEData("data: old", []byte("a\nb"))); got != "data: a\ndata: b" {
		t.Fatalf("unterminated event acquired a terminator: %q", got)
	}
	if got := string(replaceAntigravitySSEData("data: old\r: keep\ndata: tail", []byte("a\nb"))); got != ": keep\ndata: a\ndata: b" {
		t.Fatalf("unterminated data changed metadata or acquired a terminator: %q", got)
	}
}

func TestNativeSSEMixedEndingsKeepEventBoundaries(t *testing.T) {
	inner := `{"candidates":[{"content":{"parts":[{"text":"你好"}]},"finishReason":"STOP"}]}`
	usage := `{"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`
	for _, first := range []string{"\n", "\r\n", "\r"} {
		for _, last := range []string{"\n", "\r\n", "\r"} {
			for _, blank := range []string{"\n", "\r\n", "\r"} {
				if last == "\r" && blank == "\n" {
					continue // This is one CRLF, not a complete SSE event.
				}
				for _, metadata := range []string{"", "id: 7\r: keep\nevent: message\r\nretry: 50\r"} {
					input := "data: {\"response\":" + first + metadata + "data: " + inner + "}" + last + blank
					input += "data: {\"response\":" + usage + "}\n\n"
					for cut := 0; cut <= len(input); cut++ {
						chunks := []string{input[:cut], input[cut:]}
						if cut == len(input) {
							// Split strings by bytes too, including within UTF-8 characters.
							chunks = make([]string, 0, len(input))
							for i := 0; i < len(input); i++ {
								chunks = append(chunks, input[i:i+1])
							}
						}
						stream, apiErr := newAntigravityNativeStream(nil, &http.Response{Body: io.NopCloser(&splitAntigravityReader{chunks})}, wire.NewNativeDecoder(1), nil, nil)
						if apiErr != nil {
							t.Fatal(apiErr)
						}
						frames, failures := collectAntigravityStream(t, stream)
						output := strings.Join(frames, "")
						if len(failures) != 1 || !errors.Is(failures[0], io.EOF) || !strings.Contains(output, metadata) {
							t.Fatalf("first=%q last=%q blank=%q cut=%d failures=%v output=%q metadata=%q", first, last, blank, cut, failures, output, metadata)
						}
						// Frames emitted by the provider must remain complete on the wire.
						framer := requester.NewSSEEventFramer(wire.MaxRetainedBytes)
						var payloads []string
						for output != "" {
							line, rest := requester.SplitSSELine(output)
							output = rest
							event, complete, err := framer.PushLine([]byte(line))
							if err != nil {
								t.Fatal(err)
							}
							if complete {
								payload, _ := commonresponses.SSEDataPayload(string(event))
								payloads = append(payloads, payload)
							}
						}
						if framer.BufferedLen() != 0 || len(payloads) != 2 || payloads[0] != inner || payloads[1] != usage {
							t.Fatalf("event boundaries changed: pending=%d payloads=%q", framer.BufferedLen(), payloads)
						}
					}
				}
			}
		}
	}
}

type splitAntigravityReader struct{ chunks []string }

func (r *splitAntigravityReader) Read(p []byte) (int, error) {
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

func TestNativeSSEUnwrapIsIndependentOfNetworkChunking(t *testing.T) {
	inner := "{\n\"future\":9007199254740993,\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"你好\"}]},\"finishReason\":\"STOP\"}]\n}"
	usage := `{"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		data := func(payload string) string {
			return "data: " + strings.ReplaceAll(payload, "\n", ending+"data: ") + ending + ending
		}
		metadata := "id: id-1" + ending + "event: message" + ending + "retry: 100" + ending
		comment := ": keepalive" + ending + ending
		input := comment + metadata + data(`{"response":`+inner+`}`) + data(`{"response":`+usage+`}`)
		want := comment + metadata + data(inner) + data(usage)
		for cut := 0; cut <= len(input); cut++ {
			chunks := []string{input[:cut], input[cut:]}
			if cut == len(input) {
				chunks = nil
				// Byte boundaries also split multi-byte UTF-8.
				for i := 0; i < len(input); i++ {
					chunks = append(chunks, input[i:i+1])
				}
			}
			stream, apiErr := newAntigravityNativeStream(nil, &http.Response{Body: io.NopCloser(&splitAntigravityReader{chunks})}, wire.NewNativeDecoder(1), nil, nil)
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			frames, failures := collectAntigravityStream(t, stream)
			if len(failures) != 1 || !errors.Is(failures[0], io.EOF) || strings.Join(frames, "") != want {
				t.Fatalf("ending=%q cut=%d failures=%v\ngot %q\nwant %q", ending, cut, failures, strings.Join(frames, ""), want)
			}
		}
		// A missing event separator remains truncation even if finishReason was received.
		truncated := strings.TrimSuffix(input, ending)
		stream, apiErr := newAntigravityNativeStream(nil, &http.Response{Body: io.NopCloser(strings.NewReader(truncated))}, wire.NewNativeDecoder(1), nil, nil)
		if apiErr != nil {
			t.Fatal(apiErr)
		}
		_, failures := collectAntigravityStream(t, stream)
		if len(failures) != 1 || !errors.Is(failures[0], io.ErrUnexpectedEOF) {
			t.Fatalf("truncation became success: %v", failures)
		}
	}
}
