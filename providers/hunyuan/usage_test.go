package hunyuan

import (
	"encoding/json"
	"testing"
)

func TestHunyuanUsageRequiresCompleteTerminalSnapshot(t *testing.T) {
	var complete HunyuanUsage
	if err := json.Unmarshal([]byte(`{"PromptTokens":5,"CompletionTokens":0,"TotalTokens":5,"PromptTokensDetails":{"CachedTokens":2}}`), &complete); err != nil {
		t.Fatal(err)
	}
	usage := hunyuanUsageToOpenAI(&complete)
	if usage == nil || !usage.HasProviderUsage() || usage.CompletionTokens != 0 {
		t.Fatalf("complete zero-output Hunyuan usage was not authorized: %+v", usage)
	}

	var partial HunyuanUsage
	if err := json.Unmarshal([]byte(`{"PromptTokens":5,"TotalTokens":5}`), &partial); err != nil {
		t.Fatal(err)
	}
	if usage := hunyuanUsageToOpenAI(&partial); usage == nil || usage.HasProviderUsage() {
		t.Fatalf("partial Hunyuan snapshot became provider evidence: %+v", usage)
	}

	var conflicting HunyuanUsage
	if err := json.Unmarshal([]byte(`{"PromptTokens":5,"CompletionTokens":1,"TotalTokens":5}`), &conflicting); err != nil {
		t.Fatal(err)
	}
	if usage := hunyuanUsageToOpenAI(&conflicting); usage == nil || usage.HasProviderUsage() || !usage.ProviderTokenConflict {
		t.Fatalf("conflicting Hunyuan totals became provider evidence: %+v", usage)
	}
}
