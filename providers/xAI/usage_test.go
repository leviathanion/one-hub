package xAI

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/types"
)

func TestUsageNormalizesReasoningWithoutLosingDetails(t *testing.T) {
	var usage types.Usage
	raw := `{"prompt_tokens":1245,"completion_tokens":1,"total_tokens":1282,"prompt_tokens_details":{"text_tokens":1245,"image_tokens":0,"cached_tokens":1152},"completion_tokens_details":{"reasoning_tokens":36}}`
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatal(err)
	}
	usage.MarkProviderReported()
	if !usageHandler(&usage) || !usage.HasProviderUsage() || usage.CompletionTokens != 37 || usage.PromptTokensDetails.CachedTokens != 1152 || usage.CompletionTokensDetails.ReasoningTokens != 36 {
		t.Fatalf("推理与缓存用量归一失败：%+v", usage)
	}
	if !usage.ProviderTokenFields[config.UsageExtraInputImageTokens] || !usage.ProviderTokenFields[config.UsageExtraCache] {
		t.Fatal("显式图片零用量或缓存证据丢失")
	}
}

func TestUsageRejectsMissingOrConflictingEvidence(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		conflict bool
	}{
		{"missing_prompt", `{"completion_tokens":1,"total_tokens":2}`, false},
		{"missing_completion", `{"prompt_tokens":1,"total_tokens":2}`, false},
		{"missing_output_basis", `{"prompt_tokens":1,"completion_tokens":1}`, false},
		{"null_output_basis", `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":null}`, false},
		{"negative_completion", `{"prompt_tokens":3,"completion_tokens":-1,"total_tokens":8}`, false},
		{"negative_prompt", `{"prompt_tokens":-3,"completion_tokens":1,"total_tokens":8}`, false},
		{"negative_total", `{"prompt_tokens":3,"completion_tokens":1,"total_tokens":-8}`, false},
		{"negative_reasoning", `{"prompt_tokens":3,"completion_tokens":1,"total_tokens":8,"completion_tokens_details":{"reasoning_tokens":-1}}`, false},
		{"inconsistent_total", `{"prompt_tokens":3,"completion_tokens":1,"total_tokens":8,"completion_tokens_details":{"reasoning_tokens":2}}`, true},
		{"cache_exceeds_prompt", `{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4,"prompt_tokens_details":{"cached_tokens":4}}`, true},
		{"image_exceeds_prompt", `{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4,"prompt_tokens_details":{"image_tokens":4}}`, true},
		{"negative_cache", `{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4,"prompt_tokens_details":{"cached_tokens":-1}}`, false},
		{"reasoning_sum_overflow", fmt.Sprintf(`{"prompt_tokens":0,"completion_tokens":%d,"completion_tokens_details":{"reasoning_tokens":1}}`, math.MaxInt), true},
		{"overflow_sum", fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}`, math.MaxInt, math.MaxInt, math.MaxInt-1), true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var usage types.Usage
			if err := json.Unmarshal([]byte(test.raw), &usage); err != nil {
				t.Fatal(err)
			}
			usage.MarkProviderReported()
			usageHandler(&usage)
			if usage.HasProviderUsage() || usage.ProviderTokenConflict != test.conflict {
				t.Fatalf("不完整或冲突用量被接受：%+v", usage)
			}
		})
	}
}

func TestUsageAcceptsLegacyNonReasoningAndZeroSnapshots(t *testing.T) {
	for _, raw := range []string{
		`{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}`,
		`{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`,
		fmt.Sprintf(`{"prompt_tokens":0,"completion_tokens":%d,"total_tokens":%d}`, math.MaxInt, math.MaxInt),
	} {
		var usage types.Usage
		if err := json.Unmarshal([]byte(raw), &usage); err != nil {
			t.Fatal(err)
		}
		usage.MarkProviderReported()
		usageHandler(&usage)
		if !usage.HasProviderBaseUsage() || usage.ProviderTokenFields[config.UsageExtraReasoning] || usage.HasProviderUsage() {
			t.Fatalf("合法基数被拒绝或派生推理量被标为观察证据：%+v", usage)
		}
	}
}

func TestUsageRejectsNonIntegerWireCounters(t *testing.T) {
	for _, raw := range []string{
		`{"prompt_tokens":3,"completion_tokens":1.5,"total_tokens":5}`,
		`{"prompt_tokens":3,"completion_tokens":"1","total_tokens":4}`,
		`{"prompt_tokens":3,"completion_tokens":18446744073709551616,"total_tokens":4}`,
	} {
		var usage types.Usage
		if json.Unmarshal([]byte(raw), &usage) == nil {
			t.Fatalf("非法计数被接受：%s", raw)
		}
	}
}

func TestUsageAcceptsCompleteOutputWithoutRedundantFields(t *testing.T) {
	for _, test := range []struct {
		name, raw                 string
		strict, reasoningObserved bool
	}{
		{"explicit reasoning without total", `{"prompt_tokens":3,"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":2}}`, true, true},
		{"explicit reasoning with null total", `{"prompt_tokens":3,"completion_tokens":5,"total_tokens":null,"completion_tokens_details":{"reasoning_tokens":2}}`, true, true},
		{"total without reasoning partition", `{"prompt_tokens":3,"completion_tokens":5,"total_tokens":10}`, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var usage types.Usage
			if err := json.Unmarshal([]byte(test.raw), &usage); err != nil {
				t.Fatal(err)
			}
			usage.MarkProviderReported()
			if !usageHandler(&usage) || !usage.HasProviderBaseUsage() || usage.CompletionTokens != 7 || usage.HasProviderUsage() != test.strict {
				t.Fatalf("可定价的基数因冗余字段缺失而丢失：%+v", usage)
			}
			if usage.GetExtraTokens()[config.UsageExtraOutputTextTokens] != 5 || !usage.ProviderTokenFields[config.UsageExtraOutputTextTokens] || usage.ProviderTokenFields[config.UsageExtraReasoning] != test.reasoningObserved {
				t.Fatalf("输出分区来源不正确：%+v", usage)
			}
			if len(usage.TokenExtraEvidenceGroups) != 1 || len(usage.TokenExtraEvidenceGroups[0]) != 2 {
				t.Fatalf("缺少共享计价的输出分区契约：%+v", usage.TokenExtraEvidenceGroups)
			}
			if usage.CompletionTokensDetails.TextTokens != 0 {
				t.Fatal("内部别名不应改写公开text_tokens字段")
			}
			encoded, err := json.Marshal(&usage)
			if err != nil || strings.Contains(string(encoded), `"text_tokens"`) {
				t.Fatalf("公开usage被补入未观察字段：%s", encoded)
			}
		})
	}
}

func TestUsageKeepsExplicitTextPartitionInsteadOfVisibleAlias(t *testing.T) {
	for _, reasoning := range []string{`"reasoning_tokens":2,`, ""} {
		for _, text := range []int{5, 4, 0} {
			t.Run(fmt.Sprintf("reasoning %s text %d", reasoning, text), func(t *testing.T) {
				raw := fmt.Sprintf(`{"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{%s"text_tokens":%d}}`, reasoning, text)
				var usage types.Usage
				if err := json.Unmarshal([]byte(raw), &usage); err != nil {
					t.Fatal(err)
				}
				usage.MarkProviderReported()
				formatted := usageHandler(&usage)
				if usage.CompletionTokensDetails.TextTokens != text || usage.GetExtraTokens()[config.UsageExtraOutputTextTokens] != text || !usage.ProviderTokenFields[config.UsageExtraOutputTextTokens] {
					t.Fatalf("已观察文本细分被别名覆盖：%+v", usage)
				}
				if text != 5 {
					if formatted || !usage.ProviderTokenConflict || usage.HasProviderBaseUsage() || usage.CompletionTokens != 5 {
						t.Fatalf("矛盾文本分区被归一成可计费证据：%+v", usage)
					}
				} else if !formatted || !usage.HasProviderBaseUsage() || usage.CompletionTokens != 7 {
					t.Fatalf("匹配文本分区规范化失败：%+v", usage)
				}
			})
		}
	}
}

func TestVisibleOutputAloneDoesNotAuthorizeCompleteOutputSnapshot(t *testing.T) {
	var usage types.Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":3,"completion_tokens":5}`), &usage); err != nil {
		t.Fatal(err)
	}
	usage.MarkProviderReported()
	if usageHandler(&usage) || usage.HasProviderBaseUsage() || usage.ProviderTokenFields["completion_tokens"] || len(usage.RequiredTokenExtraKeys) != 0 {
		t.Fatalf("未确定的完整输出被授权为基础快照：%+v", usage)
	}
	if usage.CompletionTokens != 5 {
		t.Fatalf("原始可见输出被改写：%+v", usage)
	}
}
