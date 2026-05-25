package wsconn

import "time"

type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	AfterFunc(d time.Duration, f func()) Timer
}

type Timer interface {
	Chan() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer {
	return realTimer{timer: time.NewTimer(d)}
}

func (realClock) AfterFunc(d time.Duration, f func()) Timer {
	return realTimer{timer: time.AfterFunc(d, f)}
}

type realTimer struct {
	timer *time.Timer
}

func (t realTimer) Chan() <-chan time.Time { return t.timer.C }
func (t realTimer) Stop() bool             { return t.timer.Stop() }
func (t realTimer) Reset(d time.Duration) bool {
	return t.timer.Reset(d)
}

func normalizeClock(clock Clock) Clock {
	if clock == nil {
		return realClock{}
	}
	return clock
}
