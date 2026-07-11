package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"one-api/common/config"
	commonredis "one-api/common/redis"
)

const (
	CodexUsageGenerationKeyPrefix = "codex:usage:v2:generation"
	// Pending rotations must outlive every old usage entry. This bounds memory
	// during a prolonged cache outage without allowing old data to become current.
	CodexUsageRotationPendingTTL = 2 * time.Hour
	CodexUsageGenerationTTL      = 2*time.Hour + 15*time.Minute
	generationPublishConcurrency = 8
)

type pendingGenerationRotation struct {
	rotations uint64
	retryable bool
	running   bool
	done      chan struct{}
	expiresAt time.Time
}

type activeGenerationReads struct {
	refs     uint64
	revision uint64
}

// GenerationStore provides one atomic namespace marker shared by model mutations
// and provider reads. Pending records contain rotation intent, never a value: every
// publish attempt uses a fresh random namespace. Thus an indeterminate SET retried
// after another instance's SET can only invalidate more cache; it cannot resurrect
// an older namespace.
//
// CAP trade-off: while this process cannot reach Redis, another instance cannot
// observe its pending intent because no shared medium is available. Mutating
// controllers surface a warning, reads in this process fail closed, and this
// store's single worker keeps publishing for up to pendingTTL after connectivity
// returns. Deliberately coupling cache invalidation to the business DB would make
// that failure mode more expensive and is not justified here.
type GenerationStore struct {
	prefix     string
	markerTTL  time.Duration
	pendingTTL time.Duration

	localMu     sync.Mutex
	mu          sync.Mutex
	pending     map[int]*pendingGenerationRotation
	activeReads map[int]*activeGenerationReads

	retryDelay   time.Duration
	wakeRetry    chan struct{}
	publishSlots chan struct{}
	stop         chan struct{}
	workerDone   chan struct{}
	workerCtx    context.Context
	cancelWorker context.CancelFunc
	workerOnce   sync.Once
	closeOnce    sync.Once
	getOrInit    func(context.Context, string, string, time.Duration) (string, error)
	publish      func(context.Context, string, string, time.Duration) error
	localGet     func(context.Context, string) (string, error)
	localSet     func(context.Context, string, string, time.Duration) error
}

var generationInitScript = commonredis.NewScript(`
local current = redis.call("GET", KEYS[1])
if not current then
  redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[2], "NX")
  current = redis.call("GET", KEYS[1])
end
if current then redis.call("PEXPIRE", KEYS[1], ARGV[2]) end
return current
`)

func NewGenerationStore(prefix string, markerTTL, pendingTTL time.Duration) *GenerationStore {
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	s := &GenerationStore{
		prefix: prefix, markerTTL: markerTTL, pendingTTL: pendingTTL,
		pending: make(map[int]*pendingGenerationRotation), activeReads: make(map[int]*activeGenerationReads),
		retryDelay: 100 * time.Millisecond, wakeRetry: make(chan struct{}, 1),
		publishSlots: make(chan struct{}, generationPublishConcurrency), stop: make(chan struct{}),
		workerDone: make(chan struct{}), workerCtx: workerCtx, cancelWorker: cancelWorker,
	}
	s.getOrInit = func(ctx context.Context, key, candidate string, ttl time.Duration) (string, error) {
		value, err := commonredis.ScriptRunCtx(ctx, generationInitScript, []string{key}, candidate, ttl.Milliseconds())
		if err != nil {
			return "", err
		}
		generation, _ := value.(string)
		return generation, nil
	}
	s.publish = func(ctx context.Context, key, generation string, ttl time.Duration) error {
		return commonredis.GetRedisClient().Set(ctx, key, generation, ttl).Err()
	}
	s.localGet = GetCacheContext[string]
	s.localSet = func(ctx context.Context, key, generation string, ttl time.Duration) error {
		return SetCacheContext(ctx, key, generation, ttl)
	}
	return s
}

var CodexUsageGenerations = NewGenerationStore(CodexUsageGenerationKeyPrefix, CodexUsageGenerationTTL, CodexUsageRotationPendingTTL)

func (s *GenerationStore) key(id int) string { return fmt.Sprintf("%s:%d", s.prefix, id) }

func randomGeneration() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *GenerationStore) GetOrInit(id int) (string, error) {
	return s.GetOrInitContext(context.Background(), id)
}

func (s *GenerationStore) GetOrInitContext(ctx context.Context, id int) (string, error) {
	if id <= 0 {
		return "", fmt.Errorf("invalid generation id")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("get or initialize generation for %d: %w", id, err)
	}

	// One deadline covers pending publication, marker reads, and retries caused by
	// a concurrent Rotate. CacheTimeout caps, rather than replaces, the caller's
	// operation deadline. The per-ID revision exists only while this read is active.
	ctx, cancel := context.WithTimeout(ctx, CacheTimeout)
	defer cancel()
	s.beginRead(id)
	defer s.endRead(id)
	for {
		revision, pending := s.readRevision(id)
		if pending {
			if err := s.retryPending(ctx, id); err != nil {
				s.scheduleRetry()
				return "", fmt.Errorf("generation rotation pending for %d: %w", id, err)
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("get or initialize generation for %d: %w", id, err)
		}

		candidate, err := randomGeneration()
		if err != nil {
			return "", err
		}
		generation, err := s.readMarker(ctx, id, candidate)
		if err != nil {
			return "", fmt.Errorf("get or initialize generation for %d: %w", id, err)
		}
		if strings.TrimSpace(generation) == "" {
			return "", fmt.Errorf("empty generation for %d", id)
		}
		if s.revisionIsCurrent(id, revision) {
			return generation, nil
		}
		// Rotate registered after our first pending check. Retry even when its
		// publish already succeeded and removed the pending record.
	}
}

func (s *GenerationStore) readMarker(ctx context.Context, id int, candidate string) (string, error) {
	key := s.key(id)
	if config.RedisEnabled && commonredis.GetRedisClient() != nil {
		return s.getOrInit(ctx, key, candidate, s.markerTTL)
	}

	s.localMu.Lock()
	defer s.localMu.Unlock()
	generation, err := s.localGet(ctx, key)
	if errors.Is(err, CacheNotFound) {
		generation, err = candidate, nil
	}
	if err != nil {
		return "", err
	}
	// Cancellation may arrive after the read hook/cache adapter returns. Do not
	// turn that canceled lookup into an initialization write.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.localSet(ctx, key, generation, s.markerTTL); err != nil {
		return "", err
	}
	// A local adapter can complete the write concurrently with cancellation. The
	// marker may safely remain (it is only a namespace), but the caller must not
	// observe success after its operation was canceled.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return generation, nil
}

func (s *GenerationStore) Rotate(id int) error {
	if id <= 0 {
		return fmt.Errorf("invalid generation id")
	}
	s.remember(id)
	ctx, cancel := context.WithTimeout(context.Background(), CacheTimeout)
	err := s.retryPending(ctx, id)
	cancel()
	if err != nil {
		s.scheduleRetry()
		return fmt.Errorf("rotate generation for %d: %w", id, err)
	}
	return nil
}

// RotateMany first records every distinct valid ID, then attempts publication
// with fixed concurrency under one shared CacheTimeout budget. Unfinished work is
// left to the store-wide retry worker, so an outage costs O(CacheTimeout), not
// O(number of channels * CacheTimeout).
func (s *GenerationStore) RotateMany(ids []int) error {
	unique := make([]int, 0, len(ids))
	seen := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		s.remember(id)
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), CacheTimeout)
	defer cancel()
	jobs := make(chan int)
	workers := generationPublishConcurrency
	if len(unique) < workers {
		workers = len(unique)
	}
	var wg sync.WaitGroup
	var failuresMu sync.Mutex
	failures := 0
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for id := range jobs {
				if err := s.retryPending(ctx, id); err != nil {
					failuresMu.Lock()
					failures++
					failuresMu.Unlock()
				}
			}
		}()
	}
	for _, id := range unique {
		select {
		case jobs <- id:
		case <-ctx.Done():
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	if pending := s.pendingCountFor(unique); pending != 0 {
		s.scheduleRetry()
		cause := ctx.Err()
		if cause == nil {
			cause = errors.New("cache publish failed")
		}
		return fmt.Errorf("%d generation rotation(s) pending after batch publish (%d immediate failure(s)): %w", pending, failures, cause)
	}
	return nil
}

func (s *GenerationStore) remember(id int) {
	now := time.Now()
	s.mu.Lock()
	pending := s.pending[id]
	if pending == nil || (!pending.running && !now.Before(pending.expiresAt)) {
		pending = &pendingGenerationRotation{}
		s.pending[id] = pending
	}
	pending.rotations++
	if reads := s.activeReads[id]; reads != nil {
		reads.revision++
	}
	pending.retryable = true
	pending.expiresAt = now.Add(s.pendingTTL)
	s.mu.Unlock()
}

func (s *GenerationStore) beginRead(id int) {
	s.mu.Lock()
	reads := s.activeReads[id]
	if reads == nil {
		reads = &activeGenerationReads{}
		s.activeReads[id] = reads
	}
	reads.refs++
	s.mu.Unlock()
}

func (s *GenerationStore) endRead(id int) {
	s.mu.Lock()
	if reads := s.activeReads[id]; reads != nil {
		reads.refs--
		if reads.refs == 0 {
			delete(s.activeReads, id)
		}
	}
	s.mu.Unlock()
}

func (s *GenerationStore) hasPending(id int) bool {
	_, pending := s.readRevision(id)
	return pending
}

func (s *GenerationStore) readRevision(id int) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending[id]
	if pending != nil && !pending.running && !time.Now().Before(pending.expiresAt) {
		delete(s.pending, id)
		pending = nil
	}
	reads := s.activeReads[id]
	if reads == nil {
		return 0, pending != nil
	}
	return reads.revision, pending != nil
}

func (s *GenerationStore) revisionIsCurrent(id int, revision uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending[id]
	if pending != nil && !pending.running && !time.Now().Before(pending.expiresAt) {
		delete(s.pending, id)
		pending = nil
	}
	reads := s.activeReads[id]
	return reads != nil && reads.revision == revision && pending == nil
}

func (s *GenerationStore) retryPending(ctx context.Context, id int) error {
	for {
		s.mu.Lock()
		pending := s.pending[id]
		if pending == nil {
			s.mu.Unlock()
			return nil
		}
		if !pending.running && !time.Now().Before(pending.expiresAt) {
			delete(s.pending, id)
			s.mu.Unlock()
			return nil
		}
		if pending.running {
			done := pending.done
			s.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		pending.running, pending.done = true, make(chan struct{})
		done := pending.done
		s.mu.Unlock()

		select {
		case s.publishSlots <- struct{}{}:
		case <-ctx.Done():
			err := ctx.Err()
			s.finishPublishAttempt(id, pending, done, err)
			return err
		}
		generation, err := randomGeneration()
		if err == nil {
			err = s.publishGeneration(ctx, id, generation)
		}
		<-s.publishSlots
		s.finishPublishAttempt(id, pending, done, err)
		if err != nil {
			return err
		}
		// A Rotate arriving during publish increments rotations on this same record;
		// consume it before reporting success so it cannot be lost.
	}
}

func (s *GenerationStore) finishPublishAttempt(id int, pending *pendingGenerationRotation, done chan struct{}, err error) {
	s.mu.Lock()
	if s.pending[id] == pending {
		pending.running = false
		if err == nil {
			pending.rotations--
			pending.retryable = pending.rotations != 0
		} else {
			pending.retryable = !errors.Is(err, CacheNotInitialized)
		}
		if pending.rotations == 0 || !time.Now().Before(pending.expiresAt) {
			delete(s.pending, id)
		}
		close(done)
	}
	s.mu.Unlock()
}

func (s *GenerationStore) publishGeneration(ctx context.Context, id int, generation string) error {
	key := s.key(id)
	if config.RedisEnabled && commonredis.GetRedisClient() != nil {
		return s.publish(ctx, key, generation, s.markerTTL)
	}
	s.localMu.Lock()
	defer s.localMu.Unlock()
	return SetCache(key, generation, s.markerTTL)
}

func (s *GenerationStore) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

func (s *GenerationStore) pendingCountFor(ids []int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, id := range ids {
		if _, ok := s.pending[id]; ok {
			count++
		}
	}
	return count
}

func (s *GenerationStore) scheduleRetry() {
	select {
	case <-s.stop:
		return
	default:
	}
	s.workerOnce.Do(func() { go s.retryWorker() })
	select {
	case s.wakeRetry <- struct{}{}:
	case <-s.stop:
	default:
	}
}

// Close stops this store's retry worker. The process-wide production store is
// intentionally never closed; short-lived stores (especially tests) must close.
func (s *GenerationStore) Close() {
	s.closeOnce.Do(func() {
		s.workerOnce.Do(func() { go s.retryWorker() })
		close(s.stop)
		s.cancelWorker()
		<-s.workerDone
	})
}

func (s *GenerationStore) retryWorker() {
	defer close(s.workerDone)
	for {
		select {
		case <-s.wakeRetry:
		case <-s.stop:
			return
		}
		for {
			s.mu.Lock()
			ids := make([]int, 0, len(s.pending))
			now := time.Now()
			var nextExpiry time.Time
			for id, pending := range s.pending {
				if !pending.running && !now.Before(pending.expiresAt) {
					delete(s.pending, id)
					continue
				}
				if nextExpiry.IsZero() || pending.expiresAt.Before(nextExpiry) {
					nextExpiry = pending.expiresAt
				}
				if pending.retryable && !pending.running {
					ids = append(ids, id)
				}
			}
			remaining := len(s.pending)
			s.mu.Unlock()
			if remaining == 0 {
				break
			}
			for _, id := range ids {
				ctx, cancel := context.WithTimeout(s.workerCtx, CacheTimeout)
				_ = s.retryPending(ctx, id)
				cancel()
				if s.workerCtx.Err() != nil {
					return
				}
			}

			delay := s.retryDelay
			if len(ids) == 0 {
				delay = time.Until(nextExpiry)
				if delay < 0 {
					delay = 0
				}
			}
			timer := time.NewTimer(delay)
			select {
			case <-s.wakeRetry:
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
			case <-s.stop:
				if !timer.Stop() {
					<-timer.C
				}
				return
			}
		}
	}
}

func GetOrInitCodexUsageGeneration(channelID int) (string, error) {
	return GetOrInitCodexUsageGenerationContext(context.Background(), channelID)
}

func GetOrInitCodexUsageGenerationContext(ctx context.Context, channelID int) (string, error) {
	return CodexUsageGenerations.GetOrInitContext(ctx, channelID)
}

func RotateCodexUsageGeneration(channelID int) error {
	return CodexUsageGenerations.Rotate(channelID)
}

func RotateCodexUsageGenerations(channelIDs []int) error {
	return CodexUsageGenerations.RotateMany(channelIDs)
}
