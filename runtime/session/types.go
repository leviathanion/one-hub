package session

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"one-api/types"
)

type TransportMode string

const (
	TransportModeRealtimeWS          TransportMode = "realtime-ws"
	TransportModeResponsesWS         TransportMode = "responses-ws"
	TransportModeResponsesHTTPBridge TransportMode = "responses-http-bridge"
)

const BindingScopeChatRealtime = "chat-realtime"

type SessionState string

const (
	SessionStateIdle   SessionState = "idle"
	SessionStateActive SessionState = "active"
	SessionStateClosed SessionState = "closed"
)

type Visibility string

const (
	VisibilityShared    Visibility = "shared"
	VisibilityLocalOnly Visibility = "local_only"
)

type PublishIntent string

const (
	PublishIntentNone           PublishIntent = "none"
	PublishIntentCreateIfAbsent PublishIntent = "create_if_absent"
	PublishIntentReplaceIfMatch PublishIntent = "replace_if_matches"
)

type ResolveStatus string

const (
	ResolveHit          ResolveStatus = "hit"
	ResolveMiss         ResolveStatus = "miss"
	ResolveBackendError ResolveStatus = "backend_error"
)

type RevocationStatus string

const (
	RevocationRevoked    RevocationStatus = "revoked"
	RevocationNotRevoked RevocationStatus = "not_revoked"
	RevocationUnknown    RevocationStatus = "unknown"
)

type BindingWriteStatus string

const (
	BindingWriteApplied           BindingWriteStatus = "applied"
	BindingWriteConditionMismatch BindingWriteStatus = "condition_mismatch"
	BindingWriteBackendError      BindingWriteStatus = "backend_error"
)

var ErrSessionClosed = errors.New("realtime session closed")

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

type Metadata struct {
	Key               string
	BindingKey        string
	SessionID         string
	ClientSuppliedID  bool
	CallerNS          string
	CapacityNS        string
	ChannelID         int
	CompatibilityHash string
	UpstreamIdentity  string
	Model             string
	Protocol          string
	IdleTTL           time.Duration
}

type RealtimeSession interface {
	SendClient(ctx context.Context, mt int, payload []byte) error
	// Recv returns the next supplier payload for the downstream websocket.
	// When both payload and err are non-empty/non-nil, payload is already the
	// authoritative wire frame to deliver first. Callers must not blindly turn
	// err into another websocket error frame; only ClientPayloadError carries an
	// additional client-visible payload that should be emitted after payload.
	Recv(ctx context.Context) (mt int, payload []byte, usage *types.UsageEvent, err error)
	// Detach releases the current downstream attachment without force-closing the
	// upstream provider transport. Implementations may continue draining and
	// finalizing turn state after Detach, and may explicitly stop queueing new
	// downstream frames during a bounded grace window once no consumer remains.
	Detach(reason string)
	// Abort force-closes the underlying provider transport and any remaining
	// realtime session work.
	Abort(reason string)
	SetTurnObserverFactory(factory TurnObserverFactory)
}

// GracefulDetachCapable lets sessions opt into downstream detach-on-close
// semantics when the client disconnects gracefully. Sessions that do not
// implement this interface, or return false, are aborted instead.
type GracefulDetachCapable interface {
	SupportsGracefulDetach() bool
}

type RealtimeOpenOptions struct {
	ClientSessionID           string
	ResolvedUpstreamSessionID string
	ForceFresh                bool
}

type TurnFinalizePayload struct {
	SessionID         string
	Model             string
	TurnSeq           int64
	LastResponseID    string
	TerminationReason string
	StartedAt         time.Time
	FirstResponseAt   time.Time
	CompletedAt       time.Time
	Usage             *types.UsageEvent
}

type TurnObserver interface {
	ObserveTurnUsage(usage *types.UsageEvent) error
	FinalizeTurn(payload TurnFinalizePayload)
}

type TurnObserverFunc func(payload TurnFinalizePayload)

func (f TurnObserverFunc) ObserveTurnUsage(usage *types.UsageEvent) error {
	_ = usage
	return nil
}

func (f TurnObserverFunc) FinalizeTurn(payload TurnFinalizePayload) {
	if f != nil {
		f(payload)
	}
}

type TurnObserverFactory func() TurnObserver

type guardedTurnObserver struct {
	mu        sync.Mutex
	observer  TurnObserver
	finalized bool
}

func GuardTurnObserver(observer TurnObserver) TurnObserver {
	if observer == nil {
		return nil
	}
	if _, ok := observer.(*guardedTurnObserver); ok {
		return observer
	}
	return &guardedTurnObserver{observer: observer}
}

func (o *guardedTurnObserver) ObserveTurnUsage(usage *types.UsageEvent) error {
	if o == nil || usage == nil {
		return nil
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finalized || o.observer == nil {
		return nil
	}
	return o.observer.ObserveTurnUsage(usage)
}

func (o *guardedTurnObserver) FinalizeTurn(payload TurnFinalizePayload) {
	if o == nil {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finalized || o.observer == nil {
		return
	}
	o.finalized = true
	o.observer.FinalizeTurn(payload)
}

type Binding struct {
	Key               string
	SessionKey        string
	Scope             string
	SessionID         string
	CallerNS          string
	ChannelID         int
	CompatibilityHash string
	UpdatedAt         time.Time
}

type ExecutionSession struct {
	mu sync.Mutex

	reservations atomic.Int32
	closed       atomic.Bool

	Key               string
	BindingKey        string
	SessionID         string
	ClientSuppliedID  bool
	CallerNS          string
	CapacityNS        string
	ChannelID         int
	CompatibilityHash string
	UpstreamIdentity  string
	Model             string
	Protocol          string
	IdleTTL           time.Duration

	Transport             TransportMode
	State                 SessionState
	LastResponseID        string
	FallbackUntil         time.Time
	LastUsedAt            time.Time
	Inflight              bool
	Attached              bool
	CloseReason           string
	Visibility            Visibility
	PublishIntent         PublishIntent
	SharedStateUncertain  bool
	ExpectedOldSessionKey string

	Data any
}

func NewExecutionSession(meta Metadata) *ExecutionSession {
	now := time.Now()
	return &ExecutionSession{
		Key:               meta.Key,
		BindingKey:        meta.BindingKey,
		SessionID:         meta.SessionID,
		ClientSuppliedID:  meta.ClientSuppliedID,
		CallerNS:          meta.CallerNS,
		CapacityNS:        meta.CapacityNS,
		ChannelID:         meta.ChannelID,
		CompatibilityHash: meta.CompatibilityHash,
		UpstreamIdentity:  meta.UpstreamIdentity,
		Model:             meta.Model,
		Protocol:          meta.Protocol,
		IdleTTL:           meta.IdleTTL,
		State:             SessionStateIdle,
		LastUsedAt:        now,
		Visibility:        VisibilityShared,
		PublishIntent:     PublishIntentNone,
	}
}

func (s *ExecutionSession) Lock() {
	s.mu.Lock()
}

func (s *ExecutionSession) TryLock() bool {
	return s.mu.TryLock()
}

func (s *ExecutionSession) Unlock() {
	s.mu.Unlock()
}

func (s *ExecutionSession) IsClosed() bool {
	if s == nil {
		return true
	}
	return s.closed.Load()
}

func (s *ExecutionSession) MarkClosed(reason string) {
	if s == nil {
		return
	}
	s.State = SessionStateClosed
	s.closed.Store(true)
	if trimmed := strings.TrimSpace(reason); trimmed != "" {
		s.CloseReason = trimmed
	}
}

func (s *ExecutionSession) Reopen() {
	if s == nil {
		return
	}
	s.closed.Store(false)
	s.CloseReason = ""
}

func (s *ExecutionSession) Touch(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	s.LastUsedAt = now
}

func (s *ExecutionSession) IsExpired(now time.Time, defaultTTL time.Duration) bool {
	if now.IsZero() {
		now = time.Now()
	}
	if s.reservations.Load() > 0 {
		return false
	}
	if s.IsClosed() {
		return true
	}
	ttl := s.IdleTTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if ttl <= 0 {
		return false
	}
	return !s.Attached && !s.Inflight && now.Sub(s.LastUsedAt) >= ttl
}

func (s *ExecutionSession) reserveLease() {
	if s == nil {
		return
	}
	s.reservations.Add(1)
}

func (s *ExecutionSession) releaseLease() {
	if s == nil {
		return
	}
	if remaining := s.reservations.Add(-1); remaining < 0 {
		s.reservations.Store(0)
	}
}

func (s *ExecutionSession) BuildBinding() *Binding {
	if s == nil || strings.TrimSpace(s.BindingKey) == "" || strings.TrimSpace(s.Key) == "" {
		return nil
	}

	scope := BindingScopeChatRealtime
	sessionID := ""
	if _, parsedScope, _, ok := parseBindingKey(s.BindingKey); ok && strings.TrimSpace(parsedScope) != "" {
		scope = parsedScope
	}
	if _, _, parsedSessionID, ok := parseBindingKey(s.BindingKey); ok {
		sessionID = parsedSessionID
	}

	return &Binding{
		Key:               s.BindingKey,
		SessionKey:        s.Key,
		Scope:             scope,
		SessionID:         sessionID,
		CallerNS:          s.CallerNS,
		ChannelID:         s.ChannelID,
		CompatibilityHash: s.CompatibilityHash,
		UpdatedAt:         time.Now(),
	}
}

func BuildBindingKey(callerNS, scope, sessionID string) string {
	return escapeBindingKeySegment(callerNS) + "/" + escapeBindingKeySegment(scope) + "/" + escapeBindingKeySegment(sessionID)
}

func parseBindingKey(bindingKey string) (callerNS, scope, sessionID string, ok bool) {
	parts := strings.SplitN(bindingKey, "/", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}

	callerNS, err := url.PathUnescape(parts[0])
	if err != nil {
		return "", "", "", false
	}
	scope, err = url.PathUnescape(parts[1])
	if err != nil {
		return "", "", "", false
	}
	sessionID, err = url.PathUnescape(parts[2])
	if err != nil {
		return "", "", "", false
	}
	return callerNS, scope, sessionID, true
}

func escapeBindingKeySegment(segment string) string {
	return url.PathEscape(segment)
}
