package wsconn

import (
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
	c.watchdog = &inboundWatchdog{conn: c}
	c.watchdog.t = c.clock.AfterFunc(d, func() {
		c.Close(CloseInfo{Kind: CloseKindInboundIdle, Reason: "inbound_idle"})
	})
}

func (c *ManagedConn) markInboundActivity(now time.Time, invokeCallback bool) {
	if c.watchdog != nil {
		c.watchdog.reset()
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
		logWarnf("wsconn: InboundActivityTimeout returned negative; disabling watchdog")
		return 0
	}
	return d
}
