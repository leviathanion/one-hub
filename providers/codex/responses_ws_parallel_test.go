package codex

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"one-api/common/responsesws"
	"one-api/types"
)

func TestCodexResponsesWSParallelEvidenceHasExplicitIdentity(t *testing.T) {
	a := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5"}
	send := func(payload string) responsesws.ProviderFrameResult {
		t.Helper()
		result := a.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(payload)))
		if result.Err != nil || result.CloseTransport || result.EmitFrame == nil {
			t.Fatalf("delivery failed: %+v", result)
		}
		return result
	}
	for _, lane := range []string{"a", "b"} {
		send(fmt.Sprintf(`{"type":"response.created","stream_id":%q,"response":{"id":%q,"model":"gpt-5"}}`, lane, "resp_"+lane))
	}
	if _, err := a.PrepareClientFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.create","stream_id":"c","model":"gpt-5","input":"next"}`))); err != nil {
		t.Fatal(err)
	}
	if len(a.responses) != 2 {
		t.Fatal("client create discarded inflight evidence")
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	for _, lane := range []string{"b", "a"} {
		result := send(fmt.Sprintf(`{"type":"response.output_item.done","stream_id":%q,"response_id":%q,"item_id":"same_item_id","output_index":0,"item":{"id":"same_item_id","type":"web_search_call","status":"completed","action":{"type":"search"}}}`, lane, "resp_"+lane))
		if result.Usage == nil || result.Usage.ResponseID != "resp_"+lane || result.Usage.ExtraBilling[key].CallCount != 1 {
			t.Fatalf("cross-response evidence collision: %+v", result.Usage)
		}
	}
	for index, lane := range []string{"b", "a"} {
		result := send(fmt.Sprintf(`{"type":"response.done","stream_id":%q,"sequence_number":0,"response":{"id":%q,"status":"completed","usage":{"input_tokens":%d,"output_tokens":1,"total_tokens":%d},"output":[{"id":"same_item_id","type":"web_search_call","status":"completed","action":{"type":"search"}}]}}`, lane, "resp_"+lane, index+3, index+4))
		if result.Usage == nil || result.Usage.ResponseID != "resp_"+lane || result.Usage.InputTokens != index+3 || result.Usage.ExtraBilling[key].CallCount != 0 {
			t.Fatalf("terminal evidence mixed or tool double counted: %+v", result.Usage)
		}
	}
	if len(a.responses) != 0 {
		t.Fatalf("terminal observers retained: %d", len(a.responses))
	}
}

func TestCodexResponsesWSUnattributableAndFutureEventsStayRaw(t *testing.T) {
	a := &codexResponsesWSAdapter{provider: &CodexProvider{}}
	for _, payload := range []string{
		`{"type":"response.output_item.done","stream_id":"b","item":{"id":"item","type":"web_search_call","status":"completed","action":{"type":"search"}}}`,
		`{"type":"response.completed","response_id":"wrong","response":{"id":"other","usage":{"input_tokens":99}}}`,
		`{"type":"response.done","stream_id":"b","response":{"status":"completed","usage":{"input_tokens":99}}}`,
		`{"type":"response.completed","stream_id":"b","response":{"status":"completed","usage":{"input_tokens":99}}}`,
		`{"type":"response.done","response":{"status":"cancelled","usage":{"input_tokens":99}}}`,
		`{"type":"response.future","response":{"id":"future","status":"completed","usage":{"input_tokens":99}},"sequence_number":{"future":true}}`,
	} {
		result := a.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(payload)))
		if result.Err != nil || result.CloseTransport || result.EmitFrame == nil || string(result.EmitFrame.Payload()) != payload || result.Usage != nil {
			t.Fatalf("unattributable or future event affected: %+v", result)
		}
	}
}

func TestCodexResponsesWSObservationBudgetNeverGatesDelivery(t *testing.T) {
	a := &codexResponsesWSAdapter{provider: &CodexProvider{}}
	largeID := fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, strings.Repeat("x", 1025))
	result := a.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(largeID)))
	if result.Err != nil || result.EmitFrame == nil || string(result.EmitFrame.Payload()) != largeID || len(a.responses) != 0 {
		t.Fatalf("large identity must skip observation only: %+v", result)
	}
	for index := 0; index < 80; index++ {
		payload := fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_%d"}}`, index)
		result := a.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(payload)))
		if result.Err != nil || result.CloseTransport || result.EmitFrame == nil || string(result.EmitFrame.Payload()) != payload {
			t.Fatalf("budget gated delivery: %+v", result)
		}
	}
	payload := fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, strings.Repeat("x", 1025))
	a.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(payload)))
	if len(a.responses) != codexResponsesWSMaxTrackedResponses {
		t.Fatalf("unbounded observers: %d", len(a.responses))
	}
	a.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_0","usage":{"input_tokens":1}}}`)))
	a.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.created","response":{"id":"resp_new"}}`)))
	if a.responses["resp_new"] == nil {
		t.Fatal("terminal did not release observation capacity")
	}
	a.MapProviderClose(context.Background(), responsesws.ProviderCloseInfo{})
	if len(a.responses) != 0 {
		t.Fatal("connection close retained observers")
	}
}

func TestCodexResponsesWSOpaqueIdentity(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5"}
	for _, id := range []string{" Resp Raw ", "Resp Raw", " resp raw "} {
		raw := fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"model":"gpt-5"}}`, id)
		result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(raw)))
		if result.Err != nil || result.EmitFrame == nil || string(result.EmitFrame.Payload()) != raw {
			t.Fatalf("created changed: %+v", result)
		}
	}
	if len(adapter.responses) != 3 {
		t.Fatal("opaque response identities shared an observer")
	}
	raw := `{"type":"response.completed","event_id":" Event Raw ","response":{"id":" Resp Raw ","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(raw)))
	if result.Err != nil || result.Usage == nil || result.Usage.ResponseID != " Resp Raw " || string(result.EmitFrame.Payload()) != raw {
		t.Fatalf("usage identity changed: %+v", result.Usage)
	}
	if len(adapter.responses) != 2 {
		t.Fatal("terminal removed a different response observer")
	}
}
