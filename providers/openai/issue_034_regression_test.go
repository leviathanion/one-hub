package openai

import (
	"testing"

	commonresponses "one-api/common/responses"
	"one-api/types"
)

const issue034CompletedEvent = `data: {"type":"response.completed","response":{"id":"resp_i034","model":"gpt-5.6-actual","service_tier":"flex","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":3,"text_tokens":97},"output_tokens_details":{"reasoning_tokens":4,"text_tokens":16}}}}`

func TestFixI034AcceptedResponsesUsageAuthorizesOnlyProviderEvidence(t *testing.T) {
	t.Run("complete usage is marked at accepted producer boundary", func(t *testing.T) {
		handler := &OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "requested-model"}
		if err := handler.ObserveResponsesEvent(issue034CompletedEvent); err != nil {
			t.Fatalf("accepted completed event failed: %v", err)
		}
		usage := handler.Usage
		if !usage.ProviderReported || !usage.HasProviderUsage() {
			t.Fatalf("accepted provider usage was not authorized: %+v", usage)
		}
		if usage.PromptTokens != 100 || usage.CompletionTokens != 20 || usage.TotalTokens != 120 {
			t.Fatalf("accepted token totals changed: %+v", usage)
		}
		if usage.ResponseModel != "gpt-5.6-actual" || usage.ServiceTier != "flex" {
			t.Fatalf("accepted model/tier attribution changed: %+v", usage)
		}
		if usage.PromptTokensDetails.CachedTokens != 3 || usage.PromptTokensDetails.TextTokens != 97 || usage.CompletionTokensDetails.ReasoningTokens != 4 || usage.CompletionTokensDetails.TextTokens != 16 {
			t.Fatalf("accepted token details changed: %+v", usage)
		}

		if err := handler.ObserveResponsesEvent(issue034CompletedEvent); err != nil {
			t.Fatalf("repeated completed event failed: %v", err)
		}
		if usage.PromptTokens != 100 || usage.CompletionTokens != 20 || usage.TotalTokens != 120 {
			t.Fatalf("repeated terminal doubled usage: %+v", usage)
		}
	})

	for _, testCase := range []struct {
		name  string
		event string
	}{
		{
			name:  "missing usage",
			event: `data: {"type":"response.completed","response":{"id":"resp_i034_missing","model":"gpt-5.6-actual","status":"completed"}}`,
		},
		{
			name:  "invalid usage shape",
			event: `data: {"type":"response.completed","response":{"id":"resp_i034_invalid","status":"completed","usage":{"input_tokens":"100","output_tokens":20,"total_tokens":120}}}`,
		},
		{
			name:  "empty terminal response",
			event: `data: {"type":"response.completed","response":{}}`,
		},
		{
			name:  "unrelated event",
			event: `data: {"type":"response.output_text.delta","delta":"hello"}`,
		},
		{
			name:  "tool event usage is not token authorization",
			event: `data: {"type":"response.output_item.done","item":{"type":"web_search_call","id":"search_i034","status":"completed"},"response":{"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}}`,
		},
		{
			name:  "partial image usage is not token authorization",
			event: `data: {"type":"response.image_generation_call.partial_image","item_id":"image_i034","output_index":0,"partial_image_index":0,"item":{"type":"image_generation_call","id":"image_i034","status":"in_progress"},"response":{"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			handler := &OpenAIResponsesStreamHandler{Usage: &types.Usage{}}
			if err := handler.ObserveResponsesEvent(testCase.event); err != nil {
				t.Fatalf("negative event failed unexpectedly: %v", err)
			}
			if handler.Usage.ProviderReported || handler.Usage.HasProviderUsage() {
				t.Fatalf("negative event became provider billing evidence: %+v", handler.Usage)
			}
		})
	}
}

func TestFixI034AcceptedResponsesUsagePreservesLifecycleObserverConflict(t *testing.T) {
	handler := &OpenAIResponsesStreamHandler{Usage: &types.Usage{}}
	observer := commonresponses.NewStreamObserver()
	if err := observer.ObserveEvent(`data: {"type":"response.created","response":{"id":"resp_i034_a","model":"gpt-5"}}`); err != nil {
		t.Fatalf("created event failed: %v", err)
	}
	conflict := `data: {"type":"response.completed","response":{"id":"resp_i034_b","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}}`
	if err := observer.ObserveEvent(conflict); err == nil {
		t.Fatal("response identity conflict was accepted")
	}
	if handler.Usage.ProviderReported || handler.Usage.HasProviderUsage() {
		t.Fatalf("conflicting terminal became provider billing evidence: %+v", handler.Usage)
	}
}
