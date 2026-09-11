package responsesws

import (
	"context"
	"testing"
	"time"

	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
)

func TestNativeAuxiliarySendKeepsCreateReceiveIdentity(t *testing.T) {
	for _, auxiliary := range []string{
		`{"type":"response.inject","response_id":"response-A","input":{"future":true}}`,
		`{"type":"response.steer","previous_response_id":"response-B","input":[]}`,
	} {
		t.Run(auxiliary, func(t *testing.T) {
			client, server := wstest.Pair(t)
			session := NewNativeSession(client, nativeTestAdapter{}, NativeSessionOptions{})
			defer session.Abort("test_cleanup")
			defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
			for _, request := range []SendRequest{
				{AttemptID: "create-B", Frame: NewTextFrame([]byte(`{"type":"response.create","model":"test-model"}`))},
				{AttemptID: "auxiliary-A", Frame: NewTextFrame([]byte(auxiliary))},
			} {
				if result := session.SendClientWithResult(t.Context(), request); result.Status != ResponsesWSTransportSendAttempted {
					t.Fatalf("send failed: %+v", result)
				}
			}
			payload := `{"type":"response.output_text.delta","item_id":"item-B","delta":"B output"}`
			if err := server.WriteMessage(wsconn.TextMessage, []byte(payload)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			event, err := session.Recv(ctx)
			if err != nil || event.AttemptID != "create-B" || event.Frame == nil || string(event.Frame.Payload()) != payload {
				t.Fatalf("auxiliary command changed B output: %+v %v", event, err)
			}
		})
	}
}
