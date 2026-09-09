package codex

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/types"
)

func TestCodexResolveUsageEventRequiresCompleteProviderTokenPartition(t *testing.T) {
	for name, raw := range map[string]string{
		"missing output": `{"usage":{"input_tokens":3,"total_tokens":3}}`,
		"null output":    `{"usage":{"input_tokens":3,"output_tokens":null,"total_tokens":3}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var response types.OpenAIResponsesResponses
			if err := json.Unmarshal([]byte(raw), &response); err != nil {
				t.Fatal(err)
			}
			usage := newCodexTurnUsageAccumulator().ResolveUsageEvent(&response)
			if usage == nil || usage.ProviderTokenEvidence {
				t.Fatalf("partial provider partition was authorized: %+v", usage)
			}
		})
	}

	var response types.OpenAIResponsesResponses
	if err := json.Unmarshal([]byte(`{"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`), &response); err != nil {
		t.Fatal(err)
	}
	usage := newCodexTurnUsageAccumulator().ResolveUsageEvent(&response)
	if usage == nil || !usage.ProviderTokenEvidence {
		t.Fatalf("complete provider partition was not authorized: %+v", usage)
	}
}

func TestCodexResolveUsagePreservesProviderReportedZeroFields(t *testing.T) {
	for name, raw := range map[string]string{
		"zero output": `{"usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3},"output":[{"type":"message","content":[{"type":"output_text","text":"must not be estimated"}]}]}`,
		"zero total":  `{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0},"output":[{"type":"message","content":[{"type":"output_text","text":"must not change total"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var response types.OpenAIResponsesResponses
			if err := json.Unmarshal([]byte(raw), &response); err != nil {
				t.Fatal(err)
			}
			usage := newCodexTurnUsageAccumulator().ResolveUsageEvent(&response)
			if usage == nil || !usage.ProviderTokenEvidence {
				t.Fatalf("complete provider zero partition was not authorized: %+v", usage)
			}
			if name == "zero output" && (usage.OutputTokens != 0 || usage.TotalTokens != 3) {
				t.Fatalf("provider output zero was overwritten: %+v", usage)
			}
			if name == "zero total" && usage.TotalTokens != 0 {
				t.Fatalf("provider total zero was overwritten: %+v", usage)
			}
		})
	}
}

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
		Type:     "response.created",
		Response: &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}}},
	})
	outputIndex := 0
	accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type:        "response.output_item.added",
		OutputIndex: &outputIndex,
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
		Model:       "gpt-5.6-terra-2026-08-01",
		ServiceTier: "flex",
		Output: []types.ResponsesOutput{
			{
				Type:    types.InputTypeMessage,
				Role:    types.ChatMessageRoleAssistant,
				Content: []types.ContentResponses{{Type: types.ContentTypeOutputText, Text: "assistant reply"}},
			},
			{Type: types.InputTypeWebSearchCall, ID: "ws_1", Status: "completed", Action: map[string]any{"type": "search"}},
		},
		Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}},
	}
	if err := accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: "response.completed", Response: response}); err != nil {
		t.Fatalf("observe terminal billing: %v", err)
	}
	resolvedUsage := accumulator.ResolveUsage(response)
	if resolvedUsage == nil || resolvedUsage.PromptTokens != 5 || resolvedUsage.CompletionTokens != 7 || resolvedUsage.TotalTokens != 12 {
		t.Fatalf("expected observed responses usage to win, got %+v", resolvedUsage)
	}
	if resolvedUsage.ResponseModel != response.Model || resolvedUsage.ServiceTier != response.ServiceTier {
		t.Fatalf("expected terminal response model and service tier on resolved usage, got %+v", resolvedUsage)
	}
	if resolvedUsage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected resolved usage to preserve tool billing, got %+v", resolvedUsage.ExtraBilling)
	}

	usageEvent := accumulator.ResolveUsageEvent(response)
	if usageEvent == nil || usageEvent.InputTokens != 5 || usageEvent.TotalTokens != 12 {
		t.Fatalf("expected resolved usage event to mirror resolved usage, got %+v", usageEvent)
	}
	if usageEvent.ResponseModel != response.Model || usageEvent.ServiceTier != response.ServiceTier {
		t.Fatalf("expected terminal response model and service tier on usage event, got %+v", usageEvent)
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
	if err := accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.created", Response: &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}}},
	}); err != nil {
		t.Fatalf("observe search configuration: %v", err)
	}

	webSearch := &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, ID: "ws_1", Status: "completed", Action: map[string]any{"type": "search"}}
	webOutputIndex := 1
	webEvent := &types.OpenAIResponsesStreamResponses{Type: "response.output_item.done", Item: webSearch, OutputIndex: &webOutputIndex}
	if err := accumulator.ObserveEvent(webEvent); err != nil {
		t.Fatalf("observe web search: %v", err)
	}
	if err := accumulator.ObserveEvent(webEvent); err != nil {
		t.Fatalf("observe duplicate web search: %v", err)
	}
	if accumulator.toolUsage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected duplicate web search tool billing to be deduped, got %+v", accumulator.toolUsage.ExtraBilling)
	}
	gaAccumulator := newCodexTurnUsageAccumulator()
	gaAccumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.created",
		Response: &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{
			Type: types.APIToolTypeWebSearch, SearchContextSize: "high",
		}}},
	})
	gaAccumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.output_item.done", ItemID: "ws_ga",
		Item: &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}},
	})
	if gaAccumulator.toolUsage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "high")].CallCount != 1 {
		t.Fatalf("expected Codex GA web search billing evidence, got %+v", gaAccumulator.toolUsage.ExtraBilling)
	}

	rejectedWebSearches := newCodexTurnUsageAccumulator()
	for index, output := range []types.ResponsesOutput{
		{Type: types.InputTypeWebSearchCall, Status: "in_progress", Action: map[string]any{"type": "search"}},
		{Type: types.InputTypeWebSearchCall, Status: "failed", Action: map[string]any{"type": "search"}},
		{Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "open_page"}},
		{Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "find_in_page"}},
	} {
		outputIndex := index
		if err := rejectedWebSearches.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: "response.output_item.done", Item: &output, OutputIndex: &outputIndex}); err != nil {
			t.Fatalf("observe non-billable web action: %v", err)
		}
	}
	if len(rejectedWebSearches.toolUsage.ExtraBilling) != 0 {
		t.Fatalf("expected non-search or unsuccessful web actions not to be billed, got %+v", rejectedWebSearches.toolUsage.ExtraBilling)
	}
	unknownWebSearch := &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "future_action"}}
	unknownOutputIndex := 9
	if err := rejectedWebSearches.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: "response.output_item.done", Item: unknownWebSearch, OutputIndex: &unknownOutputIndex}); err != nil {
		t.Fatalf("observe unknown web action: %v", err)
	}
	unknownKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if rejectedWebSearches.toolUsage.ExtraBilling[unknownKey].CallCount != 1 || !rejectedWebSearches.toolUsage.BillingDiagnostics["web_search_action_unknown"] {
		t.Fatalf("expected unknown successful action to bill once with diagnostic, usage=%+v", rejectedWebSearches.toolUsage)
	}

	codeInterpreter := &types.ResponsesOutput{Type: types.InputTypeCodeInterpreterCall, CallID: "call_ci"}
	fileSearch := &types.ResponsesOutput{Type: types.InputTypeFileSearchCall, Name: "search_files"}
	_ = accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: "response.output_item.done", Item: codeInterpreter})
	_ = accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: "response.output_item.done", Item: fileSearch})
	for _, unsupported := range []string{types.APIToolTypeCodeInterpreter, types.APIToolTypeFileSearch} {
		if _, exists := accumulator.toolUsage.ExtraBilling[types.BuildExtraBillingKey(unsupported, "")]; exists {
			t.Fatalf("unsupported hosted tool %q produced billing evidence: %+v", unsupported, accumulator.toolUsage.ExtraBilling)
		}
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

}

func TestCodexTurnUsageAccumulatorBillsCompletedWebSearchBeforeTerminal(t *testing.T) {
	accumulator := newCodexTurnUsageAccumulator()
	if err := accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.created", Response: &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}}},
	}); err != nil {
		t.Fatalf("observe search configuration: %v", err)
	}
	outputIndex := 3
	accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type:        "response.output_item.added",
		ItemID:      "ws_done",
		OutputIndex: &outputIndex,
		Item:        &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, Status: "in_progress"},
	})
	if usage := accumulator.BillingUsageEvent(); usage != nil {
		t.Fatalf("in-progress search must not produce billing evidence, got %+v", usage)
	}
	done := &types.OpenAIResponsesStreamResponses{
		Type:        "response.output_item.done",
		ItemID:      "ws_done",
		OutputIndex: &outputIndex,
		Item:        &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}},
	}
	accumulator.ObserveEvent(done)
	accumulator.ObserveEvent(done)
	usage := accumulator.BillingUsageEvent()
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")
	if usage == nil || usage.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("completed search must produce one billing increment, got %+v", usage)
	}
	if repeated := accumulator.BillingUsageEvent(); repeated != nil {
		t.Fatalf("unchanged cumulative evidence must not be emitted twice, got %+v", repeated)
	}
}

func TestCodexTurnUsageAccumulatorEmitsToolBillingDeltas(t *testing.T) {
	accumulator := newCodexTurnUsageAccumulator()
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	for index, itemID := range []string{"ws_1", "ws_2"} {
		outputIndex := index
		accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
			Type:        "response.output_item.done",
			ItemID:      itemID,
			OutputIndex: &outputIndex,
			Item:        &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, ID: itemID, Status: "completed", Action: map[string]any{"type": "search"}},
		})
		usage := accumulator.BillingUsageEvent()
		if usage == nil || usage.ExtraBilling[key].CallCount != 1 {
			t.Fatalf("tool %d must emit one incremental charge, got %+v", index+1, usage)
		}
	}
}

func TestCodexTurnUsageAccumulatorBoundsToolIdentities(t *testing.T) {
	accumulator := newCodexTurnUsageAccumulator()
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	for index := 0; index < 1024; index++ {
		outputIndex := index
		itemID := "ws_" + strconv.Itoa(index)
		err := accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
			Type: "response.output_item.done", ItemID: itemID, OutputIndex: &outputIndex,
			Item: &types.ResponsesOutput{ID: itemID, Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}},
		})
		if err != nil {
			t.Fatalf("tool identity %d within limit failed: %v", index, err)
		}
	}
	overflowIndex := 1024
	err := accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.output_item.done", ItemID: "ws_overflow", OutputIndex: &overflowIndex,
		Item: &types.ResponsesOutput{ID: "ws_overflow", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "future_action"}},
	})
	var apiErr *types.OpenAIErrorWithStatusCode
	if !errors.As(err, &apiErr) || apiErr.Code != "provider_usage_state_limit" || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("unexpected tool identity overflow error: %#v", err)
	}
	if accumulator.toolUsage.ExtraBilling[key].CallCount != 1024 {
		t.Fatalf("overflow changed cumulative billing: %+v", accumulator.toolUsage.ExtraBilling)
	}
	if len(accumulator.toolUsage.BillingDiagnostics) != 0 {
		t.Fatalf("overflowing event changed diagnostics: %+v", accumulator.toolUsage.BillingDiagnostics)
	}
}

func TestCodexTurnUsageAccumulatorRejectsTerminalToolOverflowAtomically(t *testing.T) {
	accumulator := newCodexTurnUsageAccumulator()
	prefixIndex := 0
	prefix := &types.ResponsesOutput{ID: "ws_prefix", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}
	if err := accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: "response.output_item.done", Item: prefix, OutputIndex: &prefixIndex}); err != nil {
		t.Fatalf("accept prefix tool event: %v", err)
	}
	outputs := make([]types.ResponsesOutput, 0, 1025)
	outputs = append(outputs, *prefix)
	for index := 0; index < 1024; index++ {
		outputs = append(outputs, types.ResponsesOutput{ID: "ws_terminal_" + strconv.Itoa(index), Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}})
	}
	err := accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.completed", Response: &types.OpenAIResponsesResponses{Status: "completed", Output: outputs},
	})
	var apiErr *types.OpenAIErrorWithStatusCode
	if !errors.As(err, &apiErr) || apiErr.Code != "provider_usage_state_limit" {
		t.Fatalf("unexpected terminal overflow error: %#v", err)
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if accumulator.toolUsage.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("failed terminal partially entered cumulative billing: %+v", accumulator.toolUsage.ExtraBilling)
	}
	usage := accumulator.BillingUsageEvent()
	if usage == nil || usage.ExtraBilling[key].CallCount != 1 || accumulator.BillingUsageEvent() != nil {
		t.Fatalf("error flush did not contain exactly the accepted prefix: %+v", usage)
	}
}

func TestCodexTurnUsageAccumulatorRejectsToolAliasConflictAsProviderProtocolError(t *testing.T) {
	accumulator := newCodexTurnUsageAccumulator()
	outputIndex := 0
	err := accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.output_item.done", ItemID: "ws_top", OutputIndex: &outputIndex,
		Item: &types.ResponsesOutput{ID: "ws_item", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}},
	})
	var apiErr *types.OpenAIErrorWithStatusCode
	if !errors.As(err, &apiErr) || apiErr.Code != "provider_protocol_error" || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("unexpected tool alias conflict error: %#v", err)
	}
	if len(accumulator.toolUsage.ExtraBilling) != 0 {
		t.Fatalf("tool alias conflict changed accumulator state: %+v", accumulator)
	}
}

func TestCodexTurnUsageAccumulatorBoundsAndAllowsSearchDimensionChanges(t *testing.T) {
	oversized := newCodexTurnUsageAccumulator()
	err := oversized.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.created", Response: &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: strings.Repeat("x", 257)}}},
	})
	var apiErr *types.OpenAIErrorWithStatusCode
	if !errors.As(err, &apiErr) || apiErr.Code != "provider_usage_state_limit" || oversized.searchServiceType != "" || oversized.searchType != "" {
		t.Fatalf("oversized search dimension was retained: err=%#v accumulator=%+v", err, oversized)
	}

	changing := newCodexTurnUsageAccumulator()
	if err := changing.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.created", Response: &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}}},
	}); err != nil {
		t.Fatalf("accept initial search dimension: %v", err)
	}
	firstIndex := 0
	if err := changing.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.output_item.done", ItemID: "ws_high", OutputIndex: &firstIndex,
		Item: &types.ResponsesOutput{ID: "ws_high", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}},
	}); err != nil {
		t.Fatalf("bill with initial search dimension: %v", err)
	}
	err = changing.ObserveEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.created", Response: &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "low"}}},
	})
	secondIndex := 1
	if err == nil {
		err = changing.ObserveEvent(&types.OpenAIResponsesStreamResponses{
			Type: "response.output_item.done", ItemID: "ws_low", OutputIndex: &secondIndex,
			Item: &types.ResponsesOutput{ID: "ws_low", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}},
		})
	}
	searchKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "low")
	if err != nil || changing.searchType != "low" || changing.toolUsage.ExtraBilling[searchKey].CallCount != 2 {
		t.Fatalf("bounded search dimension change was not accepted: err=%#v accumulator=%+v", err, changing)
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
	if usage := nilAccumulator.ResolveUsage(nil); usage != nil {
		t.Fatalf("expected nil response usage resolution to stay nil, got %+v", usage)
	}
	if usageEvent := nilAccumulator.ResolveUsageEvent(nil); usageEvent != nil {
		t.Fatalf("expected nil response usage-event resolution to stay nil, got %+v", usageEvent)
	}
	if seed := nilAccumulator.seedResponsesUsage(); seed != nil {
		t.Fatalf("expected nil accumulator seed usage to stay nil, got %+v", seed)
	}

	accumulator := newCodexTurnUsageAccumulator()
	accumulator.SeedPromptFromRequest(nil, 0)
	accumulator.ObserveEvent(nil)

	resolved := accumulator.ResolveUsage(&types.OpenAIResponsesResponses{})
	if resolved == nil {
		t.Fatal("expected zeroed responses usage to still resolve into an OpenAI usage shell")
	}

	fallbackResponse := &types.OpenAIResponsesResponses{Output: []types.ResponsesOutput{{Type: types.InputTypeMessage, Content: []types.ContentResponses{{Type: types.ContentTypeOutputText, Text: "must not be estimated"}}}}}
	fallbackUsage := accumulator.ResolveUsage(fallbackResponse)
	if fallbackUsage == nil || fallbackResponse.Usage == nil || fallbackUsage.CompletionTokens != 0 || fallbackUsage.ProviderReported {
		t.Fatalf("provider-missing content produced authoritative token usage: usage=%+v response=%+v", fallbackUsage, fallbackResponse.Usage)
	}

	if usageEvent := accumulator.ResolveUsageEvent(&types.OpenAIResponsesResponses{}); usageEvent == nil {
		t.Fatal("expected ResolveUsageEvent to mirror zeroed usage resolution")
	}

}

func TestCodexImageBillingProjectionIsIdempotent(t *testing.T) {
	accumulator := newCodexTurnUsageAccumulator()
	response := &types.OpenAIResponsesResponses{
		Status: "completed",
		Usage:  &types.ResponsesUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		Tools: []types.ResponsesTools{{
			Type: types.APIToolTypeImageGeneration, Model: "gpt-image-1-mini", Quality: "low", Size: "1024x1024",
		}},
		Output: []types.ResponsesOutput{{
			ID: "img_1", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "low", Size: "1024x1024",
		}, {
			ID: "ws_1", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"},
		}},
	}
	outputIndex := 0
	accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: "response.created", Response: response})
	accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: "response.output_item.done", Item: &response.Output[0], OutputIndex: &outputIndex})
	accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: "response.completed", Response: response})
	key := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-1-mini|low|1024x1024|0")
	first := accumulator.ResolveUsage(response)
	second := accumulator.ResolveUsage(response)
	if first.ExtraBilling[key].CallCount != 1 || second.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("expected repeated terminal projection to remain one image call, first=%+v second=%+v", first.ExtraBilling, second.ExtraBilling)
	}
	searchKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if first.ExtraBilling[searchKey].CallCount != 1 || second.ExtraBilling[searchKey].CallCount != 1 {
		t.Fatalf("expected image and terminal-only web billing to coexist once, first=%+v second=%+v", first.ExtraBilling, second.ExtraBilling)
	}
}

func TestCodexPrivateDoneIsNormalizedBeforeImageBilling(t *testing.T) {
	accumulator := newCodexTurnUsageAccumulator()
	response := &types.OpenAIResponsesResponses{
		Status: "completed",
		Usage:  &types.ResponsesUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		Tools: []types.ResponsesTools{{
			Type: types.APIToolTypeImageGeneration, Model: "gpt-image-1-mini", Quality: "low", Size: "1024x1024",
		}},
		Output: []types.ResponsesOutput{{
			ID: "img_private_done", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "low", Size: "1024x1024",
		}},
	}
	accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: types.EventTypeResponseDone, Response: response})
	usage := accumulator.ResolveUsage(response)
	key := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-1-mini|low|1024x1024|0")
	if usage == nil || usage.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("expected private response.done image billing, got %+v", usage)
	}
}
