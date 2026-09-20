package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ResourceReference is a protocol-defined account resource location, not an
// arbitrary JSON property with a familiar name. Empty IDs describe hosted
// resource creation whose ownership is not yet implemented.
type ResourceReference struct{ Kind, ID, Path string }

// Resource IDs are opaque wire values. Only protocol discriminators may be
// normalized; authorization must not borrow another ID's ownership.
func resourceID(value any) string     { id, _ := value.(string); return id }
func resourceTag(value any) string    { return strings.TrimSpace(resourceID(value)) }
func object(value any) map[string]any { result, _ := value.(map[string]any); return result }
func addReference(out *[]ResourceReference, kind, path string, value any) {
	if id := resourceID(value); id != "" {
		*out = append(*out, ResourceReference{Kind: kind, ID: id, Path: path})
	}
}

// InputResourceReferences visits only protocol containers. Function arguments,
// string/object tool outputs and JSON schemas are opaque business data.
func InputResourceReferences(value any) []ResourceReference {
	var refs []ResourceReference
	var visit func(any)
	visit = func(value any) {
		if items, ok := value.([]any); ok {
			for _, item := range items {
				visit(item)
			}
			return
		}
		item := object(value)
		if item == nil {
			return
		}
		kind := resourceTag(item["type"])
		switch kind {
		case "input_file", "input_image":
			addReference(&refs, "file", "file_id", item["file_id"])
		case "file": // Chat file content part.
			addReference(&refs, "file", "file.file_id", object(item["file"])["file_id"])
		case "message", "":
			if kind == "message" || resourceTag(item["role"]) != "" {
				visit(item["content"])
				if resourceTag(item["role"]) == "assistant" {
					addReference(&refs, "chat_audio", "audio.id", object(item["audio"])["id"])
				}
			}
		case "tool_search_output":
			refs = append(refs, ToolResourceReferences(item["tools"])...)
		case "function_call_output":
			// Only the content-array form has protocol-defined resource slots.
			// Do not recurse through arbitrary objects or unknown content parts.
			if parts, ok := item["output"].([]any); ok {
				for _, raw := range parts {
					part := object(raw)
					switch resourceTag(part["type"]) {
					case "input_file", "input_image":
						addReference(&refs, "file", "output.file_id", part["file_id"])
					}
				}
			}
		case "container_reference":
			addReference(&refs, "container", "container_id", item["container_id"])
		case "skill_reference":
			addReference(&refs, "skill", "skill_id", item["skill_id"])
		}
	}
	visit(value)
	return refs
}

func ToolResourceReferences(value any) []ResourceReference {
	var refs []ResourceReference
	for _, raw := range normalizeResourceTools(value) {
		tool := object(raw)
		switch resourceTag(tool["type"]) {
		case "namespace":
			refs = append(refs, ToolResourceReferences(tool["tools"])...)
		case "file_search":
			if ids, ok := tool["vector_store_ids"].([]any); ok {
				for _, id := range ids {
					addReference(&refs, "vector_store", "vector_store_ids", id)
				}
			}
		case "code_interpreter":
			refs = append(refs, ResourceReference{Kind: "container", ID: resourceID(tool["container"]), Path: "container"})
			for _, id := range normalizeResourceTools(object(tool["container"])["file_ids"]) {
				addReference(&refs, "file", "container.file_ids", id)
			}
		case "image_generation":
			addReference(&refs, "file", "input_image_mask.file_id", object(tool["input_image_mask"])["file_id"])
		case "shell":
			environment := object(tool["environment"])
			switch resourceTag(environment["type"]) {
			case "container_reference":
				refs = append(refs, ResourceReference{Kind: "container", ID: resourceID(environment["container_id"]), Path: "environment.container_id"})
			case "container_auto":
				refs = append(refs, ResourceReference{Kind: "container", Path: "environment"})
			}
			refs = append(refs, InputResourceReferences(environment["skills"])...)
		}
	}
	return refs
}

func ExtractAccountScopedResourceReferences(body map[string]any) []ResourceReference {
	var conversation []ResourceReference
	if id, ok := body["conversation"].(string); ok {
		addReference(&conversation, "conversation", "conversation", id)
	} else {
		addReference(&conversation, "conversation", "conversation.id", object(body["conversation"])["id"])
	}
	refs := InputResourceReferences(body["input"])
	refs = append(refs, InputResourceReferences(body["messages"])...)
	refs = append(refs, ToolResourceReferences(body["tools"])...)
	return append(refs, conversation...)
}

func ValidateNoAccountScopedResources(body map[string]any) error {
	refs := ExtractAccountScopedResourceReferences(body)
	if len(refs) > 0 {
		return fmt.Errorf("%s requires an account-scoped %s owner", refs[0].Path, refs[0].Kind)
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
	var values []any
	if json.Unmarshal(encoded, &values) == nil {
		return values
	}
	var single map[string]any
	if json.Unmarshal(encoded, &single) == nil {
		return []any{single}
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
	refs := InputResourceReferences(value)
	if len(refs) > 0 {
		return refs[0].Path, true
	}
	return "", false
}
func FindAccountScopedResourceReferenceJSON(raw json.RawMessage) (string, bool) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return "", false
	}
	return FindAccountScopedResourceReference(value)
}
