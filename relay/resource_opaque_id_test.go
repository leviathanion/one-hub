package relay

import (
	"one-api/model"
	"one-api/relay/relay_util"
	"testing"
)

func TestResourceReferenceAuthorizationUsesExactWireID(t *testing.T) {
	setupRelayTestDB(t, &model.ResourceOwner{})
	seedRequestResource(t, "file", "file-a", 1, 7)
	raw := []byte(`{"input":[{"type":"input_file","file_id":" file-a "}]}`)
	c := resourceRequestContext(raw, 1)
	if err := prepareResourceRequest(c, raw, "responses"); err == nil {
		t.Fatal("whitespace alias borrowed another wire ID's ownership")
	}
	seedRequestResource(t, "file", " file-a ", 1, 8)
	c = resourceRequestContext(raw, 1)
	if err := prepareResourceRequest(c, raw, "responses"); err != nil {
		t.Fatal(err)
	}
	if c.GetInt(relay_util.ResourceOwnerChannelKey) != 8 {
		t.Fatal("opaque spaced ID did not select its own channel")
	}
}

func TestCreatedResourceIDPreservesOpaqueJSONAndLocation(t *testing.T) {
	op := resourceHTTPOperation{kind: "file", createKind: "file"}
	id, err := createdResourceID([]byte(`{"id":" file-a "}`), op, "/v1/files/%20file-a%20")
	if err != nil || id != " file-a " {
		t.Fatalf("created ID was normalized: %q %v", id, err)
	}
	if _, err := createdResourceID([]byte(`{"id":" file-a "}`), op, "/v1/files/file-a"); err == nil {
		t.Fatal("different Location identity was aliased")
	}
}
