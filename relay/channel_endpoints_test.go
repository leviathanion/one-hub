package relay

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/providerendpoint"
	"one-api/model"
)

func TestCustomEndpointCandidatesExcludeDisabledBeforeProviderWork(t *testing.T) {
	for _, tc := range []struct{ id, path, body string }{
		{"openai.completions", "/v1/completions", `{"model":"gpt-test","prompt":"hi"}`},
		{"openai.embeddings", "/v1/embeddings", `{"model":"gpt-test","input":"hi"}`},
		{"openai.moderations", "/v1/moderations", `{"model":"gpt-test","input":"hi"}`},
		{"openai.images_generations", "/v1/images/generations", `{"model":"gpt-test","prompt":"hi"}`},
		{"openai.images_edits", "/v1/images/edits", `{"model":"gpt-test","prompt":"hi"}`},
		{"openai.images_variations", "/v1/images/variations", `{"model":"gpt-test"}`},
		{"openai.audio_translations", "/v1/audio/translations", `{"model":"gpt-test"}`},
	} {
		t.Run(tc.id, func(t *testing.T) {
			db := setupRelayTestDB(t, &model.Channel{})
			first := capabilitySelectionChannel(1, config.ChannelTypeCustom, "gpt-test")
			second := capabilitySelectionChannel(2, config.ChannelTypeCustom, "gpt-test")
			baseURL := "http://127.0.0.1:1"
			first.BaseURL, second.BaseURL = &baseURL, &baseURL
			first.Plugin.Data()["endpoints"][tc.id] = (providerendpoint.Setting{UpstreamURL: "/retained"}).Data()
			ctx := capabilitySelectionContext(t, tc.path, tc.body, []*model.Channel{first, second})
			if tc.id == "openai.audio_translations" {
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				if err := writer.WriteField("model", "gpt-test"); err != nil {
					t.Fatal(err)
				}
				part, err := writer.CreateFormFile("file", "speech.wav")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := part.Write([]byte("audio")); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, &body)
				ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
			}
			relay := Path2Relay(ctx, tc.path)
			if err := relay.setRequest(); err != nil {
				t.Fatal(err)
			}
			if err := relay.setProvider("gpt-test"); err != nil {
				t.Fatal(err)
			}
			if got := relay.getProvider().GetChannel().Id; got != 2 {
				t.Fatalf("selected disabled channel %d", got)
			}
			if err := db.Create(first).Error; err != nil {
				t.Fatal(err)
			}
			ctx.Set("specific_channel_id", 1)
			if err := relay.setProvider("gpt-test"); err == nil {
				t.Fatal("pinned disabled endpoint did not fail locally")
			}
		})
	}
}

func TestDisabledCustomMessagesFailsDuringProviderPreparation(t *testing.T) {
	channel := capabilitySelectionChannel(1, config.ChannelTypeCustom, "claude-test")
	ctx := capabilitySelectionContext(t, "/claude/v1/messages", "", []*model.Channel{channel})
	provider, _, err := prepareProviderForChannel(ctx, "claude-test", channel)
	if err == nil || provider != nil {
		t.Fatal("disabled native Messages reached provider preparation")
	}
}
