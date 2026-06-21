package responsesws

import (
	"encoding/json"
	"errors"
	"strings"
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

func TestRawResponsesCreateFrameCloneWithDefaultPreviousResponseID(t *testing.T) {
	frame, err := ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	cloned, err := frame.CloneWithDefaultPreviousResponseID("resp_default")
	if err != nil {
		t.Fatalf("clone frame: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(cloned, &got); err != nil {
		t.Fatalf("decode cloned frame: %v", err)
	}
	if string(got["previous_response_id"]) != `"resp_default"` {
		t.Fatalf("expected default previous_response_id, got %s", got["previous_response_id"])
	}

	frame, err = ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","previous_response_id":"resp_client","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse frame with previous_response_id: %v", err)
	}
	cloned, err = frame.CloneWithDefaultPreviousResponseID("resp_default")
	if err != nil {
		t.Fatalf("clone frame with previous_response_id: %v", err)
	}
	if string(cloned) != string(frame.Raw) {
		t.Fatalf("expected client previous_response_id to be preserved\nwant: %s\n got: %s", string(frame.Raw), string(cloned))
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

func TestBuildResponsesHTTPBridgeBodyStripsEnvelopeAndForcesHTTPFields(t *testing.T) {
	frame, err := ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","event_id":"evt_1","model":"gpt-5","input":"hi","stream":true,"background":false,"stream_options":{"include_usage":true},"unknown_number":12345678901234567890,"future_object":{"enabled":true}}`))
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	body, err := BuildResponsesHTTPBridgeBody(frame.Object, "gpt-5-mini", "resp_default")
	if err != nil {
		t.Fatalf("build HTTP bridge body: %v", err)
	}

	for _, key := range []string{"type", "event_id", "background"} {
		if _, ok := body[key]; ok {
			t.Fatalf("expected %s to be stripped from HTTP bridge body, got %s", key, body[key])
		}
	}
	if string(body["stream_options"]) != `{"include_usage":true}` {
		t.Fatalf("expected stream_options to be preserved, got %s", body["stream_options"])
	}
	if string(body["model"]) != `"gpt-5-mini"` {
		t.Fatalf("expected normalized model, got %s", body["model"])
	}
	if string(body["stream"]) != `true` {
		t.Fatalf("expected HTTP bridge body to force stream=true, got %s", body["stream"])
	}
	if string(body["previous_response_id"]) != `"resp_default"` {
		t.Fatalf("expected default previous_response_id, got %s", body["previous_response_id"])
	}
	if string(body["unknown_number"]) != `12345678901234567890` {
		t.Fatalf("expected numeric raw value to be preserved, got %s", body["unknown_number"])
	}
	if string(body["future_object"]) != `{"enabled":true}` {
		t.Fatalf("expected unknown object raw value to be preserved, got %s", body["future_object"])
	}
}

func TestBuildResponsesHTTPBridgeBodyRejectsUnsupportedTransportFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		param string
	}{
		{name: "stream false", raw: `{"type":"response.create","model":"gpt-5","input":"hi","stream":false}`, param: "stream"},
		{name: "background true", raw: `{"type":"response.create","model":"gpt-5","input":"hi","background":true}`, param: "background"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := ParseRawResponsesCreateFrame([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parse frame: %v", err)
			}
			_, err = BuildResponsesHTTPBridgeBody(frame.Object, "gpt-5", "")
			if err == nil {
				t.Fatal("expected unsupported bridge field to fail")
			}
			payload := string(ClientPayloadFromError(err))
			if !strings.Contains(payload, `"unsupported_responses_ws_bridge_field"`) || !strings.Contains(payload, `"`+tc.param+`"`) {
				t.Fatalf("expected client payload to name unsupported field %q, got %s", tc.param, payload)
			}
		})
	}
}

func TestNormalizeResponsesHTTPBridgeRequestMapReappliesFinalTransportContract(t *testing.T) {
	body := map[string]interface{}{
		"type":       "response.create",
		"event_id":   "evt_1",
		"model":      "gpt-5",
		"background": false,
	}

	if err := NormalizeResponsesHTTPBridgeRequestMap(body); err != nil {
		t.Fatalf("normalize bridge request: %v", err)
	}
	for _, key := range []string{"type", "event_id", "background"} {
		if _, ok := body[key]; ok {
			t.Fatalf("expected %s to be stripped from normalized bridge request, got %#v", key, body)
		}
	}
	if body["stream"] != true {
		t.Fatalf("expected bridge request to force stream=true, got %#v", body["stream"])
	}
}

func TestNormalizeResponsesHTTPBridgeRequestMapRejectsUnsupportedTransportFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  map[string]interface{}
		param string
	}{
		{name: "stream false", body: map[string]interface{}{"stream": false}, param: "stream"},
		{name: "stream non bool", body: map[string]interface{}{"stream": "false"}, param: "stream"},
		{name: "background true", body: map[string]interface{}{"background": true}, param: "background"},
		{name: "background non bool", body: map[string]interface{}{"background": "true"}, param: "background"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := NormalizeResponsesHTTPBridgeRequestMap(tc.body)
			if err == nil {
				t.Fatal("expected unsupported bridge field to fail")
			}
			payload := string(ClientPayloadFromError(err))
			if !strings.Contains(payload, `"unsupported_responses_ws_bridge_field"`) || !strings.Contains(payload, `"`+tc.param+`"`) {
				t.Fatalf("expected client payload to name unsupported field %q, got %s", tc.param, payload)
			}
		})
	}
}

func TestBuildResponsesHTTPBridgeBodyPreservesClientPreviousResponseID(t *testing.T) {
	frame, err := ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","previous_response_id":"resp_client","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	body, err := BuildResponsesHTTPBridgeBody(frame.Object, "gpt-5", "resp_default")
	if err != nil {
		t.Fatalf("build HTTP bridge body: %v", err)
	}
	if string(body["previous_response_id"]) != `"resp_client"` {
		t.Fatalf("expected client previous_response_id to be preserved, got %s", body["previous_response_id"])
	}
}

func TestBuildResponsesHTTPBridgeBodyRequiresModel(t *testing.T) {
	frame, err := ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	if _, err := BuildResponsesHTTPBridgeBody(frame.Object, "", ""); err == nil {
		t.Fatal("expected empty model to be rejected")
	}
}
