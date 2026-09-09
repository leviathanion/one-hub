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

func TestChatCompletionWireUsageOmitsInternalCacheDetails(t *testing.T) {
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
			CachedWriteTokens:    7,
			CachedReadTokens:     8,
		},
	}

	tests := []struct {
		name  string
		value any
	}{
		{
			name: "response",
			value: ChatCompletionResponse{
				ID: "chatcmpl_1", Object: "chat.completion", Choices: []ChatCompletionChoice{}, Usage: usage,
			},
		},
		{
			name: "stream response",
			value: ChatCompletionStreamResponse{
				ID: "chatcmpl_1", Object: "chat.completion.chunk", Choices: []ChatCompletionStreamChoice{}, Usage: usage,
			},
		},
		{
			name:  "stream choice",
			value: ChatCompletionStreamChoice{Index: 0, Usage: usage},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.value)
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
			details, ok := wireUsage["prompt_tokens_details"].(map[string]any)
			if !ok {
				t.Fatalf("expected prompt token details, got %s", raw)
			}
			for _, field := range []string{"audio_tokens", "cached_tokens", "text_tokens", "image_tokens", "cache_write_tokens"} {
				if _, ok := details[field]; !ok {
					t.Fatalf("expected Chat wire field %q, got %s", field, raw)
				}
			}
			for _, field := range []string{"cached_tokens_internal", "cached_write_tokens", "cached_read_tokens"} {
				if _, ok := details[field]; ok {
					t.Fatalf("internal field %q escaped onto Chat wire: %s", field, raw)
				}
			}
		})
	}

	if usage.PromptTokensDetails.CachedReadTokens != 8 || usage.PromptTokensDetails.CachedWriteTokens != 7 {
		t.Fatalf("wire projection mutated internal usage: %+v", usage.PromptTokensDetails)
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
