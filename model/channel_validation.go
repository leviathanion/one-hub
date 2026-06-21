package model

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"one-api/common/config"
	runtimesession "one-api/runtime/session"
)

const InvalidChannelRuntimeConfigCode = "invalid_channel_runtime_config"

type InvalidChannelRuntimeConfigError struct {
	ChannelID int
	Err       error
}

func NewInvalidChannelRuntimeConfigError(channelID int, err error) error {
	if err == nil {
		return nil
	}
	return &InvalidChannelRuntimeConfigError{ChannelID: channelID, Err: err}
}

func (e *InvalidChannelRuntimeConfigError) Error() string {
	if e == nil {
		return ""
	}
	if e.ChannelID > 0 {
		return fmt.Sprintf("%s: channel #%d: %v", InvalidChannelRuntimeConfigCode, e.ChannelID, e.Err)
	}
	return fmt.Sprintf("%s: %v", InvalidChannelRuntimeConfigCode, e.Err)
}

func (e *InvalidChannelRuntimeConfigError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

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
	if err := validateOptionalJSONStringMap("model_headers", modelHeaders); err != nil {
		return err
	}
	if err := validateOptionalJSONObject("custom_parameter", channel.GetCustomParameter()); err != nil {
		return err
	}
	if err := validateChannelOtherJSON(channel.Other); err != nil {
		return err
	}
	if channelType == config.ChannelTypeCustom {
		if err := validateCustomChannelClaudePlugin(channel); err != nil {
			return err
		}
	}
	if channelType == config.ChannelTypeCodex {
		if err := validateCodexChannelOther(channel.Other); err != nil {
			return err
		}
	}
	if channelType == config.ChannelTypeAzure {
		if err := validateAzureChannelOther(channel.Other); err != nil {
			return err
		}
	}
	if channelType == config.ChannelTypeAzureV1 {
		if err := validateAzureV1RuntimeEndpoint(channel); err != nil {
			return err
		}
	}
	if err := validateProviderKnownOtherFields(channelType, channel.Other); err != nil {
		return err
	}
	if channelType == config.ChannelTypeAzureSpeech {
		if err := validateAzureSpeechRuntimeEndpoint(channel); err != nil {
			return err
		}
	}

	return nil
}

func validateChannelOtherJSON(raw string) error {
	parsed, err := parseOptionalJSONObject("other", raw)
	if err != nil {
		return err
	}
	if len(parsed) == 0 {
		return nil
	}
	if rawTransport, ok := parsed["responses_ws_transport"]; ok {
		if err := validateCodexResponsesWSTransportField("other.responses_ws_transport", rawTransport); err != nil {
			return err
		}
	}
	if rawNative, ok := parsed["responses_ws_native"]; ok {
		if err := validateJSONBoolField("other.responses_ws_native", rawNative); err != nil {
			return err
		}
	}
	if rawSelfHosted, ok := parsed["self_hosted"]; ok {
		if err := validateJSONBoolField("other.self_hosted", rawSelfHosted); err != nil {
			return err
		}
	}
	if rawResponsesWSSelfHosted, ok := parsed["responses_ws_self_hosted"]; ok {
		if err := validateJSONBoolField("other.responses_ws_self_hosted", rawResponsesWSSelfHosted); err != nil {
			return err
		}
	}
	return nil
}

func validateOptionalJSONObject(fieldName, raw string) error {
	_, err := parseOptionalJSONObject(fieldName, raw)
	return err
}

func validateOptionalJSONStringMap(fieldName, raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return nil
	}

	var parsed map[string]string
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return fmt.Errorf("%s must be a JSON object with string values: %w", fieldName, err)
	}
	return nil
}

func parseOptionalJSONObject(fieldName, raw string) (map[string]json.RawMessage, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return nil, nil
	}
	if trimmed == "null" {
		return nil, fmt.Errorf("%s must be a JSON object, not null", fieldName)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, fmt.Errorf("%s must be a valid JSON object: %w", fieldName, err)
	}
	return parsed, nil
}

type AzureChannelOther struct {
	APIVersion            string `json:"api_version"`
	ResponsesWSTransport  string `json:"responses_ws_transport,omitempty"`
	SelfHosted            *bool  `json:"self_hosted,omitempty"`
	ResponsesWSSelfHosted *bool  `json:"responses_ws_self_hosted,omitempty"`
}

func ParseAzureChannelOther(raw string) (AzureChannelOther, error) {
	options, _, err := parseAzureChannelOtherObject(raw)
	return options, err
}

func parseAzureChannelOtherObject(raw string) (AzureChannelOther, map[string]json.RawMessage, error) {
	parsed, err := parseOptionalJSONObject("other", raw)
	if err != nil {
		return AzureChannelOther{}, nil, err
	}
	if len(parsed) == 0 {
		return AzureChannelOther{}, nil, fmt.Errorf("other.api_version must be a non-empty string")
	}

	var options AzureChannelOther
	for key, value := range parsed {
		fieldName := "other." + key
		switch key {
		case "api_version":
			var apiVersion string
			if err := json.Unmarshal(value, &apiVersion); err != nil {
				return AzureChannelOther{}, nil, fmt.Errorf("%s must be a string: %w", fieldName, err)
			}
			options.APIVersion = strings.TrimSpace(apiVersion)
			if options.APIVersion == "" {
				return AzureChannelOther{}, nil, fmt.Errorf("%s must be a non-empty string", fieldName)
			}
		case "responses_ws_transport":
			mode, err := runtimesession.ParseResponsesWSTransportField(value)
			if err != nil {
				return AzureChannelOther{}, nil, fmt.Errorf("%s %w", fieldName, err)
			}
			options.ResponsesWSTransport = runtimesession.ResponsesWSTransportConfigValue(mode)
		case "self_hosted":
			var selfHosted bool
			if err := json.Unmarshal(value, &selfHosted); err != nil {
				return AzureChannelOther{}, nil, fmt.Errorf("%s must be a boolean: %w", fieldName, err)
			}
			options.SelfHosted = &selfHosted
		case "responses_ws_self_hosted":
			var selfHosted bool
			if err := json.Unmarshal(value, &selfHosted); err != nil {
				return AzureChannelOther{}, nil, fmt.Errorf("%s must be a boolean: %w", fieldName, err)
			}
			options.ResponsesWSSelfHosted = &selfHosted
		case "extra", "vendor_extra":
			// Opaque namespaces are kept for vendor data and never interpreted by
			// Azure runtime logic. Unknown top-level runtime fields still fail fast.
		default:
			return AzureChannelOther{}, nil, fmt.Errorf("%s is not supported for Azure channels", fieldName)
		}
	}
	if strings.TrimSpace(options.APIVersion) == "" {
		return AzureChannelOther{}, nil, fmt.Errorf("other.api_version must be a non-empty string")
	}
	return options, parsed, nil
}

func (channel *Channel) GetAzureAPIVersion() (string, error) {
	if channel == nil {
		return "", nil
	}
	options, err := ParseAzureChannelOther(channel.Other)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(options.APIVersion), nil
}

func (channel *Channel) GetOtherStringField(fieldName string) (string, error) {
	if channel == nil {
		return "", nil
	}
	fieldName = strings.TrimSpace(fieldName)
	if fieldName == "" {
		return "", nil
	}
	other, err := channel.GetOtherMap()
	if err != nil {
		return "", err
	}
	raw, ok := other[fieldName]
	if !ok || len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("other.%s must be a string: %w", fieldName, err)
	}
	return strings.TrimSpace(value), nil
}

func validateAzureChannelOther(raw string) error {
	_, err := ParseAzureChannelOther(raw)
	return err
}

func validateProviderKnownOtherFields(channelType int, raw string) error {
	switch channelType {
	case config.ChannelTypeOpenAI, config.ChannelTypeCustom, config.ChannelTypeAzureV1:
		return validateKnownOtherFields(raw, nil, nil)
	case config.ChannelTypeAli:
		return validateKnownOtherFields(raw, []string{"dashscope_plugin"}, []string{"dashscope_plugin"})
	case config.ChannelTypeGemini, config.ChannelTypeXunfei:
		return validateKnownOtherFields(raw, []string{"api_version"}, []string{"api_version"})
	case config.ChannelTypeAzureSpeech:
		return validateKnownOtherFields(raw, []string{"region"}, []string{"region"})
	case config.ChannelTypeVertexAI:
		if err := validateKnownOtherFields(raw, []string{"region", "project_id"}, []string{"region", "project_id"}); err != nil {
			return err
		}
		return validateRequiredKnownOtherStringFields(raw, "region", "project_id")
	default:
		return nil
	}
}

func validateAzureSpeechRuntimeEndpoint(channel *Channel) error {
	if channel == nil {
		return nil
	}
	region, err := channel.GetOtherStringField("region")
	if err != nil {
		return err
	}
	baseURL := ""
	if channel.BaseURL != nil {
		baseURL = strings.TrimSpace(*channel.BaseURL)
	}
	if strings.TrimSpace(region) == "" && baseURL == "" {
		return fmt.Errorf("other.region or base_url must be configured for Azure Speech channels")
	}
	return nil
}

func validateAzureV1RuntimeEndpoint(channel *Channel) error {
	if channel == nil {
		return nil
	}
	baseURL := strings.TrimSpace(channel.GetBaseURL())
	if baseURL == "" {
		return fmt.Errorf("base_url is required for Azure V1 channels")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("base_url must be an absolute http(s) URL for Azure V1 channels")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("base_url must use http or https for Azure V1 channels")
	}
	if azureV1BaseURLHasDeploymentPath(parsed.Path) {
		return fmt.Errorf("base_url must be a resource-level Azure V1 endpoint, not a deployment path")
	}
	return nil
}

func azureV1BaseURLHasDeploymentPath(path string) bool {
	segments := strings.Split(strings.Trim(strings.ToLower(path), "/"), "/")
	for index := 0; index+1 < len(segments); index++ {
		if segments[index] == "openai" && segments[index+1] == "deployments" {
			return true
		}
	}
	return false
}

func validateKnownOtherFields(raw string, allowedProviderFields []string, stringFields []string) error {
	parsed, err := parseOptionalJSONObject("other", raw)
	if err != nil {
		return err
	}
	if len(parsed) == 0 {
		return nil
	}

	allowed := make(map[string]struct{}, len(allowedProviderFields)+6)
	for _, field := range allowedProviderFields {
		allowed[field] = struct{}{}
	}
	for _, field := range []string{
		"responses_ws_transport",
		"responses_ws_native",
		"self_hosted",
		"responses_ws_self_hosted",
		"extra",
		"vendor_extra",
	} {
		allowed[field] = struct{}{}
	}
	for field := range parsed {
		if _, ok := allowed[field]; !ok {
			// These providers have a finite runtime Other contract. Keep unknown
			// vendor data under explicit namespaces so misspelled runtime fields
			// fail fast instead of being saved and silently ignored.
			return fmt.Errorf("other.%s is not supported for this channel type; use other.extra or other.vendor_extra for opaque vendor data", field)
		}
	}
	for _, field := range stringFields {
		value, ok := parsed[field]
		if !ok || len(value) == 0 || strings.TrimSpace(string(value)) == "null" {
			continue
		}
		var stringValue string
		if err := json.Unmarshal(value, &stringValue); err != nil {
			return fmt.Errorf("other.%s must be a string: %w", field, err)
		}
	}
	return nil
}

func validateRequiredKnownOtherStringFields(raw string, fields ...string) error {
	parsed, err := parseOptionalJSONObject("other", raw)
	if err != nil {
		return err
	}
	for _, field := range fields {
		value, ok := parsed[field]
		if !ok || len(value) == 0 || strings.TrimSpace(string(value)) == "null" {
			return fmt.Errorf("other.%s is required", field)
		}
		var stringValue string
		if err := json.Unmarshal(value, &stringValue); err != nil {
			return fmt.Errorf("other.%s must be a string: %w", field, err)
		}
		if strings.TrimSpace(stringValue) == "" {
			return fmt.Errorf("other.%s is required", field)
		}
	}
	return nil
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
		case "responses_ws_transport":
			if err := validateCodexResponsesWSTransportField(fieldName, value); err != nil {
				return err
			}
		case "self_hosted", "responses_ws_self_hosted":
			if err := validateCodexBoolField(fieldName, value); err != nil {
				return err
			}
		case "execution_session_ttl_seconds", "websocket_retry_cooldown_seconds":
			if err := validateCodexPositiveIntField(fieldName, value); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is not supported for Codex channels", fieldName)
		}
	}

	return nil
}

func validateJSONBoolField(fieldName string, raw json.RawMessage) error {
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%s must be a boolean: %w", fieldName, err)
	}
	return nil
}

func validateCodexBoolField(fieldName string, raw json.RawMessage) error {
	return validateJSONBoolField(fieldName, raw)
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

func validateCodexResponsesWSTransportField(fieldName string, raw json.RawMessage) error {
	if _, err := runtimesession.ParseResponsesWSTransportField(raw); err != nil {
		return fmt.Errorf("%s %w", fieldName, err)
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

func validateCustomChannelClaudePlugin(channel *Channel) error {
	if channel == nil || channel.Plugin == nil {
		return nil
	}

	claudeConfig, ok := channel.Plugin.Data()[customClaudePluginKey]
	if !ok || claudeConfig == nil {
		return nil
	}

	if rawEnabled, exists := claudeConfig[customClaudeEnabledPluginKey]; exists {
		if _, ok := rawEnabled.(bool); !ok {
			return fmt.Errorf("plugin.claude.enabled must be a boolean")
		}
	}

	rawBaseURL, exists := claudeConfig[customClaudeBaseURLPluginKey]
	if !exists || rawBaseURL == nil {
		return nil
	}

	baseURL, ok := rawBaseURL.(string)
	if !ok {
		return fmt.Errorf("plugin.claude.base_url must be a string")
	}

	_, err := normalizeClaudeBaseURL("plugin.claude.base_url", baseURL)
	return err
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
