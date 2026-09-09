package responses

import (
	"errors"
	"testing"

	"one-api/types"
)

func TestIssue048CompletedSearchTrackerSurvivesTerminalUsageReplacement(t *testing.T) {
	usage := &types.Usage{}
	tracker := &ToolBillingStreamTracker{}
	item := &types.ResponsesOutput{
		ID: "ws_i048", Type: types.InputTypeWebSearchCall, Status: "completed",
		Action: map[string]any{"type": "search"},
	}

	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", item, item.ID, nil, "", "medium", tracker); err != nil {
		t.Fatalf("record completed search: %v", err)
	}
	if err := ApplyResponsesStreamOutputItemBillingWithToolTracker(usage, "response.output_item.done", item, item.ID, nil, "", "medium", tracker); err != nil {
		t.Fatalf("deduplicate completed search: %v", err)
	}

	terminalResponse := &types.OpenAIResponsesResponses{
		Status: "completed",
		Usage:  &types.ResponsesUsage{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
	}
	ApplyResponsesUsageWithImageTracker(usage, terminalResponse, nil)
	ApplyResponsesUsageWithImageTracker(usage, terminalResponse, nil)

	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if usage.ExtraBilling[key].CallCount != 1 || !usage.HasProviderExtraBilling(key) {
		t.Fatalf("completed search evidence was lost or double-counted: %+v", usage)
	}
}

func TestIssue048ImageTrackerWaitsForSuccessfulTerminal(t *testing.T) {
	tracker := &ImageGenerationStreamTracker{}
	outputIndex := 0
	item := &types.ResponsesOutput{
		ID: "img_i048", Type: types.InputTypeImageGenerationCall, Status: "completed",
		Quality: "medium", Size: "1024x1024",
	}
	if err := tracker.ObserveUsageEvent(StreamUsageEvent{
		Type: "response.output_item.done", Item: item, ItemID: item.ID, OutputIndex: &outputIndex,
	}); err != nil {
		t.Fatalf("record image output item: %v", err)
	}

	failedUsage := &types.Usage{}
	tracker.ApplyExtraBilling(&types.OpenAIResponsesResponses{
		Status: "failed", Output: []types.ResponsesOutput{*item},
	}, failedUsage)
	if len(failedUsage.ExtraBilling) != 0 {
		t.Fatalf("failed image terminal produced billing: %+v", failedUsage.ExtraBilling)
	}

	successTracker := &ImageGenerationStreamTracker{}
	if err := successTracker.ObserveUsageEvent(StreamUsageEvent{
		Type: "response.output_item.done", Item: item, ItemID: item.ID, OutputIndex: &outputIndex,
	}); err != nil {
		t.Fatalf("record image output item for success: %v", err)
	}
	successUsage := &types.Usage{}
	successResponse := &types.OpenAIResponsesResponses{
		Status: "completed", Tools: []types.ResponsesTools{{
			Type: types.APIToolTypeImageGeneration, Model: "gpt-image-1-mini", Quality: "medium", Size: "1024x1024",
		}}, Output: []types.ResponsesOutput{*item},
	}
	successTracker.ObserveUsageEvent(StreamUsageEvent{Type: "response.completed", Response: successResponse})
	ApplyResponsesUsageWithImageTracker(successUsage, successResponse, successTracker)
	ApplyResponsesUsageWithImageTracker(successUsage, successResponse, successTracker)
	key := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-1-mini|medium|1024x1024|0")
	if successUsage.ExtraBilling[key].CallCount != 1 || !successUsage.HasProviderExtraBilling(key) {
		t.Fatalf("successful image terminal did not produce trusted billing: %+v", successUsage)
	}
}

func TestIssue048TerminalToolEvidenceIsAtomic(t *testing.T) {
	usage := &types.Usage{}
	tracker := &ToolBillingStreamTracker{maxEntries: 1}
	response := &types.OpenAIResponsesResponses{Output: []types.ResponsesOutput{
		{ID: "ws_atomic_1", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}},
		{ID: "ws_atomic_2", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}},
	}}

	if err := ApplyResponsesTerminalOutputItemBillingWithToolTracker(usage, response, "", "high", tracker); !errors.Is(err, errToolBillingStreamLimit) {
		t.Fatalf("terminal overflow error=%v, want %v", err, errToolBillingStreamLimit)
	}
	if tracker.entryCount() != 0 || len(usage.ExtraBilling) != 0 || len(usage.ProviderExtraBilling) != 0 {
		t.Fatalf("failed terminal partially committed trusted evidence: entries=%d usage=%+v", tracker.entryCount(), usage)
	}
}
