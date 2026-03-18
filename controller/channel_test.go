package controller

import (
	"testing"

	"one-api/common/config"
	"one-api/model"
)

func TestBuildChannelsForCreateKeepsCodexJSONIntact(t *testing.T) {
	channel := model.Channel{
		Type: config.ChannelTypeCodex,
		Key: "{\n" +
			`  "access_token": "access-token",` + "\n" +
			`  "refresh_token": "refresh-token"` + "\n" +
			"}",
		Name: "codex",
	}

	channels := buildChannelsForCreate(channel)
	if len(channels) != 1 {
		t.Fatalf("expected a single codex channel, got %d", len(channels))
	}
	if channels[0].Key != channel.Key {
		t.Fatalf("expected codex key to stay intact, got %q", channels[0].Key)
	}
}

func TestBuildChannelsForCreateSplitsNonCodexKeysByNewline(t *testing.T) {
	channel := model.Channel{
		Type: config.ChannelTypeOpenAI,
		Key:  "key-1\nkey-2",
		Name: "openai",
	}

	channels := buildChannelsForCreate(channel)
	if len(channels) != 2 {
		t.Fatalf("expected two channels, got %d", len(channels))
	}
	if channels[0].Key != "key-1" || channels[1].Key != "key-2" {
		t.Fatalf("unexpected keys after split: %#v", channels)
	}
	if channels[1].Name != "openai_2" {
		t.Fatalf("expected suffixed second channel name, got %q", channels[1].Name)
	}
}
