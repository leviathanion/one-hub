package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/logger"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

func TestResponsesWebSocketNegotiatesOptionalCompression(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() { logger.Logger = originalLogger })
	setResponsesWSTestRedisEnabled(t, false)
	setResponsesWSTestViperInt(t, "responses_ws.connect_per_credential_per_minute", -1)
	installResponsesWSTestAPILimiter(t, 60)

	for _, compression := range []bool{true, false} {
		t.Run(map[bool]string{true: "compressed", false: "uncompressed"}[compression], func(t *testing.T) {
			handlerDone := make(chan struct{})
			router := gin.New()
			router.GET("/v1/responses", func(c *gin.Context) {
				defer close(handlerDone)
				c.Set("id", 7)
				c.Set("token_id", nextResponsesWSConnectionAttemptTokenID())
				c.Set("group", "default")
				ResponsesWebSocket(c)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			dialer := websocket.Dialer{EnableCompression: compression}
			conn, response, err := dialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			negotiated := strings.Contains(response.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate")
			if negotiated != compression {
				t.Fatalf("compression negotiated=%v, offered=%v", negotiated, compression)
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":`)); err != nil {
				t.Fatal(err)
			}
			if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			_, payload, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			assertResponsesWSErrorPayload(t, string(payload), http.StatusBadRequest, responsesWSErrorCodeInvalidResponseCreate, responsesWSMessageInvalidResponseCreate)
			select {
			case <-handlerDone:
			case <-time.After(time.Second):
				t.Fatal("websocket handler did not release the connection")
			}
		})
	}
}
