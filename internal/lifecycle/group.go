// Package lifecycle tracks process-local work through shutdown. It does not
// schedule or retry work. A group has one lifetime and cannot be reopened.
package lifecycle

import (
	"context"
	"net/http"
	"sync"
)

// Group fences admission before waiting, so registration cannot race a zero
// count. Its memory is constant regardless of the number of active operations.
type Group struct {
	mu     sync.Mutex
	active int
	closed bool
	done   chan struct{}
}

func (g *Group) Start() (finish func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, false
	}
	g.active++
	var once sync.Once
	return func() { once.Do(func() { g.mu.Lock(); defer g.mu.Unlock(); g.active--; g.completeLocked() }) }, true
}
func (g *Group) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	if g.done == nil {
		g.done = make(chan struct{})
	}
	g.completeLocked()
}
func (g *Group) completeLocked() {
	if g.closed && g.active == 0 {
		select {
		case <-g.done:
		default:
			close(g.done)
		}
	}
}
func (g *Group) Wait(ctx context.Context) error {
	g.mu.Lock()
	if g.done == nil {
		g.done = make(chan struct{})
	}
	done := g.done
	g.mu.Unlock()
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Handler tracks handler completion even after HTTP hijacking. The wrapper
// deliberately leaves ResponseWriter untouched so optional interfaces survive.
func (g *Group) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finish, ok := g.Start()
		if !ok {
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		defer finish()
		next.ServeHTTP(w, r)
	})
}
