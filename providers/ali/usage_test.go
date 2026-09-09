package ali

import (
	"encoding/json"
	"testing"

	"one-api/common/config"
)

func TestAliUsagePreservesZeroOutputAndProviderDimensions(t *testing.T) {
	var providerUsage AliUsage
	if err := json.Unmarshal([]byte(`{
		"input_tokens":12,"output_tokens":0,"total_tokens":12,
		"input_tokens_details":{"cached_tokens":4,"image_tokens":3,"audio_tokens":0,"video_tokens":2},
		"output_tokens_details":{"reasoning_tokens":0}
	}`), &providerUsage); err != nil {
		t.Fatalf("decode Ali usage: %v", err)
	}
	usage := aliUsageToOpenAI(&providerUsage)
	if usage == nil || !usage.HasProviderUsage() || usage.CompletionTokens != 0 {
		t.Fatalf("Ali zero-output usage was lost: %+v", usage)
	}
	extra := usage.GetExtraTokens()
	if extra[config.UsageExtraCache] != 4 || extra[config.UsageExtraInputImageTokens] != 3 || extra[config.UsageExtraInputVideoTokens] != 2 {
		t.Fatalf("Ali provider dimensions were lost: %+v", extra)
	}
}

func TestAliUsageDoesNotAuthorizePartialEnvelope(t *testing.T) {
	var providerUsage AliUsage
	if err := json.Unmarshal([]byte(`{"total_tokens":12}`), &providerUsage); err != nil {
		t.Fatalf("decode Ali usage: %v", err)
	}
	if usage := aliUsageToOpenAI(&providerUsage); usage == nil || usage.HasProviderUsage() {
		t.Fatalf("partial Ali usage became priceable: %+v", usage)
	}
}
