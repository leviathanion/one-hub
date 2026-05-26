package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"one-api/common/logger"
	"one-api/common/wsconn"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const realtimeRelayClientFrameQueueSize = 128

const (
	realtimeRelayStateOpen int32 = iota
	realtimeRelayStateExitEmitted
	realtimeRelayStateClosing
)

type realtimeRelayClientFrame struct {
	mt      wsconn.MessageType
	payload []byte
}

type realtimeRelayExit struct {
	source                string
	err                   error
	graceful              bool
	downstreamCloseCode   wsconn.CloseCode
	downstreamCloseReason string
	hasDownstreamClose    bool
}

type realtimeRelayActor struct {
	client  *wsconn.ManagedConn
	session runtimesession.RealtimeSession
	timeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	clientFrames   chan realtimeRelayClientFrame
	done           chan struct{}
	userClosed     chan struct{}
	supplierClosed chan struct{}
	exitCh         chan realtimeRelayExit

	workers       sync.WaitGroup
	doneOnce      sync.Once
	userCloseOnce sync.Once

	started              atomic.Bool
	closeState           atomic.Int32
	dropDownstreamWrites atomic.Bool
	exitDropLogged       atomic.Bool
	lastActivityUnixNano atomic.Int64

	providerPayloadObserver func(wsconn.MessageType, []byte)
}

func newRealtimeRelayActor(client *wsconn.ManagedConn, session runtimesession.RealtimeSession, timeout time.Duration) *realtimeRelayActor {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &realtimeRelayActor{
		client:         client,
		session:        session,
		timeout:        timeout,
		ctx:            ctx,
		cancel:         cancel,
		clientFrames:   make(chan realtimeRelayClientFrame, realtimeRelayClientFrameQueueSize),
		done:           make(chan struct{}),
		userClosed:     make(chan struct{}),
		supplierClosed: make(chan struct{}),
		exitCh:         make(chan realtimeRelayExit, 16),
	}
	b.markActivity(time.Now())
	return b
}

func (b *realtimeRelayActor) Start() {
	if b == nil || !b.started.CompareAndSwap(false, true) {
		return
	}
	if b.closeState.Load() != realtimeRelayStateOpen {
		b.signalDone()
		return
	}
	b.workers.Add(4)
	go b.runWorker(b.clientPump)
	go b.runWorker(b.clientToSession)
	go b.runWorker(b.sessionToClient)
	go b.runWorker(b.idleWatchdog)
	go b.coordinate()
}

func (b *realtimeRelayActor) Wait() {
	<-b.done
}

func (b *realtimeRelayActor) Close() {
	if b.started.Load() {
		if b.beginExternalClose() {
			b.emitExit(realtimeRelayExit{source: "external", err: context.Canceled, graceful: true})
		}
		return
	}
	if b.beginClose() {
		b.cancel()
		if b.client != nil {
			b.client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "proxy_closed"})
		}
		if b.session != nil {
			b.safeSessionAction("detach", func() {
				b.session.Detach("proxy_closed")
			})
		}
		b.signalDone()
	}
}

func (b *realtimeRelayActor) beginClose() bool {
	return b != nil && b.closeState.CompareAndSwap(realtimeRelayStateOpen, realtimeRelayStateClosing)
}

func (b *realtimeRelayActor) beginExternalClose() bool {
	return b != nil && b.closeState.CompareAndSwap(realtimeRelayStateOpen, realtimeRelayStateExitEmitted)
}

func (b *realtimeRelayActor) coordinateClose() bool {
	if b == nil {
		return false
	}
	for {
		state := b.closeState.Load()
		switch state {
		case realtimeRelayStateOpen, realtimeRelayStateExitEmitted:
			if b.closeState.CompareAndSwap(state, realtimeRelayStateClosing) {
				return true
			}
		default:
			return false
		}
	}
}

func (b *realtimeRelayActor) UserClosed() <-chan struct{} {
	return b.userClosed
}

func (b *realtimeRelayActor) SupplierClosed() <-chan struct{} {
	return b.supplierClosed
}

func (b *realtimeRelayActor) clientPump() {
	pump := wsconn.Pump{
		Conn: b.client,
		Handle: func(_ context.Context, mt wsconn.MessageType, payload []byte) {
			frame := realtimeRelayClientFrame{mt: mt, payload: append([]byte(nil), payload...)}
			select {
			case b.clientFrames <- frame:
				b.markActivity(time.Now())
			default:
				if b.client != nil {
					b.client.Close(wsconn.CloseInfo{
						Kind:   wsconn.CloseKindBackpressure,
						Code:   wsconn.CloseTryAgainLater,
						Reason: "client_frame_backpressure",
					})
				}
			}
		},
		OnClose: func(info wsconn.CloseInfo) {
			b.userCloseOnce.Do(func() {
				close(b.clientFrames)
				close(b.userClosed)
			})
			b.emitExit(realtimeRelayExit{
				source:   "user",
				err:      info.Err,
				graceful: realtimeRelayClientCloseIsGraceful(info),
			})
		},
	}
	pump.Run(b.ctx)
}

func (b *realtimeRelayActor) clientToSession() {
	for {
		select {
		case <-b.ctx.Done():
			return
		case frame, ok := <-b.clientFrames:
			if !ok {
				return
			}
			if b.session == nil {
				b.emitExit(realtimeRelayExit{source: "user", err: net.ErrClosed})
				return
			}
			if err := b.session.SendClient(b.ctx, realtimeRelayFrameFromMessage(frame.mt, frame.payload)); err != nil {
				if payload := realtimeRelayErrorPayload(err); payload != nil && !b.dropDownstreamWrites.Load() {
					if writeErr := b.writeClientMessage(wsconn.TextMessage, payload); writeErr != nil {
						b.emitExit(realtimeRelayExit{source: "user", err: writeErr})
						return
					}
					if realtimeRelayRecoverableError(err) {
						continue
					}
				}
				b.emitExit(realtimeRelayExit{source: "user", err: err})
				return
			}
		}
	}
}

func (b *realtimeRelayActor) sessionToClient() {
	defer close(b.supplierClosed)

	if b.session == nil {
		b.emitExit(realtimeRelayExit{source: "supplier", err: net.ErrClosed})
		return
	}

	for {
		event, err := b.session.Recv(b.ctx)
		if err != nil {
			if event.ProviderClose != nil {
				exit := b.providerCloseExit(event.ProviderClose, err)
				b.closeDownstream(exit.downstreamCloseCode, exit.downstreamCloseReason)
				b.emitExit(exit)
				return
			}
			deliveredPayload := b.deliverEventFrame(event)
			if !b.dropDownstreamWrites.Load() {
				if errorPayload := runtimesession.ClientPayloadFromError(err); errorPayload != nil {
					deliveredFramePayload := []byte(nil)
					if event.Frame != nil {
						deliveredFramePayload = event.Frame.Payload()
					}
					if !(deliveredPayload && bytes.Equal(errorPayload, deliveredFramePayload)) {
						_ = b.writeClientMessage(wsconn.TextMessage, errorPayload)
					}
				}
			}
			b.emitExit(realtimeRelayExit{source: "supplier", err: err, graceful: errors.Is(err, runtimesession.ErrSessionClosed) || errors.Is(err, context.Canceled)})
			return
		}

		b.markActivity(time.Now())
		if event.ProviderClose != nil {
			exit := b.providerCloseExit(event.ProviderClose, event.ProviderClose.Err)
			b.closeDownstream(exit.downstreamCloseCode, exit.downstreamCloseReason)
			b.emitExit(exit)
			return
		}
		if event.Err != nil {
			deliveredPayload := b.deliverEventFrame(event)
			if !b.dropDownstreamWrites.Load() {
				if errorPayload := runtimesession.ClientPayloadFromError(event.Err); errorPayload != nil {
					deliveredFramePayload := []byte(nil)
					if event.Frame != nil {
						deliveredFramePayload = event.Frame.Payload()
					}
					if !(deliveredPayload && bytes.Equal(errorPayload, deliveredFramePayload)) {
						_ = b.writeClientMessage(wsconn.TextMessage, errorPayload)
					}
				}
			}
			b.emitExit(realtimeRelayExit{source: "supplier", err: event.Err})
			return
		}
		b.deliverEventFrame(event)
	}
}

func (b *realtimeRelayActor) deliverEventFrame(event runtimesession.RecvEvent) bool {
	if event.Frame == nil || len(event.Frame.Payload()) == 0 || b.dropDownstreamWrites.Load() {
		return false
	}
	mt := realtimeRelayMessageTypeFromFrame(*event.Frame)
	payload := event.Frame.Payload()
	if err := b.writeClientMessage(mt, payload); err != nil {
		b.emitExit(realtimeRelayExit{source: "supplier", err: err, graceful: realtimeRelayDisconnectError(err)})
		return false
	}
	b.observeProviderPayload(mt, payload, event.Origin)
	return true
}

func (b *realtimeRelayActor) providerCloseExit(closeInfo *runtimesession.ProviderClose, fallbackErr error) realtimeRelayExit {
	if closeInfo == nil {
		return realtimeRelayExit{source: "supplier", err: fallbackErr, graceful: errors.Is(fallbackErr, runtimesession.ErrSessionClosed) || errors.Is(fallbackErr, context.Canceled)}
	}
	err := closeInfo.Err
	if err == nil {
		err = fallbackErr
	}
	return realtimeRelayExit{
		source:                "supplier",
		err:                   err,
		graceful:              true,
		hasDownstreamClose:    true,
		downstreamCloseCode:   wsconn.SanitizeWireCloseCode(closeInfo.Code),
		downstreamCloseReason: closeInfo.Reason,
	}
}

func (b *realtimeRelayActor) idleWatchdog() {
	if b.timeout <= 0 {
		return
	}
	checkInterval := minRealtimeActorDuration(b.timeout/4, 5*time.Second)
	if checkInterval < time.Second {
		checkInterval = time.Second
	}
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			lastActivity := time.Unix(0, b.lastActivityUnixNano.Load())
			if time.Since(lastActivity) < b.timeout {
				continue
			}
			b.emitExit(realtimeRelayExit{source: "idle", err: context.DeadlineExceeded})
			return
		}
	}
}

func (b *realtimeRelayActor) coordinate() {
	defer b.finishCoordinate()

	var first realtimeRelayExit
	select {
	case first = <-b.exitCh:
	case <-b.ctx.Done():
		first = realtimeRelayExit{source: "ctx", err: b.ctx.Err(), graceful: true}
	}
	reason := realtimeActorDetachReason(first.source)

	if first.source == "user" && first.graceful {
		b.dropDownstreamWrites.Store(true)
	}

	if b.coordinateClose() {
		switch {
		case b.session == nil:
		case first.source == "idle":
			b.session.Abort(reason)
		case first.source == "user" && !first.graceful:
			b.session.Abort(reason)
		case first.source == "user" && first.graceful && !sessionSupportsGracefulDetach(b.session):
			b.session.Abort(reason)
		default:
			b.session.Detach(reason)
		}
		if first.hasDownstreamClose {
			b.closeDownstream(first.downstreamCloseCode, first.downstreamCloseReason)
		} else {
			b.closeDownstream(wsconn.CloseNormalClosure, reason)
		}
	}
}

func (b *realtimeRelayActor) runWorker(fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.SysError(fmt.Sprintf("realtime relay actor worker panic: %v", recovered))
			b.emitExit(realtimeRelayExit{source: "proxy_panic", err: fmt.Errorf("proxy_panic: %v", recovered)})
			b.emergencyShutdown("proxy_panic")
		}
		b.workers.Done()
	}()
	fn()
}

func (b *realtimeRelayActor) finishCoordinate() {
	if recovered := recover(); recovered != nil {
		logger.SysError(fmt.Sprintf("realtime relay actor coordinator panic: %v", recovered))
		b.emergencyShutdown("proxy_panic")
	}
	b.workers.Wait()
	b.signalDone()
}

func (b *realtimeRelayActor) emitExit(exit realtimeRelayExit) {
	select {
	case b.exitCh <- exit:
	default:
		if b != nil && b.exitDropLogged.CompareAndSwap(false, true) {
			logger.SysError(fmt.Sprintf("realtime relay actor exit signal dropped: source=%s err=%v", exit.source, exit.err))
		}
	}
}

func (b *realtimeRelayActor) signalDone() {
	b.doneOnce.Do(func() {
		close(b.done)
	})
}

func (b *realtimeRelayActor) emergencyShutdown(reason string) {
	if b == nil {
		return
	}
	if b.cancel != nil {
		b.cancel()
	}
	if b.client != nil {
		b.client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: strings.TrimSpace(reason)})
	}
	if b.session != nil {
		b.safeSessionAction("abort", func() {
			b.session.Abort(strings.TrimSpace(reason))
		})
	}
}

func (b *realtimeRelayActor) closeDownstream(code wsconn.CloseCode, reason string) {
	if b == nil {
		return
	}
	if b.cancel != nil {
		b.cancel()
	}
	if b.client != nil {
		b.client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Code: code, Reason: reason})
	}
}

func (b *realtimeRelayActor) safeSessionAction(label string, fn func()) {
	if fn == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.SysError(fmt.Sprintf("realtime relay actor session %s panic: %v", strings.TrimSpace(label), recovered))
			if b.cancel != nil {
				b.cancel()
			}
			if b.client != nil {
				b.client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "session_panic"})
			}
			if !b.started.Load() {
				b.signalDone()
			}
		}
	}()
	fn()
}

func (b *realtimeRelayActor) writeClientMessage(mt wsconn.MessageType, payload []byte) error {
	if b == nil || b.client == nil {
		return net.ErrClosed
	}
	return b.client.WriteMessage(mt, payload)
}

func (b *realtimeRelayActor) observeProviderPayload(mt wsconn.MessageType, payload []byte, origin runtimesession.RealtimePayloadOrigin) {
	if b == nil || origin != runtimesession.RealtimePayloadOriginProvider || len(payload) == 0 || b.providerPayloadObserver == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.SysError(fmt.Sprintf("realtime relay actor provider payload observer panic: %v", recovered))
		}
	}()
	b.providerPayloadObserver(mt, append([]byte(nil), payload...))
}

func (b *realtimeRelayActor) markActivity(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	b.lastActivityUnixNano.Store(now.UnixNano())
}

func realtimeRelayFrameFromMessage(mt wsconn.MessageType, payload []byte) runtimesession.Frame {
	if mt == wsconn.BinaryMessage {
		return runtimesession.NewBinaryFrame(payload)
	}
	return runtimesession.NewTextFrame(payload)
}

func realtimeRelayMessageTypeFromFrame(frame runtimesession.Frame) wsconn.MessageType {
	if frame.Kind() == runtimesession.FrameKindBinary {
		return wsconn.BinaryMessage
	}
	return wsconn.TextMessage
}

func realtimeRelayErrorPayload(err error) []byte {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, runtimesession.ErrSessionClosed) {
		return nil
	}
	if payload := runtimesession.ClientPayloadFromError(err); len(payload) > 0 {
		return payload
	}

	var event *types.Event
	if errors.As(err, &event) && event != nil && event.IsError() {
		code := "system_error"
		if event.ErrorDetail != nil && event.ErrorDetail.Code != nil {
			code = strings.TrimSpace(fmt.Sprint(event.ErrorDetail.Code))
		}
		return []byte(types.NewErrorEvent("", "system_error", code, realtimeRelayStaticErrorMessage(code)).Error())
	}

	logger.SysError(fmt.Sprintf("realtime relay actor internal error: %v", err))
	return []byte(types.NewErrorEvent("", "system_error", "system_error", realtimeRelayStaticErrorMessage("system_error")).Error())
}

func realtimeRelayStaticErrorMessage(code string) string {
	switch strings.TrimSpace(code) {
	case "session_busy":
		return "realtime session is busy"
	case "ws_write_failed":
		return "upstream websocket write failed"
	case "provider_connection_closed":
		return "upstream websocket connection closed"
	default:
		return "realtime proxy request failed"
	}
}

func realtimeRelayRecoverableError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, runtimesession.ErrSessionClosed) {
		return false
	}

	var event *types.Event
	if !errors.As(err, &event) || event == nil || !event.IsError() || event.ErrorDetail == nil {
		return false
	}
	code := strings.TrimSpace(fmt.Sprint(event.ErrorDetail.Code))
	switch code {
	case "invalid_event", "unsupported_client_event", "session_busy":
		return true
	default:
		return false
	}
}

func realtimeRelayClientCloseIsGraceful(info wsconn.CloseInfo) bool {
	switch info.Kind {
	case wsconn.CloseKindPeerClose, wsconn.CloseKindNormal, wsconn.CloseKindGracefulShutdown:
		return true
	default:
		return realtimeRelayDisconnectError(info.Err)
	}
}

func realtimeRelayDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	var closeErr *wsconn.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
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

func realtimeActorDetachReason(source string) string {
	switch strings.TrimSpace(source) {
	case "supplier":
		return "supplier_closed"
	case "idle":
		return "idle_timeout"
	case "external":
		return "proxy_closed"
	case "proxy_panic":
		return "proxy_panic"
	default:
		return "user_closed"
	}
}

func sessionSupportsGracefulDetach(session runtimesession.RealtimeSession) bool {
	detachable, ok := session.(runtimesession.GracefulDetachCapable)
	return ok && detachable.SupportsGracefulDetach()
}

func minRealtimeActorDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}
