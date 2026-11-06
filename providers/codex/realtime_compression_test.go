package codex

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/wsconn"

	"github.com/gorilla/websocket"
)

func TestCodexRealtimeOffersCompression(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "negotiated", false: "peer declines"}[enabled], func(t *testing.T) {
			upgrader := websocket.Upgrader{EnableCompression: enabled}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate") {
					t.Error("Codex did not offer WebSocket compression")
				}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer conn.Close()
				mt, payload, err := conn.ReadMessage()
				if err == nil {
					err = conn.WriteMessage(mt, payload)
				}
				if err != nil {
					t.Errorf("echo: %v", err)
				}
			}))
			defer server.Close()
			provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
			conn, apiErr := provider.dialChatRealtimeConnWithContext(t.Context(), &codexRealtimeConnPlan{
				wsURL: "ws" + strings.TrimPrefix(server.URL, "http"), allowSelfHosted: true,
			})
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			defer conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
			payload := `{"type":"response.create","future_field":9007199254740993,"input":"` + strings.Repeat("hello", 1024) + `"}`
			if err := conn.WriteMessage(wsconn.TextMessage, []byte(payload)); err != nil {
				t.Fatal(err)
			}
			mt, got, err := conn.ReadInitial(t.Context())
			if err != nil || mt != wsconn.TextMessage || string(got) != payload {
				t.Fatalf("Codex compressed roundtrip: type=%d bytes=%d err=%v", mt, len(got), err)
			}
		})
	}
}
