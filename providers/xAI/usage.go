package xAI

import (
	"math"
	"one-api/common/config"
	"one-api/types"
)

// xAI 的 completion_tokens 只含可见输出，total_tokens 还包含 reasoning。
// 分区缺失交由共享定价层处理，不要求上游提供冗余字段才能计费。
func usageHandler(usage *types.Usage) bool {
	if usage == nil || !usage.HasProviderBaseUsage() {
		return false
	}
	fields := usage.ProviderTokenFields
	visible := usage.CompletionTokens
	reasoning := usage.CompletionTokensDetails.ReasoningTokens
	output := 0
	conflict := false
	switch {
	case fields[config.UsageExtraReasoning]:
		conflict = visible > math.MaxInt-reasoning
		if !conflict {
			output = visible + reasoning
		}
	case fields["total_tokens"]:
		conflict = usage.TotalTokens < usage.PromptTokens
		if !conflict {
			output = usage.TotalTokens - usage.PromptTokens
			conflict = visible > output
		}
	default:
		// 原始计数只有可见输出，不能授权归一后的完整输出基数。
		delete(fields, "completion_tokens")
		return false
	}
	if fields[config.UsageExtraOutputTextTokens] && usage.CompletionTokensDetails.TextTokens != visible {
		// 两个显式文本计数不能被别名覆盖，也不能用来推导不同的推理量。
		conflict = true
	}
	if fields["total_tokens"] && !conflict {
		// 用减法核对冗余总量，避免把多个不可信计数相加后溢出。
		conflict = usage.TotalTokens < usage.PromptTokens || usage.TotalTokens-usage.PromptTokens != output
	}
	for _, value := range []int{
		usage.PromptTokensDetails.CachedTokens,
		usage.PromptTokensDetails.TextTokens,
		usage.PromptTokensDetails.ImageTokens,
		usage.PromptTokensDetails.AudioTokens,
	} {
		conflict = conflict || value > usage.PromptTokens
	}
	for _, value := range []int{
		usage.CompletionTokensDetails.TextTokens,
		usage.CompletionTokensDetails.ImageTokens,
		usage.CompletionTokensDetails.AudioTokens,
	} {
		conflict = conflict || value > output
	}
	if conflict {
		usage.ProviderTokenConflict = true
		usage.AddBillingDiagnostic("xai_token_usage_conflict")
		return false
	}
	usage.CompletionTokens = output
	// completion_tokens 是上游直接报告的可见文本量；别名只用于计费，
	// 不向公开响应补 text_tokens，也不把派生的 reasoning 标为已观察。
	// 已观察的标准细分（包括显式零值）不由别名覆盖。
	if !fields[config.UsageExtraOutputTextTokens] {
		usage.SetExtraTokens(config.UsageExtraOutputTextTokens, visible)
		usage.MarkProviderTokenField(config.UsageExtraOutputTextTokens)
	}
	usage.RequireTokenExtraEvidence(config.UsageExtraOutputTextTokens, config.UsageExtraReasoning)
	usage.SetTokenExtraEvidenceGroups([]string{config.UsageExtraOutputTextTokens, config.UsageExtraReasoning})
	return visible != output
}
