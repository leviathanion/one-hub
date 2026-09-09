package openai

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/requester"
	"one-api/types"
)

func TestResponsesChatSSEWaitsForCompleteEventsBeforeBilling(t *testing.T) {
	handler := &OpenAIResponsesStreamHandler{Usage: &types.Usage{}}
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":99,\"output_tokens\":99,\"total_tokens\":198}}}\n"
	stream, apiErr := requester.RequestNoTrimStreamWithOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, handler.ChatSSEHandler(handler.ObserveAcceptedResponsesEvent), requester.StreamReadOptions{RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	defer requester.CloseAndDrainStream[string](stream)
	data, errs := stream.Recv()
	var delivered strings.Builder
	var failure error
	for data != nil || errs != nil {
		select {
		case value, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			delivered.WriteString(value)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			failure = err
		}
	}
	if !errors.Is(failure, requester.ErrStreamProtocolTerminalMissing) || handler.Usage.TotalTokens != 0 || strings.Contains(delivered.String(), `"finish_reason":"stop"`) {
		t.Fatalf("partial Responses terminal changed Chat or billing: err=%v usage=%+v body=%s", failure, handler.Usage, delivered.String())
	}
}

func TestResponsesChatSSEStopsBeforeRejectedAccountingAndLaterEvents(t *testing.T) {
	handler := &OpenAIResponsesStreamHandler{Usage: &types.Usage{}}
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"rejected\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n"
	calls := 0
	observe := func(event string) error {
		calls++
		if strings.Contains(event, "rejected") {
			return common.StringErrorWrapperLocal("tracking failed", "provider_usage_state_limit", 502)
		}
		return handler.ObserveAcceptedResponsesEvent(event)
	}
	stream, apiErr := requester.RequestNoTrimStreamWithOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, handler.ChatSSEHandler(observe), requester.StreamReadOptions{RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	defer requester.CloseAndDrainStream[string](stream)
	data, errs := stream.Recv()
	var delivered strings.Builder
	errorCount := 0
	for data != nil || errs != nil {
		select {
		case value, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			delivered.WriteString(value)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			errorCount++
			var failure *types.OpenAIErrorWithStatusCode
			if !errors.As(err, &failure) || failure.Code != "provider_usage_state_limit" {
				t.Fatalf("unexpected failure: %v", err)
			}
		}
	}
	if calls != 2 || errorCount != 1 || strings.Contains(delivered.String(), "rejected") || strings.Contains(delivered.String(), "late") {
		t.Fatalf("accounting rejection did not stop the stream: calls=%d errors=%d body=%s", calls, errorCount, delivered.String())
	}
}
