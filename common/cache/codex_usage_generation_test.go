package cache

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	commonredis "one-api/common/redis"

	"github.com/redis/go-redis/v9"
)

func useRedisGenerationHooks(t *testing.T) {
	t.Helper()
	oldEnabled, oldClient := config.RedisEnabled, commonredis.RDB
	config.RedisEnabled = true
	commonredis.RDB = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() {
		config.RedisEnabled, commonredis.RDB = oldEnabled, oldClient
	})
}

func newTestGenerationStore(t *testing.T, prefix string, markerTTL, pendingTTL time.Duration) *GenerationStore {
	t.Helper()
	store := NewGenerationStore(prefix, markerTTL, pendingTTL)
	t.Cleanup(store.Close)
	return store
}

func TestGenerationStoreGetOrInitContextHonorsCancellation(t *testing.T) {
	useRedisGenerationHooks(t)
	store := newTestGenerationStore(t, "test:generation-cancel", time.Hour, time.Hour)
	var calls atomic.Int32
	store.getOrInit = func(ctx context.Context, _ string, _ string, _ time.Duration) (string, error) {
		calls.Add(1)
		<-ctx.Done()
		return "", ctx.Err()
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.GetOrInitContext(canceled, 41); !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled request returned %v, want context.Canceled", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("already canceled request reached Redis hook %d times", calls.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := store.GetOrInitContext(ctx, 42)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight cancellation returned %v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("generation lookup kept waiting after caller cancellation")
	}
}

func TestGenerationStoreLocalReadCancellationSkipsMarkerWrite(t *testing.T) {
	oldEnabled := config.RedisEnabled
	config.RedisEnabled = false
	t.Cleanup(func() { config.RedisEnabled = oldEnabled })

	store := newTestGenerationStore(t, "test:generation-local-read-cancel", time.Hour, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	store.localGet = func(context.Context, string) (string, error) {
		cancel()
		return "", CacheNotFound
	}
	var writes atomic.Int32
	store.localSet = func(context.Context, string, string, time.Duration) error {
		writes.Add(1)
		return nil
	}

	if _, err := store.GetOrInitContext(ctx, 43); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation after marker read returned %v, want context.Canceled", err)
	}
	if got := writes.Load(); got != 0 {
		t.Fatalf("canceled marker lookup performed %d writes, want 0", got)
	}
}

func TestGenerationStoreLocalWriteCancellationIsNotSuccess(t *testing.T) {
	oldEnabled := config.RedisEnabled
	config.RedisEnabled = false
	t.Cleanup(func() { config.RedisEnabled = oldEnabled })

	store := newTestGenerationStore(t, "test:generation-local-write-cancel", time.Hour, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	store.localGet = func(context.Context, string) (string, error) {
		return "current", nil
	}
	store.localSet = func(context.Context, string, string, time.Duration) error {
		cancel()
		return nil
	}

	if generation, err := store.GetOrInitContext(ctx, 44); !errors.Is(err, context.Canceled) || generation != "" {
		t.Fatalf("cancellation after marker write returned generation=%q err=%v", generation, err)
	}
}

func TestGenerationStoreRetryUsesFreshNamespace(t *testing.T) {
	useRedisGenerationHooks(t)
	store := newTestGenerationStore(t, "test:generation-fresh", time.Hour, time.Hour)
	var values []string
	store.publish = func(_ context.Context, _ string, value string, _ time.Duration) error {
		values = append(values, value)
		if len(values) == 1 {
			// Model an indeterminate network result: Redis may have applied this SET.
			return errors.New("result unknown")
		}
		return nil
	}
	store.remember(40)
	if err := store.retryPending(context.Background(), 40); err == nil {
		t.Fatal("expected indeterminate first result")
	}
	if err := store.retryPending(context.Background(), 40); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if len(values) != 2 || values[0] == values[1] {
		t.Fatalf("each publish attempt must use a fresh namespace: %v", values)
	}
}

func TestGenerationStoreRotateDuringPublishIsNotLost(t *testing.T) {
	useRedisGenerationHooks(t)
	store := newTestGenerationStore(t, "test:generation-interleave", time.Hour, time.Hour)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var values []string
	store.publish = func(_ context.Context, _ string, value string, _ time.Duration) error {
		mu.Lock()
		values = append(values, value)
		call := len(values)
		mu.Unlock()
		if call == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- store.Rotate(41) }()
	<-firstStarted
	secondDone := make(chan error, 1)
	go func() { secondDone <- store.Rotate(41) }()

	deadline := time.Now().Add(time.Second)
	for {
		store.mu.Lock()
		rotations := store.pending[41].rotations
		store.mu.Unlock()
		if rotations == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second rotation was not registered during first publish")
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Rotate failed: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Rotate failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(values) != 2 {
		t.Fatalf("want two published rotations, got %d", len(values))
	}
	if values[0] == values[1] {
		t.Fatal("successive rotations reused a namespace")
	}
}

func TestGenerationStoreGetOrInitRejectsReadOverlappingFailedRotation(t *testing.T) {
	useRedisGenerationHooks(t)
	store := newTestGenerationStore(t, "test:generation-failed-read-race", time.Hour, time.Hour)
	store.retryDelay = time.Hour
	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	store.getOrInit = func(context.Context, string, string, time.Duration) (string, error) {
		close(readStarted)
		<-releaseRead
		return "stale", nil
	}
	unavailable := atomic.Bool{}
	unavailable.Store(true)
	store.publish = func(context.Context, string, string, time.Duration) error {
		if unavailable.Load() {
			return errors.New("cache unavailable")
		}
		return nil
	}

	readDone := make(chan struct {
		generation string
		err        error
	}, 1)
	go func() {
		generation, err := store.GetOrInit(42)
		readDone <- struct {
			generation string
			err        error
		}{generation, err}
	}()
	<-readStarted
	if err := store.Rotate(42); err == nil {
		t.Fatal("expected rotation failure")
	}
	close(releaseRead)

	result := <-readDone
	if result.err == nil || result.generation != "" {
		t.Fatalf("read overlapping pending rotation returned stale marker: generation=%q err=%v", result.generation, result.err)
	}
	if !store.hasPending(42) {
		t.Fatal("failed rotation must remain pending")
	}

	// Let the store worker finish before the Redis test hook is restored.
	unavailable.Store(false)
	store.scheduleRetry()
	deadline := time.Now().Add(time.Second)
	for store.hasPending(42) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if store.hasPending(42) {
		t.Fatal("pending rotation did not recover during test cleanup")
	}
}

func TestGenerationStoreGetOrInitRejectsReadOverlappingCompletedRotation(t *testing.T) {
	useRedisGenerationHooks(t)
	store := newTestGenerationStore(t, "test:generation-completed-read-race", time.Hour, time.Hour)
	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	var reads atomic.Int32
	store.getOrInit = func(context.Context, string, string, time.Duration) (string, error) {
		if reads.Add(1) == 1 {
			close(readStarted)
			<-releaseRead
			return "stale", nil
		}
		return "current", nil
	}
	store.publish = func(context.Context, string, string, time.Duration) error { return nil }

	readDone := make(chan struct {
		generation string
		err        error
	}, 1)
	go func() {
		generation, err := store.GetOrInit(43)
		readDone <- struct {
			generation string
			err        error
		}{generation, err}
	}()
	<-readStarted
	if err := store.Rotate(43); err != nil {
		t.Fatalf("rotation failed: %v", err)
	}
	if store.hasPending(43) {
		t.Fatal("successful rotation must clear pending before old read returns")
	}
	close(releaseRead)

	result := <-readDone
	if result.err != nil || result.generation != "current" {
		t.Fatalf("overlapping read was not retried: generation=%q err=%v", result.generation, result.err)
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("marker read count = %d, want 2", got)
	}
}

func TestGenerationStoreGetOrInitIgnoresUnrelatedRotation(t *testing.T) {
	useRedisGenerationHooks(t)
	store := newTestGenerationStore(t, "test:generation-per-id-revision", time.Hour, time.Hour)
	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	var reads atomic.Int32
	store.getOrInit = func(context.Context, string, string, time.Duration) (string, error) {
		if reads.Add(1) == 1 {
			close(readStarted)
			<-releaseRead
			return "first", nil
		}
		return "retried", nil
	}
	store.publish = func(context.Context, string, string, time.Duration) error { return nil }

	result := make(chan string, 1)
	go func() {
		generation, err := store.GetOrInit(50)
		if err != nil {
			result <- "error: " + err.Error()
			return
		}
		result <- generation
	}()
	<-readStarted
	for id := 51; id < 1051; id++ {
		if err := store.Rotate(id); err != nil {
			t.Fatalf("unrelated rotation %d failed: %v", id, err)
		}
	}
	close(releaseRead)
	if got := <-result; got != "first" {
		t.Fatalf("read overlapping unrelated rotation returned %q", got)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("unrelated rotation caused marker read count %d, want 1", got)
	}
}

func TestGenerationStoreDoesNotRetainHistoricalRevisionIDs(t *testing.T) {
	useRedisGenerationHooks(t)
	store := newTestGenerationStore(t, "test:generation-no-history", time.Hour, time.Hour)
	store.publish = func(context.Context, string, string, time.Duration) error { return nil }

	const historicalIDs = 5000
	for id := 1; id <= historicalIDs; id++ {
		if err := store.Rotate(id); err != nil {
			t.Fatalf("rotate %d: %v", id, err)
		}
	}
	if got := store.pendingCount(); got != 0 {
		t.Fatalf("successful historical rotations retained %d pending entries", got)
	}
	if _, exists := reflect.TypeOf(store).Elem().FieldByName("revisions"); exists {
		t.Fatal("GenerationStore retains a historical per-channel revisions map")
	}
	store.mu.Lock()
	activeReads := len(store.activeReads)
	store.mu.Unlock()
	if activeReads != 0 {
		t.Fatalf("historical rotations retained %d active read states", activeReads)
	}
}

func TestGenerationStoreCloseWakesSleepingRetryWorker(t *testing.T) {
	useRedisGenerationHooks(t)
	store := newTestGenerationStore(t, "test:generation-close", time.Hour, time.Hour)
	store.retryDelay = time.Hour
	store.publish = func(context.Context, string, string, time.Duration) error {
		return errors.New("cache unavailable")
	}
	if err := store.Rotate(42); err == nil {
		t.Fatal("expected rotation failure")
	}

	closed := make(chan struct{})
	go func() {
		store.Close()
		store.Close() // Close is idempotent.
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not wake the retry worker")
	}
}

func TestGenerationStorePendingFailureFailsClosedUntilRecovery(t *testing.T) {
	useRedisGenerationHooks(t)
	store := newTestGenerationStore(t, "test:generation-closed", time.Hour, 40*time.Millisecond)
	store.retryDelay = 5 * time.Millisecond
	store.getOrInit = func(context.Context, string, string, time.Duration) (string, error) {
		return "stale", nil
	}
	unavailable := atomic.Bool{}
	unavailable.Store(true)
	store.publish = func(context.Context, string, string, time.Duration) error {
		if unavailable.Load() {
			return errors.New("cache unavailable")
		}
		return nil
	}

	if err := store.Rotate(42); err == nil {
		t.Fatal("expected rotation failure")
	}
	if generation, err := store.GetOrInit(42); err == nil || generation != "" {
		t.Fatalf("pending rotation must bypass stale cache: generation=%q err=%v", generation, err)
	}
	unavailable.Store(false)
	generation, err := store.GetOrInit(42)
	if err != nil || generation != "stale" {
		t.Fatalf("expected synchronous pending recovery then normal read: generation=%q err=%v", generation, err)
	}
}

func TestGenerationStoreSingleBatchSchedulerBoundsConcurrencyAndCleansExpiry(t *testing.T) {
	useRedisGenerationHooks(t)
	store := newTestGenerationStore(t, "test:generation-batch", time.Hour, 40*time.Millisecond)
	store.retryDelay = 5 * time.Millisecond
	var active atomic.Int32
	var maximum atomic.Int32
	firstWave := make(chan struct{})
	var release sync.Once
	store.publish = func(context.Context, string, string, time.Duration) error {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		if n == generationPublishConcurrency {
			release.Do(func() { close(firstWave) })
		}
		<-firstWave
		return errors.New("cache unavailable")
	}

	ids := make([]int, 200)
	for i := range ids {
		ids[i] = i + 1
	}
	ids = append(ids, 1, 2, 3)
	if err := store.RotateMany(ids); err == nil {
		t.Fatal("expected batch failure")
	}
	if got := maximum.Load(); got > generationPublishConcurrency {
		t.Fatalf("batch exceeded fixed concurrency: got %d want <= %d", got, generationPublishConcurrency)
	}
	store.mu.Lock()
	pending := len(store.pending)
	store.mu.Unlock()
	if pending != 200 {
		t.Fatalf("all distinct IDs must be registered before publishing: got %d", pending)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for store.pendingCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := store.pendingCount(); got != 0 {
		t.Fatalf("expired pending map entries were retained: %d", got)
	}
}

func TestGenerationStoreRotateManyIgnoresUnrelatedPendingIntent(t *testing.T) {
	useRedisGenerationHooks(t)
	store := newTestGenerationStore(t, "test:generation-unrelated", time.Hour, time.Hour)
	store.retryDelay = time.Hour
	store.publish = func(_ context.Context, key, _ string, _ time.Duration) error {
		if strings.HasSuffix(key, ":1") {
			return errors.New("channel A unavailable")
		}
		return nil
	}

	if err := store.Rotate(1); err == nil {
		t.Fatal("expected channel A rotation to remain pending")
	}
	if err := store.RotateMany([]int{2, 2}); err != nil {
		t.Fatalf("channel B batch must not inherit unrelated pending A: %v", err)
	}
	if !store.hasPending(1) {
		t.Fatal("expected unrelated channel A intent to remain pending")
	}
	if store.hasPending(2) {
		t.Fatal("expected channel B intent to publish successfully")
	}
}

func TestCodexPendingRotationTTLExceedsUsageEntryLifetimeFloor(t *testing.T) {
	if CodexUsageRotationPendingTTL < 2*time.Hour {
		t.Fatalf("pending TTL %s is too short to outlive old usage entries", CodexUsageRotationPendingTTL)
	}
}
