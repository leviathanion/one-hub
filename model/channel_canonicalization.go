package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"one-api/common/config"
)

func (channel *Channel) CanonicalizeRuntimeConfigJSON() error {
	if channel == nil {
		return nil
	}
	return channel.CanonicalizeRuntimeConfigJSONWithType(channel.Type)
}

func (channel *Channel) CanonicalizeRuntimeConfigJSONWithType(channelType int) error {
	if channel == nil {
		return nil
	}
	converted, changed, err := canonicalizeChannelOtherJSON(channelType, channel.Other)
	if err != nil {
		return err
	}
	if changed {
		channel.Other = converted
		channel.runtimeConfigParsed = false
	}
	return nil
}

func canonicalizeChannelOtherJSON(channelType int, raw string) (string, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false, nil
	}

	parsed, err := parseOptionalJSONObject("other", trimmed)
	if err == nil {
		return canonicalizeChannelOtherObject(channelType, parsed)
	}
	if legacyOtherLooksLikeJSON(trimmed) {
		return "", false, nil
	}

	switch channelType {
	case config.ChannelTypeOpenAI, config.ChannelTypeCustom:
		return marshalLegacyOpaqueOtherString(trimmed)
	case config.ChannelTypeAzure:
		return marshalLegacyOtherString("api_version", trimmed)
	case config.ChannelTypeGemini, config.ChannelTypeXunfei:
		return marshalLegacyOtherString("api_version", trimmed)
	case config.ChannelTypeAzureSpeech:
		return marshalLegacyOtherString("region", trimmed)
	case config.ChannelTypeAli:
		return marshalLegacyOtherString("dashscope_plugin", trimmed)
	case config.ChannelTypeVertexAI:
		parts := strings.Split(trimmed, "|")
		if len(parts) != 2 {
			return "", false, nil
		}
		region := strings.TrimSpace(parts[0])
		projectID := strings.TrimSpace(parts[1])
		if region == "" || projectID == "" {
			return "", false, nil
		}
		encoded, err := json.Marshal(struct {
			Region    string `json:"region"`
			ProjectID string `json:"project_id"`
		}{
			Region:    region,
			ProjectID: projectID,
		})
		if err != nil {
			return "", false, err
		}
		return string(encoded), true, nil
	default:
		return "", false, nil
	}
}

func canonicalizeChannelOtherObject(channelType int, parsed map[string]json.RawMessage) (string, bool, error) {
	if channelType != config.ChannelTypeCodex || len(parsed) == 0 {
		return "", false, nil
	}

	rawMode, ok := parsed["websocket_mode"]
	if !ok || len(rawMode) == 0 {
		return "", false, nil
	}

	var mode string
	if err := json.Unmarshal(rawMode, &mode); err != nil {
		return "", false, nil
	}
	if strings.ToLower(strings.TrimSpace(mode)) != "required" {
		return "", false, nil
	}

	encodedMode, err := json.Marshal("force")
	if err != nil {
		return "", false, err
	}
	parsed["websocket_mode"] = encodedMode
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return "", false, err
	}
	return string(encoded), true, nil
}

func marshalLegacyOpaqueOtherString(value string) (string, bool, error) {
	encoded, err := json.Marshal(map[string]map[string]string{
		"vendor_extra": {
			"legacy_other": strings.TrimSpace(value),
		},
	})
	if err != nil {
		return "", false, err
	}
	return string(encoded), true, nil
}

func marshalLegacyOtherString(field string, value string) (string, bool, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return "", false, fmt.Errorf("legacy other field is required")
	}
	encoded, err := json.Marshal(map[string]string{field: strings.TrimSpace(value)})
	if err != nil {
		return "", false, err
	}
	return string(encoded), true, nil
}
