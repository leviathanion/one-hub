package realtime

import (
	"context"
	"errors"

	runtimesession "one-api/runtime/session"
	"one-api/types"
)

var ErrSessionClosed = errors.New("realtime session closed")
var ErrInvalidFrame = errors.New("invalid realtime frame")

type FrameKind int

const (
	FrameKindText FrameKind = iota + 1
	FrameKindBinary
)

// Frame carries a downstream/upstream data frame without exposing transport
// message-type integers outside the session boundary. Callers transfer payload
// ownership to NewTextFrame/NewBinaryFrame; after construction the payload is
// treated as immutable. Payload returns read-only bytes and intentionally does
// not clone to avoid copying large audio chunks. Code that sends a Frame across
// goroutines or channels must copy before publishing when ownership is shared.
type Frame struct {
	kind    FrameKind
	payload []byte
}

func NewTextFrame(payload []byte) Frame {
	return Frame{kind: FrameKindText, payload: payload}
}

func NewBinaryFrame(payload []byte) Frame {
	return Frame{kind: FrameKindBinary, payload: payload}
}

func (f Frame) Kind() FrameKind {
	return f.kind
}

func (f Frame) Payload() []byte {
	return f.payload
}

func (f Frame) ClonePayload() []byte {
	return append([]byte(nil), f.payload...)
}

func (f Frame) IsZero() bool {
	return f.kind == 0 && f.payload == nil
}

func (f Frame) valid() bool {
	return f.kind == FrameKindText || f.kind == FrameKindBinary
}

// ClientPayloadError marks an error that carries an explicit client-facing
// websocket payload. Callers should forward Payload as-is after delivering any
// primary Recv payload, instead of serializing err.Error() generically.
type ClientPayloadError struct {
	cause   error
	payload []byte
}

func NewClientPayloadError(cause error, payload []byte) error {
	if cause == nil && len(payload) == 0 {
		return nil
	}
	clonedPayload := append([]byte(nil), payload...)
	return &ClientPayloadError{
		cause:   cause,
		payload: clonedPayload,
	}
}

func (e *ClientPayloadError) Error() string {
	if e == nil {
		return ""
	}
	if e.cause != nil {
		return e.cause.Error()
	}
	return string(e.payload)
}

func (e *ClientPayloadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func ClientPayloadFromError(err error) []byte {
	var payloadErr *ClientPayloadError
	if !errors.As(err, &payloadErr) || payloadErr == nil || len(payloadErr.payload) == 0 {
		return nil
	}
	return append([]byte(nil), payloadErr.payload...)
}

type RealtimePayloadOrigin int

const (
	RealtimePayloadOriginProxyLocal RealtimePayloadOrigin = iota
	RealtimePayloadOriginProvider
)

type ProviderClose struct {
	Code   int
	Reason string
	Err    error
}

type RecvEvent struct {
	Frame         *Frame
	ProviderClose *ProviderClose
	Usage         *types.UsageEvent
	Origin        RealtimePayloadOrigin
	Err           error
}

// RealtimeSession is the session-facing surface for realtime callers.
type RealtimeSession interface {
	SendClient(ctx context.Context, frame Frame) error
	// Recv returns the next supplier event for the downstream WS client.
	// Provider business errors are reported through RecvEvent.Err; the top-level
	// error means there is no event to consume.
	Recv(ctx context.Context) (RecvEvent, error)
	// Detach releases the current downstream attachment without force-closing the
	// upstream provider transport. Implementations may continue draining and
	// finalizing turn state after Detach, and may explicitly stop queueing new
	// downstream frames during a bounded grace window once no consumer remains.
	Detach(reason string)
	// Abort force-closes the underlying provider transport and any remaining
	// realtime session work.
	Abort(reason string)
	SetTurnObserverFactory(factory runtimesession.TurnObserverFactory)
}

// GracefulDetachCapable lets sessions opt into downstream detach-on-close
// semantics when the client disconnects gracefully. Sessions that do not
// implement this interface, or return false, are aborted instead.
type GracefulDetachCapable interface {
	SupportsGracefulDetach() bool
}

type RealtimeOpenOptions struct {
	Context                   context.Context
	ClientSessionID           string
	ResolvedUpstreamSessionID string
	ForceFresh                bool
	PreferredTransport        runtimesession.TransportMode
	RequireWS                 bool
}
