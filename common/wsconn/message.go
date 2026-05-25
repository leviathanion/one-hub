package wsconn

import "errors"

// MessageType is the only data-frame type business code may observe or write.
// Ping, pong, and close control frames are intentionally not exported.
type MessageType int

const (
	TextMessage   MessageType = 1
	BinaryMessage MessageType = 2
)

var ErrInvalidMessageType = errors.New("wsconn: invalid message type")

func validDataMessageType(mt MessageType) bool {
	return mt == TextMessage || mt == BinaryMessage
}
