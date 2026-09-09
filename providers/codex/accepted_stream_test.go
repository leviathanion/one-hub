package codex

import (
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/types"
)

func TestCodexCollectorIDLessPrefixPreventsDefinitiveRejection(t *testing.T) {
	for _, prefix := range []bool{false, true} {
		t.Run(fmt.Sprint(prefix), func(t *testing.T) {
			handler := newCodexResponsesStreamHandler(&types.Usage{})
			body := ": keepalive\n\n"
			if prefix {
				body += "data: {\"type\":\"future.event\"}\n\n"
			}
			body += "data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"upstream error\"}\n\n"
			rawStream, apiErr := requester.RequestNoTrimStreamWithEmitterOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{})
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			_, apiErr = (&CodexProvider{}).collectResponsesStreamResponse(commonresponses.NewEventStream(rawStream, handler.ObserveAcceptedResponsesEvent))
			if apiErr == nil || apiErr.UpstreamAccepted != prefix {
				t.Fatalf("incorrect prefix acceptance: prefix=%t err=%+v", prefix, apiErr)
			}
		})
	}
}

func TestCodexTerminalToolFailureDoesNotCommitImageOrTokenState(t *testing.T) {
	a := newCodexTurnUsageAccumulator()
	if err := a.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: "response.created", Response: &types.OpenAIResponsesResponses{Usage: &types.ResponsesUsage{InputTokens: 3}}}); err != nil {
		t.Fatal(err)
	}
	beforeImage := a.imageTracker
	beforeUsage := cloneCodexResponsesUsage(a.observedResponsesUsage)
	outputs := []types.ResponsesOutput{{ID: "img_1", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "high", Size: "1024x1024"}}
	for i := 0; i < 1025; i++ {
		outputs = append(outputs, types.ResponsesOutput{ID: fmt.Sprintf("ws_%d", i), Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}})
	}
	err := a.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: "response.completed", Response: &types.OpenAIResponsesResponses{ID: "resp_1", Status: "completed", Output: outputs, Usage: &types.ResponsesUsage{InputTokens: 999}}})
	if err == nil || !reflect.DeepEqual(beforeImage, a.imageTracker) || !reflect.DeepEqual(beforeUsage, a.observedResponsesUsage) || len(a.toolUsage.ExtraBilling) != 0 {
		t.Fatalf("rejected terminal changed another accounting component: err=%v usage=%+v billing=%+v", err, a.observedResponsesUsage, a.toolUsage)
	}
}

func TestCodexCollectorIgnoresIncompleteBillingAndStopsAtTerminal(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprintf("incomplete=%t", incomplete), func(t *testing.T) {
			handler := newCodexResponsesStreamHandler(&types.Usage{})
			terminal := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n"
			if !incomplete {
				terminal += "\n" + "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"status\":\"completed\",\"usage\":{\"input_tokens\":999}}}\n\n"
			}
			rawStream, apiErr := requester.RequestNoTrimStreamWithEmitterOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(terminal))}, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{})
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			stream := commonresponses.NewEventStream(rawStream, handler.ObserveAcceptedResponsesEvent)
			response, apiErr := (&CodexProvider{}).collectResponsesStreamResponse(stream)
			if incomplete {
				if apiErr == nil || response != nil || handler.Usage.TotalTokens != 0 || handler.accumulator.observedResponsesUsage != nil {
					t.Fatalf("incomplete terminal entered accounting: response=%+v err=%v usage=%+v", response, apiErr, handler.Usage)
				}
			} else if apiErr != nil || response == nil || response.ID != "resp_1" || handler.Usage.TotalTokens != 5 {
				t.Fatalf("terminal did not close accepted prefix: response=%+v err=%v usage=%+v", response, apiErr, handler.Usage)
			}
		})
	}
}

func TestCodexCollectorPreservesProviderEventOverflow(t *testing.T) {
	previous := codexResponsesStreamMaxEventBytes
	codexResponsesStreamMaxEventBytes = 80
	t.Cleanup(func() { codexResponsesStreamMaxEventBytes = previous })
	handler := newCodexResponsesStreamHandler(&types.Usage{})
	body := "event: response.created\ndata: {\"type\":\"response.created\",\ndata: \"response\":{\"id\":\"resp_oversized\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"late\",\"status\":\"completed\",\"usage\":{\"input_tokens\":999}}}\n\n"
	rawStream, apiErr := requester.RequestNoTrimStreamWithEmitterOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	calls := 0
	stream := commonresponses.NewEventStream(rawStream, func(event string) error { calls++; return handler.ObserveAcceptedResponsesEvent(event) })
	response, apiErr := (&CodexProvider{}).collectResponsesStreamResponse(stream)
	if response != nil || apiErr == nil || apiErr.Code != "provider_usage_state_limit" || !apiErr.UpstreamAccepted || calls != 0 || handler.Usage.TotalTokens != 0 {
		t.Fatalf("provider event overflow changed classification or committed accounting: response=%+v err=%v calls=%d usage=%+v", response, apiErr, calls, handler.Usage)
	}
}
