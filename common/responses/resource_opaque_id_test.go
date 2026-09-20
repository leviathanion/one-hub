package responses

import (
	"encoding/json"
	"testing"
)

func TestResourceReferencesPreserveOpaqueIDs(t *testing.T) {
	for _, test := range []struct{ operation, raw string }{
		{"responses", `{"input":[{"type":"input_file","file_id":" opaque "}]}`},
		{"responses", `{"conversation":" opaque "}`},
		{"responses", `{"tools":[{"type":"code_interpreter","container":" opaque "}]}`},
		{"responses", `{"tools":[{"type":"shell","environment":{"type":"container_reference","container_id":" opaque "}}]}`},
		{"responses", `{"tools":[{"type":"file_search","vector_store_ids":[" opaque "]}]}`},
		{"chat", `{"messages":[{"role":"assistant","audio":{"id":" opaque "}}]}`},
		{"speech", `{"voice":{"id":" opaque "}}`},
		{"images", `{"mask":{"file_id":" opaque "}}`},
	} {
		var body map[string]any
		if err := json.Unmarshal([]byte(test.raw), &body); err != nil {
			t.Fatal(err)
		}
		refs := ProtocolResourceReferences(body, test.operation)
		if len(refs) != 1 || refs[0].ID != " opaque " {
			t.Fatalf("%s normalized resource identity: %+v", test.raw, refs)
		}
	}
}
