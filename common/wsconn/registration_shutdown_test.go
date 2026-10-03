package wsconn

import (
	"testing"
	"time"
)

func TestShutdownLateManagedConnectionsUnregisterBeforeDone(t *testing.T) {
	original := defaultActiveRegistry
	registry := &activeRegistry{closing: true, conns: make(map[*ManagedConn]struct{})}
	defaultActiveRegistry = registry
	t.Cleanup(func() { defaultActiveRegistry = original })
	for i := 0; i < 100; i++ {
		client, server := managedPairForTest(t)
		for _, c := range []*ManagedConn{client, server} {
			select {
			case <-c.Done():
			case <-time.After(time.Second):
				t.Fatal("late constructor connection not closed")
			}
		}
		registry.mu.Lock()
		remaining := len(registry.conns)
		registry.mu.Unlock()
		if remaining != 0 {
			t.Fatalf("round %d: %d closed connections leaked in registry", i, remaining)
		}
	}
}
