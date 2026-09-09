package webauthn

import (
	"testing"

	"one-api/common/config"
)

func TestGetWebAuthnUsesCurrentOptionsForEachCeremony(t *testing.T) {
	manager := config.NewOptionManager()
	serverAddress, systemName := "", ""
	manager.RegisterString("ServerAddress", &serverAddress)
	manager.RegisterString("SystemName", &systemName)

	original := config.GlobalOption
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = original })

	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{
		"ServerAddress": "https://first.example:8443",
		"SystemName":    "First",
	}); err != nil {
		t.Fatalf("publish first options: %v", err)
	}
	first, err := GetWebAuthn()
	if err != nil {
		t.Fatalf("build first WebAuthn config: %v", err)
	}
	if first.Config.RPID != "first.example" || first.Config.RPDisplayName != "First" {
		t.Fatalf("unexpected first config: %+v", first.Config)
	}

	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{
		"ServerAddress": "https://second.example",
		"SystemName":    "Second",
	}); err != nil {
		t.Fatalf("publish second options: %v", err)
	}
	second, err := GetWebAuthn()
	if err != nil {
		t.Fatalf("build second WebAuthn config: %v", err)
	}
	if second == first {
		t.Fatal("WebAuthn instance was reused across ceremonies")
	}
	if second.Config.RPID != "second.example" || second.Config.RPDisplayName != "Second" {
		t.Fatalf("unexpected second config: %+v", second.Config)
	}
}
