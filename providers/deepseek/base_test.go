package deepseek

import (
	"encoding/json"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/types"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestDeepSeekUsageNormalizesCacheHit(t *testing.T) {
	for _, tc := range []struct {
		name, fields      string
		want              int
		present, mismatch bool
	}{
		{"相同别名", `"prompt_tokens_details":{"cached_tokens":7},"prompt_cache_hit_tokens":7,"prompt_cache_miss_tokens":3`, 7, true, false},
		{"标准优先", `"prompt_tokens_details":{"cached_tokens":7},"prompt_cache_hit_tokens":9,"prompt_cache_miss_tokens":1`, 7, true, true},
		{"标准零值优先", `"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":9`, 0, true, true},
		{"只有标准字段", `"prompt_tokens_details":{"cached_tokens":7}`, 7, true, false},
		{"原生字段补入", `"prompt_cache_hit_tokens":7,"prompt_cache_miss_tokens":3`, 7, true, false},
		{"无需未命中字段", `"prompt_cache_hit_tokens":7`, 7, true, false},
		{"标准null补入", `"prompt_tokens_details":{"cached_tokens":null},"prompt_cache_hit_tokens":7`, 7, true, false},
		{"明细null补入", `"prompt_tokens_details":null,"prompt_cache_hit_tokens":7`, 7, true, false},
		{"原生零值补入", `"prompt_cache_hit_tokens":0`, 0, true, false},
		{"不从miss构造命中", `"prompt_cache_miss_tokens":3`, 0, false, false},
		{"原生null保持缺失", `"prompt_cache_hit_tokens":null`, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.ErrorLevel)
			previous := logger.Logger
			logger.Logger = zap.New(core)
			t.Cleanup(func() { logger.Logger = previous })
			var usage types.Usage
			if err := json.Unmarshal([]byte(`{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,`+tc.fields+`}`), &usage); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				deepSeekUsageHandler(&usage)
				if usage.PromptTokensDetails.CachedTokens != tc.want || usage.ProviderTokenFields[config.UsageExtraCache] != tc.present {
					t.Fatalf("缓存归一化错误：%+v", usage)
				}
				if usage.HasProviderUsage() != tc.present || usage.ProviderTokenConflict {
					t.Fatalf("标准字段优先级或证据存在性错误：%+v", usage)
				}
				if usage.BillingDiagnostics["deepseek_cache_alias_mismatch"] != tc.mismatch {
					t.Fatalf("别名差异诊断错误：%v", usage.BillingDiagnostics)
				}
				for key := range usage.GetExtraTokens() {
					if key != config.UsageExtraCache {
						t.Fatalf("产生了标准缓存以外的计价维度：%q", key)
					}
				}
			}
			wantLogs := 0
			if tc.mismatch {
				wantLogs = 1
			}
			if logs.Len() != wantLogs {
				t.Fatalf("系统 error 数量=%d，期望 %d", logs.Len(), wantLogs)
			}
			if tc.mismatch && logs.FilterMessageSnippet("采用 cached_tokens").Len() != 1 {
				t.Fatal("系统日志未说明采用标准字段")
			}
		})
	}
}

func TestDeepSeekUsageRetainsInvalidCanonicalCacheEvidence(t *testing.T) {
	for _, fields := range []string{
		`"prompt_tokens_details":{"cached_tokens":-1},"prompt_cache_hit_tokens":7`,
		`"prompt_cache_hit_tokens":-1`,
	} {
		var usage types.Usage
		if err := json.Unmarshal([]byte(`{"prompt_tokens":10,"completion_tokens":2,`+fields+`}`), &usage); err != nil {
			t.Fatal(err)
		}
		deepSeekUsageHandler(&usage)
		if usage.PromptTokensDetails.CachedTokens != -1 || usage.HasProviderBaseUsage() {
			t.Fatalf("无效标准缓存被覆盖或接受：%+v", usage)
		}
	}
}
