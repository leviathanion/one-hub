package responses

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestFunctionOutputProtocolResources(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		ids          []string
	}{
		{"files and images", `[{"type":"input_file","file_id":" file_a "},{"type":"input_image","file_id":"file_b"}]`, []string{" file_a ", "file_b"}},
		{"inline contents", `[{"type":"input_text","text":"file_id"},{"type":"input_file","file_data":"base64"},{"type":"input_image","image_url":"https://example.test/image"}]`, nil},
		{"business object", `{"type":"input_file","file_id":"business"}`, nil},
		{"business string", `"{\"type\":\"input_file\",\"file_id\":\"business\"}"`, nil},
		{"unknown part", `[{"type":"future","file_id":"business","content":[{"type":"input_file","file_id":"nested"}]}]`, nil},
		{"non-content containers", `[{"type":"message","content":[{"type":"input_file","file_id":"nested"}]},{"type":"function_call_output","output":[{"type":"input_file","file_id":"nested"}]}]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(`{"input":[{"type":"function_call_output","call_id":"call_1","output":`+tc.output+`,"arguments":{"file_id":"business"}}]}`), &body); err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, ref := range ProtocolResourceReferences(body, "responses") {
				if ref.Kind != "file" {
					t.Fatalf("unexpected resource: %+v", ref)
				}
				ids = append(ids, ref.ID)
			}
			if !reflect.DeepEqual(ids, tc.ids) {
				t.Fatalf("resource IDs=%q want=%q", ids, tc.ids)
			}
		})
	}
}
