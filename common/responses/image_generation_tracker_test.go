package responses

import (
	"errors"
	"strings"
	"testing"

	"one-api/types"
)

func TestImageGenerationStreamTrackerBoundsOnlyImageState(t *testing.T) {
	tracker := &ImageGenerationStreamTracker{maxState: 1}
	web := &types.OpenAIResponsesStreamResponses{
		Type:   "response.output_item.done",
		ItemID: strings.Repeat("w", maxImageGenerationIdentifierBytes+1),
		Item:   &types.ResponsesOutput{Type: types.InputTypeWebSearchCall, ID: strings.Repeat("w", maxImageGenerationIdentifierBytes+1), Status: "completed"},
	}
	if err := tracker.ObserveResponsesEvent(web); err != nil || tracker.observedState != 0 {
		t.Fatalf("non-image event consumed image state: state=%d err=%v", tracker.observedState, err)
	}

	outputIndex, firstPartial, secondPartial := 0, 0, 1
	if err := tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.image_generation_call.partial_image", ItemID: "img_1", OutputIndex: &outputIndex, PartialImageIndex: &firstPartial,
	}); err != nil {
		t.Fatalf("first image event failed: %v", err)
	}
	if err := tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.image_generation_call.partial_image", ItemID: "img_1", OutputIndex: &outputIndex, PartialImageIndex: &secondPartial,
	}); !errors.Is(err, errImageGenerationStreamLimit) {
		t.Fatalf("image event overflow error=%v, want %v", err, errImageGenerationStreamLimit)
	}
	if tracker.observedState != 1 || tracker.PartialImageCount(&types.ResponsesOutput{ID: "img_1"}, &outputIndex) != 1 {
		t.Fatalf("overflow partially changed image state: %+v", tracker)
	}
}

func TestImageGenerationStreamTrackerRejectsOversizedStateBeforeMutation(t *testing.T) {
	tracker := &ImageGenerationStreamTracker{maxState: 1}
	oversizedConfig := &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{
		Type: types.APIToolTypeImageGeneration, Model: strings.Repeat("m", maxImageGenerationConfiguredTypeBytes+1),
	}}}
	if err := tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: "response.created", Response: oversizedConfig}); !errors.Is(err, errImageGenerationStreamLimit) {
		t.Fatalf("oversized configured type error=%v, want %v", err, errImageGenerationStreamLimit)
	}
	if tracker.observedState != 0 || tracker.configuredType != "" {
		t.Fatalf("oversized configured type changed tracker: %+v", tracker)
	}

	tracker = &ImageGenerationStreamTracker{maxState: 1}
	terminal := &types.OpenAIResponsesResponses{Status: "completed", Output: []types.ResponsesOutput{
		{ID: "img_1", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "high", Size: "1024x1024"},
		{ID: "img_2", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "high", Size: "1024x1024"},
	}}
	if err := tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: "response.completed", Response: terminal}); !errors.Is(err, errImageGenerationStreamLimit) {
		t.Fatalf("multi-output terminal overflow error=%v, want %v", err, errImageGenerationStreamLimit)
	}
	if tracker.observedState != 0 || tracker.terminalEventType != "" {
		t.Fatalf("terminal overflow partially changed tracker: %+v", tracker)
	}
}

func TestImageGenerationStreamTrackerRejectsConflictingAliasesBeforeMutation(t *testing.T) {
	outputIndex, otherOutputIndex, partialIndex := 0, 1, 0
	tests := []struct {
		name    string
		tracker func() *ImageGenerationStreamTracker
		event   *types.OpenAIResponsesStreamResponses
	}{
		{
			name:    "top-level and item IDs disagree",
			tracker: func() *ImageGenerationStreamTracker { return &ImageGenerationStreamTracker{} },
			event: &types.OpenAIResponsesStreamResponses{
				Type: "response.output_item.done", ItemID: "img_top", OutputIndex: &outputIndex,
				Item: &types.ResponsesOutput{ID: "img_item", Type: types.InputTypeImageGenerationCall, Status: "completed"},
			},
		},
		{
			name: "one output index maps to two items",
			tracker: func() *ImageGenerationStreamTracker {
				tracker := &ImageGenerationStreamTracker{}
				if err := tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{
					Type: "response.image_generation_call.partial_image", ItemID: "img_a", OutputIndex: &outputIndex, PartialImageIndex: &partialIndex,
				}); err != nil {
					t.Fatalf("seed alias: %v", err)
				}
				return tracker
			},
			event: &types.OpenAIResponsesStreamResponses{
				Type: "response.image_generation_call.partial_image", ItemID: "img_b", OutputIndex: &outputIndex, PartialImageIndex: &partialIndex,
			},
		},
		{
			name: "one item maps to two output indexes",
			tracker: func() *ImageGenerationStreamTracker {
				tracker := &ImageGenerationStreamTracker{}
				if err := tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{
					Type: "response.image_generation_call.partial_image", ItemID: "img_a", OutputIndex: &outputIndex, PartialImageIndex: &partialIndex,
				}); err != nil {
					t.Fatalf("seed alias: %v", err)
				}
				return tracker
			},
			event: &types.OpenAIResponsesStreamResponses{
				Type: "response.image_generation_call.partial_image", ItemID: "img_a", OutputIndex: &otherOutputIndex, PartialImageIndex: &partialIndex,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracker := test.tracker()
			stateBefore := tracker.observedState
			if err := tracker.ObserveResponsesEvent(test.event); !errors.Is(err, errImageGenerationStreamIdentityConflict) {
				t.Fatalf("identity conflict error=%v, want %v", err, errImageGenerationStreamIdentityConflict)
			}
			bindingsChanged := false
			if stateBefore == 0 {
				bindingsChanged = len(tracker.outputByItemID) != 0 || len(tracker.itemIDByOutput) != 0
			} else {
				bindingsChanged = len(tracker.outputByItemID) != 1 || len(tracker.itemIDByOutput) != 1 ||
					tracker.outputByItemID["img_a"] != outputIndex || tracker.itemIDByOutput[outputIndex] != "img_a"
			}
			if tracker.observedState != stateBefore || bindingsChanged {
				t.Fatalf("identity conflict changed tracker state: %+v", tracker)
			}
		})
	}
}

func TestImageGenerationStreamTrackerRejectsDuplicateTerminalIdentity(t *testing.T) {
	tracker := &ImageGenerationStreamTracker{}
	response := &types.OpenAIResponsesResponses{Status: "completed", Output: []types.ResponsesOutput{
		{ID: "img_duplicate", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "high", Size: "1024x1024"},
		{ID: "img_duplicate", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "high", Size: "1024x1024"},
	}}
	if err := tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: "response.completed", Response: response}); !errors.Is(err, errImageGenerationStreamIdentityConflict) {
		t.Fatalf("duplicate terminal identity error=%v, want %v", err, errImageGenerationStreamIdentityConflict)
	}
	if tracker.observedState != 0 || tracker.terminalEventType != "" {
		t.Fatalf("duplicate terminal identity changed tracker state: %+v", tracker)
	}
	usage := &types.Usage{}
	tracker.ApplyExtraBilling(response, usage)
	if len(usage.ExtraBilling) != 0 {
		t.Fatalf("duplicate terminal identity produced billing: %+v", usage.ExtraBilling)
	}
}

func TestImageGenerationStreamTrackerPreservesNonBillableResponseOutputs(t *testing.T) {
	for _, test := range []struct {
		name      string
		eventType string
		status    string
		terminal  string
	}{
		{name: "failed terminal", eventType: "response.failed", status: "failed", terminal: "response.failed"},
		{name: "incomplete terminal", eventType: "response.incomplete", status: "incomplete", terminal: "response.incomplete"},
		{name: "completed event with failed status", eventType: "response.completed", status: "failed", terminal: "response.completed"},
		{name: "created snapshot", eventType: "response.created", status: "in_progress"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := &ImageGenerationStreamTracker{}
			outputIndex, partialIndex := 0, 0
			if err := tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{
				Type: "response.image_generation_call.partial_image", ItemID: "img_existing", OutputIndex: &outputIndex, PartialImageIndex: &partialIndex,
			}); err != nil {
				t.Fatalf("seed image alias: %v", err)
			}
			response := &types.OpenAIResponsesResponses{
				Status: test.status,
				Output: []types.ResponsesOutput{
					{ID: "img_conflict", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: strings.Repeat("q", maxImageGenerationDimensionBytes+1)},
					{ID: "img_conflict", Type: types.InputTypeImageGenerationCall, Status: "completed", Size: strings.Repeat("s", maxImageGenerationDimensionBytes+1)},
				},
			}
			if test.eventType != "response.created" {
				response.Tools = []types.ResponsesTools{{Type: types.APIToolTypeImageGeneration, Model: strings.Repeat("m", maxImageGenerationConfiguredTypeBytes+1)}}
			}
			if err := tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: test.eventType, Response: response}); err != nil {
				t.Fatalf("non-billable response output changed provider semantics: %v", err)
			}
			if tracker.observedState != 1 || tracker.terminalEventType != test.terminal {
				t.Fatalf("non-billable response changed image state: %+v", tracker)
			}
			usage := &types.Usage{}
			tracker.ApplyExtraBilling(response, usage)
			if len(usage.ExtraBilling) != 0 {
				t.Fatalf("non-billable response produced image billing: %+v", usage.ExtraBilling)
			}
		})
	}
}

func TestImageGenerationStreamTrackerIgnoresFailedImageItemState(t *testing.T) {
	tracker := &ImageGenerationStreamTracker{}
	outputIndex := 0
	event := &types.OpenAIResponsesStreamResponses{
		Type: "response.output_item.done", ItemID: "img_top", OutputIndex: &outputIndex,
		Item:     &types.ResponsesOutput{ID: "img_item", Type: types.InputTypeImageGenerationCall, Status: "failed", Quality: strings.Repeat("q", maxImageGenerationDimensionBytes+1)},
		Response: &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeImageGeneration, Model: strings.Repeat("m", maxImageGenerationConfiguredTypeBytes+1)}}},
	}
	if err := tracker.ObserveResponsesEvent(event); err != nil {
		t.Fatalf("failed image item changed provider semantics: %v", err)
	}
	if tracker.observedState != 0 || len(tracker.outputByItemID) != 0 || len(tracker.completedByItemID) != 0 {
		t.Fatalf("failed image item changed billing state: %+v", tracker)
	}
}

func TestImageGenerationStreamTrackerRejectsOversizedOutputDimensionsBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name    string
		quality string
		size    string
	}{
		{name: "quality", quality: strings.Repeat("q", maxImageGenerationDimensionBytes+1), size: "1024x1024"},
		{name: "size", quality: "high", size: strings.Repeat("s", maxImageGenerationDimensionBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			accepted := &ImageGenerationStreamTracker{}
			acceptedOutputIndex := 0
			acceptedEvent := &types.OpenAIResponsesStreamResponses{Type: "response.output_item.done", OutputIndex: &acceptedOutputIndex, Item: &types.ResponsesOutput{
				ID: "img_boundary", Type: types.InputTypeImageGenerationCall, Status: "completed",
				Quality: strings.Repeat("q", maxImageGenerationDimensionBytes), Size: strings.Repeat("s", maxImageGenerationDimensionBytes),
			}}
			if err := accepted.ObserveResponsesEvent(acceptedEvent); err != nil {
				t.Fatalf("exact dimension boundary rejected: %v", err)
			}

			tracker := &ImageGenerationStreamTracker{}
			outputIndex := 0
			event := &types.OpenAIResponsesStreamResponses{Type: "response.output_item.done", OutputIndex: &outputIndex, Item: &types.ResponsesOutput{
				ID: "img_1", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: test.quality, Size: test.size,
			}}
			if err := tracker.ObserveResponsesEvent(event); !errors.Is(err, errImageGenerationStreamLimit) {
				t.Fatalf("oversized %s error=%v, want %v", test.name, err, errImageGenerationStreamLimit)
			}
			if tracker.observedState != 0 || len(tracker.completedByItemID) != 0 {
				t.Fatalf("oversized %s changed tracker state: %+v", test.name, tracker)
			}
		})
	}
}

func TestImageGenerationStreamTrackerStoresCompactCompletedFact(t *testing.T) {
	tracker := &ImageGenerationStreamTracker{}
	outputIndex := 0
	item := &types.ResponsesOutput{
		ID: "img_1", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "high", Size: "1024x1024",
		Content: strings.Repeat("content", 1024), Result: strings.Repeat("result", 1024), Outputs: strings.Repeat("outputs", 1024),
	}
	if err := tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: "response.output_item.done", Item: item, OutputIndex: &outputIndex}); err != nil {
		t.Fatalf("remember completed image output: %v", err)
	}
	fact := tracker.completedByItemID[item.ID]
	if fact.ID != item.ID || fact.Quality != item.Quality || fact.Size != item.Size {
		t.Fatalf("compact image billing fact mismatch: %+v", fact)
	}
}

func TestImageGenerationStreamTrackerCountsDistinctPartialsPerItem(t *testing.T) {
	tracker := &ImageGenerationStreamTracker{}
	createdResponse := &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{
		Type: types.APIToolTypeImageGeneration, Model: "gpt-image-2", Quality: "auto", Size: "auto", PartialImages: 3,
	}}}
	tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: "response.created", Response: createdResponse})

	outputA, outputB := 0, 1
	partial0, partial1 := 0, 1
	partial := func(itemID string, outputIndex, partialIndex *int) {
		tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{
			Type: "response.image_generation_call.partial_image", ItemID: itemID, OutputIndex: outputIndex, PartialImageIndex: partialIndex,
		})
	}
	partial("img_a", &outputA, &partial0)
	partial("img_a", &outputA, &partial0) // provider replay
	partial("img_a", &outputA, &partial1)
	partial("img_b", &outputB, &partial0)

	tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.output_item.done", OutputIndex: &outputA,
		Item: &types.ResponsesOutput{ID: "img_a", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "high", Size: "1024x1024"},
	})
	tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.output_item.done", OutputIndex: &outputB,
		Item: &types.ResponsesOutput{ID: "img_b", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "high", Size: "1024x1024"},
	})

	usage := &types.Usage{}
	if len(usage.ExtraBilling) != 0 {
		t.Fatalf("expected output-item completion not to commit billing before a terminal, got %+v", usage.ExtraBilling)
	}
	terminal := &types.OpenAIResponsesResponses{Status: "completed"}
	tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: "response.completed", Response: terminal})
	tracker.ApplyExtraBilling(terminal, usage)

	wantA := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-2|high|1024x1024|2")
	wantB := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-2|high|1024x1024|1")
	if usage.ExtraBilling[wantA].CallCount != 1 || usage.ExtraBilling[wantB].CallCount != 1 {
		t.Fatalf("expected replay dedupe and per-item counts 2 and 1, got %+v", usage.ExtraBilling)
	}
	requestLimitKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-2|high|1024x1024|3")
	if _, exists := usage.ExtraBilling[requestLimitKey]; exists {
		t.Fatalf("expected request partial_images limit not to become billing evidence, got %+v", usage.ExtraBilling)
	}
}

func TestImageGenerationStreamTrackerRequiresSuccessfulTerminal(t *testing.T) {
	tests := []struct {
		name          string
		terminalEvent string
		status        string
	}{
		{name: "failed", terminalEvent: "response.failed", status: "failed"},
		{name: "incomplete", terminalEvent: "response.incomplete", status: "incomplete"},
		{name: "realtime terminal", terminalEvent: "response.done", status: "completed"},
		{name: "no terminal", status: "in_progress"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracker := &ImageGenerationStreamTracker{}
			outputIndex, partialIndex := 0, 0
			tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{
				Type:     "response.created",
				Response: &types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeImageGeneration, Model: "gpt-image-2", Quality: "high", Size: "1024x1024", PartialImages: 3}}},
			})
			tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: "response.image_generation_call.partial_image", ItemID: "img_1", OutputIndex: &outputIndex, PartialImageIndex: &partialIndex})
			tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{
				Type: "response.output_item.done", OutputIndex: &outputIndex,
				Item: &types.ResponsesOutput{ID: "img_1", Type: types.InputTypeImageGenerationCall, Status: "completed", Quality: "high", Size: "1024x1024"},
			})

			terminal := &types.OpenAIResponsesResponses{Status: test.status}
			if test.terminalEvent != "" {
				tracker.ObserveResponsesEvent(&types.OpenAIResponsesStreamResponses{Type: test.terminalEvent, Response: terminal})
			}
			usage := &types.Usage{}
			tracker.ApplyExtraBilling(terminal, usage)
			if len(usage.ExtraBilling) != 0 {
				t.Fatalf("expected %s path not to commit image billing, got %+v", test.name, usage.ExtraBilling)
			}
		})
	}
}
