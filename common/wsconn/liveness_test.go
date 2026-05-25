package wsconn

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestPongMissTimeoutTriggersWithFakeClock(t *testing.T) {
	clock := newManualClock(time.Unix(100, 0))
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock:           clock,
		PingInterval:    10 * time.Millisecond,
		PongMissTimeout: 5 * time.Millisecond,
	}, Config{})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn:    client,
		OnClose: func(info CloseInfo) { closed <- info },
	}.Run(context.Background())
	clock.waitTimers(t, 1)

	clock.Advance(10 * time.Millisecond)
	clock.waitTimers(t, 2)
	clock.Advance(5 * time.Millisecond)

	select {
	case info := <-closed:
		if info.Kind != CloseKindPongMiss {
			t.Fatalf("CloseInfo=%+v, want pong_miss", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for pong miss close")
	}
}

func TestPingLoopWithoutPongMissKeepsSendingAndDoesNotClose(t *testing.T) {
	clock := newManualClock(time.Unix(200, 0))
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock:        clock,
		PingInterval: 10 * time.Millisecond,
	}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	go Pump{Conn: client}.Run(context.Background())
	clock.waitTimers(t, 1)

	for i := 0; i < 3; i++ {
		clock.Advance(10 * time.Millisecond)
	}
	client.pong.mu.Lock()
	gen := client.pong.gen
	awaiting := client.pong.awaiting
	client.pong.mu.Unlock()
	if gen != 3 || awaiting {
		t.Fatalf("gen=%d awaiting=%v, want three pings without awaiting", gen, awaiting)
	}
	select {
	case <-client.Done():
		t.Fatalf("client closed without PongMissTimeout")
	default:
	}
}

func TestPongGenerationAwaitingSkipsAndMatchingPongAllowsNextPing(t *testing.T) {
	clock := newManualClock(time.Unix(300, 0))
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock:           clock,
		PingInterval:    10 * time.Millisecond,
		PongMissTimeout: 30 * time.Millisecond,
	}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	go Pump{Conn: client}.Run(context.Background())
	clock.waitTimers(t, 1)

	clock.Advance(10 * time.Millisecond)
	client.pong.mu.Lock()
	if client.pong.gen != 1 || !client.pong.awaiting || client.pong.outstandingGen != 1 || client.pong.outstandingTimer == nil {
		t.Fatalf("unexpected outstanding state after first ping: gen=%d awaiting=%v outstanding=%d timer=%v", client.pong.gen, client.pong.awaiting, client.pong.outstandingGen, client.pong.outstandingTimer)
	}
	client.pong.mu.Unlock()

	clock.Advance(10 * time.Millisecond)
	client.pong.mu.Lock()
	if client.pong.gen != 1 {
		t.Fatalf("gen=%d, want awaiting tick to skip new ping", client.pong.gen)
	}
	if !client.pong.awaiting || client.pong.outstandingGen != 1 || client.pong.outstandingTimer == nil {
		t.Fatalf("outstanding state changed during awaiting tick: awaiting=%v outstanding=%d timer=%v", client.pong.awaiting, client.pong.outstandingGen, client.pong.outstandingTimer)
	}
	client.pong.mu.Unlock()
	if got := clock.activeTimerCount(); got != 2 {
		t.Fatalf("expected exactly ping timer + one outstanding pong timer while awaiting, got %d active timers", got)
	}

	var payload [8]byte
	binary.BigEndian.PutUint64(payload[:], 1)
	client.observePongGeneration(string(payload[:]))
	client.pong.mu.Lock()
	if client.pong.awaiting || client.pong.outstandingGen != 0 || client.pong.outstandingTimer != nil || !client.pong.lastMatchedPongAt.Equal(clock.Now()) {
		t.Fatalf("unexpected state after matching pong: awaiting=%v outstanding=%d timer=%v matched=%s", client.pong.awaiting, client.pong.outstandingGen, client.pong.outstandingTimer, client.pong.lastMatchedPongAt)
	}
	client.pong.mu.Unlock()

	clock.Advance(10 * time.Millisecond)
	client.pong.mu.Lock()
	gen := client.pong.gen
	client.pong.mu.Unlock()
	if gen != 2 {
		t.Fatalf("gen=%d, want next ping after matching pong", gen)
	}
}

func TestPongGenerationMismatchedPongStillTimesOut(t *testing.T) {
	clock := newManualClock(time.Unix(350, 0))
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock:           clock,
		PingInterval:    10 * time.Millisecond,
		PongMissTimeout: 30 * time.Millisecond,
	}, Config{})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn:    client,
		OnClose: func(info CloseInfo) { closed <- info },
	}.Run(context.Background())
	clock.waitTimers(t, 1)

	clock.Advance(10 * time.Millisecond)
	var payload [8]byte
	binary.BigEndian.PutUint64(payload[:], 99)
	client.observePongGeneration(string(payload[:]))
	client.pong.mu.Lock()
	if !client.pong.awaiting || client.pong.outstandingGen != 1 || client.pong.outstandingTimer == nil {
		t.Fatalf("mismatched pong cleared outstanding state: awaiting=%v outstanding=%d timer=%v", client.pong.awaiting, client.pong.outstandingGen, client.pong.outstandingTimer)
	}
	client.pong.mu.Unlock()

	clock.Advance(30 * time.Millisecond)
	select {
	case info := <-closed:
		if info.Kind != CloseKindPongMiss {
			t.Fatalf("CloseInfo=%+v, want pong_miss", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for mismatched pong miss close")
	}
}

func TestEnqueuePingFailureStopsOutstandingPongMiss(t *testing.T) {
	clock := newManualClock(time.Unix(375, 0))
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock:           clock,
		PingInterval:    10 * time.Millisecond,
		PongMissTimeout: 30 * time.Millisecond,
	}, Config{})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn:    client,
		OnClose: func(info CloseInfo) { closed <- info },
	}.Run(context.Background())
	clock.waitTimers(t, 1)
	client.control.Stop()

	clock.Advance(10 * time.Millisecond)
	select {
	case info := <-closed:
		if info.Kind != CloseKindWriteError {
			t.Fatalf("CloseInfo=%+v, want write_error", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for enqueue ping write error")
	}

	client.pong.mu.Lock()
	awaiting := client.pong.awaiting
	outstandingGen := client.pong.outstandingGen
	outstandingTimer := client.pong.outstandingTimer
	client.pong.mu.Unlock()
	if awaiting || outstandingGen != 0 || outstandingTimer != nil {
		t.Fatalf("enqueue ping failure left outstanding pong state: awaiting=%v outstanding=%d timer=%v", awaiting, outstandingGen, outstandingTimer)
	}

	clock.Advance(30 * time.Millisecond)
	if info := client.CloseInfo(); info.Kind != CloseKindWriteError {
		t.Fatalf("CloseInfo after pong miss deadline=%+v, want write_error preserved", info)
	}
}

func TestPongGenerationCallbacksTolerateConcurrentInterleaving(t *testing.T) {
	client, server := managedPairForTestWithConfigs(t, Config{
		PingInterval:    time.Hour,
		PongMissTimeout: time.Hour,
	}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	client.startPingLoop()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			client.onPingTick()
		}()
		go func(gen uint64) {
			defer wg.Done()
			var payload [8]byte
			binary.BigEndian.PutUint64(payload[:], gen)
			client.observePongGeneration(string(payload[:]))
		}(uint64(i + 1))
		go func(gen uint64) {
			defer wg.Done()
			client.onPongMiss(gen)
		}(uint64(i + 1 + 1000))
	}
	wg.Wait()
}

func TestInboundActivityTimeoutTriggersWithFakeClock(t *testing.T) {
	clock := newManualClock(time.Unix(400, 0))
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock:                  clock,
		InboundActivityTimeout: func() time.Duration { return 10 * time.Millisecond },
	}, Config{})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn:    client,
		OnClose: func(info CloseInfo) { closed <- info },
	}.Run(context.Background())
	clock.waitTimers(t, 1)

	clock.Advance(10 * time.Millisecond)
	select {
	case info := <-closed:
		if info.Kind != CloseKindInboundIdle {
			t.Fatalf("CloseInfo=%+v, want inbound_idle", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for inbound idle close")
	}
}

func TestInboundControlFrameResetsInboundActivityTimeout(t *testing.T) {
	clock := newManualClock(time.Unix(450, 0))
	activity := make(chan time.Time, 2)
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock:                  clock,
		InboundActivityTimeout: func() time.Duration { return 10 * time.Millisecond },
		OnActivity: func(at time.Time) {
			activity <- at
		},
	}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	go Pump{Conn: client}.Run(context.Background())
	clock.waitTimers(t, 1)

	clock.Advance(5 * time.Millisecond)
	if err := server.raw.WriteControl(websocket.PingMessage, []byte("keepalive"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("server write ping: %v", err)
	}
	select {
	case <-activity:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for inbound control activity")
	}

	clock.Advance(9 * time.Millisecond)
	select {
	case <-client.Done():
		t.Fatalf("client closed before reset inbound watchdog deadline: %+v", client.CloseInfo())
	default:
	}
	clock.Advance(1 * time.Millisecond)
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for reset inbound watchdog deadline")
	}
	if info := client.CloseInfo(); info.Kind != CloseKindInboundIdle {
		t.Fatalf("CloseInfo=%+v, want inbound_idle", info)
	}
}

func TestInboundActivityTimeoutResetsOnDataMessage(t *testing.T) {
	clock := newManualClock(time.Unix(500, 0))
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock:                  clock,
		InboundActivityTimeout: func() time.Duration { return 10 * time.Millisecond },
	}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	handled := make(chan struct{}, 1)
	go Pump{
		Conn: client,
		Handle: func(context.Context, MessageType, []byte) {
			handled <- struct{}{}
		},
	}.Run(context.Background())
	clock.waitTimers(t, 1)

	clock.Advance(5 * time.Millisecond)
	if err := server.WriteMessage(TextMessage, []byte("activity")); err != nil {
		t.Fatalf("server write activity: %v", err)
	}
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for data activity")
	}
	clock.Advance(4 * time.Millisecond)
	select {
	case <-client.Done():
		t.Fatalf("client closed before reset watchdog deadline")
	default:
	}
	clock.Advance(5 * time.Millisecond)
	select {
	case <-client.Done():
		t.Fatalf("client closed before full reset watchdog deadline")
	default:
	}
	clock.Advance(1 * time.Millisecond)
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for reset inbound idle deadline")
	}
	if info := client.CloseInfo(); info.Kind != CloseKindInboundIdle {
		t.Fatalf("CloseInfo=%+v, want inbound_idle", info)
	}
}

func TestInboundActivityTimeoutNilOrZeroDoesNotStartWatchdog(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{name: "nil", cfg: Config{}},
		{name: "zero", cfg: Config{InboundActivityTimeout: func() time.Duration { return 0 }}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newManualClock(time.Unix(600, 0))
			tc.cfg.Clock = clock
			client, server := managedPairForTestWithConfigs(t, tc.cfg, Config{})
			defer client.Close(CloseInfo{Kind: CloseKindAbort})
			defer server.Close(CloseInfo{Kind: CloseKindAbort})

			go Pump{Conn: client}.Run(context.Background())
			time.Sleep(10 * time.Millisecond)
			if got := clock.activeTimerCount(); got != 0 {
				t.Fatalf("expected no inbound watchdog timers, got %d", got)
			}
			clock.Advance(time.Hour)
			select {
			case <-client.Done():
				t.Fatalf("client closed despite disabled inbound watchdog: %+v", client.CloseInfo())
			default:
			}
		})
	}
}

func TestOutboundPingDoesNotRefreshInboundActivityTimeout(t *testing.T) {
	clock := newManualClock(time.Unix(700, 0))
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock:                  clock,
		PingInterval:           5 * time.Millisecond,
		InboundActivityTimeout: func() time.Duration { return 10 * time.Millisecond },
	}, Config{})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn:    client,
		OnClose: func(info CloseInfo) { closed <- info },
	}.Run(context.Background())
	clock.waitTimers(t, 2)

	clock.Advance(5 * time.Millisecond)
	client.pong.mu.Lock()
	gen := client.pong.gen
	client.pong.mu.Unlock()
	if gen != 1 {
		t.Fatalf("expected outbound ping generation 1, got %d", gen)
	}

	clock.Advance(5 * time.Millisecond)
	select {
	case info := <-closed:
		if info.Kind != CloseKindInboundIdle {
			t.Fatalf("CloseInfo=%+v, want inbound_idle", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for inbound idle after outbound ping")
	}
}

type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
}

func newManualClock(now time.Time) *manualClock {
	return &manualClock{now: now}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) NewTimer(d time.Duration) Timer {
	t := &manualTimer{
		clock:    c,
		deadline: c.Now().Add(d),
		ch:       make(chan time.Time, 1),
	}
	c.mu.Lock()
	c.timers = append(c.timers, t)
	c.mu.Unlock()
	return t
}

func (c *manualClock) AfterFunc(d time.Duration, f func()) Timer {
	t := &manualTimer{
		clock:    c,
		deadline: c.Now().Add(d),
		f:        f,
		ch:       make(chan time.Time, 1),
	}
	c.mu.Lock()
	c.timers = append(c.timers, t)
	c.mu.Unlock()
	return t
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	for {
		var due []*manualTimer
		c.mu.Lock()
		now := c.now
		for _, t := range c.timers {
			t.mu.Lock()
			if !t.stopped && !t.deadline.After(now) {
				t.stopped = true
				due = append(due, t)
			}
			t.mu.Unlock()
		}
		c.mu.Unlock()
		if len(due) == 0 {
			return
		}
		for _, t := range due {
			t.fire(now)
		}
	}
}

func (c *manualClock) activeTimerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, t := range c.timers {
		t.mu.Lock()
		if !t.stopped {
			count++
		}
		t.mu.Unlock()
	}
	return count
}

func (c *manualClock) waitTimers(t testing.TB, want int) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		if c.activeTimerCount() >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d active timers, got %d", want, c.activeTimerCount())
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

type manualTimer struct {
	clock    *manualClock
	mu       sync.Mutex
	deadline time.Time
	stopped  bool
	f        func()
	ch       chan time.Time
}

func (t *manualTimer) Chan() <-chan time.Time {
	return t.ch
}

func (t *manualTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := !t.stopped
	t.stopped = true
	return wasActive
}

func (t *manualTimer) Reset(d time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := !t.stopped
	t.deadline = t.clock.Now().Add(d)
	t.stopped = false
	return wasActive
}

func (t *manualTimer) fire(now time.Time) {
	if t.f != nil {
		t.f()
		return
	}
	select {
	case t.ch <- now:
	default:
	}
}
