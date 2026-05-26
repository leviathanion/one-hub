package wsconn

import (
	"context"
	"sync"
)

type activeRegistry struct {
	mu    sync.Mutex
	conns map[*ManagedConn]struct{}
}

type connRegistration struct {
	conn      *ManagedConn
	watchDone bool
}

var defaultActiveRegistry = &activeRegistry{conns: make(map[*ManagedConn]struct{})}

// RegisterActive tracks conn for process-level graceful shutdown. The returned
// function is idempotent. ManagedConn registers itself internally without a
// per-connection goroutine; this compatibility helper still watches Done for
// external/manual registrations.
func RegisterActive(conn *ManagedConn) func() {
	return defaultActiveRegistry.register(connRegistration{conn: conn, watchDone: true})
}

func (r *activeRegistry) register(reg connRegistration) func() {
	conn := reg.conn
	if r == nil || conn == nil {
		return func() {}
	}
	r.mu.Lock()
	if r.conns == nil {
		r.conns = make(map[*ManagedConn]struct{})
	}
	r.conns[conn] = struct{}{}
	r.mu.Unlock()

	var once sync.Once
	unregister := func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.conns, conn)
			r.mu.Unlock()
		})
	}
	if reg.watchDone {
		go func() {
			<-conn.Done()
			unregister()
		}()
	}
	return unregister
}

// ShutdownActive closes all currently tracked connections and waits until each
// one is done or ctx expires. The close reason intentionally stays generic so
// wsconn does not learn about HTTP servers, relays, providers, or process code.
func ShutdownActive(ctx context.Context) error {
	return defaultActiveRegistry.shutdown(ctx, CloseInfo{
		Kind:   CloseKindGracefulShutdown,
		Code:   CloseGoingAway,
		Reason: "server_shutdown",
	})
}

func (r *activeRegistry) shutdown(ctx context.Context, info CloseInfo) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	conns := make([]*ManagedConn, 0, len(r.conns))
	for conn := range r.conns {
		conns = append(conns, conn)
	}
	r.mu.Unlock()

	for _, conn := range conns {
		conn.Close(info)
	}
	for _, conn := range conns {
		select {
		case <-conn.Done():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
