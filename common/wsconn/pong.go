package wsconn

import (
	"encoding/binary"
	"sync"
	"time"
)

type pongState struct {
	mu                sync.Mutex
	gen               uint64
	awaiting          bool
	outstandingGen    uint64
	outstandingTimer  Timer
	lastMatchedPongAt time.Time
	pingTimer         Timer
}

func (c *ManagedConn) startPingLoop() {
	if c.cfg.PingInterval <= 0 {
		return
	}
	c.pong.mu.Lock()
	if c.pong.pingTimer == nil {
		c.pong.pingTimer = c.clock.AfterFunc(c.cfg.PingInterval, c.onPingTick)
	}
	c.pong.mu.Unlock()
}

func (c *ManagedConn) onPingTick() {
	c.pong.mu.Lock()
	if c.closeStarted.Load() {
		c.pong.mu.Unlock()
		return
	}
	if c.cfg.PongMissTimeout > 0 && c.pong.awaiting {
		c.pong.pingTimer.Reset(c.cfg.PingInterval)
		c.pong.mu.Unlock()
		return
	}
	c.pong.gen++
	gen := c.pong.gen
	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, gen)
	var timer Timer
	if c.cfg.PongMissTimeout > 0 {
		c.pong.awaiting = true
		c.pong.outstandingGen = gen
		timer = c.clock.AfterFunc(c.cfg.PongMissTimeout, func() {
			c.onPongMiss(gen)
		})
		c.pong.outstandingTimer = timer
	}
	c.pong.mu.Unlock()

	if err := c.control.EnqueuePing(payload); err != nil {
		c.pong.mu.Lock()
		if timer != nil {
			timer.Stop()
		}
		if c.pong.outstandingGen == gen {
			c.pong.awaiting = false
			c.pong.outstandingGen = 0
			c.pong.outstandingTimer = nil
		}
		c.pong.mu.Unlock()
		c.Close(CloseInfo{Kind: CloseKindWriteError, Reason: "enqueue_ping_failed", Err: err})
		return
	}

	c.pong.mu.Lock()
	if !c.closeStarted.Load() && c.pong.pingTimer != nil {
		c.pong.pingTimer.Reset(c.cfg.PingInterval)
	}
	c.pong.mu.Unlock()
}

func (c *ManagedConn) onPongMiss(gen uint64) {
	c.pong.mu.Lock()
	missed := c.pong.awaiting && c.pong.outstandingGen == gen
	if missed {
		c.pong.awaiting = false
		c.pong.outstandingGen = 0
		c.pong.outstandingTimer = nil
	}
	c.pong.mu.Unlock()
	if missed {
		c.Close(CloseInfo{Kind: CloseKindPongMiss, Reason: "pong_miss"})
	}
}

func (c *ManagedConn) observePongGeneration(payload string) {
	if len(payload) != 8 {
		return
	}
	gen := binary.BigEndian.Uint64([]byte(payload))
	now := c.clock.Now()
	c.pong.mu.Lock()
	defer c.pong.mu.Unlock()
	if !c.pong.awaiting || c.pong.outstandingGen != gen {
		return
	}
	if c.pong.outstandingTimer != nil {
		c.pong.outstandingTimer.Stop()
	}
	c.pong.awaiting = false
	c.pong.outstandingGen = 0
	c.pong.outstandingTimer = nil
	c.pong.lastMatchedPongAt = now
}

func (p *pongState) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pingTimer != nil {
		p.pingTimer.Stop()
	}
	if p.outstandingTimer != nil {
		p.outstandingTimer.Stop()
	}
}
