package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMediaReferencesDeniedBeforeChannelSelection(t *testing.T) {
	for _, tc := range []struct {
		name, raw      string
		speech, denied bool
	}{
		{"chat custom voice", `{"model":"gpt-4o","messages":[],"audio":{"voice":{"id":"voice_other"}}}`, false, true},
		{"chat historical audio", `{"model":"gpt-4o","messages":[{"role":"assistant","audio":{"id":"audio_other"}}]}`, false, true},
		{"chat builtin voice", `{"model":"gpt-4o","messages":[],"audio":{"voice":"alloy"}}`, false, false},
		{"chat future union", `{"model":"gpt-4o","messages":[],"audio":{"voice":{"future":"union"}}}`, false, false},
		{"speech custom voice", `{"model":"tts-1","input":"hello","voice":{"id":"voice_other"}}`, true, true},
		{"speech builtin voice", `{"model":"tts-1","input":"hello","voice":"alloy"}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.raw))
			c.Request.Header.Set("Content-Type", "application/json")
			var err error
			if tc.speech {
				err = NewRelaySpeech(c).setRequest()
			} else {
				err = NewRelayChat(c).setRequest()
			}
			if (err != nil) != tc.denied {
				t.Fatalf("err=%v denied=%v", err, tc.denied)
			}
			if tc.denied && c.GetInt("channel_id") != 0 {
				t.Fatal("selected channel before resource authorization")
			}
		})
	}
}
