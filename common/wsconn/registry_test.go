package wsconn

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestActiveRegistryShutdownClosesAndWaits(t *testing.T) {
	client, server := managedPairForTest(t)
	registry := &activeRegistry{}
	unregister := registry.register(server)
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
	unregister := registry.register(server)
	defer unregister()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := registry.shutdown(ctx, CloseInfo{Kind: CloseKindGracefulShutdown, Code: CloseGoingAway, Reason: "server_shutdown"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown err=%v, want context.Canceled", err)
	}
}
