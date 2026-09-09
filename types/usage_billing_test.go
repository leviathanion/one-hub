package types

import (
	"encoding/json"
	"math"
	"testing"
)

func TestProviderUsageEvidenceIsExplicitAndValidated(t *testing.T) {
	local := &Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}
	if local.HasProviderUsage() {
		t.Fatal("in-memory token estimate became provider usage")
	}

	var provider Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}`), &provider); err != nil {
		t.Fatal(err)
	}
	if provider.HasProviderUsage() {
		t.Fatal("JSON decoding alone authorized provider billing")
	}
	provider.MarkProviderReported()
	if !provider.HasProviderUsage() {
		t.Fatal("explicit provider extractor evidence was not recognized")
	}

	provider.ExtraUsageUnits = map[string]float64{"duration": math.NaN()}
	if provider.HasProviderUsage() {
		t.Fatal("invalid provider usage was accepted")
	}
}

func TestEmptyOrTotalOnlyProviderUsageCannotBeAuthorized(t *testing.T) {
	for _, raw := range []string{`{}`, `{"total_tokens":0}`, `{"prompt_tokens":null,"completion_tokens":0}`, `{"prompt_tokens":0,"completion_tokens":null}`} {
		var usage Usage
		if err := json.Unmarshal([]byte(raw), &usage); err != nil {
			t.Fatalf("decode usage %s: %v", raw, err)
		}
		usage.MarkProviderReported()
		if usage.HasProviderUsage() {
			t.Fatalf("partial wire usage became provider evidence: raw=%s usage=%+v", raw, usage)
		}
	}

	for _, raw := range []string{`{}`, `{"total_tokens":0}`, `{"input_tokens":null,"output_tokens":0}`, `{"input_tokens":0,"output_tokens":null}`} {
		var usage ResponsesUsage
		if err := json.Unmarshal([]byte(raw), &usage); err != nil {
			t.Fatalf("decode Responses usage %s: %v", raw, err)
		}
		usage.MarkProviderReported()
		if converted := usage.ToOpenAIUsage(); converted.HasProviderUsage() {
			t.Fatalf("partial Responses wire usage became provider evidence: raw=%s usage=%+v", raw, converted)
		}
	}
}
