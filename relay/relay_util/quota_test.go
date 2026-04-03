package relay_util

import (
	"context"
	"testing"
	"time"

	"one-api/model"
	"one-api/types"

	"gorm.io/datatypes"
)

type quotaContextKey string

func TestQuotaCloneClearsManagedTurnTiming(t *testing.T) {
	startedAt := time.Unix(1700000000, 0)
	firstResponseAt := startedAt.Add(250 * time.Millisecond)
	completedAt := startedAt.Add(2 * time.Second)

	quota := &Quota{}
	quota.SeedTiming(startedAt, firstResponseAt, completedAt)

	cloned := quota.Clone()
	if cloned == nil {
		t.Fatal("expected quota clone")
	}
	if !cloned.startTime.IsZero() {
		t.Fatalf("expected cloned quota start time to reset, got %v", cloned.startTime)
	}
	if !cloned.firstResponseTime.IsZero() {
		t.Fatalf("expected cloned quota first response time to reset, got %v", cloned.firstResponseTime)
	}
	if cloned.requestFrozen {
		t.Fatal("expected cloned quota request duration override to reset")
	}
	if cloned.requestDuration != 0 {
		t.Fatalf("expected cloned quota request duration override to reset, got %v", cloned.requestDuration)
	}
}

func TestQuotaSeedTimingFreezesManagedTurnLatency(t *testing.T) {
	startedAt := time.Unix(1700000000, 0)
	firstResponseAt := startedAt.Add(375 * time.Millisecond)
	completedAt := startedAt.Add(3250 * time.Millisecond)

	quota := &Quota{}
	quota.SeedTiming(startedAt, firstResponseAt, completedAt)

	if got := quota.getRequestTime(); got != int(completedAt.Sub(startedAt).Milliseconds()) {
		t.Fatalf("expected frozen request time %d, got %d", completedAt.Sub(startedAt).Milliseconds(), got)
	}
	if got := quota.GetFirstResponseTime(); got != firstResponseAt.Sub(startedAt).Milliseconds() {
		t.Fatalf("expected frozen first response %d, got %d", firstResponseAt.Sub(startedAt).Milliseconds(), got)
	}
}

func TestQuotaSeedTimingRejectsInvalidBounds(t *testing.T) {
	startedAt := time.Unix(1700000000, 0)
	firstResponseAt := startedAt.Add(-250 * time.Millisecond)
	completedAt := startedAt.Add(-time.Second)

	quota := &Quota{}
	quota.SeedTiming(startedAt, firstResponseAt, completedAt)

	if got := quota.GetFirstResponseTime(); got != 0 {
		t.Fatalf("expected invalid first response timing to be ignored, got %d", got)
	}
	if got := quota.getRequestTime(); got != 0 {
		t.Fatalf("expected invalid request duration to clamp to zero, got %d", got)
	}
}

func TestQuotaCloneDetachesRequestContextCancellation(t *testing.T) {
	parent := context.WithValue(context.Background(), quotaContextKey("trace_id"), "trace-123")
	parent, cancel := context.WithCancel(parent)

	quota := &Quota{requestContext: parent}
	cloned := quota.Clone()
	if cloned == nil {
		t.Fatal("expected quota clone")
	}

	cancel()

	select {
	case <-cloned.requestContext.Done():
		t.Fatal("expected cloned quota context to ignore parent cancellation")
	default:
	}

	if got := cloned.requestContext.Value(quotaContextKey("trace_id")); got != "trace-123" {
		t.Fatalf("expected cloned quota context to preserve values, got %#v", got)
	}
}

func TestQuotaCloneDeepCopiesPricePointers(t *testing.T) {
	extraRatios := datatypes.NewJSONType(map[string]float64{
		"custom_ratio": 1.5,
	})
	quota := &Quota{
		price: model.Price{
			ExtraRatios: &extraRatios,
			ModelInfo: &model.ModelInfoResponse{
				InputModalities:  []string{"text"},
				OutputModalities: []string{"text"},
				Tags:             []string{"codex"},
				SupportUrl:       []string{"https://example.com"},
			},
		},
	}

	cloned := quota.Clone()
	if cloned == nil {
		t.Fatal("expected quota clone")
	}
	if cloned.price.ExtraRatios == quota.price.ExtraRatios {
		t.Fatal("expected clone to own a distinct ExtraRatios pointer")
	}
	if cloned.price.ModelInfo == quota.price.ModelInfo {
		t.Fatal("expected clone to own a distinct ModelInfo pointer")
	}

	clonedRatios := cloned.price.ExtraRatios.Data()
	clonedRatios["custom_ratio"] = 3.25
	cloned.price.ModelInfo.InputModalities[0] = "image"

	originalRatios := quota.price.ExtraRatios.Data()
	if got := originalRatios["custom_ratio"]; got != 1.5 {
		t.Fatalf("expected original price ratios to remain unchanged, got %v", got)
	}
	if got := quota.price.ModelInfo.InputModalities[0]; got != "text" {
		t.Fatalf("expected original model info slice to remain unchanged, got %q", got)
	}
}

func TestQuotaGetExtraBillingDataSeparatesImageGenerationVariantPricing(t *testing.T) {
	quota := &Quota{modelName: "gpt-image-1"}
	lowKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "low-1024x1024")
	highKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "high-1536x1024")

	quota.GetExtraBillingData(map[string]types.ExtraBilling{
		lowKey: {
			ServiceType: types.APIToolTypeImageGeneration,
			Type:        "low-1024x1024",
			CallCount:   1,
		},
		highKey: {
			ServiceType: types.APIToolTypeImageGeneration,
			Type:        "high-1536x1024",
			CallCount:   1,
		},
	})

	if len(quota.extraBillingData) != 2 {
		t.Fatalf("expected distinct image generation variants to produce separate priced entries, got %+v", quota.extraBillingData)
	}
	if got := quota.extraBillingData[lowKey]; got.ServiceType != types.APIToolTypeImageGeneration || got.Price != 0.011 {
		t.Fatalf("expected low variant to keep its own price, got %+v", got)
	}
	if got := quota.extraBillingData[highKey]; got.ServiceType != types.APIToolTypeImageGeneration || got.Price != 0.25 {
		t.Fatalf("expected high variant to keep its own price, got %+v", got)
	}
}
