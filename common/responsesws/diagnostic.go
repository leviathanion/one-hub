package responsesws

import (
	"crypto/sha256"
	"fmt"
	"runtime"
)

// DiagnosticHook receives sanitized transport diagnostics from ResponsesWS
// native and bridge adapters.
type DiagnosticHook func(Diagnostic)

// Diagnostic is safe metadata for adapter panic or transport diagnostics.
type Diagnostic struct {
	Code        string
	Provider    string
	ChannelID   int
	Transport   string
	Phase       RecvDetailPhase
	StackHash   string
	PanicClass  string
	DetailError string
}

// NativeDiagnostic is the native websocket diagnostic shape.
type NativeDiagnostic = Diagnostic

// NativeDiagnosticHook receives native websocket diagnostics.
type NativeDiagnosticHook = DiagnosticHook

// BridgeDiagnostic is the HTTP bridge diagnostic shape.
type BridgeDiagnostic = Diagnostic

// BridgeDiagnosticHook receives HTTP bridge diagnostics.
type BridgeDiagnosticHook = DiagnosticHook

func panicClass(recovered any) string {
	if recovered == nil {
		return ""
	}
	if _, ok := recovered.(runtime.Error); ok {
		return "runtime_error"
	}
	if _, ok := recovered.(error); ok {
		return "error"
	}
	if _, ok := recovered.(string); ok {
		return "string"
	}
	return "other"
}

func diagnosticStackHash(stack []byte) string {
	sum := sha256.Sum256(stack)
	return fmt.Sprintf("%x", sum[:8])
}
