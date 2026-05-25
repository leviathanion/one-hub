package wsconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/gorilla/websocket"
)

const (
	readIdle int32 = iota
	readInitialActive
	readInitialReady
	readPumpActive
	readTerminal
)

type Pump struct {
	Conn *ManagedConn
	// Handle must return quickly, normally within 1ms. It should only copy the
	// payload and non-blockingly post it to an actor or worker; do not perform
	// external IO, admission checks, JSON reprocessing, or blocking channel sends.
	Handle func(context.Context, MessageType, []byte)
	// OnClose runs after connection cleanup has completed. Use it for metrics,
	// logging, actor posts, state transitions, and resource release only; do not
	// write this connection or perform blocking external IO here.
	OnClose func(CloseInfo)
}

func (c *ManagedConn) beginReadInitial() {
	if !c.readState.CompareAndSwap(readIdle, readInitialActive) {
		panic(fmt.Sprintf("wsconn: ReadInitial invalid read state %d", c.readState.Load()))
	}
}

func (c *ManagedConn) finishReadInitial(err error) {
	if err != nil {
		c.readState.Store(readTerminal)
		return
	}
	c.readState.Store(readInitialReady)
}

func (c *ManagedConn) beginPump() bool {
	if c.closeStarted.Load() {
		return false
	}
	if c.readState.CompareAndSwap(readIdle, readPumpActive) {
		return true
	}
	if c.readState.CompareAndSwap(readInitialReady, readPumpActive) {
		return true
	}
	panic(fmt.Sprintf("wsconn: Pump.Run invalid read state %d", c.readState.Load()))
}

func (c *ManagedConn) finishPump() {
	c.readState.Store(readTerminal)
}

var ErrFirstFrameTooLarge = errors.New("wsconn: first frame too large")

func (c *ManagedConn) ReadInitial(ctx context.Context) (MessageType, []byte, error) {
	if c == nil || c.raw == nil {
		return 0, nil, net.ErrClosed
	}
	c.beginReadInitial()
	var retErr error
	defer func() { c.finishReadInitial(retErr) }()
	c.raw.SetReadLimit(0)
	defer c.raw.SetReadLimit(c.readLimit())
	stopWatcher := c.applyTemporaryReadDeadline(ctx)
	defer stopWatcher()

	mt, reader, err := c.raw.NextReader()
	if err != nil {
		retErr = classifyContextReadError(ctx, convertCloseError(err))
		return 0, nil, retErr
	}
	if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
		retErr = ErrInvalidMessageType
		return 0, nil, retErr
	}
	limit := c.readLimit() + 1
	payload, err := io.ReadAll(io.LimitReader(reader, limit))
	if err != nil {
		retErr = classifyContextReadError(ctx, err)
		return 0, nil, retErr
	}
	if int64(len(payload)) > c.readLimit() {
		retErr = fmt.Errorf("%w: limit %d", ErrFirstFrameTooLarge, c.readLimit())
		return 0, nil, retErr
	}
	return MessageType(mt), payload, nil
}

func (p Pump) Run(ctx context.Context) {
	if p.Conn == nil {
		return
	}
	c := p.Conn
	if !c.beginPump() {
		<-c.Done()
		if p.OnClose != nil {
			p.OnClose(c.CloseInfo())
		}
		return
	}
	defer func() {
		c.finishPump()
		c.Close(CloseInfo{Kind: CloseKindAbort, Reason: "pump_exit_without_close"})
		<-c.Done()
		if p.OnClose != nil {
			p.OnClose(c.CloseInfo())
		}
	}()
	c.startPingLoop()
	c.startInboundWatchdog()

	done := make(chan struct{})
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		select {
		case <-ctx.Done():
			c.Close(CloseInfo{Kind: CloseKindAbort, Reason: "ctx_done", Err: ctx.Err()})
		case <-done:
		}
	}()
	defer close(done)

	for {
		mt, payload, err := c.raw.ReadMessage()
		if err != nil {
			c.Close(classifyReadCloseInfo(err))
			return
		}
		if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
			continue
		}
		c.markInboundActivity(c.clock.Now(), true)
		func() {
			start := c.clock.Now()
			defer func() {
				if r := recover(); r != nil {
					c.Close(CloseInfo{Kind: CloseKindHandlerPanic, Reason: "handler_panic", Err: fmt.Errorf("%v", r)})
				}
				if elapsed := c.clock.Now().Sub(start); elapsed > time.Millisecond {
					observeSlowHandle(elapsed)
				}
			}()
			if p.Handle != nil {
				p.Handle(ctx, MessageType(mt), payload)
			}
		}()
		if c.closeStarted.Load() {
			return
		}
	}
}

func classifyReadCloseInfo(err error) CloseInfo {
	err = convertCloseError(err)
	var closeErr *CloseError
	if errors.As(err, &closeErr) {
		if closeErr.Code == CloseAbnormalClosure {
			return CloseInfo{Kind: CloseKindReadError, Code: closeErr.Code, Reason: closeErr.Reason, Err: err}
		}
		return CloseInfo{Kind: CloseKindPeerClose, Code: closeErr.Code, Reason: closeErr.Reason, Err: err}
	}
	if err == websocket.ErrReadLimit {
		return CloseInfo{Kind: CloseKindReadError, Code: CloseMessageTooBig, Reason: "message_too_big", Err: err}
	}
	return CloseInfo{Kind: CloseKindReadError, Reason: "read_failed", Err: err}
}

func classifyContextReadError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (c *ManagedConn) readLimit() int64 {
	if c.cfg.ReadLimit <= 0 {
		return defaultReadLimit
	}
	return c.cfg.ReadLimit
}

func (c *ManagedConn) applyTemporaryReadDeadline(ctx context.Context) func() {
	if ctx == nil {
		return func() {}
	}
	done := make(chan struct{})
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.raw.SetReadDeadline(deadline)
		return func() {
			close(done)
			_ = c.raw.SetReadDeadline(time.Time{})
		}
	}
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			_ = c.raw.SetReadDeadline(time.Now())
		case <-done:
		}
	}()
	return func() {
		close(done)
		<-watcherDone
		_ = c.raw.SetReadDeadline(time.Time{})
	}
}

func (c *ManagedConn) installHandlers() {
	defaultCloseHandler := c.raw.CloseHandler()
	c.raw.SetCloseHandler(func(code int, text string) error {
		now := c.clock.Now()
		c.markInboundActivity(now, c.readState.Load() == readPumpActive)
		return defaultCloseHandler(code, text)
	})
	c.raw.SetPingHandler(func(appData string) error {
		now := c.clock.Now()
		c.markInboundActivity(now, c.readState.Load() == readPumpActive)
		if err := c.control.EnqueuePong([]byte(appData)); err != nil {
			c.Close(CloseInfo{Kind: CloseKindWriteError, Reason: "enqueue_pong_failed", Err: err})
			return err
		}
		return nil
	})
	c.raw.SetPongHandler(func(appData string) error {
		now := c.clock.Now()
		c.markInboundActivity(now, c.readState.Load() == readPumpActive)
		c.observePongGeneration(appData)
		return nil
	})
}
