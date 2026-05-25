package wsconn

import (
	"net/http"

	"github.com/gorilla/websocket"
)

func Subprotocols(r *http.Request) []string {
	return websocket.Subprotocols(r)
}

func IsUpgrade(r *http.Request) bool {
	return websocket.IsWebSocketUpgrade(r)
}
