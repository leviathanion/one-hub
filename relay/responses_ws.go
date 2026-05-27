package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/middleware"
	"one-api/model"
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
)

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

type responsesWSClientWriter interface {
	WriteFrame(mt int, payload []byte, mode ResponsesWSWriteMode) error
	CloseWithCode(code int, reason string)
	Abort(reason string)
}

type responsesWSManagedClientWriter struct {
	conn *wsconn.ManagedConn
}

func newResponsesWSManagedClientWriter(conn *wsconn.ManagedConn) *responsesWSManagedClientWriter {
	if conn == nil {
		return nil
	}
	return &responsesWSManagedClientWriter{conn: conn}
}

func (w *responsesWSManagedClientWriter) WriteFrame(mt int, payload []byte, _ ResponsesWSWriteMode) error {
	if w == nil || w.conn == nil || len(payload) == 0 {
		return nil
	}
	if mt == responsesWSCloseMessageType {
		code, reason := parseResponsesWSClosePayload(payload)
		w.CloseWithCode(code, reason)
		return nil
	}
	return w.conn.WriteMessage(wsconn.MessageType(mt), payload)
}

func (w *responsesWSManagedClientWriter) CloseWithCode(code int, reason string) {
	if w == nil || w.conn == nil {
		return
	}
	w.conn.Close(wsconn.CloseInfo{
		Kind:   wsconn.CloseKindNormal,
		Code:   wsconn.SanitizeWireCloseCode(code),
		Reason: reason,
	})
}

func (w *responsesWSManagedClientWriter) Abort(reason string) {
	if w == nil || w.conn == nil {
		return
	}
	w.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: strings.TrimSpace(reason)})
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
	ctx    context.Context
	cancel context.CancelFunc
	writer responsesWSClientWriter
	actor  *ResponsesWSSessionActor
	armed  sync.Map
	wg     sync.WaitGroup
}

func NewResponsesWSManagedBridge(conn *wsconn.ManagedConn, actor *ResponsesWSSessionActor) *ResponsesWSIOBridge {
	ctx, cancel := context.WithCancel(context.Background())
	bridge := &ResponsesWSIOBridge{
		ctx:    ctx,
		cancel: cancel,
		writer: newResponsesWSManagedClientWriter(conn),
		actor:  actor,
	}
	return bridge
}

func newResponsesWSBridgeForTest(writer responsesWSClientWriter, actor *ResponsesWSSessionActor) *ResponsesWSIOBridge {
	ctx, cancel := context.WithCancel(context.Background())
	return &ResponsesWSIOBridge{
		ctx:    ctx,
		cancel: cancel,
		writer: writer,
		actor:  actor,
	}
}

func responsesWSClientWSConfig() wsconn.Config {
	inboundActivityTimeout := config.ResponsesWebsocketClientInboundActivityTimeout()
	writeTimeout := config.RealtimeWebsocketWriteTimeout()
	return wsconn.Config{
		Label:           "client-responses-ws",
		PingInterval:    config.ResponsesWebsocketClientPingInterval(),
		PongMissTimeout: config.ResponsesWebsocketClientPongMissTimeout(),
		InboundActivityTimeout: func() time.Duration {
			return inboundActivityTimeout
		},
		ReadLimit:    config.RealtimeWebsocketReadLimit(),
		WriteTimeout: func() time.Duration { return writeTimeout },
	}
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
			event, err := session.Recv(b.ctx)
			receivedAt := time.Now()
			if responsesWSRecvHasProviderActivity(event, err) {
				b.actor.markActivity()
			}
			if event.ProviderClose != nil {
				b.actor.PostReliable(ResponsesWSEventProviderClosed{
					UpstreamSessionGeneration: upstreamSessionGeneration,
					ChannelID:                 selectedChannelID,
					Code:                      event.ProviderClose.Code,
					Reason:                    event.ProviderClose.Reason,
					Err:                       event.ProviderClose.Err,
					ReceivedAt:                receivedAt,
				})
				return
			}
			hasFrame := event.Frame != nil && len(event.Frame.Payload()) > 0
			if event.Usage != nil && !hasFrame {
				b.actor.PostReliable(ResponsesWSEventProviderUsageObserved{
					UpstreamSessionGeneration: upstreamSessionGeneration,
					ChannelID:                 selectedChannelID,
					Usage:                     event.Usage,
					Origin:                    event.Origin,
					ReceivedAt:                receivedAt,
				})
			}
			if err != nil {
				deliveredPayload := hasFrame
				if deliveredPayload {
					mt, payload := responsesWSMessageFromSessionFrame(*event.Frame)
					b.actor.PostReliable(ResponsesWSEventProviderDownstream{
						UpstreamSessionGeneration: upstreamSessionGeneration,
						ChannelID:                 selectedChannelID,
						Kind:                      ProviderDownstreamFrame,
						MessageType:               mt,
						Payload:                   payload,
						Usage:                     event.Usage,
						Origin:                    event.Origin,
						ReceivedAt:                receivedAt,
					})
				}
				deliveredClientErrorPayload := false
				if errorPayload := runtimesession.ClientPayloadFromError(err); len(errorPayload) > 0 {
					var deliveredFramePayload []byte
					if event.Frame != nil {
						deliveredFramePayload = event.Frame.Payload()
					}
					deliveredClientErrorPayload = deliveredPayload && bytes.Equal(errorPayload, deliveredFramePayload)
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
					b.actor.PostReliable(ResponsesWSEventProviderRecvFailed{
						UpstreamSessionGeneration: upstreamSessionGeneration,
						ChannelID:                 selectedChannelID,
						Err:                       err,
					})
				}
				return
			}
			if event.Err != nil {
				deliveredPayload := hasFrame
				if deliveredPayload {
					mt, payload := responsesWSMessageFromSessionFrame(*event.Frame)
					b.actor.PostReliable(ResponsesWSEventProviderDownstream{
						UpstreamSessionGeneration: upstreamSessionGeneration,
						ChannelID:                 selectedChannelID,
						Kind:                      ProviderDownstreamFrame,
						MessageType:               mt,
						Payload:                   payload,
						Usage:                     event.Usage,
						Origin:                    event.Origin,
						ReceivedAt:                receivedAt,
					})
				}
				deliveredClientErrorPayload := false
				if errorPayload := runtimesession.ClientPayloadFromError(event.Err); len(errorPayload) > 0 {
					var deliveredFramePayload []byte
					if event.Frame != nil {
						deliveredFramePayload = event.Frame.Payload()
					}
					deliveredClientErrorPayload = deliveredPayload && bytes.Equal(errorPayload, deliveredFramePayload)
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
					b.actor.PostReliable(ResponsesWSEventProviderBusinessError{
						UpstreamSessionGeneration: upstreamSessionGeneration,
						ChannelID:                 selectedChannelID,
						Err:                       event.Err,
					})
				}
				return
			}
			if !hasFrame {
				continue
			}
			mt, payload := responsesWSMessageFromSessionFrame(*event.Frame)
			b.actor.PostReliable(ResponsesWSEventProviderDownstream{
				UpstreamSessionGeneration: upstreamSessionGeneration,
				ChannelID:                 selectedChannelID,
				Kind:                      ProviderDownstreamFrame,
				MessageType:               mt,
				Payload:                   payload,
				Usage:                     event.Usage,
				Origin:                    event.Origin,
				ReceivedAt:                receivedAt,
			})
		}
	}()
}

func responsesWSRecvHasProviderActivity(event runtimesession.RecvEvent, err error) bool {
	if event.Usage != nil || event.Frame != nil && len(event.Frame.Payload()) > 0 || event.Err != nil || event.ProviderClose != nil {
		return true
	}
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, runtimesession.ErrSessionClosed)
}

func (a *ResponsesWSSessionActor) startSendWorker() {
	if a == nil {
		return
	}
	a.sendOnce.Do(func() {
		a.sendWG.Add(1)
		go func() {
			defer a.sendWG.Done()
			defer recoverResponsesWSGoroutine("send_worker", func(reason string) {
				if a != nil {
					a.PostReliable(ResponsesWSEventTimeout{Reason: reason})
				}
			})
			for {
				select {
				case <-a.done:
					return
				case command := <-a.sendCommands:
					a.handleSendCommand(command)
				}
			}
		}()
	})
}

func (a *ResponsesWSSessionActor) handleSendCommand(command responsesWSSendCommand) {
	defer recoverResponsesWSGoroutine("send_command", func(reason string) {
		if a != nil {
			a.PostReliable(ResponsesWSEventSendResult{
				AttemptID:         command.AttemptID,
				SelectedChannelID: command.SelectedChannelID,
				Outcome:           SendOutcomeAmbiguous,
				Err:               errors.New(reason),
			})
		}
	})
	select {
	case <-a.done:
		if a != nil {
			a.PostReliable(ResponsesWSEventSendResult{
				AttemptID:         command.AttemptID,
				SelectedChannelID: command.SelectedChannelID,
				Outcome:           SendOutcomeNotSent,
				Err:               runtimesession.ErrSessionClosed,
			})
		}
		return
	default:
	}
	ctx := context.Background()
	if actorCtx := a.Context(); actorCtx != nil && actorCtx.Request != nil {
		ctx = actorCtx.Request.Context()
	}
	err := command.Session.SendClient(ctx, responsesWSSessionFrameFromMessage(command.MessageType, command.Payload))
	if a != nil {
		a.PostReliable(ResponsesWSEventSendResult{
			AttemptID:         command.AttemptID,
			SelectedChannelID: command.SelectedChannelID,
			Outcome:           responsesWSSendOutcomeFromError(err),
			Err:               err,
		})
	}
}

func responsesWSSessionFrameFromMessage(messageType int, payload []byte) runtimesession.Frame {
	if messageType == responsesWSBinaryMessageType {
		return runtimesession.NewBinaryFrame(payload)
	}
	return runtimesession.NewTextFrame(payload)
}

func responsesWSMessageFromSessionFrame(frame runtimesession.Frame) (int, []byte) {
	if frame.Kind() == runtimesession.FrameKindBinary {
		return responsesWSBinaryMessageType, frame.Payload()
	}
	return responsesWSTextMessageType, frame.Payload()
}

func responsesWSProviderClosePayload(code int, reason string) []byte {
	sanitized := wsconn.SanitizeWireCloseCode(code)
	return wsconn.SafeCloseMessage(sanitized, responsesWSCloseReason(reason))
}

func (a *ResponsesWSSessionActor) SendProviderFrame(attemptID string, selectedChannelID int, session runtimesession.RealtimeSession, mt int, payload []byte) bool {
	if a == nil || session == nil {
		return false
	}
	a.startSendWorker()
	command := responsesWSSendCommand{
		AttemptID:         attemptID,
		SelectedChannelID: selectedChannelID,
		Session:           session,
		MessageType:       mt,
		Payload:           payload,
	}
	select {
	case <-a.done:
		return false
	case a.sendCommands <- command:
		return true
	default:
		return false
	}
}

func (b *ResponsesWSIOBridge) WriteClientFrame(mt int, payload []byte, mode ResponsesWSWriteMode) error {
	if b == nil || len(payload) == 0 {
		return nil
	}
	if b.writer == nil {
		return nil
	}
	return b.writer.WriteFrame(mt, payload, mode)
}

func (b *ResponsesWSIOBridge) WriteCloseControl(code int, reason string) {
	if b == nil {
		return
	}
	if b.writer == nil {
		return
	}
	b.writer.CloseWithCode(code, reason)
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
		b.writer.Abort("bridge_closed")
	}
	b.wg.Wait()
}

func parseResponsesWSClosePayload(payload []byte) (int, string) {
	if len(payload) < 2 {
		return int(wsconn.CloseNormalClosure), ""
	}
	code := int(binary.BigEndian.Uint16(payload[:2]))
	return code, string(payload[2:])
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
	client   *wsconn.ManagedConn
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
	runWG                       sync.WaitGroup
	sendCommands                chan responsesWSSendCommand
	sendOnce                    sync.Once
	sendWG                      sync.WaitGroup
	lastActivityUnixNano        atomic.Int64
	busyRejectWindowStart       time.Time
	busyRejects                 int
	setupCancelMu               sync.Mutex
	setupCancel                 context.CancelFunc
}

func NewResponsesWSSessionActor(c *gin.Context) *ResponsesWSSessionActor {
	actor := &ResponsesWSSessionActor{
		events:       make(chan ResponsesWSEvent, responsesWSEventQueueSize),
		done:         make(chan struct{}),
		sendCommands: make(chan responsesWSSendCommand, responsesWSSendQueueSize),
		state:        responsesWSStateOpening,
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

func (a *ResponsesWSSessionActor) SetClientConn(conn *wsconn.ManagedConn) {
	if a == nil {
		return
	}
	a.client = conn
}

func (a *ResponsesWSSessionActor) Start() {
	a.runWG.Add(2)
	go func() {
		defer a.runWG.Done()
		a.loop()
	}()
	go func() {
		defer a.runWG.Done()
		a.idleWatchdog()
	}()
}

func (a *ResponsesWSSessionActor) Done() <-chan struct{} {
	return a.done
}

func (a *ResponsesWSSessionActor) waitStartedGoroutines() {
	if a == nil {
		return
	}
	a.runWG.Wait()
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
	var managedCloseErr *wsconn.CloseError
	if errors.As(err, &managedCloseErr) {
		switch managedCloseErr.Code {
		case wsconn.CloseNormalClosure, wsconn.CloseGoingAway, wsconn.CloseNoStatusReceived, wsconn.CloseAbnormalClosure:
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
	case ResponsesWSEventProviderUsageObserved:
		a.handleProviderUsageObserved(typed)
	case ResponsesWSEventProviderBusinessError:
		a.handleProviderBusinessError(typed)
	case ResponsesWSEventProviderRecvFailed:
		a.handleProviderRecvFailed(typed)
	case ResponsesWSEventProviderClosed:
		a.handleProviderClosed(typed)
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
				a.bridge.WriteCloseControl(int(wsconn.CloseNormalClosure), "responses_ws_unsupported_for_channel")
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
	if !a.SendProviderFrame(attempt.AttemptID, openResult.Channel.Id, openResult.Session, responsesWSTextMessageType, payload) {
		a.postSendQueueFull(attempt.AttemptID, openResult.Channel.Id)
	}
}

func (a *ResponsesWSSessionActor) handleClientFrame(event ResponsesWSEventClientFrame) {
	if event.MessageType != responsesWSTextMessageType {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", "only text websocket events are supported"))
		return
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(event.Payload, &envelope); err != nil {
		logger.LogError(context.Background(), "responses websocket client frame parse failed: "+err.Error())
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", responsesWSMessageInvalidWebsocketEvent))
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
		if !a.SendProviderFrame("", a.sessionChannelID, a.session, event.MessageType, event.Payload) {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "responses_ws_send_queue_full", responsesWSStaticErrorMessage("responses_ws_send_queue_full")))
		}
	default:
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "unsupported_client_event", "unsupported responses websocket client event"))
	}
}

func (a *ResponsesWSSessionActor) onClientFrame(ctx context.Context, mt wsconn.MessageType, payload []byte) {
	if a == nil {
		return
	}
	select {
	case <-a.done:
		return
	default:
	}
	cloned := append([]byte(nil), payload...)
	a.markActivity()
	event := ResponsesWSEventClientFrame{
		MessageType: int(mt),
		Payload:     cloned,
		ReceivedAt:  time.Now(),
	}
	select {
	case a.events <- event:
	case <-a.done:
		return
	default:
		a.requestCloseIntent("client_frame_backpressure")
		if a.client != nil {
			a.client.Close(wsconn.CloseInfo{
				Kind:   wsconn.CloseKindBackpressure,
				Code:   wsconn.CloseTryAgainLater,
				Reason: "client_frame_backpressure",
				Err:    errResponsesWSClientFrameBackpressure,
			})
		}
	}
	_ = ctx
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
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, responsesWSErrorCodeInvalidResponseCreate, responsesWSMessageInvalidResponseCreate))
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
	if err := a.preflightResponsesWSSend(ctx, frame.EventID, &request); err != nil {
		a.handleResponsesWSPreflightError(err)
		return
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
	if !a.SendProviderFrame(attempt.AttemptID, a.sessionChannelID, a.session, responsesWSTextMessageType, payload) {
		a.postSendQueueFull(attempt.AttemptID, a.sessionChannelID)
	}
}

func (a *ResponsesWSSessionActor) preflightResponsesWSSend(c *gin.Context, eventID string, request *types.OpenAIResponsesRequest) error {
	if a == nil || a.session == nil || request == nil {
		return nil
	}
	preflight, ok := a.session.(runtimesession.ResponsesWSSendPreflightCapable)
	if !ok {
		return nil
	}
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	return preflight.PreflightResponsesWSSend(ctx, eventID, request)
}

func (a *ResponsesWSSessionActor) handleResponsesWSPreflightError(err error) {
	if err == nil {
		return
	}
	a.writeProxyLocal(responsesWSErrorFromErr(err))
	if errors.Is(err, runtimesession.ErrStaleResponsesWSContinuation) {
		a.close("previous_response_not_found")
		return
	}
	a.close("responses_ws_preflight_failed")
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
		if isProviderReportedContinuationMiss(event.Err) {
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
			if errors.Is(event.Err, runtimesession.ErrStaleResponsesWSContinuation) {
				a.close("previous_response_not_found")
			}
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
		if event.Usage != nil {
			a.pendingAttempt.MarkProviderUsageSeen()
			mergeResponsesWSUsageEvent(a.pendingAttempt.Usage, event.Usage)
		}
		if event.Kind == ProviderDownstreamFrame && event.MessageType != responsesWSCloseMessageType && len(event.Payload) > 0 {
			a.pendingAttempt.MarkFirstProviderResponse(receivedAt)
		}
		a.bufferPendingProviderEvent(event)
		return
	}
	if event.MessageType == responsesWSCloseMessageType {
		if a.activeAttempt != nil {
			a.activeAttempt.MarkCompleted(receivedAt)
		}
		a.finalizeActiveAttempt()
		a.clearActiveTurn()
		a.markDownstreamCloseSent()
		if err := a.bridge.WriteClientFrame(responsesWSCloseMessageType, event.Payload, ResponsesWSWriteProvider); err != nil {
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
	if event.Kind == ProviderDownstreamFrame && event.MessageType != responsesWSCloseMessageType && len(event.Payload) > 0 {
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
	if event.Usage != nil {
		a.activeAttempt.MarkProviderUsageSeen()
		mergeResponsesWSUsageEvent(a.activeAttempt.Usage, event.Usage)
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

func (a *ResponsesWSSessionActor) handleProviderUsageObserved(event ResponsesWSEventProviderUsageObserved) {
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstreamSessionGeneration {
		return
	}
	if event.ChannelID > 0 && event.ChannelID != a.sessionChannelID {
		a.failClosed("responses_ws_provider_channel_mismatch")
		return
	}
	if event.Usage == nil {
		return
	}
	if a.shouldDropUnpricedProviderUsage(event.Usage) {
		return
	}
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
}

func (a *ResponsesWSSessionActor) shouldDropUnpricedProviderUsage(usage *types.UsageEvent) bool {
	if usage == nil || usage.Source != types.UsageSourceInputAudioTranscription {
		return false
	}
	modelName := a.billingModelName()
	if transcriptionPricingConfigured(modelName) {
		return false
	}
	recordUsageObservedUnbilled(string(types.UsageSourceInputAudioTranscription), modelName)
	return true
}

func (a *ResponsesWSSessionActor) billingModelName() string {
	if a == nil {
		return ""
	}
	if a.pendingAttempt != nil && a.pendingAttempt.Quota != nil {
		return strings.TrimSpace(a.pendingAttempt.Quota.ModelName())
	}
	if a.activeAttempt != nil && a.activeAttempt.Quota != nil {
		return strings.TrimSpace(a.activeAttempt.Quota.ModelName())
	}
	_, billingModel := responsesWSCurrentModelNames(a.Context())
	return strings.TrimSpace(billingModel)
}

func transcriptionPricingConfigured(modelName string) bool {
	if model.PricingInstance == nil {
		return false
	}
	price := model.PricingInstance.GetPrice(strings.TrimSpace(modelName))
	if price == nil || price.ExtraRatios == nil {
		return false
	}
	ratios := price.ExtraRatios.Data()
	_, ok := ratios[config.UsageExtraInputAudioTranscription]
	return ok
}

func (a *ResponsesWSSessionActor) handleProviderBusinessError(event ResponsesWSEventProviderBusinessError) {
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstreamSessionGeneration {
		return
	}
	if event.ChannelID > 0 && event.ChannelID != a.sessionChannelID {
		a.failClosed("responses_ws_provider_channel_mismatch")
		return
	}
	if event.Err != nil {
		a.writeProxyLocal(responsesWSErrorFromErr(event.Err))
	}
	a.close("provider_business_error")
}

func (a *ResponsesWSSessionActor) handleProviderRecvFailed(event ResponsesWSEventProviderRecvFailed) {
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstreamSessionGeneration {
		return
	}
	if event.ChannelID > 0 && event.ChannelID != a.sessionChannelID {
		a.failClosed("responses_ws_provider_channel_mismatch")
		return
	}
	if payload := runtimesession.ClientPayloadFromError(event.Err); len(payload) > 0 {
		a.writeProxyLocal(payload)
	}
	a.close("provider_recv_failed")
}

func (a *ResponsesWSSessionActor) handleProviderClosed(event ResponsesWSEventProviderClosed) {
	if event.UpstreamSessionGeneration != "" && event.UpstreamSessionGeneration != a.upstreamSessionGeneration {
		return
	}
	if event.ChannelID > 0 && event.ChannelID != a.sessionChannelID {
		a.failClosed("responses_ws_provider_channel_mismatch")
		return
	}
	receivedAt := event.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	if a.pendingAttempt != nil {
		a.pendingProviderEvidenceSeen = true
		a.pendingAttempt.MarkProviderAcceptedTurnEvidence()
		a.bufferPendingProviderEvent(ResponsesWSEventProviderDownstream{
			UpstreamSessionGeneration: event.UpstreamSessionGeneration,
			ChannelID:                 event.ChannelID,
			Kind:                      ProviderDownstreamFrame,
			MessageType:               responsesWSCloseMessageType,
			Payload:                   responsesWSProviderClosePayload(event.Code, event.Reason),
			Err:                       event.Err,
			Origin:                    runtimesession.RealtimePayloadOriginProvider,
			ReceivedAt:                receivedAt,
		})
		return
	}
	if a.activeAttempt != nil {
		a.activeAttempt.MarkCompleted(receivedAt)
	}
	a.finalizeActiveAttempt()
	a.clearActiveTurn()
	a.markDownstreamCloseSent()
	if a.closeProviderDownstream(event) {
		a.close("provider_closed")
		return
	}
	if a.bridge != nil {
		if err := a.bridge.WriteClientFrame(responsesWSCloseMessageType, responsesWSProviderClosePayload(event.Code, event.Reason), ResponsesWSWriteProvider); err != nil {
			a.close("client_write_failed")
			return
		}
	}
	a.close("provider_closed")
}

func (a *ResponsesWSSessionActor) closeProviderDownstream(event ResponsesWSEventProviderClosed) bool {
	if a == nil || a.client == nil {
		return false
	}
	a.client.Close(wsconn.CloseInfo{
		Kind:   wsconn.CloseKindGracefulShutdown,
		Code:   wsconn.SanitizeWireCloseCode(event.Code),
		Reason: event.Reason,
		Err:    event.Err,
	})
	return true
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
	if err := a.bridge.WriteClientFrame(responsesWSTextMessageType, payload, ResponsesWSWriteProvider); err != nil {
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
	if err := a.bridge.WriteClientFrame(responsesWSTextMessageType, payload, ResponsesWSWriteProxyLocal); err != nil {
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
	if a.bridge != nil && a.bridge.writer != nil {
		if !a.downstreamCloseSent.Swap(true) {
			a.bridge.WriteCloseControl(int(wsconn.CloseNormalClosure), responsesWSCloseReason(reason))
		}
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
	return wsconn.SafeCloseReason(strings.TrimSpace(reason))
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
	if !wsconn.IsUpgrade(c.Request) {
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

	clientConn, err := wsconn.AcceptManaged(c.Writer, c.Request, responsesWSClientWSConfig(), wsconn.AcceptOptions{
		CheckOrigin:       realtimeWebSocketOriginAllowed,
		ResponseHeader:    websocketUpgradeResponseHeader(c.Request),
		EnableCompression: false,
		Subprotocols:      echoableClientWebSocketSubprotocols(c.Request),
	})
	if err != nil {
		common.AbortWithMessage(c, http.StatusInternalServerError, "upgrade_failed")
		return
	}
	firstCtx, cancelFirstFrame := context.WithTimeout(c.Request.Context(), config.ResponsesWSFirstFrameTimeout())
	mt, raw, err := clientConn.ReadInitial(firstCtx)
	cancelFirstFrame()
	firstFrameReceivedAt := time.Now()
	if err != nil {
		_ = clientConn.WriteMessage(wsconn.TextMessage, responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", responsesWSFirstFrameReadErrorMessage(err)))
		clientConn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "first_frame_read_failed", Err: err})
		return
	}
	if mt != wsconn.TextMessage {
		clientConn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.CloseUnsupportedData, Reason: "text_only"})
		return
	}
	frame, err := responsesws.ParseRawResponsesCreateFrame(raw)
	if err != nil {
		_ = clientConn.WriteMessage(wsconn.TextMessage, responsesWSErrorPayload(http.StatusBadRequest, responsesWSErrorCodeInvalidResponseCreate, responsesWSMessageInvalidResponseCreate))
		clientConn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.ClosePolicyViolation, Reason: responsesWSErrorCodeInvalidResponseCreate, Err: err})
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
	bridge := NewResponsesWSManagedBridge(clientConn, actor)
	defer bridge.Close()
	actor.SetBridge(bridge)
	actor.SetClientConn(clientConn)
	actor.Start()
	defer armResponsesWSMaxLifetime(actor)()
	pump := wsconn.Pump{
		Conn:    clientConn,
		Handle:  actor.onClientFrame,
		OnClose: func(info wsconn.CloseInfo) { actor.PostReliable(ResponsesWSEventClientClosed{Err: info.Err}) },
	}
	go pump.Run(c.Request.Context())
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
