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
		if err := validateOptionalJSONObject("other", channel.Other); err != nil {
			return err
		}
	}

	return nil
}

func validateOptionalJSONObject(fieldName, raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return nil
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return fmt.Errorf("%s must be a valid JSON object: %w", fieldName, err)
	}
	return nil
}
