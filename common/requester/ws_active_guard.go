package requester

import "sync/atomic"

// WSActiveCounterGuard owns a websocket capacity counter release function and
// guarantees it runs at most once. The counter dimension and admission policy
// remain with the caller; this helper only protects the lifecycle edge.
type WSActiveCounterGuard struct {
	release  func()
	released atomic.Bool
}

func NewWSActiveCounterGuard(release func()) *WSActiveCounterGuard {
	return &WSActiveCounterGuard{release: release}
}

// Release runs the underlying release function once and reports whether this
// call performed the release.
func (g *WSActiveCounterGuard) Release() bool {
	if g == nil {
		return false
	}
	if !g.released.CompareAndSwap(false, true) {
		return false
	}
	if g.release != nil {
		g.release()
	}
	return true
}

func (g *WSActiveCounterGuard) Released() bool {
	return g == nil || g.released.Load()
}
