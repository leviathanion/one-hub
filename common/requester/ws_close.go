package requester

import (
	"encoding/binary"
	"unicode/utf8"
)

// wsCloseReasonMaxBytes is the maximum number of UTF-8 bytes that may appear in
// a WebSocket close-frame reason. RFC 6455 caps control-frame payloads at 125
// bytes; the first 2 bytes carry the status code, leaving 123 bytes for the
// UTF-8 reason text.
const wsCloseReasonMaxBytes = 123

// SafeWSCloseReason truncates reason to fit within the WebSocket close-frame
// payload budget while preserving UTF-8 validity. Multi-byte sequences are
// never cut mid-rune; invalid UTF-8 bytes in the input are dropped.
func SafeWSCloseReason(reason string) string {
	if len(reason) <= wsCloseReasonMaxBytes && utf8.ValidString(reason) {
		return reason
	}

	out := make([]byte, 0, min(len(reason), wsCloseReasonMaxBytes))
	for len(reason) > 0 {
		r, size := utf8.DecodeRuneInString(reason)
		if r == utf8.RuneError && size == 1 {
			reason = reason[size:]
			continue
		}
		runeLen := utf8.RuneLen(r)
		if runeLen < 0 || len(out)+runeLen > wsCloseReasonMaxBytes {
			break
		}
		out = append(out, reason[:size]...)
		reason = reason[size:]
	}
	return string(out)
}

// SafeWSCloseMessage formats a WebSocket close frame payload with status code
// and a UTF-8 safe, length-bounded reason. Mirrors gorilla websocket's
// FormatCloseMessage but guarantees the reason fits within the control-frame
// budget without breaking UTF-8 boundaries.
func SafeWSCloseMessage(code int, reason string) []byte {
	if code == 1005 {
		return []byte{}
	}
	safe := SafeWSCloseReason(reason)
	buf := make([]byte, 2+len(safe))
	binary.BigEndian.PutUint16(buf, uint16(code))
	copy(buf[2:], safe)
	return buf
}
