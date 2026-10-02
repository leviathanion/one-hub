package codex

import (
	"context"
	"sync"
)

// Readers can outlive a detached HTTP attachment. Keep their provider evidence,
// observer finalization and panic cleanup alive until the process drain joins.
var realtimeWorkers sync.WaitGroup

// Call only after HTTP/WS handlers and admin background work have drained.
// Then the janitor and already-counted readers are the only possible parents of
// cleanup workers. Join the janitor first; a reader adds a child before its own
// Done, so Wait cannot race a new zero-to-one registration.
func StopExecutionSessionRuntime(ctx context.Context) error {
	if err := currentCodexExecutionSessions().Shutdown(ctx); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { realtimeWorkers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
