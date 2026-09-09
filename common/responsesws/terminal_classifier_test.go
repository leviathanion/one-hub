package responsesws

import "testing"

func TestClassifyResponsesWSEventTerminalCases(t *testing.T) {
	cases := []struct {
		name            string
		payload         string
		kind            ResponsesTerminalKind
		requestError    bool
		connectionError bool
		miss            bool
	}{
		{
			name:    "completed success without status",
			payload: `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_1"}}`,
			kind:    ResponsesSuccessTerminal,
		},
		{
			name:    "completed event wins over queued status",
			payload: `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_queued","status":"queued"}}`,
			kind:    ResponsesSuccessTerminal,
		},
		{
			name:    "completed event wins over in progress status",
			payload: `{"type":"response.completed","sequence_number":3,"response":{"id":"resp_in_progress","status":"in_progress"}}`,
			kind:    ResponsesSuccessTerminal,
		},
		{
			name:    "completed event wins over future status",
			payload: `{"type":"response.completed","sequence_number":4,"response":{"id":"resp_future","status":"future_terminal_status"}}`,
			kind:    ResponsesSuccessTerminal,
		},
		{
			name:    "completed with future output quality union",
			payload: `{"type":"response.completed","sequence_number":5,"response":{"id":"resp_future_quality","status":"completed","output":[{"type":"image_generation_call","id":"img_future","status":"completed","quality":{"future":"quality"}}]}}`,
			kind:    ResponsesSuccessTerminal,
		},
		{
			name:    "failed lifecycle event",
			payload: `{"type":"response.failed","sequence_number":6,"response":{"id":"resp_1","status":"failed"}}`,
			kind:    ResponsesFailedTerminal,
		},
		{
			name:    "incomplete lifecycle event",
			payload: `{"type":"response.incomplete","sequence_number":7,"response":{"id":"resp_1","status":"incomplete"}}`,
			kind:    ResponsesFailedTerminal,
		},
		{
			name:    "completed with response error",
			payload: `{"type":"response.completed","sequence_number":8,"response":{"id":"resp_1","error":{"code":"bad","message":"bad request"}}}`,
			kind:    ResponsesFailedTerminal,
		},
		{
			name:    "completed with cancelled response status",
			payload: `{"type":"response.completed","sequence_number":9,"response":{"id":"resp_cancelled","status":"cancelled"}}`,
			kind:    ResponsesFailedTerminal,
		},
		{
			name:         "continuation miss request error",
			payload:      `{"type":"error","error":{"message":"previous response was not found"}}`,
			kind:         ResponsesNonTerminal,
			requestError: true,
			miss:         true,
		},
		{
			name:            "connection lifetime error",
			payload:         `{"type":"error","error":{"code":"websocket_connection_limit_reached","message":"connection expired"}}`,
			kind:            ResponsesNonTerminal,
			connectionError: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyResponsesWSEvent([]byte(tc.payload))
			if got.Kind != tc.kind {
				t.Fatalf("expected kind %d, got %d", tc.kind, got.Kind)
			}
			if got.ContinuationMiss != tc.miss {
				t.Fatalf("expected continuation miss %v, got %v", tc.miss, got.ContinuationMiss)
			}
			if got.RequestError != tc.requestError {
				t.Fatalf("expected request error %v, got %v", tc.requestError, got.RequestError)
			}
			if got.ConnectionError != tc.connectionError {
				t.Fatalf("expected connection error %v, got %v", tc.connectionError, got.ConnectionError)
			}
			if (tc.kind != ResponsesNonTerminal || tc.requestError || tc.connectionError) && got.EventType == "" {
				t.Fatal("expected event type to be recorded")
			}
		})
	}
}

func TestClassifyResponsesWSEventRejectsRealtimeTerminalDialect(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.done","response":{"id":"resp_1","status":"completed"}}`,
		`{"type":"response.cancelled","response":{"id":"resp_1","status":"cancelled"}}`,
		`{"type":"response.canceled","response":{"id":"resp_1","status":"cancelled"}}`,
	} {
		got := ClassifyResponsesWSEvent([]byte(payload))
		if !got.Malformed || got.Kind != ResponsesFailedTerminal || got.MalformedError == "" {
			t.Fatalf("expected Realtime terminal dialect to be malformed, payload=%s got=%+v", payload, got)
		}
	}
}

func TestClassifyResponsesWSEventRequiresTerminalIdentityAndSequence(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.completed"}`,
		`{"type":"response.completed","sequence_number":1,"response":null}`,
		`{"type":"response.completed","sequence_number":1,"response":"opaque"}`,
		`{"type":"response.completed","sequence_number":1,"response":{}}`,
		`{"type":"response.completed","response":{"id":"resp_1"}}`,
		`{"type":"response.completed","sequence_number":null,"response":{"id":"resp_1"}}`,
		`{"type":"response.completed","sequence_number":"1","response":{"id":"resp_1"}}`,
		`{"type":"response.completed","sequence_number":1.5,"response":{"id":"resp_1"}}`,
		`{"type":"response.completed","sequence_number":-1,"response":{"id":"resp_1"}}`,
	} {
		got := ClassifyResponsesWSEvent([]byte(payload))
		if !got.Malformed || got.Kind != ResponsesFailedTerminal || got.MalformedError == "" {
			t.Fatalf("expected invalid terminal identity/sequence to be malformed, payload=%s got=%+v", payload, got)
		}
	}

	got := ClassifyResponsesWSEvent([]byte(`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1"}}`))
	if got.Malformed || got.Kind != ResponsesNonTerminal || !got.HasSequenceNumber || got.SequenceNumber != 0 {
		t.Fatalf("expected non-terminal sequence metadata, got %+v", got)
	}

	future := ClassifyResponsesWSEvent([]byte(`{"type":"response.future","response":"opaque"}`))
	if future.Malformed || future.Kind != ResponsesNonTerminal {
		t.Fatalf("expected future event with opaque response to remain passthrough non-terminal, got %+v", future)
	}
}

func TestClassifyResponsesWSEventKeepsTerminalIndependentFromUsage(t *testing.T) {
	got := ClassifyResponsesWSEvent([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","status":"completed","usage":"future-shape"}}`))
	if got.Malformed || got.Kind != ResponsesSuccessTerminal || got.Response == nil || got.Response.ID != "resp_1" || got.Response.Usage != nil {
		t.Fatalf("malformed usage changed lifecycle terminal: %+v", got)
	}
}
