package relay_util

import (
	"math"
	"testing"

	"one-api/types"
)

func TestGetDefaultExtraServicePriceOnlyPricesSupportedHostedServices(t *testing.T) {
	if got := getDefaultExtraServicePrice(types.APIToolTypeWebSearch, "gpt-4o", "high"); got != 0.01 {
		t.Fatalf("expected GA web search price 0.01, got %v", got)
	}
	if got := getDefaultExtraServicePrice(types.APIToolTypeWebSearchPreview, "gpt-4o", "high"); got != 0.025 {
		t.Fatalf("expected legacy non-reasoning preview price 0.025, got %v", got)
	}
	if got := getDefaultExtraServicePrice(types.APIToolTypeFileSearch, "future-model", ""); got != 0 {
		t.Fatalf("unsupported file search must not retain a dormant price, got %v", got)
	}
	if got := getDefaultExtraServicePrice(types.APIToolTypeCodeInterpreter, "future-model", ""); got != 0 {
		t.Fatalf("unsupported code interpreter must not retain a dormant price, got %v", got)
	}
}

func TestGPTImage2PricingUsesTokenFormula(t *testing.T) {
	tests := []struct {
		name      string
		variant   string
		wantPrice float64
	}{
		{name: "low square", variant: "gpt-image-2|low|1024x1024|0", wantPrice: 0.00588},
		{name: "medium square", variant: "gpt-image-2|medium|1024x1024|0", wantPrice: 0.05268},
		{name: "high square", variant: "gpt-image-2|high|1024x1024|0", wantPrice: 0.21072},
		{name: "snapshot", variant: "gpt-image-2-2026-04-14|high|1024x1024|0", wantPrice: 0.21072},
		{name: "arbitrary valid size", variant: "gpt-image-2|high|2048x2048|0", wantPrice: 0.42816},
		{name: "partial images", variant: "gpt-image-2|low|1024x1024|2", wantPrice: 0.01188},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := getDefaultExtraServicePrice(types.APIToolTypeImageGeneration, "gpt-5.6", test.variant)
			if math.Abs(got-test.wantPrice) > 1e-12 {
				t.Fatalf("expected price %.8f, got %.8f", test.wantPrice, got)
			}
		})
	}
}

func TestGPTImage2OversizedDimensionsFallBackBeforeIntegerMultiplication(t *testing.T) {
	oversized := gptImage2BasePrice("high", "137438953472x137438953472")
	conservative := gptImage2BasePrice("high", "invalid")
	if oversized != conservative || math.IsNaN(oversized) || math.IsInf(oversized, 0) || oversized <= 0 {
		t.Fatalf("expected oversized dimensions to use the finite conservative price before multiplication, oversized=%v conservative=%v", oversized, conservative)
	}
}

func TestGPTImage2FlexibleSizePixelBoundary(t *testing.T) {
	if _, _, ok := parseGPTImage2Size("640x1024"); !ok {
		t.Fatal("expected the documented 655,360-pixel boundary to be accepted")
	}
	if _, _, ok := parseGPTImage2Size("640x640"); ok {
		t.Fatal("expected a 640x640 image below the documented pixel minimum to be rejected")
	}
}

func TestGPTImage1MiniPricing(t *testing.T) {
	tests := []struct {
		quality   string
		size      string
		wantPrice float64
	}{
		{quality: "low", size: "1024x1024", wantPrice: 0.005},
		{quality: "low", size: "1024x1536", wantPrice: 0.006},
		{quality: "low", size: "1536x1024", wantPrice: 0.006},
		{quality: "medium", size: "1024x1024", wantPrice: 0.011},
		{quality: "medium", size: "1024x1536", wantPrice: 0.015},
		{quality: "medium", size: "1536x1024", wantPrice: 0.015},
		{quality: "high", size: "1024x1024", wantPrice: 0.036},
		{quality: "high", size: "1024x1536", wantPrice: 0.052},
		{quality: "high", size: "1536x1024", wantPrice: 0.052},
	}

	for _, test := range tests {
		t.Run(test.quality+"/"+test.size, func(t *testing.T) {
			variant := "gpt-image-1-mini|" + test.quality + "|" + test.size + "|0"
			got := getDefaultExtraServicePrice(types.APIToolTypeImageGeneration, "gpt-5.6", variant)
			if math.Abs(got-test.wantPrice) > 1e-12 {
				t.Fatalf("expected price %.8f, got %.8f", test.wantPrice, got)
			}
		})
	}
}

func TestImageGenerationPartialPricingUsesSelectedModelRate(t *testing.T) {
	tests := []struct {
		variant   string
		wantPrice float64
	}{
		{variant: "gpt-image-1.5|low|1024x1024|1", wantPrice: 0.0122},
		{variant: "gpt-image-1|low|1024x1024|1", wantPrice: 0.015},
		{variant: "gpt-image-1-mini|low|1024x1024|1", wantPrice: 0.0058},
	}
	for _, test := range tests {
		got := getDefaultExtraServicePrice(types.APIToolTypeImageGeneration, "gpt-5.6", test.variant)
		if math.Abs(got-test.wantPrice) > 1e-12 {
			t.Fatalf("%s: expected price %.8f, got %.8f", test.variant, test.wantPrice, got)
		}
	}
}

func TestGPTImage15Pricing(t *testing.T) {
	tests := []struct {
		quality   string
		size      string
		wantPrice float64
	}{
		{quality: "low", size: "1024x1024", wantPrice: 0.009},
		{quality: "low", size: "1024x1536", wantPrice: 0.013},
		{quality: "low", size: "1536x1024", wantPrice: 0.013},
		{quality: "medium", size: "1024x1024", wantPrice: 0.034},
		{quality: "medium", size: "1024x1536", wantPrice: 0.05},
		{quality: "medium", size: "1536x1024", wantPrice: 0.05},
		{quality: "high", size: "1024x1024", wantPrice: 0.133},
		{quality: "high", size: "1024x1536", wantPrice: 0.20},
		{quality: "high", size: "1536x1024", wantPrice: 0.20},
	}

	for _, test := range tests {
		t.Run(test.quality+"/"+test.size, func(t *testing.T) {
			variant := "gpt-image-1.5|" + test.quality + "|" + test.size + "|0"
			got := getDefaultExtraServicePrice(types.APIToolTypeImageGeneration, "gpt-5.6", variant)
			if math.Abs(got-test.wantPrice) > 1e-12 {
				t.Fatalf("expected price %.8f, got %.8f", test.wantPrice, got)
			}
		})
	}
}

func TestImageGenerationPricingDistinguishesCatalogModels(t *testing.T) {
	tests := []struct {
		name      string
		variant   string
		wantPrice float64
	}{
		{name: "GPT Image 1 mini snapshot", variant: "gpt-image-1-mini-2025-10-06|high|1024x1024|0", wantPrice: 0.036},
		{name: "GPT Image 1 regression", variant: "gpt-image-1|high|1024x1024|0", wantPrice: 0.167},
		{name: "GPT Image 1 snapshot", variant: "gpt-image-1-2025-10-06|high|1024x1024|0", wantPrice: 0.167},
		{name: "non-snapshot suffix", variant: "gpt-image-1-miniature|high|1024x1024|0", wantPrice: 0.71157},
		{name: "latest suffix", variant: "gpt-image-1-mini-latest|high|1024x1024|0", wantPrice: 0.71157},
		{name: "invalid date suffix", variant: "gpt-image-1-mini-2025-02-30|high|1024x1024|0", wantPrice: 0.71157},
		{name: "snapshot preview suffix", variant: "gpt-image-1-mini-2025-10-06-preview|high|1024x1024|0", wantPrice: 0.71157},
		{name: "GPT Image 1.5", variant: "gpt-image-1.5|high|1024x1024|0", wantPrice: 0.133},
		{name: "GPT Image 1.5 snapshot", variant: "gpt-image-1.5-2025-12-16|high|1024x1024|0", wantPrice: 0.133},
		{name: "GPT Image 1.5 invalid suffix", variant: "gpt-image-1.5-latest|high|1024x1024|0", wantPrice: 0.71157},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := getDefaultExtraServicePrice(types.APIToolTypeImageGeneration, "gpt-5.6", test.variant)
			if math.Abs(got-test.wantPrice) > 1e-12 {
				t.Fatalf("expected price %.8f, got %.8f", test.wantPrice, got)
			}
		})
	}
}

func TestImageGenerationPricingUsesConservativeCatalogFallback(t *testing.T) {
	if got := getDefaultExtraServicePrice(types.APIToolTypeImageGeneration, "gpt-5.6", "|low|1024x1024|0"); got != 0.011 {
		t.Fatalf("expected unspecified model to use the highest compatible catalog price, got %v", got)
	}
	if got := getDefaultExtraServicePrice(types.APIToolTypeImageGeneration, "gpt-5.6", "future-image|low|1024x1024|0"); got != 0.71157 {
		t.Fatalf("expected unknown model to use the catalog maximum, got %v", got)
	}
}

func TestImageGenerationPricingReportsConservativeFallback(t *testing.T) {
	tests := []struct {
		name       string
		variant    string
		diagnostic string
	}{
		{name: "exact GPT Image 2 evidence", variant: "gpt-image-2|high|2048x2048|0"},
		{name: "auto evidence", variant: "auto|auto|auto|0", diagnostic: imageGenerationConservativePriceDiagnostic},
		{name: "unknown model", variant: "future-image|high|1024x1024|0", diagnostic: imageGenerationConservativePriceDiagnostic},
		{name: "invalid size", variant: "gpt-image-2|high|future-size|0", diagnostic: imageGenerationConservativePriceDiagnostic},
		{name: "invalid partial count", variant: "gpt-image-2|high|1024x1024|unknown", diagnostic: imageGenerationConservativePriceDiagnostic},
		{name: "clamped partial count", variant: "gpt-image-2|high|1024x1024|4", diagnostic: imageGenerationConservativePriceDiagnostic},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			price, diagnostic := getDefaultExtraServicePriceDecision(types.APIToolTypeImageGeneration, "future-model", test.variant)
			if price <= 0 || diagnostic != test.diagnostic {
				t.Fatalf("price=%v diagnostic=%q, want nonzero/%q", price, diagnostic, test.diagnostic)
			}
		})
	}
}
