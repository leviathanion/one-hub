package codex

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/common/requester"
	"one-api/types"
)

type codexProfileRoundTripper func(*http.Request) (*http.Response, error)

func (f codexProfileRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCodexResponsesStreamsDoNotInheritWorkActionOverallTimeout(t *testing.T) {
	originalClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{
		Timeout: 10 * time.Millisecond,
		Transport: codexProfileRoundTripper(func(req *http.Request) (*http.Response, error) {
			select {
			case <-time.After(40 * time.Millisecond):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}

			contentType := "text/event-stream"
			body := "event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_profile","object":"response","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
			if strings.HasSuffix(req.URL.Path, "/compact") {
				contentType = "application/json"
				body = `{"id":"resp_compact","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{contentType}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		}),
	}
	t.Cleanup(func() { requester.HTTPClient = originalClient })

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	request := &types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hello"}

	t.Run("outward unary aggregates provider SSE", func(t *testing.T) {
		response, apiErr := provider.CreateResponsesForTest(request)
		if apiErr != nil {
			t.Fatalf("CreateResponses inherited WorkAction timeout: %+v", apiErr)
		}
		if response == nil || response.ID != "resp_profile" {
			t.Fatalf("unexpected aggregated response: %#v", response)
		}
	})

	t.Run("outward stream opens provider SSE", func(t *testing.T) {
		stream, apiErr := provider.CreateResponsesStreamForTest(request)
		if apiErr != nil {
			t.Fatalf("CreateResponsesStream inherited WorkAction timeout: %+v", apiErr)
		}
		requester.CloseAndDrainStream(stream)
	})

	t.Run("compact remains a WorkAction", func(t *testing.T) {
		response, apiErr := provider.CompactResponsesForTest(request)
		if apiErr == nil {
			t.Fatalf("CompactResponses unexpectedly escaped the WorkAction timeout: %#v", response)
		}
	})
}
