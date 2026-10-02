package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestShutdownJoinsJanitorCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	m := NewManager(time.Minute, time.Millisecond, func(*ExecutionSession) { close(entered); <-release })
	defer m.Close()
	sess, _, err := m.GetOrCreate(Metadata{Key: "shutdown-cleanup", SessionID: "shutdown", IdleTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	sess.Lock()
	sess.LastUsedAt = time.Now().Add(-time.Hour)
	sess.Unlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("janitor did not enter cleanup")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = m.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unfinished cleanup falsely completed: %v", err)
	}
	unblock()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestShutdownWithDisabledJanitor(t *testing.T) {
	m := NewManager(time.Minute, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
