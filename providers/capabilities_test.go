package providers

import (
	"encoding/json"
	"errors"
	"testing"

	"one-api/common/config"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"

	"gorm.io/datatypes"
)

func TestResolveAdapterSupportUsesAdapterFacts(t *testing.T) {
	openAI := ResolveAdapterSupport(&model.Channel{Type: config.ChannelTypeOpenAI})
	if !openAI.SupportsStoredResponses() || !openAI.Supports(base.OperationResponsesWebSocket) {
		t.Fatalf("expected official OpenAI adapter support, got %+v", openAI)
	}

	customBaseURL := "https://compatible.example"
	custom := ResolveAdapterSupport(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, BaseURL: &customBaseURL})
	if custom.SupportsStoredResponses() || !custom.Supports(base.OperationResponsesCreate) || !custom.Supports(base.OperationResponsesCompact) || !custom.Supports(base.OperationResponsesInputTokens) || custom.Supports(base.OperationResponsesWebSocket) {
		t.Fatalf("expected custom create/token-count support without Stored/WS opt-in, got %+v", custom)
	}
	custom = ResolveAdapterSupport(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, BaseURL: &customBaseURL, Other: `{"responses_stored_lifecycle":true}`})
	if !custom.SupportsStoredResponses() {
		t.Fatalf("expected explicit Stored lifecycle opt-in, got %+v", custom)
	}
	custom = ResolveAdapterSupport(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, BaseURL: &customBaseURL, Other: `{"responses_ws_native":true}`})
	if !custom.Supports(base.OperationResponsesWebSocket) {
		t.Fatalf("expected explicit native WS transport opt-in, got %+v", custom)
	}

	azure := ResolveAdapterSupport(&model.Channel{Type: config.ChannelTypeAzureV1})
	if !azure.SupportsStoredResponses() || !azure.Supports(base.OperationResponsesWebSocket) {
		t.Fatalf("expected Azure adapter support, got %+v", azure)
	}
	if !ResolveAdapterSupport(&model.Channel{Type: config.ChannelTypeCodex}).Supports(base.OperationResponsesCompact) {
		t.Fatal("Codex factory lost its implemented Responses compact operation")
	}
}

func TestResolveAdapterSupportHonorsDisabledCustomResponsesEndpoint(t *testing.T) {
	plugin := datatypes.NewJSONType(model.PluginType{
		"endpoints": {
			"openai.responses": map[string]any{"enabled": false, "upstream_url": ""},
		},
	})
	baseURL := "https://compatible.example"
	channel := &model.Channel{Type: config.ChannelTypeCustom, BaseURL: &baseURL, Plugin: &plugin, Other: `{"responses_ws_native":true}`}
	support := ResolveAdapterSupport(channel)
	for _, operation := range []base.Operation{
		base.OperationResponsesCreate,
		base.OperationResponsesCompact,
		base.OperationResponsesInputTokens,
		base.OperationResponsesRetrieve,
		base.OperationResponsesDelete,
		base.OperationResponsesInputItems,
	} {
		if support.Supports(operation) {
			t.Fatalf("disabled custom Responses endpoint still advertises %s: %+v", operation, support)
		}
	}
	if support.Supports(base.OperationResponsesWebSocket) {
		t.Fatalf("disabled custom Responses endpoint still advertises websocket support: %+v", support)
	}
}

func TestResolveAdapterSupportDoesNotInferUpstreamProductAvailability(t *testing.T) {
	baseURL := "https://compatible.example/v1"
	channel := &model.Channel{Type: config.ChannelTypeOpenAI, BaseURL: &baseURL}
	support := ResolveAdapterSupport(channel)
	if support.SupportsStoredResponses() || !support.Supports(base.OperationResponsesCreate) {
		t.Fatalf("compatible endpoint must require Stored lifecycle opt-in while retaining create, got %+v", support)
	}
	if support.Supports(base.OperationResponsesWebSocket) {
		t.Fatalf("unknown endpoint must require the separate native WS transport opt-in, got %+v", support)
	}
	channel.Other = `{"responses_ws_native":true}`
	if !ResolveAdapterSupport(channel).Supports(base.OperationResponsesWebSocket) {
		t.Fatal("expected native WS opt-in to enable the installed adapter transport")
	}
}

func TestResolveAdapterSupportDoesNotTrustOfficialHostWithPathPrefix(t *testing.T) {
	baseURL := "https://api.openai.com/proxy/v1"
	channel := &model.Channel{Type: config.ChannelTypeOpenAI, BaseURL: &baseURL}
	if ResolveAdapterSupport(channel).Supports(base.OperationResponsesWebSocket) {
		t.Fatal("an OpenAI host with a path prefix is not the registered exact-wire endpoint")
	}
}

func TestResolveAdapterSupportPreservesExplicitCrossProtocolConversion(t *testing.T) {
	support := ResolveAdapterSupport(&model.Channel{
		Type:               config.ChannelTypeAnthropic,
		CompatibleResponse: true,
	})
	if path, ok := support.DataPath(base.OperationResponsesCreate); !ok || path != base.DataPathCrossProtocol {
		t.Fatalf("expected explicit Responses-to-Chat conversion support, got %+v", support)
	}
	if support.SupportsStoredResponses() {
		t.Fatalf("cross-protocol create must not invent a Stored Responses lifecycle, got %+v", support)
	}
}

func TestResolveAdapterSupportDoesNotGiveNonChatFactoryCrossProtocolCreate(t *testing.T) {
	support := ResolveAdapterSupport(&model.Channel{Type: config.ChannelTypeJina, CompatibleResponse: true})
	if support.Supports(base.OperationResponsesCreate) {
		t.Fatalf("non-Chat factory gained Responses-to-Chat support: %+v", support)
	}
}

func TestInheritedOpenAIChatFactoryUsesBillingEvidenceAssessment(t *testing.T) {
	_, err := AssessChatRequest(
		&model.Channel{Type: config.ChannelTypeDeepseek},
		"gpt-5-search-api",
		&types.ChatCompletionRequest{Model: "gpt-5-search-api"},
		nil,
	)
	var capabilityErr *base.RequestCapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Param != "model" {
		t.Fatalf("inherited OpenAI Chat path missed billing-evidence gate: %v", err)
	}
}

func TestResolveAdapterSupportExcludesDataResidencyEndpoint(t *testing.T) {
	for _, channelType := range []int{config.ChannelTypeOpenAI, config.ChannelTypeCustom} {
		baseURL := "https://eu.api.openai.com"
		support := ResolveAdapterSupport(&model.Channel{Type: channelType, BaseURL: &baseURL})
		if len(support.Operations) != 0 {
			t.Fatalf("unsupported channel type %d data-residency endpoint must remain outside the local contract surface, got %+v", channelType, support)
		}
	}
}

func TestValidateResponsesRequestLeavesCodexParameterSemanticsUpstream(t *testing.T) {
	fields := map[string]json.RawMessage{"truncation": json.RawMessage(`"auto"`)}
	if err := ValidateResponsesRequest(
		&model.Channel{Type: config.ChannelTypeCodex},
		base.OperationResponsesCreate,
		fields,
		"gpt-5",
		true,
	); err != nil {
		t.Fatalf("encodable Codex parameters must not become candidate capability errors: %v", err)
	}
	if err := ValidateResponsesRequest(
		&model.Channel{Type: config.ChannelTypeOpenAI},
		base.OperationResponsesCreate,
		fields,
		"gpt-5",
		true,
	); err != nil {
		t.Fatalf("OpenAI channel must remain eligible for truncation: %v", err)
	}
}

func TestValidateResponsesRequestRejectsUnrepresentableCodexCompactFields(t *testing.T) {
	for _, field := range []string{"context_management", "truncation"} {
		t.Run(field, func(t *testing.T) {
			err := ValidateResponsesRequest(
				&model.Channel{Type: config.ChannelTypeCodex},
				base.OperationResponsesCompact,
				map[string]json.RawMessage{field: json.RawMessage(`null`)},
				"gpt-5",
				true,
			)
			var capabilityErr *base.RequestCapabilityError
			if !errors.As(err, &capabilityErr) || capabilityErr.Param != field {
				t.Fatalf("expected compact %s capability error, got %v", field, err)
			}
		})
	}
}

func TestValidateResponsesRequestRejectsLifecycleCustomParameterChanges(t *testing.T) {
	fields := map[string]json.RawMessage{
		"model": json.RawMessage(`"client-model"`),
		"input": json.RawMessage(`"hello"`),
	}
	customParameter := `{"per_model":true,"mapped-model":{"store":false}}`
	channel := &model.Channel{
		Type:            config.ChannelTypeOpenAI,
		CustomParameter: &customParameter,
	}

	err := ValidateResponsesRequest(channel, base.OperationResponsesCreate, fields, "mapped-model", true)
	var capabilityErr *base.RequestCapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Param != "store" {
		t.Fatalf("expected mapped-model store capability error, got %v", err)
	}
	if err := ValidateResponsesRequest(channel, base.OperationResponsesInputTokens, fields, "mapped-model", true); err != nil {
		t.Fatalf("input_tokens inherited an unrelated Stored lifecycle freeze: %v", err)
	}

	fields["store"] = json.RawMessage(`false`)
	if err := ValidateResponsesRequest(channel, base.OperationResponsesCreate, fields, "mapped-model", true); err != nil {
		t.Fatalf("unchanged lifecycle field must remain eligible: %v", err)
	}
}

func TestAzureResponsesRejectsLifecycleCustomParameterChanges(t *testing.T) {
	fields := map[string]json.RawMessage{
		"model": json.RawMessage(`"client-model"`),
		"input": json.RawMessage(`"hello"`),
	}
	customParameter := `{"per_model":true,"mapped-model":{"store":false}}`
	for _, channelType := range []int{config.ChannelTypeAzure, config.ChannelTypeAzureV1} {
		channel := &model.Channel{Type: channelType, CustomParameter: &customParameter}
		err := ValidateResponsesRequest(channel, base.OperationResponsesCreate, fields, "mapped-model", true)
		var capabilityErr *base.RequestCapabilityError
		if !errors.As(err, &capabilityErr) || capabilityErr.Param != "store" {
			t.Fatalf("channel type %d must reject lifecycle-changing custom parameters, got %v", channelType, err)
		}
	}
}
