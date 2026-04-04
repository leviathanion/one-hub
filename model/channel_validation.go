package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"one-api/common/config"
)

func (channel *Channel) ValidateRuntimeConfigJSON() error {
	if channel == nil {
		return nil
	}
	return channel.ValidateRuntimeConfigJSONWithType(channel.Type)
}

func (channel *Channel) ValidateRuntimeConfigJSONWithType(channelType int) error {
	if channel == nil {
		return nil
	}

	if err := validateOptionalJSONObject("model_mapping", channel.GetModelMapping()); err != nil {
		return err
	}
	modelHeaders := ""
	if channel.ModelHeaders != nil {
		modelHeaders = *channel.ModelHeaders
	}
	if err := validateOptionalJSONObject("model_headers", modelHeaders); err != nil {
		return err
	}
	if err := validateOptionalJSONObject("custom_parameter", channel.GetCustomParameter()); err != nil {
		return err
	}
	if channelType == config.ChannelTypeCodex {
		if err := validateCodexChannelOther(channel.Other); err != nil {
			return err
		}
	}

	return nil
}

func validateOptionalJSONObject(fieldName, raw string) error {
	_, err := parseOptionalJSONObject(fieldName, raw)
	return err
}

func parseOptionalJSONObject(fieldName, raw string) (map[string]json.RawMessage, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return nil, nil
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, fmt.Errorf("%s must be a valid JSON object: %w", fieldName, err)
	}
	return parsed, nil
}

// Keep Codex channel.Other as a documented finite-field contract so create/edit,
// batch import, and any batch update path reject silent docs/code drift.
func validateCodexChannelOther(raw string) error {
	parsed, err := parseOptionalJSONObject("other", raw)
	if err != nil {
		return err
	}
	if len(parsed) == 0 {
		return nil
	}

	for key, value := range parsed {
		fieldName := "other." + key
		switch key {
		case "prompt_cache_key_strategy":
			if err := validateCodexEnumField(fieldName, value, normalizeCodexPromptCacheStrategyValidation, "auto, off, session_id, auth_header, token_id, user_id"); err != nil {
				return err
			}
		case "websocket_mode":
			if err := validateCodexEnumField(fieldName, value, normalizeCodexWebsocketModeValidation, "auto, force, off"); err != nil {
				return err
			}
		case "execution_session_ttl_seconds", "websocket_retry_cooldown_seconds":
			if err := validateCodexPositiveIntField(fieldName, value); err != nil {
				return err
			}
		case "user_agent":
			if err := validateCodexStringField(fieldName, value); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is not supported for Codex channels", fieldName)
		}
	}

	return nil
}

func validateCodexStringField(fieldName string, raw json.RawMessage) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%s must be a string: %w", fieldName, err)
	}
	return nil
}

func validateCodexEnumField(fieldName string, raw json.RawMessage, normalize func(string) string, supportedValues string) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%s must be a string: %w", fieldName, err)
	}
	if normalize(value) == "" {
		return fmt.Errorf("%s must be one of: %s", fieldName, supportedValues)
	}
	return nil
}

func validateCodexPositiveIntField(fieldName string, raw json.RawMessage) error {
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%s must be a positive integer: %w", fieldName, err)
	}
	if value <= 0 {
		return fmt.Errorf("%s must be greater than 0", fieldName)
	}
	return nil
}

func normalizeCodexPromptCacheStrategyValidation(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "auto", "off", "session_id", "auth_header", "token_id", "user_id":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func normalizeCodexWebsocketModeValidation(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "auto", "force", "off":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}
