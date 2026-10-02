package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"one-api/common/config"
	"one-api/common/wsconn"
	"one-api/internal/lifecycle"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
)

func TestShutdownWaitsForHijackedHandlerSettlementBeforeBatchFlush(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.Log{}); err != nil {
		t.Fatal(err)
	}
	oldDB, oldInterval := model.DB, config.BatchUpdateInterval
	model.DB, config.BatchUpdateInterval = db, 3600
	t.Cleanup(func() {
		model.DB, config.BatchUpdateInterval = oldDB, oldInterval
		pool, _ := db.DB()
		_ = pool.Close()
	})
	model.InitBatchUpdater()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = model.StopBatchUpdater(ctx)
	}()
	requests := &lifecycle.Group{}
	ready, settling, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	enqueue := make(chan error, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	server := httptest.NewServer(requests.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, acceptErr := wsconn.AcceptManaged(w, r, wsconn.Config{}, wsconn.AcceptOptions{})
		if acceptErr != nil {
			enqueue <- acceptErr
			close(ready)
			return
		}
		close(ready)
		wsconn.Pump{Conn: conn, OnClose: func(wsconn.CloseInfo) {
			close(settling)
			<-release
			enqueue <- model.AddLogToBatch(&model.Log{Content: "settlement after connection close"})
		}}.Run(context.Background())
	})))
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	<-ready
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	requests.Close()
	stopped := make(chan error, 1)
	go func() {
		stopped <- gracefulShutdownSteps(ctx, shutdownOperations{http: server.Config.Shutdown, connections: wsconn.ShutdownActive, requests: requests.Wait, batches: model.StopBatchUpdater})
	}()
	select {
	case <-settling:
	case <-ctx.Done():
		t.Fatal("settlement did not start")
	}
	select {
	case err := <-stopped:
		t.Fatalf("retired batch before handler finished: %v", err)
	default:
	}
	unblock()
	select {
	case err := <-enqueue:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("late business write stuck")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("drain stuck")
	}
	var count int64
	if err := db.Model(&model.Log{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("final log count=%d err=%v", count, err)
	}
}
