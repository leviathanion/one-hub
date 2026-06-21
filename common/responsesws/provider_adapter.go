package responsesws

import (
	"context"
	"errors"

	"one-api/types"
)

var ErrInvalidProviderFrameResult = errors.New("invalid responses websocket provider frame result")

// ProviderAdapter maps provider-native websocket traffic into ResponsesWS
// transport evidence. Relay actor code remains the accounting owner.
type ProviderAdapter interface {
	// PrepareClientFrame validates and rewrites client payload shape before
	// transport send. It must not own quota, admission, or terminal accounting.
	PrepareClientFrame(ctx context.Context, frame Frame) (Frame, error)
	// HandleProviderFrame filters provider bootstrap/control frames, extracts
	// usage, rewrites payloads, and maps provider frame parse errors.
	HandleProviderFrame(ctx context.Context, frame Frame) ProviderFrameResult
	// MapProviderClose converts an upstream close into provider close evidence
	// or a proxy-local transport error; it does not finalize actor accounting.
	MapProviderClose(ctx context.Context, info ProviderCloseInfo) ProviderCloseResult
}

// BinaryProviderFrameCapable marks adapters that intentionally accept binary
// provider frames.
type BinaryProviderFrameCapable interface {
	SupportsBinaryProviderFrames() bool
}

type ProviderCloseKind string

const (
	ProviderCloseKindUnknown    ProviderCloseKind = ""
	ProviderCloseKindPeerClose  ProviderCloseKind = "peer_close"
	ProviderCloseKindReadError  ProviderCloseKind = "read_error"
	ProviderCloseKindWriteError ProviderCloseKind = "write_error"
	ProviderCloseKindNormal     ProviderCloseKind = "normal"
	ProviderCloseKindLocalAbort ProviderCloseKind = "local_abort"
	ProviderCloseKindLocalClose ProviderCloseKind = "local_close"
)

// ProviderCloseInfo is the transport close information passed to an adapter for
// provider-close evidence classification.
type ProviderCloseInfo struct {
	Kind   ProviderCloseKind
	Code   int
	Reason string
	Err    error
}

// ProviderCloseResult is the adapter's typed interpretation of an upstream
// close. It must not finalize relay actor accounting.
type ProviderCloseResult struct {
	ProviderClose *ProviderClose
	Err           error
	Origin        RecvDetailOrigin
}

// ProviderFrameResult is the adapter's typed interpretation of one provider
// frame, including any usage evidence or proxy-local transport error.
type ProviderFrameResult struct {
	EmitFrame      *Frame
	Usage          *types.UsageEvent
	Origin         RecvDetailOrigin
	Err            error
	CloseTransport bool
	Filtered       bool
}

func ValidateProviderFrameResult(result ProviderFrameResult) error {
	switch result.Origin {
	case RecvDetailOriginProviderFrame:
	case RecvDetailOriginProviderMalformed, RecvDetailOriginAdapterPanic:
		if result.Err == nil || !result.CloseTransport || result.EmitFrame != nil || result.Usage != nil || result.Filtered {
			return ErrInvalidProviderFrameResult
		}
		return nil
	default:
		return ErrInvalidProviderFrameResult
	}

	if result.Err != nil {
		return ErrInvalidProviderFrameResult
	}
	if result.CloseTransport {
		return ErrInvalidProviderFrameResult
	}
	if result.Filtered {
		if result.EmitFrame != nil || result.Usage != nil {
			return ErrInvalidProviderFrameResult
		}
		return nil
	}
	if result.EmitFrame == nil && result.Usage == nil {
		return ErrInvalidProviderFrameResult
	}
	return nil
}
