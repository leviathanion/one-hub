package claude

import (
	"encoding/json"
	"testing"

	"one-api/common/config"
	"one-api/types"
)

func decodeClaudeUsage(t *testing.T, raw string) *Usage {
	t.Helper()
	var usage Usage
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatal(err)
	}
	return &usage
}

func TestClaudeUsageMergeCumulativeFields(t *testing.T) {
	usage := decodeClaudeUsage(t, `{"input_tokens":2679,"output_tokens":3}`)
	ClaudeUsageMerge(usage, decodeClaudeUsage(t, `{"output_tokens":20}`))
	if usage.InputTokens != 2679 || usage.OutputTokens != 20 {
		t.Fatalf("省略 input 必须保留，累计 output 必须覆盖：%+v", usage)
	}
	ClaudeUsageMerge(usage, decodeClaudeUsage(t, `{"input_tokens":10682,"output_tokens":510}`))
	if usage.InputTokens != 10682 || usage.OutputTokens != 510 {
		t.Fatalf("累计 usage 被重复相加：%+v", usage)
	}
	ClaudeUsageMerge(usage, decodeClaudeUsage(t, `{"input_tokens":0,"output_tokens":0}`))
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Fatalf("明确 0 没有覆盖旧值：%+v", usage)
	}
}

func TestClaudeUsageMergeCachePartitionAndToolSnapshots(t *testing.T) {
	usage := decodeClaudeUsage(t, `{"input_tokens":10,"output_tokens":3,"cache_creation_input_tokens":8,"cache_read_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":3},"server_tool_use":{"web_search_requests":1}}`)
	converted := &types.Usage{}
	for i := 0; i < 2; i++ {
		ClaudeUsageMerge(usage, decodeClaudeUsage(t, `{"cache_creation_input_tokens":8,"cache_read_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":3},"server_tool_use":{"web_search_requests":2}}`))
		ClaudeUsageToOpenaiUsage(usage, converted)
	}
	if !converted.HasProviderUsage() || converted.PromptTokens != 22 || converted.ExtraBilling[types.APIToolTypeWebSearch].CallCount != 2 {
		t.Fatalf("缓存或搜索累计值被相加：%+v", converted)
	}
	ClaudeUsageMerge(usage, decodeClaudeUsage(t, `{"cache_creation_input_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":0},"server_tool_use":{"web_search_requests":0}}`))
	ClaudeUsageToOpenaiUsage(usage, converted)
	if !converted.HasProviderUsage() || converted.PromptTokens != 17 || converted.GetExtraTokens()[config.UsageExtraClaudeCacheWrite5m] != 0 || converted.GetExtraTokens()[config.UsageExtraClaudeCacheWrite1h] != 3 || converted.ExtraBilling[types.APIToolTypeWebSearch].CallCount != 0 {
		t.Fatalf("嵌套字段省略或明确 0 处理错误：%+v", converted)
	}
	ClaudeUsageMerge(usage, decodeClaudeUsage(t, `{"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_1h_input_tokens":0}}`))
	ClaudeUsageToOpenaiUsage(usage, converted)
	if !converted.HasProviderUsage() || converted.PromptTokens != 10 || len(converted.ExtraTokens) != 0 || len(converted.RequiredTokenExtraKeys) != 0 {
		t.Fatalf("旧缓存计量残留：%+v", converted)
	}
}

func TestClaudeUsageCacheEvidenceCanCompleteInLaterSnapshot(t *testing.T) {
	usage := decodeClaudeUsage(t, `{"input_tokens":10,"output_tokens":3,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":5}}`)
	converted := &types.Usage{}
	ClaudeUsageToOpenaiUsage(usage, converted)
	if converted.HasProviderUsage() {
		t.Fatal("缺少 1h presence 的分区不应可计费")
	}
	ClaudeUsageMerge(usage, decodeClaudeUsage(t, `{"cache_creation":{"ephemeral_1h_input_tokens":0}}`))
	ClaudeUsageToOpenaiUsage(usage, converted)
	if !converted.HasProviderUsage() {
		t.Fatalf("完整快照仍残留旧缺证据标记：%+v", converted)
	}
}

func TestClaudeUsageNullDoesNotCreateZeroEvidence(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"input_tokens":null,"output_tokens":0}`, `{"input_tokens":0,"output_tokens":null}`} {
		usage := decodeClaudeUsage(t, raw)
		converted := &types.Usage{}
		if ClaudeUsageToOpenaiUsage(usage, converted) || converted.HasProviderUsage() {
			t.Fatalf("无计量被伪装为明确 0：%s %+v", raw, converted)
		}
	}
	usage := decodeClaudeUsage(t, `{"input_tokens":10,"output_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":0},"server_tool_use":{"web_search_requests":2}}`)
	ClaudeUsageMerge(usage, decodeClaudeUsage(t, `{"input_tokens":null,"output_tokens":null,"cache_creation_input_tokens":null,"cache_read_input_tokens":null,"cache_creation":{"ephemeral_5m_input_tokens":null},"server_tool_use":{"web_search_requests":null}}`))
	if usage.InputTokens != 10 || usage.OutputTokens != 3 || usage.CacheCreationInputTokens != 5 || usage.CacheReadInputTokens != 4 || usage.CacheCreation.Ephemeral5mInputTokens != 5 || usage.ServerToolUse.WebSearchRequests != 2 {
		t.Fatalf("null 覆盖了既有累计证据：%+v", usage)
	}
	for _, raw := range []string{`{"input_tokens":"bad"}`, `{"output_tokens":1.5}`, `{"cache_creation":{"ephemeral_5m_input_tokens":false}}`} {
		var usage Usage
		if json.Unmarshal([]byte(raw), &usage) == nil {
			t.Fatalf("无效计量被接受：%s", raw)
		}
	}
}

func TestClaudeUsageKeepsIndependentSearchWithoutTokenEvidence(t *testing.T) {
	converted := &types.Usage{}
	usage := decodeClaudeUsage(t, `{"server_tool_use":{"web_search_requests":2}}`)
	if ClaudeUsageToOpenaiUsage(usage, converted) || converted.HasProviderUsage() {
		t.Fatal("搜索证据不应补齐 token 证据")
	}
	if !converted.HasProviderExtraBilling(types.APIToolTypeWebSearch) || converted.ExtraBilling[types.APIToolTypeWebSearch].CallCount != 2 {
		t.Fatalf("完整独立搜索证据丢失：%+v", converted)
	}
}

func TestClaudeUsageZeroCacheTotalCannotHideConflictingPartition(t *testing.T) {
	usage := decodeClaudeUsage(t, `{"input_tokens":10,"output_tokens":3,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":0}}`)
	ClaudeUsageMerge(usage, decodeClaudeUsage(t, `{"cache_creation_input_tokens":0}`))
	converted := &types.Usage{}
	ClaudeUsageToOpenaiUsage(usage, converted)
	if converted.HasProviderUsage() {
		t.Fatal("cache total=0 与保留的非零分区冲突，不应被计费")
	}
}

func TestClaudeLegacyRelayStreamUsesCumulativeSnapshots(t *testing.T) {
	handler := &ClaudeRelayStreamHandler{Usage: &types.Usage{}, Prefix: "data: "}
	data := make(chan string, 5)
	errors := make(chan error, 5)
	for _, event := range []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":2679,"output_tokens":3}}}`,
		`data: {"type":"message_delta","usage":{"input_tokens":10682,"output_tokens":20}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":510}}`,
	} {
		line := []byte(event)
		handler.HandlerStream(&line, data, errors)
	}
	if len(errors) > 0 || handler.Usage.PromptTokens != 10682 || handler.Usage.CompletionTokens != 510 {
		t.Fatalf("云厂商复用的 Messages stream 合并错误：%+v errors=%d", handler.Usage, len(errors))
	}
}
