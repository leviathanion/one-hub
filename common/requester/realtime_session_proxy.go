package requester

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"one-api/common/config"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

type realtimeProxyExit struct {
	source   string
	err      error
	graceful bool
}

type RealtimeSessionProxy struct {
	userConn *websocket.Conn
	writer   *WSClientWriter
	session  runtimesession.RealtimeSession
	timeout  time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	done              chan struct{}
	userClosed        chan struct{}
	supplierClosed    chan struct{}
	exitCh            chan realtimeProxyExit
	doneOnce          sync.Once
	closeOnce         sync.Once
	externalCloseOnce sync.Once
	workers           sync.WaitGroup

	started              atomic.Bool
	dropDownstreamWrites atomic.Bool
	exitDropLogged       atomic.Bool
	lastActivityUnixNano atomic.Int64
}

func NewRealtimeSessionProxy(userConn *websocket.Conn, session runtimesession.RealtimeSession, timeout time.Duration) *RealtimeSessionProxy {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	var writer *WSClientWriter
	if userConn != nil {
		writer = NewWSClientWriter(userConn, config.RealtimeWebsocketWriteTimeout)
	}
	proxy := &RealtimeSessionProxy{
		userConn:       userConn,
		writer:         writer,
		session:        session,
		timeout:        timeout,
		ctx:            ctx,
		cancel:         cancel,
		done:           make(chan struct{}),
		userClosed:     make(chan struct{}),
		supplierClosed: make(chan struct{}),
		exitCh:         make(chan realtimeProxyExit, 6),
	}
	proxy.markActivity(time.Now())
	proxy.configureUserConn()
	return proxy
}

func (p *RealtimeSessionProxy) Start() {
	// Lifecycle calls are owned by the relay handler and must be serialized by
	// the caller; Close is idempotent, but Start racing with Close is unsupported.
	p.started.Store(true)
	p.workers.Add(4)
	go p.runWorker(p.userToSession)
	go p.runWorker(p.sessionToUser)
	go p.runWorker(p.idleWatchdog)
	go p.runWorker(p.clientPingLoop)
	go p.coordinate()
}

func (p *RealtimeSessionProxy) Wait() {
	<-p.done
}

func (p *RealtimeSessionProxy) Close() {
	// See Start for the lifecycle serialization contract.
	if p.started.Load() {
		p.externalCloseOnce.Do(func() {
			p.emitExit(realtimeProxyExit{source: "external", err: context.Canceled, graceful: true})
		})
		return
	}
	p.closeOnce.Do(func() {
		p.cancel()
		if p.writer != nil {
			_ = p.writer.Close()
		} else if p.userConn != nil {
			_ = p.userConn.Close()
		}
		if p.session != nil {
			p.safeSessionAction("detach", func() {
				p.session.Detach("proxy_closed")
			})
		}
	})
}

func (p *RealtimeSessionProxy) UserClosed() <-chan struct{} {
	return p.userClosed
}

func (p *RealtimeSessionProxy) SupplierClosed() <-chan struct{} {
	return p.supplierClosed
}

func (p *RealtimeSessionProxy) userToSession() {
	defer close(p.userClosed)

	for {
		messageType, message, err := p.userConn.ReadMessage()
		if err != nil {
			if errors.Is(err, websocket.ErrReadLimit) && !p.dropDownstreamWrites.Load() {
				_ = p.writeUserMessage(websocket.TextMessage, []byte(types.NewErrorEvent("", "invalid_request_error", "invalid_event", "frame is too large or invalid; send smaller audio chunks").Error()))
			}
			p.emitExit(realtimeProxyExit{source: "user", err: err, graceful: isRealtimeDisconnectError(err)})
			return
		}
		p.markActivity(time.Now())

		if err := p.session.SendClient(p.ctx, messageType, message); err != nil {
			if payload := proxyErrorPayload(err); payload != nil && !p.dropDownstreamWrites.Load() {
				if writeErr := WriteWSLocalError(p.writer, payload); writeErr != nil {
					p.emitExit(realtimeProxyExit{source: "user", err: writeErr})
					return
				}
				if isRecoverableRealtimeProxyError(err) {
					continue
				}
			}
			p.emitExit(realtimeProxyExit{source: "user", err: err})
			return
		}
	}
}

func (p *RealtimeSessionProxy) sessionToUser() {
	defer close(p.supplierClosed)

	for {
		messageType, payload, _, _, err := p.session.Recv(p.ctx)
		if err != nil {
			if !p.dropDownstreamWrites.Load() && len(payload) > 0 {
				_ = p.writeUserMessage(messageType, payload)
			} else if !p.dropDownstreamWrites.Load() {
				var closeErr *websocket.CloseError
				if errors.As(err, &closeErr) {
					_ = p.writeUserMessage(websocket.CloseMessage, SafeWSCloseMessage(closeErr.Code, closeErr.Text))
				}
			}
			if !p.dropDownstreamWrites.Load() {
				if errorPayload := runtimesession.ClientPayloadFromError(err); errorPayload != nil {
					_ = p.writeUserMessage(websocket.TextMessage, errorPayload)
				}
			}
			p.emitExit(realtimeProxyExit{source: "supplier", err: err, graceful: errors.Is(err, runtimesession.ErrSessionClosed) || errors.Is(err, context.Canceled)})
			return
		}

		p.markActivity(time.Now())
		if len(payload) == 0 || p.dropDownstreamWrites.Load() {
			continue
		}
		if err := p.writeUserMessage(messageType, payload); err != nil {
			p.emitExit(realtimeProxyExit{source: "supplier", err: err, graceful: isRealtimeDisconnectError(err)})
			return
		}
	}
}

func (p *RealtimeSessionProxy) idleWatchdog() {
	if p.timeout <= 0 {
		return
	}
	checkInterval := minRealtimeProxyDuration(p.timeout/4, 5*time.Second)
	if checkInterval < time.Second {
		checkInterval = time.Second
	}
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			lastActivity := time.Unix(0, p.lastActivityUnixNano.Load())
			if time.Since(lastActivity) < p.timeout {
				continue
			}
			p.emitExit(realtimeProxyExit{source: "idle", err: context.DeadlineExceeded})
			return
		}
	}
}

func (p *RealtimeSessionProxy) clientPingLoop() {
	interval := config.RealtimeWebsocketPingInterval()
	if p == nil || p.writer == nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			if err := p.writer.WriteControl(websocket.PingMessage, nil); err != nil {
				p.emitExit(realtimeProxyExit{source: "user", err: err, graceful: isRealtimeDisconnectError(err)})
				return
			}
		}
	}
}

func (p *RealtimeSessionProxy) coordinate() {
	defer p.finishCoordinate()

	first := <-p.exitCh
	reason := p.detachReason(first.source)

	if first.source == "user" && first.graceful {
		p.dropDownstreamWrites.Store(true)
	}

	p.closeOnce.Do(func() {
		switch {
		case p.session == nil:
		case first.source == "idle":
			p.session.Abort(reason)
		case first.source == "user" && !first.graceful:
			p.session.Abort(reason)
		case first.source == "user" && first.graceful && !sessionSupportsGracefulDetach(p.session):
			p.session.Abort(reason)
		default:
			p.session.Detach(reason)
		}
		p.closeDownstream(websocket.CloseNormalClosure, reason)
	})
}

func (p *RealtimeSessionProxy) runWorker(fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("realtime proxy worker panic: %v", recovered)
			p.emitExit(realtimeProxyExit{source: "proxy_panic", err: fmt.Errorf("proxy_panic: %v", recovered)})
			p.emergencyShutdown("proxy_panic")
		}
		p.workers.Done()
	}()
	fn()
}

func (p *RealtimeSessionProxy) emitExit(exit realtimeProxyExit) {
	select {
	case p.exitCh <- exit:
	default:
		if p != nil && p.exitDropLogged.CompareAndSwap(false, true) {
			log.Printf("realtime proxy exit signal dropped: source=%s err=%v", exit.source, exit.err)
		}
	}
}

func (p *RealtimeSessionProxy) signalDone() {
	p.doneOnce.Do(func() {
		close(p.done)
	})
}

func (p *RealtimeSessionProxy) finishCoordinate() {
	if recovered := recover(); recovered != nil {
		log.Printf("realtime proxy coordinator panic: %v", recovered)
		p.emergencyShutdown("proxy_panic")
	}
	p.workers.Wait()
	p.signalDone()
}

func (p *RealtimeSessionProxy) emergencyShutdown(reason string) {
	if p == nil {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	if p.writer != nil {
		_ = p.writer.Close()
	} else if p.userConn != nil {
		_ = p.userConn.Close()
	}
	if p.session != nil {
		p.safeSessionAction("abort", func() {
			p.session.Abort(strings.TrimSpace(reason))
		})
	}
}

func (p *RealtimeSessionProxy) closeDownstream(code int, reason string) {
	if p == nil {
		return
	}
	if p.writer != nil {
		_ = p.writer.WriteClose(code, reason)
		if p.cancel != nil {
			p.cancel()
		}
		_ = p.writer.Close()
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	if p.userConn != nil {
		_ = p.userConn.Close()
	}
}

func (p *RealtimeSessionProxy) safeSessionAction(label string, fn func()) {
	if fn == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("realtime proxy session %s panic: %v", strings.TrimSpace(label), recovered)
			if p.cancel != nil {
				p.cancel()
			}
			if p.writer != nil {
				_ = p.writer.Close()
			} else if p.userConn != nil {
				_ = p.userConn.Close()
			}
			if !p.started.Load() {
				p.signalDone()
			}
		}
	}()
	fn()
}

func (p *RealtimeSessionProxy) markActivity(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	p.lastActivityUnixNano.Store(now.UnixNano())
}

func (p *RealtimeSessionProxy) configureUserConn() {
	if p == nil || p.userConn == nil {
		return
	}
	ApplyWSReadLimit(p.userConn, config.RealtimeWebsocketReadLimit)
	p.refreshUserReadDeadline()
	InstallWSActivityHandlers(p.userConn, func() {
		p.markActivity(time.Now())
		p.refreshUserReadDeadline()
	})
}

func (p *RealtimeSessionProxy) detachReason(source string) string {
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

func proxyErrorPayload(err error) []byte {
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
		return []byte(types.NewErrorEvent("", "system_error", code, realtimeProxyStaticErrorMessage(code)).Error())
	}

	log.Printf("realtime proxy internal error: %v", err)
	return []byte(types.NewErrorEvent("", "system_error", "system_error", realtimeProxyStaticErrorMessage("system_error")).Error())
}

func realtimeProxyStaticErrorMessage(code string) string {
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

func isRecoverableRealtimeProxyError(err error) bool {
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

func (p *RealtimeSessionProxy) writeUserMessage(messageType int, payload []byte) error {
	if p == nil || p.writer == nil {
		return net.ErrClosed
	}
	if messageType == websocket.CloseMessage {
		return p.writer.WriteControl(websocket.CloseMessage, payload)
	}
	return p.writer.WriteMessage(messageType, payload)
}

func isRealtimeDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
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

func (p *RealtimeSessionProxy) refreshUserReadDeadline() {
	if p == nil || p.userConn == nil {
		return
	}
	interval := config.RealtimeWebsocketPingInterval()
	if interval <= 0 {
		_ = p.userConn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		return
	}
	_ = p.userConn.SetReadDeadline(time.Now().Add(2 * interval))
}

func sessionSupportsGracefulDetach(session runtimesession.RealtimeSession) bool {
	detachable, ok := session.(runtimesession.GracefulDetachCapable)
	return ok && detachable.SupportsGracefulDetach()
}

func minRealtimeProxyDuration(a, b time.Duration) time.Duration {
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
