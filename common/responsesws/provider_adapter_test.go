package responsesws

import (
	"errors"
	"one-api/types"
	"testing"
)

func TestProviderFrameResultLegalCombinations(t *testing.T) {
	frame := NewTextFrame([]byte(`{"type":"response.created"}`))
	usage := &types.UsageEvent{TotalTokens: 1}
	baseErr := errors.New("bad provider frame")

	cases := []struct {
		name   string
		result ProviderFrameResult
	}{
		{
			name: "ordinary provider frame",
			result: ProviderFrameResult{
				EmitFrame: &frame,
				Origin:    RecvDetailOriginProviderFrame,
			},
		},
		{
			name: "ordinary provider frame with usage",
			result: ProviderFrameResult{
				EmitFrame: &frame,
				Usage:     usage,
				Origin:    RecvDetailOriginProviderFrame,
			},
		},
		{
			name: "bootstrap filter",
			result: ProviderFrameResult{
				Filtered: true,
				Origin:   RecvDetailOriginProviderFrame,
			},
		},
		{
			name: "usage only",
			result: ProviderFrameResult{
				Usage:  usage,
				Origin: RecvDetailOriginProviderFrame,
			},
		},
		{
			name: "provider malformed frame",
			result: ProviderFrameResult{
				Err:            baseErr,
				CloseTransport: true,
				Origin:         RecvDetailOriginProviderMalformed,
			},
		},
		{
			name: "adapter panic",
			result: ProviderFrameResult{
				Err:            baseErr,
				CloseTransport: true,
				Origin:         RecvDetailOriginAdapterPanic,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateProviderFrameResult(tc.result); err != nil {
				t.Fatalf("expected provider frame result to be legal, got %v", err)
			}
		})
	}
}

func TestProviderFrameResultRejectsIllegalCombinations(t *testing.T) {
	frame := NewTextFrame([]byte(`{"type":"response.created"}`))
	usage := &types.UsageEvent{TotalTokens: 1}
	baseErr := errors.New("bad provider frame")

	cases := []struct {
		name   string
		result ProviderFrameResult
	}{
		{
			name: "filtered with frame",
			result: ProviderFrameResult{
				EmitFrame: &frame,
				Filtered:  true,
				Origin:    RecvDetailOriginProviderFrame,
			},
		},
		{
			name: "filtered with usage",
			result: ProviderFrameResult{
				Usage:    usage,
				Filtered: true,
				Origin:   RecvDetailOriginProviderFrame,
			},
		},
		{
			name: "err with frame",
			result: ProviderFrameResult{
				EmitFrame:      &frame,
				Err:            baseErr,
				CloseTransport: true,
				Origin:         RecvDetailOriginProviderMalformed,
			},
		},
		{
			name: "err uses provider frame origin",
			result: ProviderFrameResult{
				Err:            baseErr,
				CloseTransport: true,
				Origin:         RecvDetailOriginProviderFrame,
			},
		},
		{
			name: "usage with proxy local origin",
			result: ProviderFrameResult{
				Usage:  usage,
				Origin: RecvDetailOriginProxyLocal,
			},
		},
		{
			name: "ordinary frame closes transport",
			result: ProviderFrameResult{
				EmitFrame:      &frame,
				CloseTransport: true,
				Origin:         RecvDetailOriginProviderFrame,
			},
		},
		{
			name: "empty provider frame result",
			result: ProviderFrameResult{
				Origin: RecvDetailOriginProviderFrame,
			},
		},
		{
			name: "unknown origin",
			result: ProviderFrameResult{
				EmitFrame: &frame,
				Origin:    RecvDetailOrigin("future_origin"),
			},
		},
		{
			name: "bridge stream origin is not native provider frame result",
			result: ProviderFrameResult{
				EmitFrame: &frame,
				Origin:    RecvDetailOriginProviderStream,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateProviderFrameResult(tc.result); !errors.Is(err, ErrInvalidProviderFrameResult) {
				t.Fatalf("expected invalid provider frame result error, got %v", err)
			}
		})
	}
}
