package wire

import (
	"strings"
	"testing"

	"one-api/common/jsonobject"
)

func TestMetadataFromResponsesBodyValidatesShapeAndReservedFields(t *testing.T) {
	object, err := jsonobject.Parse([]byte(`{"client_metadata":{"session_id":"sess-body","thread_id":"thread-body","x-codex-ws-stream-request-start-ms":"1710000000123"}}`))
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	metadata, err := MetadataFromResponsesBody(object)
	if err != nil {
		t.Fatalf("metadata from body: %v", err)
	}
	if value, state, err := metadata.String("session_id", validID); err != nil || state != FieldPresent || value != "sess-body" {
		t.Fatalf("expected session metadata, value=%q state=%v err=%v", value, state, err)
	}
	if err := validateMetadata(metadata); err != nil {
		t.Fatalf("expected metadata to validate, got %v", err)
	}

	object, err = jsonobject.Parse([]byte(`{"client_metadata":{"session_id":"bad\nvalue"}}`))
	if err != nil {
		t.Fatalf("parse invalid metadata body: %v", err)
	}
	metadata, err = MetadataFromResponsesBody(object)
	if err != nil {
		t.Fatalf("metadata shape should parse before grammar validation: %v", err)
	}
	if err := validateMetadata(metadata); err == nil || !strings.Contains(err.Error(), "session_id") {
		t.Fatalf("expected reserved metadata grammar rejection, got %v", err)
	}
}

func TestMetadataFromResponsesBodyRejectsNonObjectAndOversize(t *testing.T) {
	object, err := jsonobject.Parse([]byte(`{"client_metadata":"opaque"}`))
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if _, err := MetadataFromResponsesBody(object); err == nil || !strings.Contains(err.Error(), "client_metadata") {
		t.Fatalf("expected non-object metadata rejection, got %v", err)
	}

	oversize := `{"client_metadata":{"future":"` + strings.Repeat("x", 64*1024) + `"}}`
	object, err = jsonobject.Parse([]byte(oversize))
	if err != nil {
		t.Fatalf("parse oversize body: %v", err)
	}
	if _, err := MetadataFromResponsesBody(object); err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("expected oversized metadata rejection, got %v", err)
	}
}
