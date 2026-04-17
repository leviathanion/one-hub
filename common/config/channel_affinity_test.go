package config

import (
	"strings"
	"testing"
)

func TestChannelAffinitySettingsDefaultsCloneAndRoundTrip(t *testing.T) {
	settings := DefaultChannelAffinitySettings()
	if !settings.Enabled || settings.DefaultTTLSeconds != 3600 || settings.MaxEntries != 50000 {
		t.Fatalf("unexpected default channel affinity settings: %+v", settings)
	}
	if len(settings.Rules) != 3 {
		t.Fatalf("expected default rules to be populated, got %+v", settings.Rules)
	}
	if got := settings.Rules[0].KeySources[0].Alias; got != ChannelAffinityAliasResponseID {
		t.Fatalf("expected previous_response_id alias normalization, got %q", got)
	}
	if got := settings.Rules[1].KeySources[0].Alias; got != ChannelAffinityAliasPromptCacheKey {
		t.Fatalf("expected prompt cache alias normalization, got %q", got)
	}
	if got := settings.Rules[1].KeySources[1].Source; got != "request_hint" {
		t.Fatalf("expected prompt cache request hint source, got %q", got)
	}
	if got := settings.Rules[2].KeySources[0].Alias; got != ChannelAffinityAliasSessionID {
		t.Fatalf("expected realtime session alias normalization, got %q", got)
	}

	cloned := settings.Clone()
	cloned.Rules[0].Name = "mutated"
	cloned.Rules[0].KeySources[0].Alias = "mutated"
	if settings.Rules[0].Name == "mutated" || settings.Rules[0].KeySources[0].Alias == "mutated" {
		t.Fatalf("expected Clone to deep-copy rules, got original=%+v clone=%+v", settings.Rules[0], cloned.Rules[0])
	}

	var roundTrip ChannelAffinitySettings
	if err := roundTrip.SetFromJSON(settings.JSONString()); err != nil {
		t.Fatalf("expected JSONString round-trip to succeed, got %v", err)
	}
	if len(roundTrip.Rules) != len(settings.Rules) || roundTrip.Rules[2].Kind != "realtime" {
		t.Fatalf("unexpected settings round-trip: %+v", roundTrip)
	}
}

func TestChannelAffinitySettingsSetFromJSONNormalizesRulesAndAliases(t *testing.T) {
	var settings ChannelAffinitySettings
	err := settings.SetFromJSON(`{
		"enabled": true,
		"default_ttl_seconds": 0,
		"max_entries": 0,
		"rules": [
			{
				"name": " Responses Rule ",
				"enabled": true,
				"kind": " RESPONSES ",
				"user_agent_regex": "  Codex/1.0 ",
				"key_sources": [
					{"source": " HEADER ", "key": " X-Session-Id ", "alias": "", "value_regex": " ^sess-" },
					{"source": " request_hint ", "key": " responses.prompt_cache_key ", "alias": " Prompt_Cache_Key "}
				]
			},
			{
				"name": "disabled",
				"enabled": false,
				"kind": "REALTIME",
				"key_sources": [
					{"source": " QUERY ", "key": " session_id ", "alias": "  "}
				]
			}
		]
	}`)
	if err != nil {
		t.Fatalf("expected settings JSON to decode, got %v", err)
	}
	if settings.DefaultTTLSeconds != 3600 || settings.MaxEntries != 50000 {
		t.Fatalf("expected zero defaults to be normalized, got %+v", settings)
	}
	if got := settings.Rules[0].Name; got != "Responses Rule" {
		t.Fatalf("expected rule name trimming, got %q", got)
	}
	if got := settings.Rules[0].Kind; got != "responses" {
		t.Fatalf("expected rule kind lowercase normalization, got %q", got)
	}
	if got := settings.Rules[0].UserAgentRegex; got != "Codex/1.0" {
		t.Fatalf("expected user-agent regex trimming, got %q", got)
	}
	if got := settings.Rules[0].KeySources[0].Source; got != "header" {
		t.Fatalf("expected key source normalization, got %q", got)
	}
	if got := settings.Rules[0].KeySources[0].Alias; got != "x-session-id" {
		t.Fatalf("expected empty alias to fall back to normalized key, got %q", got)
	}
	if got := settings.Rules[0].KeySources[0].ValueRegex; got != "^sess-" {
		t.Fatalf("expected value regex trimming, got %q", got)
	}
	if got := settings.Rules[0].KeySources[1].Alias; got != "prompt_cache_key" {
		t.Fatalf("expected explicit alias normalization, got %q", got)
	}
	if got := settings.Rules[0].KeySources[1].Source; got != "request_hint" {
		t.Fatalf("expected request hint source normalization, got %q", got)
	}
	if got := settings.Rules[1].Kind; got != "realtime" {
		t.Fatalf("expected disabled rule kind to still be normalized, got %q", got)
	}
	if got := settings.Rules[1].KeySources[0].Alias; got != "  " {
		t.Fatalf("expected disabled rule key sources to remain untouched, got %q", got)
	}
	if got := normalizeChannelAffinityAlias("  Session_ID  ", "fallback"); got != "session_id" {
		t.Fatalf("expected explicit alias normalization, got %q", got)
	}
	if got := normalizeChannelAffinityAlias("", " X-Session-Id "); got != "x-session-id" {
		t.Fatalf("expected fallback alias normalization, got %q", got)
	}

	if err := settings.SetFromJSON(""); err != nil {
		t.Fatalf("expected blank JSON to reset defaults, got %v", err)
	}
	if len(settings.Rules) != 3 {
		t.Fatalf("expected blank JSON reset to defaults, got %+v", settings)
	}
	if err := settings.SetFromJSON("{"); err == nil {
		t.Fatal("expected invalid JSON to fail")
	}
}

func TestChannelAffinitySettingsSetFromJSONRejectsInvalidRegex(t *testing.T) {
	t.Run("rule regex", func(t *testing.T) {
		var settings ChannelAffinitySettings
		err := settings.SetFromJSON(`{
			"rules": [
				{
					"name": "broken",
					"enabled": true,
					"kind": "responses",
					"model_regex": "["
				}
			]
		}`)
		if err == nil {
			t.Fatal("expected invalid regex to fail")
		}
		if !strings.Contains(err.Error(), "rules[0].model_regex") {
			t.Fatalf("expected field-specific regex error, got %v", err)
		}
	})

	t.Run("key source regex", func(t *testing.T) {
		var settings ChannelAffinitySettings
		err := settings.SetFromJSON(`{
			"rules": [
				{
					"name": "broken",
					"enabled": true,
					"kind": "responses",
					"key_sources": [
						{"source": "header", "key": "x-session-id", "value_regex": "["}
					]
				}
			]
		}`)
		if err == nil {
			t.Fatal("expected invalid key source regex to fail")
		}
		if !strings.Contains(err.Error(), "rules[0].key_sources[0].value_regex") {
			t.Fatalf("expected value_regex field-specific error, got %v", err)
		}
	})
}
