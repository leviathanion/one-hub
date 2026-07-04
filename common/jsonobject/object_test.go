package jsonobject

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRejectsDuplicateTopLevelKeys(t *testing.T) {
	if _, err := Parse([]byte(`{"model":"gpt-5","model":"gpt-4"}`)); err == nil {
		t.Fatal("expected duplicate key error")
	}
}

func TestParseRejectsNonObjectAndTrailingData(t *testing.T) {
	if _, err := Parse([]byte(`[]`)); err == nil {
		t.Fatal("expected non-object error")
	}
	if _, err := Parse([]byte(`{"model":"gpt-5"} trailing`)); err == nil {
		t.Fatal("expected trailing data error")
	}
}

func TestObjectPatchPreservesUnknownFields(t *testing.T) {
	obj, err := Parse([]byte(`{"model":"gpt-4","future":{"keep":true},"n":12345678901234567890}`))
	if err != nil {
		t.Fatalf("parse object: %v", err)
	}
	obj.SetJSON("model", "gpt-5")
	if err := obj.SetRaw("stream", json.RawMessage("true")); err != nil {
		t.Fatalf("set raw stream: %v", err)
	}

	raw, err := obj.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal object: %v", err)
	}
	if !strings.HasPrefix(string(raw), `{"model":"gpt-5","future"`) || !strings.HasSuffix(string(raw), `,"stream":true}`) {
		t.Fatalf("expected patched object to keep original field order and append new fields, got %s", raw)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode patched object: %v", err)
	}
	if string(decoded["model"]) != `"gpt-5"` || string(decoded["stream"]) != `true` {
		t.Fatalf("expected patched model/stream, got %s", raw)
	}
	if string(decoded["future"]) != `{"keep":true}` || string(decoded["n"]) != `12345678901234567890` {
		t.Fatalf("expected unknown fields preserved, got %s", raw)
	}
}

func TestObjectMarshalReturnsRawWhenUnmodified(t *testing.T) {
	raw := []byte(" \n {\"model\":\"gpt-5\",\"future\":{\"keep\":true}} \t")
	obj, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse object: %v", err)
	}
	marshaled, err := obj.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal object: %v", err)
	}
	if string(marshaled) != string(raw) {
		t.Fatalf("expected unmodified object to marshal original bytes, got %q", marshaled)
	}
}

func TestSetRawRejectsInvalidJSON(t *testing.T) {
	obj, err := Parse([]byte(`{"model":"gpt-5"}`))
	if err != nil {
		t.Fatalf("parse object: %v", err)
	}
	if err := obj.SetRaw("broken", json.RawMessage(`{bad-json`)); err == nil {
		t.Fatal("expected invalid raw JSON to be rejected")
	}
	if _, exists := obj.Fields["broken"]; exists {
		t.Fatal("expected invalid raw JSON not to be stored")
	}
}

func TestParseRejectsExcessiveDepth(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxObjectDepth+1; i++ {
		b.WriteString(`{"x":`)
	}
	b.WriteString(`null`)
	for i := 0; i < maxObjectDepth+1; i++ {
		b.WriteByte('}')
	}
	if _, err := Parse([]byte(b.String())); err == nil {
		t.Fatal("expected excessive JSON depth to be rejected")
	}
}
