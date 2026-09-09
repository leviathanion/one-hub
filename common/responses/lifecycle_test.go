package responses

import "testing"

func TestObserveEventLifecycleIgnoresMalformedUsageEvidence(t *testing.T) {
	event, err := ObserveEventLifecycle([]byte(`{"type":"response.completed","sequence_number":7,"response":{"id":"resp_1","status":"completed","model":"gpt-5","usage":"future-shape"}}`))
	if err != nil {
		t.Fatalf("observe lifecycle: %v", err)
	}
	if event.Type != "response.completed" || !event.HasSequence || event.Sequence != 7 {
		t.Fatalf("unexpected event facts: %+v", event)
	}
	if !event.ResponseObject || event.Response == nil || event.Response.ID != "resp_1" || event.Response.Status != "completed" {
		t.Fatalf("stable lifecycle facts were lost: %+v", event)
	}
	if event.Response.Usage != nil || event.ResponseFieldError != nil {
		t.Fatalf("malformed usage became lifecycle data/error: %+v", event)
	}
}

func TestObserveEventLifecycleKeepsUnknownOpaqueEventNonFatal(t *testing.T) {
	event, err := ObserveEventLifecycle([]byte(`{"type":"response.future","future":{"union":true},"response":"opaque"}`))
	if err != nil {
		t.Fatalf("observe future event: %v", err)
	}
	if event.Type != "response.future" || event.ResponseObject || event.Response != nil {
		t.Fatalf("future event was reinterpreted: %+v", event)
	}
}

func TestObserveEventLifecycleReportsLifecycleFieldErrorsOnly(t *testing.T) {
	event, err := ObserveEventLifecycle([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":42,"usage":{"input_tokens":1}}}`))
	if err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	if event.ResponseFieldError == nil {
		t.Fatalf("invalid response.id was not reported: %+v", event)
	}
	if _, err := ObserveEventLifecycle([]byte(`{"type":"response.created","type":"response.completed"}`)); err == nil {
		t.Fatal("duplicate top-level type was accepted")
	}
}
