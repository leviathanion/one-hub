package telegram

import (
	"strings"
	"testing"

	"one-api/common/config"
)

func TestGetChatURLReadsCurrentPublication(t *testing.T) {
	manager := config.NewOptionManager()
	serverAddress := ""
	chatLink := ""
	manager.RegisterStringOption("ServerAddress", &serverAddress, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterStringOption("ChatLink", &chatLink, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	original := config.GlobalOption
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = original })

	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"ServerAddress": "https://old.example", "ChatLink": "https://old-chat.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{"ServerAddress": "https://new.example", "ChatLink": "https://new-chat.example"}); err != nil {
		t.Fatal(err)
	}

	got := getChatUrl()
	if !strings.Contains(got, "new.example") || !strings.Contains(got, "new-chat.example") {
		t.Fatalf("chat URL did not use current publication: %s", got)
	}
	if strings.Contains(got, "old.example") || strings.Contains(got, "old-chat.example") {
		t.Fatalf("chat URL retained an old publication: %s", got)
	}
}

func TestGetChatURLDisabledByCurrentEmptyServerAddress(t *testing.T) {
	manager := config.NewOptionManager()
	serverAddress := ""
	chatLink := ""
	manager.RegisterStringOption("ServerAddress", &serverAddress, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterStringOption("ChatLink", &chatLink, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	original := config.GlobalOption
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = original })

	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"ServerAddress": ""}); err != nil {
		t.Fatal(err)
	}
	if got := getChatUrl(); got != "" {
		t.Fatalf("empty current server address should disable chat URL, got %q", got)
	}
}
