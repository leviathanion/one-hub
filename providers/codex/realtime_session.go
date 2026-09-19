package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"one-api/common"
	"one-api/common/authutil"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/providerresponse"
	commonredis "one-api/common/redis"
	commonresponses "one-api/common/responses"
	"one-api/common/wsconn"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/spf13/viper"
)

const codexRealtimeProtocolName = "codex-responses-ws"
const codexRealtimeReadTimeout = 2 * time.Minute
const codexExecutionSessionRedisPrefix = "one-hub:execution-session"
const codexRealtimeLocalCancelReason = "turn_cancelled_by_client"

var codexRealtimeOutboundBackpressureTimeout = 5 * time.Second
var codexRealtimeTurnReadTimeout = codexRealtimeReadTimeout

type codexRealtimeClientEvent struct {
	Type       string `json:"type"`
	EventID    string `json:"event_id,omitempty"`
	ResponseID string `json:"response_id,omitempty"`
}

type codexRealtimeOutbound struct {
	credentials   *providerresponse.CredentialSnapshot
	messageType   wsconn.MessageType
	payload       []byte
	providerClose *runtimerealtime.ProviderClose
	usage         *types.UsageEvent
	origin        runtimerealtime.RealtimePayloadOrigin
	err           error
}

const codexRealtimeAttachmentQueueCapacity = 64

// codexAttachment is a bounded outbound mailbox that preserves queued frames
// during shutdown while rejecting any enqueue that races with close.
type codexAttachment struct {
	mu                  sync.Mutex
	waitCh              chan struct{}
	queue               []codexAttachmentItem
	head                int
	size                int
	reserved            *codexAttachmentItem
	reservedAfter       int
	byteBudget          *runtimerealtime.ByteBudget
	backpressureTimeout time.Duration
	closed              bool
}

type codexAttachmentItem struct {
	outbound codexRealtimeOutbound
	credit   *runtimerealtime.ByteCredit
}

func (i *codexAttachmentItem) release() {
	if i == nil {
		return
	}
	if i.credit != nil {
		i.credit.Release()
		i.credit = nil
	}
}

type codexManagedRuntimeState struct {
	wsCredentials         *providerresponse.CredentialSnapshot
	attachment            *codexAttachment
	ownerSeq              uint64
	wsConn                *wsconn.ManagedConn
	wsConnGeneration      uint64
	wsReaderConn          *wsconn.ManagedConn
	wsReaderContext       context.Context
	skipBootstrapConn     *wsconn.ManagedConn
	turnSeq               int64
	turnStartedAt         time.Time
	turnFirstResponseAt   time.Time
	turnCompletedAt       time.Time
	turnLastResponseID    string
	turnTerminationReason string
	turnUsage             *types.UsageEvent
	turnAccumulator       *codexTurnUsageAccumulator
	turnFinalized         bool
	turnFinalizing        bool
	turnObserver          runtimesession.TurnObserver
	turnObserverFactory   runtimesession.TurnObserverFactory
	turnContext           context.Context
	turnCancel            context.CancelFunc
	turnReadTimer         *time.Timer
	turnReadGen           int64
	deferWSReader         bool
	models                runtimesession.ModelBinding
	turnModels            runtimesession.ModelBinding
}

type codexClearedWebsocket struct {
	conn *wsconn.ManagedConn
}

type codexWSFrame struct {
	messageType wsconn.MessageType
	payload     []byte
	credit      *runtimerealtime.ByteCredit
}

func (f *codexWSFrame) release() {
	if f == nil {
		return
	}
	if f.credit != nil {
		f.credit.Release()
		f.credit = nil
	}
}

type codexManagedRealtimeSession struct {
	provider        *CodexProvider
	exec            *runtimesession.ExecutionSession
	attachment      *codexAttachment
	ownerSeq        uint64
	detachOnce      sync.Once
	abortOnce       sync.Once
	sessionDone     chan struct{}
	sessionDoneOnce sync.Once
}

type codexRealtimeHandshakePolicy struct {
	EffectiveUserAgent string            `json:"effective_user_agent,omitempty"`
	EffectiveHeaders   map[string]string `json:"effective_headers,omitempty"`
}

var (
	codexExecutionSessionsMu sync.RWMutex
	codexExecutionSessions   = runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL:           defaultExecutionSessionTTL,
		JanitorInterval:      time.Minute,
		Cleanup:              cleanupCodexExecutionSession,
		MaxSessions:          defaultExecutionSessionCap,
		MaxSessionsPerCaller: defaultExecutionSessionCallerCap,
		RedisClient:          commonredis.GetRedisClient(),
		RedisPrefix:          codexExecutionSessionRedisPrefix,
		RevocationTimeout:    codexExecutionSessionRevocationTimeout(),
	})
)

func currentCodexExecutionSessions() *runtimesession.Manager {
	codexExecutionSessionsMu.RLock()
	defer codexExecutionSessionsMu.RUnlock()
	return codexExecutionSessions
}

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

	sharedBinding, status := currentCodexExecutionSessions().ResolveBinding(bindingKey)
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
		switch currentCodexExecutionSessions().CreateBindingIfAbsent(binding, ttl) {
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
		switch currentCodexExecutionSessions().ReplaceBindingIfSessionMatches(bindingKey, expectedOldSessionKey, binding, ttl) {
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

func codexAcquireLocalOnlyExecutionSession(expected runtimesession.Metadata, excludedSessionKey string) (*runtimesession.ExecutionSession, func(), bool) {
	if strings.TrimSpace(expected.BindingKey) == "" {
		return nil, nil, false
	}

	binding, ok := currentCodexExecutionSessions().ResolveLocal(expected.BindingKey)
	if !ok || binding == nil || binding.SessionKey == strings.TrimSpace(excludedSessionKey) {
		return nil, nil, false
	}

	exec, releaseLease, ok := currentCodexExecutionSessions().AcquireExisting(binding.SessionKey)
	if !ok {
		return nil, nil, false
	}

	exec.Lock()
	// 本地恢复只能恢复同一上游身份；不能绕过 shared binding 已作出的替换决定。
	if exec.IsClosed() || exec.Visibility != runtimesession.VisibilityLocalOnly ||
		exec.ChannelID != expected.ChannelID || exec.CompatibilityHash != expected.CompatibilityHash {
		exec.Unlock()
		releaseLease()
		return nil, nil, false
	}
	exec.Unlock()
	return exec, releaseLease, true
}

func (p *CodexProvider) planRealtimeOpen(meta runtimesession.Metadata, options runtimerealtime.RealtimeOpenOptions) codexRealtimeOpenPlan {
	plan := codexRealtimeOpenPlan{publishIntent: runtimesession.PublishIntentNone}
	if options.ForceFresh || strings.TrimSpace(meta.BindingKey) == "" {
		return plan
	}

	binding, status := currentCodexExecutionSessions().ResolveBinding(meta.BindingKey)
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

		switch currentCodexExecutionSessions().CheckRevocation(binding.SessionKey) {
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

	binding, status := currentCodexExecutionSessions().ResolveBinding(meta.BindingKey)
	switch status {
	case runtimesession.ResolveMiss:
		plan.publishIntent = runtimesession.PublishIntentCreateIfAbsent
	case runtimesession.ResolveBackendError:
		return plan
	case runtimesession.ResolveHit:
		if binding == nil || strings.TrimSpace(binding.SessionKey) == "" {
			return plan
		}
		if localExec, releaseLease, ok := currentCodexExecutionSessions().AcquireExisting(binding.SessionKey); ok {
			localExec.Lock()
			localExec.MarkClosed("force_fresh_replaced")
			localExec.Unlock()
			releaseLease()
			if localExec.BindingKey == meta.BindingKey {
				currentCodexExecutionSessions().DeleteBinding(meta.BindingKey)
			}
			currentCodexExecutionSessions().DeleteIf(localExec.Key, localExec)
		}
		if currentCodexExecutionSessions().DeleteBindingAndRevokeIfSessionMatches(meta.BindingKey, binding.SessionKey, currentCodexExecutionSessions().RevocationTTLForSession(binding.SessionKey)) == runtimesession.BindingWriteApplied {
			plan.publishIntent = runtimesession.PublishIntentCreateIfAbsent
		}
	}
	return plan
}

func (p *CodexProvider) OpenRealtimeSession(modelName string) (runtimerealtime.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	return p.OpenRealtimeSessionWithOptions(modelName, runtimerealtime.RealtimeOpenOptions{})
}

func codexExecutionSessionRevocationTimeout() time.Duration {
	timeoutMS := viper.GetInt("codex.execution_session_revocation_timeout_ms")
	if timeoutMS <= 0 {
		timeoutMS = 200
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

func InitExecutionSessionManager() {
	currentCodexExecutionSessions().ConfigureRemote(commonredis.GetRedisClient(), codexExecutionSessionRedisPrefix, codexExecutionSessionRevocationTimeout())
}

func ExecutionSessionStats() runtimesession.Stats {
	return currentCodexExecutionSessions().Stats()
}

func (p *CodexProvider) OpenRealtimeSessionWithOptions(modelName string, options runtimerealtime.RealtimeOpenOptions) (runtimerealtime.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	normalizedModelName := strings.TrimSpace(modelName)
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
			exec, releaseLease, ok = currentCodexExecutionSessions().AcquireExisting(plan.candidateSessionKey)
			if ok {
				created = false
			}
		}
		if !options.ForceFresh && exec == nil && strings.TrimSpace(meta.BindingKey) != "" {
			exec, releaseLease, ok = codexAcquireLocalOnlyExecutionSession(meta, plan.candidateSessionKey)
			if ok {
				created = false
			}
		}
		if exec == nil {
			exec, created, releaseLease, err = currentCodexExecutionSessions().AcquireOrCreate(meta)
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

		sessionModel := strings.TrimSpace(exec.Model)
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
		models := options.Models
		if models.RequestedModel == "" {
			models.RequestedModel = normalizedModelName
		}
		if models.ProviderModel == "" {
			models.ProviderModel = normalizedModelName
		}
		if models.BillingModel == "" {
			models.BillingModel = normalizedModelName
		}
		wasAttached = exec.Attached
		staleAttachment = state.attachment
		staleOwnerSeq = state.ownerSeq
		staleTurnObserver = state.turnObserver
		staleTurnObserverFactory = state.turnObserverFactory
		state.ownerSeq++
		state.attachment = attachment
		// The relay installs the turn observer factory immediately after Open.
		// Defer provider reads until the first Recv so a provider-initiated turn
		// cannot race ahead of its principal/billing owner.
		state.deferWSReader = true
		state.turnObserverFactory = nil
		if !exec.Inflight {
			state.turnObserver = nil
		}
		exec.Attached = true

		if errWithCode := p.ensureRealtimeTransportLocked(options.Context, exec, state); errWithCode != nil {
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
				currentCodexExecutionSessions().DeleteIf(meta.Key, exec)
			}
			return nil, errWithCode
		}
		state.models = models
		if state.wsConn == nil {
			state.deferWSReader = false
		}
		if staleAttachment != nil && staleAttachment != attachment {
			if !staleAttachment.takeoverTo(attachment) {
				exec.MarkClosed("attachment_takeover_failed")
				releaseLease()
				exec.Unlock()
				attachment.close()
				return nil, common.StringErrorWrapperLocal("execution session attachment takeover failed", "session_takeover_failed", http.StatusServiceUnavailable)
			}
		}
		if exec.Visibility == runtimesession.VisibilityLocalOnly {
			exec.Unlock()
			codexMaybePromoteExecutionSession(exec)
			exec.Lock()
		}
		releaseLease()
		exec.Unlock()
		if exec.Visibility == runtimesession.VisibilityShared {
			currentCodexExecutionSessions().TouchBinding(exec.BindingKey, exec.Key, meta.ChannelID)
		}

		return &codexManagedRealtimeSession{
			provider:    p,
			exec:        exec,
			attachment:  attachment,
			ownerSeq:    state.ownerSeq,
			sessionDone: make(chan struct{}),
		}, nil
	}

	return nil, common.StringErrorWrapperLocal("execution session is closed during open", "session_closed", http.StatusConflict)
}

func (s *codexManagedRealtimeSession) SendClient(ctx context.Context, frame runtimerealtime.Frame) error {
	if s == nil || s.exec == nil || s.provider == nil {
		return runtimerealtime.ErrSessionClosed
	}
	mt, payload, err := codexRealtimeMessageFromFrame(frame)
	if err != nil {
		return err
	}
	if mt != wsconn.TextMessage {
		return newCodexRealtimeClientError("", "unsupported_client_event", "only text websocket events are supported")
	}

	var event codexRealtimeClientEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		logCodexRealtimeInternalError("codex realtime client event decode failed: " + err.Error())
		return newCodexRealtimeClientError("", "invalid_event", codexRealtimeStaticErrorMessage("invalid_event"))
	}

	s.exec.Lock()
	unlockExec := sync.OnceFunc(func() {
		s.exec.Unlock()
	})
	defer unlockExec()
	state := getCodexManagedRuntimeStateLocked(s.exec)
	if s.exec.IsClosed() || !codexManagedSessionOwnsAttachmentLocked(state, s.ownerSeq, s.attachment) {
		unlockExec()
		return runtimerealtime.ErrSessionClosed
	}

	switch strings.TrimSpace(event.Type) {
	case "response.create":
		if s.exec.Inflight || state.turnFinalizing {
			unlockExec()
			return newCodexRealtimeClientError(event.EventID, "session_busy", "execution session already has an inflight response")
		}

		models := state.models
		if models.RequestedModel == "" {
			name := strings.TrimSpace(s.exec.Model)
			models = runtimesession.ModelBinding{RequestedModel: name, ProviderModel: name, BillingModel: name}
		}
		eventID, request, encodedPayload, err := s.provider.prepareCodexRealtimeCreatePayload(payload, models)
		if err != nil {
			unlockExec()
			return err
		}
		if err := codexCheckStaleResponsesWSContinuationLocked(state, eventID, request); err != nil {
			unlockExec()
			return err
		}

		s.exec.LastResponseID = ""
		s.exec.Inflight = true
		s.exec.State = runtimesession.SessionStateActive
		now := time.Now()
		s.exec.Touch(now)

		if err := s.provider.ensureRealtimeTransportLocked(ctx, s.exec, state); err != nil {
			s.exec.Inflight = false
			s.exec.State = runtimesession.SessionStateIdle
			resetCodexTurnLocked(state)
			unlockExec()
			return codexRealtimeErrorFromOpenAIError(eventID, err)
		}

		beginCodexTurnLocked(state, now, ctx)
		if state.turnAccumulator != nil {
			state.turnAccumulator.SeedPromptFromRequest(request, s.provider.codexPreCost())
		}
		promptTokenEstimate := int64(0)
		if state.turnAccumulator != nil && state.turnAccumulator.seedPromptTokens > 0 {
			promptTokenEstimate = int64(state.turnAccumulator.seedPromptTokens)
		}
		unknownChargeDimensions := false
		for _, tool := range request.Tools {
			if strings.TrimSpace(tool.Type) != "function" {
				unknownChargeDimensions = true
				break
			}
		}
		turnAdmission := runtimesession.TurnAdmission{
			ExplicitClientCreate:      true,
			AutomaticFeaturesDisabled: true,
			Models:                    state.turnModels,
			WorkID:                    fmt.Sprintf("response:%d", state.turnSeq),
			SessionID:                 s.exec.SessionID,
			PromptTokens:              promptTokenEstimate,
			MaxOutputTokens:           int64(request.MaxOutputTokens),
			ServiceTier:               request.ServiceTier,
			UnknownChargeDimensions:   unknownChargeDimensions,
		}
		observer, turnSeq := state.turnObserver, state.turnSeq
		// 准入查询与预扣在会话锁外执行；在途标记防止同时提交第二轮。
		s.exec.Unlock()
		admissionErr := runtimesession.AdmitBoundedTurn(observer, turnAdmission)
		s.exec.Lock()
		if s.exec.IsClosed() || state.turnSeq != turnSeq || state.turnFinalized || !codexManagedSessionOwnsAttachmentLocked(state, s.ownerSeq, s.attachment) {
			unlockExec()
			rollbackCodexTurnAdmission(observer, "session_closed_before_send")
			return runtimerealtime.ErrSessionClosed
		}
		if admissionErr != nil {
			s.exec.Inflight = false
			s.exec.State = runtimesession.SessionStateIdle
			resetCodexTurnLocked(state)
			unlockExec()
			if observer != nil {
				finishCodexTurn(s.exec, observer, runtimesession.TurnFinalizePayload{SessionID: s.exec.SessionID, TurnSeq: turnSeq, Models: turnAdmission.Models, WorkID: turnAdmission.WorkID, TerminationReason: "admission_failed"})
			}
			return codexRealtimeClientPayloadErrorFromObserver(eventID, admissionErr)
		}
		armCodexTurnReadTimeoutLocked(s.exec, state)
		if err := sendCodexRealtimeWSEventLocked(s.exec, state, encodedPayload, eventID, s.ownerSeq, s.attachment); err != nil {
			var finalizer runtimesession.TurnObserver
			var rollbackObserver runtimesession.TurnObserver
			var finalizePayload runtimesession.TurnFinalizePayload
			if codexManagedSessionOwnsAttachmentLocked(state, s.ownerSeq, s.attachment) {
				if codexRealtimeShouldRollbackAdmissionAfterLocalFailure(err) {
					rollbackObserver = state.turnObserver
				} else if codexRealtimeIsAmbiguousWSWriteFailure(err) {
					// The provider may have observed this response.create. Preserve the
					// admitted floor and emit the normal turn settlement/log exactly once;
					// a reset without finalization would orphan the preconsume forever.
					finalizer, finalizePayload = finalizeCodexTurnLocked(s.exec, state, "ws_write_failed", time.Now())
				}
				resetCodexTurnLocked(state)
			}
			unlockExec()
			rollbackCodexTurnAdmission(rollbackObserver, "send_local_failure")
			if rollbackObserver != nil {
				finishCodexTurn(s.exec, rollbackObserver, runtimesession.TurnFinalizePayload{SessionID: s.exec.SessionID, TurnSeq: turnSeq, Models: turnAdmission.Models, WorkID: turnAdmission.WorkID, TerminationReason: "send_local_failure"})
			}
			if finalizer != nil {
				finishCodexTurn(s.exec, finalizer, finalizePayload)
			}
			return err
		}
		unlockExec()
		return nil

	case "response.cancel":
		s.exec.Touch(time.Now())
		if state.wsConn == nil {
			finalizer, finalizePayload := finalizeCodexTurnLocked(s.exec, state, codexRealtimeLocalCancelReason, time.Now())
			s.exec.Inflight = false
			s.exec.State = runtimesession.SessionStateIdle
			unlockExec()
			if finalizer != nil {
				finishCodexTurn(s.exec, finalizer, finalizePayload)
			}
			codexMaybeDeleteDetachedExecutionSession(s.exec, "detached_ephemeral_session")
			return nil
		}
		conn := state.wsConn
		if err := writeCodexRealtimeWSMessageWithExecUnlocked(s.exec, conn, wsconn.TextMessage, payload); err != nil {
			logCodexRealtimeInternalError("codex realtime websocket request write failed: " + err.Error())
			if !codexManagedSessionOwnsAttachmentLocked(state, s.ownerSeq, s.attachment) {
				unlockExec()
				return runtimerealtime.ErrSessionClosed
			}
			clearedToClose := codexClearedWebsocket{conn: conn}
			if state.wsConn == conn {
				clearedToClose = clearCodexManagedWebsocketLocked(state)
			}
			finalizer, finalizePayload := finalizeCodexTurnLocked(s.exec, state, "ws_request_failed", time.Now())
			s.exec.Inflight = false
			s.exec.State = runtimesession.SessionStateIdle
			unlockExec()
			if finalizer != nil {
				finishCodexTurn(s.exec, finalizer, finalizePayload)
			}
			closeCodexClearedWebsocket(clearedToClose)
			codexMaybeDeleteDetachedExecutionSession(s.exec, "detached_ephemeral_session")
			return newCodexRealtimeProviderError(event.EventID, "ws_request_failed", codexRealtimeStaticErrorMessage("ws_request_failed"))
		}
		unlockExec()
		return nil

	default:
		unlockExec()
		return newCodexRealtimeClientError(event.EventID, "unsupported_client_event", "unsupported realtime client event")
	}
}

func (s *codexManagedRealtimeSession) Recv(ctx context.Context) (runtimerealtime.RecvEvent, error) {
	if s == nil || s.attachment == nil {
		return runtimerealtime.RecvEvent{}, runtimerealtime.ErrSessionClosed
	}
	s.startDeferredRealtimeWSReader()

	outbound, err := s.attachment.recvWithStop(ctx, s.sessionDone)
	if err != nil {
		return runtimerealtime.RecvEvent{}, err
	}
	event := runtimerealtime.RecvEvent{
		Credentials:   outbound.credentials,
		ProviderClose: outbound.providerClose,
		Usage:         outbound.usage,
		Origin:        outbound.origin,
		Err:           outbound.err,
	}
	if len(outbound.payload) > 0 {
		frame := codexRealtimeFrameFromMessage(outbound.messageType, outbound.payload)
		event.Frame = &frame
	}
	return event, nil
}

func codexRealtimeMessageFromFrame(frame runtimerealtime.Frame) (wsconn.MessageType, []byte, error) {
	if frame.IsZero() {
		return 0, nil, runtimerealtime.ErrInvalidFrame
	}
	switch frame.Kind() {
	case runtimerealtime.FrameKindText:
		return wsconn.TextMessage, frame.Payload(), nil
	case runtimerealtime.FrameKindBinary:
		return wsconn.BinaryMessage, frame.Payload(), nil
	default:
		return 0, nil, runtimerealtime.ErrInvalidFrame
	}
}

func codexRealtimeFrameFromMessage(messageType wsconn.MessageType, payload []byte) runtimerealtime.Frame {
	if messageType == wsconn.BinaryMessage {
		return runtimerealtime.NewBinaryFrame(payload)
	}
	return runtimerealtime.NewTextFrame(payload)
}

func (s *codexManagedRealtimeSession) startDeferredRealtimeWSReader() {
	if s == nil || s.exec == nil || s.provider == nil {
		return
	}
	s.exec.Lock()
	state := getCodexManagedRuntimeStateLocked(s.exec)
	if codexManagedSessionOwnsAttachmentLocked(state, s.ownerSeq, s.attachment) &&
		state.wsConn != nil &&
		state.deferWSReader {
		s.provider.startRealtimeWSReaderLocked(s.exec, state)
	}
	s.exec.Unlock()
}

func (s *codexManagedRealtimeSession) Detach(reason string) {
	if s == nil || s.exec == nil || s.attachment == nil {
		return
	}

	s.detachOnce.Do(func() {
		s.stopSessionAttachment()
		var cleared codexClearedWebsocket
		var observer runtimesession.TurnObserver
		var finalizePayload runtimesession.TurnFinalizePayload
		s.exec.Lock()
		state := getCodexManagedRuntimeStateLocked(s.exec)
		if codexManagedSessionOwnsAttachmentLocked(state, s.ownerSeq, s.attachment) {
			now := time.Now()
			if s.exec.ClientSuppliedID {
				// Reattach replaces only the downstream attachment. The logical
				// execution, provider transport and admitted turn keep running; the
				// detached attachment remains the bounded mailbox until takeover.
				s.exec.Attached = false
				s.exec.Touch(now)
				s.exec.Unlock()
				return
			}
			state.attachment = nil
			s.exec.Attached = false
			if s.exec.Inflight {
				observer, finalizePayload = finalizeCodexTurnLocked(s.exec, state, "detached", now)
				s.exec.Inflight = false
				s.exec.State = runtimesession.SessionStateIdle
			}
			s.exec.Touch(now)
			cleared = clearCodexManagedWebsocketLocked(state)
		}
		s.exec.Unlock()

		s.attachment.close()
		if observer != nil && finalizePayload.TurnSeq > 0 {
			finishCodexTurn(s.exec, observer, finalizePayload)
		}
		if cleared.conn != nil {
			cleared.conn.Close(wsconn.CloseInfo{
				Kind:   wsconn.CloseKindGracefulShutdown,
				Code:   wsconn.CloseNormalClosure,
				Reason: strings.TrimSpace(reason),
			})
		}
		codexMaybeDeleteDetachedExecutionSession(s.exec, "detached_ephemeral_session")
	})
}

func (s *codexManagedRealtimeSession) Abort(reason string) {
	if s == nil || s.exec == nil {
		return
	}

	s.abortOnce.Do(func() {
		s.stopSessionAttachment()
		var owned bool
		var cleared codexClearedWebsocket
		var observer runtimesession.TurnObserver
		var finalizePayload runtimesession.TurnFinalizePayload
		s.exec.Lock()
		state := getCodexManagedRuntimeStateLocked(s.exec)
		if codexManagedSessionOwnsStateLocked(state, s.ownerSeq) {
			owned = true
			cleared = clearCodexManagedWebsocketLocked(state)
			// Abort owns the closure cut. Once
			// provider work was admitted, exactly one finalization must run even
			// when no terminal/usage frame was observed.
			if s.exec.Inflight {
				observer, finalizePayload = finalizeCodexTurnLocked(s.exec, state, strings.TrimSpace(reason), time.Now())
			}
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
			// When ownership is stale (new handler already re-attached),
			// we cannot safely mutate exec state, but we must release our
			// attachment to prevent mailbox goroutine leaks.
			// close() is idempotent and safe to call after the new owner
			// has already closed it.
			if s.attachment != nil {
				s.attachment.close()
			}
			return
		}
		closeCodexClearedWebsocket(cleared)
		if observer != nil {
			finishCodexTurn(s.exec, observer, finalizePayload)
		}
		currentCodexExecutionSessions().DeleteIf(s.exec.Key, s.exec)
	})
}

func (s *codexManagedRealtimeSession) stopSessionAttachment() {
	if s == nil || s.sessionDone == nil {
		return
	}
	s.sessionDoneOnce.Do(func() { close(s.sessionDone) })
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
	_ = cleanupCodexExecutionSessionWithLock(exec, false)
}

// cleanupCodexExecutionSessionAfterPanic releases as much as possible without
// risking a self-deadlock when the calling goroutine panicked while holding
// exec.Lock(). A held mutex cannot be force-unlocked, but we can still close
// the WebSocket, surface observer finalization, and avoid blocking forever on
// state cleanup. The bookkeeping fields (Inflight, State) are only mutated
// when TryLock succeeds.
func cleanupCodexExecutionSessionAfterPanic(exec *runtimesession.ExecutionSession) {
	if cleanupCodexExecutionSessionWithLock(exec, true) {
		return
	}
	scheduleCodexExecutionSessionPanicCleanup(exec)
}

func cleanupCodexExecutionSessionWithLock(exec *runtimesession.ExecutionSession, tryLockOnly bool) bool {
	if exec == nil {
		return true
	}

	var attachment *codexAttachment
	var clearedWS codexClearedWebsocket
	var observer runtimesession.TurnObserver
	var finalizePayload runtimesession.TurnFinalizePayload
	locked := false
	if tryLockOnly {
		locked = exec.TryLock()
	} else {
		exec.Lock()
		locked = true
	}
	if locked {
		state := getCodexManagedRuntimeStateLocked(exec)
		attachment = state.attachment
		state.attachment = nil
		state.ownerSeq = 0
		clearedWS = clearCodexManagedWebsocketLocked(state)
		observer, finalizePayload = finalizeCodexTurnLocked(exec, state, "session_aborted", time.Now())
		exec.Attached = false
		exec.Inflight = false
		exec.MarkClosed("session_aborted")
		if attachment != nil {
			attachment.close()
		}
		exec.Unlock()
	} else {
		return false
	}

	closeCodexClearedWebsocket(clearedWS)
	if observer != nil {
		finishCodexTurn(exec, observer, finalizePayload)
	}
	return true
}

func scheduleCodexExecutionSessionPanicCleanup(exec *runtimesession.ExecutionSession) {
	if exec == nil {
		return
	}
	exec.MarkClosedBestEffort("session_aborted_panic")
	currentCodexExecutionSessions().DeleteIfWithoutCleanup(exec.Key, exec)
	go func() {
		timer := time.NewTimer(25 * time.Millisecond)
		defer timer.Stop()
		for attempt := 0; attempt < 20; attempt++ {
			<-timer.C
			if cleanupCodexExecutionSessionWithLock(exec, true) {
				return
			}
			timer.Reset(time.Duration(25+attempt*25) * time.Millisecond)
		}
		logger.SysError(fmt.Sprintf("codex realtime panic cleanup could not acquire execution session lock session=%s", exec.SessionID))
	}()
}

func recoverCodexRealtimeGoroutine(label string, exec *runtimesession.ExecutionSession, cleanup ...func()) {
	if recovered := recover(); recovered != nil {
		sessionID := ""
		if exec != nil {
			sessionID = exec.SessionID
		}
		logger.SysError(fmt.Sprintf("codex realtime %s panic session=%s: %v", label, sessionID, recovered))
		logger.SysError(fmt.Sprintf("stacktrace from panic: %s", string(debug.Stack())))
		for _, fn := range cleanup {
			if fn != nil {
				fn()
			}
		}
		// Avoid self-deadlock when the goroutine panicked while exec.Lock() was
		// held: the panicked goroutine still owns the mutex and a blocking
		// Lock() here would hang forever. Use TryLock so cleanup is best-effort.
		cleanupCodexExecutionSessionAfterPanic(exec)
	}
}

func (p *CodexProvider) buildExecutionSessionMetadata(modelName string, options runtimerealtime.RealtimeOpenOptions) (runtimesession.Metadata, *types.OpenAIErrorWithStatusCode) {
	modelName = strings.TrimSpace(modelName)
	clientSessionID, clientSuppliedID, errWithCode := p.readRealtimeClientSessionID(options)
	if errWithCode != nil {
		return runtimesession.Metadata{}, errWithCode
	}
	callerNS := p.readRealtimeCallerNamespace()
	capacityNS := p.readRealtimeCapacityNamespace()
	upstreamIdentity := p.readRealtimeUpstreamIdentity()
	compatibilityHash, err := p.buildRealtimeCompatibilityHash(modelName, upstreamIdentity)
	if err != nil {
		return runtimesession.Metadata{}, codexClientIdentityError(err)
	}
	channelID := 0
	if channel := p.codexChannel(); channel != nil {
		channelID = channel.Id
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

func (p *CodexProvider) resolveExecutionSessionKey(bindingKey string, channelID int, compatibilityHash string, options runtimerealtime.RealtimeOpenOptions) (string, string) {
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

func (p *CodexProvider) buildRealtimeCompatibilityHash(modelName, upstreamIdentity string) (string, error) {
	signature, err := p.buildRealtimeHandshakePolicySignature()
	if err != nil {
		return "", err
	}
	return hashCodexExecutionIdentity(strings.Join([]string{
		codexRealtimeProtocolName,
		strings.TrimSpace(modelName),
		upstreamIdentity,
		signature,
	}, "|")), nil
}

func (p *CodexProvider) readRealtimeClientSessionID(options runtimerealtime.RealtimeOpenOptions) (string, bool, *types.OpenAIErrorWithStatusCode) {
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

func (p *CodexProvider) buildRealtimeHandshakePolicySignature() (string, error) {
	headers, err := p.buildRealtimeCompatibilityHeaders()
	if err != nil {
		return "", err
	}
	userAgent := ""
	if value := strings.TrimSpace(headers["user-agent"]); value != "" {
		userAgent = value
		delete(headers, "user-agent")
	}

	policy := codexRealtimeHandshakePolicy{
		EffectiveUserAgent: userAgent,
		EffectiveHeaders:   headers,
	}
	if policy.EffectiveUserAgent == "" && len(policy.EffectiveHeaders) == 0 {
		return "", nil
	}

	payload, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func (p *CodexProvider) buildRealtimeCompatibilityHeaders() (map[string]string, error) {
	headers := newCodexHeaderBagFromMap(p.buildRealtimeRequestCompatibilityHeaders())
	for key, value := range p.buildRealtimeChannelCompatibilityHeaders() {
		headers.Set(key, value)
	}
	p.applyRealtimeRequestHeaderOverrides(headers)
	if err := p.applyClientIdentityHeaders(headers); err != nil {
		return nil, err
	}
	// 签名使用规范化键，与实际请求采用同一份身份解析结果。
	result := make(map[string]string)
	for key, value := range headers.Map() {
		result[strings.ToLower(key)] = value
	}
	return result, nil
}

func (p *CodexProvider) buildRealtimeChannelCompatibilityHeaders() map[string]string {
	headers := make(map[string]string)
	channel := p.codexChannel()
	if channel == nil {
		return headers
	}

	modelHeaders, err := channel.GetModelHeadersMap()
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
	if credentials := p.credentialsSnapshot(); credentials != nil {
		if accountID := strings.TrimSpace(credentials.AccountID); accountID != "" {
			return "account:" + accountID
		}
		if refreshToken := strings.TrimSpace(credentials.RefreshToken); refreshToken != "" {
			return "refresh:" + hashCodexExecutionIdentity(refreshToken)
		}
		if accessToken := strings.TrimSpace(credentials.AccessToken); accessToken != "" {
			return "access:" + hashCodexExecutionIdentity(accessToken)
		}
	}
	if channel := p.codexChannel(); channel != nil {
		if key := strings.TrimSpace(channel.Key); key != "" {
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
	return runtimesession.ValidateClientSessionID(sessionID)
}

func codexRealtimeInvalidSessionIDError(err error) *types.OpenAIErrorWithStatusCode {
	if err == nil {
		return nil
	}
	logCodexRealtimeInternalError("codex realtime invalid session id: " + err.Error())
	return common.StringErrorWrapperLocal(codexRealtimeStaticErrorMessage("invalid_session_id"), "invalid_session_id", http.StatusBadRequest)
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

func codexRealtimePumpContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

func (p *CodexProvider) ensureRealtimeTransportLocked(ctx context.Context, exec *runtimesession.ExecutionSession, state *codexManagedRuntimeState) *types.OpenAIErrorWithStatusCode {
	if state.wsConn != nil {
		state.wsReaderContext = codexRealtimePumpContext(ctx)
		if !state.deferWSReader {
			p.startRealtimeWSReaderLocked(exec, state)
		}
		return nil
	}
	plan, apiErr := p.prepareChatRealtimeConn(exec.Model, exec.SessionID)
	if apiErr != nil {
		return apiErr
	}
	conn, apiErr := p.dialChatRealtimeConnWithContext(ctx, plan)
	if apiErr != nil {
		return apiErr
	}
	state.wsConn = conn
	state.wsCredentials = providerresponse.NewCredentialSnapshot(providerresponse.ConnectionCredentials(plan.wsURL, codexRealtimeHTTPHeader(plan.headers)))
	state.wsConnGeneration++
	state.wsReaderContext = codexRealtimePumpContext(ctx)
	state.skipBootstrapConn = conn
	if !state.deferWSReader {
		p.startRealtimeWSReaderLocked(exec, state)
	}
	return nil
}

func (p *CodexProvider) startRealtimeWSReaderLocked(exec *runtimesession.ExecutionSession, state *codexManagedRuntimeState) {
	if state == nil || state.wsConn == nil {
		return
	}

	conn := state.wsConn
	credentials := state.wsCredentials
	pumpCtx := state.wsReaderContext
	if pumpCtx == nil {
		pumpCtx = context.Background()
	}
	generation := state.wsConnGeneration
	if state.wsReaderConn == conn {
		state.deferWSReader = false
		return
	}
	state.deferWSReader = false
	state.wsReaderConn = conn

	go func() {
		var panicked atomic.Bool
		defer func() {
			if panicked.Load() {
				// Goroutine panicked while exec.Lock() may have been held; a
				// blocking Lock() here would self-deadlock. The recover defer
				// already attempted best-effort cleanup via TryLock.
				conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "codex_ws_reader_panic"})
				return
			}
			cleared := codexClearedWebsocket{conn: conn}
			func() {
				exec.Lock()
				defer exec.Unlock()
				currentState := getCodexManagedRuntimeStateLocked(exec)
				if currentState.wsConn == conn {
					cleared = clearCodexManagedWebsocketLocked(currentState)
				} else if currentState.wsReaderConn == conn {
					currentState.wsReaderConn = nil
				}
				if currentState.skipBootstrapConn == conn {
					currentState.skipBootstrapConn = nil
				}
			}()

			closeCodexClearedWebsocket(cleared)
		}()
		defer recoverCodexRealtimeGoroutine("ws_reader", exec, func() {
			panicked.Store(true)
		})

		frameCh := make(chan codexWSFrame, 64)
		providerFrameBudget := runtimerealtime.NewByteBudget(config.RealtimeWebsocketProviderFrameQueueMaxBytes())
		closeCh := make(chan wsconn.CloseInfo, 1)
		var finishPumpOnce sync.Once
		finishPump := func(info wsconn.CloseInfo) {
			finishPumpOnce.Do(func() {
				closeCh <- info
				close(frameCh)
			})
		}
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					finishPump(wsconn.CloseInfo{
						Kind:   wsconn.CloseKindAbort,
						Reason: "codex_provider_pump_panic",
						Err:    fmt.Errorf("%v", recovered),
					})
				}
			}()
			pump := &wsconn.Pump{
				Conn: conn,
				Handle: func(handleCtx context.Context, messageType wsconn.MessageType, payload []byte) {
					credit, err := providerFrameBudget.Acquire(handleCtx, len(payload))
					if err != nil {
						if handleCtx.Err() == nil {
							conn.Close(wsconn.CloseInfo{
								Kind:   wsconn.CloseKindBackpressure,
								Code:   wsconn.CloseTryAgainLater,
								Reason: "provider_frame_byte_budget",
								Err:    err,
							})
						}
						return
					}
					frame := codexWSFrame{messageType: messageType, payload: append([]byte(nil), payload...), credit: credit}
					select {
					case frameCh <- frame:
					case <-handleCtx.Done():
						frame.release()
					case <-conn.Done():
						frame.release()
					}
				},
				OnClose: finishPump,
			}
			pump.Run(pumpCtx)
		}()

		for {
			frame, ok := <-frameCh
			if !ok {
				info := <-closeCh
				var observer runtimesession.TurnObserver
				var finalizePayload runtimesession.TurnFinalizePayload
				var currentAttachment *codexAttachment
				clearedOnReadErr := codexClearedWebsocket{}
				wasCurrent := false
				wasInflight := false
				func() {
					exec.Lock()
					defer exec.Unlock()
					currentState := getCodexManagedRuntimeStateLocked(exec)
					wasCurrent = currentState.wsConn == conn
					if wasCurrent {
						currentAttachment = currentState.attachment
						wasInflight = exec.Inflight
						clearedOnReadErr = clearCodexManagedWebsocketLocked(currentState)
						observer, finalizePayload = finalizeCodexTurnLocked(exec, currentState, "provider_connection_closed", time.Now())
						exec.Inflight = false
						exec.State = runtimesession.SessionStateIdle
						exec.Touch(time.Now())
					}
				}()
				closeCodexClearedWebsocket(clearedOnReadErr)
				if observer != nil {
					finishCodexTurn(exec, observer, finalizePayload)
				}
				codexMaybeDeleteDetachedExecutionSession(exec, "detached_ephemeral_session")

				if wasCurrent && wasInflight && currentAttachment != nil {
					outbound := codexRealtimeOutboundFromCloseInfo(info)
					outbound.credentials = credentials
					if !enqueueCodexOutbound(currentAttachment, outbound) {
						cleanupCodexExecutionSession(exec)
						return
					}
				}
				return
			}
			frame.release()
			messageType, payload := frame.messageType, frame.payload
			terminal, lastResponseID, terminationReason := inspectCodexSupplierMessage(messageType, payload)

			var (
				accumulator                   *codexTurnUsageAccumulator
				admission                     runtimesession.TurnAdmission
				providerInitiated             bool
				providerInitiatedObserver     runtimesession.TurnObserver
				providerInitiatedAdmissionErr error
				shouldContinueLoop            = true
				shouldSkipBootstrap           bool
			)
			func() {
				exec.Lock()
				defer exec.Unlock()
				currentState := getCodexManagedRuntimeStateLocked(exec)
				isCurrentConn := currentState.wsConn == conn && currentState.wsConnGeneration == generation
				if !isCurrentConn {
					shouldContinueLoop = false
					return
				}
				checkBootstrap := currentState.skipBootstrapConn == conn
				if checkBootstrap {
					currentState.skipBootstrapConn = nil
				}
				if checkBootstrap && isCodexRealtimeBootstrapMessage(messageType, payload) {
					exec.Touch(time.Now())
					shouldSkipBootstrap = true
					return
				}
				if !exec.Inflight && currentState.turnObserverFactory != nil && lastResponseID != "" && lastResponseID != exec.LastResponseID {
					beginCodexTurnLocked(currentState, time.Now(), context.Background())
					exec.Inflight = true
					exec.State = runtimesession.SessionStateActive
					armCodexTurnReadTimeoutLocked(exec, currentState)
					providerInitiated = true
					providerInitiatedObserver = currentState.turnObserver
				}
				accumulator = currentState.turnAccumulator
				admission = runtimesession.TurnAdmission{
					Models:    currentState.turnModels,
					WorkID:    fmt.Sprintf("response:%d", currentState.turnSeq),
					SessionID: exec.SessionID,
				}
			}()
			if !shouldContinueLoop {
				return
			}
			if shouldSkipBootstrap {
				continue
			}
			if providerInitiated {
				providerInitiatedAdmissionErr = runtimesession.ObserveProviderInitiatedTurn(providerInitiatedObserver, admission)
				if providerInitiatedAdmissionErr != nil {
					logCodexRealtimeInternalError("provider-initiated realtime turn admission failed: " + providerInitiatedAdmissionErr.Error())
				}
			}

			shouldContinue, usage, newMessage, handlerErr := p.handleCodexSupplierMessage(messageType, payload, accumulator)
			if newMessage != nil {
				payload = newMessage
			}
			topLevelError := codexSupplierPayloadIsTopLevelError(payload)
			receivedAt := time.Now()

			var attachment *codexAttachment
			var turnObserver runtimesession.TurnObserver
			var finalizePayload runtimesession.TurnFinalizePayload
			var ownsConn bool
			var connectionError bool
			var connectionErrorConn codexClearedWebsocket
			func() {
				exec.Lock()
				defer exec.Unlock()
				currentState := getCodexManagedRuntimeStateLocked(exec)
				ownsConn = currentState.wsConn == conn
				if ownsConn {
					attachment = currentState.attachment
					turnObserver = currentState.turnObserver
					if handlerErr != nil || providerInitiatedAdmissionErr != nil || codexSupplierErrorIsConnectionScoped(topLevelError, lastResponseID, currentState.turnLastResponseID) {
						connectionError = true
						terminal = false
						connectionErrorConn = clearCodexManagedWebsocketLocked(currentState)
					}
					armCodexTurnReadTimeoutLocked(exec, currentState)
					markCodexTurnFirstResponseLocked(currentState, receivedAt)
					if lastResponseID != "" {
						exec.LastResponseID = lastResponseID
						currentState.turnLastResponseID = lastResponseID
					}
					if usage != nil {
						mergeCodexTurnUsageLocked(currentState, usage)
					}
					if terminal || connectionError {
						exec.Inflight = false
						exec.State = runtimesession.SessionStateIdle
						if providerInitiatedAdmissionErr != nil {
							terminationReason = "provider_initiated_admission_failed"
							exec.MarkClosed(terminationReason)
						} else if handlerErr != nil {
							terminationReason = codexStreamTrackingErrorCode(handlerErr)
							if terminationReason == "" {
								terminationReason = "provider_protocol_error"
							}
						} else if connectionError {
							terminationReason = "provider_connection_error"
						}
						turnObserver, finalizePayload = finalizeCodexTurnLocked(exec, currentState, terminationReason, receivedAt)
					}
					exec.Touch(receivedAt)
				}
			}()
			if !ownsConn {
				return
			}
			usageErr := observeCodexTurnUsage(turnObserver, usage)
			if usageErr != nil {
				var clearedToClose codexClearedWebsocket
				if finalizePayload.TurnSeq == 0 {
					func() {
						exec.Lock()
						defer exec.Unlock()
						errorState := getCodexManagedRuntimeStateLocked(exec)
						if errorState.wsConn == conn {
							attachment = errorState.attachment
							clearedToClose = clearCodexManagedWebsocketLocked(errorState)
							turnObserver, finalizePayload = finalizeCodexTurnLocked(exec, errorState, "quota_exhausted", time.Now())
							exec.Inflight = false
							exec.State = runtimesession.SessionStateIdle
							exec.Touch(time.Now())
						}
					}()
				} else {
					func() {
						exec.Lock()
						defer exec.Unlock()
						errorState := getCodexManagedRuntimeStateLocked(exec)
						if errorState.wsConn == conn {
							clearedToClose = clearCodexManagedWebsocketLocked(errorState)
							exec.Touch(time.Now())
						}
					}()
				}
				closeCodexClearedWebsocket(clearedToClose)
				if turnObserver != nil && finalizePayload.TurnSeq > 0 {
					finishCodexTurn(exec, turnObserver, finalizePayload)
				}
				codexMaybeDeleteDetachedExecutionSession(exec, "detached_ephemeral_session")
				if attachment != nil {
					_ = enqueueCodexOutbound(attachment, codexRealtimeOutbound{
						credentials: credentials,
						messageType: messageType,
						payload:     payload,
						usage:       usage,
						origin:      runtimerealtime.RealtimePayloadOriginProvider,
						err:         codexRealtimeTurnUsageError(usageErr),
					})
				}
				return
			}
			var finalResult runtimesession.TurnFinalizationResult
			if turnObserver != nil && finalizePayload.TurnSeq > 0 {
				finalResult = finishCodexTurn(exec, turnObserver, finalizePayload)
			}
			if connectionError {
				closeCodexClearedWebsocket(connectionErrorConn)
			}
			if terminal {
				codexMaybeDeleteDetachedExecutionSession(exec, "detached_ephemeral_session")
			}

			if handlerErr != nil {
				if attachment != nil {
					errorCode := codexStreamTrackingErrorCode(handlerErr)
					if errorCode == "" {
						errorCode = "provider_protocol_error"
					}
					errorPayload := codexRealtimeProviderErrorEventPayload("", errorCode, codexRealtimeStaticErrorMessage(errorCode))
					_ = enqueueCodexOutbound(attachment, codexRealtimeOutbound{
						credentials: credentials,
						messageType: wsconn.TextMessage,
						payload:     errorPayload,
						origin:      runtimerealtime.RealtimePayloadOriginProxyLocal,
					})
				}
				return
			}
			if !shouldContinue {
				return
			}

			if attachment != nil {
				if !enqueueCodexOutbound(attachment, codexRealtimeOutbound{
					credentials: credentials,
					messageType: messageType,
					payload:     payload,
					usage:       usage,
					origin:      runtimerealtime.RealtimePayloadOriginProvider,
				}) {
					cleanupCodexExecutionSession(exec)
					return
				}
			}
			if providerInitiatedAdmissionErr != nil || finalResult.StopFutureWork {
				if attachment != nil {
					_ = enqueueCodexOutbound(attachment, codexRealtimeOutbound{
						credentials: credentials,
						providerClose: &runtimerealtime.ProviderClose{
							Code:   int(wsconn.ClosePolicyViolation),
							Reason: "principal_revoked",
							Err:    errors.Join(providerInitiatedAdmissionErr, finalResult.Err),
						},
						origin: runtimerealtime.RealtimePayloadOriginProxyLocal,
					})
				}
				cleanupCodexExecutionSession(exec)
				return
			}
			if connectionError {
				return
			}
		}
	}()
}

func codexSupplierPayloadIsTopLevelError(payload []byte) bool {
	var envelope struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(payload, &envelope) == nil && strings.TrimSpace(envelope.Type) == types.EventTypeError
}

func codexSupplierErrorIsConnectionScoped(topLevelError bool, eventResponseID, turnResponseID string) bool {
	if !topLevelError {
		return false
	}
	eventResponseID = strings.TrimSpace(eventResponseID)
	turnResponseID = strings.TrimSpace(turnResponseID)
	return eventResponseID == "" || turnResponseID != "" && eventResponseID != turnResponseID
}

func codexRealtimeOutboundFromCloseInfo(info wsconn.CloseInfo) codexRealtimeOutbound {
	if codexRealtimeCloseAsProviderClose(info.Kind) {
		return codexRealtimeOutbound{
			providerClose: &runtimerealtime.ProviderClose{
				Code:   int(info.Code),
				Reason: info.Reason,
				Err:    runtimerealtime.ErrSessionClosed,
			},
			origin: runtimerealtime.RealtimePayloadOriginProvider,
		}
	}
	return codexRealtimeOutbound{
		messageType: wsconn.TextMessage,
		payload:     codexRealtimeProviderErrorEventPayload("", "provider_connection_closed", codexRealtimeStaticErrorMessage("provider_connection_closed")),
		origin:      runtimerealtime.RealtimePayloadOriginProxyLocal,
		err:         runtimerealtime.ErrSessionClosed,
	}
}

func codexRealtimeCloseAsProviderClose(kind wsconn.CloseKind) bool {
	switch kind {
	case wsconn.CloseKindPeerClose, wsconn.CloseKindNormal, wsconn.CloseKindGracefulShutdown:
		return true
	default:
		return false
	}
}

func sendCodexRealtimeWSEventLocked(exec *runtimesession.ExecutionSession, state *codexManagedRuntimeState, payload []byte, eventID string, ownerSeq uint64, attachment *codexAttachment) error {
	if state.wsConn == nil {
		exec.Inflight = false
		exec.State = runtimesession.SessionStateIdle
		return newCodexRealtimeProviderError(eventID, "transport_unavailable", "websocket transport is unavailable")
	}

	conn := state.wsConn
	writeErr := writeCodexRealtimeWSMessageWithExecUnlocked(exec, conn, wsconn.TextMessage, payload)
	if writeErr == nil {
		return nil
	}
	if ownerSeq != 0 && !codexManagedSessionOwnsAttachmentLocked(state, ownerSeq, attachment) {
		return runtimerealtime.ErrSessionClosed
	}

	// A websocket write error cannot prove that the provider did not observe the
	// frame. Reconnecting or replaying here could repeat billable or stateful work.
	logCodexRealtimeInternalError("codex realtime websocket write failed: " + writeErr.Error())
	clearedToClose := codexClearedWebsocket{conn: conn}
	if state.wsConn == conn {
		clearedToClose = clearCodexManagedWebsocketLocked(state)
	}
	closeCodexClearedWebsocket(clearedToClose)
	exec.Inflight = false
	exec.State = runtimesession.SessionStateIdle
	return newCodexRealtimeProviderError(eventID, "ws_write_failed", codexRealtimeStaticErrorMessage("ws_write_failed"))
}

func codexCheckStaleResponsesWSContinuationLocked(state *codexManagedRuntimeState, eventID string, request *types.OpenAIResponsesRequest) error {
	if request == nil || !codexShouldFailStaleResponsesWSContinuationLocked(state, request.PreviousResponseID) {
		return nil
	}

	// Trade-off: a disconnected Codex Responses websocket cannot prove that the
	// upstream still knows the requested previous_response_id. Replaying local
	// transcript state would add billing, concurrency, and semantic ambiguity, so
	// the managed WS path fails closed and leaves affinity/cache bindings intact.
	return codexStaleResponsesWSContinuationError(eventID)
}

func codexShouldFailStaleResponsesWSContinuationLocked(state *codexManagedRuntimeState, previousResponseID string) bool {
	return state != nil &&
		strings.TrimSpace(previousResponseID) != "" &&
		state.wsConnGeneration > 0 &&
		state.wsConn == nil
}

func getCodexManagedRuntimeStateLocked(exec *runtimesession.ExecutionSession) *codexManagedRuntimeState {
	if state, ok := exec.Data.(*codexManagedRuntimeState); ok && state != nil {
		return state
	}

	state := &codexManagedRuntimeState{}
	exec.Data = state
	return state
}

func clearCodexManagedWebsocketLocked(state *codexManagedRuntimeState) codexClearedWebsocket {
	if state == nil {
		return codexClearedWebsocket{}
	}

	stopCodexTurnReadTimeoutLocked(state)
	cleared := codexClearedWebsocket{
		conn: state.wsConn,
	}
	state.wsConn = nil
	state.wsCredentials = nil
	if state.wsReaderConn == cleared.conn {
		state.wsReaderConn = nil
		state.wsReaderContext = nil
	}
	if state.skipBootstrapConn == cleared.conn {
		state.skipBootstrapConn = nil
	}
	return cleared
}

func closeCodexClearedWebsocket(cleared codexClearedWebsocket) {
	if cleared.conn != nil {
		cleared.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "codex_ws_closed"})
	}
}

func writeCodexRealtimeWSMessage(conn *wsconn.ManagedConn, messageType wsconn.MessageType, payload []byte) error {
	if conn == nil {
		return net.ErrClosed
	}
	return conn.WriteMessage(messageType, payload)
}

func writeCodexRealtimeWSMessageWithExecUnlocked(exec *runtimesession.ExecutionSession, conn *wsconn.ManagedConn, messageType wsconn.MessageType, payload []byte) error {
	if exec == nil {
		return writeCodexRealtimeWSMessage(conn, messageType, payload)
	}
	exec.Unlock()
	err := writeCodexRealtimeWSMessage(conn, messageType, payload)
	exec.Lock()
	return err
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
		currentCodexExecutionSessions().DeleteIf(exec.Key, exec)
	}
}

func newCodexAttachmentWithCapacity(capacity int) *codexAttachment {
	return newCodexAttachmentWithLimits(capacity, config.RealtimeWebsocketAttachmentQueueMaxBytes())
}

func newCodexAttachmentWithLimits(capacity int, maxBytes int64) *codexAttachment {
	if capacity <= 0 {
		capacity = codexRealtimeAttachmentQueueCapacity
	}
	return &codexAttachment{
		waitCh:              make(chan struct{}),
		queue:               make([]codexAttachmentItem, capacity),
		byteBudget:          runtimerealtime.NewByteBudget(maxBytes),
		backpressureTimeout: codexRealtimeOutboundBackpressureTimeout,
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

// takeoverTo atomically moves every not-yet-consumed outbound event to the new
// physical attachment. The old attachment is then closed so only one consumer
// can claim future delivery.
func (a *codexAttachment) takeoverTo(replacement *codexAttachment) bool {
	if a == nil || replacement == nil || a == replacement {
		return a == replacement && a != nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	replacement.mu.Lock()
	defer replacement.mu.Unlock()
	if replacement.closed || replacement.size != 0 || replacement.reserved != nil || a.size > len(replacement.queue) {
		return false
	}
	targetCredits := make([]*runtimerealtime.ByteCredit, 0, a.size+1)
	for offset := 0; offset < a.size; offset++ {
		item := a.queue[(a.head+offset)%len(a.queue)]
		credit, ok := replacement.byteBudget.TryAcquire(len(item.outbound.payload))
		if !ok {
			for _, acquired := range targetCredits {
				acquired.Release()
			}
			return false
		}
		targetCredits = append(targetCredits, credit)
	}
	if a.reserved != nil {
		credit, ok := replacement.byteBudget.TryAcquire(len(a.reserved.outbound.payload))
		if !ok {
			for _, acquired := range targetCredits {
				acquired.Release()
			}
			return false
		}
		targetCredits = append(targetCredits, credit)
	}
	creditIndex := 0
	for a.size > 0 {
		item := a.queue[a.head]
		a.queue[a.head] = codexAttachmentItem{}
		a.head = (a.head + 1) % len(a.queue)
		a.size--
		item.release()
		item.credit = targetCredits[creditIndex]
		creditIndex++
		tail := (replacement.head + replacement.size) % len(replacement.queue)
		replacement.queue[tail] = item
		replacement.size++
	}
	if a.reserved != nil {
		item := *a.reserved
		item.release()
		item.credit = targetCredits[creditIndex]
		replacement.reserved = &item
	}
	replacement.reservedAfter = a.reservedAfter
	a.reserved = nil
	a.reservedAfter = 0
	a.closed = true
	a.signalLocked()
	if replacement.size > 0 || replacement.reserved != nil {
		replacement.signalLocked()
	}
	return true
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
	return a.recvWithStop(ctx, nil)
}

func (a *codexAttachment) recvWithStop(ctx context.Context, stop <-chan struct{}) (codexRealtimeOutbound, error) {
	if a == nil {
		return codexRealtimeOutbound{}, runtimerealtime.ErrSessionClosed
	}

	for {
		a.mu.Lock()
		if a.reserved != nil && a.reservedAfter == 0 {
			item := *a.reserved
			a.reserved = nil
			item.release()
			a.signalLocked()
			a.mu.Unlock()
			return item.outbound, nil
		}
		if a.size > 0 {
			item := a.queue[a.head]
			a.queue[a.head] = codexAttachmentItem{}
			a.head = (a.head + 1) % len(a.queue)
			a.size--
			item.release()
			if a.reserved != nil && a.reservedAfter > 0 {
				a.reservedAfter--
			}
			a.signalLocked()
			a.mu.Unlock()
			return item.outbound, nil
		}
		if a.closed {
			a.mu.Unlock()
			return codexRealtimeOutbound{}, runtimerealtime.ErrSessionClosed
		}
		waitCh := a.waitCh
		a.mu.Unlock()

		select {
		case <-ctx.Done():
			return codexRealtimeOutbound{}, ctx.Err()
		case <-stop:
			return codexRealtimeOutbound{}, runtimerealtime.ErrSessionClosed
		case <-waitCh:
		}
	}
}

func (a *codexAttachment) signalLocked() {
	close(a.waitCh)
	a.waitCh = make(chan struct{})
}

func enqueueCodexOutbound(attachment *codexAttachment, outbound codexRealtimeOutbound) bool {
	for _, current := range normalizeCodexRealtimeOutbound(outbound) {
		if !enqueueCodexSingleOutbound(attachment, current) {
			return false
		}
	}
	return true
}

func normalizeCodexRealtimeOutbound(outbound codexRealtimeOutbound) []codexRealtimeOutbound {
	hasPayload := len(outbound.payload) > 0
	hasUsage := outbound.usage != nil
	hasErr := outbound.err != nil
	if outbound.providerClose != nil && (hasPayload || hasUsage || hasErr) {
		return []codexRealtimeOutbound{{
			providerClose: outbound.providerClose,
			origin:        outbound.origin,
		}}
	}
	if !hasErr || !hasUsage {
		return []codexRealtimeOutbound{outbound}
	}
	errEvent := codexRealtimeOutbound{
		origin: outbound.origin,
		err:    outbound.err,
	}
	if payload := runtimerealtime.ClientPayloadFromError(outbound.err); len(payload) > 0 {
		errEvent.messageType = wsconn.TextMessage
		errEvent.payload = payload
		errEvent.origin = runtimerealtime.RealtimePayloadOriginProxyLocal
	}
	outbound.err = nil
	return []codexRealtimeOutbound{outbound, errEvent}
}

func enqueueCodexSingleOutbound(attachment *codexAttachment, outbound codexRealtimeOutbound) bool {
	if attachment == nil {
		return false
	}

	var timer *time.Timer
	var timerC <-chan time.Time
	if attachment.backpressureTimeout > 0 {
		timer = time.NewTimer(attachment.backpressureTimeout)
		defer timer.Stop()
		timerC = timer.C
	}

	for {
		attachment.mu.Lock()
		if attachment.closed {
			attachment.mu.Unlock()
			return false
		}
		if int64(len(outbound.payload)) > attachment.byteBudget.Limit() {
			attachment.closed = true
			attachment.signalLocked()
			attachment.mu.Unlock()
			return false
		}
		if attachment.size < len(attachment.queue) {
			credit, ok := attachment.byteBudget.TryAcquire(len(outbound.payload))
			if !ok {
				waitCh := attachment.waitCh
				attachment.mu.Unlock()
				if !waitCodexAttachmentCapacity(waitCh, timerC) {
					attachment.close()
					return false
				}
				continue
			}
			tail := (attachment.head + attachment.size) % len(attachment.queue)
			attachment.queue[tail] = codexAttachmentItem{outbound: outbound, credit: credit}
			attachment.size++
			attachment.signalLocked()
			attachment.mu.Unlock()
			return true
		}
		if attachment.reserved == nil && codexRealtimeOutboundClosureRelevant(outbound) {
			credit, ok := attachment.byteBudget.TryAcquire(len(outbound.payload))
			if !ok {
				waitCh := attachment.waitCh
				attachment.mu.Unlock()
				if !waitCodexAttachmentCapacity(waitCh, timerC) {
					attachment.close()
					return false
				}
				continue
			}
			reserved := codexAttachmentItem{outbound: outbound, credit: credit}
			attachment.reserved = &reserved
			attachment.reservedAfter = attachment.size
			attachment.signalLocked()
			attachment.mu.Unlock()
			return true
		}
		waitCh := attachment.waitCh
		attachment.mu.Unlock()

		if !waitCodexAttachmentCapacity(waitCh, timerC) {
			attachment.close()
			return false
		}
	}
}

func waitCodexAttachmentCapacity(waitCh <-chan struct{}, timerC <-chan time.Time) bool {
	if timerC == nil {
		<-waitCh
		return true
	}
	select {
	case <-waitCh:
		return true
	case <-timerC:
		return false
	}
}

func codexRealtimeOutboundClosureRelevant(outbound codexRealtimeOutbound) bool {
	if outbound.usage != nil || outbound.err != nil || outbound.providerClose != nil {
		return true
	}
	if outbound.messageType != wsconn.TextMessage || len(outbound.payload) == 0 {
		return false
	}
	terminal, _, _ := inspectCodexSupplierPayload(outbound.payload)
	if terminal {
		return true
	}
	var envelope struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(outbound.payload, &envelope) == nil && strings.TrimSpace(envelope.Type) == types.EventTypeError
}

func isCodexRealtimeBootstrapMessage(messageType wsconn.MessageType, payload []byte) bool {
	// wsconn.MessageType belongs at this Realtime websocket boundary. Adapters
	// that already own framing call isCodexSupplierBootstrapPayload directly.
	if messageType != wsconn.TextMessage {
		return false
	}
	return isCodexSupplierBootstrapPayload(payload)
}

func beginCodexTurnLocked(state *codexManagedRuntimeState, now time.Time, bases ...context.Context) {
	if state == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}

	state.turnSeq++
	state.turnModels = state.models
	state.turnStartedAt = now
	state.turnFirstResponseAt = time.Time{}
	state.turnCompletedAt = time.Time{}
	state.turnLastResponseID = ""
	state.turnTerminationReason = ""
	state.turnUsage = &types.UsageEvent{}
	state.turnFinalized = false
	state.turnFinalizing = false
	if state.turnCancel != nil {
		state.turnCancel()
	}
	base := context.Background()
	if len(bases) > 0 && bases[0] != nil {
		base = context.WithoutCancel(bases[0])
	}
	state.turnAccumulator = newCodexTurnUsageAccumulator()
	state.turnContext, state.turnCancel = context.WithCancel(base)
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
	if state.turnCancel != nil {
		state.turnCancel()
	}
	state.turnContext = nil
	state.turnCancel = nil
	stopCodexTurnReadTimeoutLocked(state)
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
	stopCodexTurnReadTimeoutLocked(state)
	if now.IsZero() {
		now = time.Now()
	}
	if state.turnStartedAt.IsZero() {
		state.turnStartedAt = now
	}

	state.turnFinalized = true
	state.turnFinalizing = state.turnObserver != nil
	if state.turnCancel != nil {
		state.turnCancel()
		state.turnCancel = nil
		state.turnContext = nil
	}
	state.turnCompletedAt = now
	if trimmed := strings.TrimSpace(reason); trimmed != "" {
		state.turnTerminationReason = trimmed
	}
	if state.turnLastResponseID != "" {
		exec.LastResponseID = state.turnLastResponseID
	}

	models := state.turnModels
	if models.BillingModel == "" {
		models = runtimesession.ModelBinding{RequestedModel: exec.Model, ProviderModel: exec.Model, BillingModel: exec.Model}
	}
	if state.turnUsage != nil && state.turnUsage.ResponseModel != "" {
		models.ReportedModel = state.turnUsage.ResponseModel
	}
	return state.turnObserver, runtimesession.TurnFinalizePayload{
		SessionID:         exec.SessionID,
		Model:             models.BillingModel,
		Models:            models,
		WorkID:            fmt.Sprintf("response:%d", state.turnSeq),
		TurnSeq:           state.turnSeq,
		LastResponseID:    state.turnLastResponseID,
		TerminationReason: state.turnTerminationReason,
		StartedAt:         state.turnStartedAt,
		FirstResponseAt:   state.turnFirstResponseAt,
		CompletedAt:       state.turnCompletedAt,
		Usage:             state.turnUsage.Clone(),
	}
}

// 结算结束前不开始下一轮；未结清的结果不能被当作未来工作的许可。
func finishCodexTurn(exec *runtimesession.ExecutionSession, observer runtimesession.TurnObserver, payload runtimesession.TurnFinalizePayload) runtimesession.TurnFinalizationResult {
	observer.FinalizeTurn(payload)
	result := runtimesession.TurnResult(observer)
	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	if state.turnSeq == payload.TurnSeq {
		state.turnFinalizing = false
	}
	if result.StopFutureWork || result.Unsettled || result.Err != nil {
		exec.MarkClosed("realtime_work_stopped")
		result.StopFutureWork = true
	}
	exec.Unlock()
	return result
}

func armCodexTurnReadTimeoutLocked(exec *runtimesession.ExecutionSession, state *codexManagedRuntimeState) {
	if exec == nil || state == nil || state.wsConn == nil || state.turnSeq == 0 || state.turnFinalized || codexRealtimeTurnReadTimeout <= 0 {
		return
	}
	conn := state.wsConn
	connGeneration := state.wsConnGeneration
	turnSeq := state.turnSeq
	state.turnReadGen++
	timerGeneration := state.turnReadGen
	if state.turnReadTimer != nil {
		state.turnReadTimer.Stop()
	}
	state.turnReadTimer = time.AfterFunc(codexRealtimeTurnReadTimeout, func() {
		handleCodexTurnReadTimeout(exec, conn, connGeneration, turnSeq, timerGeneration)
	})
}

func stopCodexTurnReadTimeoutLocked(state *codexManagedRuntimeState) {
	if state == nil {
		return
	}
	state.turnReadGen++
	if state.turnReadTimer != nil {
		state.turnReadTimer.Stop()
		state.turnReadTimer = nil
	}
}

func handleCodexTurnReadTimeout(exec *runtimesession.ExecutionSession, conn *wsconn.ManagedConn, connGeneration uint64, turnSeq int64, timerGeneration int64) {
	if exec == nil || conn == nil {
		return
	}
	shouldClose := false
	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	if state.wsConn == conn &&
		state.wsConnGeneration == connGeneration &&
		state.turnSeq == turnSeq &&
		state.turnReadGen == timerGeneration &&
		!state.turnFinalized &&
		exec.Inflight {
		shouldClose = true
	}
	exec.Unlock()
	if shouldClose {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "turn_read_timeout"})
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
		return runtimerealtime.NewClientPayloadError(event, []byte(event.Error()))
	}

	logCodexRealtimeInternalError("codex realtime turn usage error: " + err.Error())
	message := codexRealtimeStaticErrorMessage("system_error")
	if strings.Contains(strings.ToLower(err.Error()), "quota") {
		message = "user quota is not enough"
	}
	event = types.NewErrorEvent("", "system_error", "system_error", message)
	return runtimerealtime.NewClientPayloadError(event, []byte(event.Error()))
}

func inspectCodexSupplierMessage(messageType wsconn.MessageType, payload []byte) (bool, string, string) {
	// wsconn.MessageType belongs at the concrete websocket boundary. Adapters
	// that already own framing should call inspectCodexSupplierPayload directly.
	if messageType != wsconn.TextMessage {
		return false, "", ""
	}
	return inspectCodexSupplierPayload(payload)
}

func (p *CodexProvider) prepareCodexRealtimeCreatePayload(payload []byte, models runtimesession.ModelBinding) (string, *types.OpenAIResponsesRequest, []byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		logCodexRealtimeInternalError("codex realtime response.create envelope decode failed: " + err.Error())
		return "", nil, nil, newCodexRealtimeClientError("", "invalid_event", codexRealtimeStaticErrorMessage("invalid_event"))
	}
	eventID := codexRawString(envelope["event_id"])
	if err := commonresponses.ValidateNoAccountScopedResourcesJSON(payload); err != nil {
		return eventID, nil, nil, newCodexRealtimeClientError(eventID, "unsupported_resource_reference", err.Error())
	}

	var request types.OpenAIResponsesRequest
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		logCodexRealtimeInternalError("codex realtime response.create request decode failed: " + err.Error())
		return eventID, nil, nil, newCodexRealtimeClientError(eventID, "invalid_event", codexRealtimeStaticErrorMessage("invalid_event"))
	}
	if strings.TrimSpace(request.Model) == "" {
		return eventID, nil, nil, newCodexRealtimeClientError(eventID, "invalid_event", "model is required in response.create payload")
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.Model != models.RequestedModel {
		return eventID, nil, nil, newCodexRealtimeClientError(eventID, "session_model_mismatch", "execution session model mismatch")
	}
	if models.ProviderModel != models.RequestedModel {
		patched, err := replaceCodexRealtimeModel(payload, models.ProviderModel)
		if err != nil {
			return eventID, nil, nil, err
		}
		payload = patched
		request.Model = models.ProviderModel
	}
	return eventID, &request, payload, nil
}

// 只替换代理负责的模型字段，其余字节（包括未知重复键）保持原样。
func replaceCodexRealtimeModel(payload []byte, modelName string) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	start, end := int64(-1), int64(0)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if key == "model" {
			if start >= 0 {
				return nil, newCodexRealtimeClientError("", "invalid_event", "model must appear once when mapped")
			}
			end = decoder.InputOffset()
			start = end - int64(len(value))
		}
	}
	if start < 0 {
		return nil, newCodexRealtimeClientError("", "invalid_event", "model is required")
	}
	value, err := json.Marshal(modelName)
	if err != nil {
		return nil, err
	}
	result := append([]byte(nil), payload[:start]...)
	result = append(result, value...)
	return append(result, payload[end:]...), nil
}

func codexRawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return strings.TrimSpace(value)
}
