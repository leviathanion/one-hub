package openai

import (
	"context"
	"fmt"
	"testing"

	"one-api/common/responsesws"
)

func TestOpenAIResponsesWSParallelToolEvidenceUsesResponseIdentity(t *testing.T) {
	adapter := &openAIResponsesWSAdapter{}
	send := func(payload string) responsesws.ProviderFrameResult {
		t.Helper()
		result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(payload)))
		if result.Err != nil || result.CloseTransport || result.EmitFrame == nil || string(result.EmitFrame.Payload()) != payload {
			t.Fatalf("wire changed: %+v", result)
		}
		return result
	}
	for _, id := range []string{"A", "B"} {
		send(fmt.Sprintf(`{"type":"response.created","stream_id":%q,"response":{"id":%q,"model":%q,"tools":[{"type":"web_search"}]}}`, id, id, "model-"+id))
	}
	// Both responses can reuse an item identifier; neither tracker owns the other.
	for _, id := range []string{"B", "A"} {
		payload := fmt.Sprintf(`{"type":"response.output_item.done","response_id":%q,"item_id":"same","item":{"id":"same","type":"web_search_call","status":"completed","action":{"type":"search"}}}`, id)
		result := send(payload)
		if result.Usage == nil || result.Usage.ResponseID != id || result.Usage.ResponseModel != "model-"+id || len(result.Usage.ExtraBilling) != 1 {
			t.Fatalf("wrong response evidence: %+v", result.Usage)
		}
		if duplicate := send(payload); duplicate.Usage != nil {
			t.Fatalf("duplicate tool event rebilled: %+v", duplicate.Usage)
		}
	}
	missing := send(`{"type":"response.output_item.done","stream_id":"A","item_id":"unknown","item":{"id":"unknown","type":"web_search_call","status":"completed","action":{"type":"search"}}}`)
	if missing.Usage != nil {
		t.Fatalf("lane alone invented usage owner: %+v", missing.Usage)
	}
	conflict := send(`{"type":"response.completed","response_id":"A","response":{"id":"B","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`)
	if conflict.Usage != nil {
		t.Fatalf("conflicting response identity was billed: %+v", conflict.Usage)
	}
}

func TestOpenAIResponsesWSTrackerCapacityDoesNotBlockWireOrTokens(t *testing.T) {
	adapter := &openAIResponsesWSAdapter{}
	for i := 0; i <= openAIResponsesWSMaxTrackedResponses; i++ {
		payload := fmt.Sprintf(`{"type":"response.created","response":{"id":"response-%d","tools":[{"type":"web_search"}]}}`, i)
		result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(payload)))
		if result.Err != nil || result.CloseTransport || result.EmitFrame == nil || string(result.EmitFrame.Payload()) != payload {
			t.Fatalf("capacity blocked delivery: %+v", result)
		}
	}
	if len(adapter.responses) != openAIResponsesWSMaxTrackedResponses {
		t.Fatalf("unbounded tracker count %d", len(adapter.responses))
	}
	payload := `{"type":"response.completed","sequence_number":{"future":true},"response":{"id":"response-64","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(payload)))
	if result.Err != nil || result.CloseTransport || result.Usage == nil || result.Usage.ResponseID != "response-64" || result.Usage.TotalTokens != 5 {
		t.Fatalf("tool capacity or sequence blocked independent tokens: %+v", result)
	}
	adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.completed","response":{"id":"response-0"}}`)))
	if len(adapter.responses) != openAIResponsesWSMaxTrackedResponses-1 {
		t.Fatal("terminal did not release observation capacity")
	}
}

func TestOpenAIResponsesWSSessionFramesPreserveExtensionsAndRemoveCredentials(t *testing.T) {
	adapter := &openAIResponsesWSAdapter{}
	for _, eventType := range []string{"session.created", "session.updated"} {
		safe := fmt.Sprintf(`{ "type":%q,"session":{"id":"session-1","future":{"client_secret":"business-data","big":9007199254740993}} }`, eventType)
		result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(safe)))
		if result.Err != nil || result.Filtered || result.EmitFrame == nil || string(result.EmitFrame.Payload()) != safe || result.Usage != nil {
			t.Fatalf("safe session frame changed: %+v", result)
		}
		secret := fmt.Sprintf(`{"type":%q,"session":{"id":"session-1","client_secret":{"value":"upstream-ephemeral-secret","expires_at":42},"future":{"client_secret":"business-data","big":9007199254740993}},"extension":[true]}`, eventType)
		result = adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(secret)))
		if result.Err != nil || result.Filtered || result.EmitFrame == nil || result.Usage != nil {
			t.Fatalf("session frame was not safely delivered: %+v", result)
		}
		want := fmt.Sprintf(`{"type":%q,"session":{"id":"session-1","future":{"client_secret":"business-data","big":9007199254740993}},"extension":[true]}`, eventType)
		if string(result.EmitFrame.Payload()) != want {
			t.Fatalf("credential or extension handling changed: %s", result.EmitFrame.Payload())
		}
	}
}

func TestOpenAIResponsesWSOpaqueIdentity(t *testing.T) {
	adapter := &openAIResponsesWSAdapter{}
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
	if result.Err != nil || result.Usage == nil || result.Usage.ResponseID != " Resp Raw " || result.Usage.ProviderEventID != " Event Raw " {
		t.Fatalf("usage identity changed: %+v", result.Usage)
	}
	if len(adapter.responses) != 2 {
		t.Fatal("terminal removed a different response observer")
	}
}
