package openai

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requestctx"
	"one-api/model"
	"one-api/types"
)

type recordingMediaPolicy struct {
	refs []string
	err  error
}

func (p *recordingMediaPolicy) AuthorizeResourceReference(kind, id string) error {
	p.refs = append(p.refs, kind+":"+id)
	return p.err
}

func TestEffectiveMediaRequestAuthorizesChannelOverlay(t *testing.T) {
	for _, tc := range []struct {
		name, raw, custom string
		mode              int
	}{
		{"speech", `{"model":"test","input":"hello","voice":"alloy"}`, `{"overwrite":true,"voice":{"id":"voice_injected"}}`, config.RelayModeAudioSpeech},
		{"chat", `{"model":"test","messages":[]}`, `{"audio":{"voice":{"id":"voice_injected"}}}`, config.RelayModeChatCompletions},
		{"history", `{"model":"test","messages":[]}`, `{"overwrite":true,"messages":[{"role":"assistant","audio":{"id":"audio_injected"}}]}`, config.RelayModeChatCompletions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.raw))
			if _, err := common.CacheRequestBody(ctx); err != nil {
				t.Fatal(err)
			}
			policy := &recordingMediaPolicy{err: errors.New("owner denied")}
			requestctx.SetResourceReferencePolicy(ctx, policy)
			proxy := ""
			p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "test", Proxy: &proxy, CustomParameter: &tc.custom}, "https://api.openai.com")
			p.SetContext(ctx)
			req, apiErr := p.GetRequestTextBody(tc.mode, "test", &types.ChatCompletionRequest{Model: "test"})
			if req != nil || apiErr == nil || len(policy.refs) != 1 {
				t.Fatalf("req=%v err=%v refs=%v", req, apiErr, policy.refs)
			}
		})
	}
}

func TestNativeVoiceUnknownUnionPassesUnmodified(t *testing.T) {
	raw := `{"model":"test","input":"hello","voice":{"future":"union"},"extra":{"id":"business"}}`
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatal(err)
	}
	proxy := ""
	p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "test", Proxy: &proxy}, "https://api.openai.com")
	p.SetContext(ctx)
	req, apiErr := p.GetRequestTextBody(config.RelayModeAudioSpeech, "test", &types.SpeechAudioRequest{Model: "test"})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	defer req.Body.Close()
	got, _ := io.ReadAll(req.Body)
	if string(got) != raw {
		t.Fatalf("body changed: %s", got)
	}
}

type realtimeVoicePolicy struct {
	realtimeInputTestPolicy
	recordingMediaPolicy
}

func TestRealtimeVoiceReferencesAuthorizedBeforeSettingsAndCreate(t *testing.T) {
	for _, event := range []string{"session.update", "response.create"} {
		t.Run(event, func(t *testing.T) {
			s := newOpenAIRealtimeHelperSession()
			policy := &realtimeVoicePolicy{recordingMediaPolicy: recordingMediaPolicy{err: errors.New("owner denied")}}
			s.workPolicy = policy
			field := "session"
			if event == "response.create" {
				field = "response"
			}
			raw := []byte(`{"type":"` + event + `","` + field + `":{"audio":{"output":{"voice":{"id":"voice_private"}}}}}`)
			if _, _, err := s.prepareInputSettings(raw, event); err == nil {
				t.Fatal("unowned reference accepted")
			}
			if len(policy.refs) != 1 || policy.refs[0] != "custom_voice:voice_private" {
				t.Fatalf("refs=%v", policy.refs)
			}
			// Built-in session voice cannot grant access to a create-only override.
			builtin := []byte(`{"type":"` + event + `","` + field + `":{"audio":{"output":{"voice":"alloy"}},"future":{"voice":{"id":"business"}}}}`)
			if _, _, err := s.prepareInputSettings(builtin, event); err != nil {
				t.Fatal(err)
			}
			if len(policy.refs) != 1 {
				t.Fatalf("business data reached policy: %v", policy.refs)
			}
		})
	}
}

func TestImageJSONFinalRequestAuthorizesFileSlots(t *testing.T) {
	for _, mode := range []int{config.RelayModeImagesEdits, config.RelayModeImagesVariations, config.RelayModeImagesGenerations} {
		t.Run(string(rune('a'+mode)), func(t *testing.T) {
			for _, allow := range []bool{false, true} {
				raw := `{"model":"gpt-image-1","images":[{"file_id":"file_a"}],"mask":{"file_id":"file_mask"},"extra":{"file_id":"business"}}`
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
				ctx.Request.Header.Set("Content-Type", "application/json")
				if _, err := common.CacheRequestBody(ctx); err != nil {
					t.Fatal(err)
				}
				policy := &recordingMediaPolicy{}
				if !allow {
					policy.err = errors.New("owner denied")
				}
				requestctx.SetResourceReferencePolicy(ctx, policy)
				proxy := ""
				p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "test", Proxy: &proxy}, "https://api.openai.com")
				p.SetContext(ctx)
				var req *http.Request
				var apiErr *types.OpenAIErrorWithStatusCode
				if mode == config.RelayModeImagesGenerations {
					req, apiErr = p.GetRequestTextBody(mode, "gpt-image-1", &types.ImageRequest{})
				} else {
					req, apiErr = p.getRequestImageBody(mode, "gpt-image-1", &types.ImageEditRequest{Model: "gpt-image-1"})
				}
				if !allow {
					if apiErr == nil || req != nil {
						t.Fatal("unowned file accepted")
					}
					continue
				}
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				if len(policy.refs) != 2 || policy.refs[0] != "file:file_a" || policy.refs[1] != "file:file_mask" {
					t.Fatalf("refs=%v", policy.refs)
				}
				wire, err := io.ReadAll(req.Body)
				_ = req.Body.Close()
				if err != nil || string(wire) != raw {
					t.Fatalf("wire changed: %s error=%v", wire, err)
				}
			}
		})
	}
}
