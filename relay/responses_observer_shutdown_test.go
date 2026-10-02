package relay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"one-api/internal/lifecycle"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	"one-api/types"
)

func TestObserverShutdownJoinsDatabaseWorkAfterDeliveryReturns(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	oldDB := model.DB
	model.DB = db
	// Production groups have a single process lifetime; isolate this test's fence.
	oldObservers := backgroundObservers
	backgroundObservers = &lifecycle.Group{}
	t.Cleanup(func() { model.DB = oldDB; backgroundObservers = oldObservers; pool, _ := db.DB(); _ = pool.Close() })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	if err = db.Callback().Query().Before("gorm:query").Register("test:observer_barrier", func(tx *gorm.DB) { close(entered); <-release; tx.AddError(gorm.ErrRecordNotFound) }); err != nil {
		t.Fatal(err)
	}
	sink := newBackgroundObservationSink(context.Background(), "shutdown-owner")
	sink.Submit(&types.OpenAIResponsesResponses{ID: "resp_shutdown", Status: "completed"}, nil)
	sink.Close() // HTTP delivery is already finished; the SQL observation is not.
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("observer did not reach database")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = StopBackgroundObservers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unfinished observer reported complete: %v", err)
	}
	unblock()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = StopBackgroundObservers(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sink.done:
	default:
		t.Fatal("observer not joined")
	}
	late := newBackgroundObservationSink(context.Background(), "late")
	late.Close()
	select {
	case <-late.done:
	default:
		t.Fatal("late observer started")
	}
}
