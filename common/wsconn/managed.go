package wsconn

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultReadLimit    int64 = 16 << 20
	defaultWriteTimeout       = 5 * time.Second
)

var ErrInvalidConfig = errors.New("wsconn: invalid config")

type Config struct {
	Label           string
	Clock           Clock
	PingInterval    time.Duration
	PongMissTimeout time.Duration
	// InboundActivityTimeout must be fast, non-blocking, and side-effect free.
	// wsconn may call it at validation and runtime; do not query databases,
	// take locks, or perform remote calls from this closure.
	InboundActivityTimeout func() time.Duration
	ReadLimit              int64
	// WriteTimeout must be fast, non-blocking, and side-effect free. wsconn may
	// call it at validation and runtime; do not query databases, take locks, or
	// perform remote calls from this closure.
	WriteTimeout func() time.Duration
	// OnActivity is a transport hook for metrics or refreshing business idle
	// state only. It must not write the connection or perform blocking work.
	OnActivity func(time.Time)
}

type readLimitConn interface {
	SetReadLimit(int64)
}

// ApplyReadLimit applies a configured websocket read limit to conn and returns
// the installed value. It exists for legacy gorilla paths while callers migrate
// to ManagedConn.Config.ReadLimit.
func ApplyReadLimit(conn readLimitConn, readLimit func() int64) int64 {
	if conn == nil {
		return 0
	}
	limit := int64(0)
	if readLimit != nil {
		limit = readLimit()
	}
	if limit <= 0 {
		limit = defaultReadLimit
	}
	conn.SetReadLimit(limit)
	return limit
}

func validateConfig(cfg Config) error {
	if cfg.PingInterval < 0 {
		return fmt.Errorf("%w: PingInterval must be >= 0", ErrInvalidConfig)
	}
	if cfg.PongMissTimeout < 0 {
		return fmt.Errorf("%w: PongMissTimeout must be >= 0", ErrInvalidConfig)
	}
	if cfg.PingInterval <= 0 && cfg.PongMissTimeout > 0 {
		return fmt.Errorf("%w: PongMissTimeout requires PingInterval", ErrInvalidConfig)
	}
	if cfg.ReadLimit < 0 {
		return fmt.Errorf("%w: ReadLimit must be >= 0", ErrInvalidConfig)
	}
	if cfg.WriteTimeout != nil && cfg.WriteTimeout() < 0 {
		return fmt.Errorf("%w: WriteTimeout must be >= 0", ErrInvalidConfig)
	}
	if cfg.InboundActivityTimeout != nil && cfg.InboundActivityTimeout() < 0 {
		return fmt.Errorf("%w: InboundActivityTimeout must be >= 0", ErrInvalidConfig)
	}
	return nil
}

type ManagedConn struct {
	raw   *websocket.Conn
	cfg   Config
	clock Clock

	writeMu sync.Mutex

	closeStarted atomic.Bool
	closeInfoMu  sync.RWMutex
	closeInfo    CloseInfo
	done         chan struct{}

	readState atomic.Int32
	control   *controlWriter
	pong      pongState
	watchdog  *inboundWatchdog
}

func newManagedConn(raw *websocket.Conn, cfg Config) *ManagedConn {
	clock := normalizeClock(cfg.Clock)
	c := &ManagedConn{
		raw:   raw,
		cfg:   cfg,
		clock: clock,
		done:  make(chan struct{}),
	}
	if cfg.ReadLimit == 0 {
		raw.SetReadLimit(defaultReadLimit)
	} else {
		raw.SetReadLimit(cfg.ReadLimit)
	}
	c.control = newControlWriter(c)
	c.installHandlers()
	RegisterActive(c)
	return c
}

func (c *ManagedConn) WriteMessage(mt MessageType, payload []byte) error {
	if c == nil || c.raw == nil {
		return net.ErrClosed
	}
	if !validDataMessageType(mt) {
		return ErrInvalidMessageType
	}
	c.writeMu.Lock()
	if c.closeStarted.Load() {
		c.writeMu.Unlock()
		return net.ErrClosed
	}
	err := c.withWriteDeadlineLocked(func() error {
		return c.raw.WriteMessage(int(mt), payload)
	})
	c.writeMu.Unlock()
	if err != nil {
		c.Close(CloseInfo{Kind: CloseKindWriteError, Reason: "write_message_failed", Err: err})
	}
	return err
}

func (c *ManagedConn) Close(info CloseInfo) {
	if c == nil {
		return
	}
	if !c.closeStarted.CompareAndSwap(false, true) {
		return
	}
	if info.Kind == CloseKindUnknown {
		logWarnf("wsconn: Close called with unknown kind; falling back to abort")
		info.Kind = CloseKindAbort
	}
	info.At = c.clock.Now()
	c.closeInfoMu.Lock()
	c.closeInfo = info
	c.closeInfoMu.Unlock()
	go c.cleanup(info)
}

func (c *ManagedConn) Done() <-chan struct{} {
	if c == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return c.done
}

func (c *ManagedConn) CloseInfo() CloseInfo {
	if c == nil {
		return CloseInfo{}
	}
	c.closeInfoMu.RLock()
	defer c.closeInfoMu.RUnlock()
	return c.closeInfo
}

func (c *ManagedConn) cleanup(info CloseInfo) {
	if shouldSendCloseFrame(info) {
		_ = c.writeCloseFrame(wireCloseCodeFor(info), info.Reason)
	}
	if c.control != nil {
		c.control.Stop()
		c.waitControlWriterDone()
	}
	if c.watchdog != nil {
		c.watchdog.stop()
	}
	c.pong.stop()
	if c.raw != nil {
		_ = c.raw.Close()
	}
	close(c.done)
}

func (c *ManagedConn) waitControlWriterDone() {
	if c == nil || c.control == nil {
		return
	}
	select {
	case <-c.control.done:
		return
	default:
	}
	wait := c.cleanupWaitTimeout()
	timer := c.clock.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-c.control.done:
	case <-timer.Chan():
		logWarnf("wsconn: control writer cleanup wait timed out")
	}
}

func (c *ManagedConn) cleanupWaitTimeout() time.Duration {
	d := c.runtimeWriteTimeout()
	if d > 0 {
		return d
	}
	// Trade-off: WriteTimeout=0 means socket writes have no gorilla deadline,
	// but cleanup still needs a bounded Go-side wait so Done cannot hang forever
	// behind an internal control writer. Reusing the default keeps the public
	// Config small instead of adding a separate cleanup timeout knob.
	return defaultWriteTimeout
}

func (c *ManagedConn) writeCloseFrame(code CloseCode, reason string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.raw == nil {
		return net.ErrClosed
	}
	timeout := c.runtimeWriteTimeout()
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	return c.raw.WriteControl(websocket.CloseMessage, safeCloseMessage(code, reason), deadline)
}

func (c *ManagedConn) withWriteDeadlineLocked(write func() error) error {
	timeout := c.runtimeWriteTimeout()
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	if err := c.raw.SetWriteDeadline(deadline); err != nil {
		return err
	}
	err := write()
	_ = c.raw.SetWriteDeadline(time.Time{})
	return err
}

func (c *ManagedConn) runtimeWriteTimeout() time.Duration {
	if c.cfg.WriteTimeout == nil {
		return defaultWriteTimeout
	}
	d := c.cfg.WriteTimeout()
	if d < 0 {
		logWarnf("wsconn: WriteTimeout returned negative; falling back to default")
		return defaultWriteTimeout
	}
	return d
}
