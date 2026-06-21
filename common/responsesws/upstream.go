package responsesws

import (
	"context"
	"errors"
	"strings"

	runtimesession "one-api/runtime/session"
	"one-api/types"
)

var ErrUpstreamClosed = errors.New("responses websocket upstream closed")
var ErrInvalidFrame = errors.New("invalid responses websocket frame")
var ErrStaleContinuation = errors.New("stale responses websocket continuation")
var ErrMissingAttemptID = errors.New("responses websocket send attempt id is required")

// FrameKind identifies the payload shape carried by a ResponsesWS frame.
type FrameKind int

const (
	FrameKindText FrameKind = iota + 1
	FrameKindBinary
)

// Frame carries a downstream/upstream data frame without exposing transport
// message-type integers outside the ResponsesWS boundary.
type Frame struct {
	kind    FrameKind
	payload []byte
}

func NewTextFrame(payload []byte) Frame {
	return Frame{kind: FrameKindText, payload: append([]byte(nil), payload...)}
}

func NewBinaryFrame(payload []byte) Frame {
	return Frame{kind: FrameKindBinary, payload: append([]byte(nil), payload...)}
}

func (f Frame) Kind() FrameKind {
	return f.kind
}

// Payload returns a copy of the frame payload. Use PayloadLen when only the
// length is needed.
func (f Frame) Payload() []byte {
	return f.ClonePayload()
}

func (f Frame) PayloadLen() int {
	return len(f.payload)
}

func (f Frame) ClonePayload() []byte {
	return append([]byte(nil), f.payload...)
}

func (f Frame) IsZero() bool {
	return f.kind == 0 && f.payload == nil
}

// PayloadOrigin is derived from RecvDetailOrigin for code that still needs the
// coarse provider/proxy-local distinction at an IO boundary.
type PayloadOrigin int

const (
	PayloadOriginProxyLocal PayloadOrigin = iota
	PayloadOriginProvider
)

// ProviderClose carries upstream provider close evidence to the relay actor.
type ProviderClose struct {
	Code   int
	Reason string
	Err    error
}

// UpstreamEvent is the common event shape emitted by ResponsesWS transports.
// It is evidence only; the relay actor owns accounting and terminal side effects.
type UpstreamEvent struct {
	Frame         *Frame
	ProviderClose *ProviderClose
	Usage         *types.UsageEvent
	AttemptID     string
	ResponseID    string
	DetailOrigin  RecvDetailOrigin
	DetailPhase   RecvDetailPhase
	Err           error
}

// OpenOptions configures a provider ResponsesWS upstream open.
type OpenOptions struct {
	UpstreamSessionID  string
	PreviousResponseID string
	Transport          runtimesession.TransportMode
	ChannelID          int
	Diagnostics        DiagnosticHook
}

// SendRequest carries ResponsesWS protocol identity explicitly. Context remains
// for cancellation, deadlines, and logging metadata only.
type SendRequest struct {
	AttemptID                 string
	Frame                     Frame
	DefaultPreviousResponseID string
}

func validateClientAttemptID(req SendRequest) error {
	if req.Frame.Kind() != FrameKindText {
		return nil
	}
	envelope, err := ParseClientEventEnvelope(req.Frame.Payload())
	if err != nil {
		return nil
	}
	switch envelope.Type {
	case "response.create", "response.cancel":
		if strings.TrimSpace(req.AttemptID) == "" {
			return ErrMissingAttemptID
		}
	}
	return nil
}

type TransportSendCapable interface {
	SendClientWithResult(ctx context.Context, req SendRequest) ResponsesWSTransportSendResult
}

// Upstream is the transport boundary consumed by the relay actor.
type Upstream interface {
	TransportSendCapable
	Recv(ctx context.Context) (UpstreamEvent, error)
	Abort(reason string)
}

// ControlSendCapable marks upstreams with a dedicated control lane for events
// such as response.cancel.
type ControlSendCapable interface {
	SendControl(ctx context.Context, req SendRequest) ResponsesWSTransportSendResult
}

// BridgeContinuationDefaultCapable marks bridge sessions that can safely inject
// a relay-owned default previous_response_id.
type BridgeContinuationDefaultCapable interface {
	SupportsBridgeContinuationDefault() bool
}

func PayloadOriginForDetailOrigin(origin RecvDetailOrigin) PayloadOrigin {
	if coarse, ok := ExpectedPayloadOriginForRecvDetailOrigin(origin); ok &&
		coarse == PayloadOriginProvider {
		return PayloadOriginProvider
	}
	return PayloadOriginProxyLocal
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
	return &ClientPayloadError{
		cause:   cause,
		payload: append([]byte(nil), payload...),
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

// OpenAIErrorWithCause wraps a typed OpenAI API error while preserving an
// underlying transport or adapter cause for errors.As/errors.Is callers.
type OpenAIErrorWithCause struct {
	apiErr *types.OpenAIErrorWithStatusCode
	cause  error
}

func NewOpenAIErrorWithCause(apiErr *types.OpenAIErrorWithStatusCode, cause error) error {
	if apiErr == nil {
		return cause
	}
	return &OpenAIErrorWithCause{apiErr: apiErr, cause: cause}
}

func (e *OpenAIErrorWithCause) Error() string {
	if e == nil || e.apiErr == nil {
		return ""
	}
	return e.apiErr.Error()
}

func (e *OpenAIErrorWithCause) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *OpenAIErrorWithCause) As(target any) bool {
	if e == nil || e.apiErr == nil {
		return false
	}
	apiErrTarget, ok := target.(**types.OpenAIErrorWithStatusCode)
	if !ok {
		return false
	}
	*apiErrTarget = e.apiErr
	return true
}

// SendPreflightCapable lets a provider fail a ResponsesWS response.create
// before relay-side RPM/quota admission. It is optional and scoped to the
// ResponsesWS upstream boundary.
type SendPreflightCapable interface {
	PreflightResponsesWSSend(ctx context.Context, eventID string, request *types.OpenAIResponsesRequest) error
}
