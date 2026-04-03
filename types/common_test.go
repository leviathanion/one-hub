package types

import "testing"

func TestUsageExtraBillingHelperBranches(t *testing.T) {
	merged := mergeExtraTokensMap(nil, map[string]int{
		"cached": 2,
		"image":  1,
	})
	if merged["cached"] != 2 || merged["image"] != 1 {
		t.Fatalf("expected mergeExtraTokensMap to initialize and copy source values, got %+v", merged)
	}

	if got := BuildExtraBillingKey("", "high"); got != "" {
		t.Fatalf("expected empty service type to produce an empty billing key, got %q", got)
	}
	if got := ResolveExtraBillingType(" image_generation| high-1024x1024 ", ExtraBilling{}); got != "high-1024x1024" {
		t.Fatalf("expected billing type to be resolved from keyed variants, got %q", got)
	}
	if got := ResolveExtraBillingType("web_search_preview", ExtraBilling{}); got != "" {
		t.Fatalf("expected non-variant billing keys to have no derived type, got %q", got)
	}

	usage := &Usage{}
	usage.MergeExtraBilling(map[string]ExtraBilling{
		"": {
			CallCount: 1,
		},
		BuildExtraBillingKey(APIToolTypeImageGeneration, "high-1024x1024"): {
			CallCount: 2,
		},
	})
	if len(usage.ExtraBilling) != 1 {
		t.Fatalf("expected invalid extra billing keys to be skipped, got %+v", usage.ExtraBilling)
	}
	if got := usage.ExtraBilling[BuildExtraBillingKey(APIToolTypeImageGeneration, "high-1024x1024")].CallCount; got != 2 {
		t.Fatalf("expected valid extra billing keys to merge, got %d", got)
	}

	usage.IncExtraBilling("", "")
	if len(usage.ExtraBilling) != 1 {
		t.Fatalf("expected empty extra billing increments to be ignored, got %+v", usage.ExtraBilling)
	}
}

func TestUsageExtraBillingAdditionalBranches(t *testing.T) {
	if cloned := cloneExtraBillingMap(nil); cloned != nil {
		t.Fatalf("expected nil extra billing clone to stay nil, got %+v", cloned)
	}
	if got := ResolveExtraBillingServiceType("ignored", ExtraBilling{ServiceType: "custom-service"}); got != "custom-service" {
		t.Fatalf("expected explicit service type to win, got %q", got)
	}

	usage := &Usage{}
	usage.MergeExtraBilling(nil)
	if usage.ExtraBilling != nil {
		t.Fatalf("expected merging nil extra billing to stay nil, got %+v", usage.ExtraBilling)
	}

	imageKey := BuildExtraBillingKey(APIToolTypeImageGeneration, "high-1024x1024")
	usage.MergeExtraBilling(map[string]ExtraBilling{
		imageKey: {CallCount: 1},
	})
	entry := usage.ExtraBilling[imageKey]
	if entry.ServiceType != APIToolTypeImageGeneration || entry.Type != "high-1024x1024" || entry.CallCount != 1 {
		t.Fatalf("expected merge to backfill service/type for keyed billing entries, got %+v", entry)
	}
}
