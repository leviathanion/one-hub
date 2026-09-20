package responses

// ProtocolResourceReferences never assigns semantics from another dialect to
// an unknown extension. Only the current operation's resource-bearing positions
// participate in local authorization.
func ProtocolResourceReferences(body map[string]any, operation string) []ResourceReference {
	var refs []ResourceReference
	switch operation {
	case "responses":
		refs = append(refs, InputResourceReferences(body["input"])...)
		refs = append(refs, ToolResourceReferences(body["tools"])...)
		if id, ok := body["conversation"].(string); ok {
			addReference(&refs, "conversation", "conversation", id)
		} else {
			addReference(&refs, "conversation", "conversation.id", object(body["conversation"])["id"])
		}
	case "chat":
		refs = append(refs, InputResourceReferences(body["messages"])...)
	}
	for _, ref := range MediaResourceReferences(body, operation) {
		if operation == "chat" && ref.Kind == "chat_audio" {
			continue
		}
		refs = append(refs, ref)
	}
	return refs
}
