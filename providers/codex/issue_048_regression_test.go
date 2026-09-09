package codex

import (
	"testing"

	"one-api/common/wsconn"
	"one-api/types"
)

func TestIssue048CodexSupplierPublishesSearchDeltaBeforeTerminal(t *testing.T) {
	for _, test := range []struct {
		name       string
		tool       string
		billingKey string
	}{
		{
			name:       "web_search_preview",
			tool:       types.APIToolTypeWebSearchPreview,
			billingKey: types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high"),
		},
		{
			name:       "web_search",
			tool:       types.APIToolTypeWebSearch,
			billingKey: types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "high"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &CodexProvider{}
			accumulator := newCodexTurnUsageAccumulator()
			created := []byte(`{"type":"response.created","response":{"id":"resp_i048","status":"in_progress","tools":[{"type":"` + test.tool + `","search_context_size":"high"}]}}`)
			if _, usage, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, created, accumulator); err != nil || usage != nil {
				t.Fatalf("response.created unexpectedly emitted usage: usage=%+v err=%v", usage, err)
			}

			done := []byte(`{"type":"response.output_item.done","item_id":"search_i048","output_index":0,"item":{"id":"search_i048","type":"web_search_call","status":"completed","action":{"type":"search"}}}`)
			shouldContinue, usage, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, done, accumulator)
			if err != nil || !shouldContinue {
				t.Fatalf("search done was not accepted: continue=%v usage=%+v err=%v", shouldContinue, usage, err)
			}
			if usage == nil || usage.ProviderTokenEvidence || usage.ExtraBilling[test.billingKey].CallCount != 1 || !usage.ProviderExtraBilling[test.billingKey] {
				t.Fatalf("search done did not publish an independent provider delta: %+v", usage)
			}

			_, duplicate, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, done, accumulator)
			if err != nil || duplicate != nil {
				t.Fatalf("duplicate search done was charged again: usage=%+v err=%v", duplicate, err)
			}

			terminal := []byte(`{"type":"response.completed","response":{"id":"resp_i048","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},"tools":[{"type":"` + test.tool + `","search_context_size":"high"}],"output":[{"id":"search_i048","type":"web_search_call","status":"completed","action":{"type":"search"}}]}}`)
			_, terminalUsage, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, terminal, accumulator)
			if err != nil || terminalUsage == nil || !terminalUsage.ProviderTokenEvidence || terminalUsage.InputTokens != 3 || terminalUsage.OutputTokens != 2 || terminalUsage.TotalTokens != 5 {
				t.Fatalf("completed terminal lost provider token evidence: usage=%+v err=%v", terminalUsage, err)
			}
			if len(terminalUsage.ExtraBilling) != 0 || len(terminalUsage.ProviderExtraBilling) != 0 {
				t.Fatalf("completed terminal duplicated the already-published search delta: %+v", terminalUsage)
			}
		})
	}
}

func TestIssue048CodexSearchDeltaSurvivesSupplierCloseErrorAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name       string
		terminal   []byte
		billingKey string
	}{
		{
			name:       "provider close follows top-level error",
			terminal:   []byte(`{"type":"error","error":{"type":"provider_error","message":"upstream closed"}}`),
			billingKey: types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium"),
		},
		{
			name:       "cancelled without usage",
			terminal:   []byte(`{"type":"response.cancelled","response":{"id":"resp_i048","status":"cancelled"}}`),
			billingKey: types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "medium"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &CodexProvider{}
			accumulator := newCodexTurnUsageAccumulator()
			tool := types.APIToolTypeWebSearchPreview
			if test.billingKey == types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "medium") {
				tool = types.APIToolTypeWebSearch
			}
			created := []byte(`{"type":"response.created","response":{"id":"resp_i048","status":"in_progress","tools":[{"type":"` + tool + `"}]}}`)
			if _, _, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, created, accumulator); err != nil {
				t.Fatalf("response.created failed: %v", err)
			}
			done := []byte(`{"type":"response.output_item.done","item_id":"search_i048","output_index":0,"item":{"id":"search_i048","type":"web_search_call","status":"completed","action":{"type":"search"}}}`)
			_, usage, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, done, accumulator)
			if err != nil || usage == nil || usage.ExtraBilling[test.billingKey].CallCount != 1 || !usage.ProviderExtraBilling[test.billingKey] {
				t.Fatalf("independent search usage was not emitted before terminal path: usage=%+v err=%v", usage, err)
			}
			_, lateUsage, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, test.terminal, accumulator)
			if err != nil || lateUsage != nil {
				t.Fatalf("terminal error/cancel unexpectedly changed already-emitted usage: usage=%+v err=%v", lateUsage, err)
			}
		})
	}
}

func TestIssue048CodexSuccessfulImageTerminalRemainsTerminalOnlyEvidence(t *testing.T) {
	provider := &CodexProvider{}
	accumulator := newCodexTurnUsageAccumulator()
	created := []byte(`{"type":"response.created","response":{"id":"resp_image_i048","status":"in_progress","tools":[{"type":"image_generation","model":"gpt-image-1","quality":"high","size":"1024x1024"}]}}`)
	if _, _, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, created, accumulator); err != nil {
		t.Fatalf("image response.created failed: %v", err)
	}
	done := []byte(`{"type":"response.output_item.done","item_id":"image_i048","output_index":0,"item":{"id":"image_i048","type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}}`)
	if _, usage, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, done, accumulator); err != nil || usage != nil {
		t.Fatalf("image output item prematurely published independent usage: usage=%+v err=%v", usage, err)
	}
	terminal := []byte(`{"type":"response.completed","response":{"id":"resp_image_i048","status":"completed","tools":[{"type":"image_generation","model":"gpt-image-1","quality":"high","size":"1024x1024"}],"output":[{"id":"image_i048","type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}]}}`)
	_, usage, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, terminal, accumulator)
	if err != nil || usage == nil {
		t.Fatalf("successful image terminal did not publish usage: usage=%+v err=%v", usage, err)
	}
	imageKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-1|high|1024x1024|0")
	if usage.ExtraBilling[imageKey].CallCount != 1 || !usage.ProviderExtraBilling[imageKey] {
		t.Fatalf("successful image terminal evidence missing: %+v", usage)
	}
}

func TestIssue048CodexUnknownToolDoesNotCreateSearchEvidence(t *testing.T) {
	provider := &CodexProvider{}
	accumulator := newCodexTurnUsageAccumulator()
	created := []byte(`{"type":"response.created","response":{"id":"resp_unknown_i048","status":"in_progress","tools":[{"type":"future_hosted_tool"}]}}`)
	if _, _, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, created, accumulator); err != nil {
		t.Fatalf("unknown tool response.created failed: %v", err)
	}
	done := []byte(`{"type":"response.output_item.done","item_id":"unknown_i048","output_index":0,"item":{"id":"unknown_i048","type":"future_hosted_call","status":"completed"}}`)
	_, usage, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, done, accumulator)
	if err != nil || usage != nil {
		t.Fatalf("unknown hosted tool created billing evidence: usage=%+v err=%v", usage, err)
	}
}
