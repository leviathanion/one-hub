package responsesws

import (
	"errors"
	"testing"
)

func TestResponsesWSTransportSendResultLegality(t *testing.T) {
	baseErr := errors.New("write failed")
	cases := []struct {
		name    string
		result  ResponsesWSTransportSendResult
		wantErr bool
	}{
		{
			name:   "attempted without error",
			result: ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAttempted},
		},
		{
			name:    "attempted with error rejected",
			result:  ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAttempted, Err: baseErr},
			wantErr: true,
		},
		{
			name:   "rejected before stream without error",
			result: ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendRejectedBeforeStream},
		},
		{
			name:    "rejected before stream with error rejected",
			result:  ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendRejectedBeforeStream, Err: baseErr},
			wantErr: true,
		},
		{
			name:   "not attempted with error",
			result: ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted, Err: baseErr},
		},
		{
			name:    "not attempted without error or reason rejected",
			result:  ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendNotAttempted},
			wantErr: true,
		},
		{
			name: "not attempted with reason",
			result: ResponsesWSTransportSendResult{
				Status: ResponsesWSTransportSendNotAttempted,
				Reason: ResponsesWSTransportSendReasonNoActiveBridgeCancel,
			},
		},
		{
			name: "attempted with reason rejected",
			result: ResponsesWSTransportSendResult{
				Status: ResponsesWSTransportSendAttempted,
				Reason: ResponsesWSTransportSendReasonNoActiveBridgeCancel,
			},
			wantErr: true,
		},
		{
			name:   "ambiguous with error",
			result: ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAmbiguous, Err: baseErr},
		},
		{
			name:    "ambiguous without error rejected",
			result:  ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendAmbiguous},
			wantErr: true,
		},
		{
			name: "ambiguous with reason rejected",
			result: ResponsesWSTransportSendResult{
				Status: ResponsesWSTransportSendAmbiguous,
				Reason: ResponsesWSTransportSendReasonNoActiveBridgeCancel,
			},
			wantErr: true,
		},
		{
			name:    "unknown non-empty status rejected",
			result:  ResponsesWSTransportSendResult{Status: ResponsesWSTransportSendStatus("sent"), Err: baseErr},
			wantErr: true,
		},
		{
			name:    "empty status rejected",
			result:  ResponsesWSTransportSendResult{Err: baseErr},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateResponsesWSTransportSendResult(tc.result)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidResponsesWSTransportSendResult) {
					t.Fatalf("expected invalid send result error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected valid send result, got %v", err)
			}
		})
	}
}

func TestResponsesWSFramePayloadCopyAndLen(t *testing.T) {
	source := []byte("hello")
	frame := NewTextFrame(source)
	source[0] = 'x'

	if got := frame.PayloadLen(); got != 5 {
		t.Fatalf("PayloadLen = %d, want 5", got)
	}
	if got := string(frame.Payload()); got != "hello" {
		t.Fatalf("constructor must copy payload, got %q", got)
	}

	payload := frame.Payload()
	payload[1] = 'x'
	if got := string(frame.Payload()); got != "hello" {
		t.Fatalf("Payload must return a copy, got %q", got)
	}

	cloned := frame.ClonePayload()
	cloned[2] = 'x'
	if got := string(frame.ClonePayload()); got != "hello" {
		t.Fatalf("ClonePayload must return a copy, got %q", got)
	}

	if got := NewBinaryFrame([]byte{1, 2, 3}).PayloadLen(); got != 3 {
		t.Fatalf("binary PayloadLen = %d, want 3", got)
	}
	if got := (Frame{}).PayloadLen(); got != 0 {
		t.Fatalf("zero frame PayloadLen = %d, want 0", got)
	}
}

func TestRecvDetailOriginTerminalAllowlist(t *testing.T) {
	if !RecvDetailOriginKnown(RecvDetailOriginProviderFrame) || !RecvDetailOriginKnown(RecvDetailOriginNativeReadError) {
		t.Fatal("expected declared detail origins to be known")
	}
	if RecvDetailOriginKnown(RecvDetailOrigin("future_origin")) {
		t.Fatal("expected unknown non-empty detail origin to remain unknown")
	}
	if !RecvDetailOriginCanCarryProviderTerminal(RecvDetailOriginProviderFrame) ||
		!RecvDetailOriginCanCarryProviderTerminal(RecvDetailOriginProviderStream) {
		t.Fatal("expected provider frame and stream origins to be terminal-capable")
	}
	for _, origin := range []RecvDetailOrigin{
		RecvDetailOrigin("future_origin"),
		RecvDetailOriginNativeProviderClose,
		RecvDetailOriginProviderMalformed,
		RecvDetailOriginProxyLocal,
		RecvDetailOriginSyntheticBridge,
	} {
		if RecvDetailOriginCanCarryProviderTerminal(origin) {
			t.Fatalf("expected origin %q not to be terminal-capable", origin)
		}
	}
}

func TestRecvDetailOriginPayloadOriginMatrix(t *testing.T) {
	providerOrigins := []RecvDetailOrigin{
		RecvDetailOriginProviderFrame,
		RecvDetailOriginProviderStream,
		RecvDetailOriginBridgeStreamOpened,
		RecvDetailOriginBridgeOpenProviderError,
		RecvDetailOriginNativeProviderClose,
		RecvDetailOriginNativeProviderEOF,
	}
	for _, origin := range providerOrigins {
		got, ok := ExpectedPayloadOriginForRecvDetailOrigin(origin)
		if !ok || got != PayloadOriginProvider {
			t.Fatalf("expected detail origin %q to map to provider, got origin=%v ok=%v", origin, got, ok)
		}
		if PayloadOriginForDetailOrigin(origin) != PayloadOriginProvider {
			t.Fatalf("expected payload origin helper for %q to return provider", origin)
		}
	}

	proxyLocalOrigins := []RecvDetailOrigin{
		RecvDetailOriginProviderMalformed,
		RecvDetailOriginSyntheticBridge,
		RecvDetailOriginBridgeStreamError,
		RecvDetailOriginBridgeStreamEOF,
		RecvDetailOriginProxyLocal,
		RecvDetailOriginAdapterPanic,
		RecvDetailOriginNativeLocalAbort,
		RecvDetailOriginNativeLocalDetach,
		RecvDetailOriginNativeBackpressure,
		RecvDetailOriginNativeReadError,
	}
	for _, origin := range proxyLocalOrigins {
		got, ok := ExpectedPayloadOriginForRecvDetailOrigin(origin)
		if !ok || got != PayloadOriginProxyLocal {
			t.Fatalf("expected detail origin %q to map to proxy local, got origin=%v ok=%v", origin, got, ok)
		}
		if PayloadOriginForDetailOrigin(origin) != PayloadOriginProxyLocal {
			t.Fatalf("expected payload origin helper for %q to return proxy local", origin)
		}
	}

	if _, ok := ExpectedPayloadOriginForRecvDetailOrigin(RecvDetailOrigin("future_origin")); ok {
		t.Fatal("expected unknown detail origin not to have a payload origin mapping")
	}
	if PayloadOriginForDetailOrigin(RecvDetailOrigin("future_origin")) != PayloadOriginProxyLocal {
		t.Fatal("expected unknown detail origin helper fallback to proxy local")
	}
}
