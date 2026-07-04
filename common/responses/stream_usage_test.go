package responses

import (
	"testing"

	"one-api/types"
)

func TestParseStreamUsageEventTracksReasoningSummaryAndTerminalDone(t *testing.T) {
	event, ok := ParseStreamUsageEvent([]byte(`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`))
	if !ok || event.Type != "response.reasoning_summary_text.delta" {
		t.Fatalf("expected reasoning summary event to be tracked, got event=%+v ok=%v", event, ok)
	}
	if delta, ok := StreamEventDeltaString(event.Delta); !ok || delta != "thinking" {
		t.Fatalf("expected string delta, got %q ok=%v", delta, ok)
	}

	event, ok = ParseStreamUsageEvent([]byte(`{"type":"response.done","response":{"id":"resp_done","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`))
	if !ok || event.Response == nil || event.Response.Usage == nil || event.Response.Usage.TotalTokens != 3 {
		t.Fatalf("expected response.done usage event, got event=%+v ok=%v", event, ok)
	}
}

func TestApplyResponsesOutputItemBillingCoversSharedToolTypes(t *testing.T) {
	usage := &types.Usage{}
	ApplyResponsesOutputItemBilling(usage, &types.ResponsesOutput{Type: types.InputTypeWebSearchCall}, "")
	ApplyResponsesOutputItemBilling(usage, &types.ResponsesOutput{Type: types.InputTypeCodeInterpreterCall}, "")
	ApplyResponsesOutputItemBilling(usage, &types.ResponsesOutput{Type: types.InputTypeFileSearchCall}, "")
	ApplyResponsesOutputItemBilling(usage, &types.ResponsesOutput{Type: types.InputTypeImageGenerationCall, Quality: "high", Size: "1024x1024"}, "")

	for _, key := range []string{
		types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium"),
		types.BuildExtraBillingKey(types.APIToolTypeCodeInterpreter, ""),
		types.BuildExtraBillingKey(types.APIToolTypeFileSearch, ""),
		types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "high-1024x1024"),
	} {
		if usage.ExtraBilling[key].CallCount != 1 {
			t.Fatalf("expected one billing event for %q, got %+v", key, usage.ExtraBilling)
		}
	}
}
