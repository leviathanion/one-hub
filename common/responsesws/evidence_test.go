package responsesws

import (
	"one-api/types"
	"testing"
)

func TestRecvEventProviderEvidenceAllowlist(t *testing.T) {
	terminalFrame := NewTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`))
	binaryTerminalFrame := NewBinaryFrame([]byte(`{"type":"response.completed","response":{"id":"resp_binary","status":"completed"}}`))
	nonTerminalFrame := NewTextFrame([]byte(`{"type":"response.created","response":{"id":"resp_1"}}`))
	usage := &types.UsageEvent{TotalTokens: 1}
	cases := []struct {
		name             string
		event            UpstreamEvent
		wantActivity     bool
		wantTerminal     bool
		wantUsage        bool
		wantProxyLocal   bool
		wantLastActivity RecvDetailOrigin
	}{
		{
			name: "provider frame terminal",
			event: UpstreamEvent{
				Frame:        &terminalFrame,
				DetailOrigin: RecvDetailOriginProviderFrame,
			},
			wantActivity:     true,
			wantTerminal:     true,
			wantLastActivity: RecvDetailOriginProviderFrame,
		},
		{
			name: "provider stream terminal",
			event: UpstreamEvent{
				Frame:        &terminalFrame,
				DetailOrigin: RecvDetailOriginProviderStream,
			},
			wantActivity:     true,
			wantTerminal:     true,
			wantLastActivity: RecvDetailOriginProviderStream,
		},
		{
			name: "provider frame non terminal usage",
			event: UpstreamEvent{
				Frame:        &nonTerminalFrame,
				Usage:        usage,
				DetailOrigin: RecvDetailOriginProviderFrame,
			},
			wantActivity:     true,
			wantUsage:        true,
			wantLastActivity: RecvDetailOriginProviderFrame,
		},
		{
			name: "provider binary frame is never terminal evidence",
			event: UpstreamEvent{
				Frame:        &binaryTerminalFrame,
				DetailOrigin: RecvDetailOriginProviderFrame,
			},
			wantActivity:     true,
			wantLastActivity: RecvDetailOriginProviderFrame,
		},
		{
			name: "bridge stream opened evidence only",
			event: UpstreamEvent{
				DetailOrigin: RecvDetailOriginBridgeStreamOpened,
			},
			wantActivity:     true,
			wantLastActivity: RecvDetailOriginBridgeStreamOpened,
		},
		{
			name: "bridge open provider error is not provider activity",
			event: UpstreamEvent{
				DetailOrigin: RecvDetailOriginBridgeOpenProviderError,
			},
		},
		{
			name: "native provider close evidence only",
			event: UpstreamEvent{
				ProviderClose: &ProviderClose{Code: 1000},
				DetailOrigin:  RecvDetailOriginNativeProviderClose,
			},
			wantActivity:     true,
			wantLastActivity: RecvDetailOriginNativeProviderClose,
		},
		{
			name: "provider malformed is upstream activity but proxy local terminal",
			event: UpstreamEvent{
				DetailOrigin: RecvDetailOriginProviderMalformed,
			},
			wantActivity:     true,
			wantProxyLocal:   true,
			wantLastActivity: RecvDetailOriginProviderMalformed,
		},
		{
			name: "adapter panic during provider frame records activity",
			event: UpstreamEvent{
				DetailOrigin: RecvDetailOriginAdapterPanic,
				DetailPhase:  RecvDetailPhaseHandleProviderFrame,
			},
			wantActivity:     true,
			wantProxyLocal:   true,
			wantLastActivity: RecvDetailOriginAdapterPanic,
		},
		{
			name: "adapter panic outside provider frame is proxy local only",
			event: UpstreamEvent{
				DetailOrigin: RecvDetailOriginAdapterPanic,
				DetailPhase:  RecvDetailPhasePrepareClientFrame,
			},
			wantProxyLocal: true,
		},
		{
			name: "native provider eof alone is not request activity",
			event: UpstreamEvent{
				DetailOrigin: RecvDetailOriginNativeProviderEOF,
			},
		},
		{
			name: "bridge stream error is proxy local only",
			event: UpstreamEvent{
				DetailOrigin: RecvDetailOriginBridgeStreamError,
			},
			wantProxyLocal: true,
		},
		{
			name: "synthetic bridge is proxy local only",
			event: UpstreamEvent{
				Frame:        &terminalFrame,
				DetailOrigin: RecvDetailOriginSyntheticBridge,
			},
			wantProxyLocal: true,
		},
		{
			name: "native backpressure is proxy local only",
			event: UpstreamEvent{
				DetailOrigin: RecvDetailOriginNativeBackpressure,
			},
			wantProxyLocal: true,
		},
		{
			name: "unknown explicit detail origin is not provider terminal evidence",
			event: UpstreamEvent{
				Frame:        &terminalFrame,
				DetailOrigin: RecvDetailOrigin("future_origin"),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpstreamEventHasProviderEvidence(tc.event); got != tc.wantActivity {
				t.Fatalf("provider activity mismatch: got %v want %v", got, tc.wantActivity)
			}
			if got := UpstreamEventHasProviderTerminalEvidence(tc.event); got != tc.wantTerminal {
				t.Fatalf("provider terminal mismatch: got %v want %v", got, tc.wantTerminal)
			}
			if got := UpstreamEventIsProxyLocalTerminal(tc.event); got != tc.wantProxyLocal {
				t.Fatalf("proxy-local terminal mismatch: got %v want %v", got, tc.wantProxyLocal)
			}
			var projected ProviderSettlementLogProjection
			projected.Observe(NewProviderObservation(tc.event))
			if projected.HasActivity() != tc.wantActivity {
				t.Fatalf("facts activity mismatch: got %v want %v", projected.HasActivity(), tc.wantActivity)
			}
			if projected.Activity.ProviderUsageSeen != tc.wantUsage {
				t.Fatalf("facts usage mismatch: got %v want %v", projected.Activity.ProviderUsageSeen, tc.wantUsage)
			}
			if projected.LastActivityOrigin() != tc.wantLastActivity {
				t.Fatalf("facts last activity origin mismatch: got %q want %q", projected.LastActivityOrigin(), tc.wantLastActivity)
			}
		})
	}
}

func TestProjectProviderObservationKnownOriginMatrix(t *testing.T) {
	tests := []struct {
		origin        RecvDetailOrigin
		phase         RecvDetailPhase
		wantActivity  ProviderActivityFact
		wantCandidate ZeroChargeProofCandidate
	}{
		{
			origin:       RecvDetailOriginProviderFrame,
			wantActivity: ProviderActivityFact{ProviderFrameSeen: true},
		},
		{
			origin:       RecvDetailOriginProviderStream,
			wantActivity: ProviderActivityFact{ProviderFrameSeen: true},
		},
		{
			origin:       RecvDetailOriginProviderMalformed,
			wantActivity: ProviderActivityFact{ProviderFrameSeen: true},
		},
		{origin: RecvDetailOriginProxyLocal},
		{origin: RecvDetailOriginAdapterPanic},
		{
			origin:       RecvDetailOriginAdapterPanic,
			phase:        RecvDetailPhaseHandleProviderFrame,
			wantActivity: ProviderActivityFact{ProviderFrameSeen: true},
		},
		{origin: RecvDetailOriginSyntheticBridge},
		{
			origin:       RecvDetailOriginBridgeStreamOpened,
			wantActivity: ProviderActivityFact{ProviderStreamOpened: true},
		},
		{
			origin:        RecvDetailOriginBridgeOpenProviderError,
			wantCandidate: ZeroChargeProofCandidateProviderRejectedBeforeStream,
		},
		{origin: RecvDetailOriginBridgeStreamError},
		{origin: RecvDetailOriginBridgeStreamEOF},
		{
			origin:       RecvDetailOriginNativeProviderClose,
			wantActivity: ProviderActivityFact{ProviderPeerCloseSeen: true},
		},
		{origin: RecvDetailOriginNativeProviderEOF},
		{origin: RecvDetailOriginNativeLocalAbort},
		{origin: RecvDetailOriginNativeLocalDetach},
		{origin: RecvDetailOriginNativeBackpressure},
		{origin: RecvDetailOriginNativeReadError},
	}

	seenKnown := map[RecvDetailOrigin]bool{}
	for _, tc := range tests {
		t.Run(string(tc.origin)+"/"+string(tc.phase), func(t *testing.T) {
			if !RecvDetailOriginKnown(tc.origin) {
				t.Fatalf("test case uses unknown origin %q", tc.origin)
			}
			seenKnown[tc.origin] = true
			got := ProjectProviderObservationForSettlement(ProviderObservation{
				DetailOrigin: tc.origin,
				DetailPhase:  tc.phase,
			})
			if got.Activity != tc.wantActivity {
				t.Fatalf("activity mismatch: got %+v want %+v", got.Activity, tc.wantActivity)
			}
			if got.Diagnostic.DetailOrigin != tc.origin || got.Diagnostic.DetailPhase != tc.phase {
				t.Fatalf("diagnostic mismatch: got %+v", got.Diagnostic)
			}
			if got.ZeroChargeProofCandidate != tc.wantCandidate {
				t.Fatalf("zero proof candidate mismatch: got %v want %v", got.ZeroChargeProofCandidate, tc.wantCandidate)
			}
		})
	}
	for _, origin := range []RecvDetailOrigin{
		RecvDetailOriginProviderFrame,
		RecvDetailOriginProviderStream,
		RecvDetailOriginProviderMalformed,
		RecvDetailOriginProxyLocal,
		RecvDetailOriginAdapterPanic,
		RecvDetailOriginSyntheticBridge,
		RecvDetailOriginBridgeStreamOpened,
		RecvDetailOriginBridgeOpenProviderError,
		RecvDetailOriginBridgeStreamError,
		RecvDetailOriginBridgeStreamEOF,
		RecvDetailOriginNativeProviderClose,
		RecvDetailOriginNativeProviderEOF,
		RecvDetailOriginNativeLocalAbort,
		RecvDetailOriginNativeLocalDetach,
		RecvDetailOriginNativeBackpressure,
		RecvDetailOriginNativeReadError,
	} {
		if !seenKnown[origin] {
			t.Fatalf("missing known origin projection test for %q", origin)
		}
	}
}

func TestProjectProviderObservationUsageRequiresActivityOrigin(t *testing.T) {
	providerUsage := ProjectProviderObservationForSettlement(ProviderObservation{
		DetailOrigin: RecvDetailOriginProviderFrame,
		HasUsage:     true,
	})
	if !providerUsage.Activity.ProviderUsageSeen {
		t.Fatal("expected provider usage on provider origin to set usage activity")
	}
	proxyUsage := ProjectProviderObservationForSettlement(ProviderObservation{
		DetailOrigin: RecvDetailOriginProxyLocal,
		HasUsage:     true,
	})
	if proxyUsage.Activity.ProviderUsageSeen || proxyUsage.Activity.HasActivity() {
		t.Fatalf("expected invalid-origin usage not to become provider activity, got %+v", proxyUsage.Activity)
	}
}

func TestRecvEventTerminalEvidenceRejectsMalformedPayload(t *testing.T) {
	frame := NewTextFrame([]byte(`{`))
	event := UpstreamEvent{
		Frame:        &frame,
		DetailOrigin: RecvDetailOriginProviderFrame,
	}
	if !UpstreamEventHasProviderEvidence(event) {
		t.Fatal("expected malformed provider frame to remain provider activity")
	}
	if UpstreamEventHasProviderTerminalEvidence(event) {
		t.Fatal("expected malformed provider frame not to become provider terminal evidence")
	}
}
