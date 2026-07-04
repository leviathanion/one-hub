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
		ModelHeaders:    testStringPtr(`{}`),
		CustomParameter: testStringPtr(`{"temperature":0.2}`),
		Other:           `{"websocket_mode":"auto"}`,
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

	channel.ModelHeaders = testStringPtr(`{"X-Test":"1"}`)
	if err := channel.ValidateRuntimeConfigJSONWithType(config.ChannelTypeCodex); err == nil || !strings.Contains(err.Error(), "model_headers") {
		t.Fatalf("expected Codex model_headers to fail validation, got %v", err)
	}

	channel.ModelHeaders = testStringPtr(`{"User-Agent":123}`)
	if err := channel.ValidateRuntimeConfigJSONWithType(config.ChannelTypeCodex); err == nil || !strings.Contains(err.Error(), "model_headers") {
		t.Fatalf("expected non-string model_headers value to fail validation, got %v", err)
	}

	channel.ModelHeaders = testStringPtr(`null`)
	if err := channel.ValidateRuntimeConfigJSONWithType(config.ChannelTypeCodex); err != nil {
		t.Fatalf("expected Codex null model_headers to validate as empty, got %v", err)
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
	if err := validateOptionalJSONObject("other", `null`); err == nil || !strings.Contains(err.Error(), "other must be a JSON object, not null") {
		t.Fatalf("expected literal null optional json payload to fail validation, got %v", err)
	}
}

func TestParseAzureChannelOtherRejectsLiteralNullAsJSONObjectContractError(t *testing.T) {
	_, err := ParseAzureChannelOther("null")
	if err == nil || !strings.Contains(err.Error(), "other must be a JSON object, not null") {
		t.Fatalf("expected Azure literal null Other to fail JSON object contract, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "api_version") {
		t.Fatalf("expected null error before api_version validation, got %v", err)
	}
}

func TestValidateCodexChannelOtherAcceptsDocumentedFields(t *testing.T) {
	channel := &Channel{
		Type: config.ChannelTypeCodex,
		Other: `{
			"prompt_cache_key_strategy":" AUTO ",
			"websocket_mode":" force ",
			"responses_ws_transport":" native ",
			"self_hosted":true,
			"responses_ws_self_hosted":false,
			"execution_session_ttl_seconds":600,
			"websocket_retry_cooldown_seconds":120,
			"codex":{
				"fedramp":true,
				"residency":"us",
				"default_originator":"codex_cli_rs",
				"trust_client_attestation":false,
				"auto_generate":{
					"session_id":true,
					"thread_id":true,
					"client_request_id":true,
					"installation_id":true,
					"ws_stream_request_start_ms":true
				}
			}
		}`,
	}
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected documented Codex other fields to validate, got %v", err)
	}
}

func TestValidateCodexChannelOtherRejectsResponsesWSHTTPBridge(t *testing.T) {
	channel := &Channel{
		Type:  config.ChannelTypeCodex,
		Other: `{"responses_ws_transport":"http_bridge"}`,
	}
	if err := channel.ValidateRuntimeConfigJSON(); err == nil || !strings.Contains(err.Error(), "responses_ws_transport") || !strings.Contains(err.Error(), "HTTP bridge is not supported") {
		t.Fatalf("expected Codex http_bridge transport to fail validation, got %v", err)
	}
}

func TestValidateCodexChannelOtherRejectsUnknownOfficialPolicyKeys(t *testing.T) {
	cases := []struct {
		name     string
		other    string
		contains string
	}{
		{
			name:     "unknown codex policy key",
			other:    `{"codex":{"fedramp":false,"legacy_profile":"pi"}}`,
			contains: "other.codex.legacy_profile",
		},
		{
			name:     "codex policy not object",
			other:    `{"codex":"legacy"}`,
			contains: "other.codex",
		},
		{
			name:     "codex policy bool string",
			other:    `{"codex":{"trust_client_attestation":"false"}}`,
			contains: "other.codex.trust_client_attestation",
		},
		{
			name:     "codex policy invalid residency",
			other:    `{"codex":{"residency":"bad value"}}`,
			contains: "other.codex.residency",
		},
		{
			name:     "codex policy invalid default originator",
			other:    `{"codex":{"default_originator":"bad value"}}`,
			contains: "other.codex.default_originator",
		},
		{
			name:     "codex auto generate not object",
			other:    `{"codex":{"auto_generate":true}}`,
			contains: "other.codex.auto_generate",
		},
		{
			name:     "codex auto generate unknown key",
			other:    `{"codex":{"auto_generate":{"everything":true}}}`,
			contains: "other.codex.auto_generate.everything",
		},
		{
			name:     "codex auto generate non bool",
			other:    `{"codex":{"auto_generate":{"session_id":"true"}}}`,
			contains: "other.codex.auto_generate.session_id",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			channel := &Channel{Type: config.ChannelTypeCodex, Other: tt.other}
			if err := channel.ValidateRuntimeConfigJSON(); err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("expected %s validation error, got %v", tt.contains, err)
			}
		})
	}
}

func TestValidateChannelOtherRejectsNullAndValidatesResponsesWSNative(t *testing.T) {
	nullOther := &Channel{
		Type:  config.ChannelTypeOpenAI,
		Other: "null",
	}
	if err := nullOther.ValidateRuntimeConfigJSON(); err == nil || !strings.Contains(err.Error(), "other") {
		t.Fatalf("expected literal null other to fail validation, got %v", err)
	}

	validNative := &Channel{
		Type:  config.ChannelTypeCustom,
		Other: `{"responses_ws_native":true,"self_hosted":false,"responses_ws_self_hosted":true}`,
	}
	if err := validNative.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected boolean OpenAI-compatible other flags to validate, got %v", err)
	}

	cases := []struct {
		name     string
		other    string
		contains string
	}{
		{
			name:     "responses ws native string",
			other:    `{"responses_ws_native":"true"}`,
			contains: "other.responses_ws_native",
		},
		{
			name:     "realtime self hosted string",
			other:    `{"self_hosted":"true"}`,
			contains: "other.self_hosted",
		},
		{
			name:     "responses ws self hosted string",
			other:    `{"responses_ws_self_hosted":"true"}`,
			contains: "other.responses_ws_self_hosted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invalid := &Channel{
				Type:  config.ChannelTypeCustom,
				Other: tc.other,
			}
			if err := invalid.ValidateRuntimeConfigJSON(); err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected non-boolean flag validation error containing %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestValidateOpenAICompatibleOtherRejectsUnsupportedRuntimeField(t *testing.T) {
	validCases := []struct {
		name        string
		channelType int
		other       string
	}{
		{
			name:        "openai public fields",
			channelType: config.ChannelTypeOpenAI,
			other:       `{"responses_ws_transport":"http_bridge","self_hosted":false,"responses_ws_self_hosted":true,"vendor_extra":{"provider":"x"}}`,
		},
		{
			name:        "custom native flag",
			channelType: config.ChannelTypeCustom,
			other:       `{"responses_ws_native":true,"extra":{"provider":"x"}}`,
		},
		{
			name:        "azure v1 public fields",
			channelType: config.ChannelTypeAzureV1,
			other:       `{"responses_ws_transport":"native","self_hosted":true,"responses_ws_self_hosted":false,"vendor_extra":{"provider":"azure-v1"}}`,
		},
	}
	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			channel := &Channel{Type: tc.channelType, Other: tc.other}
			if tc.channelType == config.ChannelTypeAzureV1 {
				channel.BaseURL = testStringPtr("https://resource.openai.azure.com")
			}
			if err := channel.ValidateRuntimeConfigJSON(); err != nil {
				t.Fatalf("expected OpenAI-compatible public other fields to validate, got %v", err)
			}
		})
	}

	invalidCases := []struct {
		name        string
		channelType int
		other       string
		contains    string
	}{
		{
			name:        "openai typo native",
			channelType: config.ChannelTypeOpenAI,
			other:       `{"responses_ws_nativ":true}`,
			contains:    "other.responses_ws_nativ",
		},
		{
			name:        "custom typo self hosted",
			channelType: config.ChannelTypeCustom,
			other:       `{"selfHosted":true}`,
			contains:    "other.selfHosted",
		},
		{
			name:        "custom camel native",
			channelType: config.ChannelTypeCustom,
			other:       `{"responsesWsNative":true}`,
			contains:    "other.responsesWsNative",
		},
		{
			name:        "azure v1 ignores classic api version",
			channelType: config.ChannelTypeAzureV1,
			other:       `{"api_version":"2024-10-01-preview"}`,
			contains:    "other.api_version",
		},
		{
			name:        "azure v1 typo self hosted",
			channelType: config.ChannelTypeAzureV1,
			other:       `{"responses_ws_selfHosted":true}`,
			contains:    "other.responses_ws_selfHosted",
		},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			channel := &Channel{Type: tc.channelType, Other: tc.other}
			if tc.channelType == config.ChannelTypeAzureV1 {
				channel.BaseURL = testStringPtr("https://resource.openai.azure.com")
			}
			err := channel.ValidateRuntimeConfigJSON()
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected unsupported OpenAI-compatible field error containing %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestValidateAzureV1RequiresResourceLevelBaseURL(t *testing.T) {
	channel := &Channel{Type: config.ChannelTypeAzureV1, Other: `{}`}
	if err := channel.ValidateRuntimeConfigJSON(); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("expected Azure V1 without base_url to be rejected, got %v", err)
	}

	deploymentBaseURL := "https://resource.openai.azure.com/openai/deployments/gpt-5"
	channel.BaseURL = &deploymentBaseURL
	if err := channel.ValidateRuntimeConfigJSON(); err == nil || !strings.Contains(err.Error(), "resource-level") {
		t.Fatalf("expected Azure V1 deployment-path base_url to be rejected, got %v", err)
	}

	deploymentListBaseURL := "https://resource.openai.azure.com/openai/deployments?api-version=2024-10-21"
	channel.BaseURL = &deploymentListBaseURL
	if err := channel.ValidateRuntimeConfigJSON(); err == nil || !strings.Contains(err.Error(), "resource-level") {
		t.Fatalf("expected Azure V1 deployment-list base_url to be rejected, got %v", err)
	}

	resourceBaseURL := "https://resource.openai.azure.com/gateway"
	channel.BaseURL = &resourceBaseURL
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected Azure V1 resource-level base_url to validate, got %v", err)
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
			name:     "legacy required websocket mode without canonicalization",
			other:    `{"websocket_mode":"required"}`,
			contains: "other.websocket_mode",
		},
		{
			name:     "invalid prompt cache strategy",
			other:    `{"prompt_cache_key_strategy":"weird"}`,
			contains: "other.prompt_cache_key_strategy",
		},
		{
			name:     "invalid responses websocket transport",
			other:    `{"responses_ws_transport":"auto"}`,
			contains: "other.responses_ws_transport",
		},
		{
			name:     "non-string responses websocket transport",
			other:    `{"responses_ws_transport":123}`,
			contains: "other.responses_ws_transport",
		},
		{
			name:     "non-positive execution session ttl",
			other:    `{"execution_session_ttl_seconds":0}`,
			contains: "other.execution_session_ttl_seconds",
		},
		{
			name:     "legacy user agent field",
			other:    `{"user_agent":"Codex/1.0"}`,
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

func TestCanonicalizeRuntimeConfigJSONNormalizesLegacyOther(t *testing.T) {
	openAI := &Channel{Type: config.ChannelTypeOpenAI, Other: "legacy-openai"}
	if err := openAI.CanonicalizeRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected OpenAI legacy other canonicalization to succeed, got %v", err)
	}
	assertJSONObjectsEqual(t, openAI.Other, `{"vendor_extra":{"legacy_other":"legacy-openai"}}`)
	if err := openAI.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected canonicalized OpenAI other to validate, got %v", err)
	}

	custom := &Channel{Type: config.ChannelTypeCustom, Other: "legacy-custom"}
	if err := custom.CanonicalizeRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected Custom legacy other canonicalization to succeed, got %v", err)
	}
	assertJSONObjectsEqual(t, custom.Other, `{"vendor_extra":{"legacy_other":"legacy-custom"}}`)
	if err := custom.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected canonicalized Custom other to validate, got %v", err)
	}

	codex := &Channel{Type: config.ChannelTypeCodex, Other: `{"websocket_mode":"required"}`}
	if err := codex.CanonicalizeRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected Codex legacy websocket_mode canonicalization to succeed, got %v", err)
	}
	assertJSONObjectsEqual(t, codex.Other, `{"websocket_mode":"force"}`)
	if err := codex.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected canonicalized Codex other to validate, got %v", err)
	}
}

func TestCanonicalizeRuntimeConfigJSONPreservesCanonicalAndFailClosedValues(t *testing.T) {
	canonical := &Channel{
		Type:  config.ChannelTypeOpenAI,
		Other: `{"responses_ws_transport":"http_bridge","vendor_extra":{"future":true}}`,
	}
	if err := canonical.CanonicalizeRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected canonical OpenAI other to remain valid, got %v", err)
	}
	if canonical.Other != `{"responses_ws_transport":"http_bridge","vendor_extra":{"future":true}}` {
		t.Fatalf("expected canonical JSON object to be preserved byte-for-byte, got %q", canonical.Other)
	}

	malformed := &Channel{Type: config.ChannelTypeOpenAI, Other: `{"vendor_extra":`}
	if err := malformed.CanonicalizeRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected malformed JSON-like other to be left for validation, got %v", err)
	}
	if malformed.Other != `{"vendor_extra":` {
		t.Fatalf("expected malformed JSON-like other to remain unchanged, got %q", malformed.Other)
	}
	if err := malformed.ValidateRuntimeConfigJSON(); err == nil {
		t.Fatal("expected malformed JSON-like other to keep failing validation")
	}

	jsonString := &Channel{Type: config.ChannelTypeOpenAI, Other: `"legacy-openai"`}
	if err := jsonString.CanonicalizeRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected JSON string other to be left for validation, got %v", err)
	}
	if jsonString.Other != `"legacy-openai"` {
		t.Fatalf("expected JSON string other to remain unchanged, got %q", jsonString.Other)
	}
	if err := jsonString.ValidateRuntimeConfigJSON(); err == nil {
		t.Fatal("expected JSON string other to keep failing validation")
	}
}

func TestValidateAzureChannelOtherRequiresJSONAPIConfig(t *testing.T) {
	valid := &Channel{
		Type:  config.ChannelTypeAzure,
		Other: `{"api_version":"2024-10-01-preview","responses_ws_transport":"http_bridge","self_hosted":true,"responses_ws_self_hosted":false,"extra":{"deployment":"x"},"vendor_extra":{"owner":"ops"}}`,
	}
	if err := valid.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected Azure JSON other to validate, got %v", err)
	}
	options, err := ParseAzureChannelOther(valid.Other)
	if err != nil {
		t.Fatalf("expected Azure other parser to accept responses ws self hosted flag, got %v", err)
	}
	if options.ResponsesWSSelfHosted == nil || *options.ResponsesWSSelfHosted {
		t.Fatalf("expected Azure parser to preserve explicit false responses_ws_self_hosted, got %+v", options.ResponsesWSSelfHosted)
	}
	if options.SelfHosted == nil || !*options.SelfHosted {
		t.Fatalf("expected Azure parser to preserve explicit true self_hosted, got %+v", options.SelfHosted)
	}
	apiVersion, err := valid.GetAzureAPIVersion()
	if err != nil || apiVersion != "2024-10-01-preview" {
		t.Fatalf("expected Azure api_version helper to read JSON, version=%q err=%v", apiVersion, err)
	}

	cases := []struct {
		name     string
		other    string
		contains string
	}{
		{
			name:     "legacy plain string",
			other:    "2024-10-01-preview",
			contains: "other",
		},
		{
			name:     "blank",
			other:    "",
			contains: "other.api_version",
		},
		{
			name:     "missing api version",
			other:    `{"responses_ws_transport":"native"}`,
			contains: "other.api_version",
		},
		{
			name:     "blank api version",
			other:    `{"api_version":" "}`,
			contains: "other.api_version",
		},
		{
			name:     "non string api version",
			other:    `{"api_version":123}`,
			contains: "other.api_version",
		},
		{
			name:     "invalid transport",
			other:    `{"api_version":"2024-10-01-preview","responses_ws_transport":"auto"}`,
			contains: "other.responses_ws_transport",
		},
		{
			name:     "non string transport",
			other:    `{"api_version":"2024-10-01-preview","responses_ws_transport":123}`,
			contains: "other.responses_ws_transport",
		},
		{
			name:     "non boolean responses ws self hosted",
			other:    `{"api_version":"2024-10-01-preview","responses_ws_self_hosted":"true"}`,
			contains: "other.responses_ws_self_hosted",
		},
		{
			name:     "non boolean realtime self hosted",
			other:    `{"api_version":"2024-10-01-preview","self_hosted":"true"}`,
			contains: "other.self_hosted",
		},
		{
			name:     "unsupported field",
			other:    `{"api_version":"2024-10-01-preview","websocket_mode":"force"}`,
			contains: "other.websocket_mode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel := &Channel{
				Type:  config.ChannelTypeAzure,
				Other: tc.other,
			}
			err := channel.ValidateRuntimeConfigJSON()
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected Azure other validation error containing %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestValidateProviderKnownOtherStringFields(t *testing.T) {
	cases := []struct {
		name         string
		channelType  int
		validOther   string
		invalidOther string
		contains     string
	}{
		{
			name:         "ali dashscope plugin",
			channelType:  config.ChannelTypeAli,
			validOther:   `{"dashscope_plugin":"plugin-a","extra":{"future":123}}`,
			invalidOther: `{"dashscope_plugin":123}`,
			contains:     "other.dashscope_plugin",
		},
		{
			name:         "gemini api version",
			channelType:  config.ChannelTypeGemini,
			validOther:   `{"api_version":"v1","vendor_extra":{"future":true}}`,
			invalidOther: `{"api_version":123}`,
			contains:     "other.api_version",
		},
		{
			name:         "xunfei api version",
			channelType:  config.ChannelTypeXunfei,
			validOther:   `{"api_version":"v3.5","extra":{"future":{"ok":true}}}`,
			invalidOther: `{"api_version":false}`,
			contains:     "other.api_version",
		},
		{
			name:         "azure speech region",
			channelType:  config.ChannelTypeAzureSpeech,
			validOther:   `{"region":"eastus","vendor_extra":{"future":123}}`,
			invalidOther: `{"region":123}`,
			contains:     "other.region",
		},
		{
			name:         "vertex region",
			channelType:  config.ChannelTypeVertexAI,
			validOther:   `{"region":"us-central1","project_id":"project-a","extra":{"future":123}}`,
			invalidOther: `{"region":123,"project_id":"project-a"}`,
			contains:     "other.region",
		},
		{
			name:         "vertex project id",
			channelType:  config.ChannelTypeVertexAI,
			validOther:   `{"region":"global","project_id":"project-a","vendor_extra":{"future":123}}`,
			invalidOther: `{"region":"global","project_id":123}`,
			contains:     "other.project_id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			valid := &Channel{Type: tc.channelType, Other: tc.validOther}
			if err := valid.ValidateRuntimeConfigJSON(); err != nil {
				t.Fatalf("expected valid provider other to pass, got %v", err)
			}
			invalid := &Channel{Type: tc.channelType, Other: tc.invalidOther}
			err := invalid.ValidateRuntimeConfigJSON()
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected provider other validation error containing %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestValidateAzureSpeechRequiresRegionOrBaseURL(t *testing.T) {
	channel := &Channel{Type: config.ChannelTypeAzureSpeech, Other: ""}
	if err := channel.ValidateRuntimeConfigJSON(); err == nil || !strings.Contains(err.Error(), "other.region or base_url") {
		t.Fatalf("expected Azure Speech without region/base_url to be rejected, got %v", err)
	}

	channel.Other = `{"region":"eastus"}`
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected Azure Speech region to validate, got %v", err)
	}

	channel.Other = ""
	baseURL := "https://custom-speech.example.com"
	channel.BaseURL = &baseURL
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected Azure Speech base_url fallback to validate, got %v", err)
	}
}

func TestValidateProviderKnownOtherFieldsRejectsUnsupportedRuntimeField(t *testing.T) {
	cases := []struct {
		name        string
		channelType int
		other       string
		contains    string
	}{
		{
			name:        "ali typo",
			channelType: config.ChannelTypeAli,
			other:       `{"dashscopePlugin":"plugin-a"}`,
			contains:    "other.dashscopePlugin",
		},
		{
			name:        "gemini typo",
			channelType: config.ChannelTypeGemini,
			other:       `{"apiVersion":"v1"}`,
			contains:    "other.apiVersion",
		},
		{
			name:        "xunfei typo",
			channelType: config.ChannelTypeXunfei,
			other:       `{"apiVersion":"v3.5"}`,
			contains:    "other.apiVersion",
		},
		{
			name:        "azure speech typo",
			channelType: config.ChannelTypeAzureSpeech,
			other:       `{"regionName":"eastus"}`,
			contains:    "other.regionName",
		},
		{
			name:        "vertex typo",
			channelType: config.ChannelTypeVertexAI,
			other:       `{"region":"us-central1","project_id":"project-a","projectId":"project-b"}`,
			contains:    "other.projectId",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel := &Channel{Type: tc.channelType, Other: tc.other}
			err := channel.ValidateRuntimeConfigJSON()
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected unsupported provider other field error containing %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestValidateVertexAIRequiresRegionAndProjectID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		other    string
		contains string
	}{
		{name: "missing both", other: `{}`, contains: "other.region"},
		{name: "missing project", other: `{"region":"us-central1"}`, contains: "other.project_id"},
		{name: "blank region", other: `{"region":" ","project_id":"project-a"}`, contains: "other.region"},
		{name: "blank project", other: `{"region":"global","project_id":" "}`, contains: "other.project_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := &Channel{Type: config.ChannelTypeVertexAI, Other: tc.other}
			err := channel.ValidateRuntimeConfigJSON()
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("expected VertexAI other validation error containing %q, got %v", tc.contains, err)
			}
		})
	}
}
