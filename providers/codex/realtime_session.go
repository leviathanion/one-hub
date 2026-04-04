package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"one-api/common"
	"one-api/common/authutil"
	commonredis "one-api/common/redis"
	"one-api/common/requester"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const codexRealtimeProtocolName = "codex-responses-ws"
const codexRealtimeReadTimeout = 2 * time.Minute
const codexRealtimeSessionIDMaxLen = 128
const codexRealtimeGeneratedSessionIDContextKey = "codex_generated_execution_session_id"

var codexRealtimeOutboundBackpressureTimeout = 5 * time.Second

type codexRealtimeClientEvent struct {
	Type       string                        `json:"type"`
	EventID    string                        `json:"event_id,omitempty"`
	ResponseID string                        `json:"response_id,omitempty"`
	Response   *types.OpenAIResponsesRequest `json:"response,omitempty"`
}

type codexRealtimeOutbound struct {
	messageType int
	payload     []byte
	usage       *types.UsageEvent
	err         error
}

const codexRealtimeAttachmentQueueCapacity = 64

// codexAttachment is a bounded outbound mailbox that preserves queued frames
// during shutdown while rejecting any enqueue that races with close.
type codexAttachment struct {
	mu     sync.Mutex
	waitCh chan struct{}
	queue  []codexRealtimeOutbound
	head   int
	size   int
	closed bool
}

type codexManagedRuntimeState struct {
	attachment            *codexAttachment
	ownerSeq              uint64
	wsConn                *websocket.Conn
	wsConnGeneration      uint64
	wsReaderConn          *websocket.Conn
	wsWriteMu             sync.Mutex
	bridgeStream          requester.StreamReaderInterface[string]
	skipBootstrapConn     *websocket.Conn
	turnSeq               int64
	turnStartedAt         time.Time
	turnFirstResponseAt   time.Time
	turnCompletedAt       time.Time
	turnLastResponseID    string
	turnTerminationReason string
	turnUsage             *types.UsageEvent
	turnAccumulator       *codexTurnUsageAccumulator
	turnFinalized         bool
	turnObserver          runtimesession.TurnObserver
	turnObserverFactory   runtimesession.TurnObserverFactory
}

type codexManagedRealtimeSession struct {
	provider   *CodexProvider
	exec       *runtimesession.ExecutionSession
	attachment *codexAttachment
	ownerSeq   uint64
	detachOnce sync.Once
	abortOnce  sync.Once
}

type codexRealtimeHandshakePolicy struct {
	EffectiveUserAgent string            `json:"effective_user_agent,omitempty"`
	EffectiveHeaders   map[string]string `json:"effective_headers,omitempty"`
}

var codexExecutionSessions = runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
	DefaultTTL:           defaultExecutionSessionTTL,
	JanitorInterval:      time.Minute,
	Cleanup:              cleanupCodexExecutionSession,
	MaxSessions:          defaultExecutionSessionCap,
	MaxSessionsPerCaller: defaultExecutionSessionCallerCap,
	RedisClient:          commonredis.GetRedisClient(),
	RedisPrefix:          "one-hub:execution-session",
})

type codexRealtimeOpenPlan struct {
	candidateSessionKey   string
	sharedHitCompatible   bool
	publishIntent         runtimesession.PublishIntent
	expectedOldSessionKey string
}

func codexMarkExecutionSessionSharedLocked(exec *runtimesession.ExecutionSession) {
	if exec == nil {
		return
	}
	exec.Visibility = runtimesession.VisibilityShared
	exec.PublishIntent = runtimesession.PublishIntentNone
	exec.ExpectedOldSessionKey = ""
	exec.SharedStateUncertain = false
}

func codexMarkExecutionSessionLocalOnlyLocked(exec *runtimesession.ExecutionSession, intent runtimesession.PublishIntent, expectedOldSessionKey string) {
	if exec == nil {
		return
	}
	exec.Visibility = runtimesession.VisibilityLocalOnly
	exec.PublishIntent = intent
	exec.ExpectedOldSessionKey = strings.TrimSpace(expectedOldSessionKey)
}

func codexStopExecutionSessionRepublishLocked(exec *runtimesession.ExecutionSession) {
	if exec == nil {
		return
	}
	exec.PublishIntent = runtimesession.PublishIntentNone
	exec.ExpectedOldSessionKey = ""
}

func codexMaybePromoteExecutionSession(exec *runtimesession.ExecutionSession) {
	if exec == nil {
		return
	}

	exec.Lock()
	if exec.Visibility != runtimesession.VisibilityLocalOnly || strings.TrimSpace(exec.BindingKey) == "" {
		exec.Unlock()
		return
	}

	bindingKey := exec.BindingKey
	sessionKey := exec.Key
	intent := exec.PublishIntent
	expectedOldSessionKey := exec.ExpectedOldSessionKey
	ttl := exec.IdleTTL
	binding := exec.BuildBinding()
	exec.Unlock()

	if binding == nil {
		return
	}

	sharedBinding, status := codexExecutionSessions.ResolveBinding(bindingKey)
	switch status {
	case runtimesession.ResolveBackendError:
		return
	case runtimesession.ResolveHit:
		if sharedBinding != nil && sharedBinding.SessionKey == sessionKey {
			exec.Lock()
			codexMarkExecutionSessionSharedLocked(exec)
			exec.Unlock()
			return
		}
	case runtimesession.ResolveMiss:
	}

	switch intent {
	case runtimesession.PublishIntentCreateIfAbsent:
		if status != runtimesession.ResolveMiss {
			exec.Lock()
			codexStopExecutionSessionRepublishLocked(exec)
			exec.Unlock()
			return
		}
		switch codexExecutionSessions.CreateBindingIfAbsent(binding, ttl) {
		case runtimesession.BindingWriteApplied:
			exec.Lock()
			codexMarkExecutionSessionSharedLocked(exec)
			exec.Unlock()
		case runtimesession.BindingWriteConditionMismatch:
			exec.Lock()
			codexStopExecutionSessionRepublishLocked(exec)
			exec.Unlock()
		}
	case runtimesession.PublishIntentReplaceIfMatch:
		if status != runtimesession.ResolveHit || sharedBinding == nil || sharedBinding.SessionKey != expectedOldSessionKey {
			exec.Lock()
			codexStopExecutionSessionRepublishLocked(exec)
			exec.Unlock()
			return
		}
		switch codexExecutionSessions.ReplaceBindingIfSessionMatches(bindingKey, expectedOldSessionKey, binding, ttl) {
		case runtimesession.BindingWriteApplied:
			exec.Lock()
			codexMarkExecutionSessionSharedLocked(exec)
			exec.Unlock()
		case runtimesession.BindingWriteConditionMismatch:
			exec.Lock()
			codexStopExecutionSessionRepublishLocked(exec)
			exec.Unlock()
		}
	}
}

func codexAcquireLocalOnlyExecutionSession(bindingKey, excludedSessionKey string) (*runtimesession.ExecutionSession, func(), bool) {
	if strings.TrimSpace(bindingKey) == "" {
		return nil, nil, false
	}

	binding, ok := codexExecutionSessions.ResolveLocal(bindingKey)
	if !ok || binding == nil || binding.SessionKey == strings.TrimSpace(excludedSessionKey) {
		return nil, nil, false
	}

	exec, releaseLease, ok := codexExecutionSessions.AcquireExisting(binding.SessionKey)
	if !ok {
		return nil, nil, false
	}

	exec.Lock()
	if exec.IsClosed() || exec.Visibility != runtimesession.VisibilityLocalOnly {
		exec.Unlock()
		releaseLease()
		return nil, nil, false
	}
	exec.Unlock()
	return exec, releaseLease, true
}

func (p *CodexProvider) planRealtimeOpen(meta runtimesession.Metadata, options runtimesession.RealtimeOpenOptions) codexRealtimeOpenPlan {
	plan := codexRealtimeOpenPlan{publishIntent: runtimesession.PublishIntentNone}
	if options.ForceFresh || strings.TrimSpace(meta.BindingKey) == "" {
		return plan
	}

	binding, status := codexExecutionSessions.ResolveBinding(meta.BindingKey)
	switch status {
	case runtimesession.ResolveMiss:
		plan.publishIntent = runtimesession.PublishIntentCreateIfAbsent
	case runtimesession.ResolveBackendError:
		return plan
	case runtimesession.ResolveHit:
		if binding == nil || strings.TrimSpace(binding.SessionKey) == "" {
			return plan
		}
		if binding.ChannelID != meta.ChannelID || binding.CompatibilityHash != meta.CompatibilityHash {
			plan.publishIntent = runtimesession.PublishIntentReplaceIfMatch
			plan.expectedOldSessionKey = binding.SessionKey
			return plan
		}

		switch codexExecutionSessions.CheckRevocation(binding.SessionKey) {
		case runtimesession.RevocationNotRevoked:
			plan.candidateSessionKey = binding.SessionKey
			plan.sharedHitCompatible = true
		case runtimesession.RevocationRevoked:
			plan.publishIntent = runtimesession.PublishIntentReplaceIfMatch
			plan.expectedOldSessionKey = binding.SessionKey
		}
	}
	return plan
}

func (p *CodexProvider) planForceFreshRealtimeOpen(meta runtimesession.Metadata) codexRealtimeOpenPlan {
	plan := codexRealtimeOpenPlan{publishIntent: runtimesession.PublishIntentNone}
	if strings.TrimSpace(meta.BindingKey) == "" {
		return plan
	}

	binding, status := codexExecutionSessions.ResolveBinding(meta.BindingKey)
	switch status {
	case runtimesession.ResolveMiss:
		plan.publishIntent = runtimesession.PublishIntentCreateIfAbsent
	case runtimesession.ResolveBackendError:
		return plan
	case runtimesession.ResolveHit:
		if binding == nil || strings.TrimSpace(binding.SessionKey) == "" {
			return plan
		}
		if localExec, releaseLease, ok := codexExecutionSessions.AcquireExisting(binding.SessionKey); ok {
			localExec.Lock()
			localExec.MarkClosed("force_fresh_replaced")
			localExec.Unlock()
			releaseLease()
			if localExec.BindingKey == meta.BindingKey {
				codexExecutionSessions.DeleteBinding(meta.BindingKey)
			}
			codexExecutionSessions.DeleteIf(localExec.Key, localExec)
		}
		if codexExecutionSessions.DeleteBindingAndRevokeIfSessionMatches(meta.BindingKey, binding.SessionKey, codexExecutionSessions.RevocationTTLForSession(binding.SessionKey)) == runtimesession.BindingWriteApplied {
			plan.publishIntent = runtimesession.PublishIntentCreateIfAbsent
		}
	}
	return plan
}

func (p *CodexProvider) OpenRealtimeSession(modelName string) (runtimesession.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	return p.OpenRealtimeSessionWithOptions(modelName, runtimesession.RealtimeOpenOptions{})
}

func ExecutionSessionStats() runtimesession.Stats {
	codexExecutionSessions.ConfigureRedis(commonredis.GetRedisClient(), "one-hub:execution-session")
	return codexExecutionSessions.Stats()
}

func (p *CodexProvider) OpenRealtimeSessionWithOptions(modelName string, options runtimesession.RealtimeOpenOptions) (runtimesession.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	codexExecutionSessions.ConfigureRedis(commonredis.GetRedisClient(), "one-hub:execution-session")
	normalizedModelName := normalizeCodexModelName(modelName)
	meta, errWithCode := p.buildExecutionSessionMetadata(normalizedModelName, options)
	if errWithCode != nil {
		return nil, errWithCode
	}
	plan := p.planRealtimeOpen(meta, options)
	if options.ForceFresh {
		plan = p.planForceFreshRealtimeOpen(meta)
	}

	for attempt := 0; attempt < 3; attempt++ {
		var (
			exec         *runtimesession.ExecutionSession
			created      bool
			releaseLease func()
			err          error
			ok           bool
		)

		if plan.candidateSessionKey != "" {
			exec, releaseLease, ok = codexExecutionSessions.AcquireExisting(plan.candidateSessionKey)
			if ok {
				created = false
			}
		}
		if !options.ForceFresh && exec == nil && strings.TrimSpace(meta.BindingKey) != "" {
			exec, releaseLease, ok = codexAcquireLocalOnlyExecutionSession(meta.BindingKey, plan.candidateSessionKey)
			if ok {
				created = false
			}
		}
		if exec == nil {
			exec, created, releaseLease, err = codexExecutionSessions.AcquireOrCreate(meta)
			if err != nil {
				return nil, codexRealtimeManagerError(err)
			}
		}

		exec.Lock()
		if exec.IsClosed() {
			releaseLease()
			exec.Unlock()
			continue
		}

		attachment := newCodexAttachment()
		var staleAttachment *codexAttachment
		var staleOwnerSeq uint64
		var staleTurnObserver runtimesession.TurnObserver
		var staleTurnObserverFactory runtimesession.TurnObserverFactory
		var wasAttached bool

		sessionModel := normalizeCodexModelName(exec.Model)
		if exec.Model != "" && sessionModel != normalizedModelName {
			releaseLease()
			exec.Unlock()
			attachment.close()
			return nil, common.StringErrorWrapperLocal("execution session model mismatch", "session_model_mismatch", http.StatusConflict)
		}

		exec.Model = normalizedModelName
		exec.Protocol = codexRealtimeProtocolName
		exec.IdleTTL = p.getExecutionSessionTTL()
		if exec.BindingKey == "" {
			exec.BindingKey = meta.BindingKey
		}
		if exec.CompatibilityHash == "" {
			exec.CompatibilityHash = meta.CompatibilityHash
		}
		exec.ChannelID = meta.ChannelID
		exec.Touch(time.Now())
		exec.Reopen()
		if created {
			codexMarkExecutionSessionLocalOnlyLocked(exec, plan.publishIntent, plan.expectedOldSessionKey)
			if strings.TrimSpace(exec.BindingKey) == "" {
				codexMarkExecutionSessionSharedLocked(exec)
			}
		}

		state := getCodexManagedRuntimeStateLocked(exec)
		wasAttached = exec.Attached
		staleAttachment = state.attachment
		staleOwnerSeq = state.ownerSeq
		staleTurnObserver = state.turnObserver
		staleTurnObserverFactory = state.turnObserverFactory
		state.ownerSeq++
		state.attachment = attachment
		state.turnObserverFactory = nil
		if !exec.Inflight {
			state.turnObserver = nil
		}
		exec.Attached = true

		if errWithCode := p.ensureRealtimeTransportLocked(exec, state, time.Now()); errWithCode != nil {
			state.attachment = staleAttachment
			state.ownerSeq = staleOwnerSeq
			state.turnObserver = codexGuardTurnObserver(staleTurnObserver)
			state.turnObserverFactory = staleTurnObserverFactory
			exec.Attached = wasAttached && staleAttachment != nil

			shouldDeleteFreshSession := created && !wasAttached && staleAttachment == nil
			if shouldDeleteFreshSession {
				exec.Touch(time.Now())
				exec.MarkClosed("open_failed")
			}
			attachment.close()

			releaseLease()
			exec.Unlock()
			if shouldDeleteFreshSession {
				codexExecutionSessions.DeleteIf(meta.Key, exec)
			}
			return nil, errWithCode
		}
		if staleAttachment != nil && staleAttachment != attachment {
			staleAttachment.close()
		}
		if exec.Visibility == runtimesession.VisibilityLocalOnly {
			exec.Unlock()
			codexMaybePromoteExecutionSession(exec)
			exec.Lock()
		}
		releaseLease()
		exec.Unlock()
		if exec.Visibility == runtimesession.VisibilityShared {
			codexExecutionSessions.TouchBinding(exec.BindingKey, exec.Key, meta.ChannelID)
		}

		return &codexManagedRealtimeSession{
			provider:   p,
			exec:       exec,
			attachment: attachment,
			ownerSeq:   state.ownerSeq,
		}, nil
	}

	return nil, common.StringErrorWrapperLocal("execution session is closed during open", "session_closed", http.StatusConflict)
}

func (s *codexManagedRealtimeSession) SendClient(ctx context.Context, mt int, payload []byte) error {
	if s == nil || s.exec == nil || s.provider == nil {
		return runtimesession.ErrSessionClosed
	}
	if mt != websocket.TextMessage {
		return newCodexRealtimeClientError("", "unsupported_client_event", "only text websocket events are supported")
	}

	var event codexRealtimeClientEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return newCodexRealtimeClientError("", "invalid_event", err.Error())
	}

	s.exec.Lock()
	state := getCodexManagedRuntimeStateLocked(s.exec)
	if !codexManagedSessionOwnsAttachmentLocked(state, s.ownerSeq, s.attachment) {
		s.exec.Unlock()
		return runtimesession.ErrSessionClosed
	}

	switch strings.TrimSpace(event.Type) {
	case "response.create":
		if event.Response == nil {
			s.exec.Unlock()
			return newCodexRealtimeClientError(event.EventID, "invalid_event", "response payload is required")
		}
		if s.exec.Inflight {
			s.exec.Unlock()
			return newCodexRealtimeClientError(event.EventID, "session_busy", "execution session already has an inflight response")
		}

		request, err := cloneCodexResponsesRequest(event.Response)
		if err != nil {
			s.exec.Unlock()
			return newCodexRealtimeClientError(event.EventID, "invalid_event", err.Error())
		}
		sessionModel := normalizeCodexModelName(s.exec.Model)
		if s.exec.Model != sessionModel {
			s.exec.Model = sessionModel
		}
		if request.Model == "" {
			request.Model = sessionModel
		}
		request.Model = normalizeCodexModelName(request.Model)
		if request.Model != sessionModel {
			s.exec.Unlock()
			return newCodexRealtimeClientError(event.EventID, "session_model_mismatch", "execution session model mismatch")
		}
		s.provider.prepareCodexRequest(request)
		event.Response = request

		s.exec.LastResponseID = ""
		s.exec.Inflight = true
		s.exec.State = runtimesession.SessionStateActive
		now := time.Now()
		s.exec.Touch(now)

		if err := s.provider.ensureRealtimeTransportLocked(s.exec, state, time.Now()); err != nil {
			s.exec.Inflight = false
			s.exec.State = runtimesession.SessionStateIdle
			resetCodexTurnLocked(state)
			s.exec.Unlock()
			return codexRealtimeErrorFromOpenAIError(event.EventID, err)
		}

		encodedPayload, err := json.Marshal(event)
		if err != nil {
			s.exec.Inflight = false
			s.exec.State = runtimesession.SessionStateIdle
			resetCodexTurnLocked(state)
			s.exec.Unlock()
			return newCodexRealtimeClientError(event.EventID, "invalid_event", err.Error())
		}

		beginCodexTurnLocked(state, now)
		if state.turnAccumulator != nil {
			state.turnAccumulator.SeedPromptFromRequest(request, s.provider.Channel.PreCost)
		}

		switch s.exec.Transport {
		case runtimesession.TransportModeResponsesWS:
			if err := s.provider.sendRealtimeWSEventLocked(ctx, s.exec, state, encodedPayload, event.EventID, request); err != nil {
				resetCodexTurnLocked(state)
				s.exec.Unlock()
				return err
			}
			s.exec.Unlock()
		case runtimesession.TransportModeResponsesHTTPBridge:
			if err := s.provider.startRealtimeHTTPBridgeLocked(s.exec, state, request, event.EventID); err != nil {
				resetCodexTurnLocked(state)
				s.exec.Unlock()
				return err
			}
			s.exec.Unlock()
		default:
			s.exec.Inflight = false
			s.exec.State = runtimesession.SessionStateIdle
			resetCodexTurnLocked(state)
			s.exec.Unlock()
			return newCodexRealtimeProviderError(event.EventID, "transport_unavailable", "no realtime transport available")
		}
		return nil

	case "response.cancel":
		s.exec.Touch(time.Now())
		switch s.exec.Transport {
		case runtimesession.TransportModeResponsesHTTPBridge:
			stream := state.bridgeStream
			attachment := state.attachment
			responseID := event.ResponseID
			if strings.TrimSpace(responseID) == "" {
				responseID = s.exec.LastResponseID
			}
			wasInflight := s.exec.Inflight
			state.bridgeStream = nil
			s.exec.Inflight = false
			s.exec.State = runtimesession.SessionStateIdle
			finalizer, finalizePayload := finalizeCodexTurnLocked(s.exec, state, "response.cancelled", time.Now())
			s.exec.Unlock()
			if finalizer != nil {
				finalizer.FinalizeTurn(finalizePayload)
			}
			if wasInflight && attachment != nil {
				_ = enqueueCodexOutbound(attachment, codexRealtimeOutbound{
					messageType: websocket.TextMessage,
					payload:     buildCodexRealtimeCancelledPayload(responseID),
				})
			}
			if stream != nil {
				stream.Close()
			}
			codexMaybeDeleteDetachedExecutionSession(s.exec, "detached_ephemeral_session")
			return nil
		case runtimesession.TransportModeResponsesWS:
			if state.wsConn == nil {
				finalizer, finalizePayload := finalizeCodexTurnLocked(s.exec, state, "response.cancelled", time.Now())
				s.exec.Inflight = false
				s.exec.State = runtimesession.SessionStateIdle
				s.exec.Unlock()
				if finalizer != nil {
					finalizer.FinalizeTurn(finalizePayload)
				}
				codexMaybeDeleteDetachedExecutionSession(s.exec, "detached_ephemeral_session")
				return nil
			}
			if err := writeCodexRealtimeWSMessageLocked(state, state.wsConn, websocket.TextMessage, payload); err != nil {
				conn := clearCodexManagedWebsocketLocked(state)
				finalizer, finalizePayload := finalizeCodexTurnLocked(s.exec, state, "ws_request_failed", time.Now())
				s.exec.Inflight = false
				s.exec.State = runtimesession.SessionStateIdle
				s.exec.Unlock()
				if finalizer != nil {
					finalizer.FinalizeTurn(finalizePayload)
				}
				if conn != nil {
					_ = conn.Close()
				}
				codexMaybeDeleteDetachedExecutionSession(s.exec, "detached_ephemeral_session")
				return newCodexRealtimeProviderError(event.EventID, "ws_request_failed", err.Error())
			}
			s.exec.Unlock()
			return nil
		default:
			s.exec.Unlock()
			return nil
		}

	default:
		s.exec.Unlock()
		return newCodexRealtimeClientError(event.EventID, "unsupported_client_event", "unsupported realtime client event")
	}
}

func (s *codexManagedRealtimeSession) Recv(ctx context.Context) (int, []byte, *types.UsageEvent, error) {
	if s == nil || s.attachment == nil {
		return 0, nil, nil, runtimesession.ErrSessionClosed
	}

	outbound, err := s.attachment.recv(ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	return outbound.messageType, outbound.payload, outbound.usage, outbound.err
}

func (s *codexManagedRealtimeSession) Detach(reason string) {
	if s == nil || s.exec == nil || s.attachment == nil {
		return
	}

	s.detachOnce.Do(func() {
		s.exec.Lock()
		state := getCodexManagedRuntimeStateLocked(s.exec)
		if codexManagedSessionOwnsAttachmentLocked(state, s.ownerSeq, s.attachment) {
			state.attachment = nil
			s.exec.Attached = false
			s.exec.Touch(time.Now())
		}
		s.exec.Unlock()

		s.attachment.close()
		codexMaybeDeleteDetachedExecutionSession(s.exec, "detached_ephemeral_session")
		_ = reason
	})
}

func (s *codexManagedRealtimeSession) Abort(reason string) {
	if s == nil || s.exec == nil {
		return
	}

	s.abortOnce.Do(func() {
		var owned bool
		s.exec.Lock()
		state := getCodexManagedRuntimeStateLocked(s.exec)
		if codexManagedSessionOwnsStateLocked(state, s.ownerSeq) {
			owned = true
			attachment := state.attachment
			state.attachment = nil
			state.ownerSeq = 0
			s.exec.Attached = false
			s.exec.Inflight = false
			s.exec.MarkClosed(strings.TrimSpace(reason))
			s.exec.Touch(time.Now())
			if attachment != nil {
				attachment.close()
			} else if s.attachment != nil {
				s.attachment.close()
			}
		}
		s.exec.Unlock()
		if !owned {
			return
		}
		codexExecutionSessions.DeleteIf(s.exec.Key, s.exec)
	})
}

func (s *codexManagedRealtimeSession) SupportsGracefulDetach() bool {
	return true
}

func (s *codexManagedRealtimeSession) SetTurnObserverFactory(factory runtimesession.TurnObserverFactory) {
	if s == nil || s.exec == nil {
		return
	}

	s.exec.Lock()
	state := getCodexManagedRuntimeStateLocked(s.exec)
	if !codexManagedSessionOwnsAttachmentLocked(state, s.ownerSeq, s.attachment) {
		s.exec.Unlock()
		return
	}
	state.turnObserverFactory = factory
	if s.exec.Inflight && state.turnObserver == nil && factory != nil {
		state.turnObserver = codexGuardTurnObserver(factory())
	}
	s.exec.Unlock()
}

func cleanupCodexExecutionSession(exec *runtimesession.ExecutionSession) {
	if exec == nil {
		return
	}

	var attachment *codexAttachment
	var bridgeStream requester.StreamReaderInterface[string]
	var wsConn *websocket.Conn
	var observer runtimesession.TurnObserver
	var finalizePayload runtimesession.TurnFinalizePayload

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	attachment = state.attachment
	state.attachment = nil
	state.ownerSeq = 0
	bridgeStream = state.bridgeStream
	state.bridgeStream = nil
	wsConn = clearCodexManagedWebsocketLocked(state)
	observer, finalizePayload = finalizeCodexTurnLocked(exec, state, "session_aborted", time.Now())
	exec.Attached = false
	exec.Inflight = false
	exec.MarkClosed("session_aborted")
	if attachment != nil {
		attachment.close()
	}
	exec.Unlock()

	if bridgeStream != nil {
		bridgeStream.Close()
	}
	if wsConn != nil {
		_ = wsConn.Close()
	}
	if observer != nil {
		observer.FinalizeTurn(finalizePayload)
	}
}

func (p *CodexProvider) buildExecutionSessionMetadata(modelName string, options runtimesession.RealtimeOpenOptions) (runtimesession.Metadata, *types.OpenAIErrorWithStatusCode) {
	modelName = normalizeCodexModelName(modelName)
	clientSessionID, clientSuppliedID, errWithCode := p.readRealtimeClientSessionID(options)
	if errWithCode != nil {
		return runtimesession.Metadata{}, errWithCode
	}
	callerNS := p.readRealtimeCallerNamespace()
	capacityNS := p.readRealtimeCapacityNamespace()
	upstreamIdentity := p.readRealtimeUpstreamIdentity()
	compatibilityHash := p.buildRealtimeCompatibilityHash(modelName, upstreamIdentity)
	channelID := 0
	if p.Channel != nil {
		channelID = p.Channel.Id
	}
	bindingKey := ""
	if clientSessionID != "" {
		bindingKey = runtimesession.BuildBindingKey(callerNS, runtimesession.BindingScopeChatRealtime, clientSessionID)
	}
	sessionKey, upstreamSessionID := p.resolveExecutionSessionKey(bindingKey, channelID, compatibilityHash, options)

	return runtimesession.Metadata{
		Key:               sessionKey,
		BindingKey:        bindingKey,
		SessionID:         upstreamSessionID,
		ClientSuppliedID:  clientSuppliedID,
		CallerNS:          callerNS,
		CapacityNS:        capacityNS,
		ChannelID:         channelID,
		CompatibilityHash: compatibilityHash,
		UpstreamIdentity:  upstreamIdentity,
		Model:             modelName,
		Protocol:          codexRealtimeProtocolName,
		IdleTTL:           p.getExecutionSessionTTL(),
	}, nil
}

func (p *CodexProvider) resolveExecutionSessionKey(bindingKey string, channelID int, compatibilityHash string, options runtimesession.RealtimeOpenOptions) (string, string) {
	upstreamSessionID := strings.TrimSpace(options.ResolvedUpstreamSessionID)
	if upstreamSessionID == "" {
		upstreamSessionID = uuid.NewString()
	}

	return buildCodexExecutionSessionKey(channelID, compatibilityHash, upstreamSessionID), upstreamSessionID
}

func buildCodexExecutionSessionKey(channelID int, compatibilityHash, sessionID string) string {
	return fmt.Sprintf("channel:%d/%s/%s", channelID, compatibilityHash, sessionID)
}

func parseCodexExecutionSessionKey(key string) (int, string, string, bool) {
	parts := strings.SplitN(strings.TrimSpace(key), "/", 3)
	if len(parts) != 3 {
		return 0, "", "", false
	}
	channelPart := strings.TrimSpace(parts[0])
	if !strings.HasPrefix(channelPart, "channel:") {
		return 0, "", "", false
	}
	channelID, err := strconv.Atoi(strings.TrimPrefix(channelPart, "channel:"))
	if err != nil || channelID <= 0 {
		return 0, "", "", false
	}
	compatibilityHash := strings.TrimSpace(parts[1])
	sessionID := strings.TrimSpace(parts[2])
	if compatibilityHash == "" || sessionID == "" {
		return 0, "", "", false
	}
	return channelID, compatibilityHash, sessionID, true
}

func (p *CodexProvider) buildRealtimeCompatibilityHash(modelName, upstreamIdentity string) string {
	return hashCodexExecutionIdentity(strings.Join([]string{
		codexRealtimeProtocolName,
		normalizeCodexModelName(modelName),
		upstreamIdentity,
		p.getWebsocketMode(),
		p.getPromptCacheKeyStrategy(),
		p.buildRealtimeHandshakePolicySignature(),
	}, "|"))
}

func (p *CodexProvider) readRealtimeClientSessionID(options runtimesession.RealtimeOpenOptions) (string, bool, *types.OpenAIErrorWithStatusCode) {
	if resolved := strings.TrimSpace(options.ClientSessionID); resolved != "" {
		if err := validateCodexRealtimeExecutionSessionID(resolved); err != nil {
			return "", false, codexRealtimeInvalidSessionIDError(err)
		}
		return resolved, true, nil
	}
	if p != nil && p.Context != nil && p.Context.Request != nil {
		if rawSessionID := runtimesession.ReadClientSessionID(p.Context.Request); rawSessionID != "" {
			if err := validateCodexRealtimeExecutionSessionID(rawSessionID); err != nil {
				return "", false, codexRealtimeInvalidSessionIDError(err)
			}
			return rawSessionID, true, nil
		}
	}
	return "", false, nil
}

func (p *CodexProvider) buildRealtimeHandshakePolicySignature() string {
	headers := p.buildRealtimeCompatibilityHeaders()
	userAgent := ""
	if value := strings.TrimSpace(headers["user-agent"]); value != "" {
		userAgent = value
		delete(headers, "user-agent")
	} else if value := p.getLegacyUserAgentOverride(); value != "" {
		userAgent = value
	}
	if userAgent == defaultUserAgent {
		userAgent = ""
	}
	if value := strings.TrimSpace(headers["originator"]); value == defaultOriginator {
		delete(headers, "originator")
	}

	policy := codexRealtimeHandshakePolicy{
		EffectiveUserAgent: userAgent,
		EffectiveHeaders:   headers,
	}
	if policy.EffectiveUserAgent == "" && len(policy.EffectiveHeaders) == 0 {
		return ""
	}

	payload, err := json.Marshal(policy)
	if err != nil {
		return ""
	}
	return string(payload)
}

func (p *CodexProvider) buildRealtimeCompatibilityHeaders() map[string]string {
	headers := newCodexHeaderBagFromMap(p.buildRealtimeRequestCompatibilityHeaders())
	for key, value := range p.buildRealtimeChannelCompatibilityHeaders() {
		headers.Set(key, value)
	}
	p.applyRealtimeRequestHeaderOverrides(headers)
	headers.SetIfAbsent("originator", defaultOriginator)
	return headers.Map()
}

func (p *CodexProvider) buildRealtimeChannelCompatibilityHeaders() map[string]string {
	headers := make(map[string]string)
	if p == nil || p.Channel == nil {
		return headers
	}

	modelHeaders, err := p.Channel.GetModelHeadersMap()
	if err != nil {
		return headers
	}

	for key, value := range modelHeaders {
		normalizedKey := strings.ToLower(strings.TrimSpace(key))
		trimmedValue := strings.TrimSpace(value)
		if normalizedKey == "" || trimmedValue == "" {
			continue
		}
		switch normalizedKey {
		case "authorization", "content-type", "accept", "connection", "openai-beta", "session_id", "x-session-id":
			continue
		}
		headers[normalizedKey] = trimmedValue
	}

	return headers
}

func (p *CodexProvider) readRealtimeCallerNamespace() string {
	if p != nil {
		return readCodexRealtimeCallerNamespace(p.Context)
	}
	return "anonymous"
}

func (p *CodexProvider) readRealtimeCapacityNamespace() string {
	if p != nil {
		return readCodexRealtimeCapacityNamespace(p.Context)
	}
	return "anonymous"
}

func (p *CodexProvider) readRealtimeUpstreamIdentity() string {
	baseURL := normalizeCodexRealtimeBaseURL(p.GetBaseURL())
	credentialIdentity := p.readRealtimeCredentialIdentity()
	if credentialIdentity == "" {
		credentialIdentity = "credentials:none"
	}
	return fmt.Sprintf("base:%s|credential:%s", baseURL, credentialIdentity)
}

func (p *CodexProvider) readRealtimeCredentialIdentity() string {
	if p != nil && p.Credentials != nil {
		if accountID := strings.TrimSpace(p.Credentials.AccountID); accountID != "" {
			return "account:" + accountID
		}
		if refreshToken := strings.TrimSpace(p.Credentials.RefreshToken); refreshToken != "" {
			return "refresh:" + hashCodexExecutionIdentity(refreshToken)
		}
		if accessToken := strings.TrimSpace(p.Credentials.AccessToken); accessToken != "" {
			return "access:" + hashCodexExecutionIdentity(accessToken)
		}
	}
	if p != nil && p.Channel != nil {
		if key := strings.TrimSpace(p.Channel.Key); key != "" {
			return "channel_key:" + hashCodexExecutionIdentity(key)
		}
	}
	return "credentials:none"
}

func normalizeCodexRealtimeBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return strings.TrimRight(trimmed, "/")
	}

	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else {
		parsed.Host = host
	}
	parsed.Scheme = scheme
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func hashCodexExecutionIdentity(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

func ensureCodexRealtimeExecutionSessionID(req *http.Request) (string, bool, error) {
	if req == nil {
		return "", false, nil
	}
	if sessionID := runtimesession.ReadClientSessionID(req); sessionID != "" {
		if err := validateCodexRealtimeExecutionSessionID(sessionID); err != nil {
			return "", false, err
		}
		if strings.TrimSpace(req.Header.Get("x-session-id")) == "" {
			req.Header.Set("x-session-id", sessionID)
		}
		return sessionID, false, nil
	}

	sessionID := uuid.NewString()
	req.Header.Set("x-session-id", sessionID)
	return sessionID, true, nil
}

func validateCodexRealtimeExecutionSessionID(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("session_id must not be empty")
	}
	if len(sessionID) > codexRealtimeSessionIDMaxLen {
		return fmt.Errorf("session_id must be %d characters or fewer", codexRealtimeSessionIDMaxLen)
	}
	for _, r := range sessionID {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return fmt.Errorf("session_id contains unsupported character %q", r)
		}
	}
	return nil
}

func codexRealtimeInvalidSessionIDError(err error) *types.OpenAIErrorWithStatusCode {
	if err == nil {
		return nil
	}
	return common.StringErrorWrapperLocal(err.Error(), "invalid_session_id", http.StatusBadRequest)
}

func codexRealtimeManagerError(err error) *types.OpenAIErrorWithStatusCode {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtimesession.ErrCallerCapacityExceeded):
		return common.StringErrorWrapperLocal("execution session caller capacity reached", "session_caller_capacity_exceeded", http.StatusTooManyRequests)
	case errors.Is(err, runtimesession.ErrCapacityExceeded):
		return common.StringErrorWrapperLocal("execution session capacity reached", "session_capacity_exceeded", http.StatusServiceUnavailable)
	default:
		return common.ErrorWrapperLocal(err, "execution_session_failed", http.StatusInternalServerError)
	}
}

func readCodexRealtimeCallerNamespace(c *gin.Context) string {
	if c != nil {
		if tokenID := c.GetInt("token_id"); tokenID > 0 {
			return fmt.Sprintf("token:%d", tokenID)
		}
		if userID := c.GetInt("id"); userID > 0 {
			return fmt.Sprintf("user:%d", userID)
		}
		if namespace := authutil.StableRequestCredentialNamespace(c.Request); namespace != "" {
			return namespace
		}
	}
	return "anonymous"
}

func readCodexRealtimeCapacityNamespace(c *gin.Context) string {
	if c != nil {
		if userID := c.GetInt("id"); userID > 0 {
			return fmt.Sprintf("user:%d", userID)
		}
		if tokenID := c.GetInt("token_id"); tokenID > 0 {
			return fmt.Sprintf("token:%d", tokenID)
		}
		if namespace := authutil.StableRequestCredentialNamespace(c.Request); namespace != "" {
			return namespace
		}
	}
	return "anonymous"
}

func (p *CodexProvider) ensureRealtimeTransportLocked(exec *runtimesession.ExecutionSession, state *codexManagedRuntimeState, now time.Time) *types.OpenAIErrorWithStatusCode {
	mode := p.getWebsocketMode()
	if mode == codexWebsocketModeOff {
		conn := clearCodexManagedWebsocketLocked(state)
		if conn != nil && state.bridgeStream == nil {
			exec.Inflight = false
			exec.State = runtimesession.SessionStateIdle
		}
		exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
		exec.FallbackUntil = time.Time{}
		if conn != nil {
			_ = conn.Close()
		}
		return nil
	}

	if state.wsConn != nil {
		exec.Transport = runtimesession.TransportModeResponsesWS
		p.startRealtimeWSReaderLocked(exec, state)
		return nil
	}
	if state.bridgeStream != nil {
		exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
		return nil
	}

	if mode == codexWebsocketModeAuto && exec.Transport == runtimesession.TransportModeResponsesHTTPBridge && now.Before(exec.FallbackUntil) {
		exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
		return nil
	}

	plan, errWithCode := p.prepareChatRealtimeConn(exec.Model, exec.SessionID)
	if errWithCode != nil {
		return errWithCode
	}

	conn, errWithCode := p.dialChatRealtimeConn(plan)
	if errWithCode == nil {
		state.wsConn = conn
		state.wsConnGeneration++
		state.skipBootstrapConn = conn
		exec.Transport = runtimesession.TransportModeResponsesWS
		exec.FallbackUntil = time.Time{}
		configureCodexRealtimeConn(state, conn)
		p.startRealtimeWSReaderLocked(exec, state)
		return nil
	}

	if mode == codexWebsocketModeForce {
		return errWithCode
	}

	exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
	exec.FallbackUntil = now.Add(p.getWebsocketRetryCooldown())
	return nil
}

func (p *CodexProvider) startRealtimeWSReaderLocked(exec *runtimesession.ExecutionSession, state *codexManagedRuntimeState) {
	if state.wsConn == nil {
		return
	}

	conn := state.wsConn
	generation := state.wsConnGeneration
	if state.wsReaderConn == conn {
		return
	}
	state.wsReaderConn = conn

	go func() {
		defer func() {
			exec.Lock()
			currentState := getCodexManagedRuntimeStateLocked(exec)
			if currentState.wsConn == conn {
				currentState.wsConn = nil
			}
			if currentState.wsReaderConn == conn {
				currentState.wsReaderConn = nil
			}
			if currentState.skipBootstrapConn == conn {
				currentState.skipBootstrapConn = nil
			}
			exec.Unlock()

			_ = conn.Close()
		}()

		for {
			_ = conn.SetReadDeadline(time.Now().Add(codexRealtimeReadTimeout))
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				var observer runtimesession.TurnObserver
				var finalizePayload runtimesession.TurnFinalizePayload
				exec.Lock()
				currentState := getCodexManagedRuntimeStateLocked(exec)
				wasCurrent := currentState.wsConn == conn
				var currentAttachment *codexAttachment
				wasInflight := false
				if wasCurrent {
					currentAttachment = currentState.attachment
					wasInflight = exec.Inflight
					currentState.wsConn = nil
					observer, finalizePayload = finalizeCodexTurnLocked(exec, currentState, "provider_connection_closed", time.Now())
					exec.Inflight = false
					exec.State = runtimesession.SessionStateIdle
					exec.Touch(time.Now())
				}
				exec.Unlock()
				if observer != nil {
					observer.FinalizeTurn(finalizePayload)
				}
				codexMaybeDeleteDetachedExecutionSession(exec, "detached_ephemeral_session")

				if wasCurrent && wasInflight && currentAttachment != nil {
					if !enqueueCodexOutbound(currentAttachment, codexRealtimeOutbound{
						messageType: websocket.TextMessage,
						payload:     []byte(newCodexRealtimeProviderError("", "provider_connection_closed", err.Error()).Error()),
					}) {
						return
					}
				}
				return
			}

			exec.Lock()
			currentState := getCodexManagedRuntimeStateLocked(exec)
			isCurrentConn := currentState.wsConn == conn && currentState.wsConnGeneration == generation
			if !isCurrentConn {
				exec.Unlock()
				return
			}
			checkBootstrap := currentState.skipBootstrapConn == conn
			if checkBootstrap {
				currentState.skipBootstrapConn = nil
			}
			if checkBootstrap && isCodexRealtimeBootstrapMessage(messageType, payload) {
				exec.Touch(time.Now())
				exec.Unlock()
				continue
			}
			accumulator := currentState.turnAccumulator
			modelName := exec.Model
			exec.Unlock()

			shouldContinue, usage, newMessage, handlerErr := p.handleRealtimeSupplierMessage(messageType, payload, accumulator, modelName)
			if newMessage != nil {
				payload = newMessage
			}

			terminal, lastResponseID, terminationReason := inspectCodexRealtimeSupplierEvent(messageType, payload)
			receivedAt := time.Now()

			exec.Lock()
			currentState = getCodexManagedRuntimeStateLocked(exec)
			ownsConn := currentState.wsConn == conn
			var attachment *codexAttachment
			var turnObserver runtimesession.TurnObserver
			var finalizePayload runtimesession.TurnFinalizePayload
			if ownsConn {
				attachment = currentState.attachment
				turnObserver = currentState.turnObserver
				markCodexTurnFirstResponseLocked(currentState, receivedAt)
				if lastResponseID != "" {
					exec.LastResponseID = lastResponseID
					currentState.turnLastResponseID = lastResponseID
				}
				if usage != nil {
					mergeCodexTurnUsageLocked(currentState, usage)
				}
				if terminal {
					exec.Inflight = false
					exec.State = runtimesession.SessionStateIdle
					turnObserver, finalizePayload = finalizeCodexTurnLocked(exec, currentState, terminationReason, receivedAt)
				}
				exec.Touch(receivedAt)
			}
			exec.Unlock()
			if !ownsConn {
				return
			}
			usageErr := observeCodexTurnUsage(turnObserver, usage)
			if usageErr != nil {
				var connToClose *websocket.Conn
				if finalizePayload.TurnSeq == 0 {
					exec.Lock()
					errorState := getCodexManagedRuntimeStateLocked(exec)
					if errorState.wsConn == conn {
						attachment = errorState.attachment
						connToClose = clearCodexManagedWebsocketLocked(errorState)
						turnObserver, finalizePayload = finalizeCodexTurnLocked(exec, errorState, "quota_exhausted", time.Now())
						exec.Inflight = false
						exec.State = runtimesession.SessionStateIdle
						exec.Touch(time.Now())
					}
					exec.Unlock()
				} else {
					exec.Lock()
					errorState := getCodexManagedRuntimeStateLocked(exec)
					if errorState.wsConn == conn {
						connToClose = clearCodexManagedWebsocketLocked(errorState)
						exec.Touch(time.Now())
					}
					exec.Unlock()
				}
				if connToClose != nil {
					_ = connToClose.Close()
				}
				if turnObserver != nil && finalizePayload.TurnSeq > 0 {
					turnObserver.FinalizeTurn(finalizePayload)
				}
				codexMaybeDeleteDetachedExecutionSession(exec, "detached_ephemeral_session")
				if attachment != nil {
					_ = enqueueCodexOutbound(attachment, codexRealtimeOutbound{
						messageType: messageType,
						payload:     payload,
						usage:       usage,
						err:         codexRealtimeTurnUsageError(usageErr),
					})
				}
				return
			}
			if turnObserver != nil && finalizePayload.TurnSeq > 0 {
				turnObserver.FinalizeTurn(finalizePayload)
			}
			if terminal {
				codexMaybeDeleteDetachedExecutionSession(exec, "detached_ephemeral_session")
			}

			if handlerErr != nil {
				if attachment != nil {
					_ = enqueueCodexOutbound(attachment, codexRealtimeOutbound{
						messageType: websocket.TextMessage,
						payload:     []byte(handlerErr.Error()),
					})
				}
				return
			}
			if !shouldContinue {
				return
			}

			if attachment != nil {
				if !enqueueCodexOutbound(attachment, codexRealtimeOutbound{
					messageType: messageType,
					payload:     payload,
					usage:       usage,
				}) {
					return
				}
			}
		}
	}()
}

func (p *CodexProvider) sendRealtimeWSEventLocked(ctx context.Context, exec *runtimesession.ExecutionSession, state *codexManagedRuntimeState, payload []byte, eventID string, request *types.OpenAIResponsesRequest) error {
	if errWithCode := p.ensureRealtimeTransportLocked(exec, state, time.Now()); errWithCode != nil {
		exec.Inflight = false
		exec.State = runtimesession.SessionStateIdle
		return codexRealtimeErrorFromOpenAIError(eventID, errWithCode)
	}

	if exec.Transport != runtimesession.TransportModeResponsesWS || state.wsConn == nil {
		if err := p.startRealtimeHTTPBridgeLocked(exec, state, request, eventID); err != nil {
			return err
		}
		return nil
	}

	if err := writeCodexRealtimeWSMessageLocked(state, state.wsConn, websocket.TextMessage, payload); err == nil {
		return nil
	}

	conn := clearCodexManagedWebsocketLocked(state)
	if conn != nil {
		_ = conn.Close()
	}

	if errWithCode := p.ensureRealtimeTransportLocked(exec, state, time.Now()); errWithCode == nil && exec.Transport == runtimesession.TransportModeResponsesWS && state.wsConn != nil {
		if err := writeCodexRealtimeWSMessageLocked(state, state.wsConn, websocket.TextMessage, payload); err == nil {
			return nil
		}
		conn = clearCodexManagedWebsocketLocked(state)
		if conn != nil {
			_ = conn.Close()
		}
	}

	if p.getWebsocketMode() == codexWebsocketModeAuto {
		exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
		exec.FallbackUntil = time.Now().Add(p.getWebsocketRetryCooldown())
		return p.startRealtimeHTTPBridgeLocked(exec, state, request, eventID)
	}

	exec.Inflight = false
	exec.State = runtimesession.SessionStateIdle
	return newCodexRealtimeProviderError(eventID, "ws_request_failed", "failed to deliver realtime websocket request")
}

func (p *CodexProvider) startRealtimeHTTPBridgeLocked(exec *runtimesession.ExecutionSession, state *codexManagedRuntimeState, request *types.OpenAIResponsesRequest, eventID string) error {
	stream, errWithCode := p.createResponsesStreamWithSession(request, exec.SessionID)
	if errWithCode != nil {
		exec.Inflight = false
		exec.State = runtimesession.SessionStateIdle
		return codexRealtimeErrorFromOpenAIError(eventID, errWithCode)
	}

	state.bridgeStream = stream
	exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
	exec.Touch(time.Now())

	go p.pumpRealtimeHTTPBridge(exec, stream)
	return nil
}

func (p *CodexProvider) pumpRealtimeHTTPBridge(exec *runtimesession.ExecutionSession, stream requester.StreamReaderInterface[string]) {
	dataChan, errChan := stream.Recv()
	defer stream.Close()

	for {
		select {
		case data, ok := <-dataChan:
			if !ok {
				return
			}

			payload := extractJSONFromSSE(data)
			if payload == "" {
				continue
			}

			exec.Lock()
			state := getCodexManagedRuntimeStateLocked(exec)
			accumulator := state.turnAccumulator
			modelName := exec.Model
			exec.Unlock()

			usage, terminal, responseID, terminationReason := inspectCodexBridgePayload(payload, accumulator, modelName)
			receivedAt := time.Now()

			var attachment *codexAttachment
			var turnObserver runtimesession.TurnObserver
			var finalizePayload runtimesession.TurnFinalizePayload

			exec.Lock()
			state = getCodexManagedRuntimeStateLocked(exec)
			ownsBridge := state.bridgeStream == stream
			if ownsBridge && responseID != "" {
				exec.LastResponseID = responseID
				state.turnLastResponseID = responseID
			}
			if ownsBridge {
				markCodexTurnFirstResponseLocked(state, receivedAt)
			}
			if ownsBridge && usage != nil {
				mergeCodexTurnUsageLocked(state, usage)
				turnObserver = state.turnObserver
			}
			if terminal && ownsBridge {
				state.bridgeStream = nil
				exec.Inflight = false
				exec.State = runtimesession.SessionStateIdle
				turnObserver, finalizePayload = finalizeCodexTurnLocked(exec, state, terminationReason, receivedAt)
			}
			if ownsBridge {
				attachment = state.attachment
				exec.Touch(receivedAt)
			}
			exec.Unlock()
			usageErr := observeCodexTurnUsage(turnObserver, usage)
			if usageErr != nil {
				if finalizePayload.TurnSeq == 0 {
					exec.Lock()
					errorState := getCodexManagedRuntimeStateLocked(exec)
					if errorState.bridgeStream == stream {
						attachment = errorState.attachment
						errorState.bridgeStream = nil
						turnObserver, finalizePayload = finalizeCodexTurnLocked(exec, errorState, "quota_exhausted", time.Now())
						exec.Inflight = false
						exec.State = runtimesession.SessionStateIdle
						exec.Touch(time.Now())
					}
					exec.Unlock()
				}
				if turnObserver != nil && finalizePayload.TurnSeq > 0 {
					turnObserver.FinalizeTurn(finalizePayload)
				}
				codexMaybeDeleteDetachedExecutionSession(exec, "detached_ephemeral_session")
				stream.Close()
				if attachment != nil {
					_ = enqueueCodexOutbound(attachment, codexRealtimeOutbound{
						messageType: websocket.TextMessage,
						payload:     []byte(payload),
						usage:       usage,
						err:         codexRealtimeTurnUsageError(usageErr),
					})
				}
				return
			}
			if turnObserver != nil && finalizePayload.TurnSeq > 0 {
				turnObserver.FinalizeTurn(finalizePayload)
			}
			if terminal {
				codexMaybeDeleteDetachedExecutionSession(exec, "detached_ephemeral_session")
			}

			if ownsBridge && attachment != nil {
				if !enqueueCodexOutbound(attachment, codexRealtimeOutbound{
					messageType: websocket.TextMessage,
					payload:     []byte(payload),
					usage:       usage,
				}) {
					stream.Close()
					return
				}
			}

		case err := <-errChan:
			var attachment *codexAttachment
			shouldReportTruncatedBridge := false
			var turnObserver runtimesession.TurnObserver
			var finalizePayload runtimesession.TurnFinalizePayload

			exec.Lock()
			state := getCodexManagedRuntimeStateLocked(exec)
			ownsBridge := state.bridgeStream == stream
			if ownsBridge {
				attachment = state.attachment
				shouldReportTruncatedBridge = exec.Inflight && errors.Is(err, io.EOF)
				state.bridgeStream = nil
				turnObserver, finalizePayload = finalizeCodexTurnLocked(exec, state, bridgeTerminationReason(err, shouldReportTruncatedBridge), time.Now())
				exec.Inflight = false
				exec.State = runtimesession.SessionStateIdle
				exec.Touch(time.Now())
			}
			exec.Unlock()
			if turnObserver != nil {
				turnObserver.FinalizeTurn(finalizePayload)
			}
			codexMaybeDeleteDetachedExecutionSession(exec, "detached_ephemeral_session")

			if err != nil && ownsBridge && attachment != nil {
				payload := []byte(newCodexRealtimeProviderError("", "bridge_stream_failed", err.Error()).Error())
				if shouldReportTruncatedBridge {
					payload = []byte(newCodexRealtimeProviderError("", "bridge_stream_failed", "provider bridge stream closed before a terminal response event").Error())
				}
				if !errors.Is(err, io.EOF) || shouldReportTruncatedBridge {
					_ = enqueueCodexOutbound(attachment, codexRealtimeOutbound{
						messageType: websocket.TextMessage,
						payload:     payload,
					})
				}
			}
			return
		}
	}
}

func (p *CodexProvider) createResponsesStreamWithSession(request *types.OpenAIResponsesRequest, sessionID string) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	request, err := cloneCodexResponsesRequest(request)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_event", http.StatusBadRequest)
	}

	p.prepareCodexRequest(request)
	request.Stream = true

	req, errWithCode := p.getResponsesRequestWithSession(request, sessionID)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	resp, errWithCode := p.Requester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	handler := &CodexResponsesStreamHandler{
		Usage: p.Usage,
	}
	return requester.RequestNoTrimStream(p.Requester, resp, handler.HandlerResponsesStream)
}

func getCodexManagedRuntimeStateLocked(exec *runtimesession.ExecutionSession) *codexManagedRuntimeState {
	if state, ok := exec.Data.(*codexManagedRuntimeState); ok && state != nil {
		return state
	}

	state := &codexManagedRuntimeState{}
	exec.Data = state
	return state
}

func clearCodexManagedWebsocketLocked(state *codexManagedRuntimeState) *websocket.Conn {
	if state == nil {
		return nil
	}

	conn := state.wsConn
	state.wsConn = nil
	if state.wsReaderConn == conn {
		state.wsReaderConn = nil
	}
	if state.skipBootstrapConn == conn {
		state.skipBootstrapConn = nil
	}
	return conn
}

func configureCodexRealtimeConn(state *codexManagedRuntimeState, conn *websocket.Conn) {
	if state == nil || conn == nil {
		return
	}
	conn.SetPingHandler(func(appData string) error {
		state.wsWriteMu.Lock()
		defer state.wsWriteMu.Unlock()
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
	})
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(codexRealtimeReadTimeout))
	})
}

func writeCodexRealtimeWSMessageLocked(state *codexManagedRuntimeState, conn *websocket.Conn, messageType int, payload []byte) error {
	if conn == nil {
		return websocket.ErrCloseSent
	}
	if state == nil {
		return conn.WriteMessage(messageType, payload)
	}
	state.wsWriteMu.Lock()
	defer state.wsWriteMu.Unlock()
	return conn.WriteMessage(messageType, payload)
}

func newCodexAttachment() *codexAttachment {
	return newCodexAttachmentWithCapacity(codexRealtimeAttachmentQueueCapacity)
}

func codexManagedSessionOwnsStateLocked(state *codexManagedRuntimeState, ownerSeq uint64) bool {
	return state != nil && ownerSeq != 0 && state.ownerSeq == ownerSeq
}

func codexManagedSessionOwnsAttachmentLocked(state *codexManagedRuntimeState, ownerSeq uint64, attachment *codexAttachment) bool {
	return codexManagedSessionOwnsStateLocked(state, ownerSeq) && state.attachment == attachment
}

func codexShouldDeleteDetachedExecutionSessionLocked(exec *runtimesession.ExecutionSession) bool {
	if exec == nil {
		return false
	}
	return !exec.ClientSuppliedID && !exec.Attached && !exec.Inflight
}

func codexMarkDetachedExecutionSessionClosedLocked(exec *runtimesession.ExecutionSession, reason string) bool {
	if !codexShouldDeleteDetachedExecutionSessionLocked(exec) {
		return false
	}
	exec.MarkClosed(reason)
	return true
}

func codexMaybeDeleteDetachedExecutionSession(exec *runtimesession.ExecutionSession, reason string) {
	if exec == nil {
		return
	}
	exec.Lock()
	shouldDelete := codexMarkDetachedExecutionSessionClosedLocked(exec, reason)
	exec.Unlock()
	if shouldDelete {
		codexExecutionSessions.DeleteIf(exec.Key, exec)
	}
}

func newCodexAttachmentWithCapacity(capacity int) *codexAttachment {
	if capacity <= 0 {
		capacity = codexRealtimeAttachmentQueueCapacity
	}
	return &codexAttachment{
		waitCh: make(chan struct{}),
		queue:  make([]codexRealtimeOutbound, capacity),
	}
}

func (a *codexAttachment) close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	a.closed = true
	a.signalLocked()
}

func (a *codexAttachment) isClosed() bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}

func (a *codexAttachment) recv(ctx context.Context) (codexRealtimeOutbound, error) {
	if a == nil {
		return codexRealtimeOutbound{}, runtimesession.ErrSessionClosed
	}

	for {
		a.mu.Lock()
		if a.size > 0 {
			outbound := a.queue[a.head]
			a.queue[a.head] = codexRealtimeOutbound{}
			a.head = (a.head + 1) % len(a.queue)
			a.size--
			a.signalLocked()
			a.mu.Unlock()
			return outbound, nil
		}
		if a.closed {
			a.mu.Unlock()
			return codexRealtimeOutbound{}, runtimesession.ErrSessionClosed
		}
		waitCh := a.waitCh
		a.mu.Unlock()

		select {
		case <-ctx.Done():
			return codexRealtimeOutbound{}, ctx.Err()
		case <-waitCh:
		}
	}
}

func (a *codexAttachment) signalLocked() {
	close(a.waitCh)
	a.waitCh = make(chan struct{})
}

func enqueueCodexOutbound(attachment *codexAttachment, outbound codexRealtimeOutbound) bool {
	if attachment == nil {
		return false
	}

	var timer *time.Timer
	var timerC <-chan time.Time
	if codexRealtimeOutboundBackpressureTimeout > 0 {
		timer = time.NewTimer(codexRealtimeOutboundBackpressureTimeout)
		defer timer.Stop()
		timerC = timer.C
	}

	for {
		attachment.mu.Lock()
		if attachment.closed {
			attachment.mu.Unlock()
			return false
		}
		if attachment.size < len(attachment.queue) {
			tail := (attachment.head + attachment.size) % len(attachment.queue)
			attachment.queue[tail] = outbound
			attachment.size++
			attachment.signalLocked()
			attachment.mu.Unlock()
			return true
		}
		waitCh := attachment.waitCh
		attachment.mu.Unlock()

		if timerC == nil {
			<-waitCh
			continue
		}

		select {
		case <-waitCh:
		case <-timerC:
			attachment.close()
			return false
		}
	}
}

func buildCodexRealtimeCancelledPayload(responseID string) []byte {
	response := map[string]any{
		"status": types.ResponseStatusCancelled,
	}
	if trimmed := strings.TrimSpace(responseID); trimmed != "" {
		response["id"] = trimmed
	}

	payload, err := json.Marshal(map[string]any{
		"type":     "response.cancelled",
		"response": response,
	})
	if err != nil {
		return []byte(`{"type":"response.cancelled","response":{"status":"cancelled"}}`)
	}
	return payload
}

func isCodexRealtimeBootstrapMessage(messageType int, payload []byte) bool {
	if messageType != websocket.TextMessage {
		return false
	}

	var event types.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		return false
	}

	return strings.TrimSpace(event.Type) == types.EventTypeSessionCreated
}

func beginCodexTurnLocked(state *codexManagedRuntimeState, now time.Time) {
	if state == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}

	state.turnSeq++
	state.turnStartedAt = now
	state.turnFirstResponseAt = time.Time{}
	state.turnCompletedAt = time.Time{}
	state.turnLastResponseID = ""
	state.turnTerminationReason = ""
	state.turnUsage = &types.UsageEvent{}
	state.turnAccumulator = newCodexTurnUsageAccumulator()
	state.turnFinalized = false
	if state.turnObserverFactory != nil {
		state.turnObserver = codexGuardTurnObserver(state.turnObserverFactory())
	} else {
		state.turnObserver = nil
	}
}

func resetCodexTurnLocked(state *codexManagedRuntimeState) {
	if state == nil {
		return
	}

	state.turnStartedAt = time.Time{}
	state.turnFirstResponseAt = time.Time{}
	state.turnCompletedAt = time.Time{}
	state.turnLastResponseID = ""
	state.turnTerminationReason = ""
	state.turnUsage = nil
	state.turnAccumulator = nil
	state.turnFinalized = false
	state.turnObserver = nil
}

func mergeCodexTurnUsageLocked(state *codexManagedRuntimeState, usage *types.UsageEvent) {
	if state == nil || usage == nil {
		return
	}
	if state.turnUsage == nil {
		state.turnUsage = &types.UsageEvent{}
	}
	state.turnUsage.Merge(usage)
}

func markCodexTurnFirstResponseLocked(state *codexManagedRuntimeState, now time.Time) {
	if state == nil || state.turnSeq == 0 || state.turnFinalized {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if state.turnStartedAt.IsZero() {
		state.turnStartedAt = now
	}
	if state.turnFirstResponseAt.IsZero() {
		state.turnFirstResponseAt = now
	}
}

func finalizeCodexTurnLocked(exec *runtimesession.ExecutionSession, state *codexManagedRuntimeState, reason string, now time.Time) (runtimesession.TurnObserver, runtimesession.TurnFinalizePayload) {
	if exec == nil || state == nil || state.turnSeq == 0 || state.turnFinalized {
		return nil, runtimesession.TurnFinalizePayload{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	if state.turnStartedAt.IsZero() {
		state.turnStartedAt = now
	}

	state.turnFinalized = true
	state.turnCompletedAt = now
	if trimmed := strings.TrimSpace(reason); trimmed != "" {
		state.turnTerminationReason = trimmed
	}
	if state.turnLastResponseID != "" {
		exec.LastResponseID = state.turnLastResponseID
	}

	return state.turnObserver, runtimesession.TurnFinalizePayload{
		SessionID:         exec.SessionID,
		Model:             exec.Model,
		TurnSeq:           state.turnSeq,
		LastResponseID:    state.turnLastResponseID,
		TerminationReason: state.turnTerminationReason,
		StartedAt:         state.turnStartedAt,
		FirstResponseAt:   state.turnFirstResponseAt,
		CompletedAt:       state.turnCompletedAt,
		Usage:             state.turnUsage.Clone(),
	}
}

func observeCodexTurnUsage(observer runtimesession.TurnObserver, usage *types.UsageEvent) error {
	if observer == nil || usage == nil {
		return nil
	}
	return observer.ObserveTurnUsage(usage.Clone())
}

func codexGuardTurnObserver(observer runtimesession.TurnObserver) runtimesession.TurnObserver {
	if observer == nil {
		return nil
	}
	return runtimesession.GuardTurnObserver(observer)
}

func codexRealtimeTurnUsageError(err error) error {
	if err == nil {
		return nil
	}

	var event *types.Event
	if errors.As(err, &event) {
		return runtimesession.NewClientPayloadError(event, []byte(event.Error()))
	}

	event = types.NewErrorEvent("", "system_error", "system_error", err.Error())
	return runtimesession.NewClientPayloadError(event, []byte(event.Error()))
}

func codexTurnTerminationReason(eventType string, response *types.OpenAIResponsesResponses) string {
	if response != nil {
		if status := strings.TrimSpace(response.Status); status != "" {
			return "response." + status
		}
	}
	if trimmed := strings.TrimSpace(eventType); trimmed != "" {
		return trimmed
	}
	return "response.completed"
}

func bridgeTerminationReason(err error, truncated bool) string {
	if truncated {
		return "bridge_stream_truncated"
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "bridge_stream_failed"
	}
	return "bridge_stream_closed"
}

func inspectCodexRealtimeSupplierEvent(messageType int, payload []byte) (bool, string, string) {
	if messageType != websocket.TextMessage {
		return false, "", ""
	}

	var event types.OpenAIResponsesStreamResponses
	if err := json.Unmarshal(payload, &event); err != nil {
		return false, "", ""
	}
	return inspectCodexRealtimeEvent(&event)
}

func inspectCodexRealtimeEvent(event *types.OpenAIResponsesStreamResponses) (bool, string, string) {
	responseID := ""
	if event != nil && event.Response != nil {
		responseID = strings.TrimSpace(event.Response.ID)
	}

	if event == nil {
		return false, responseID, ""
	}

	switch event.Type {
	case "error":
		return true, responseID, "error"
	}

	if isCodexRealtimeTerminalEvent(event) {
		return true, responseID, codexTurnTerminationReason(event.Type, event.Response)
	}

	return false, responseID, ""
}

func inspectCodexBridgePayload(payload string, accumulator *codexTurnUsageAccumulator, modelName string) (*types.UsageEvent, bool, string, string) {
	var event types.OpenAIResponsesStreamResponses
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, false, "", ""
	}

	if accumulator != nil {
		accumulator.ObserveEvent(&event)
	}

	terminal, responseID, terminationReason := inspectCodexRealtimeEvent(&event)
	if !terminal {
		return nil, false, responseID, ""
	}

	usage := codexRealtimeUsageEvent(event.Response, accumulator, modelName)
	return usage, terminal, responseID, terminationReason
}

func cloneCodexResponsesRequest(request *types.OpenAIResponsesRequest) (*types.OpenAIResponsesRequest, error) {
	if request == nil {
		return nil, errors.New("response payload is required")
	}

	cloned := *request
	if len(request.Metadata) > 0 {
		cloned.Metadata = make(map[string]string, len(request.Metadata))
		for key, value := range request.Metadata {
			cloned.Metadata[key] = value
		}
	}
	if len(request.Tools) > 0 {
		cloned.Tools = append([]types.ResponsesTools(nil), request.Tools...)
	}
	cloned.Include = cloneCodexMutableValue(request.Include)
	cloned.ToolChoice = cloneCodexMutableValue(request.ToolChoice)
	return &cloned, nil
}

func cloneCodexMutableValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, value := range typed {
			cloned[key] = cloneCodexMutableValue(value)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for i, value := range typed {
			cloned[i] = cloneCodexMutableValue(value)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	case map[string]string:
		cloned := make(map[string]string, len(typed))
		for key, value := range typed {
			cloned[key] = value
		}
		return cloned
	case []map[string]any:
		cloned := make([]map[string]any, len(typed))
		for i, value := range typed {
			current := make(map[string]any, len(value))
			for key, nested := range value {
				current[key] = cloneCodexMutableValue(nested)
			}
			cloned[i] = current
		}
		return cloned
	case json.RawMessage:
		return append(json.RawMessage(nil), typed...)
	default:
		return value
	}
}

func newCodexRealtimeClientError(eventID, code, message string) error {
	return types.NewErrorEvent(eventID, "invalid_request_error", code, message)
}

func newCodexRealtimeProviderError(eventID, code, message string) error {
	return types.NewErrorEvent(eventID, "provider_error", code, message)
}

func codexRealtimeErrorCodeString(code any, fallback string) string {
	switch typed := code.(type) {
	case string:
		if trimmed := strings.TrimSpace(typed); trimmed != "" {
			return trimmed
		}
	case nil:
	default:
		if trimmed := strings.TrimSpace(fmt.Sprint(typed)); trimmed != "" && trimmed != "<nil>" {
			return trimmed
		}
	}
	return fallback
}

func codexRealtimeErrorFromOpenAIError(eventID string, errWithCode *types.OpenAIErrorWithStatusCode) error {
	if errWithCode == nil {
		return newCodexRealtimeProviderError(eventID, "provider_error", "provider error")
	}

	code := codexRealtimeErrorCodeString(errWithCode.Code, "provider_error")
	message := strings.TrimSpace(errWithCode.Message)
	if message == "" {
		message = "provider error"
	}
	return newCodexRealtimeProviderError(eventID, code, message)
}
