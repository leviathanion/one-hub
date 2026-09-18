package types

import (
	"encoding/json"
	"testing"
)

func TestEffectiveReasoningPrefersStructuredConfigAndFallsBackToOfficialEffort(t *testing.T) {
	officialEffort := "high"
	structured := &ChatReasoning{MaxTokens: 512, Effort: "low"}

	request := &ChatCompletionRequest{Reasoning: structured, ReasoningEffort: &officialEffort}
	if got := request.EffectiveReasoning(); got != structured {
		t.Fatalf("structured reasoning must win, got %#v", got)
	}

	request.Reasoning = nil
	if got := request.EffectiveReasoning(); got == nil || got.Effort != officialEffort || got.MaxTokens != 0 {
		t.Fatalf("official reasoning_effort fallback = %#v", got)
	}

	request.ReasoningEffort = nil
	if got := request.EffectiveReasoning(); got != nil {
		t.Fatalf("empty reasoning request returned %#v", got)
	}
}

func TestChatReasoningPreservesExplicitZeroMaxTokens(t *testing.T) {
	var request ChatCompletionRequest
	if err := json.Unmarshal([]byte(`{"model":"gemini-2.5-flash","reasoning":{"max_tokens":0}}`), &request); err != nil {
		t.Fatalf("decode explicit zero reasoning budget: %v", err)
	}
	if request.Reasoning == nil || !request.Reasoning.HasMaxTokens() || request.Reasoning.MaxTokens != 0 {
		t.Fatalf("explicit zero reasoning budget lost: %#v", request.Reasoning)
	}

	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("encode explicit zero reasoning budget: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode encoded request: %v", err)
	}
	var reasoning map[string]json.RawMessage
	if err := json.Unmarshal(wire["reasoning"], &reasoning); err != nil {
		t.Fatalf("decode encoded reasoning: %v", err)
	}
	if got := string(reasoning["max_tokens"]); got != "0" {
		t.Fatalf("encoded max_tokens = %q, want explicit zero; request=%s", got, raw)
	}
}

func TestChatReasoningDistinguishesMissingMaxTokens(t *testing.T) {
	var reasoning ChatReasoning
	if err := json.Unmarshal([]byte(`{"effort":"high"}`), &reasoning); err != nil {
		t.Fatalf("decode effort-only reasoning: %v", err)
	}
	if reasoning.HasMaxTokens() {
		t.Fatalf("effort-only reasoning unexpectedly has max_tokens: %#v", reasoning)
	}
	raw, err := json.Marshal(reasoning)
	if err != nil {
		t.Fatalf("encode effort-only reasoning: %v", err)
	}
	if string(raw) != `{"effort":"high"}` {
		t.Fatalf("effort-only reasoning wire = %s", raw)
	}

	programmatic := &ChatReasoning{}
	programmatic.SetMaxTokens(0)
	if !programmatic.HasMaxTokens() {
		t.Fatal("programmatic explicit zero was not recorded")
	}
}

func chatCompletionWireValues(usage *Usage) []any {
	return []any{
		ChatCompletionResponse{
			ID: "chatcmpl_1", Object: "chat.completion", Choices: []ChatCompletionChoice{}, Usage: usage,
		},
		ChatCompletionStreamResponse{
			ID: "chatcmpl_1", Object: "chat.completion.chunk", Choices: []ChatCompletionStreamChoice{}, Usage: usage,
		},
		ChatCompletionStreamChoice{Index: 0, Usage: usage},
	}
}

func chatCompletionWireUsage(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal Chat wire value: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode Chat wire value: %v", err)
	}
	wireUsage, ok := payload["usage"].(map[string]any)
	if !ok {
		t.Fatalf("expected usage object, got %s", raw)
	}
	return wireUsage
}

func TestChatCompletionWireUsageKeepsOpenAICacheFields(t *testing.T) {
	usage := &Usage{
		PromptTokens:     11,
		CompletionTokens: 7,
		TotalTokens:      18,
		PromptTokensDetails: PromptTokensDetails{
			AudioTokens:          1,
			CachedTokens:         2,
			TextTokens:           3,
			ImageTokens:          4,
			CachedTokensInternal: 5,
			CacheWriteTokens:     6,
		},
		CompletionTokensDetails: CompletionTokensDetails{
			AudioTokens:              1,
			TextTokens:               2,
			ReasoningTokens:          3,
			AcceptedPredictionTokens: 4,
			RejectedPredictionTokens: 5,
			ImageTokens:              99,
		},
	}
	for _, value := range chatCompletionWireValues(usage) {
		wireUsage := chatCompletionWireUsage(t, value)
		promptDetails, ok := wireUsage["prompt_tokens_details"].(map[string]any)
		if !ok {
			t.Fatalf("expected prompt token details, got %#v", wireUsage)
		}
		for _, field := range []string{"audio_tokens", "cached_tokens", "text_tokens", "image_tokens", "cache_write_tokens"} {
			if _, ok := promptDetails[field]; !ok {
				t.Fatalf("expected Chat wire field %q, got %#v", field, promptDetails)
			}
		}
		if _, ok := promptDetails["cached_tokens_internal"]; ok {
			t.Fatalf("internal field escaped onto Chat wire: %#v", promptDetails)
		}
		completionDetails, ok := wireUsage["completion_tokens_details"].(map[string]any)
		if !ok {
			t.Fatalf("expected completion token details, got %#v", wireUsage)
		}
		for _, field := range []string{"audio_tokens", "text_tokens", "reasoning_tokens", "accepted_prediction_tokens", "rejected_prediction_tokens"} {
			if _, ok := completionDetails[field]; !ok {
				t.Fatalf("expected official completion detail %q, got %#v", field, completionDetails)
			}
		}
		if _, ok := completionDetails["image_tokens"]; ok {
			t.Fatalf("non-official completion detail image_tokens leaked: %#v", completionDetails)
		}
	}
	if usage.PromptTokensDetails.CacheWriteTokens != 6 || usage.PromptTokensDetails.CachedTokensInternal != 5 {
		t.Fatalf("wire projection mutated OpenAI usage: %+v", usage.PromptTokensDetails)
	}
	if usage.CompletionTokensDetails.ImageTokens != 99 {
		t.Fatalf("wire projection mutated completion details: %+v", usage.CompletionTokensDetails)
	}
}

func TestChatCompletionWireUsageOmitsClaudeCacheEvidence(t *testing.T) {
	usage := &Usage{
		PromptTokens:     11,
		CompletionTokens: 7,
		TotalTokens:      18,
		PromptTokensDetails: PromptTokensDetails{
			CacheCreationInputTokens: 7,
			CacheReadInputTokens:     8,
		},
	}
	for _, value := range chatCompletionWireValues(usage) {
		wireUsage := chatCompletionWireUsage(t, value)
		promptDetails, ok := wireUsage["prompt_tokens_details"].(map[string]any)
		if !ok {
			t.Fatalf("expected prompt token details, got %#v", wireUsage)
		}
		for _, field := range []string{"cached_tokens_internal", "cache_creation_input_tokens", "cache_read_input_tokens"} {
			if _, ok := promptDetails[field]; ok {
				t.Fatalf("internal field %q escaped onto Chat wire: %#v", field, promptDetails)
			}
		}
	}
	if usage.PromptTokensDetails.CacheCreationInputTokens != 7 || usage.PromptTokensDetails.CacheReadInputTokens != 8 {
		t.Fatalf("wire projection mutated Claude usage: %+v", usage.PromptTokensDetails)
	}
}

func TestChatToResponsesPreservesChatStoreSemantics(t *testing.T) {
	tests := []struct {
		name  string
		store *bool
		want  bool
	}{
		{name: "omitted defaults to false", store: nil, want: false},
		{name: "explicit false", store: boolPointer(false), want: false},
		{name: "explicit true", store: boolPointer(true), want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			converted := (&ChatCompletionRequest{Model: "gpt-5", Store: test.store}).ToResponsesRequest()
			if converted.Store == nil || *converted.Store != test.want {
				t.Fatalf("converted store=%v, want %v", converted.Store, test.want)
			}
		})
	}
}

func TestChatToResponsesMapsOnlyStringPromptCacheKey(t *testing.T) {
	converted := (&ChatCompletionRequest{PromptCacheKey: "cache-key"}).ToResponsesRequest()
	if converted.PromptCacheKey != "cache-key" {
		t.Fatalf("prompt_cache_key was not mapped: %q", converted.PromptCacheKey)
	}
	converted = (&ChatCompletionRequest{PromptCacheKey: map[string]any{"future": true}}).ToResponsesRequest()
	if converted.PromptCacheKey != "" {
		t.Fatalf("non-string prompt_cache_key was fabricated across protocols: %q", converted.PromptCacheKey)
	}
}

func boolPointer(value bool) *bool {
	return &value
}
