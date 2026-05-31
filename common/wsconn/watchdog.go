package wsconn

import (
	"context"
	"sync"
	"time"
)

type inboundWatchdog struct {
	conn *ManagedConn
	mu   sync.Mutex
	t    Timer
}

func (c *ManagedConn) startInboundWatchdog() {
	if c.cfg.InboundActivityTimeout == nil {
		return
	}
	d := c.runtimeInboundActivityTimeout()
	if d <= 0 {
		return
	}
	watchdog := &inboundWatchdog{conn: c}
	watchdog.t = c.clock.AfterFunc(d, func() {
		c.Close(CloseInfo{Kind: CloseKindInboundIdle, Reason: "inbound_idle"})
	})
	if !c.watchdog.CompareAndSwap(nil, watchdog) {
		watchdog.stop()
		return
	}
	if c.closeStarted.Load() {
		watchdog.stop()
	}
}

func (c *ManagedConn) markInboundActivity(now time.Time, invokeCallback bool) {
	if watchdog := c.watchdog.Load(); watchdog != nil {
		watchdog.reset()
	}
	if invokeCallback && c.cfg.OnActivity != nil {
		c.cfg.OnActivity(now)
	}
}

func (w *inboundWatchdog) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.t == nil {
		return
	}
	d := w.conn.runtimeInboundActivityTimeout()
	if d <= 0 {
		w.t.Stop()
		w.t = nil
		return
	}
	w.t.Reset(d)
}

func (w *inboundWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.t != nil {
		w.t.Stop()
		w.t = nil
	}
}

func (c *ManagedConn) runtimeInboundActivityTimeout() time.Duration {
	if c.cfg.InboundActivityTimeout == nil {
		return 0
	}
	d := c.cfg.InboundActivityTimeout()
	if d < 0 {
		// Fail open: a runtime config bug may leave dead connections alive
		// longer, but it avoids immediately closing every active connection.
		logWarnf(context.Background(), "wsconn[%s]: InboundActivityTimeout returned negative; disabling watchdog", c.label())
		return 0
	}
	return d
}
