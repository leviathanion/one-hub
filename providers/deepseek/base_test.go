package deepseek

import (
	"encoding/json"
	"testing"

	"one-api/common/config"
	"one-api/types"
)

func TestDeepSeekUsageRequiresHitAndMissPartitions(t *testing.T) {
	var usage types.Usage
	if err := json.Unmarshal([]byte(`{
		"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,
		"prompt_cache_hit_tokens":7,"prompt_cache_miss_tokens":3
	}`), &usage); err != nil {
		t.Fatalf("decode DeepSeek usage: %v", err)
	}
	usage.MarkProviderReported()
	deepSeekUsageHandler(&usage)
	if !usage.HasProviderUsage() || usage.GetExtraTokens()[config.UsageExtraDeepSeekCacheHit] != 7 || usage.GetExtraTokens()[config.UsageExtraDeepSeekCacheMiss] != 3 {
		t.Fatalf("DeepSeek cache partitions were lost: %+v", usage)
	}

	var partial types.Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_cache_hit_tokens":7}`), &partial); err != nil {
		t.Fatalf("decode partial DeepSeek usage: %v", err)
	}
	partial.MarkProviderReported()
	deepSeekUsageHandler(&partial)
	if partial.HasProviderUsage() {
		t.Fatalf("partial DeepSeek cache partition became priceable: %+v", partial)
	}
}
