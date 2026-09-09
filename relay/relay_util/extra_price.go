package relay_util

import (
	"math"
	"strconv"
	"strings"
	"time"

	"one-api/types"
)

type ExtraServicePriceConfig struct {
	WebSearch   map[string]float64 `json:"web_search"`
	WebSearchGA float64            `json:"web_search_ga"`
}

var defaultExtraServicePrices = ExtraServicePriceConfig{
	WebSearch: map[string]float64{
		"high_tier": 0.025,
		"standard":  0.01,
	},
	WebSearchGA: 0.01,
}

var legacyGPTImage1Prices = map[string]map[string]float64{
	"low": {
		"1024x1024": 0.011,
		"1024x1536": 0.016,
		"1536x1024": 0.016,
	},
	"medium": {
		"1024x1024": 0.042,
		"1024x1536": 0.063,
		"1536x1024": 0.063,
	},
	"high": {
		"1024x1024": 0.167,
		"1024x1536": 0.25,
		"1536x1024": 0.25,
	},
}

var gptImage15Prices = map[string]map[string]float64{
	"low": {
		"1024x1024": 0.009,
		"1024x1536": 0.013,
		"1536x1024": 0.013,
	},
	"medium": {
		"1024x1024": 0.034,
		"1024x1536": 0.05,
		"1536x1024": 0.05,
	},
	"high": {
		"1024x1024": 0.133,
		"1024x1536": 0.20,
		"1536x1024": 0.20,
	},
}

var gptImage1MiniPrices = map[string]map[string]float64{
	"low": {
		"1024x1024": 0.005,
		"1024x1536": 0.006,
		"1536x1024": 0.006,
	},
	"medium": {
		"1024x1024": 0.011,
		"1024x1536": 0.015,
		"1536x1024": 0.015,
	},
	"high": {
		"1024x1024": 0.036,
		"1024x1536": 0.052,
		"1536x1024": 0.052,
	},
}

const (
	gptImage2OutputPricePerMillion    = 30.0
	gptImage15OutputPricePerMillion   = 32.0
	gptImage1OutputPricePerMillion    = 40.0
	gptImageMiniOutputPricePerMillion = 8.0
	gptImage2MaxPartialImages         = 3
	// GPT Image 2 flexible-size constraints:
	// https://developers.openai.com/api/docs/guides/image-generation#size-and-quality-options
	gptImage2MinPixels        = int64(655_360)
	gptImage2MaxPixels        = int64(8_294_400)
	gptImage2MaxEdge          = int64(3_840)
	gptImage2ConservativeEdge = int64(2_880)
)

func getModelTier(modelName string) string {
	if strings.HasPrefix(modelName, "gpt-4.1") || strings.HasPrefix(modelName, "gpt-4o") {
		return "high_tier"
	}
	return "standard"
}

func getDefaultExtraServicePrice(serviceType, modelName, extraType string) float64 {
	price, _ := getDefaultExtraServicePriceDecision(serviceType, modelName, extraType)
	return price
}

const imageGenerationConservativePriceDiagnostic = "image_generation_price_conservative_fallback"

func getDefaultExtraServicePriceDecision(serviceType, modelName, extraType string) (float64, string) {
	switch serviceType {
	case types.APIToolTypeWebSearch:
		return defaultExtraServicePrices.WebSearchGA, ""
	case types.APIToolTypeWebSearchPreview:
		return defaultExtraServicePrices.WebSearch[getModelTier(modelName)], ""
	case types.APIToolTypeImageGeneration:
		price, conservative := imageGenerationPriceDecision(extraType)
		if conservative {
			return price, imageGenerationConservativePriceDiagnostic
		}
		return price, ""
	default:
		return 0, ""
	}
}

type imageGenerationBillingEvidence struct {
	model         string
	quality       string
	size          string
	partialImages int64
	partialExact  bool
}

func imageGenerationPrice(extraType string) float64 {
	price, _ := imageGenerationPriceDecision(extraType)
	return price
}

func imageGenerationPriceDecision(extraType string) (float64, bool) {
	evidence := parseImageGenerationBillingEvidence(extraType)

	switch {
	case isGPTImage2Model(evidence.model):
		_, _, validSize := parseGPTImage2Size(evidence.size)
		return gptImage2BasePrice(evidence.quality, evidence.size) + imagePartialPrice(evidence.partialImages, gptImage2OutputPricePerMillion), !isGPTImageQuality(evidence.quality) || !validSize || !evidence.partialExact
	case isGPTImage15Model(evidence.model):
		return catalogImageBasePrice(gptImage15Prices, evidence.quality, evidence.size) + imagePartialPrice(evidence.partialImages, gptImage15OutputPricePerMillion), !catalogImageEvidenceExact(gptImage15Prices, evidence.quality, evidence.size) || !evidence.partialExact
	case isGPTImage1MiniModel(evidence.model):
		return catalogImageBasePrice(gptImage1MiniPrices, evidence.quality, evidence.size) + imagePartialPrice(evidence.partialImages, gptImageMiniOutputPricePerMillion), !catalogImageEvidenceExact(gptImage1MiniPrices, evidence.quality, evidence.size) || !evidence.partialExact
	case isGPTImage1Model(evidence.model):
		return catalogImageBasePrice(legacyGPTImage1Prices, evidence.quality, evidence.size) + imagePartialPrice(evidence.partialImages, gptImage1OutputPricePerMillion), !catalogImageEvidenceExact(legacyGPTImage1Prices, evidence.quality, evidence.size) || !evidence.partialExact
	case evidence.model == "", evidence.model == "auto":
		return max(
			gptImage2BasePrice(evidence.quality, evidence.size)+imagePartialPrice(evidence.partialImages, gptImage2OutputPricePerMillion),
			catalogImageBasePrice(gptImage15Prices, evidence.quality, evidence.size)+imagePartialPrice(evidence.partialImages, gptImage15OutputPricePerMillion),
			catalogImageBasePrice(legacyGPTImage1Prices, evidence.quality, evidence.size)+imagePartialPrice(evidence.partialImages, gptImage1OutputPricePerMillion),
			catalogImageBasePrice(gptImage1MiniPrices, evidence.quality, evidence.size)+imagePartialPrice(evidence.partialImages, gptImageMiniOutputPricePerMillion),
		), true
	default:
		// Unknown tool models are outside the supported catalogue. Keep the
		// accounting fail-safe if such evidence reaches settlement.
		return max(
			gptImage2BasePrice("high", "")+imagePartialPrice(evidence.partialImages, gptImage2OutputPricePerMillion),
			catalogImageBasePrice(gptImage15Prices, "high", "")+imagePartialPrice(evidence.partialImages, gptImage15OutputPricePerMillion),
			catalogImageBasePrice(legacyGPTImage1Prices, "high", "")+imagePartialPrice(evidence.partialImages, gptImage1OutputPricePerMillion),
			catalogImageBasePrice(gptImage1MiniPrices, "high", "")+imagePartialPrice(evidence.partialImages, gptImageMiniOutputPricePerMillion),
		), true
	}
}

func isGPTImageQuality(quality string) bool {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "low", "medium", "high":
		return true
	default:
		return false
	}
}

func catalogImageEvidenceExact(pricesByQuality map[string]map[string]float64, quality, size string) bool {
	prices, ok := pricesByQuality[strings.ToLower(strings.TrimSpace(quality))]
	if !ok {
		return false
	}
	_, ok = prices[strings.ToLower(strings.TrimSpace(size))]
	return ok
}

func imagePartialPrice(count int64, outputPricePerMillion float64) float64 {
	return float64(count*100) * outputPricePerMillion / 1_000_000
}

func parseImageGenerationBillingEvidence(extraType string) imageGenerationBillingEvidence {
	value := strings.ToLower(strings.TrimSpace(extraType))
	if parts := strings.Split(value, "|"); len(parts) == 4 {
		partialImages, partialExact := parsePartialImages(parts[3])
		return imageGenerationBillingEvidence{
			model:         strings.TrimSpace(parts[0]),
			quality:       strings.TrimSpace(parts[1]),
			size:          strings.TrimSpace(parts[2]),
			partialImages: partialImages,
			partialExact:  partialExact,
		}
	}

	// Read the previous in-process key format so mixed-version workers settle
	// already-started requests conservatively during a rolling deployment.
	parts := strings.SplitN(value, "-", 2)
	if len(parts) == 2 {
		return imageGenerationBillingEvidence{quality: parts[0], size: parts[1]}
	}
	return imageGenerationBillingEvidence{}
}

func parsePartialImages(value string) (int64, bool) {
	count, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || count < 0 {
		return 0, false
	}
	if count > gptImage2MaxPartialImages {
		return gptImage2MaxPartialImages, false
	}
	return count, true
}

func isGPTImage2Model(model string) bool {
	return isModelAliasOrSnapshot(model, "gpt-image-2")
}

func isGPTImage15Model(model string) bool {
	return isModelAliasOrSnapshot(model, "gpt-image-1.5")
}

func isGPTImage1MiniModel(model string) bool {
	return isModelAliasOrSnapshot(model, "gpt-image-1-mini")
}

func isGPTImage1Model(model string) bool {
	return isModelAliasOrSnapshot(model, "gpt-image-1")
}

func isModelAliasOrSnapshot(model, alias string) bool {
	if model == alias {
		return true
	}
	snapshot := strings.TrimPrefix(model, alias+"-")
	if snapshot == model || len(snapshot) != len("2006-01-02") {
		return false
	}
	_, err := time.Parse("2006-01-02", snapshot)
	return err == nil
}

func gptImage2BasePrice(quality, size string) float64 {
	qualityEdge, ok := map[string]int64{
		"low":    16,
		"medium": 48,
		"high":   96,
	}[strings.ToLower(strings.TrimSpace(quality))]
	if !ok {
		qualityEdge = 96
	}

	width, height, ok := parseGPTImage2Size(size)
	if !ok {
		width, height = gptImage2ConservativeEdge, gptImage2ConservativeEdge
	}

	longEdge, shortEdge := width, height
	if shortEdge > longEdge {
		longEdge, shortEdge = shortEdge, longEdge
	}
	scaledEdge := int64(math.Round(float64(qualityEdge*shortEdge) / float64(longEdge)))
	gridWidth, gridHeight := qualityEdge, scaledEdge
	if height > width {
		gridWidth, gridHeight = scaledEdge, qualityEdge
	}

	numerator := gridWidth * gridHeight * (2_000_000 + width*height)
	outputTokens := (numerator + 4_000_000 - 1) / 4_000_000
	return float64(outputTokens) * gptImage2OutputPricePerMillion / 1_000_000
}

func parseGPTImage2Size(size string) (int64, int64, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(size)), "x")
	if len(parts) != 2 {
		return 0, 0, false
	}
	width, widthErr := strconv.ParseInt(parts[0], 10, 64)
	height, heightErr := strconv.ParseInt(parts[1], 10, 64)
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return 0, 0, false
	}
	if width > gptImage2MaxEdge || height > gptImage2MaxEdge {
		return 0, 0, false
	}
	pixels := width * height
	longEdge, shortEdge := width, height
	if shortEdge > longEdge {
		longEdge, shortEdge = shortEdge, longEdge
	}
	if width%16 != 0 || height%16 != 0 || pixels < gptImage2MinPixels || pixels > gptImage2MaxPixels || longEdge > gptImage2MaxEdge || longEdge > 3*shortEdge {
		return 0, 0, false
	}
	return width, height, true
}

func catalogImageBasePrice(pricesByQuality map[string]map[string]float64, quality, size string) float64 {
	quality = strings.ToLower(strings.TrimSpace(quality))
	size = strings.ToLower(strings.TrimSpace(size))
	maxPrice := 0.0
	for configuredQuality, prices := range pricesByQuality {
		if quality != "" && quality != "auto" && quality != configuredQuality {
			continue
		}
		for configuredSize, price := range prices {
			if size != "" && size != "auto" && size != configuredSize {
				continue
			}
			if price > maxPrice {
				maxPrice = price
			}
		}
	}
	if maxPrice == 0 && quality != "" && quality != "auto" {
		for _, price := range pricesByQuality[quality] {
			if price > maxPrice {
				maxPrice = price
			}
		}
	}
	if maxPrice == 0 {
		for _, prices := range pricesByQuality {
			for _, price := range prices {
				if price > maxPrice {
					maxPrice = price
				}
			}
		}
	}
	return maxPrice
}
