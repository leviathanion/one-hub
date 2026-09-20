package responses

// MediaResourceReferences extracts the resource-bearing union members only.
// Unknown shapes remain upstream-owned parameter semantics.
func MediaResourceReferences(body map[string]any, operation string) []ResourceReference {
	var refs []ResourceReference
	switch operation {
	case "speech":
		addReference(&refs, "custom_voice", "voice.id", object(body["voice"])["id"])
	case "chat":
		addReference(&refs, "custom_voice", "audio.voice.id", object(object(body["audio"])["voice"])["id"])
		for _, ref := range InputResourceReferences(body["messages"]) {
			if ref.Kind == "chat_audio" {
				refs = append(refs, ref)
			}
		}
	case "images":
		// JSON image operations name these input slots explicitly. Their URLs,
		// inline data and unknown union members are not interpreted as IDs.
		for _, field := range []string{"images", "image", "mask"} {
			value := body[field]
			if items, ok := value.([]any); ok {
				for _, item := range items {
					addReference(&refs, "file", field+"[].file_id", object(item)["file_id"])
				}
			} else {
				addReference(&refs, "file", field+".file_id", object(value)["file_id"])
			}
		}
	case "realtime":
		field := ""
		switch resourceTag(body["type"]) {
		case "session.update":
			field = "session"
		case "response.create":
			field = "response"
		}
		if field != "" {
			settings := object(body[field])
			output := object(object(settings["audio"])["output"])
			addReference(&refs, "custom_voice", field+".audio.output.voice.id", object(output["voice"])["id"])
			// The legacy Realtime dialect keeps voice directly on session/response.
			addReference(&refs, "custom_voice", field+".voice.id", object(settings["voice"])["id"])
		}
	}
	return refs
}
