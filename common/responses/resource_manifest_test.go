package responses

import "testing"

func TestValidateNoAccountScopedResourcesChecksEffectiveManifest(t *testing.T) {
	tests := []struct {
		name    string
		body    map[string]any
		wantErr bool
	}{
		{name: "inline input file", body: map[string]any{"input": []any{map[string]any{"type": "input_file", "file_data": "data", "filename": "a.txt"}}}},
		{name: "function schema label", body: map[string]any{"tools": []any{map[string]any{"type": "function", "parameters": map[string]any{"properties": map[string]any{"file_id": map[string]any{"type": "string"}}}}}}},
		{name: "input file id", body: map[string]any{"input": []any{map[string]any{"file_id": "file_1"}}}, wantErr: true},
		{name: "vector store", body: map[string]any{"tools": []any{map[string]any{"type": "file_search", "vector_store_ids": []any{"vs_1"}}}}, wantErr: true},
		{name: "hosted container", body: map[string]any{"tools": []any{map[string]any{"type": "code_interpreter"}}}, wantErr: true},
		{name: "skill", body: map[string]any{"tools": []any{map[string]any{"type": "function", "skill_reference": "skill_1"}}}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateNoAccountScopedResources(test.body); (err != nil) != test.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}
