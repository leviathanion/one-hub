package requester

import (
	"net"

	"github.com/gorilla/websocket"
)

// WriteWSLocalError writes a caller-built protocol-compatible local error frame
// through the shared client writer. It deliberately has no terminal, quota, or
// retry side effects; callers own those protocol decisions.
func WriteWSLocalError(writer *WSClientWriter, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	if writer == nil {
		return net.ErrClosed
	}
	return writer.WriteMessage(websocket.TextMessage, payload)
}
