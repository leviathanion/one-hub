package responses

import (
	"encoding/json"
	"testing"
)

func TestResourceManifestUsesProtocolLocations(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"tool search function schema", `{"input":[{"type":"tool_search_output","tools":[{"type":"function","parameters":{"properties":{"file_id":{"type":"string"},"container_id":{"type":"string"}}}}]}]}`, 0},
		{"namespace schema", `{"input":[{"type":"tool_search_output","tools":[{"type":"namespace","tools":[{"type":"function","parameters":{"properties":{"file_id":"business"}}}]}]}]}`, 0},
		{"business tool result", `{"input":[{"type":"function_call_output","output":{"file_id":"business","type":"input_file"}}]}`, 0},
		{"business custom input", `{"input":[{"type":"custom_tool_call","input":{"file_id":"business","type":"input_file"}}]}`, 0},
		{"unknown future input", `{"input":[{"type":"future","payload":{"type":"input_file","file_id":"business"}}]}`, 0},
		{"real input beside schema", `{"input":[{"type":"tool_search_output","tools":[{"type":"function","parameters":{"file_id":"business"}}]},{"role":"user","content":[{"type":"input_file","file_id":"file_real"}]}]}`, 1},
		{"chat file", `{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file_real"}}]}]}`, 1},
		{"dynamic hosted tool", `{"input":[{"type":"tool_search_output","tools":[{"type":"file_search","vector_store_ids":["vs_real"]}]}]}`, 1},
		{"mask file", `{"tools":[{"type":"image_generation","input_image_mask":{"file_id":"file_real"}}]}`, 1},
		{"unknown tool payload", `{"tools":[{"type":"future","file_id":"business"}]}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(tt.body), &body); err != nil {
				t.Fatal(err)
			}
			refs := ExtractAccountScopedResourceReferences(body)
			if len(refs) != tt.want {
				t.Fatalf("refs=%+v want %d", refs, tt.want)
			}
			if (ValidateNoAccountScopedResources(body) != nil) != (tt.want > 0) {
				t.Fatal("manifest gate differs from extraction")
			}
		})
	}
}

func TestMediaReferencesAreSemantic(t *testing.T) {
	tests := []struct{ op, raw, kind string }{
		{"speech", `{"voice":{"id":"voice_a"}}`, "custom_voice"},
		{"speech", `{"voice":"alloy","extra":{"voice":{"id":"business"}}}`, ""},
		{"chat", `{"audio":{"voice":{"id":"voice_a"}}}`, "custom_voice"},
		{"chat", `{"messages":[{"role":"assistant","audio":{"id":"audio_a"}}]}`, "chat_audio"},
		{"chat", `{"messages":[{"role":"tool","content":"{\"audio\":{\"id\":\"business\"}}"}]}`, ""},
		{"realtime", `{"type":"session.update","session":{"audio":{"output":{"voice":{"id":"voice_a"}}}}}`, "custom_voice"},
		{"realtime", `{"type":"response.create","response":{"audio":{"output":{"voice":{"id":"voice_a"}}}}}`, "custom_voice"},
		{"realtime", `{"type":"response.create","response":{"audio":{"output":{"voice":"alloy"}},"metadata":{"voice":{"id":"business"}}}}`, ""},
	}
	for _, tt := range tests {
		var body map[string]any
		_ = json.Unmarshal([]byte(tt.raw), &body)
		refs := MediaResourceReferences(body, tt.op)
		if tt.kind == "" {
			if len(refs) != 0 {
				t.Errorf("%s unexpected %+v", tt.raw, refs)
			}
		} else if len(refs) != 1 || refs[0].Kind != tt.kind {
			t.Errorf("%s refs=%+v", tt.raw, refs)
		}
	}
}

func TestImageReferencesUseOnlyImageSlots(t *testing.T) {
	raw := `{"images":[{"file_id":"file_a"},{"image_url":"https://example.test/a"},{"future":{"file_id":"business"}}],"image":{"file_id":"file_b"},"mask":{"file_id":"file_c"},"metadata":{"file_id":"business"},"prompt":"file_id business"}`
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	refs := MediaResourceReferences(body, "images")
	if len(refs) != 3 {
		t.Fatalf("refs=%+v", refs)
	}
	for i, id := range []string{"file_a", "file_b", "file_c"} {
		if refs[i].Kind != "file" || refs[i].ID != id {
			t.Fatalf("refs=%+v", refs)
		}
	}
}
