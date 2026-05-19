package responsesws

import (
	"encoding/json"
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

func TestRawResponsesCreateFrameCodexNestedPayloadPreservesUnknownValues(t *testing.T) {
	frame, err := ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","event_id":"evt_1","model":"gpt-5","input":"hi","generate":true}`))
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	nested, err := frame.CodexNestedPayload("gpt-5-mini")
	if err != nil {
		t.Fatalf("build nested payload: %v", err)
	}
	var got struct {
		Type     string                     `json:"type"`
		EventID  string                     `json:"event_id"`
		Response map[string]json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(nested, &got); err != nil {
		t.Fatalf("decode nested payload: %v", err)
	}
	if got.Type != "response.create" || got.EventID != "evt_1" {
		t.Fatalf("unexpected envelope: %+v", got)
	}
	if string(got.Response["model"]) != `"gpt-5-mini"` || string(got.Response["generate"]) != `true` {
		t.Fatalf("expected model rewrite and unknown field preservation, got %+v", got.Response)
	}
	if _, exists := got.Response["type"]; exists {
		t.Fatal("did not expect event type to be nested into response payload")
	}
}
