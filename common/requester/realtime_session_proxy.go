package requester

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"sync"
	"sync/atomic"
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
	session  runtimesession.RealtimeSession
	timeout  time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	done           chan struct{}
	userClosed     chan struct{}
	supplierClosed chan struct{}
	exitCh         chan realtimeProxyExit
	doneOnce       sync.Once
	closeOnce      sync.Once
	workers        sync.WaitGroup
	writeMu        sync.Mutex

	dropDownstreamWrites atomic.Bool
	lastActivityUnixNano atomic.Int64
}

func NewRealtimeSessionProxy(userConn *websocket.Conn, session runtimesession.RealtimeSession, timeout time.Duration) *RealtimeSessionProxy {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	proxy := &RealtimeSessionProxy{
		userConn:       userConn,
		session:        session,
		timeout:        timeout,
		ctx:            ctx,
		cancel:         cancel,
		done:           make(chan struct{}),
		userClosed:     make(chan struct{}),
		supplierClosed: make(chan struct{}),
		exitCh:         make(chan realtimeProxyExit, 4),
	}
	proxy.markActivity(time.Now())
	return proxy
}

func (p *RealtimeSessionProxy) Start() {
	p.workers.Add(3)
	go p.runWorker(p.userToSession)
	go p.runWorker(p.sessionToUser)
	go p.runWorker(p.idleWatchdog)
	go p.coordinate()
}

func (p *RealtimeSessionProxy) Wait() {
	<-p.done
}

func (p *RealtimeSessionProxy) Close() {
	p.closeOnce.Do(func() {
		p.cancel()
		if p.userConn != nil {
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
			p.emitExit(realtimeProxyExit{source: "user", err: err, graceful: isRealtimeDisconnectError(err)})
			return
		}
		p.markActivity(time.Now())

		if err := p.session.SendClient(p.ctx, messageType, message); err != nil {
			if payload := proxyErrorPayload(err); payload != nil && !p.dropDownstreamWrites.Load() {
				if writeErr := p.writeUserMessage(websocket.TextMessage, payload); writeErr != nil {
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
		messageType, payload, _, err := p.session.Recv(p.ctx)
		if err != nil {
			if !p.dropDownstreamWrites.Load() && len(payload) > 0 {
				_ = p.writeUserMessage(messageType, payload)
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
		p.cancel()
		if p.userConn != nil {
			_ = p.userConn.Close()
		}
	})
}

func (p *RealtimeSessionProxy) runWorker(fn func()) {
	defer p.workers.Done()
	fn()
}

func (p *RealtimeSessionProxy) emitExit(exit realtimeProxyExit) {
	select {
	case p.exitCh <- exit:
	default:
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
	if p.userConn != nil {
		_ = p.userConn.Close()
	}
	if p.session != nil {
		p.safeSessionAction("abort", func() {
			p.session.Abort(strings.TrimSpace(reason))
		})
	}
}

func (p *RealtimeSessionProxy) safeSessionAction(label string, fn func()) {
	if fn == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("realtime proxy session %s panic: %v", strings.TrimSpace(label), recovered)
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

func (p *RealtimeSessionProxy) detachReason(source string) string {
	switch strings.TrimSpace(source) {
	case "supplier":
		return "supplier_closed"
	case "idle":
		return "idle_timeout"
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
		return []byte(event.Error())
	}

	return []byte(types.NewErrorEvent("", "system_error", "system_error", err.Error()).Error())
}

func isRecoverableRealtimeProxyError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, runtimesession.ErrSessionClosed) {
		return false
	}

	var event *types.Event
	return errors.As(err, &event) && event.IsError()
}

func (p *RealtimeSessionProxy) writeUserMessage(messageType int, payload []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.userConn.WriteMessage(messageType, payload)
}

func isRealtimeDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
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
	return strings.Contains(message, "broken pipe") || strings.Contains(message, "connection reset by peer")
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
