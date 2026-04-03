package model

import (
	"strings"
	"testing"

	"one-api/common/config"
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
