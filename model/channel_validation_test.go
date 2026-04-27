package model

import (
	"strings"
	"testing"

	"one-api/common/config"

	"gorm.io/datatypes"
)

func testStringPtr(value string) *string {
	return &value
}

func TestChannelRuntimeConfigValidationBranches(t *testing.T) {
	if err := (*Channel)(nil).ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected nil channel runtime config validation to no-op, got %v", err)
	}
	if err := (*Channel)(nil).ValidateRuntimeConfigJSONWithType(config.ChannelTypeCodex); err != nil {
		t.Fatalf("expected nil channel typed runtime config validation to no-op, got %v", err)
	}

	channel := &Channel{
		Type:            config.ChannelTypeCodex,
		ModelMapping:    testStringPtr(`{"gpt-5":"gpt-5-codex"}`),
		ModelHeaders:    testStringPtr(`{"X-Test":"1"}`),
		CustomParameter: testStringPtr(`{"temperature":0.2}`),
		Other:           `{"user_agent":"codex-cli"}`,
	}
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected valid codex runtime config json, got %v", err)
	}

	channel.ModelMapping = testStringPtr(`{"broken":`)
	if err := channel.ValidateRuntimeConfigJSONWithType(config.ChannelTypeCodex); err == nil || !strings.Contains(err.Error(), "model_mapping") {
		t.Fatalf("expected invalid model_mapping json to fail validation, got %v", err)
	}

	channel.ModelMapping = nil
	channel.ModelHeaders = testStringPtr(`{"broken":`)
	if err := channel.ValidateRuntimeConfigJSONWithType(config.ChannelTypeCodex); err == nil || !strings.Contains(err.Error(), "model_headers") {
		t.Fatalf("expected invalid model_headers json to fail validation, got %v", err)
	}

	channel.ModelHeaders = nil
	channel.CustomParameter = testStringPtr(`{"broken":`)
	if err := channel.ValidateRuntimeConfigJSONWithType(config.ChannelTypeCodex); err == nil || !strings.Contains(err.Error(), "custom_parameter") {
		t.Fatalf("expected invalid custom_parameter json to fail validation, got %v", err)
	}

	channel.CustomParameter = nil
	channel.Other = `{"broken":`
	if err := channel.ValidateRuntimeConfigJSONWithType(config.ChannelTypeCodex); err == nil || !strings.Contains(err.Error(), "other") {
		t.Fatalf("expected invalid codex other json to fail validation, got %v", err)
	}

	if err := validateOptionalJSONObject("other", " "); err != nil {
		t.Fatalf("expected blank optional json to validate, got %v", err)
	}
	if err := validateOptionalJSONObject("other", "{}"); err != nil {
		t.Fatalf("expected empty optional json object to validate, got %v", err)
	}
	if err := validateOptionalJSONObject("other", `{"ok":true}`); err != nil {
		t.Fatalf("expected valid optional json object to validate, got %v", err)
	}
	if err := validateOptionalJSONObject("other", `[]`); err == nil || !strings.Contains(err.Error(), "other") {
		t.Fatalf("expected non-object optional json payload to fail validation, got %v", err)
	}
}

func TestValidateCodexChannelOtherAcceptsDocumentedFields(t *testing.T) {
	channel := &Channel{
		Type: config.ChannelTypeCodex,
		Other: `{
			"prompt_cache_key_strategy":" AUTO ",
			"websocket_mode":" force ",
			"execution_session_ttl_seconds":600,
			"websocket_retry_cooldown_seconds":120,
			"user_agent":"Codex/1.0"
		}`,
	}
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected documented Codex other fields to validate, got %v", err)
	}
}

func TestValidateCustomChannelClaudePlugin(t *testing.T) {
	validPlugin := datatypes.NewJSONType(PluginType{
		"claude": {
			"enabled":  true,
			"base_url": "https://provider.example.com",
		},
	})
	channel := &Channel{
		Type:   config.ChannelTypeCustom,
		Plugin: &validPlugin,
	}
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected valid custom Claude plugin config, got %v", err)
	}

	invalidEnabledPlugin := datatypes.NewJSONType(PluginType{
		"claude": {
			"enabled": "true",
		},
	})
	channel.Plugin = &invalidEnabledPlugin
	if err := channel.ValidateRuntimeConfigJSON(); err == nil || !strings.Contains(err.Error(), "plugin.claude.enabled") {
		t.Fatalf("expected invalid Claude enabled flag to fail validation, got %v", err)
	}

	invalidBaseURLPlugin := datatypes.NewJSONType(PluginType{
		"claude": {
			"enabled":  true,
			"base_url": "https://provider.example.com/v1/messages",
		},
	})
	channel.Plugin = &invalidBaseURLPlugin
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected Claude base_url ending with /v1/messages to be normalized instead of rejected, got %v", err)
	}
}

func TestValidateCodexChannelOtherRejectsUnsupportedOrInvalidFields(t *testing.T) {
	cases := []struct {
		name     string
		other    string
		contains string
	}{
		{
			name:     "unsupported field",
			other:    `{"user_agent_regex":"^Codex/"}`,
			contains: "other.user_agent_regex",
		},
		{
			name:     "invalid websocket mode",
			other:    `{"websocket_mode":"weird"}`,
			contains: "other.websocket_mode",
		},
		{
			name:     "invalid prompt cache strategy",
			other:    `{"prompt_cache_key_strategy":"weird"}`,
			contains: "other.prompt_cache_key_strategy",
		},
		{
			name:     "non-positive execution session ttl",
			other:    `{"execution_session_ttl_seconds":0}`,
			contains: "other.execution_session_ttl_seconds",
		},
		{
			name:     "invalid user agent type",
			other:    `{"user_agent":123}`,
			contains: "other.user_agent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel := &Channel{
				Type:  config.ChannelTypeCodex,
				Other: tc.other,
			}
			err := channel.ValidateRuntimeConfigJSON()
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected Codex other validation error containing %q, got %v", tc.contains, err)
			}
		})
	}
}
