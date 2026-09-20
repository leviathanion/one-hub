package responsesws

import "testing"

func TestResponsesOpaqueFrameIdentity(t *testing.T) {
	frame, err := ParseRawResponsesCreateFrame([]byte(`{"type":" response.create ","model":"gpt-5","event_id":" Event ID ","previous_response_id":" Resp ID "}`))
	if err != nil || frame.EventID != " Event ID " || frame.Projection.PreviousResponseID != " Resp ID " {
		t.Fatalf("client identity changed: %+v %v", frame, err)
	}
	envelope, err := ParseProviderEventEnvelope([]byte(`{"type":" response.created ","event_id":" Event ID ","response":{"id":" Resp ID "}}`))
	if err != nil || envelope.Type != "response.created" || envelope.EventID != " Event ID " {
		t.Fatalf("event identity changed: %+v %v", envelope, err)
	}
	if id := ProviderResponseID([]byte(`{"type":"response.created","response_id":" Resp ID ","response":{"id":" Resp ID "}}`)); id != " Resp ID " {
		t.Fatalf("raw response ID changed: %q", id)
	}
	for _, raw := range []string{
		`{"type":"response.created","response_id":"Resp ID","response":{"id":" Resp ID "}}`,
		`{"type":"response.created","response_id":" resp id ","response":{"id":" Resp ID "}}`,
	} {
		if id := ProviderResponseID([]byte(raw)); id != "" {
			t.Fatalf("different opaque IDs treated as equal: %q", id)
		}
	}
}
