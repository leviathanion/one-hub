package codex

import (
	"testing"

	"one-api/common/config"
	"one-api/types"
)

func TestCodexTurnUsageAccumulatorHelpers(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
	})

	if newCodexTurnUsageAccumulator() == nil {
		t.Fatal("expected accumulator constructor to return instance")
	}

	accumulator := newCodexTurnUsageAccumulator()
	accumulator.SeedFromUsage(&types.Usage{
		PromptTokens:        3,
		PromptTokensDetails: types.PromptTokensDetails{CachedTokens: 2},
	})
	if accumulator.seedPromptTokens != 3 || accumulator.seedPromptTokenDetails.CachedTokens != 2 {
		t.Fatalf("expected usage seed to populate prompt counters, got %+v", accumulator)
	}

	request := &types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hello"}
	accumulator.SeedPromptFromRequest(request, 0)
	if accumulator.seedPromptTokens == 0 {
		t.Fatal("expected prompt seed from request to count prompt tokens")
	}
	accumulator.SeedPromptFromRequest(&types.OpenAIResponsesRequest{Model: "", Input: "ignored"}, 0)

	accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type:     "response.output_text.delta",
		Delta:    "assistant delta",
		Response: &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}}},
	})
	accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type:        "response.output_item.added",
		OutputIndex: intPtr(0),
		Item:        &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, ID: "ws_1"},
	})
	accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.updated",
		Response: &types.OpenAIResponsesResponses{
			Usage: &types.ResponsesUsage{
				InputTokens:  5,
				OutputTokens: 7,
				TotalTokens:  12,
			},
		},
	})

	response := &types.OpenAIResponsesResponses{
		Output: []types.ResponsesOutput{
			{
				Type:    types.InputTypeMessage,
				Role:    types.ChatMessageRoleAssistant,
				Content: []types.ContentResponses{{Type: types.ContentTypeOutputText, Text: "assistant reply"}},
			},
			{Type: types.InputTypeWebSearchCall, ID: "ws_1", Status: "completed"},
		},
		Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}},
	}
	resolvedUsage := accumulator.ResolveUsage(response, "gpt-5", true)
	if resolvedUsage == nil || resolvedUsage.PromptTokens != 5 || resolvedUsage.CompletionTokens != 7 || resolvedUsage.TotalTokens != 12 {
		t.Fatalf("expected observed responses usage to win, got %+v", resolvedUsage)
	}
	if resolvedUsage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected resolved usage to preserve tool billing, got %+v", resolvedUsage.ExtraBilling)
	}

	usageEvent := accumulator.ResolveUsageEvent(response, "gpt-5", true)
	if usageEvent == nil || usageEvent.InputTokens != 5 || usageEvent.TotalTokens != 12 {
		t.Fatalf("expected resolved usage event to mirror resolved usage, got %+v", usageEvent)
	}
	usageEvent.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")] = types.ExtraBilling{ServiceType: types.APIToolTypeWebSearchPreview, Type: "high", CallCount: 9}
	if resolvedUsage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected resolved usage event extra billing clone, got %+v", resolvedUsage.ExtraBilling)
	}

	seedOnly := (&codexTurnUsageAccumulator{
		seedPromptTokens:       9,
		seedPromptTokenDetails: types.PromptTokensDetails{CachedTokens: 4},
	}).seedResponsesUsage()
	if seedOnly == nil || seedOnly.InputTokens != 9 || seedOnly.TotalTokens != 9 || seedOnly.InputTokensDetails == nil || seedOnly.InputTokensDetails.CachedTokens != 4 {
		t.Fatalf("expected seedResponsesUsage to convert prompt seed into responses usage, got %+v", seedOnly)
	}
}

func TestCodexTurnUsageAccumulatorBillingDescriptors(t *testing.T) {
	accumulator := newCodexTurnUsageAccumulator()
	accumulator.searchType = "high"

	webSearch := &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, ID: "ws_1"}
	accumulator.observeToolItem(webSearch, intPtr(1), 0)
	accumulator.observeToolItem(webSearch, intPtr(1), 0)
	if accumulator.extraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected duplicate web search tool billing to be deduped, got %+v", accumulator.extraBilling)
	}

	codeInterpreter := &types.ResponsesOutput{Type: types.InputTypeCodeInterpreterCall, CallID: "call_ci"}
	fileSearch := &types.ResponsesOutput{Type: types.InputTypeFileSearchCall, Name: "search_files"}
	imageGeneration := &types.ResponsesOutput{Type: types.InputTypeImageGenerationCall, Quality: "low", Size: "512x512"}
	accumulator.observeToolItem(codeInterpreter, nil, 2)
	accumulator.observeToolItem(fileSearch, nil, 3)
	accumulator.observeToolItem(imageGeneration, nil, 4)
	if accumulator.extraBilling[types.BuildExtraBillingKey(types.APIToolTypeCodeInterpreter, "")].CallCount != 1 {
		t.Fatalf("expected code interpreter billing, got %+v", accumulator.extraBilling)
	}
	if accumulator.extraBilling[types.BuildExtraBillingKey(types.APIToolTypeFileSearch, "")].CallCount != 1 {
		t.Fatalf("expected file search billing, got %+v", accumulator.extraBilling)
	}
	if accumulator.extraBilling[types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "low-512x512")].CallCount != 1 {
		t.Fatalf("expected image generation billing, got %+v", accumulator.extraBilling)
	}

	if key, billingKey, billingType, ok := codexToolBillingDescriptor(nil, nil, 0, ""); ok || key != "" || billingKey != "" || billingType != "" {
		t.Fatalf("expected nil tool billing descriptor to be empty, key=%q billing_key=%q billing_type=%q ok=%v", key, billingKey, billingType, ok)
	}
	if key, billingKey, billingType, ok := codexToolBillingDescriptor(&types.ResponsesOutput{Type: types.InputTypeWebSearchCall}, nil, 6, ""); !ok || key != "type:web_search_call:ordinal:6" || billingKey != types.APIToolTypeWebSearchPreview || billingType != "medium" {
		t.Fatalf("expected default web search descriptor, key=%q billing_key=%q billing_type=%q ok=%v", key, billingKey, billingType, ok)
	}
	if key := codexToolBillingItemKey(&types.ResponsesOutput{ID: "id_1"}, nil, 0); key != "id:id_1" {
		t.Fatalf("expected id-based billing item key, got %q", key)
	}
	if key := codexToolBillingItemKey(&types.ResponsesOutput{CallID: "call_1"}, nil, 0); key != "call:call_1" {
		t.Fatalf("expected call-based billing item key, got %q", key)
	}
	if key := codexToolBillingItemKey(&types.ResponsesOutput{Type: types.InputTypeFileSearchCall}, intPtr(5), 0); key != "index:5:type:file_search_call" {
		t.Fatalf("expected index-based billing item key, got %q", key)
	}
	if key := codexToolBillingItemKey(&types.ResponsesOutput{Type: types.InputTypeFileSearchCall, Name: "find"}, nil, 7); key != "type:file_search_call:name:find:ordinal:7" {
		t.Fatalf("expected name-based billing item key, got %q", key)
	}
	if key := codexToolBillingItemKey(&types.ResponsesOutput{Type: types.InputTypeFileSearchCall}, nil, 8); key != "type:file_search_call:ordinal:8" {
		t.Fatalf("expected ordinal-based billing item key, got %q", key)
	}

	response := &types.OpenAIResponsesResponses{
		Output: []types.ResponsesOutput{
			{Type: types.InputTypeWebSearchCall, ID: "ws_1", Status: "completed"},
			{Type: types.InputTypeCodeInterpreterCall, CallID: "call_ci", Status: "completed"},
		},
		Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}},
	}
	resolved := resolveCodexExtraBilling(response, accumulator)
	if len(resolved) < 2 {
		t.Fatalf("expected resolveCodexExtraBilling to merge observed output billing, got %+v", resolved)
	}

	if clone := cloneCodexResponsesUsage(nil); clone != nil {
		t.Fatalf("expected nil responses usage clone, got %+v", clone)
	}
	usage := &types.ResponsesUsage{
		InputTokens: 1,
		InputTokensDetails: &types.ResponsesUsageInputTokensDetails{
			CachedTokens: 2,
		},
		OutputTokensDetails: &types.ResponsesUsageOutputTokensDetails{
			ReasoningTokens: 3,
		},
	}
	cloned := cloneCodexResponsesUsage(usage)
	cloned.InputTokensDetails.CachedTokens = 9
	cloned.OutputTokensDetails.ReasoningTokens = 8
	if usage.InputTokensDetails.CachedTokens != 2 || usage.OutputTokensDetails.ReasoningTokens != 3 {
		t.Fatalf("expected cloned responses usage details to be detached, got %+v", usage)
	}

	value := intPtr(12)
	if value == nil || *value != 12 {
		t.Fatalf("expected intPtr helper to allocate integer, got %v", value)
	}
}

func TestCodexTurnUsageAccumulatorNilAndFallbackBranches(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
	})

	var nilAccumulator *codexTurnUsageAccumulator
	nilAccumulator.SeedPromptFromRequest(nil, 0)
	nilAccumulator.ObserveEvent(nil)
	nilAccumulator.observeToolItem(nil, nil, 0)
	if usage := nilAccumulator.ResolveUsage(nil, "gpt-5", true); usage != nil {
		t.Fatalf("expected nil response usage resolution to stay nil, got %+v", usage)
	}
	if usageEvent := nilAccumulator.ResolveUsageEvent(nil, "gpt-5", true); usageEvent != nil {
		t.Fatalf("expected nil response usage-event resolution to stay nil, got %+v", usageEvent)
	}
	if seed := nilAccumulator.seedResponsesUsage(); seed != nil {
		t.Fatalf("expected nil accumulator seed usage to stay nil, got %+v", seed)
	}

	accumulator := newCodexTurnUsageAccumulator()
	accumulator.SeedPromptFromRequest(nil, 0)
	accumulator.ObserveEvent(nil)

	resolved := accumulator.ResolveUsage(&types.OpenAIResponsesResponses{}, "gpt-5", false)
	if resolved == nil {
		t.Fatal("expected zeroed responses usage to still resolve into an OpenAI usage shell")
	}

	accumulator.textBuilder.WriteString("assistant fallback text")
	fallbackResponse := &types.OpenAIResponsesResponses{}
	fallbackUsage := accumulator.ResolveUsage(fallbackResponse, "gpt-5", true)
	if fallbackUsage == nil || fallbackResponse.Usage == nil {
		t.Fatalf("expected text-builder fallback to still produce usage metadata, usage=%+v response=%+v", fallbackUsage, fallbackResponse.Usage)
	}

	if usageEvent := accumulator.ResolveUsageEvent(&types.OpenAIResponsesResponses{}, "gpt-5", false); usageEvent == nil {
		t.Fatal("expected ResolveUsageEvent to mirror zeroed usage resolution")
	}

	if resolvedBilling := resolveCodexExtraBilling(&types.OpenAIResponsesResponses{}, nil); resolvedBilling != nil {
		t.Fatalf("expected nil accumulator extra billing resolution to stay nil for empty responses, got %+v", resolvedBilling)
	}
	if resolvedBilling := resolveCodexExtraBilling(&types.OpenAIResponsesResponses{}, accumulator); resolvedBilling != nil {
		t.Fatalf("expected empty response outputs to keep accumulator billing nil, got %+v", resolvedBilling)
	}
	if key := codexToolBillingItemKey(nil, nil, 0); key != "" {
		t.Fatalf("expected nil billing item key helper to stay empty, got %q", key)
	}
}
