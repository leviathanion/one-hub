package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	providersBase "one-api/providers/base"
)

func TestStoredResponseStreamOutlivesRelayTimeout(t *testing.T) {
	const event = "event: future_extension\ndata: {\"opaque\":[null,12345678901234567890]}\n\n"
	const rawQuery = "stream=true&starting_after=7&future=%2f&future=two"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/responses/resp_stream" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.URL.RawQuery != rawQuery && r.URL.RawQuery != "stream=false" {
			t.Errorf("raw query changed: %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 9; i++ {
			if i > 0 {
				select {
				case <-time.After(20 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
			_, _ = io.WriteString(w, event)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(server.Close)
	originalClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	requester.HTTPClient.Timeout = 80 * time.Millisecond
	t.Cleanup(func() { requester.HTTPClient = originalClient })

	for _, exactWire := range []bool{false, true} {
		name := "compatible"
		if exactWire {
			name = "exact wire"
		}
		t.Run(name, func(t *testing.T) {
			proxy := ""
			provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, server.URL)
			provider.ProviderRawJSONReplay = exactWire
			for _, stream := range []bool{true, false} {
				query := "stream=false"
				if stream {
					query = rawQuery
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				response, apiErr := provider.RelayStoredResponse(ctx, providersBase.StoredResponsesRequest{
					Operation: providersBase.OperationResponsesRetrieve, ResponseID: "resp_stream", RawQuery: query,
				})
				if apiErr != nil {
					cancel()
					t.Fatalf("open stored response stream=%v: %+v", stream, apiErr)
				}
				body, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				cancel()
				if stream {
					if err != nil || string(body) != strings.Repeat(event, 9) {
						t.Fatalf("restored stream truncated or changed: body=%q err=%v", body, err)
					}
				} else if err == nil {
					t.Fatal("stream request changed the ordinary request timeout")
				}
			}
		})
	}
	if calls.Load() != 4 {
		t.Fatalf("unexpected retry: upstream calls=%d, want 4", calls.Load())
	}
}
