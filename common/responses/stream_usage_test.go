package responses

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"one-api/types"
)

func applyResponsesStreamOutputItemBillingForTest(t *testing.T, usage *types.Usage, eventType string, item *types.ResponsesOutput, searchServiceType, searchType string) {
	t.Helper()
	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, eventType, item, "", nil, searchServiceType, searchType, nil); err != nil {
		t.Fatalf("apply stream output billing: %v", err)
	}
}

func TestToolBillingStreamTrackerBoundsUniqueHashedIdentities(t *testing.T) {
	usage := &types.Usage{}
	tracker := &ToolBillingStreamTracker{maxEntries: 1}
	first := &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, ID: strings.Repeat("a", 1<<20), Status: "completed", Action: map[string]any{"type": "search"}}
	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", first, first.ID, nil, "", "medium", tracker); err != nil {
		t.Fatalf("first unique tool identity failed: %v", err)
	}
	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", first, first.ID, nil, "", "medium", tracker); err != nil {
		t.Fatalf("duplicate tool identity consumed capacity: %v", err)
	}
	second := &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, ID: "second", Status: "completed", Action: map[string]any{"type": "search"}}
	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", second, second.ID, nil, "", "medium", tracker); !errors.Is(err, errToolBillingStreamLimit) {
		t.Fatalf("second unique tool identity error=%v, want %v", err, errToolBillingStreamLimit)
	}
	if len(tracker.entries) != 1 {
		t.Fatalf("overflow changed retained identity count: %d", len(tracker.entries))
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if usage.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("overflow or duplicate changed billing count: %+v", usage.ExtraBilling)
	}
}

func TestToolBillingStreamTrackerBoundsAnonymousEventsAndDimensions(t *testing.T) {
	usage := &types.Usage{}
	tracker := &ToolBillingStreamTracker{maxEntries: 1}
	anonymous := &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}
	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", anonymous, "", nil, "", "medium", tracker); err != nil {
		t.Fatalf("first anonymous event failed: %v", err)
	}
	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", anonymous, "", nil, "", "medium", tracker); !errors.Is(err, errToolBillingStreamLimit) {
		t.Fatalf("unbounded anonymous event error=%v, want %v", err, errToolBillingStreamLimit)
	}
	if tracker.entryCount() != 1 {
		t.Fatalf("anonymous overflow changed entry count: %d", tracker.entryCount())
	}

	dimensionTracker := &ToolBillingStreamTracker{maxEntries: 1}
	withID := &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, ID: "ws_1", Status: "completed", Action: map[string]any{"type": "search"}}
	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(&types.Usage{}, "response.output_item.done", withID, withID.ID, nil, "", strings.Repeat("x", maxToolBillingDimensionBytes+1), dimensionTracker); !errors.Is(err, errToolBillingStreamLimit) {
		t.Fatalf("oversized billing dimension error=%v, want %v", err, errToolBillingStreamLimit)
	}
	if dimensionTracker.entryCount() != 0 {
		t.Fatalf("oversized dimension changed tracker before rejection: %d", dimensionTracker.entryCount())
	}
}

func TestToolBillingStreamTrackerDefaultBoundary(t *testing.T) {
	usage := &types.Usage{}
	tracker := &ToolBillingStreamTracker{}
	for index := 0; index < maxToolBillingStreamEntries; index++ {
		itemID := "ws_" + strconv.Itoa(index)
		item := &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, ID: itemID, Status: "completed", Action: map[string]any{"type": "search"}}
		if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", item, itemID, nil, "", "medium", tracker); err != nil {
			t.Fatalf("entry %d within default limit failed: %v", index, err)
		}
	}
	first := &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, ID: "ws_0", Status: "completed", Action: map[string]any{"type": "search"}}
	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", first, first.ID, nil, "", "medium", tracker); err != nil {
		t.Fatalf("duplicate at capacity failed: %v", err)
	}
	overflow := &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, ID: "overflow", Status: "completed", Action: map[string]any{"type": "search"}}
	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", overflow, overflow.ID, nil, "", "medium", tracker); !errors.Is(err, errToolBillingStreamLimit) {
		t.Fatalf("default limit overflow error=%v, want %v", err, errToolBillingStreamLimit)
	}
}

func TestToolBillingStreamTrackerAppliesTerminalFrameAtomically(t *testing.T) {
	usage := &types.Usage{}
	tracker := &ToolBillingStreamTracker{maxEntries: 2}
	prefix := &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, ID: "ws_prefix", Status: "completed", Action: map[string]any{"type": "search"}}
	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", prefix, prefix.ID, nil, "", "medium", tracker); err != nil {
		t.Fatalf("accept prefix billing: %v", err)
	}

	overflow := &types.OpenAIResponsesResponses{Output: []types.ResponsesOutput{
		*prefix,
		{Type: types.InputTypeWebSearchCall, ID: "ws_new_1", Status: "completed", Action: map[string]any{"type": "search"}},
		{Type: types.InputTypeWebSearchCall, ID: "ws_new_2", Status: "completed", Action: map[string]any{"type": "search"}},
	}}
	if err := ApplyResponsesTerminalOutputItemBillingWithToolTracker(usage, overflow, "", "medium", tracker); !errors.Is(err, errToolBillingStreamLimit) {
		t.Fatalf("terminal overflow error=%v, want %v", err, errToolBillingStreamLimit)
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if tracker.entryCount() != 1 || usage.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("failed terminal partially committed: entries=%d usage=%+v", tracker.entryCount(), usage.ExtraBilling)
	}

	accepted := &types.OpenAIResponsesResponses{Output: overflow.Output[:2]}
	if err := ApplyResponsesTerminalOutputItemBillingWithToolTracker(usage, accepted, "", "medium", tracker); err != nil {
		t.Fatalf("terminal within remaining capacity failed: %v", err)
	}
	if tracker.entryCount() != 2 || usage.ExtraBilling[key].CallCount != 2 {
		t.Fatalf("accepted terminal did not commit exactly once: entries=%d usage=%+v", tracker.entryCount(), usage.ExtraBilling)
	}
}

func TestToolBillingStreamTrackerMergesIncrementallyCompletedAliases(t *testing.T) {
	for _, test := range []struct {
		name         string
		prefixItem   types.ResponsesOutput
		prefixID     string
		prefixIndex  *int
		terminalItem types.ResponsesOutput
		terminalAt   int
	}{
		{
			name: "index then ID", prefixItem: types.ResponsesOutput{Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}, prefixIndex: intPointer(4),
			terminalItem: types.ResponsesOutput{ID: "ws_1", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}, terminalAt: 4,
		},
		{
			name: "ID then index", prefixItem: types.ResponsesOutput{ID: "ws_1", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}, prefixID: "ws_1",
			terminalItem: types.ResponsesOutput{ID: "ws_1", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}, terminalAt: 0,
		},
		{
			name: "call ID then item ID", prefixItem: types.ResponsesOutput{CallID: "call_1", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}},
			terminalItem: types.ResponsesOutput{ID: "ws_1", CallID: "call_1", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}, terminalAt: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := &types.Usage{}
			tracker := &ToolBillingStreamTracker{}
			if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", &test.prefixItem, test.prefixID, test.prefixIndex, "", "medium", tracker); err != nil {
				t.Fatalf("accept prefix alias: %v", err)
			}
			outputs := make([]types.ResponsesOutput, test.terminalAt+1)
			outputs[test.terminalAt] = test.terminalItem
			if err := ApplyResponsesTerminalOutputItemBillingWithToolTracker(usage, &types.OpenAIResponsesResponses{Output: outputs}, "", "medium", tracker); err != nil {
				t.Fatalf("complete terminal aliases: %v", err)
			}
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
			if usage.ExtraBilling[key].CallCount != 1 || tracker.entryCount() != 1 {
				t.Fatalf("completed aliases double counted: entries=%d usage=%+v", tracker.entryCount(), usage.ExtraBilling)
			}
		})
	}
}

func TestToolBillingStreamTrackerRejectsIdentityConflictsBeforeMutation(t *testing.T) {
	newItem := func(id string) *types.ResponsesOutput {
		return &types.ResponsesOutput{ID: id, Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}
	}
	for _, test := range []struct {
		name  string
		seed  func(*types.Usage, *ToolBillingStreamTracker) error
		item  *types.ResponsesOutput
		id    string
		index int
	}{
		{name: "top-level and item IDs disagree", item: newItem("ws_item"), id: "ws_top", index: 0},
		{name: "one ID maps to two indexes", seed: func(usage *types.Usage, tracker *ToolBillingStreamTracker) error {
			index := 0
			return ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", newItem("ws_1"), "ws_1", &index, "", "medium", tracker)
		}, item: newItem("ws_1"), id: "ws_1", index: 1},
		{name: "one index maps to two IDs", seed: func(usage *types.Usage, tracker *ToolBillingStreamTracker) error {
			index := 0
			return ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", newItem("ws_1"), "ws_1", &index, "", "medium", tracker)
		}, item: newItem("ws_2"), id: "ws_2", index: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := &types.Usage{}
			tracker := &ToolBillingStreamTracker{}
			if test.seed != nil {
				if err := test.seed(usage, tracker); err != nil {
					t.Fatalf("seed identity: %v", err)
				}
			}
			beforeEntries := tracker.entryCount()
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
			beforeBilling := usage.ExtraBilling[key].CallCount
			if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", test.item, test.id, &test.index, "", "medium", tracker); !errors.Is(err, errToolBillingStreamIdentityConflict) {
				t.Fatalf("identity conflict error=%v, want %v", err, errToolBillingStreamIdentityConflict)
			}
			if tracker.entryCount() != beforeEntries || usage.ExtraBilling[key].CallCount != beforeBilling {
				t.Fatalf("identity conflict changed state: entries=%d usage=%+v", tracker.entryCount(), usage.ExtraBilling)
			}
		})
	}
}

func TestToolBillingStreamTrackerRejectsTerminalAliasConflictAtomically(t *testing.T) {
	usage := &types.Usage{}
	tracker := &ToolBillingStreamTracker{}
	index := 0
	item := &types.ResponsesOutput{ID: "ws_1", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}
	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", item, item.ID, &index, "", "medium", tracker); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	terminal := &types.OpenAIResponsesResponses{Output: []types.ResponsesOutput{
		*item,
		{ID: "ws_1", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}},
	}}
	if err := ApplyResponsesTerminalOutputItemBillingWithToolTracker(usage, terminal, "", "medium", tracker); !errors.Is(err, errToolBillingStreamIdentityConflict) {
		t.Fatalf("terminal alias conflict error=%v, want %v", err, errToolBillingStreamIdentityConflict)
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if tracker.entryCount() != 1 || usage.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("terminal alias conflict partially committed: entries=%d usage=%+v", tracker.entryCount(), usage.ExtraBilling)
	}
}

func intPointer(value int) *int {
	return &value
}

func TestParseStreamUsageEventTracksResponsesEventsOnly(t *testing.T) {
	event, ok := ParseStreamUsageEvent([]byte(`{"type":"response.created","response":{"id":"resp_created"}}`))
	if !ok || event.Type != "response.created" {
		t.Fatalf("expected response lifecycle event to be tracked, got event=%+v ok=%v", event, ok)
	}

	if event, ok = ParseStreamUsageEvent([]byte(`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`)); ok {
		t.Fatalf("text-only event entered usage tracking: %+v", event)
	}

	if event, ok = ParseStreamUsageEvent([]byte(`{"type":"response.done","response":{"id":"resp_done","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)); ok {
		t.Fatalf("expected Realtime response.done not to be tracked as a Responses event, got %+v", event)
	}
}

func TestApplyResponsesStreamOutputItemBillingCoversSharedToolTypes(t *testing.T) {
	usage := &types.Usage{}
	applyResponsesStreamOutputItemBillingForTest(t, usage, "response.output_item.done", &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}, "", "")
	applyResponsesStreamOutputItemBillingForTest(t, usage, "response.output_item.added", &types.ResponsesOutput{Type: types.InputTypeCodeInterpreterCall}, "", "")
	applyResponsesStreamOutputItemBillingForTest(t, usage, "response.output_item.added", &types.ResponsesOutput{Type: types.InputTypeFileSearchCall}, "", "")
	applyResponsesStreamOutputItemBillingForTest(t, usage, "response.output_item.added", &types.ResponsesOutput{Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "high", Size: "1024x1024"}, "", "")
	applyResponsesStreamOutputItemBillingForTest(t, usage, "response.output_item.done", &types.ResponsesOutput{Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "high", Size: "1024x1024"}, "", "")

	webSearchKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if usage.ExtraBilling[webSearchKey].CallCount != 1 {
		t.Fatalf("expected one web search billing event, got %+v", usage.ExtraBilling)
	}
	for _, unsupported := range []string{types.APIToolTypeCodeInterpreter, types.APIToolTypeFileSearch} {
		if _, exists := usage.ExtraBilling[types.BuildExtraBillingKey(unsupported, "")]; exists {
			t.Fatalf("unsupported hosted tool %q produced billing evidence: %+v", unsupported, usage.ExtraBilling)
		}
	}
	if _, ok := usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "high-1024x1024")]; ok {
		t.Fatalf("expected image generation to wait for a successful response terminal, got %+v", usage.ExtraBilling)
	}
}

func TestApplyResponsesStreamOutputItemBillingCommitsCompletedWebSearchOnce(t *testing.T) {
	usage := &types.Usage{}
	tracker := &ToolBillingStreamTracker{}
	outputIndex := 2
	added := &types.ResponsesOutput{ID: "ws_1", Type: types.InputTypeWebSearchCall, Status: "in_progress"}
	done := &types.ResponsesOutput{ID: "ws_1", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}

	_ = ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.added", added, "ws_1", &outputIndex, "", "high", tracker)
	if len(usage.ExtraBilling) != 0 {
		t.Fatalf("in-progress web search must not be billed, got %+v", usage.ExtraBilling)
	}
	_ = ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", done, "ws_1", &outputIndex, "", "high", tracker)
	_ = ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", done, "ws_1", &outputIndex, "", "high", tracker)
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")
	if usage.ExtraBilling[key].CallCount != 1 || !usage.HasProviderExtraBilling(key) {
		t.Fatalf("completed web search must be billed exactly once, got %+v", usage.ExtraBilling)
	}
}

func TestApplyResponsesStreamOutputItemBillingChargesUnknownCompletedActionOnce(t *testing.T) {
	usage := &types.Usage{}
	applyResponsesStreamOutputItemBillingForTest(t, usage, "response.output_item.done", &types.ResponsesOutput{
		Type: types.InputTypeWebSearchCall, Status: "completed",
	}, "", "")

	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if usage.ExtraBilling[key].CallCount != 1 || !usage.HasProviderExtraBilling(key) {
		t.Fatalf("unknown completed web search action must be charged once, got %+v", usage.ExtraBilling)
	}
	if !usage.BillingDiagnostics["web_search_action_unknown"] {
		t.Fatalf("unknown web search action must remain observable, got %+v", usage.BillingDiagnostics)
	}
}

func TestApplyResponsesStreamOutputItemBillingRetainsGAWebSearchService(t *testing.T) {
	usage := &types.Usage{}
	applyResponsesStreamOutputItemBillingForTest(t, usage, "response.output_item.done", &types.ResponsesOutput{
		Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"},
	}, types.APIToolTypeWebSearch, "high")
	if got := usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "high")].CallCount; got != 1 {
		t.Fatalf("expected GA web search billing evidence, got %+v", usage.ExtraBilling)
	}
}

func TestApplyResponsesStreamOutputItemBillingUsesOutputIndexIdentityFallback(t *testing.T) {
	usage := &types.Usage{}
	tracker := &ToolBillingStreamTracker{}
	outputIndex := 4
	done := &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}
	_ = ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", done, "", &outputIndex, "", "", tracker)
	_ = ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", done, "", &outputIndex, "", "", tracker)
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if usage.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("output index fallback must deduplicate one tool item, got %+v", usage.ExtraBilling)
	}
}

func TestApplyResponsesStreamOutputItemBillingSkipsFailedImageGeneration(t *testing.T) {
	usage := &types.Usage{}
	applyResponsesStreamOutputItemBillingForTest(t, usage, "response.output_item.done", &types.ResponsesOutput{
		Type: types.InputTypeImageGenerationCall, Status: "failed", Quality: "high", Size: "1024x1024",
	}, "", "")
	if len(usage.ExtraBilling) != 0 {
		t.Fatalf("expected failed image generation not to be billed, got %+v", usage.ExtraBilling)
	}
}

func TestApplyResponsesStreamOutputItemBillingMergesImageToolEvidence(t *testing.T) {
	usage := &types.Usage{}
	tracker := &ImageGenerationStreamTracker{}
	outputIndex, partialIndex := 1, 0
	tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: "response.created", Response: &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeImageGeneration, Model: "gpt-image-2", Quality: "auto", Size: "auto", PartialImages: 2}}}})
	tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: "response.image_generation_call.partial_image", ItemID: "img_1", OutputIndex: &outputIndex, PartialImageIndex: &partialIndex})
	tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: "response.output_item.done", OutputIndex: &outputIndex, Item: &types.ResponsesOutput{
		ID: "img_1", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "high", Size: "2048x2048",
	}})
	response := &types.OpenAIResponsesResponses{Status: "completed", Tools: []types.ResponsesTools{{Type: types.APIToolTypeImageGeneration, Model: "gpt-image-2", Quality: "auto", Size: "auto", PartialImages: 2}}}
	tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: "response.completed", Response: response})
	ApplyResponsesUsageWithImageTracker(usage, response, tracker)
	key := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-2|high|2048x2048|1")
	if usage.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("expected observed partial image evidence after successful terminal, got %+v", usage.ExtraBilling)
	}
}

func TestApplyResponsesUsageReconcilesStreamingAndTerminalToolBilling(t *testing.T) {
	usage := &types.Usage{}
	usage.IncProviderExtraBilling(types.APIToolTypeWebSearchPreview, "medium")
	usage.AddBillingDiagnostic("stream_diagnostic")

	response := &types.OpenAIResponsesResponses{
		Status: "completed",
		Usage:  &types.ResponsesUsage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8},
		Output: []types.ResponsesOutput{
			{Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}},
			{Type: types.InputTypeFileSearchCall, Status: "completed"},
		},
	}
	ApplyResponsesUsage(usage, response)

	if usage.PromptTokens != 3 || usage.CompletionTokens != 5 || usage.TotalTokens != 8 {
		t.Fatalf("expected terminal token counters, got %+v", usage)
	}
	if usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")].CallCount != 1 {
		t.Fatalf("expected streaming and terminal views not to double bill web search, got %+v", usage.ExtraBilling)
	}
	if !usage.HasProviderExtraBilling(types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")) {
		t.Fatalf("expected provider search evidence to survive terminal usage replacement: %+v", usage)
	}
	if _, exists := usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeFileSearch, "")]; exists {
		t.Fatalf("unsupported terminal-only tool call produced billing evidence, got %+v", usage.ExtraBilling)
	}
	if !usage.BillingDiagnostics["stream_diagnostic"] {
		t.Fatalf("expected stream-only diagnostics to survive terminal usage: %+v", usage.BillingDiagnostics)
	}
}

func TestApplyResponsesUsageWithoutTerminalCountersPreservesObservedBilling(t *testing.T) {
	usage := &types.Usage{PromptTokens: 7}
	usage.IncProviderExtraBilling(types.APIToolTypeWebSearchPreview, "high")
	response := &types.OpenAIResponsesResponses{Status: "completed"}

	ApplyResponsesUsage(usage, response)

	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")
	if usage.PromptTokens != 7 || usage.ExtraBilling[key].CallCount != 1 || !usage.HasProviderExtraBilling(key) {
		t.Fatalf("expected missing terminal usage/output not to erase observations, got %+v", usage)
	}
}

func TestApplyResponsesUsageMarksTerminalSearchWithoutTokenUsage(t *testing.T) {
	usage := &types.Usage{}
	response := &types.OpenAIResponsesResponses{
		Status: "completed",
		Output: []types.ResponsesOutput{{
			ID: "ws_terminal", Type: types.InputTypeWebSearchCall, Status: "completed",
			Action: map[string]any{"type": "search"},
		}},
	}

	ApplyResponsesUsage(usage, response)

	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if usage.ExtraBilling[key].CallCount != 1 || !usage.HasProviderExtraBilling(key) {
		t.Fatalf("terminal search without token usage lost provider evidence: %+v", usage)
	}
}
