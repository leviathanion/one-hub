package replicate

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

type replicateProfileRoundTripper func(*http.Request) (*http.Response, error)

func (f replicateProfileRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestReplicateOnlySSEGETEscapesWorkActionTimeout(t *testing.T) {
	originalClient := requester.HTTPClient
	t.Cleanup(func() { requester.HTTPClient = originalClient })

	baseURL, proxy := "https://replicate.test", ""
	provider := ReplicateProviderFactory{}.Create(&model.Channel{BaseURL: &baseURL, Proxy: &proxy, Key: "key"}).(*ReplicateProvider)
	provider.SetUsage(&types.Usage{})
	request := &types.ChatCompletionRequest{Model: "owner/model", Stream: true}

	t.Run("creation POST remains WorkAction", func(t *testing.T) {
		requester.HTTPClient = &http.Client{
			Timeout: 10 * time.Millisecond,
			Transport: replicateProfileRoundTripper(func(req *http.Request) (*http.Response, error) {
				select {
				case <-time.After(40 * time.Millisecond):
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
			}),
		}
		stream, apiErr := provider.CreateChatCompletionStream(request)
		if apiErr == nil || stream != nil {
			t.Fatalf("Replicate creation POST escaped WorkAction timeout: stream=%v err=%+v", stream, apiErr)
		}
	})

	t.Run("SSE GET uses LongStream", func(t *testing.T) {
		requester.HTTPClient = &http.Client{
			Timeout: 10 * time.Millisecond,
			Transport: replicateProfileRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"id":"pred_1","status":"starting","urls":{"stream":"https://replicate-stream.test/events"}}`)),
						Request:    req,
					}, nil
				}
				select {
				case <-time.After(40 * time.Millisecond):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(strings.NewReader("")),
						Request:    req,
					}, nil
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
			}),
		}
		stream, apiErr := provider.CreateChatCompletionStream(request)
		if apiErr != nil || stream == nil {
			t.Fatalf("Replicate SSE GET inherited WorkAction timeout: stream=%v err=%+v", stream, apiErr)
		}
		stream.Close()
	})
}

func TestReplicateTerminalLifecycleDoesNotTurnFailureIntoSuccess(t *testing.T) {
	for _, status := range []string{"failed", "canceled", "aborted"} {
		t.Run(status, func(t *testing.T) {
			usage := &types.Usage{}
			handler := &ReplicateStreamHandler{
				Usage:     usage,
				ModelName: "model",
				ID:        "pred_1",
				framer:    requester.NewSSEEventFramer(1024),
				fetchPrediction: func() *ReplicateResponse[[]string] {
					return &ReplicateResponse[[]string]{ID: "pred_1", Status: status, Error: "provider " + status}
				},
			}
			stream, apiErr := requester.RequestNoTrimStreamWithEmitterOptions[string](nil, &http.Response{
				Body: io.NopCloser(strings.NewReader("event: done\ndata: {}\n\n")),
			}, handler.HandlerChatStreamWithEmitter, requester.StreamReadOptions{RequireProtocolTerminal: true})
			if apiErr != nil {
				t.Fatalf("create stream: %+v", apiErr)
			}
			data, streamErrors := stream.Recv()
			defer stream.Close()
			for data != nil || streamErrors != nil {
				select {
				case chunk, ok := <-data:
					if !ok {
						data = nil
						continue
					}
					if chunk != "" {
						t.Fatalf("%s prediction emitted a success chunk: %s", status, chunk)
					}
				case err, ok := <-streamErrors:
					if !ok {
						streamErrors = nil
						continue
					}
					if err == nil || errors.Is(err, io.EOF) || !strings.Contains(err.Error(), status) {
						t.Fatalf("%s prediction error=%v", status, err)
					}
					streamErrors = nil
				case <-time.After(time.Second):
					t.Fatalf("timed out waiting for %s terminal", status)
				}
			}
			if usage.ProviderReported {
				t.Fatalf("failed prediction produced usage evidence: %+v", usage)
			}
		})
	}
}

func TestReplicateSucceededTerminalEmitsStopWithProviderUsage(t *testing.T) {
	usage := &types.Usage{}
	handler := &ReplicateStreamHandler{
		Usage:     usage,
		ModelName: "model",
		ID:        "pred_1",
		framer:    requester.NewSSEEventFramer(1024),
		fetchPrediction: func() *ReplicateResponse[[]string] {
			var metrics ReplicateMetrics
			if err := json.Unmarshal([]byte(`{"input_token_count":2,"output_token_count":3}`), &metrics); err != nil {
				t.Fatalf("decode provider metrics: %v", err)
			}
			return &ReplicateResponse[[]string]{ID: "pred_1", Status: "succeeded", Metrics: metrics}
		},
	}
	stream, apiErr := requester.RequestNoTrimStreamWithEmitterOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader("event: done\ndata: {}\n\n"))}, handler.HandlerChatStreamWithEmitter, requester.StreamReadOptions{RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatalf("create stream: %+v", apiErr)
	}
	data, streamErrors := stream.Recv()
	defer stream.Close()
	select {
	case chunk := <-data:
		if !strings.Contains(chunk, `"finish_reason":"stop"`) {
			t.Fatalf("successful prediction terminal changed: %s", chunk)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for success terminal")
	}
	if err := <-streamErrors; !errors.Is(err, io.EOF) {
		t.Fatalf("success terminal error=%v", err)
	}
	if !usage.ProviderReported || usage.TotalTokens != 5 {
		t.Fatalf("provider usage not recorded: %+v", usage)
	}
	if usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 {
		t.Fatalf("successful prediction did not produce a provider operation unit: %+v", usage)
	}
}

func TestReplicateInitialCanceledAndAbortedAreTerminal(t *testing.T) {
	for _, status := range []string{"canceled", "aborted"} {
		if response, err := getPrediction[[]string](nil, &ReplicateResponse[[]string]{Status: status}); err == nil || response != nil {
			t.Fatalf("status %s was not terminal failure: response=%+v err=%v", status, response, err)
		}
	}
}

func TestReplicateSucceededWithoutTokenMetricsOnlyAuthorizesOperationUnit(t *testing.T) {
	usage := &types.Usage{}
	applyReplicateSucceededEvidence(usage, "replicate-model", ReplicateMetrics{})
	if usage.ProviderReported || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 {
		t.Fatalf("missing Replicate token metrics were fabricated: %+v", usage)
	}
}
