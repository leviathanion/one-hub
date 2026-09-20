package responsesws

import (
	"context"
	"testing"
	"time"

	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
)

func TestNativeParallelFramesNeverUseLastSendAttempt(t *testing.T) {
	client, server := wstest.Pair(t)
	session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{})
	defer session.Abort("test_cleanup")
	defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
	for _, id := range []string{"attempt-A", "attempt-B"} {
		sent := session.SendClientWithResult(t.Context(), SendRequest{AttemptID: id, Frame: NewTextFrame([]byte(`{"type":"response.create"}`))})
		if sent.Err != nil {
			t.Fatal(sent.Err)
		}
	}
	for _, tc := range []struct{ payload, responseID string }{
		{`{"type":"response.completed","response":{"id":"A"}}`, "A"},
		{`{"type":"response.output_item.done","response_id":"B"}`, "B"},
		{`{"type":"error","stream_id":"A","error":{"message":"future"}}`, ""},
		{`{"type":"response.completed","response_id":"A","response":{"id":"B"}}`, ""},
	} {
		payload := tc.payload
		if err := server.WriteMessage(wsconn.TextMessage, []byte(payload)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		event, err := session.Recv(ctx)
		cancel()
		if err != nil || event.Frame == nil || string(event.Frame.Payload()) != payload || event.AttemptID != "" || event.ResponseID != tc.responseID {
			t.Fatalf("transport invented correlation: %+v %v", event, err)
		}
	}
}
