package stmp

import (
	"strings"
	"testing"

	"one-api/common/config"
)

func useSMTPOptionManager(t *testing.T) *config.OptionManager {
	t.Helper()
	manager := config.NewOptionManager()
	host := ""
	port := 587
	account := ""
	token := ""
	from := ""
	systemName := "One Hub"
	logo := ""
	manager.RegisterStringOption("SMTPServer", &host, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterIntOption("SMTPPort", &port, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterStringOption("SMTPAccount", &account, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterStringOption("SMTPToken", &token, config.OptionMetadata{Visibility: config.OptionVisibilitySensitive})
	manager.RegisterStringOption("SMTPFrom", &from, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterStringOption("SystemName", &systemName, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterStringOption("Logo", &logo, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	original := config.GlobalOption
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = original })
	return manager
}

func TestGetSystemStmpReadsLatestPublication(t *testing.T) {
	manager := useSMTPOptionManager(t)
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{
		"SMTPServer": "old.example.com", "SMTPPort": "25", "SMTPAccount": "old", "SMTPToken": "old-token", "SMTPFrom": "old@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{
		"SMTPServer": "new.example.com", "SMTPPort": "587", "SMTPAccount": "new", "SMTPToken": "new-token", "SMTPFrom": "new@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	client, err := GetSystemStmp()
	if err != nil {
		t.Fatal(err)
	}
	if client.Host != "new.example.com" || client.Port != 587 || client.Username != "new" || client.Password != "new-token" || client.From != "new@example.com" {
		t.Fatalf("SMTP client did not use latest publication: %+v", client)
	}
}

func TestTemplateReadsCurrentPublication(t *testing.T) {
	manager := useSMTPOptionManager(t)
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"SystemName": "旧名称", "Logo": "https://old.example/logo.png"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{"SystemName": "新名称", "Logo": "https://new.example/logo.png"}); err != nil {
		t.Fatal(err)
	}

	body := getDefaultTemplate("正文")
	if !strings.Contains(body, "新名称") || !strings.Contains(body, "https://new.example/logo.png") {
		t.Fatalf("template did not use current publication: %s", body)
	}
	if strings.Contains(body, "旧名称") || strings.Contains(body, "https://old.example/logo.png") {
		t.Fatalf("template retained an old publication: %s", body)
	}
}
