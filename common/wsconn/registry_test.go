package wsconn

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestActiveRegistryShutdownClosesAndWaits(t *testing.T) {
	client, server := managedPairForTest(t)
	registry := &activeRegistry{}
	unregister := registry.register(connRegistration{conn: server, watchDone: false})
	t.Cleanup(unregister)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := registry.shutdown(ctx, CloseInfo{
		Kind:   CloseKindGracefulShutdown,
		Code:   CloseGoingAway,
		Reason: "server_shutdown",
	}); err != nil {
		t.Fatalf("shutdown active conn: %v", err)
	}
	if info := server.CloseInfo(); info.Kind != CloseKindGracefulShutdown || info.Code != CloseGoingAway || info.Reason != "server_shutdown" {
		t.Fatalf("server CloseInfo=%+v, want graceful going-away server_shutdown", info)
	}
	select {
	case <-server.Done():
	default:
		t.Fatal("expected shutdown to wait for server conn done")
	}
	client.Close(CloseInfo{Kind: CloseKindAbort, Reason: "test_cleanup"})
}

func TestActiveRegistryShutdownHonorsContext(t *testing.T) {
	server := &ManagedConn{done: make(chan struct{}), clock: realClock{}}
	registry := &activeRegistry{}
	unregister := registry.register(connRegistration{conn: server, watchDone: false})
	defer unregister()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := registry.shutdown(ctx, CloseInfo{Kind: CloseKindGracefulShutdown, Code: CloseGoingAway, Reason: "server_shutdown"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown err=%v, want context.Canceled", err)
	}
}

func TestRegisterActiveShutdownActivePublicAPI(t *testing.T) {
	conn := &ManagedConn{done: make(chan struct{}), clock: realClock{}}
	unregister := RegisterActive(conn)
	t.Cleanup(func() {
		unregister()
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := ShutdownActive(ctx); err != nil {
		t.Fatalf("ShutdownActive err=%v", err)
	}
	if info := conn.CloseInfo(); info.Kind != CloseKindGracefulShutdown || info.Code != CloseGoingAway || info.Reason != "server_shutdown" {
		t.Fatalf("conn CloseInfo=%+v, want graceful going-away server_shutdown", info)
	}

	unregister()
	unregister()
}

func TestRegisterActiveNilReturnsIdempotentNoop(t *testing.T) {
	unregister := RegisterActive(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unregister()
		}()
	}
	wg.Wait()
}

func TestCleanupSafelyClosesDoneWhenUnregisterPanics(t *testing.T) {
	conn := &ManagedConn{
		done:  make(chan struct{}),
		clock: realClock{},
		unregisterActive: func() {
			panic("unregister failed")
		},
	}
	conn.cleanupSafely(CloseInfo{Kind: CloseKindAbort, Reason: "test_cleanup"})
	select {
	case <-conn.Done():
	default:
		t.Fatal("expected cleanupSafely to close Done after unregister panic")
	}
}

func TestCleanupWaitTimeoutFallsBackWhenWriteTimeoutDisabled(t *testing.T) {
	conn := &ManagedConn{
		cfg: Config{WriteTimeout: func() time.Duration { return 0 }},
	}
	if got := conn.cleanupWaitTimeout(); got != defaultWriteTimeout {
		t.Fatalf("cleanupWaitTimeout=%s, want default %s", got, defaultWriteTimeout)
	}
}
