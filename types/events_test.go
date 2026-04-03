package types

import (
	"testing"

	"one-api/common/config"
)

func TestUsageEventToChatUsagePreservesExtraBilling(t *testing.T) {
	usageEvent := &UsageEvent{
		InputTokens:  3,
		OutputTokens: 5,
		TotalTokens:  8,
		ExtraBilling: map[string]ExtraBilling{
			APIToolTypeWebSearchPreview: {
				Type:      "high",
				CallCount: 1,
			},
		},
	}

	usage := usageEvent.ToChatUsage()
	if usage == nil {
		t.Fatal("expected chat usage")
	}
	billing, ok := usage.ExtraBilling[APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected extra billing to survive UsageEvent conversion, got %+v", usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search tool charge, got %+v", billing)
	}
}

func TestUsageEventMergeAccumulatesExtraBilling(t *testing.T) {
	usage := &UsageEvent{
		ExtraBilling: map[string]ExtraBilling{
			APIToolTypeWebSearchPreview: {
				Type:      "high",
				CallCount: 1,
			},
		},
	}

	usage.Merge(&UsageEvent{
		ExtraBilling: map[string]ExtraBilling{
			APIToolTypeWebSearchPreview: {
				Type:      "high",
				CallCount: 2,
			},
			APIToolTypeCodeInterpreter: {
				CallCount: 1,
			},
		},
	})

	if got := usage.ExtraBilling[APIToolTypeWebSearchPreview].CallCount; got != 3 {
		t.Fatalf("expected web search call counts to accumulate, got %d", got)
	}
	if got := usage.ExtraBilling[APIToolTypeCodeInterpreter].CallCount; got != 1 {
		t.Fatalf("expected code interpreter billing to merge, got %d", got)
	}
}

func TestUsageEventMergeSeparatesImageGenerationVariants(t *testing.T) {
	usage := &UsageEvent{}
	usage.IncExtraBilling(APIToolTypeImageGeneration, "low-1024x1024")
	usage.IncExtraBilling(APIToolTypeImageGeneration, "high-1536x1024")

	lowKey := BuildExtraBillingKey(APIToolTypeImageGeneration, "low-1024x1024")
	highKey := BuildExtraBillingKey(APIToolTypeImageGeneration, "high-1536x1024")
	if len(usage.ExtraBilling) != 2 {
		t.Fatalf("expected image generation variants to be merged into separate billing buckets, got %+v", usage.ExtraBilling)
	}
	if got := usage.ExtraBilling[lowKey].CallCount; got != 1 {
		t.Fatalf("expected low image generation call count to remain isolated, got %d", got)
	}
	if got := usage.ExtraBilling[highKey].CallCount; got != 1 {
		t.Fatalf("expected high image generation call count to remain isolated, got %d", got)
	}
}

func TestUsageEventCloneDeepCopiesExtraMaps(t *testing.T) {
	usage := &UsageEvent{
		InputTokens:  3,
		OutputTokens: 5,
		TotalTokens:  8,
		ExtraTokens: map[string]int{
			"cached": 2,
		},
		ExtraBilling: map[string]ExtraBilling{
			APIToolTypeWebSearchPreview: {
				Type:      "high",
				CallCount: 1,
			},
		},
	}

	cloned := usage.Clone()
	if cloned == nil {
		t.Fatal("expected cloned usage event")
	}

	cloned.ExtraTokens["cached"] = 99
	cloned.ExtraBilling[APIToolTypeWebSearchPreview] = ExtraBilling{
		Type:      "low",
		CallCount: 3,
	}

	if got := usage.ExtraTokens["cached"]; got != 2 {
		t.Fatalf("expected source extra tokens to stay unchanged, got %d", got)
	}
	if got := usage.ExtraBilling[APIToolTypeWebSearchPreview].CallCount; got != 1 {
		t.Fatalf("expected source extra billing to stay unchanged, got %d", got)
	}
}

func TestEventHelpersAndUsageEventExtraTokenAssembly(t *testing.T) {
	sessionEvent := NewSessionCreatedEvent("", "sess_123")
	if sessionEvent == nil || sessionEvent.Type != EventTypeSessionCreated || sessionEvent.Session == nil || sessionEvent.Session.ID != "sess_123" || sessionEvent.EventId == "" {
		t.Fatalf("expected session created helper to populate event metadata, got %+v", sessionEvent)
	}
	if !NewErrorEvent("evt_1", "system_error", "system_error", "boom").IsError() {
		t.Fatal("expected error helper events to report IsError")
	}
	if got := (&Event{Type: EventTypeResponseDone}).Error(); got != "" {
		t.Fatalf("expected events without error detail to have empty string Error() representation, got %q", got)
	}
	if got := NewErrorEvent("evt_2", "system_error", "system_error", "boom").Error(); got == "" {
		t.Fatal("expected error helper events to serialize into a json payload")
	}

	usage := &UsageEvent{
		InputTokenDetails: PromptTokensDetails{
			CachedTokens:      2,
			AudioTokens:       3,
			TextTokens:        4,
			CachedWriteTokens: 5,
			CachedReadTokens:  6,
			ImageTokens:       7,
		},
		OutputTokenDetails: CompletionTokensDetails{
			AudioTokens:     8,
			TextTokens:      9,
			ReasoningTokens: 10,
			ImageTokens:     11,
		},
	}
	extraTokens := usage.GetExtraTokens()
	if extraTokens[APIToolTypeWebSearchPreview] != 0 {
		t.Fatal("expected unrelated extra token keys to remain unset")
	}
	if len(extraTokens) != 3 {
		t.Fatalf("expected usage extra token assembly to expose the supported realtime token buckets, got %+v", extraTokens)
	}
	if extraTokens[config.UsageExtraCache] != 2 || extraTokens[config.UsageExtraInputAudio] != 3 || extraTokens[config.UsageExtraOutputAudio] != 8 {
		t.Fatalf("expected usage extra token assembly to expose cache and audio token buckets, got %+v", extraTokens)
	}
}

func TestUsageEventAdditionalGuardBranches(t *testing.T) {
	var nilUsage *UsageEvent
	if cloned := nilUsage.Clone(); cloned != nil {
		t.Fatalf("expected nil UsageEvent clone to stay nil, got %+v", cloned)
	}

	usage := &UsageEvent{}
	usage.MergeExtraBilling(nil)
	if usage.ExtraBilling != nil {
		t.Fatalf("expected nil extra billing merge to leave UsageEvent empty, got %+v", usage.ExtraBilling)
	}

	usage.MergeExtraBilling(map[string]ExtraBilling{
		"": {CallCount: 1},
	})
	if len(usage.ExtraBilling) != 0 {
		t.Fatalf("expected invalid extra billing keys to be skipped, got %+v", usage.ExtraBilling)
	}

	usage.IncExtraBilling("", "ignored")
	if len(usage.ExtraBilling) != 0 {
		t.Fatalf("expected empty extra billing increments to be ignored, got %+v", usage.ExtraBilling)
	}
}
