package types

import (
	"encoding/json"
	"testing"
)

func TestChatAudioPreservesVoiceUnion(t *testing.T) {
	for _, voice := range []string{`"alloy"`, `{"id":"voice_1","future":{"value":1}}`, `{"future":"union"}`} {
		raw := `{"model":"test","audio":{"voice":` + voice + `,"format":"pcm16"}}`
		var request ChatCompletionRequest
		if err := json.Unmarshal([]byte(raw), &request); err != nil {
			t.Fatal(err)
		}
		if string(request.Audio.Voice) != voice {
			t.Fatalf("voice changed: %s", request.Audio.Voice)
		}
		result, err := json.Marshal(request.Audio)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(result, &fields)
		if string(fields["voice"]) != voice {
			t.Fatalf("union changed: %s", result)
		}
	}
}
