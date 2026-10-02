package scheduler

import (
	"context"
	"errors"
	"github.com/go-co-op/gocron/v2"
	"sync"
	"testing"
	"time"
)

func TestShutdownWaitsForRunningScheduledWorkAndFencesAdmission(t *testing.T) {
	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatal(err)
	}
	tm := &TaskManager{scheduler: s, jobs: make(map[string]*JobInfo)}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	if err = tm.AddJob("inflight", gocron.OneTimeJob(gocron.OneTimeJobStartImmediately()), gocron.NewTask(func() { close(entered); <-release })); err != nil {
		t.Fatal(err)
	}
	s.Start()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("job never ran")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = tm.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unfinished job reported complete: %v", err)
	}
	if err = tm.AddJob("late", gocron.DurationJob(time.Hour), gocron.NewTask(func() {})); err == nil {
		t.Fatal("accepted work after stop")
	}
	unblock()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = tm.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
