package channel

import (
	"context"
	"testing"

	"one-api/common/config"
)

func TestEmailSendChecksCurrentSMTPPublication(t *testing.T) {
	manager := config.NewOptionManager()
	host := ""
	port := 587
	account := ""
	token := ""
	from := ""
	manager.RegisterStringOption("SMTPServer", &host, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterIntOption("SMTPPort", &port, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterStringOption("SMTPAccount", &account, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterStringOption("SMTPToken", &token, config.OptionMetadata{Visibility: config.OptionVisibilitySensitive})
	manager.RegisterStringOption("SMTPFrom", &from, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	original := config.GlobalOption
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = original })

	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{
		"SMTPServer": "smtp.example.com", "SMTPAccount": "account", "SMTPToken": "token", "SMTPFrom": "sender@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{"SMTPServer": ""}); err != nil {
		t.Fatal(err)
	}

	err := NewEmail("recipient@example.com").Send(context.Background(), "title", "message")
	if err == nil || err.Error() != "smtp config is not set, skip send email notifier" {
		t.Fatalf("send did not honor current disabled SMTP configuration: %v", err)
	}
}
