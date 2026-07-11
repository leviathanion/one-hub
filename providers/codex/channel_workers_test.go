package codex

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/logger"
	"one-api/model"
)

func TestRunCodexChannelWorkersUsesConfiguredConcurrency(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1},
		{Id: 2},
		{Id: 3},
		{Id: 4},
	}
	started := make(chan int, len(channels))
	release := make(chan struct{})
	done := make(chan struct{})

	go func() {
		runCodexChannelWorkers(context.Background(), channels, len(channels), func(_ context.Context, channel *model.Channel) {
			started <- channel.Id
			<-release
		})
		close(done)
	}()

	for i := 0; i < len(channels); i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("expected worker %d to start before release", i+1)
		}
	}

	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expected workers to finish after release")
	}
}

func TestRunCodexChannelWorkersSharesConcurrencyAcrossRuns(t *testing.T) {
	first := make([]*model.Channel, autoRefreshConcurrency)
	second := make([]*model.Channel, autoRefreshConcurrency)
	for index := 0; index < autoRefreshConcurrency; index++ {
		first[index] = &model.Channel{Id: index + 1}
		second[index] = &model.Channel{Id: autoRefreshConcurrency + index + 1}
	}

	started := make(chan struct{}, len(first)+len(second))
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	results := make(chan codexChannelWorkerResult, 2)
	run := func(channels []*model.Channel) {
		results <- runCodexChannelWorkers(context.Background(), channels, len(channels), func(_ context.Context, _ *model.Channel) {
			started <- struct{}{}
			<-release
		})
	}
	go run(first)
	go run(second)

	for index := 0; index < autoRefreshConcurrency; index++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("expected global worker slot %d to start", index+1)
		}
	}
	select {
	case <-started:
		t.Fatalf("expected at most %d callbacks across concurrent runs", autoRefreshConcurrency)
	case <-time.After(50 * time.Millisecond):
	}

	releaseAll()
	for index := 0; index < 2; index++ {
		select {
		case result := <-results:
			if result.Processed != autoRefreshConcurrency || result.FailureCount() != 0 {
				t.Fatalf("unexpected worker result: %+v", result)
			}
		case <-time.After(time.Second):
			t.Fatal("expected concurrent worker run to finish")
		}
	}
}

func TestRunCodexChannelWorkersReportsSkippedOnCancellation(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1},
		{Id: 2},
		{Id: 3},
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan codexChannelWorkerResult, 1)

	go func() {
		done <- runCodexChannelWorkers(ctx, channels, 1, func(_ context.Context, _ *model.Channel) {
			close(started)
			cancel()
			<-release
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected first worker to start")
	}
	close(release)

	select {
	case result := <-done:
		if result.Err != context.Canceled {
			t.Fatalf("expected context canceled error, got %v", result.Err)
		}
		if result.Processed != 1 || result.StartedPrefix != 1 {
			t.Fatalf("expected one-channel contiguous prefix, got %+v", result)
		}
		if result.Skipped != 2 {
			t.Fatalf("expected two skipped channels, got %d", result.Skipped)
		}
	case <-time.After(time.Second):
		t.Fatal("expected workers to finish")
	}
}

func TestRunCodexChannelWorkersReportsPanicAndContinues(t *testing.T) {
	if logger.Logger == nil {
		logger.SetupLogger()
	}
	channels := []*model.Channel{{Id: 1}, {Id: 2}}
	visitedSecond := false

	result := runCodexChannelWorkers(context.Background(), channels, 1, func(_ context.Context, channel *model.Channel) {
		if channel.Id == 1 {
			panic("boom")
		}
		visitedSecond = true
	})

	if result.Processed != 2 || result.Panicked != 1 || result.FailureCount() != 1 {
		t.Fatalf("unexpected panic result: %+v", result)
	}
	if !errors.Is(result.FirstPanicErr, errCodexChannelWorkerPanic) {
		t.Fatalf("expected structured panic error, got %v", result.FirstPanicErr)
	}
	if !visitedSecond {
		t.Fatal("expected worker to continue after a recovered panic")
	}
}

func TestRunCodexChannelWorkersRedactsPanicTextFromLogAndResult(t *testing.T) {
	const secret = "round26-worker-secret"
	const channelID = 260026

	result := runCodexChannelWorkers(context.Background(), []*model.Channel{{Id: channelID}}, 1, func(_ context.Context, _ *model.Channel) {
		panic("Authorization: Bearer " + secret)
	})

	if result.FirstPanicErr == nil || !errors.Is(result.FirstPanicErr, errCodexChannelWorkerPanic) {
		t.Fatalf("expected structured panic error, got %v", result.FirstPanicErr)
	}
	if strings.Contains(result.FirstPanicErr.Error(), secret) {
		t.Fatalf("structured panic error leaked secret: %q", result.FirstPanicErr)
	}
	if !strings.Contains(result.FirstPanicErr.Error(), "[redacted]") {
		t.Fatalf("structured panic error omitted redacted panic diagnostic: %q", result.FirstPanicErr)
	}

	entries, err := logger.GetLatestLogs(500)
	if err != nil {
		t.Fatalf("get panic log: %v", err)
	}
	var panicLog string
	for _, entry := range entries {
		if strings.Contains(entry.Message, "channel 260026") {
			panicLog = entry.Message
		}
	}
	if panicLog == "" {
		t.Fatal("expected captured channel worker panic log")
	}
	if strings.Contains(panicLog, secret) {
		t.Fatalf("panic log leaked secret: %q", panicLog)
	}
	if !strings.Contains(panicLog, "[redacted]") {
		t.Fatalf("panic log omitted redacted panic diagnostic: %q", panicLog)
	}
	if !strings.Contains(panicLog, "stack:") {
		t.Fatalf("panic log did not retain stack: %q", panicLog)
	}
}

func TestRunCodexChannelWorkersDoesNotShortenRoundDeadline(t *testing.T) {
	parentDeadline := time.Now().Add(5 * time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), parentDeadline)
	defer cancel()

	var callbackDeadline time.Time
	result := runCodexChannelWorkers(ctx, []*model.Channel{{Id: 1}}, 1, func(workerCtx context.Context, _ *model.Channel) {
		var ok bool
		callbackDeadline, ok = workerCtx.Deadline()
		if !ok {
			t.Fatal("expected worker to retain round deadline")
		}
	})

	if result.Processed != 1 || result.FailureCount() != 0 {
		t.Fatalf("unexpected worker result: %+v", result)
	}
	if callbackDeadline.Sub(parentDeadline) > time.Millisecond || parentDeadline.Sub(callbackDeadline) > time.Millisecond {
		t.Fatalf("worker shortened OAuth retry budget: got %v want %v", callbackDeadline, parentDeadline)
	}
}

func TestRunCodexChannelWorkersUsesRoundDeadlineForChannel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	observedErr := make(chan error, 1)
	result := runCodexChannelWorkers(ctx, []*model.Channel{{Id: 1}}, 1, func(ctx context.Context, _ *model.Channel) {
		<-ctx.Done()
		observedErr <- ctx.Err()
	})

	if result.Processed != 1 {
		t.Fatalf("expected timed-out channel to be processed, got %+v", result)
	}
	if err := <-observedErr; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected round deadline, got %v", err)
	}
}

func TestLongestStartedPrefixDoesNotUseOutOfOrderProcessedCount(t *testing.T) {
	started := make([]atomic.Bool, 5)
	started[0].Store(true)
	started[2].Store(true)
	started[3].Store(true)
	if got := longestStartedPrefix(started); got != 1 {
		t.Fatalf("out-of-order starts must not advance past gap: got %d want 1", got)
	}
	started[1].Store(true)
	if got := longestStartedPrefix(started); got != 4 {
		t.Fatalf("expected prefix to close once gap starts: got %d want 4", got)
	}
}

func TestCodexChannelCursorUsesStableIDBoundaryAcrossChurn(t *testing.T) {
	channels := []*model.Channel{{Id: 5}, {Id: 4}, {Id: 3}, {Id: 2}, {Id: 1}}
	cursor := &codexChannelCursor{}

	cursor.advance(channels, 2)                                                       // boundary=4
	churned := []*model.Channel{{Id: 7}, {Id: 6}, {Id: 5}, {Id: 3}, {Id: 2}, {Id: 1}} // 4 deleted, 6/7 inserted
	rotated := cursor.rotate(churned)
	got := []int{rotated[0].Id, rotated[1].Id, rotated[2].Id, rotated[3].Id, rotated[4].Id, rotated[5].Id}
	want := []int{3, 2, 1, 7, 6, 5}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("unexpected churned order: got %v want %v", got, want)
		}
	}

	cursor.advance(rotated, 3) // consumed old tail through id=1
	rotated = cursor.rotate(churned)
	if rotated[0].Id != 7 {
		t.Fatalf("expected tail completion to wrap to newest channel, got %d", rotated[0].Id)
	}
}
