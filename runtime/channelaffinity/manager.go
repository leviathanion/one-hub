package channelaffinity

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultRedisPrefix    = "one-hub:channel-affinity"
	redisOperationTimeout = 2 * time.Second
	redisSweepBatchSize   = 1024
	redisFallbackSweepGap = time.Minute
)

var setRedisEntryScriptSource = `
local operation = 'channel_affinity_set_v1'
local entry_key = KEYS[1]
local index_key = KEYS[2]
local payload = ARGV[1]
local score = ARGV[2]
local member = ARGV[3]
local ttl_ms = tonumber(ARGV[4])
local max_entries = tonumber(ARGV[5])
local entry_prefix = ARGV[6]

if ttl_ms and ttl_ms > 0 then
  redis.call('SET', entry_key, payload, 'PX', ttl_ms)
else
  redis.call('SET', entry_key, payload)
end
redis.call('ZADD', index_key, score, member)

if not max_entries or max_entries <= 0 then
  return 0
end
local excess = redis.call('ZCARD', index_key) - max_entries
if excess <= 0 then
  return 0
end
local victims = redis.call('ZRANGE', index_key, 0, excess - 1)
for _, victim in ipairs(victims) do
  redis.call('DEL', entry_prefix .. victim)
end
if #victims > 0 then
  redis.call('ZREMRANGEBYRANK', index_key, 0, #victims - 1)
end
return #victims
`

var setRedisEntryScript = redis.NewScript(setRedisEntryScriptSource)

type Record struct {
	ChannelID         int       `json:"channel_id"`
	ResumeFingerprint string    `json:"resume_fingerprint,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type Stats struct {
	Backend        string `json:"backend"`
	LocalEntries   int    `json:"local_entries"`
	BackendEntries int64  `json:"backend_entries"`
	MaxEntries     int    `json:"max_entries"`
	DefaultTTL     int64  `json:"default_ttl_seconds"`
}

type ManagerOptions struct {
	DefaultTTL time.Duration
	// JanitorInterval is construction-time only and is ignored by UpdateOptions.
	JanitorInterval time.Duration
	MaxEntries      int
	RedisClient     *redis.Client
	RedisPrefix     string
}

type entry struct {
	record    Record
	expiresAt time.Time
}

type persistedEntry struct {
	Record    Record    `json:"record"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

type managerRuntimeOptions struct {
	defaultTTL  time.Duration
	maxEntries  int
	redisClient *redis.Client
	redisPrefix string
}

type Manager struct {
	mu              sync.RWMutex
	entries         map[string]entry
	janitorInterval time.Duration
	stopCh          chan struct{}
	stopOnce        sync.Once
	locks           [64]sync.Mutex
	runtime         atomic.Pointer[managerRuntimeOptions]
	lastRedisSweep  atomic.Int64
}

func NewManager(defaultTTL, janitorInterval time.Duration) *Manager {
	return NewManagerWithOptions(ManagerOptions{
		DefaultTTL:      defaultTTL,
		JanitorInterval: janitorInterval,
	})
}

func NewManagerWithOptions(options ManagerOptions) *Manager {
	manager := &Manager{
		entries:         make(map[string]entry),
		janitorInterval: options.JanitorInterval,
		stopCh:          make(chan struct{}),
	}
	manager.runtime.Store(applyManagerRuntimeOptions(nil, options))
	if manager.janitorInterval > 0 {
		go manager.runJanitor()
	}
	return manager
}

// UpdateOptions applies only runtime-tunable settings. Janitor lifecycle is
// fixed when the manager is constructed.
func (m *Manager) UpdateOptions(options ManagerOptions) {
	if m == nil {
		return
	}

	for {
		current := m.runtime.Load()
		next := applyManagerRuntimeOptions(current, options)
		if m.runtime.CompareAndSwap(current, next) {
			return
		}
	}
}

func applyManagerRuntimeOptions(current *managerRuntimeOptions, options ManagerOptions) *managerRuntimeOptions {
	next := &managerRuntimeOptions{}
	if current != nil {
		*next = *current
	}
	if options.DefaultTTL > 0 {
		next.defaultTTL = options.DefaultTTL
	}
	if options.MaxEntries >= 0 {
		next.maxEntries = options.MaxEntries
	}
	if options.RedisClient != nil || current == nil || current.redisClient == nil {
		next.redisClient = options.RedisClient
	}
	next.redisPrefix = normalizeRedisPrefix(options.RedisPrefix)
	return next
}

func (m *Manager) runtimeOptions() *managerRuntimeOptions {
	if m == nil {
		return &managerRuntimeOptions{redisPrefix: defaultRedisPrefix}
	}
	if options := m.runtime.Load(); options != nil {
		return options
	}
	return &managerRuntimeOptions{redisPrefix: defaultRedisPrefix}
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
}

func (m *Manager) Get(key string) (Record, bool) {
	if m == nil {
		return Record{}, false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return Record{}, false
	}

	now := time.Now()
	options := m.runtimeOptions()
	m.mu.RLock()
	current, ok := m.entries[key]
	m.mu.RUnlock()
	if ok {
		if current.expired(now) {
			m.delete(key, options)
			return Record{}, false
		}
		return current.record, true
	}

	if options.redisClient == nil {
		return Record{}, false
	}

	persisted, ok := m.getFromRedis(key, options)
	if !ok {
		return Record{}, false
	}
	if persisted.expired(now) {
		m.delete(key, options)
		return Record{}, false
	}

	m.mu.Lock()
	m.entries[key] = persisted
	m.enforceCapacityLocked(now, options.maxEntries)
	m.mu.Unlock()

	return persisted.record, true
}

func (m *Manager) Set(key string, channelID int, ttl time.Duration) {
	m.SetRecord(key, Record{ChannelID: channelID}, ttl)
}

func (m *Manager) SetRecord(key string, record Record, ttl time.Duration) {
	if m == nil || record.ChannelID <= 0 {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	options := m.runtimeOptions()
	if ttl <= 0 {
		ttl = options.defaultTTL
	}

	now := time.Now()
	current := entry{
		record: record,
	}
	current.record.ResumeFingerprint = strings.TrimSpace(current.record.ResumeFingerprint)
	current.record.UpdatedAt = now
	if ttl > 0 {
		current.expiresAt = now.Add(ttl)
	}

	m.mu.Lock()
	m.entries[key] = current
	m.enforceCapacityLocked(now, options.maxEntries)
	m.mu.Unlock()

	if options.redisClient != nil {
		m.setToRedis(key, current, ttl, options)
		m.maybeSweepRedisAsync(options)
	}
}

func (m *Manager) Delete(key string) {
	if m == nil {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	m.delete(key, m.runtimeOptions())
}

func (m *Manager) delete(key string, options *managerRuntimeOptions) {
	m.mu.Lock()
	delete(m.entries, key)
	m.mu.Unlock()

	if options.redisClient != nil {
		m.deleteFromRedis(key, options)
	}
}

func (m *Manager) Clear() int {
	if m == nil {
		return 0
	}

	options := m.runtimeOptions()
	m.mu.Lock()
	localEntries := len(m.entries)
	m.entries = make(map[string]entry)
	m.mu.Unlock()

	if options.redisClient == nil {
		return localEntries
	}

	ctx, cancel := context.WithTimeout(context.Background(), redisOperationTimeout)
	defer cancel()
	indexKey := redisIndexKey(options.redisPrefix)
	members, err := options.redisClient.ZRange(ctx, indexKey, 0, -1).Result()
	if err == nil && len(members) > 0 {
		pipe := options.redisClient.Pipeline()
		keys := make([]string, 0, len(members))
		for _, member := range members {
			keys = append(keys, redisEntryKey(options.redisPrefix, member))
		}
		pipe.Del(ctx, keys...)
		pipe.Del(ctx, indexKey)
		_, _ = pipe.Exec(ctx)
		return max(localEntries, len(members))
	}

	_ = options.redisClient.Del(ctx, indexKey).Err()
	return localEntries
}

func (m *Manager) Lock(key string) func() {
	if m == nil {
		return func() {}
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return func() {}
	}

	lock := &m.locks[m.lockIndex(key)]
	lock.Lock()
	return func() {
		lock.Unlock()
	}
}

func (m *Manager) Sweep(now time.Time) int {
	if m == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	options := m.runtimeOptions()

	removed := 0
	m.mu.Lock()
	for key, current := range m.entries {
		if current.expired(now) {
			delete(m.entries, key)
			removed++
		}
	}
	m.mu.Unlock()

	if options.redisClient != nil {
		removed += m.sweepRedis(now, options)
	}
	return removed
}

func (m *Manager) Stats() Stats {
	if m == nil {
		return Stats{}
	}

	options := m.runtimeOptions()
	stats := Stats{
		Backend:    "memory",
		MaxEntries: options.maxEntries,
		DefaultTTL: int64(options.defaultTTL.Seconds()),
	}

	m.mu.RLock()
	stats.LocalEntries = len(m.entries)
	m.mu.RUnlock()

	if options.redisClient == nil {
		return stats
	}

	stats.Backend = "hybrid"
	ctx, cancel := context.WithTimeout(context.Background(), redisOperationTimeout)
	defer cancel()
	count, err := options.redisClient.ZCard(ctx, redisIndexKey(options.redisPrefix)).Result()
	if err == nil {
		stats.BackendEntries = count
	}
	return stats
}

func (m *Manager) runJanitor() {
	ticker := time.NewTicker(m.janitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.Sweep(time.Now())
		case <-m.stopCh:
			return
		}
	}
}

func (m *Manager) enforceCapacityLocked(now time.Time, maxEntries int) {
	if maxEntries <= 0 {
		return
	}

	for key, current := range m.entries {
		if current.expired(now) {
			delete(m.entries, key)
		}
	}

	excess := len(m.entries) - maxEntries
	if excess <= 0 {
		return
	}

	// A normal Set overflows by one; keep that path allocation-free. Large
	// runtime capacity reductions sort once instead of rescanning the map for
	// every eviction.
	if excess == 1 {
		oldestKey := ""
		var oldestAt time.Time
		for key, current := range m.entries {
			candidateAt := capacityEvictionTime(current)
			if oldestKey == "" || candidateAt.Before(oldestAt) || (candidateAt.Equal(oldestAt) && key < oldestKey) {
				oldestKey = key
				oldestAt = candidateAt
			}
		}
		delete(m.entries, oldestKey)
		return
	}

	type evictionCandidate struct {
		key string
		at  time.Time
	}
	candidates := make([]evictionCandidate, 0, len(m.entries))
	for key, current := range m.entries {
		candidates = append(candidates, evictionCandidate{key: key, at: capacityEvictionTime(current)})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].at.Equal(candidates[j].at) {
			return candidates[i].key < candidates[j].key
		}
		return candidates[i].at.Before(candidates[j].at)
	})
	for i := 0; i < excess; i++ {
		delete(m.entries, candidates[i].key)
	}
}

func capacityEvictionTime(current entry) time.Time {
	candidateAt := current.record.UpdatedAt
	if !current.expiresAt.IsZero() && (candidateAt.IsZero() || current.expiresAt.Before(candidateAt)) {
		candidateAt = current.expiresAt
	}
	return candidateAt
}

func (m *Manager) getFromRedis(key string, options *managerRuntimeOptions) (entry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOperationTimeout)
	defer cancel()
	raw, err := options.redisClient.Get(ctx, redisEntryKey(options.redisPrefix, key)).Result()
	if err != nil {
		return entry{}, false
	}

	var persisted persistedEntry
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil {
		m.deleteFromRedis(key, options)
		return entry{}, false
	}
	return entry{
		record:    persisted.Record,
		expiresAt: persisted.ExpiresAt,
	}, true
}

func (m *Manager) setToRedis(key string, current entry, ttl time.Duration, options *managerRuntimeOptions) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOperationTimeout)
	defer cancel()
	payload, err := json.Marshal(persistedEntry{
		Record:    current.record,
		ExpiresAt: current.expiresAt,
	})
	if err != nil {
		return
	}

	indexKey := redisIndexKey(options.redisPrefix)
	entryKey := redisEntryKey(options.redisPrefix, key)
	score := redisScoreForEntry(current)

	ttlMilliseconds := int64(0)
	if ttl > 0 {
		ttlMilliseconds = ttl.Milliseconds()
		if ttlMilliseconds <= 0 {
			ttlMilliseconds = 1
		}
	}
	_, _ = setRedisEntryScript.Run(
		ctx,
		options.redisClient,
		[]string{entryKey, indexKey},
		payload,
		strconv.FormatInt(score, 10),
		key,
		strconv.FormatInt(ttlMilliseconds, 10),
		strconv.Itoa(options.maxEntries),
		redisEntryKey(options.redisPrefix, ""),
	).Result()
}

func (m *Manager) maybeSweepRedisAsync(options *managerRuntimeOptions) {
	if m == nil || options == nil || options.redisClient == nil || options.maxEntries > 0 || m.janitorInterval > 0 {
		return
	}
	now := time.Now()
	last := m.lastRedisSweep.Load()
	if last > 0 && now.UnixNano()-last < redisFallbackSweepGap.Nanoseconds() {
		return
	}
	if !m.lastRedisSweep.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	go m.sweepRedis(now, options)
}

func (m *Manager) deleteFromRedis(key string, options *managerRuntimeOptions) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOperationTimeout)
	defer cancel()
	pipe := options.redisClient.Pipeline()
	pipe.Del(ctx, redisEntryKey(options.redisPrefix, key))
	pipe.ZRem(ctx, redisIndexKey(options.redisPrefix), key)
	_, _ = pipe.Exec(ctx)
}

func (m *Manager) sweepRedis(now time.Time, options *managerRuntimeOptions) int {
	if options.redisClient == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}

	ctx, cancel := context.WithTimeout(context.Background(), redisOperationTimeout)
	defer cancel()
	indexKey := redisIndexKey(options.redisPrefix)
	cutoff := strconv.FormatInt(now.UnixNano(), 10)
	removed := 0
	for {
		members, err := options.redisClient.ZRangeByScore(ctx, indexKey, &redis.ZRangeBy{
			Min:   "-inf",
			Max:   cutoff,
			Count: redisSweepBatchSize,
		}).Result()
		if err != nil || len(members) == 0 {
			return removed
		}

		pipe := options.redisClient.Pipeline()
		for _, member := range members {
			pipe.Del(ctx, redisEntryKey(options.redisPrefix, member))
		}
		pipe.ZRem(ctx, indexKey, stringSliceToInterfaceSlice(members)...)
		if _, err := pipe.Exec(ctx); err != nil {
			return removed
		}
		removed += len(members)
		if len(members) < redisSweepBatchSize {
			return removed
		}
	}
}

func (m *Manager) redisEntryKey(key string) string {
	return redisEntryKey(m.runtimeOptions().redisPrefix, key)
}

func (m *Manager) redisIndexKey() string {
	return redisIndexKey(m.runtimeOptions().redisPrefix)
}

func redisEntryKey(prefix, key string) string { return prefix + ":entry:" + key }

func redisIndexKey(prefix string) string { return prefix + ":index" }

func (m *Manager) lockIndex(key string) uint32 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(key))
	return hasher.Sum32() % uint32(len(m.locks))
}

func (e entry) expired(now time.Time) bool {
	return !e.expiresAt.IsZero() && !e.expiresAt.After(now)
}

func normalizeRedisPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return defaultRedisPrefix
	}
	return prefix
}

func redisScoreForEntry(current entry) int64 {
	if !current.expiresAt.IsZero() {
		return current.expiresAt.UnixNano()
	}
	if !current.record.UpdatedAt.IsZero() {
		return current.record.UpdatedAt.UnixNano()
	}
	return time.Now().UnixNano()
}

func stringSliceToInterfaceSlice(values []string) []interface{} {
	items := make([]interface{}, 0, len(values))
	for _, value := range values {
		items = append(items, value)
	}
	return items
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
