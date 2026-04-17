package codex

import (
	"strings"
	"testing"
)

func TestRoutingHintSettingsSetFromJSONRejectsInvalidRegex(t *testing.T) {
	t.Run("model regex", func(t *testing.T) {
		var settings RoutingHintSettings
		err := settings.SetFromJSON(`{"prompt_cache_key_strategy":"auto","model_regex":"["}`)
		if err == nil {
			t.Fatal("expected invalid model regex to fail")
		}
		if !strings.Contains(err.Error(), "model_regex") {
			t.Fatalf("expected model_regex validation error, got %v", err)
		}
	})

	t.Run("user agent regex", func(t *testing.T) {
		var settings RoutingHintSettings
		err := settings.SetFromJSON(`{"prompt_cache_key_strategy":"auto","user_agent_regex":"["}`)
		if err == nil {
			t.Fatal("expected invalid user agent regex to fail")
		}
		if !strings.Contains(err.Error(), "user_agent_regex") {
			t.Fatalf("expected user_agent_regex validation error, got %v", err)
		}
	})
}
