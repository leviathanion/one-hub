package wsconn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

type CloseCode int

const (
	CloseNormalClosure           CloseCode = 1000
	CloseGoingAway               CloseCode = 1001
	CloseProtocolError           CloseCode = 1002
	CloseUnsupportedData         CloseCode = 1003
	CloseNoStatusReceived        CloseCode = 1005
	CloseAbnormalClosure         CloseCode = 1006
	CloseInvalidFramePayloadData CloseCode = 1007
	ClosePolicyViolation         CloseCode = 1008
	CloseMessageTooBig           CloseCode = 1009
	CloseInternalServerErr       CloseCode = 1011
	CloseServiceRestart          CloseCode = 1012
	CloseTryAgainLater           CloseCode = 1013
)

type CloseError struct {
	Code   CloseCode
	Reason string
}

func (e *CloseError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("wsconn: close %d: %s", e.Code, e.Reason)
}

type CloseKind string

const (
	CloseKindUnknown          CloseKind = ""
	CloseKindNormal           CloseKind = "normal"
	CloseKindGracefulShutdown CloseKind = "graceful_shutdown"
	CloseKindAbort            CloseKind = "abort"
	CloseKindBackpressure     CloseKind = "backpressure"
	CloseKindPeerClose        CloseKind = "peer_close"
	CloseKindPongMiss         CloseKind = "pong_miss"
	CloseKindInboundIdle      CloseKind = "inbound_idle"
	CloseKindReadError        CloseKind = "read_error"
	CloseKindWriteError       CloseKind = "write_error"
	CloseKindHandlerPanic     CloseKind = "handler_panic"
	CloseKindDialFailed       CloseKind = "dial_failed"
)

type CloseInfo struct {
	Kind   CloseKind
	Code   CloseCode
	Reason string
	Err    error
	At     time.Time
}

const closeReasonMaxBytes = 123

func SafeCloseReason(reason string) string {
	if len(reason) <= closeReasonMaxBytes && utf8.ValidString(reason) {
		return reason
	}
	out := make([]byte, 0, min(len(reason), closeReasonMaxBytes))
	for len(reason) > 0 {
		r, size := utf8.DecodeRuneInString(reason)
		if r == utf8.RuneError && size == 1 {
			reason = reason[size:]
			continue
		}
		runeLen := utf8.RuneLen(r)
		if runeLen < 0 || len(out)+runeLen > closeReasonMaxBytes {
			break
		}
		out = append(out, reason[:size]...)
		reason = reason[size:]
	}
	return string(out)
}

func SafeCloseMessage(code CloseCode, reason string) []byte {
	if code == CloseNoStatusReceived {
		return []byte{}
	}
	safe := SafeCloseReason(reason)
	buf := make([]byte, 2+len(safe))
	binary.BigEndian.PutUint16(buf, uint16(code))
	copy(buf[2:], safe)
	return buf
}

func safeCloseMessage(code CloseCode, reason string) []byte {
	return SafeCloseMessage(code, reason)
}

func SanitizeWireCloseCode(code int) CloseCode {
	switch {
	case code >= 1000 && code <= 1003:
		return CloseCode(code)
	case code >= 1007 && code <= 1014:
		return CloseCode(code)
	case code >= 3000 && code <= 4999:
		return CloseCode(code)
	default:
		logWarnf("wsconn: invalid close code %d; using 1011", code)
		return CloseInternalServerErr
	}
}

func wireCloseCodeFor(info CloseInfo) CloseCode {
	code := info.Code
	if code == 0 {
		switch info.Kind {
		case CloseKindNormal:
			code = CloseNormalClosure
		case CloseKindGracefulShutdown, CloseKindInboundIdle:
			code = CloseGoingAway
		case CloseKindBackpressure:
			code = CloseTryAgainLater
		default:
			code = CloseInternalServerErr
		}
	}
	return SanitizeWireCloseCode(int(code))
}

func shouldSendCloseFrame(info CloseInfo) bool {
	switch info.Kind {
	case CloseKindNormal, CloseKindGracefulShutdown, CloseKindBackpressure, CloseKindInboundIdle:
		return true
	default:
		return false
	}
}

func convertCloseError(err error) error {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return &CloseError{Code: CloseCode(closeErr.Code), Reason: closeErr.Text}
	}
	return err
}
