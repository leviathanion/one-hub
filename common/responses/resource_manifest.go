package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

var restrictedAccountResourceKeys = map[string]struct{}{
	"file_id": {}, "vector_store_id": {}, "vector_store_ids": {},
	"container": {}, "container_id": {}, "container_reference": {},
	"skill_reference": {}, "skill_id": {},
}

// ValidateNoAccountScopedResources checks the effective request after every
// allowed overlay. Function-tool JSON schemas are intentionally not traversed:
// a property named file_id is a schema label, not an actual resource claim.
func ValidateNoAccountScopedResources(body map[string]any) error {
	if body == nil {
		return nil
	}
	for key, value := range body {
		if _, restricted := restrictedAccountResourceKeys[key]; restricted && meaningfulResourceValue(value) {
			return fmt.Errorf("%s requires an account-scoped resource owner", key)
		}
	}
	if input, ok := body["input"]; ok {
		if field, found := FindAccountScopedResourceReference(input); found {
			return fmt.Errorf("input.%s requires an account-scoped resource owner", field)
		}
	}
	tools := normalizeResourceTools(body["tools"])
	if len(tools) == 0 {
		return nil
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		toolType, _ := tool["type"].(string)
		if strings.TrimSpace(toolType) == "code_interpreter" {
			return fmt.Errorf("tools[].type=%s requires an account-scoped container owner", toolType)
		}
		for key, value := range tool {
			if _, restricted := restrictedAccountResourceKeys[key]; restricted && meaningfulResourceValue(value) {
				return fmt.Errorf("tools[].%s requires an account-scoped resource owner", key)
			}
		}
		for _, key := range []string{"environment", "input_image_mask"} {
			if field, found := FindAccountScopedResourceReference(tool[key]); found {
				return fmt.Errorf("tools[].%s.%s requires an account-scoped resource owner", key, field)
			}
		}
		if strings.TrimSpace(toolType) == "shell" {
			if environment, ok := tool["environment"].(map[string]any); ok {
				environmentType, _ := environment["type"].(string)
				if environmentType == "container_auto" || environmentType == "container_reference" {
					return fmt.Errorf("tools[].environment.type=%s requires an account-scoped container owner", environmentType)
				}
			}
		}
	}
	return nil
}

func normalizeResourceTools(value any) []any {
	switch typed := value.(type) {
	case nil:
		return nil
	case []any:
		return typed
	case map[string]any:
		return []any{typed}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var tools []any
	if json.Unmarshal(encoded, &tools) == nil {
		return tools
	}
	var tool map[string]any
	if json.Unmarshal(encoded, &tool) == nil {
		return []any{tool}
	}
	return nil
}

func ValidateNoAccountScopedResourcesJSON(raw []byte) error {
	var body map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		return fmt.Errorf("decode resource manifest: %w", err)
	}
	return ValidateNoAccountScopedResources(body)
}

func FindAccountScopedResourceReference(value any) (string, bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if _, restricted := restrictedAccountResourceKeys[key]; restricted && meaningfulResourceValue(child) {
				return key, true
			}
			if field, found := FindAccountScopedResourceReference(child); found {
				return field, true
			}
		}
	case []any:
		for _, child := range typed {
			if field, found := FindAccountScopedResourceReference(child); found {
				return field, true
			}
		}
	}
	return "", false
}

func FindAccountScopedResourceReferenceJSON(raw json.RawMessage) (string, bool) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "invalid_json", true
	}
	return FindAccountScopedResourceReference(value)
}

func meaningfulResourceValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}
