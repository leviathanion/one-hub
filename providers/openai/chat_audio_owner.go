package openai

import (
	"encoding/json"
	"one-api/providers/base"
)

func (p *OpenAIProvider) SetChatAudioOwnerPolicy(prepare func(int) error, commit func([]base.ChatAudioResourceFact) error) {
	p.prepareChatAudioOwner = prepare
	p.commitChatAudioOwner = commit
}

func chatAudioSlots(fields map[string]json.RawMessage) int {
	var modalities []string
	_ = json.Unmarshal(fields["modalities"], &modalities)
	audio := false
	for _, modality := range modalities {
		if modality == "audio" {
			audio = true
		}
	}
	if raw := fields["audio"]; len(raw) > 0 && string(raw) != "null" {
		audio = true
	}
	if !audio {
		return 0
	}
	n := 1
	var requested int
	if json.Unmarshal(fields["n"], &requested) == nil && requested > 0 {
		n = requested
	}
	return n
}

func chatAudioFacts(raw json.RawMessage) []base.ChatAudioResourceFact {
	var choices []map[string]json.RawMessage
	if json.Unmarshal(raw, &choices) != nil {
		return nil
	}
	var facts []base.ChatAudioResourceFact
	for _, choice := range choices {
		var index *int
		_ = json.Unmarshal(choice["index"], &index)
		for _, field := range []string{"message", "delta"} {
			var message map[string]json.RawMessage
			if json.Unmarshal(choice[field], &message) != nil {
				continue
			}
			var audio map[string]json.RawMessage
			if json.Unmarshal(message["audio"], &audio) != nil {
				continue
			}
			fact := base.ChatAudioResourceFact{Index: index}
			_ = json.Unmarshal(audio["id"], &fact.ID)
			_ = json.Unmarshal(audio["expires_at"], &fact.ExpiresAt)
			if fact.ID != "" || fact.ExpiresAt != nil {
				facts = append(facts, fact)
			}
		}
	}
	return facts
}
