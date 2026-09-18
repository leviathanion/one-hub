package types

import (
	"encoding/json"
	"testing"
)

func internalCacheFieldNames() []string {
	return []string{"cache_creation_input_tokens", "cache_read_input_tokens", "cached_tokens_internal"}
}

func decodeWireUsage(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal wire value: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode wire value: %v", err)
	}
	usage, ok := payload["usage"].(map[string]any)
	if !ok {
		t.Fatalf("expected usage object, got %s", raw)
	}
	return usage
}

func TestCompletionWireUsageMatchesOfficialFields(t *testing.T) {
	response := CompletionResponse{
		ID:      "cmpl_1",
		Object:  "text_completion",
		Choices: []CompletionChoice{},
		Usage: &Usage{
			PromptTokens:     11,
			CompletionTokens: 7,
			TotalTokens:      18,
			PromptTokensDetails: PromptTokensDetails{
				AudioTokens:              9,
				CachedTokens:             2,
				CacheWriteTokens:         6,
				TextTokens:               3,
				ImageTokens:              4,
				CachedTokensInternal:     5,
				CacheCreationInputTokens: 7,
				CacheReadInputTokens:     8,
			},
			CompletionTokensDetails: CompletionTokensDetails{
				AudioTokens:              1,
				TextTokens:               2,
				ReasoningTokens:          3,
				AcceptedPredictionTokens: 4,
				RejectedPredictionTokens: 5,
				ImageTokens:              99,
			},
		},
	}
	usage := decodeWireUsage(t, response)

	promptDetails, ok := usage["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("expected prompt token details, got %#v", usage)
	}
	for _, field := range []string{"audio_tokens", "cached_tokens", "text_tokens", "image_tokens", "cache_write_tokens"} {
		if _, ok := promptDetails[field]; !ok {
			t.Fatalf("expected official prompt detail %q, got %#v", field, promptDetails)
		}
	}
	for _, field := range internalCacheFieldNames() {
		if _, ok := promptDetails[field]; ok {
			t.Fatalf("internal field %q leaked onto completion wire: %#v", field, promptDetails)
		}
	}

	completionDetails, ok := usage["completion_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("expected completion token details, got %#v", usage)
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

func TestEmbeddingWireUsageMatchesOfficialFields(t *testing.T) {
	response := EmbeddingResponse{
		Object: "list",
		Data:   []Embedding{},
		Model:  "embed-test",
		Usage: &Usage{
			PromptTokens: 5,
			TotalTokens:  5,
			PromptTokensDetails: PromptTokensDetails{
				CachedTokens:             2,
				CacheWriteTokens:         6,
				CacheCreationInputTokens: 7,
				CacheReadInputTokens:     8,
				CachedTokensInternal:     1,
			},
			CompletionTokensDetails: CompletionTokensDetails{ImageTokens: 99},
		},
	}
	usage := decodeWireUsage(t, response)

	if len(usage) != 2 || usage["prompt_tokens"] != float64(5) || usage["total_tokens"] != float64(5) {
		t.Fatalf("expected official embeddings usage shape, got %#v", usage)
	}
}

func TestRerankWireUsageMatchesJinaFields(t *testing.T) {
	response := RerankResponse{
		Model: "rerank-test",
		Usage: &Usage{
			PromptTokens: 5,
			TotalTokens:  5,
			PromptTokensDetails: PromptTokensDetails{
				CacheCreationInputTokens: 7,
				CacheReadInputTokens:     8,
				CachedTokensInternal:     1,
			},
			CompletionTokensDetails: CompletionTokensDetails{ImageTokens: 99},
		},
		Results: []RerankResult{},
	}
	usage := decodeWireUsage(t, response)

	if len(usage) != 1 || usage["total_tokens"] != float64(5) {
		t.Fatalf("expected Jina-style rerank usage shape, got %#v", usage)
	}
}
