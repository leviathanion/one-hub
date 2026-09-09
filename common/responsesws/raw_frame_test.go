package responsesws

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParseRawResponsesCreateFrameRejectsDuplicateTopLevelKey(t *testing.T) {
	if _, err := ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","model":"gpt-4"}`)); err == nil {
		t.Fatal("expected duplicate top-level key to be rejected")
	}
}

func TestParseRawResponsesCreateFrameRejectsTrailingData(t *testing.T) {
	if _, err := ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5"} trailing`)); err == nil {
		t.Fatal("expected trailing data to be rejected")
	}
}

func TestRawResponsesCreateFrameCloneForModelPreservesUnknownValues(t *testing.T) {
	frame, err := ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi","generate":true,"unknown_number":12345678901234567890,"metadata":{"trace":"abc"}}`))
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	cloned, err := frame.CloneForModel("gpt-5-mini")
	if err != nil {
		t.Fatalf("clone frame: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(cloned, &got); err != nil {
		t.Fatalf("decode cloned frame: %v", err)
	}
	if string(got["model"]) != `"gpt-5-mini"` {
		t.Fatalf("expected rewritten model, got %s", got["model"])
	}
	if string(got["generate"]) != `true` {
		t.Fatalf("expected generate to be preserved, got %s", got["generate"])
	}
	if string(got["unknown_number"]) != `12345678901234567890` {
		t.Fatalf("expected numeric raw value to be preserved, got %s", got["unknown_number"])
	}
}

func TestRawResponsesCreateFrameCloneForSameModelReturnsRawFrame(t *testing.T) {
	raw := []byte(` { "type" : "response.create", "model" : "gpt-5", "input" : {"text":"hi"}, "generate" : true } `)
	frame, err := ParseRawResponsesCreateFrame(raw)
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	cloned, err := frame.CloneForModel("gpt-5")
	if err != nil {
		t.Fatalf("clone frame: %v", err)
	}
	if string(cloned) != string(frame.Raw) {
		t.Fatalf("expected no-op model clone to return raw frame\nwant: %s\n got: %s", string(frame.Raw), string(cloned))
	}
}

func TestParseRawResponsesCreateFrameKeepsFutureProviderUnionRaw(t *testing.T) {
	raw := []byte(`{"type":"response.create","model":"gpt-5","input":"hi","tools":[{"type":"future_tool","max_num_results":"future-shape"}]}`)
	frame, err := ParseRawResponsesCreateFrame(raw)
	if err != nil {
		t.Fatalf("parse future frame: %v", err)
	}
	if frame.Projection.Model != "gpt-5" || string(frame.Raw) != string(raw) {
		t.Fatalf("unexpected projected future frame: %+v raw=%s", frame.Projection, frame.Raw)
	}
}

func TestParseRawResponsesCreateFrameDoesNotValidateProviderOwnedMultiAgentShape(t *testing.T) {
	raw := []byte(`{"type":"response.create","model":"gpt-5","input":"hi","multi_agent":{"enabled":"provider-future","future":true}}`)
	frame, err := ParseRawResponsesCreateFrame(raw)
	if err != nil {
		t.Fatalf("provider-owned multi_agent shape must remain upstream validation: %v", err)
	}
	if frame.MultiAgentEnabled {
		t.Fatal("unconfirmed local multi-agent semantics must remain disabled")
	}
	if string(frame.Object["multi_agent"]) != `{"enabled":"provider-future","future":true}` {
		t.Fatalf("expected raw multi_agent payload to remain unchanged, got %s", frame.Object["multi_agent"])
	}
}

func TestValidateProviderEventPayloadRequiresObjectWithNonEmptyType(t *testing.T) {
	for _, raw := range []string{
		`{"foo":1}`,
		`{"type":""}`,
		`{"type":null}`,
		`[]`,
	} {
		t.Run(raw, func(t *testing.T) {
			if err := ValidateProviderEventPayload([]byte(raw)); !errors.Is(err, ErrInvalidProviderEventPayload) {
				t.Fatalf("expected invalid provider event payload, got %v", err)
			}
		})
	}
	if err := ValidateProviderEventPayload([]byte(`{"type":"response.future","payload":{"unknown":true}}`)); err != nil {
		t.Fatalf("expected future typed provider event to pass minimum schema: %v", err)
	}
	envelope, err := ParseProviderEventEnvelope([]byte(`{"type":"response.future","event_id":"evt_future","payload":{"unknown":true}}`))
	if err != nil {
		t.Fatalf("parse provider event envelope: %v", err)
	}
	if envelope.Type != "response.future" || envelope.EventID != "evt_future" || string(envelope.Object["payload"]) != `{"unknown":true}` {
		t.Fatalf("unexpected provider event envelope: %+v", envelope)
	}
}

func TestValidateClientEventPayloadRequiresStrictEnvelope(t *testing.T) {
	for _, raw := range []string{
		`{"foo":1}`,
		`{"type":""}`,
		`{"type":null}`,
		`[]`,
		`{"type":"response.create","model":"gpt-5","type":"response.cancel"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if err := ValidateClientEventPayload([]byte(raw)); !errors.Is(err, ErrInvalidClientEventPayload) {
				t.Fatalf("expected invalid client event payload, got %v", err)
			}
		})
	}
	envelope, err := ParseClientEventEnvelope([]byte(`{"type":"response.cancel","event_id":"evt_cancel","future":{"keep":true}}`))
	if err != nil {
		t.Fatalf("parse client event envelope: %v", err)
	}
	if envelope.Type != "response.cancel" || envelope.EventID != "evt_cancel" || string(envelope.Object["future"]) != `{"keep":true}` {
		t.Fatalf("unexpected client event envelope: %+v", envelope)
	}
}
