package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/common/responsesws"
	"one-api/internal/billing"
	"one-api/metrics"
	"one-api/middleware"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/relay/relay_util"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/spf13/viper"
)

type ResponsesWSSendOutcome int

const (
	SendOutcomeUnknown ResponsesWSSendOutcome = iota
	SendOutcomeNotSent
	SendOutcomeLocalWriteOK
	SendOutcomeAmbiguous
)

type BillingEvidence int

const (
	BillingEvidenceNone BillingEvidence = iota
	BillingEvidenceProviderUsageSeen
	BillingEvidenceProviderAcceptedTurnEvidence
)

const (
	responsesWSEventQueueSize           = 128
	responsesWSSendQueueSize            = 64
	responsesWSPendingProviderEventsMax = 32
	responsesWSBusyRejectLimit          = 16
	responsesWSBusyRejectWindow         = 10 * time.Second
)

var errResponsesWSSendQueueFull = errors.New("responses websocket upstream send queue is full")

type ResponsesWSEvent interface{ responsesWSEvent() }

type ResponsesWSEventClientFrame struct {
	MessageType int
	Payload     []byte
	ReceivedAt  time.Time
}

func (ResponsesWSEventClientFrame) responsesWSEvent() {}

type ResponsesWSEventSendResult struct {
	AttemptID         string
	SelectedChannelID int
	Outcome           ResponsesWSSendOutcome
	Err               error
}

func (ResponsesWSEventSendResult) responsesWSEvent() {}

type ResponsesWSProviderDownstreamKind int

const (
	ProviderDownstreamFrame ResponsesWSProviderDownstreamKind = iota
	ProviderDownstreamUsage
	ProviderDownstreamRecvError
)

type ResponsesWSEventProxyLocalError struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	Payload                   []byte
	Recoverable               bool
}

func (ResponsesWSEventProxyLocalError) responsesWSEvent() {}

type ResponsesWSEventProviderDownstream struct {
	UpstreamSessionGeneration string
	ChannelID                 int
	Kind                      ResponsesWSProviderDownstreamKind
	MessageType               int
	Payload                   []byte
	Usage                     *types.UsageEvent
	Err                       error
	Origin                    runtimesession.RealtimePayloadOrigin
	ReceivedAt                time.Time
}

func (ResponsesWSEventProviderDownstream) responsesWSEvent() {}

type ResponsesWSEventClientClosed struct{ Err error }

func (ResponsesWSEventClientClosed) responsesWSEvent() {}

type ResponsesWSEventFirstTurnSetup struct {
	Frame        *responsesws.RawResponsesCreateFrame
	PendingLease middleware.ResponsesWSLease
	ReceivedAt   time.Time
}

func (ResponsesWSEventFirstTurnSetup) responsesWSEvent() {}

type ResponsesWSEventFirstTurnOpenResult struct {
	OpeningID  string
	Snapshot   *ResponsesWSRequestSnapshot
	OpenResult *responsesWSOpenResult
	Err        *types.OpenAIErrorWithStatusCode
	Adopted    chan bool
}

func (ResponsesWSEventFirstTurnOpenResult) responsesWSEvent() {}

type ResponsesWSEventTimeout struct {
	Reason                    string
	UpstreamSessionGeneration string
	ChannelID                 int
}

func (ResponsesWSEventTimeout) responsesWSEvent() {}

type ResponsesWSEventCloseIntent struct {
	Reason string
}

func (ResponsesWSEventCloseIntent) responsesWSEvent() {}

type ResponsesWSTurnAdmission struct {
	RPMAllowed bool
}

func NewResponsesWSTurnAdmission() *ResponsesWSTurnAdmission {
	return &ResponsesWSTurnAdmission{}
}

func (a *ResponsesWSTurnAdmission) AllowRPMOnce(allow func() *types.OpenAIErrorWithStatusCode) *types.OpenAIErrorWithStatusCode {
	if a == nil {
		return common.StringErrorWrapperLocal("turn admission is required", "responses_ws_admission_missing", http.StatusInternalServerError)
	}
	if a.RPMAllowed {
		return nil
	}
	if allow == nil {
		return common.StringErrorWrapperLocal("request limiter is not configured", "rate_limiter_missing", http.StatusInternalServerError)
	}
	if err := allow(); err != nil {
		return err
	}
	a.RPMAllowed = true
	return nil
}

type ResponsesWSTurnAttemptInput struct {
	Context           *gin.Context
	Snapshot          *ResponsesWSRequestSnapshot
	OpeningID         string
	Admission         *ResponsesWSTurnAdmission
	Candidate         *ResponsesTurnAffinity
	SelectedChannelID int
	Session           runtimesession.RealtimeSession
	BillingModel      string
	PromptModel       string
	Request           *types.OpenAIResponsesRequest
	StartedAt         time.Time
}

type responsesWSOpenResult struct {
	Session       runtimesession.RealtimeSession
	Provider      providersBase.ProviderInterface
	ProviderModel string
	BillingModel  string
	Channel       *model.Channel
	Candidate     *ResponsesTurnAffinity
}

type SelectedChannelSnapshot struct {
	ChannelID            int
	ChannelType          int
	PreCost              int
	ProviderModel        string
	BillingModel         string
	OriginalModel        string
	BillingOriginalModel bool
	Channel              *model.Channel
}

var openAndPrimeResponsesWSSessionForActor = openAndPrimeResponsesWSSessionWithContext

type ResponsesWSTurnAttempt struct {
	OpeningID              string
	AttemptID              string
	Admission              *ResponsesWSTurnAdmission
	Candidate              *ResponsesTurnAffinity
	SelectedChannelID      int
	Session                runtimesession.RealtimeSession
	Quota                  *relay_util.Quota
	QuotaPreconsumed       bool
	PreconsumeAttempted    bool
	PreconsumeTruthApplied bool
	PreconsumeCacheApplied bool
	QuotaFinalized         bool
	RolledBack             bool
	RollbackErr            error
	QuotaEventSinkAttached bool
	CandidateBegun         bool
	SendOutcome            ResponsesWSSendOutcome
	Usage                  *types.Usage
	BillingEvidence        BillingEvidence
	StartedAt              time.Time
	FirstResponseAt        time.Time
	CompletedAt            time.Time
	snapshot               *ResponsesWSRequestSnapshot
	providerAPIErrorKeys   map[string]struct{}
}

func PrepareResponsesWSTurnAttempt(input ResponsesWSTurnAttemptInput) (*ResponsesWSTurnAttempt, *types.OpenAIErrorWithStatusCode) {
	if input.Context == nil || input.Request == nil {
		return nil, common.StringErrorWrapperLocal("responses websocket turn context is required", "invalid_request_error", http.StatusBadRequest)
	}
	promptModel := strings.TrimSpace(input.PromptModel)
	if promptModel == "" {
		promptModel = input.BillingModel
	}
	channelPreCost := 0
	if channel, ok := input.Context.Get("responses_ws_selected_channel"); ok {
		if typed, ok := channel.(*model.Channel); ok && typed != nil {
			channelPreCost = typed.PreCost
		}
	}
	promptTokens := common.CountTokenInputMessages(input.Request.Input, promptModel, channelPreCost)
	// The local prompt estimate is only the quota pre-consume ledger. Final
	// billing usage starts empty and is filled from provider usage/terminal events.
	usage := &types.Usage{}
	snapshot := input.Snapshot
	if snapshot == nil {
		snapshot = NewResponsesWSRequestSnapshot(input.Context)
	}
	startedAt := input.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	quota := relay_util.NewQuota(input.Context, input.BillingModel, promptTokens)
	quota.SetLogProtocol(relay_util.LogProtocolResponsesWS)
	return &ResponsesWSTurnAttempt{
		OpeningID:         input.OpeningID,
		Admission:         input.Admission,
		Candidate:         input.Candidate,
		SelectedChannelID: input.SelectedChannelID,
		Session:           input.Session,
		Quota:             quota,
		Usage:             usage,
		StartedAt:         startedAt,
		snapshot:          snapshot.Clone(),
	}, nil
}

func (a *ResponsesWSTurnAttempt) Context() *gin.Context {
	if a == nil || a.snapshot == nil {
		return nil
	}
	return a.snapshot.Context()
}

func (a *ResponsesWSTurnAttempt) BeginCandidate(actor *ResponsesWSSessionActor) error {
	if a == nil || actor == nil {
		return errors.New("responses websocket attempt and actor are required")
	}
	if a.CandidateBegun {
		return nil
	}
	a.AttemptID = uuid.NewString()
	a.CandidateBegun = true
	actor.pendingAttempt = a
	actor.pendingProviderEvents = nil
	actor.pendingProviderBytes = 0
	actor.pendingProviderEvidenceSeen = false
	return nil
}

func (a *ResponsesWSTurnAttempt) PreConsumeQuota() *types.OpenAIErrorWithStatusCode {
	if a == nil || a.Quota == nil {
		return common.StringErrorWrapperLocal("quota transaction is required", "quota_transaction_missing", http.StatusInternalServerError)
	}
	err := a.Quota.PreQuotaConsumptionRollbackable()
	a.PreconsumeAttempted = true
	if a.Quota.HasPreConsumedSideEffect() {
		a.QuotaPreconsumed = true
		a.PreconsumeTruthApplied = a.Quota.PreconsumeTruthApplied
		a.PreconsumeCacheApplied = a.Quota.PreconsumeCacheApplied
	}
	return err
}

func (a *ResponsesWSTurnAttempt) CommitLocalWriteOK() {
	if a == nil {
		return
	}
	a.SendOutcome = SendOutcomeLocalWriteOK
}

func (a *ResponsesWSTurnAttempt) CommitAmbiguousAdmission(reason string) {
	if a == nil {
		return
	}
	_ = reason
	a.SendOutcome = SendOutcomeAmbiguous
}

func (a *ResponsesWSTurnAttempt) MarkProviderUsageSeen() {
	if a == nil {
		return
	}
	a.BillingEvidence = BillingEvidenceProviderUsageSeen
}

func (a *ResponsesWSTurnAttempt) MarkProviderAcceptedTurnEvidence() {
	if a == nil || a.BillingEvidence == BillingEvidenceProviderUsageSeen {
		return
	}
	a.BillingEvidence = BillingEvidenceProviderAcceptedTurnEvidence
}

func (a *ResponsesWSTurnAttempt) MarkFirstProviderResponse(now time.Time) {
	if a == nil || !a.FirstResponseAt.IsZero() {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	a.FirstResponseAt = now
}

func (a *ResponsesWSTurnAttempt) MarkCompleted(now time.Time) {
	if a == nil || !a.CompletedAt.IsZero() {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	a.CompletedAt = now
}

func (a *ResponsesWSTurnAttempt) SeedQuotaTiming(now time.Time) {
	if a == nil || a.Quota == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if a.CompletedAt.IsZero() {
		a.MarkCompleted(now)
	}
	a.Quota.SeedTiming(a.StartedAt, a.FirstResponseAt, a.CompletedAt)
}

func (a *ResponsesWSTurnAttempt) RollbackBeforeLocalWriteOK(reason string) error {
	if a == nil {
		return nil
	}
	_ = reason
	if a.QuotaPreconsumed && a.Quota != nil {
		ctx := a.Context()
		if err := a.Quota.UndoSynchronously(ctx); err != nil {
			a.RollbackErr = err
			logCtx := context.Background()
			if ctx != nil && ctx.Request != nil {
				logCtx = ctx.Request.Context()
			}
			logger.LogError(logCtx, "responses websocket quota rollback failed: "+err.Error())
			return err
		}
	}
	a.QuotaPreconsumed = false
	a.PreconsumeTruthApplied = false
	a.PreconsumeCacheApplied = false
	a.RolledBack = true
	a.RollbackErr = nil
	return nil
}

func (a *ResponsesWSTurnAttempt) FinalizeQuota(c *gin.Context) {
	if c == nil {
		c = a.Context()
	}
	a.finalizeQuota(c, false)
}

func (a *ResponsesWSTurnAttempt) FinalizeQuotaPreservingPreConsumed(c *gin.Context) {
	if c == nil {
		c = a.Context()
	}
	a.finalizeQuota(c, true)
}

func (a *ResponsesWSTurnAttempt) finalizeQuota(c *gin.Context, preservePreConsumed bool) {
	if a == nil || a.Quota == nil || a.QuotaFinalized || a.RolledBack {
		return
	}
	hasUsage := responsesWSUsageHasBillableEvidence(a.Usage)
	identity := a.settlementIdentity()
	a.SeedQuotaTiming(time.Now())
	if preservePreConsumed && !hasUsage {
		// Trade-off: once a turn has reached a send/active state, absence of a
		// terminal usage frame is not proof that the provider did no work. Keep
		// the pre-consumed floor to avoid making ambiguous completed work free;
		// explicit NotSent/pre-send paths still rollback before finalization.
		if err := a.Quota.ConsumeAtLeastPreConsumedWithIdentity(c, a.Usage, true, billing.SettlementRequestKindRealtimeTurn, identity, identity != ""); err != nil {
			logger.LogError(context.Background(), "responses websocket quota floor settlement failed: "+err.Error())
		}
	} else {
		if err := a.Quota.ConsumeWithIdentity(c, a.Usage, true, billing.SettlementRequestKindRealtimeTurn, identity, identity != ""); err != nil {
			logger.LogError(context.Background(), "responses websocket quota settlement failed: "+err.Error())
		}
	}
	a.QuotaFinalized = true
}

func (a *ResponsesWSTurnAttempt) settlementIdentity() string {
	if a == nil {
		return ""
	}
	if a.AttemptID == "" {
		a.AttemptID = uuid.NewString()
	}
	parts := make([]string, 0, 3)
	if a.OpeningID != "" {
		parts = append(parts, "opening="+a.OpeningID)
	}
	if a.AttemptID != "" {
		parts = append(parts, "attempt="+a.AttemptID)
	}
	if a.SelectedChannelID != 0 {
		parts = append(parts, fmt.Sprintf("channel=%d", a.SelectedChannelID))
	}
	return strings.Join(parts, "|") + "|finalize"
}

type RealtimeUserConn interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(int, []byte) error
	SetReadLimit(int64)
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
	Close() error
}

type RealtimeControlFrameConn interface {
	PingHandler() func(string) error
	SetPingHandler(func(string) error)
	PongHandler() func(string) error
	SetPongHandler(func(string) error)
}

type RealtimeProxyLocalReadError interface {
	error
	ProxyLocalPayload() []byte
	Recoverable() bool
}

type ResponsesWSWriteMode int

const (
	ResponsesWSWriteProvider ResponsesWSWriteMode = iota
	ResponsesWSWriteProxyLocal
)

type responsesWSSendCommand struct {
	AttemptID         string
	SelectedChannelID int
	Session           runtimesession.RealtimeSession
	MessageType       int
	Payload           []byte
}

func recoverResponsesWSGoroutine(label string, onPanic func(reason string)) {
	if recovered := recover(); recovered != nil {
		reason := "responses_ws_" + strings.TrimSpace(label) + "_panic"
		logger.SysError(fmt.Sprintf("responses websocket %s panic: %v", label, recovered))
		logger.SysError(fmt.Sprintf("stacktrace from panic: %s", string(debug.Stack())))
		if onPanic != nil {
			onPanic(reason)
		}
	}
}

type ResponsesWSIOBridge struct {
	ctx          context.Context
	cancel       context.CancelFunc
	conn         RealtimeUserConn
	writer       *requester.WSClientWriter
	actor        *ResponsesWSSessionActor
	armed        sync.Map
	sendCommands chan responsesWSSendCommand
	sendOnce     sync.Once
	wg           sync.WaitGroup
}

func NewResponsesWSIOBridge(conn RealtimeUserConn, actor *ResponsesWSSessionActor) *ResponsesWSIOBridge {
	ctx, cancel := context.WithCancel(context.Background())
	bridge := &ResponsesWSIOBridge{
		ctx:          ctx,
		cancel:       cancel,
		conn:         conn,
		actor:        actor,
		sendCommands: make(chan responsesWSSendCommand, responsesWSSendQueueSize),
	}
	if conn != nil {
		bridge.writer = requester.NewWSClientWriter(conn, config.RealtimeWebsocketWriteTimeout)
	}
	bridge.configureConn()
	return bridge
}

func (b *ResponsesWSIOBridge) configureConn() {
	if b == nil || b.conn == nil {
		return
	}
	requester.ApplyWSReadLimit(b.conn, config.RealtimeWebsocketReadLimit)
	b.refreshReadDeadline()
	if control, ok := b.conn.(RealtimeControlFrameConn); ok {
		requester.InstallWSActivityHandlers(control, func() {
			if b.actor != nil {
				b.actor.markActivity()
			}
			b.refreshReadDeadline()
		})
	}
}

func (b *ResponsesWSIOBridge) StartClientReadPump() {
	if b == nil || b.conn == nil || b.actor == nil {
		return
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer recoverResponsesWSGoroutine("client_read_pump", func(reason string) {
			if b.actor != nil {
				err := errors.New(reason)
				b.actor.markClientClosed(err)
				b.actor.PostReliable(ResponsesWSEventClientClosed{Err: err})
			}
		})
		for {
			mt, payload, err := b.conn.ReadMessage()
			receivedAt := time.Now()
			if err != nil {
				if errors.Is(err, websocket.ErrReadLimit) {
					b.actor.PostReliable(ResponsesWSEventProxyLocalError{
						Payload:     responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", "frame is too large or invalid; send smaller audio chunks"),
						Recoverable: false,
					})
					b.actor.markClientClosed(err)
					return
				}
				var proxyLocalErr RealtimeProxyLocalReadError
				if errors.As(err, &proxyLocalErr) {
					b.actor.PostReliable(ResponsesWSEventProxyLocalError{
						Payload:     proxyLocalErr.ProxyLocalPayload(),
						Recoverable: proxyLocalErr.Recoverable(),
					})
					if proxyLocalErr.Recoverable() {
						continue
					}
					b.actor.markClientClosed(err)
					return
				}
				b.actor.markClientClosed(err)
				b.actor.PostReliable(ResponsesWSEventClientClosed{Err: err})
				return
			}
			b.actor.markActivity()
			b.actor.PostReliable(ResponsesWSEventClientFrame{MessageType: mt, Payload: payload, ReceivedAt: receivedAt})
		}
	}()
}

func (b *ResponsesWSIOBridge) StartClientPingLoop() {
	if b == nil || b.conn == nil || b.writer == nil {
		return
	}
	interval := config.RealtimeWebsocketPingInterval()
	if interval <= 0 {
		return
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer recoverResponsesWSGoroutine("client_ping_loop", func(reason string) {
			if b.actor != nil {
				b.actor.PostReliable(ResponsesWSEventClientClosed{Err: errors.New(reason)})
			}
		})
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-b.ctx.Done():
				return
			case <-ticker.C:
				if err := b.writer.WriteControl(websocket.PingMessage, nil); err != nil {
					if b.actor != nil {
						b.actor.markClientClosed(err)
						b.actor.PostReliable(ResponsesWSEventClientClosed{Err: err})
					}
					return
				}
			}
		}
	}()
}

func (b *ResponsesWSIOBridge) ArmProviderRecvPump(upstreamSessionGeneration string, selectedChannelID int, session runtimesession.RealtimeSession) {
	if b == nil || session == nil || upstreamSessionGeneration == "" {
		return
	}
	if _, loaded := b.armed.LoadOrStore(upstreamSessionGeneration, struct{}{}); loaded {
		return
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.armed.Delete(upstreamSessionGeneration)
		defer recoverResponsesWSGoroutine("provider_recv_pump", func(reason string) {
			if b.actor != nil {
				b.actor.PostReliable(ResponsesWSEventTimeout{
					Reason:                    reason,
					UpstreamSessionGeneration: upstreamSessionGeneration,
					ChannelID:                 selectedChannelID,
				})
			}
		})
		for {
			mt, payload, usage, origin, err := session.Recv(b.ctx)
			receivedAt := time.Now()
			if responsesWSRecvHasProviderActivity(payload, usage, err) {
				b.actor.markActivity()
			}
			if usage != nil {
				b.actor.PostReliable(ResponsesWSEventProviderDownstream{
					UpstreamSessionGeneration: upstreamSessionGeneration,
					ChannelID:                 selectedChannelID,
					Kind:                      ProviderDownstreamUsage,
					Usage:                     usage,
					Origin:                    runtimesession.RealtimePayloadOriginProvider,
					ReceivedAt:                receivedAt,
				})
			}
			if err != nil {
				deliveredPayload := len(payload) > 0
				if len(payload) > 0 {
					kind := ProviderDownstreamRecvError
					if origin == runtimesession.RealtimePayloadOriginProvider {
						kind = ProviderDownstreamFrame
					}
					b.actor.PostReliable(ResponsesWSEventProviderDownstream{
						UpstreamSessionGeneration: upstreamSessionGeneration,
						ChannelID:                 selectedChannelID,
						Kind:                      kind,
						MessageType:               mt,
						Payload:                   payload,
						Err:                       err,
						Origin:                    origin,
						ReceivedAt:                receivedAt,
					})
				}
				deliveredClientErrorPayload := false
				if errorPayload := runtimesession.ClientPayloadFromError(err); len(errorPayload) > 0 {
					deliveredClientErrorPayload = len(payload) > 0 && bytes.Equal(errorPayload, payload)
					if !deliveredClientErrorPayload {
						deliveredClientErrorPayload = true
						b.actor.PostReliable(ResponsesWSEventProxyLocalError{
							UpstreamSessionGeneration: upstreamSessionGeneration,
							ChannelID:                 selectedChannelID,
							Payload:                   errorPayload,
							Recoverable:               false,
						})
					}
				}
				if !deliveredPayload && !deliveredClientErrorPayload {
					b.actor.PostReliable(ResponsesWSEventTimeout{
						Reason:                    "provider_closed",
						UpstreamSessionGeneration: upstreamSessionGeneration,
						ChannelID:                 selectedChannelID,
					})
				}
				return
			}
			if len(payload) == 0 {
				continue
			}
			b.actor.PostReliable(ResponsesWSEventProviderDownstream{
				UpstreamSessionGeneration: upstreamSessionGeneration,
				ChannelID:                 selectedChannelID,
				Kind:                      ProviderDownstreamFrame,
				MessageType:               mt,
				Payload:                   payload,
				Origin:                    runtimesession.RealtimePayloadOriginProvider,
				ReceivedAt:                receivedAt,
			})
		}
	}()
}

func responsesWSRecvHasProviderActivity(payload []byte, usage *types.UsageEvent, err error) bool {
	if usage != nil || len(payload) > 0 {
		return true
	}
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, runtimesession.ErrSessionClosed)
}

func (b *ResponsesWSIOBridge) startSendWorker() {
	if b == nil {
		return
	}
	b.sendOnce.Do(func() {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer recoverResponsesWSGoroutine("send_worker", func(reason string) {
				if b.actor != nil {
					b.actor.PostReliable(ResponsesWSEventTimeout{Reason: reason})
				}
			})
			for {
				select {
				case <-b.ctx.Done():
					return
				case command := <-b.sendCommands:
					b.handleSendCommand(command)
				}
			}
		}()
	})
}

func (b *ResponsesWSIOBridge) handleSendCommand(command responsesWSSendCommand) {
	defer recoverResponsesWSGoroutine("send_command", func(reason string) {
		if b.actor != nil {
			b.actor.PostReliable(ResponsesWSEventSendResult{
				AttemptID:         command.AttemptID,
				SelectedChannelID: command.SelectedChannelID,
				Outcome:           SendOutcomeAmbiguous,
				Err:               errors.New(reason),
			})
		}
	})
	select {
	case <-b.ctx.Done():
		if b.actor != nil {
			b.actor.PostReliable(ResponsesWSEventSendResult{
				AttemptID:         command.AttemptID,
				SelectedChannelID: command.SelectedChannelID,
				Outcome:           SendOutcomeNotSent,
				Err:               runtimesession.ErrSessionClosed,
			})
		}
		return
	default:
	}
	err := command.Session.SendClient(b.ctx, command.MessageType, command.Payload)
	if b.actor != nil {
		b.actor.PostReliable(ResponsesWSEventSendResult{
			AttemptID:         command.AttemptID,
			SelectedChannelID: command.SelectedChannelID,
			Outcome:           responsesWSSendOutcomeFromError(err),
			Err:               err,
		})
	}
}

func (b *ResponsesWSIOBridge) SendProviderFrame(attemptID string, selectedChannelID int, session runtimesession.RealtimeSession, mt int, payload []byte) bool {
	if b == nil || session == nil {
		return false
	}
	b.startSendWorker()
	command := responsesWSSendCommand{
		AttemptID:         attemptID,
		SelectedChannelID: selectedChannelID,
		Session:           session,
		MessageType:       mt,
		Payload:           payload,
	}
	select {
	case <-b.ctx.Done():
		return false
	case b.sendCommands <- command:
		return true
	default:
		return false
	}
}

func (b *ResponsesWSIOBridge) WriteClientFrame(mt int, payload []byte, mode ResponsesWSWriteMode) error {
	if b == nil || b.writer == nil || len(payload) == 0 {
		return nil
	}
	if mode == ResponsesWSWriteProxyLocal && mt == websocket.TextMessage {
		return requester.WriteWSLocalError(b.writer, payload)
	}
	if mt == websocket.CloseMessage {
		return b.writer.WriteControl(websocket.CloseMessage, payload)
	}
	return b.writer.WriteMessage(mt, payload)
}

func (b *ResponsesWSIOBridge) WriteCloseControl(code int, reason string) {
	if b == nil || b.writer == nil {
		return
	}
	_ = b.writer.WriteClose(code, reason)
}

func (b *ResponsesWSIOBridge) AbortSession(session runtimesession.RealtimeSession, reason string) {
	if session != nil {
		session.Abort(strings.TrimSpace(reason))
	}
}

func (b *ResponsesWSIOBridge) Close() {
	if b == nil {
		return
	}
	b.cancel()
	if b.writer != nil {
		_ = b.writer.Close()
	} else if b.conn != nil {
		_ = b.conn.Close()
	}
	b.wg.Wait()
}

func (b *ResponsesWSIOBridge) refreshReadDeadline() {
	if b == nil || b.conn == nil {
		return
	}
	interval := config.RealtimeWebsocketPingInterval()
	if interval <= 0 {
		_ = b.conn.SetReadDeadline(time.Time{})
		return
	}
	_ = b.conn.SetReadDeadline(time.Now().Add(2 * interval))
}

type responsesWSSessionState int

const (
	responsesWSStateOpening responsesWSSessionState = iota
	responsesWSStatePendingPrepare
	responsesWSStatePendingSend
	responsesWSStateInFlight
	responsesWSStateIdle
	responsesWSStateClosed
)

type responsesWSPendingTurnPhase int

const (
	responsesWSPendingTurnNone responsesWSPendingTurnPhase = iota
	responsesWSPendingTurnOpening
	responsesWSPendingTurnPrepare
	responsesWSPendingTurnSend
)

type ResponsesWSSessionActor struct {
	events   chan ResponsesWSEvent
	done     chan struct{}
	doneOnce sync.Once
	bridge   *ResponsesWSIOBridge
	snapshot *ResponsesWSRequestSnapshot

	leaseMu                   sync.Mutex
	pendingLease              middleware.ResponsesWSLease
	activeLease               middleware.ResponsesWSLease
	upstreamSessionGeneration string
	sessionChannelID          int
	session                   runtimesession.RealtimeSession
	providerRecvArmed         bool
	pendingTurnPhase          responsesWSPendingTurnPhase
	openingID                 string
	firstFrame                *responsesws.RawResponsesCreateFrame
	firstTurnStartedAt        time.Time
	firstTurnAdmission        *ResponsesWSTurnAdmission
	pendingAttempt            *ResponsesWSTurnAttempt
	pendingProviderEvents     []ResponsesWSEventProviderDownstream
	pendingProviderBytes      int
	// Usage-only provider events have no frame to buffer, but still prove that
	// the pending turn may have reached upstream.
	pendingProviderEvidenceSeen bool
	activeAttempt               *ResponsesWSTurnAttempt
	activeTurn                  *ResponsesTurnAffinity
	activeChannelID             int
	lastFinal                   *types.OpenAIResponsesResponses
	state                       responsesWSSessionState
	closed                      atomic.Bool
	clientClosed                atomic.Bool
	backpressurePosted          atomic.Bool
	downstreamCloseSent         atomic.Bool
	lastActivityUnixNano        atomic.Int64
	busyRejectWindowStart       time.Time
	busyRejects                 int
	setupCancelMu               sync.Mutex
	setupCancel                 context.CancelFunc
}

func NewResponsesWSSessionActor(c *gin.Context) *ResponsesWSSessionActor {
	actor := &ResponsesWSSessionActor{
		events: make(chan ResponsesWSEvent, responsesWSEventQueueSize),
		done:   make(chan struct{}),
		state:  responsesWSStateOpening,
	}
	actor.RefreshContext(c)
	actor.markActivity()
	return actor
}

func (a *ResponsesWSSessionActor) RefreshContext(c *gin.Context) {
	if a == nil {
		return
	}
	if c == nil {
		a.snapshot = nil
		return
	}
	a.snapshot = NewResponsesWSRequestSnapshot(c)
}

func (a *ResponsesWSSessionActor) Context() *gin.Context {
	if a == nil || a.snapshot == nil {
		return nil
	}
	return a.snapshot.Context()
}

func (a *ResponsesWSSessionActor) SetBridge(bridge *ResponsesWSIOBridge) {
	a.bridge = bridge
}

func (a *ResponsesWSSessionActor) Start() {
	go a.loop()
	go a.idleWatchdog()
}

func (a *ResponsesWSSessionActor) Done() <-chan struct{} {
	return a.done
}

func (a *ResponsesWSSessionActor) Post(event ResponsesWSEvent) bool {
	if a == nil || a.closed.Load() {
		return false
	}
	select {
	case a.events <- event:
		return true
	default:
		if a.backpressurePosted.CompareAndSwap(false, true) {
			go func() {
				defer recoverResponsesWSGoroutine("backpressure_post", nil)
				timer := time.NewTimer(5 * time.Second)
				defer timer.Stop()
				select {
				case a.events <- ResponsesWSEventTimeout{Reason: "responses_ws_event_backpressure"}:
				case <-a.done:
				case <-timer.C:
					logger.LogError(context.Background(), "responses websocket backpressure timeout post timed out")
				}
			}()
		}
		return false
	}
}

func (a *ResponsesWSSessionActor) PostReliable(event ResponsesWSEvent) bool {
	if a == nil || a.closed.Load() {
		return false
	}
	select {
	case a.events <- event:
		return true
	case <-a.done:
		return false
	}
}

func (a *ResponsesWSSessionActor) ReserveFirstTurnOpening(frame *responsesws.RawResponsesCreateFrame) string {
	a.openingID = uuid.NewString()
	a.pendingTurnPhase = responsesWSPendingTurnOpening
	a.firstFrame = frame
	a.firstTurnStartedAt = time.Time{}
	a.firstTurnAdmission = NewResponsesWSTurnAdmission()
	a.state = responsesWSStateOpening
	return a.openingID
}

func (a *ResponsesWSSessionActor) AttachUpstreamSession(session runtimesession.RealtimeSession, selectedChannelID int) string {
	a.session = session
	a.sessionChannelID = selectedChannelID
	a.upstreamSessionGeneration = uuid.NewString()
	return a.upstreamSessionGeneration
}

func (a *ResponsesWSSessionActor) BeginCandidate(attempt *ResponsesWSTurnAttempt) error {
	if a == nil || attempt == nil {
		return errors.New("attempt is required")
	}
	if a.closed.Load() {
		return errors.New("responses websocket session is closed")
	}
	if err := attempt.BeginCandidate(a); err != nil {
		return err
	}
	a.pendingTurnPhase = responsesWSPendingTurnPrepare
	a.state = responsesWSStatePendingPrepare
	return nil
}

func (a *ResponsesWSSessionActor) MarkPendingSend() {
	a.pendingTurnPhase = responsesWSPendingTurnSend
	a.state = responsesWSStatePendingSend
}

func (a *ResponsesWSSessionActor) rollbackPendingAttemptBeforeLocalWrite(reason string) error {
	if a == nil {
		return nil
	}
	attempt := a.pendingAttempt
	if attempt != nil {
		if err := attempt.RollbackBeforeLocalWriteOK(reason); err != nil {
			return err
		}
	}
	a.pendingAttempt = nil
	a.pendingProviderEvents = nil
	a.pendingProviderBytes = 0
	a.pendingProviderEvidenceSeen = false
	a.pendingTurnPhase = responsesWSPendingTurnNone
	if !a.closed.Load() {
		a.state = responsesWSStateIdle
	}
	return nil
}

func (a *ResponsesWSSessionActor) rollbackPendingAttemptOrClose(reason string) bool {
	if err := a.rollbackPendingAttemptBeforeLocalWrite(reason); err != nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_rollback_failed", responsesWSStaticErrorMessage("quota_rollback_failed")))
		a.close("quota_rollback_failed")
		return false
	}
	return true
}

func (a *ResponsesWSSessionActor) markClientClosed(err error) {
	if a == nil {
		return
	}
	if err != nil && !isResponsesWSExpectedClientDisconnectError(err) {
		logger.LogInfo(context.Background(), fmt.Sprintf("responses websocket client closed: %T: %v", err, err))
	}
	a.clientClosed.Store(true)
	a.cancelSetup()
	if a.bridge != nil && a.bridge.cancel != nil {
		a.bridge.cancel()
	}
}

func (a *ResponsesWSSessionActor) isClientGone() bool {
	return a == nil || a.closed.Load() || a.clientClosed.Load()
}

func isResponsesWSExpectedClientDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	// Codex and browsers commonly exit without completing the websocket close
	// handshake. Suppressing these transport-level disconnects keeps normal
	// shutdowns out of info logs; cleanup and quota finalization still run.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure:
			return true
		}
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(message, "broken pipe") || strings.Contains(message, "connection reset by peer") || strings.Contains(message, "software caused connection abort") {
		return true
	}
	return false
}

func (a *ResponsesWSSessionActor) setSetupCancel(cancel context.CancelFunc) {
	if a == nil {
		return
	}
	a.setupCancelMu.Lock()
	a.setupCancel = cancel
	a.setupCancelMu.Unlock()
}

func (a *ResponsesWSSessionActor) clearSetupCancel() {
	if a == nil {
		return
	}
	a.setupCancelMu.Lock()
	a.setupCancel = nil
	a.setupCancelMu.Unlock()
}

func (a *ResponsesWSSessionActor) cancelSetup() {
	if a == nil {
		return
	}
	a.setupCancelMu.Lock()
	cancel := a.setupCancel
	a.setupCancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *ResponsesWSSessionActor) releasePendingLease() {
	if a == nil {
		return
	}
	a.leaseMu.Lock()
	lease := a.pendingLease
	a.pendingLease = nil
	a.leaseMu.Unlock()
	if lease != nil {
		lease.Release()
	}
}

func (a *ResponsesWSSessionActor) releaseActiveLease() {
	if a == nil {
		return
	}
	a.leaseMu.Lock()
	lease := a.activeLease
	a.activeLease = nil
	a.leaseMu.Unlock()
	if lease != nil {
		lease.Release()
	}
}

func (a *ResponsesWSSessionActor) setPendingLease(lease middleware.ResponsesWSLease) {
	if a == nil {
		return
	}
	a.leaseMu.Lock()
	a.pendingLease = lease
	a.leaseMu.Unlock()
}

func (a *ResponsesWSSessionActor) setActiveLease(lease middleware.ResponsesWSLease) {
	if a == nil {
		return
	}
	a.leaseMu.Lock()
	a.activeLease = lease
	a.leaseMu.Unlock()
	a.armActiveLeaseLossWatch(lease)
}

func (a *ResponsesWSSessionActor) armActiveLeaseLossWatch(lease middleware.ResponsesWSLease) {
	if a == nil || lease == nil {
		return
	}
	lost := lease.Lost()
	if lost == nil {
		return
	}
	go func() {
		select {
		case <-a.done:
			return
		case <-lost:
			// Trade-off: once the shared Redis lease is lost we close the session instead
			// of silently degrading to a process-local counter, so cluster-wide active
			// limits remain trustworthy under Redis churn.
			a.PostReliable(ResponsesWSEventTimeout{Reason: "responses_ws_active_lease_lost"})
		}
	}()
}

func (a *ResponsesWSSessionActor) markActivity() {
	if a == nil {
		return
	}
	a.lastActivityUnixNano.Store(time.Now().UnixNano())
}

func (a *ResponsesWSSessionActor) loop() {
	defer a.finish()
	defer recoverResponsesWSGoroutine("actor_loop", func(reason string) {
		a.close(reason)
	})
	for {
		select {
		case <-a.done:
			return
		case event := <-a.events:
			a.handleEvent(event)
			if a.closed.Load() {
				return
			}
		}
	}
}

func (a *ResponsesWSSessionActor) finish() {
	if a == nil {
		return
	}
	a.doneOnce.Do(func() {
		close(a.done)
	})
}

func (a *ResponsesWSSessionActor) idleWatchdog() {
	defer recoverResponsesWSGoroutine("idle_watchdog", func(reason string) {
		a.PostReliable(ResponsesWSEventTimeout{Reason: reason})
	})
	timeout := config.ResponsesWSIdleTimeout()
	if timeout <= 0 {
		return
	}
	interval := timeout / 4
	if interval <= 0 || interval > 5*time.Second {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
			last := time.Unix(0, a.lastActivityUnixNano.Load())
			if time.Since(last) >= timeout {
				a.PostReliable(ResponsesWSEventTimeout{Reason: "idle_timeout"})
				return
			}
		}
	}
}

func (a *ResponsesWSSessionActor) handleEvent(event ResponsesWSEvent) {
	switch typed := event.(type) {
	case ResponsesWSEventFirstTurnSetup:
		a.handleFirstTurnSetup(typed)
	case ResponsesWSEventFirstTurnOpenResult:
		a.handleFirstTurnOpenResult(typed)
	case ResponsesWSEventClientFrame:
		a.handleClientFrame(typed)
	case ResponsesWSEventSendResult:
		a.handleSendResult(typed)
	case ResponsesWSEventProviderDownstream:
		a.handleProviderDownstream(typed)
	case ResponsesWSEventProxyLocalError:
		if typed.UpstreamSessionGeneration != "" && typed.UpstreamSessionGeneration != a.upstreamSessionGeneration {
			return
		}
		if typed.ChannelID > 0 && typed.ChannelID != a.sessionChannelID {
			return
		}
		a.writeProxyLocal(typed.Payload)
		if !typed.Recoverable {
			a.close("proxy_local_error")
		}
	case ResponsesWSEventClientClosed:
		a.handleClientClosed(typed.Err)
	case ResponsesWSEventTimeout:
		if typed.UpstreamSessionGeneration != "" && typed.UpstreamSessionGeneration != a.upstreamSessionGeneration {
			return
		}
		if typed.ChannelID > 0 && typed.ChannelID != a.sessionChannelID {
			return
		}
		a.close(typed.Reason)
	case ResponsesWSEventCloseIntent:
		a.close(typed.Reason)
	}
}

func (a *ResponsesWSSessionActor) handleFirstTurnSetup(event ResponsesWSEventFirstTurnSetup) {
	if a == nil {
		return
	}
	a.setPendingLease(event.PendingLease)
	if event.Frame == nil {
		a.releasePendingLease()
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", "response.create frame is required"))
		a.close("first_turn_setup_missing_frame")
		return
	}

	openingID := a.ReserveFirstTurnOpening(event.Frame)
	if !event.ReceivedAt.IsZero() {
		a.firstTurnStartedAt = event.ReceivedAt
	}
	if a.isClientGone() {
		a.releasePendingLease()
		a.close("client_closed_before_first_turn_setup")
		return
	}

	actorCtx := a.Context()
	request := event.Frame.Projection
	prepareResponsesChannelAffinity(actorCtx, &request)
	a.RefreshContext(actorCtx)
	admission := a.firstTurnAdmission
	if admission == nil {
		admission = NewResponsesWSTurnAdmission()
		a.firstTurnAdmission = admission
	}
	if a.isClientGone() {
		a.close("client_closed_before_active_lease")
		return
	}
	activeLease, apiErr := middleware.AcquireResponsesWSActiveLease(actorCtx)
	if apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		a.close("active_lease_failed")
		return
	}
	a.setActiveLease(activeLease)

	if apiErr := admission.AllowRPMOnce(func() *types.OpenAIErrorWithStatusCode {
		return middleware.AllowCurrentUserRequest(a.Context())
	}); apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		a.close("rpm_allow_failed")
		return
	}
	if a.isClientGone() {
		a.releasePendingLease()
		a.close("client_closed_before_upstream_open")
		return
	}
	a.releasePendingLease()

	a.startFirstTurnOpenWorker(openingID, event.Frame)
}

func (a *ResponsesWSSessionActor) startFirstTurnOpenWorker(openingID string, frame *responsesws.RawResponsesCreateFrame) {
	if a == nil || frame == nil {
		return
	}
	setupCtx, cancel := context.WithCancel(context.Background())
	a.setSetupCancel(cancel)
	actorSnapshot := a.snapshot.Clone()

	go func() {
		defer cancel()
		var openResult *responsesWSOpenResult
		handedOff := false
		defer recoverResponsesWSGoroutine("first_turn_open_worker", func(reason string) {
			if !handedOff {
				cleanupResponsesWSOpenResult(openResult, reason)
			}
			a.PostReliable(ResponsesWSEventTimeout{Reason: reason})
		})
		select {
		case <-setupCtx.Done():
			return
		default:
		}

		var apiErr *types.OpenAIErrorWithStatusCode
		actorContext := actorSnapshot.Context()
		openResult, apiErr = openAndPrimeResponsesWSSessionForActor(setupCtx, actorContext, &frame.Projection)
		if setupCtx.Err() != nil {
			cleanupResponsesWSOpenResult(openResult, "first_turn_open_cancelled")
			return
		}

		adopted := make(chan bool, 1)
		event := ResponsesWSEventFirstTurnOpenResult{
			OpeningID:  openingID,
			Snapshot:   NewResponsesWSRequestSnapshot(actorContext),
			OpenResult: openResult,
			Err:        apiErr,
			Adopted:    adopted,
		}
		select {
		case a.events <- event:
			handedOff = true
		case <-setupCtx.Done():
			cleanupResponsesWSOpenResult(openResult, "first_turn_open_cancelled")
			return
		case <-a.done:
			cleanupResponsesWSOpenResult(openResult, "first_turn_open_abandoned")
			return
		}

		select {
		case ok := <-adopted:
			if !ok {
				cleanupResponsesWSOpenResult(openResult, "first_turn_open_not_adopted")
			}
		case <-a.done:
			cleanupResponsesWSOpenResult(openResult, "first_turn_open_abandoned")
		}
	}()
}

func cleanupResponsesWSOpenResult(openResult *responsesWSOpenResult, reason string) {
	if openResult == nil || openResult.Session == nil {
		return
	}
	openResult.Session.Abort(strings.TrimSpace(reason))
}

func (a *ResponsesWSSessionActor) handleFirstTurnOpenResult(event ResponsesWSEventFirstTurnOpenResult) {
	adopted := false
	defer func() {
		if event.Adopted != nil {
			event.Adopted <- adopted
		}
	}()
	if a == nil {
		return
	}
	defer a.clearSetupCancel()
	if event.OpeningID == "" || event.OpeningID != a.openingID || a.pendingTurnPhase != responsesWSPendingTurnOpening {
		cleanupResponsesWSOpenResult(event.OpenResult, "stale_first_turn_open_result")
		adopted = true
		return
	}
	if a.isClientGone() {
		cleanupResponsesWSOpenResult(event.OpenResult, "client_closed_during_open")
		adopted = true
		a.close("client_closed_during_open")
		return
	}
	if event.Snapshot != nil {
		a.snapshot = event.Snapshot.Clone()
	}
	if event.Err != nil {
		cleanupResponsesWSOpenResult(event.OpenResult, "first_turn_open_failed")
		if openAIErrorCodeString(event.Err.Code, "") == "responses_ws_unsupported_for_channel" {
			a.writeProxyLocal(responsesWSFallbackPayload())
			if a.bridge != nil {
				a.markDownstreamCloseSent()
				a.bridge.WriteCloseControl(websocket.CloseNormalClosure, "responses_ws_unsupported_for_channel")
			}
		} else {
			a.writeProxyLocal(responsesWSErrorFromOpenAI(event.Err))
		}
		adopted = true
		a.close("open_failed")
		return
	}
	if event.OpenResult == nil || event.OpenResult.Session == nil || event.OpenResult.Channel == nil {
		cleanupResponsesWSOpenResult(event.OpenResult, "invalid_first_turn_open_result")
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "channel_error", "responses websocket open did not return a channel"))
		adopted = true
		a.close("open_failed")
		return
	}

	adopted = true
	a.prepareAndSendFirstTurn(event.OpenResult)
}

func (a *ResponsesWSSessionActor) prepareAndSendFirstTurn(openResult *responsesWSOpenResult) {
	if a == nil || openResult == nil || openResult.Session == nil || openResult.Channel == nil || a.firstFrame == nil {
		if openResult != nil && openResult.Session != nil {
			openResult.Session.Abort("first_turn_prepare_failed")
		}
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "channel_error", "responses websocket first turn setup is incomplete"))
		a.close("first_turn_prepare_failed")
		return
	}
	if a.isClientGone() {
		openResult.Session.Abort("client_closed_before_first_turn_prepare")
		a.close("client_closed_before_first_turn_prepare")
		return
	}

	attachResponsesWSSelectedChannelSnapshot(a.snapshot, openResult.Channel, openResult.ProviderModel, openResult.BillingModel)
	actorCtx := a.Context()
	upstreamSessionGeneration := a.AttachUpstreamSession(openResult.Session, openResult.Channel.Id)
	admission := a.firstTurnAdmission
	if admission == nil {
		admission = NewResponsesWSTurnAdmission()
		a.firstTurnAdmission = admission
	}
	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:           actorCtx,
		Snapshot:          a.snapshot,
		OpeningID:         a.openingID,
		Admission:         admission,
		Candidate:         openResult.Candidate,
		SelectedChannelID: openResult.Channel.Id,
		Session:           openResult.Session,
		BillingModel:      openResult.BillingModel,
		PromptModel:       openResult.ProviderModel,
		Request:           &a.firstFrame.Projection,
		StartedAt:         a.firstTurnStartedAt,
	})
	if apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		a.close("attempt_prepare_failed")
		return
	}
	if a.isClientGone() {
		a.close("client_closed_before_attempt_begin")
		return
	}
	if err := a.BeginCandidate(attempt); err != nil {
		logger.LogError(context.Background(), "responses websocket attempt begin failed: "+err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "responses_ws_attempt_failed", responsesWSStaticErrorMessage("responses_ws_attempt_failed")))
		a.close("attempt_begin_failed")
		return
	}
	payload, err := responsesWSProviderPayload(actorCtx, a.firstFrame, &a.firstFrame.Projection, openResult.ProviderModel)
	if err != nil {
		if !a.rollbackPendingAttemptOrClose("rewrite_failed") {
			return
		}
		logger.LogError(context.Background(), "responses websocket rewrite failed: "+err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "responses_ws_payload_rewrite_failed", responsesWSStaticErrorMessage("responses_ws_payload_rewrite_failed")))
		a.close("rewrite_failed")
		return
	}
	if a.isClientGone() {
		if !a.rollbackPendingAttemptOrClose("client_closed_before_quota_preconsume") {
			return
		}
		a.close("client_closed_before_quota_preconsume")
		return
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		if !a.rollbackPendingAttemptOrClose("quota_preconsume_failed") {
			return
		}
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		a.close("quota_preconsume_failed")
		return
	}
	if a.isClientGone() {
		if !a.rollbackPendingAttemptOrClose("client_closed_before_provider_send") {
			return
		}
		a.close("client_closed_before_provider_send")
		return
	}

	a.MarkPendingSend()
	a.providerRecvArmed = true
	a.bridge.ArmProviderRecvPump(upstreamSessionGeneration, openResult.Channel.Id, openResult.Session)
	if !a.bridge.SendProviderFrame(attempt.AttemptID, openResult.Channel.Id, openResult.Session, websocket.TextMessage, payload) {
		a.postSendQueueFull(attempt.AttemptID, openResult.Channel.Id)
	}
}

func (a *ResponsesWSSessionActor) handleClientFrame(event ResponsesWSEventClientFrame) {
	if event.MessageType != websocket.TextMessage {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", "only text websocket events are supported"))
		return
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(event.Payload, &envelope); err != nil {
		logger.LogError(context.Background(), "responses websocket client frame parse failed: "+err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", err.Error()))
		return
	}
	switch strings.TrimSpace(envelope.Type) {
	case "response.create":
		if a.isBusy() {
			if a.recordBusyReject() {
				a.writeProxyLocal(responsesWSErrorPayload(http.StatusTooManyRequests, "responses_ws_busy_rate_limited", "too many response.create frames while the session is busy"))
				a.close("responses_ws_busy_rate_limited")
				return
			}
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "session_busy", "responses websocket session already has an inflight response"))
			return
		}
		a.resetBusyRejects()
		a.startSubsequentTurn(event.Payload, event.ReceivedAt)
	case "response.cancel":
		if a.session == nil {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "session_closed", "responses websocket session is not open"))
			return
		}
		if !a.bridge.SendProviderFrame("", a.sessionChannelID, a.session, event.MessageType, event.Payload) {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "responses_ws_send_queue_full", responsesWSStaticErrorMessage("responses_ws_send_queue_full")))
		}
	default:
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "unsupported_client_event", "unsupported responses websocket client event"))
	}
}

func (a *ResponsesWSSessionActor) recordBusyReject() bool {
	if a == nil {
		return true
	}
	now := time.Now()
	if a.busyRejectWindowStart.IsZero() || now.Sub(a.busyRejectWindowStart) > responsesWSBusyRejectWindow {
		a.busyRejectWindowStart = now
		a.busyRejects = 0
	}
	a.busyRejects++
	return a.busyRejects > responsesWSBusyRejectLimit
}

func (a *ResponsesWSSessionActor) resetBusyRejects() {
	if a == nil {
		return
	}
	a.busyRejectWindowStart = time.Time{}
	a.busyRejects = 0
}

func (a *ResponsesWSSessionActor) startSubsequentTurn(raw []byte, receivedAt time.Time) {
	frame, err := responsesws.ParseRawResponsesCreateFrame(raw)
	if err != nil {
		logger.LogError(context.Background(), "responses websocket subsequent frame parse failed: "+err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", err.Error()))
		return
	}
	ctx := a.Context()
	lockedSessionModel := strings.TrimSpace(ctx.GetString("original_model"))
	if mismatch := responsesWSSubsequentModelMismatch(frame.Projection.Model, lockedSessionModel); mismatch != "" {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "responses_ws_model_mismatch", mismatch))
		return
	}
	request := frame.Projection
	request.Model = lockedSessionModel
	if request.Model == "" {
		request.Model = frame.Projection.Model
	}
	providerModel, billingModel := responsesWSCurrentModelNames(ctx)
	candidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{Context: ctx, Request: &request})
	if err != nil {
		logger.LogError(context.Background(), "responses websocket affinity conflict: "+err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "responses_affinity_conflict", responsesWSStaticErrorMessage("responses_affinity_conflict")))
		return
	}
	if err := responsesAffinityOwnerConflict(candidate, a.sessionChannelID); err != nil {
		logger.LogError(context.Background(), "responses websocket affinity owner conflict: "+err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "responses_affinity_conflict", responsesWSStaticErrorMessage("responses_affinity_conflict")))
		return
	}

	channel, _ := ctx.Get("responses_ws_selected_channel")
	if typed, ok := channel.(*model.Channel); ok && typed != nil {
		ctx.Set("channel_id", typed.Id)
		ctx.Set("channel_type", typed.Type)
		a.RefreshContext(ctx)
		ctx = a.Context()
	}
	admission := NewResponsesWSTurnAdmission()
	if apiErr := admission.AllowRPMOnce(func() *types.OpenAIErrorWithStatusCode {
		return middleware.AllowCurrentUserRequest(ctx)
	}); apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		return
	}
	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:           ctx,
		Snapshot:          a.snapshot,
		OpeningID:         "",
		Admission:         admission,
		Candidate:         candidate,
		SelectedChannelID: a.sessionChannelID,
		Session:           a.session,
		BillingModel:      billingModel,
		PromptModel:       providerModel,
		Request:           &request,
		StartedAt:         receivedAt,
	})
	if apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		return
	}
	if err := a.BeginCandidate(attempt); err != nil {
		logger.LogError(context.Background(), "responses websocket attempt begin failed: "+err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "responses_ws_attempt_failed", responsesWSStaticErrorMessage("responses_ws_attempt_failed")))
		return
	}
	payload, err := responsesWSProviderPayload(ctx, frame, &request, providerModel)
	if err != nil {
		if !a.rollbackPendingAttemptOrClose("rewrite_failed") {
			return
		}
		logger.LogError(context.Background(), "responses websocket rewrite failed: "+err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "responses_ws_payload_rewrite_failed", responsesWSStaticErrorMessage("responses_ws_payload_rewrite_failed")))
		return
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		if !a.rollbackPendingAttemptOrClose("quota_preconsume_failed") {
			return
		}
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		return
	}
	a.MarkPendingSend()
	if !a.providerRecvArmed {
		a.providerRecvArmed = true
		a.bridge.ArmProviderRecvPump(a.upstreamSessionGeneration, a.sessionChannelID, a.session)
	}
	if !a.bridge.SendProviderFrame(attempt.AttemptID, a.sessionChannelID, a.session, websocket.TextMessage, payload) {
		a.postSendQueueFull(attempt.AttemptID, a.sessionChannelID)
	}
}

func (a *ResponsesWSSessionActor) postSendQueueFull(attemptID string, selectedChannelID int) {
	if a == nil {
		return
	}
	a.postInternalEvent(ResponsesWSEventSendResult{
		AttemptID:         attemptID,
		SelectedChannelID: selectedChannelID,
		Outcome:           SendOutcomeNotSent,
		Err:               errResponsesWSSendQueueFull,
	}, "send_queue_full_post")
}

func (a *ResponsesWSSessionActor) handleSendResult(event ResponsesWSEventSendResult) {
	if event.AttemptID == "" {
		if event.Err != nil {
			a.writeProxyLocal(responsesWSErrorFromErr(event.Err))
		}
		return
	}
	attempt := a.pendingAttempt
	if attempt == nil || attempt.AttemptID != event.AttemptID || attempt.SelectedChannelID != event.SelectedChannelID {
		a.handleSendResultMismatch("responses_ws_send_result_mismatch")
		return
	}

	switch event.Outcome {
	case SendOutcomeLocalWriteOK:
		attempt.CommitLocalWriteOK()
		a.commitPendingAttempt(attempt)
	case SendOutcomeNotSent:
		if a.hasPendingProviderEvidence() {
			a.failProofConflict("responses_ws_not_sent_with_provider_evidence")
			return
		}
		if isResponsesContinuationMissError(event.Err) {
			ClearResponsesTurnStaleBindings(attempt.Candidate, attempt.SelectedChannelID)
		}
		if err := attempt.RollbackBeforeLocalWriteOK("send_not_sent"); err != nil {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_rollback_failed", responsesWSStaticErrorMessage("quota_rollback_failed")))
			a.close("quota_rollback_failed")
			return
		}
		a.pendingAttempt = nil
		if a.retryFirstTurnAfterNotSent(attempt) {
			return
		}
		a.pendingTurnPhase = responsesWSPendingTurnNone
		a.state = responsesWSStateIdle
		if event.Err != nil {
			a.writeProxyLocal(responsesWSErrorFromErr(event.Err))
		}
	case SendOutcomeAmbiguous:
		hadProviderEvidence := a.hasPendingProviderEvidence()
		attempt.CommitAmbiguousAdmission("send_ambiguous")
		a.commitPendingAttempt(attempt)
		if !hadProviderEvidence {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadGateway, "responses_ws_send_ambiguous", "upstream write result is ambiguous"))
			a.close("responses_ws_send_ambiguous")
		}
	default:
		a.failClosed("responses_ws_unknown_send_result")
	}
}

func (a *ResponsesWSSessionActor) handleSendResultMismatch(reason string) {
	if a == nil {
		return
	}
	attempt := a.pendingAttempt
	if attempt == nil {
		a.failClosed(reason)
		return
	}
	if a.hasPendingProviderEvidence() {
		a.failProofConflict(reason)
		return
	}
	if err := attempt.RollbackBeforeLocalWriteOK(reason); err != nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_rollback_failed", responsesWSStaticErrorMessage("quota_rollback_failed")))
		a.close("quota_rollback_failed")
		return
	}
	a.pendingAttempt = nil
	a.pendingProviderEvents = nil
	a.pendingProviderBytes = 0
	a.pendingProviderEvidenceSeen = false
	a.pendingTurnPhase = responsesWSPendingTurnNone
	a.failClosed(reason)
}

func (a *ResponsesWSSessionActor) retryFirstTurnAfterNotSent(previous *ResponsesWSTurnAttempt) bool {
	if a == nil || previous == nil || previous.OpeningID == "" || a.pendingTurnPhase == responsesWSPendingTurnNone || a.firstFrame == nil || previous.Admission == nil {
		return false
	}
	if previous.Candidate != nil && previous.Candidate.ExplicitPinID > 0 {
		return false
	}
	ctx := a.Context()
	if currentChannelAffinityStrict(ctx) {
		return false
	}

	failedChannelID := previous.SelectedChannelID
	if failedChannelID > 0 {
		(&relayBase{c: ctx}).skipChannelID(failedChannelID)
		a.RefreshContext(ctx)
	}
	if a.session != nil && a.bridge != nil {
		a.bridge.AbortSession(a.session, "send_not_sent_retry")
	}
	a.session = nil
	a.upstreamSessionGeneration = ""
	a.sessionChannelID = 0
	a.providerRecvArmed = false
	a.pendingProviderEvents = nil
	a.pendingProviderBytes = 0
	a.pendingProviderEvidenceSeen = false
	a.firstTurnAdmission = previous.Admission
	clearResponsesWSSelectedChannelSnapshot(a.snapshot)

	if a.isClientGone() {
		a.close("client_closed_before_first_turn_retry")
		return true
	}
	a.pendingTurnPhase = responsesWSPendingTurnOpening
	a.state = responsesWSStateOpening
	a.startFirstTurnOpenWorker(a.openingID, a.firstFrame)
	return true
}

func (a *ResponsesWSSessionActor) commitPendingAttempt(attempt *ResponsesWSTurnAttempt) {
	a.activeAttempt = attempt
	a.activeTurn = CommitResponsesTurnAffinity(attempt.Candidate, attempt.SelectedChannelID)
	a.activeChannelID = attempt.SelectedChannelID
	a.pendingTurnPhase = responsesWSPendingTurnNone
	a.pendingAttempt = nil
	a.state = responsesWSStateInFlight
	buffered := append([]ResponsesWSEventProviderDownstream(nil), a.pendingProviderEvents...)
	a.pendingProviderEvents = nil
	a.pendingProviderBytes = 0
	a.pendingProviderEvidenceSeen = false
	for _, downstream := range buffered {
		a.handleProviderDownstream(downstream)
		if a.closed.Load() {
			return
		}
	}
}

func (a *ResponsesWSSessionActor) handleProviderDownstream(event ResponsesWSEventProviderDownstream) {
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstreamSessionGeneration {
		return
	}
	if event.ChannelID > 0 && event.ChannelID != a.sessionChannelID {
		a.failClosed("responses_ws_provider_channel_mismatch")
		return
	}
	if event.Err != nil {
		logCtx := context.Background()
		ctx := a.Context()
		if ctx != nil && ctx.Request != nil {
			logCtx = ctx.Request.Context()
		}
		logger.LogWarn(logCtx, fmt.Sprintf(
			"responses websocket provider downstream carried err: kind=%d origin=%d msg_type=%d err=%s",
			event.Kind, event.Origin, event.MessageType, event.Err.Error()))
	}
	if event.Kind == ProviderDownstreamUsage && event.Usage != nil {
		if a.pendingAttempt != nil {
			a.pendingProviderEvidenceSeen = true
			a.pendingAttempt.MarkProviderUsageSeen()
			mergeResponsesWSUsageEvent(a.pendingAttempt.Usage, event.Usage)
			return
		}
		if a.activeAttempt != nil {
			a.activeAttempt.MarkProviderUsageSeen()
			mergeResponsesWSUsageEvent(a.activeAttempt.Usage, event.Usage)
		}
		return
	}
	if event.Origin != runtimesession.RealtimePayloadOriginProvider {
		a.writeProxyLocal(event.Payload)
		return
	}
	receivedAt := event.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	if a.pendingAttempt != nil {
		a.pendingProviderEvidenceSeen = true
		a.pendingAttempt.MarkProviderAcceptedTurnEvidence()
		if event.Kind == ProviderDownstreamFrame && event.MessageType != websocket.CloseMessage && len(event.Payload) > 0 {
			a.pendingAttempt.MarkFirstProviderResponse(receivedAt)
		}
		a.bufferPendingProviderEvent(event)
		return
	}
	if event.MessageType == websocket.CloseMessage {
		if a.activeAttempt != nil {
			a.activeAttempt.MarkCompleted(receivedAt)
		}
		a.finalizeActiveAttempt()
		a.clearActiveTurn()
		a.markDownstreamCloseSent()
		if err := a.bridge.WriteClientFrame(websocket.CloseMessage, event.Payload, ResponsesWSWriteProvider); err != nil {
			a.close("client_write_failed")
			return
		}
		a.close("provider_closed")
		return
	}
	if a.activeAttempt == nil {
		a.failClosed("responses_ws_provider_event_without_turn")
		return
	}
	if event.Kind == ProviderDownstreamFrame && event.MessageType != websocket.CloseMessage && len(event.Payload) > 0 {
		a.activeAttempt.MarkFirstProviderResponse(receivedAt)
	}

	payload := event.Payload
	classified := responsesws.ClassifyResponsesWSEvent(payload)
	if classified.Malformed {
		a.activeAttempt.MarkCompleted(receivedAt)
		a.handleMalformedProviderFrame(classified)
		return
	}
	a.activeAttempt.MarkProviderAcceptedTurnEvidence()
	if len(classified.NormalizedPayload) > 0 {
		payload = classified.NormalizedPayload
	}
	if classified.Response != nil {
		if classified.Response.Usage != nil {
			a.activeAttempt.MarkProviderUsageSeen()
		}
		mergeResponsesWSTerminalResponse(a.activeAttempt.Usage, classified.Response)
	}
	isTerminal := classified.Kind == responsesws.ResponsesSuccessTerminal ||
		classified.Kind == responsesws.ResponsesFailedTerminal ||
		classified.Kind == responsesws.ResponsesCancelledTerminal
	if isTerminal {
		a.activeAttempt.MarkCompleted(receivedAt)
		a.finalizeActiveAttempt()
		a.processProviderPayloadAPIError(payload, event.ChannelID, "responses_ws_provider_frame")
		a.applyActiveTerminalSideEffects(classified)
	}
	if err := a.bridge.WriteClientFrame(event.MessageType, payload, ResponsesWSWriteProvider); err != nil {
		a.close("client_write_failed")
		return
	}
	if !isTerminal {
		a.processProviderPayloadAPIError(payload, event.ChannelID, "responses_ws_provider_frame")
	}
}

func (a *ResponsesWSSessionActor) processProviderPayloadAPIError(payload []byte, channelID int, source string) {
	if a == nil || len(payload) == 0 {
		return
	}
	apiErr := runtimesession.ProviderAPIErrorFromPayload(payload)
	if apiErr == nil {
		return
	}
	if !a.markProviderAPIErrorSeen(apiErr, source) {
		return
	}
	channel := a.providerPayloadChannel(channelID)
	processProviderAPIError(a.Context(), channel, apiErr, source)
}

func (a *ResponsesWSSessionActor) markProviderAPIErrorSeen(apiErr *types.OpenAIErrorWithStatusCode, source string) bool {
	if a == nil || apiErr == nil {
		return true
	}
	attempt := a.activeAttempt
	if attempt == nil {
		attempt = a.pendingAttempt
	}
	if attempt == nil {
		return true
	}
	// Trade-off: dedupe only within the current turn attempt. This suppresses
	// repeated provider frames for the same failure without hiding the same
	// provider-side error if a later user turn fails independently.
	key := providerAPIErrorDedupeKey(apiErr, source)
	if _, ok := attempt.providerAPIErrorKeys[key]; ok {
		return false
	}
	if attempt.providerAPIErrorKeys == nil {
		attempt.providerAPIErrorKeys = make(map[string]struct{}, 1)
	}
	attempt.providerAPIErrorKeys[key] = struct{}{}
	return true
}

func providerAPIErrorDedupeKey(apiErr *types.OpenAIErrorWithStatusCode, source string) string {
	if apiErr == nil {
		return ""
	}
	return fmt.Sprintf(
		"%s|%d|%s|%v|%s|%s",
		strings.TrimSpace(source),
		apiErr.StatusCode,
		strings.TrimSpace(apiErr.Type),
		apiErr.Code,
		strings.TrimSpace(apiErr.Message),
		strings.TrimSpace(apiErr.Param),
	)
}

func (a *ResponsesWSSessionActor) providerPayloadChannel(channelID int) *model.Channel {
	if a == nil {
		return nil
	}
	ctx := a.Context()
	if ctx != nil {
		if raw, ok := ctx.Get("responses_ws_selected_channel"); ok {
			if channel, ok := raw.(*model.Channel); ok && channel != nil {
				return channel
			}
		}
		if raw, ok := ctx.Get("responses_ws_selected_channel_snapshot"); ok {
			if snapshot, ok := raw.(*SelectedChannelSnapshot); ok && snapshot != nil && snapshot.Channel != nil {
				return snapshot.Channel
			}
		}
	}
	if channelID <= 0 {
		channelID = a.sessionChannelID
	}
	if channelID <= 0 {
		return nil
	}
	channel, err := fetchChannelById(channelID)
	if err != nil {
		return nil
	}
	return channel
}

func (a *ResponsesWSSessionActor) handleMalformedProviderFrame(classified responsesws.ResponsesTerminalResult) {
	if a == nil || a.activeAttempt == nil {
		return
	}
	message := strings.TrimSpace(classified.MalformedError)
	if message == "" {
		message = "provider returned malformed responses websocket frame"
	}
	payload := responsesWSErrorPayload(http.StatusBadGateway, "responses_ws_provider_protocol_error", message)
	a.finalizeActiveAttempt()
	a.clearActiveTurn()
	if err := a.bridge.WriteClientFrame(websocket.TextMessage, payload, ResponsesWSWriteProvider); err != nil {
		a.close("client_write_failed")
		return
	}
	a.close("responses_ws_provider_protocol_error")
}

func (a *ResponsesWSSessionActor) bufferPendingProviderEvent(event ResponsesWSEventProviderDownstream) bool {
	if a == nil {
		return false
	}
	eventBytes := len(event.Payload)
	if len(a.pendingProviderEvents) >= responsesWSPendingProviderEventsMax ||
		a.pendingProviderBytes+eventBytes > config.ResponsesWSPendingProviderEventsMaxBytes() {
		a.failClosed("responses_ws_pending_provider_buffer_full")
		return false
	}
	a.pendingProviderEvents = append(a.pendingProviderEvents, event)
	a.pendingProviderBytes += eventBytes
	return true
}

func (a *ResponsesWSSessionActor) applyActiveTerminalSideEffects(classified responsesws.ResponsesTerminalResult) {
	if a == nil {
		return
	}
	switch classified.Kind {
	case responsesws.ResponsesSuccessTerminal:
		RecordResponsesTurnSuccess(a.Context(), a.activeTurn, classified.Response)
		a.lastFinal = classified.Response
		a.clearActiveTurn()
	case responsesws.ResponsesFailedTerminal:
		if classified.ContinuationMiss {
			ClearResponsesTurnStaleBindings(a.activeTurn, a.activeChannelID)
		}
		a.clearActiveTurn()
	case responsesws.ResponsesCancelledTerminal:
		a.clearActiveTurn()
	}
}

func (a *ResponsesWSSessionActor) finalizeActiveAttempt() {
	if a == nil || a.activeAttempt == nil || a.activeAttempt.Quota == nil {
		return
	}
	a.activeAttempt.FinalizeQuotaPreservingPreConsumed(nil)
}

func (a *ResponsesWSSessionActor) clearActiveTurn() {
	a.activeAttempt = nil
	a.activeTurn = nil
	a.activeChannelID = 0
	a.pendingTurnPhase = responsesWSPendingTurnNone
	a.state = responsesWSStateIdle
}

func (a *ResponsesWSSessionActor) handleClientClosed(err error) {
	if err != nil && !isResponsesWSExpectedClientDisconnectError(err) {
		logger.LogInfo(context.Background(), fmt.Sprintf("responses websocket client close event: %T: %v", err, err))
	}
	a.close("client_closed")
}

func (a *ResponsesWSSessionActor) isBusy() bool {
	return a.pendingTurnPhase != responsesWSPendingTurnNone || a.pendingAttempt != nil || a.activeAttempt != nil || a.state == responsesWSStateOpening || a.state == responsesWSStatePendingPrepare || a.state == responsesWSStatePendingSend || a.state == responsesWSStateInFlight
}

func (a *ResponsesWSSessionActor) writeProxyLocal(payload []byte) {
	if a == nil || a.bridge == nil || len(payload) == 0 {
		return
	}
	if err := a.bridge.WriteClientFrame(websocket.TextMessage, payload, ResponsesWSWriteProxyLocal); err != nil {
		a.markClientClosed(err)
		a.requestCloseIntent("client_write_failed")
	}
}

func (a *ResponsesWSSessionActor) requestCloseIntent(reason string) {
	if a == nil || a.closed.Load() {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "close_intent"
	}
	a.postInternalEvent(ResponsesWSEventCloseIntent{Reason: reason}, "close_intent_post")
}

func (a *ResponsesWSSessionActor) postInternalEvent(event ResponsesWSEvent, label string) bool {
	if a == nil || a.closed.Load() {
		return false
	}
	select {
	case a.events <- event:
		return true
	case <-a.done:
		return false
	default:
		go func() {
			defer recoverResponsesWSGoroutine(label, nil)
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case a.events <- event:
			case <-a.done:
			case <-timer.C:
				logger.LogError(context.Background(), "responses websocket internal event post timed out: "+label)
			}
		}()
		return false
	}
}

func (a *ResponsesWSSessionActor) hasPendingProviderEvidence() bool {
	return a != nil && (a.pendingProviderEvidenceSeen || len(a.pendingProviderEvents) > 0)
}

func (a *ResponsesWSSessionActor) applyBufferedPendingTerminalSideEffects() {
	if a == nil || a.pendingAttempt == nil || a.pendingAttempt.SendOutcome == SendOutcomeNotSent || len(a.pendingProviderEvents) == 0 {
		return
	}
	active := CommitResponsesTurnAffinity(a.pendingAttempt.Candidate, a.pendingAttempt.SelectedChannelID)
	for _, event := range a.pendingProviderEvents {
		if event.Origin != runtimesession.RealtimePayloadOriginProvider || len(event.Payload) == 0 {
			continue
		}
		classified := responsesws.ClassifyResponsesWSEvent(event.Payload)
		if classified.Response != nil {
			if classified.Response.Usage != nil {
				a.pendingAttempt.MarkProviderUsageSeen()
			}
			mergeResponsesWSTerminalResponse(a.pendingAttempt.Usage, classified.Response)
		}
		switch classified.Kind {
		case responsesws.ResponsesSuccessTerminal:
			RecordResponsesTurnSuccess(a.Context(), active, classified.Response)
			a.lastFinal = classified.Response
		case responsesws.ResponsesFailedTerminal:
			if classified.ContinuationMiss {
				ClearResponsesTurnStaleBindings(active, a.pendingAttempt.SelectedChannelID)
			}
		case responsesws.ResponsesCancelledTerminal:
		}
	}
}

func (a *ResponsesWSSessionActor) failClosed(reason string) {
	if a == nil || a.closed.Load() {
		return
	}
	a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadGateway, "responses_ws_protocol_violation", reason))
	a.close(reason)
}

func (a *ResponsesWSSessionActor) failProofConflict(reason string) {
	if a == nil {
		return
	}
	logCtx := context.Background()
	ctx := a.Context()
	if ctx != nil && ctx.Request != nil {
		logCtx = ctx.Request.Context()
	}
	// NotSent plus provider evidence means the bridge proof and upstream
	// evidence contradict each other. Keep this out of ordinary ambiguous
	// handling so buffered provider events cannot mutate quota or affinity.
	logger.LogError(logCtx, "responses websocket send proof conflict: "+reason)
	if a.pendingAttempt != nil {
		a.pendingAttempt.FinalizeQuotaPreservingPreConsumed(nil)
		a.pendingAttempt = nil
	}
	a.pendingProviderEvents = nil
	a.pendingProviderBytes = 0
	a.pendingProviderEvidenceSeen = false
	a.failClosed(reason)
}

func (a *ResponsesWSSessionActor) close(reason string) {
	if a == nil || a.closed.Swap(true) {
		return
	}
	a.cancelSetup()
	a.releasePendingLease()
	defer a.releaseActiveLease()
	if a.pendingAttempt != nil {
		a.applyBufferedPendingTerminalSideEffects()
		canRollbackPending := !a.hasPendingProviderEvidence() &&
			!a.pendingAttempt.RolledBack &&
			(a.pendingAttempt.SendOutcome == SendOutcomeNotSent ||
				(a.pendingAttempt.SendOutcome == SendOutcomeUnknown &&
					a.pendingTurnPhase != responsesWSPendingTurnSend &&
					a.state != responsesWSStatePendingSend))
		if canRollbackPending {
			a.pendingAttempt.RollbackBeforeLocalWriteOK("session_closed")
		} else {
			a.pendingAttempt.FinalizeQuotaPreservingPreConsumed(nil)
		}
	}
	if a.activeAttempt != nil {
		a.activeAttempt.FinalizeQuotaPreservingPreConsumed(nil)
	}
	if a.session != nil && a.bridge != nil {
		a.bridge.AbortSession(a.session, reason)
	}
	if a.bridge != nil && a.bridge.conn != nil {
		if !a.downstreamCloseSent.Swap(true) {
			a.bridge.WriteCloseControl(websocket.CloseNormalClosure, responsesWSCloseReason(reason))
		}
		_ = a.bridge.conn.Close()
	}
	a.state = responsesWSStateClosed
	a.finish()
}

func (a *ResponsesWSSessionActor) markDownstreamCloseSent() {
	if a != nil {
		a.downstreamCloseSent.Store(true)
	}
}

func responsesWSCloseReason(reason string) string {
	return requester.SafeWSCloseReason(strings.TrimSpace(reason))
}

func ResponsesWebSocket(c *gin.Context) {
	if apiErr := validateRealtimeWebSocketOrigin(c.Request); apiErr != nil {
		common.AbortWithErr(c, apiErr.StatusCode, apiErr)
		return
	}
	if apiErr := middleware.EnsureCurrentUserRequestAllowed(c); apiErr != nil {
		common.AbortWithMessage(c, apiErr.StatusCode, apiErr.Message)
		return
	}
	if !websocket.IsWebSocketUpgrade(c.Request) {
		common.AbortWithMessage(c, http.StatusUpgradeRequired, "websocket_upgrade_required")
		return
	}
	if apiErr := middleware.AllowResponsesWSConnectionAttempt(c); apiErr != nil {
		common.AbortWithMessage(c, apiErr.StatusCode, apiErr.Message)
		return
	}
	markResponsesWSStreamRequest(c)
	pendingLease, apiErr := middleware.AcquireResponsesWSPendingSlot(c)
	if apiErr != nil {
		common.AbortWithMessage(c, apiErr.StatusCode, apiErr.Message)
		return
	}
	defer pendingLease.Release()

	userConn, err := responsesWSUpgrader.Upgrade(c.Writer, c.Request, websocketUpgradeResponseHeader(c.Request))
	if err != nil {
		common.AbortWithMessage(c, http.StatusInternalServerError, "upgrade_failed")
		return
	}
	requester.ApplyWSReadLimit(userConn, config.RealtimeWebsocketReadLimit)
	if err := userConn.SetReadDeadline(time.Now().Add(config.ResponsesWSFirstFrameTimeout())); err != nil {
		_ = userConn.Close()
		return
	}
	mt, raw, err := userConn.ReadMessage()
	firstFrameReceivedAt := time.Now()
	if err != nil {
		writer := requester.NewWSClientWriter(userConn, config.RealtimeWebsocketWriteTimeout)
		_ = writer.WriteMessage(websocket.TextMessage, responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", responsesWSFirstFrameReadErrorMessage(err)))
		_ = writer.Close()
		return
	}
	_ = userConn.SetReadDeadline(time.Time{})
	if mt != websocket.TextMessage {
		writer := requester.NewWSClientWriter(userConn, config.RealtimeWebsocketWriteTimeout)
		_ = writer.WriteClose(websocket.CloseUnsupportedData, "only text frames are supported")
		_ = writer.Close()
		return
	}
	frame, err := responsesws.ParseRawResponsesCreateFrame(raw)
	if err != nil {
		writer := requester.NewWSClientWriter(userConn, config.RealtimeWebsocketWriteTimeout)
		_ = writer.WriteClose(websocket.ClosePolicyViolation, "invalid response.create")
		_ = writer.Close()
		return
	}

	actor := NewResponsesWSSessionActor(c)
	defer func() {
		if recovered := recover(); recovered != nil {
			actor.requestCloseIntent("handler_panic")
			select {
			case <-actor.Done():
			case <-time.After(5 * time.Second):
				actor.releasePendingLease()
				actor.releaseActiveLease()
			}
			panic(recovered)
		}
	}()
	bridge := NewResponsesWSIOBridge(userConn, actor)
	defer bridge.Close()
	actor.SetBridge(bridge)
	actor.Start()
	defer armResponsesWSMaxLifetime(actor)()
	bridge.StartClientReadPump()
	bridge.StartClientPingLoop()
	if !actor.PostReliable(ResponsesWSEventFirstTurnSetup{Frame: frame, PendingLease: pendingLease, ReceivedAt: firstFrameReceivedAt}) {
		pendingLease.Release()
		actor.requestCloseIntent("first_turn_setup_not_queued")
	}
	<-actor.Done()
}

func armResponsesWSMaxLifetime(actor *ResponsesWSSessionActor) func() {
	if actor == nil {
		return func() {}
	}
	maxLifetime := config.ResponsesWSMaxLifetime()
	if maxLifetime <= 0 {
		return func() {}
	}
	timer := time.AfterFunc(maxLifetime, func() {
		actor.PostReliable(ResponsesWSEventTimeout{Reason: "max_lifetime"})
	})
	return func() {
		timer.Stop()
	}
}

func openAndPrimeResponsesWSSession(c *gin.Context, request *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openAndPrimeResponsesWSSessionWithContext(context.Background(), c, request)
}

func openAndPrimeResponsesWSSessionWithContext(openCtx context.Context, c *gin.Context, request *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	if c == nil || request == nil {
		return nil, common.StringErrorWrapperLocal("request is required", "invalid_request_error", http.StatusBadRequest)
	}
	markResponsesWSStreamRequest(c)
	if openCtx == nil {
		openCtx = context.Background()
	}
	candidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{Context: c, Request: request})
	if err != nil {
		logger.LogError(context.Background(), "responses websocket affinity preparation failed: "+err.Error())
		return nil, common.StringErrorWrapperLocal(responsesWSStaticErrorMessage("responses_affinity_conflict"), "responses_affinity_conflict", http.StatusConflict)
	}
	relay := &relayBase{c: c}
	relay.setOriginalModel(request.Model)

	if candidate != nil && candidate.ExplicitPinID > 0 {
		return openResponsesWSSpecificChannelWithContext(openCtx, c, request.Model, candidate, candidate.ExplicitPinID)
	}
	if preferred := currentPreferredChannelID(c); preferred > 0 {
		openResult, openErr := openResponsesWSPreferredChannelWithContext(openCtx, c, request.Model, candidate, preferred)
		if openErr == nil {
			return openResult, nil
		}
		if currentChannelAffinityStrict(c) {
			return nil, openErr
		}
		relay.skipChannelID(preferred)
	}

	retryTimes := realtimeOpenRetryBudget()
	var lastErr *types.OpenAIErrorWithStatusCode
	var lastNonUnsupportedErr *types.OpenAIErrorWithStatusCode
	providerAttempted := false
	unsupportedScans := 0
	unsupportedScanLimit := responsesWSUnsupportedScanLimit()
	for i := retryTimes; i > 0; i-- {
		if err := relay.setProvider(request.Model); err != nil {
			if !providerAttempted && lastErr == nil {
				logger.LogError(context.Background(), "responses websocket channel selection failed: "+err.Error())
				lastErr = common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
			}
			break
		}
		providerAttempted = true
		provider := relay.getProvider()
		channel := provider.GetChannel()
		session, apiErr := openRealtimeSessionWithOptions(provider, relay.modelName, runtimesession.RealtimeOpenOptions{
			Context:            openCtx,
			PreferredTransport: runtimesession.TransportModeResponsesWS,
			RequireWS:          true,
		})
		if apiErr == nil {
			metrics.RecordProvider(c, 200)
			return &responsesWSOpenResult{
				Session:       session,
				Provider:      provider,
				ProviderModel: relay.modelName,
				BillingModel:  relay.getModelName(),
				Channel:       channel,
				Candidate:     candidate,
			}, nil
		}
		lastErr = apiErr
		if openAIErrorCodeString(apiErr.Code, "") == "responses_ws_unsupported_for_channel" {
			relay.skipChannelID(channel.Id)
			unsupportedScans++
			if unsupportedScans < unsupportedScanLimit {
				i++
			}
			continue
		}
		lastNonUnsupportedErr = apiErr
		if !shouldRetry(c, apiErr, channel.Type) {
			break
		}
		relay.skipChannelID(channel.Id)
	}
	if lastNonUnsupportedErr != nil {
		return nil, lastNonUnsupportedErr
	}
	if lastErr == nil {
		lastErr = common.StringErrorWrapperLocal("channel does not support Responses websocket transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
	}
	return nil, lastErr
}

func responsesWSUnsupportedScanLimit() int {
	configured := config.RetryTimes
	if configured <= 0 {
		configured = 1
	}
	if viper.IsSet("responses_ws.unsupported_scan_limit") {
		if value := viper.GetInt("responses_ws.unsupported_scan_limit"); value > 0 {
			configured = value
		}
	}
	model.ChannelGroup.RLock()
	channelCount := len(model.ChannelGroup.Channels)
	model.ChannelGroup.RUnlock()
	if channelCount > 0 && channelCount < configured {
		return channelCount
	}
	return configured
}

func openResponsesWSSpecificChannel(c *gin.Context, modelName string, candidate *ResponsesTurnAffinity, channelID int) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openResponsesWSSpecificChannelWithContext(context.Background(), c, modelName, candidate, channelID)
}

func openResponsesWSSpecificChannelWithContext(openCtx context.Context, c *gin.Context, modelName string, candidate *ResponsesTurnAffinity, channelID int) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	markResponsesWSStreamRequest(c)
	channel, err := fetchChannelById(channelID)
	if err != nil {
		logger.LogError(context.Background(), "responses websocket pinned channel fetch failed: "+err.Error())
		return nil, common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
	}
	return openResponsesWSSelectedChannelWithContext(openCtx, c, modelName, candidate, channel)
}

func openResponsesWSPreferredChannel(c *gin.Context, modelName string, candidate *ResponsesTurnAffinity, channelID int) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openResponsesWSPreferredChannelWithContext(context.Background(), c, modelName, candidate, channelID)
}

func openResponsesWSPreferredChannelWithContext(openCtx context.Context, c *gin.Context, modelName string, candidate *ResponsesTurnAffinity, channelID int) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	markResponsesWSStreamRequest(c)
	channel, err := fetchPreferredRealtimeChannel(c, modelName, channelID)
	if err != nil {
		logger.LogError(context.Background(), "responses websocket preferred channel fetch failed: "+err.Error())
		return nil, common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
	}
	return openResponsesWSSelectedChannelWithContext(openCtx, c, modelName, candidate, channel)
}

func openResponsesWSSelectedChannel(c *gin.Context, modelName string, candidate *ResponsesTurnAffinity, channel *model.Channel) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openResponsesWSSelectedChannelWithContext(context.Background(), c, modelName, candidate, channel)
}

func openResponsesWSSelectedChannelWithContext(openCtx context.Context, c *gin.Context, modelName string, candidate *ResponsesTurnAffinity, channel *model.Channel) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	if channel == nil {
		return nil, common.StringErrorWrapperLocal("channel not found", "channel_error", http.StatusServiceUnavailable)
	}
	markResponsesWSStreamRequest(c)
	if openCtx == nil {
		openCtx = context.Background()
	}
	provider, mappedModel, err := prepareProviderForChannel(c, modelName, channel)
	if err != nil {
		logger.LogError(context.Background(), "responses websocket provider preparation failed: "+err.Error())
		return nil, common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
	}
	if candidate != nil {
		candidate.SelectedChannelID = channel.Id
	}
	session, apiErr := openRealtimeSessionWithOptions(provider, mappedModel, runtimesession.RealtimeOpenOptions{
		Context:            openCtx,
		PreferredTransport: runtimesession.TransportModeResponsesWS,
		RequireWS:          true,
	})
	if apiErr != nil {
		return nil, apiErr
	}
	metrics.RecordProvider(c, 200)
	return &responsesWSOpenResult{
		Session:       session,
		Provider:      provider,
		ProviderModel: mappedModel,
		BillingModel:  responsesWSBillingModel(c, modelName, mappedModel),
		Channel:       channel,
		Candidate:     candidate,
	}, nil
}

func markResponsesWSStreamRequest(c *gin.Context) {
	if c != nil {
		c.Set("is_stream", true)
	}
}

func attachResponsesWSSelectedChannelSnapshot(snapshot *ResponsesWSRequestSnapshot, channel *model.Channel, providerModel string, billingModel string) {
	if snapshot == nil || channel == nil {
		return
	}
	selected := &SelectedChannelSnapshot{
		ChannelID:            channel.Id,
		ChannelType:          channel.Type,
		PreCost:              channel.PreCost,
		ProviderModel:        strings.TrimSpace(providerModel),
		BillingModel:         strings.TrimSpace(billingModel),
		OriginalModel:        strings.TrimSpace(snapshot.GetString("original_model")),
		BillingOriginalModel: snapshotBool(snapshot, "billing_original_model"),
		Channel:              channel,
	}
	snapshot.Set("responses_ws_selected_channel_snapshot", selected)
	snapshot.Set("responses_ws_selected_channel", channel)
	snapshot.Set("channel_id", selected.ChannelID)
	snapshot.Set("channel_type", selected.ChannelType)
	snapshot.Set("new_model", selected.ProviderModel)
	snapshot.Set("billing_original_model", selected.BillingOriginalModel)
}

func clearResponsesWSSelectedChannelSnapshot(snapshot *ResponsesWSRequestSnapshot) {
	if snapshot == nil {
		return
	}
	snapshot.Delete(
		"responses_ws_selected_channel_snapshot",
		"responses_ws_selected_channel",
		"channel_id",
		"channel_type",
		"new_model",
		"billing_original_model",
	)
}

func snapshotBool(snapshot *ResponsesWSRequestSnapshot, key string) bool {
	value, ok := snapshot.Get(key)
	if !ok {
		return false
	}
	typed, _ := value.(bool)
	return typed
}

func (r *relayBase) skipChannelID(channelID int) {
	if r == nil || r.c == nil || channelID <= 0 {
		return
	}
	skipChannelIds, ok := r.c.Get("skip_channel_ids")
	if !ok {
		r.c.Set("skip_channel_ids", []int{channelID})
		return
	}
	typed, ok := skipChannelIds.([]int)
	if !ok {
		r.c.Set("skip_channel_ids", []int{channelID})
		return
	}
	r.c.Set("skip_channel_ids", append(typed, channelID))
}

func responsesWSProviderPayload(c *gin.Context, frame *responsesws.RawResponsesCreateFrame, request *types.OpenAIResponsesRequest, mappedModel string) ([]byte, error) {
	if request == nil {
		return nil, errors.New("responses websocket request is required")
	}
	// Raw frame remains the serialization source so unknown fields and exact JSON
	// shapes survive provider rewrite; the corrected typed request owns the model
	// validation shared by affinity, quota estimate, and payload rewrite.
	if strings.TrimSpace(request.Model) == "" {
		return nil, errors.New("response.create model is required")
	}
	providerModel := strings.TrimSpace(mappedModel)
	if providerModel == "" {
		return nil, errors.New("mapped responses websocket model is required")
	}
	return frame.CloneForModel(providerModel)
}

// Raw first-frame read errors can include private socket addresses. Keep code
// stable for clients, but use a precise client-safe message for diagnosis.
func responsesWSFirstFrameReadErrorMessage(err error) string {
	if errors.Is(err, websocket.ErrReadLimit) {
		return "frame is too large or invalid; send smaller audio chunks"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout waiting for first websocket frame"
	}
	return "websocket read failed before first frame"
}

func responsesWSCurrentModelNames(c *gin.Context) (providerModel string, billingModel string) {
	if c == nil {
		return "", ""
	}
	providerModel = strings.TrimSpace(c.GetString("new_model"))
	originalModel := strings.TrimSpace(c.GetString("original_model"))
	billingModel = responsesWSBillingModel(c, originalModel, providerModel)
	return providerModel, billingModel
}

func responsesWSBillingModel(c *gin.Context, originalModel string, providerModel string) string {
	if c != nil && c.GetBool("billing_original_model") && strings.TrimSpace(originalModel) != "" {
		return strings.TrimSpace(originalModel)
	}
	return strings.TrimSpace(providerModel)
}

func responsesWSSubsequentModelMismatch(requestModel string, lockedSessionModel string) string {
	requestModel = strings.TrimSpace(requestModel)
	lockedSessionModel = strings.TrimSpace(lockedSessionModel)
	if requestModel == "" || lockedSessionModel == "" || requestModel == lockedSessionModel {
		return ""
	}
	return fmt.Sprintf("responses websocket session is locked to model %q", lockedSessionModel)
}

func responsesWSSendOutcomeFromError(err error) ResponsesWSSendOutcome {
	if err == nil {
		return SendOutcomeLocalWriteOK
	}
	if errors.Is(err, runtimesession.ErrSessionClosed) {
		return SendOutcomeNotSent
	}
	var event *types.Event
	if errors.As(err, &event) && event != nil && event.IsError() {
		code := openAIErrorCodeString(event.ErrorDetail.Code, "")
		switch code {
		case "previous_response_not_found", "invalid_event", "responses_ws_unsupported_for_channel":
			return SendOutcomeNotSent
		default:
			return SendOutcomeAmbiguous
		}
	}
	return SendOutcomeAmbiguous
}

func responsesWSErrorPayload(status int, code string, message string) []byte {
	payload := map[string]any{
		"type":   "error",
		"status": status,
		"error": map[string]any{
			"type":    "invalid_request_error",
			"code":    code,
			"message": message,
		},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

func responsesWSFallbackPayload() []byte {
	return responsesWSErrorPayload(http.StatusUpgradeRequired, "responses_ws_unsupported_for_channel", "channel does not support Responses websocket transport")
}

func responsesWSErrorFromOpenAI(apiErr *types.OpenAIErrorWithStatusCode) []byte {
	if apiErr == nil {
		return responsesWSErrorPayload(http.StatusInternalServerError, "system_error", "system error")
	}
	errType := strings.TrimSpace(apiErr.Type)
	if errType == "" {
		errType = "one_hub_error"
	}
	code := openAIErrorCodeString(apiErr.Code, "system_error")
	message := responsesWSClientMessageFromOpenAI(apiErr, code)
	param := responsesWSClientParamFromOpenAI(apiErr)
	payload := map[string]any{
		"type":   "error",
		"status": apiErr.StatusCode,
		"error": map[string]any{
			"type":    errType,
			"code":    code,
			"message": message,
			"param":   param,
		},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

func responsesWSClientMessageFromOpenAI(apiErr *types.OpenAIErrorWithStatusCode, code string) string {
	if apiErr == nil {
		return responsesWSStaticErrorMessage("system_error")
	}
	if !apiErr.LocalError && strings.TrimSpace(apiErr.Message) != "" && apiErr.StatusCode < http.StatusInternalServerError {
		return apiErr.Message
	}
	return responsesWSStaticErrorMessage(code)
}

func responsesWSClientParamFromOpenAI(apiErr *types.OpenAIErrorWithStatusCode) string {
	if apiErr == nil || apiErr.LocalError {
		return ""
	}
	param := strings.TrimSpace(apiErr.Param)
	if param == "" {
		return ""
	}
	switch param {
	case "model", "input", "instructions", "tools", "tool_choice", "temperature", "top_p", "max_output_tokens", "previous_response_id", "metadata", "stream":
		return param
	default:
		return ""
	}
}

func responsesWSErrorFromErr(err error) []byte {
	if err == nil {
		return nil
	}
	if payload := runtimesession.ClientPayloadFromError(err); len(payload) > 0 {
		return payload
	}
	var event *types.Event
	if errors.As(err, &event) && event != nil && event.IsError() {
		code := openAIErrorCodeString(event.ErrorDetail.Code, "upstream_error")
		message := responsesWSStaticErrorMessage(code)
		return responsesWSErrorPayload(http.StatusBadGateway, code, message)
	}
	logger.LogError(context.Background(), "responses websocket upstream error: "+err.Error())
	return responsesWSErrorPayload(http.StatusBadGateway, "upstream_error", responsesWSStaticErrorMessage("upstream_error"))
}

func responsesWSStaticErrorMessage(code string) string {
	switch strings.TrimSpace(code) {
	case "invalid_event":
		return "invalid response.create event"
	case "responses_affinity_conflict":
		return "responses affinity conflict"
	case "quota_rollback_failed":
		return "quota rollback failed"
	case "responses_ws_attempt_failed":
		return "responses websocket turn attempt failed"
	case "responses_ws_payload_rewrite_failed":
		return "internal payload rewrite failed"
	case "responses_ws_send_queue_full":
		return "responses websocket upstream send queue is full"
	case "previous_response_not_found":
		return "previous response was not found"
	case "provider_connection_closed":
		return "upstream websocket connection closed"
	case "ws_write_failed":
		return "upstream websocket write failed"
	case "upstream_error":
		return "upstream websocket request failed"
	default:
		return "responses websocket request failed"
	}
}

func isResponsesContinuationMissError(err error) bool {
	if err == nil {
		return false
	}
	payload := responsesWSErrorFromErr(err)
	if len(payload) == 0 {
		return false
	}
	return responsesws.ClassifyResponsesWSEvent(payload).ContinuationMiss || strings.Contains(strings.ToLower(err.Error()), "previous_response_not_found")
}

func mergeResponsesWSUsageEvent(usage *types.Usage, event *types.UsageEvent) {
	if usage == nil || event == nil {
		return
	}
	usage.PromptTokens += event.InputTokens
	usage.CompletionTokens += event.OutputTokens
	usage.TotalTokens += event.TotalTokens
	usage.PromptTokensDetails.Merge(&event.InputTokenDetails)
	usage.CompletionTokensDetails.Merge(&event.OutputTokenDetails)
	usage.ExtraTokens = mergeIntMaps(usage.ExtraTokens, event.ExtraTokens)
	usage.MergeExtraBilling(event.ExtraBilling)
}

func mergeResponsesWSResponsesUsage(usage *types.Usage, responseUsage *types.ResponsesUsage) {
	if usage == nil || responseUsage == nil {
		return
	}
	if responseUsage.InputTokens > 0 {
		usage.PromptTokens = responseUsage.InputTokens
	}
	if responseUsage.OutputTokens > 0 {
		usage.CompletionTokens = responseUsage.OutputTokens
	}
	if responseUsage.TotalTokens > 0 {
		usage.TotalTokens = responseUsage.TotalTokens
	}
	if responseUsage.InputTokensDetails != nil {
		overwritePositiveInt(&usage.PromptTokensDetails.AudioTokens, responseUsage.InputTokensDetails.AudioTokens)
		overwritePositiveInt(&usage.PromptTokensDetails.CachedTokens, responseUsage.InputTokensDetails.CachedTokens)
		overwritePositiveInt(&usage.PromptTokensDetails.CachedReadTokens, responseUsage.InputTokensDetails.CachedReadTokens)
		overwritePositiveInt(&usage.PromptTokensDetails.CachedWriteTokens, responseUsage.InputTokensDetails.CachedWriteTokens)
		overwritePositiveInt(&usage.PromptTokensDetails.TextTokens, responseUsage.InputTokensDetails.TextTokens)
		overwritePositiveInt(&usage.PromptTokensDetails.ImageTokens, responseUsage.InputTokensDetails.ImageTokens)
	}
	if responseUsage.OutputTokensDetails != nil {
		overwritePositiveInt(&usage.CompletionTokensDetails.ReasoningTokens, responseUsage.OutputTokensDetails.ReasoningTokens)
	}
}

func overwritePositiveInt(dst *int, src int) {
	if dst != nil && src > 0 {
		*dst = src
	}
}

func mergeResponsesWSTerminalResponse(usage *types.Usage, response *types.OpenAIResponsesResponses) {
	if usage == nil || response == nil {
		return
	}
	mergeResponsesWSResponsesUsage(usage, response.Usage)
	// Terminal response output is the fallback source for Responses tool billing.
	// Provider UsageEvents can already contain the same charges, so merge by max
	// count per normalized key rather than adding and risking double billing.
	usage.ExtraBilling = mergeExtraBillingMapsMax(usage.ExtraBilling, types.GetResponsesExtraBilling(response))
}

func responsesWSUsageHasBillableEvidence(usage *types.Usage) bool {
	if usage == nil {
		return false
	}
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.TotalTokens > 0 {
		return true
	}
	if len(usage.GetExtraTokens()) > 0 {
		return true
	}
	for _, extra := range usage.ExtraBilling {
		if extra.CallCount > 0 {
			return true
		}
	}
	return false
}

func mergeIntMaps(dst map[string]int, src map[string]int) map[string]int {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]int, len(src))
	}
	for key, value := range src {
		dst[key] += value
	}
	return dst
}

func mergeExtraBillingMapsMax(dst map[string]types.ExtraBilling, src map[string]types.ExtraBilling) map[string]types.ExtraBilling {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]types.ExtraBilling, len(src))
	}
	for key, value := range src {
		serviceType := types.ResolveExtraBillingServiceType(key, value)
		bType := types.ResolveExtraBillingType(key, value)
		normalizedKey := types.BuildExtraBillingKey(serviceType, bType)
		if normalizedKey == "" {
			continue
		}
		value.ServiceType = serviceType
		value.Type = bType
		if existing, ok := dst[normalizedKey]; ok && existing.CallCount >= value.CallCount {
			continue
		}
		dst[normalizedKey] = value
	}
	return dst
}

func logResponsesWSError(c *gin.Context, message string) {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	logger.LogError(ctx, message)
}
