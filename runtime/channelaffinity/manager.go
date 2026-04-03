package channelaffinity

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const defaultRedisPrefix = "one-hub:channel-affinity"

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

type Manager struct {
	mu              sync.RWMutex
	entries         map[string]entry
	defaultTTL      time.Duration
	janitorInterval time.Duration
	maxEntries      int
	stopCh          chan struct{}
	stopOnce        sync.Once
	locks           [64]sync.Mutex

	redisClient *redis.Client
	redisPrefix string
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
	manager.applyRuntimeOptions(options)
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

	m.mu.Lock()
	m.applyRuntimeOptions(options)
	m.mu.Unlock()
}

func (m *Manager) applyRuntimeOptions(options ManagerOptions) {
	if options.DefaultTTL > 0 {
		m.defaultTTL = options.DefaultTTL
	}
	if options.MaxEntries >= 0 {
		m.maxEntries = options.MaxEntries
	}
	if options.RedisClient != nil || m.redisClient == nil {
		m.redisClient = options.RedisClient
	}
	if prefix := normalizeRedisPrefix(options.RedisPrefix); prefix != "" {
		m.redisPrefix = prefix
	}
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
	m.mu.RLock()
	current, ok := m.entries[key]
	m.mu.RUnlock()
	if ok {
		if current.expired(now) {
			m.Delete(key)
			return Record{}, false
		}
		return current.record, true
	}

	if m.redisClient == nil {
		return Record{}, false
	}

	persisted, ok := m.getFromRedis(key)
	if !ok {
		return Record{}, false
	}
	if persisted.expired(now) {
		m.Delete(key)
		return Record{}, false
	}

	m.mu.Lock()
	m.entries[key] = persisted
	m.enforceCapacityLocked(now)
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
	if ttl <= 0 {
		ttl = m.defaultTTL
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
	m.enforceCapacityLocked(now)
	m.mu.Unlock()

	if m.redisClient != nil {
		m.setToRedis(key, current, ttl)
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

	m.mu.Lock()
	delete(m.entries, key)
	m.mu.Unlock()

	if m.redisClient != nil {
		m.deleteFromRedis(key)
	}
}

func (m *Manager) Clear() int {
	if m == nil {
		return 0
	}

	m.mu.Lock()
	localEntries := len(m.entries)
	m.entries = make(map[string]entry)
	m.mu.Unlock()

	if m.redisClient == nil {
		return localEntries
	}

	ctx := context.Background()
	indexKey := m.redisIndexKey()
	members, err := m.redisClient.ZRange(ctx, indexKey, 0, -1).Result()
	if err == nil && len(members) > 0 {
		pipe := m.redisClient.Pipeline()
		keys := make([]string, 0, len(members))
		for _, member := range members {
			keys = append(keys, m.redisEntryKey(member))
		}
		pipe.Del(ctx, keys...)
		pipe.Del(ctx, indexKey)
		_, _ = pipe.Exec(ctx)
		return max(localEntries, len(members))
	}

	_ = m.redisClient.Del(ctx, indexKey).Err()
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

	removed := 0
	m.mu.Lock()
	for key, current := range m.entries {
		if current.expired(now) {
			delete(m.entries, key)
			removed++
		}
	}
	m.mu.Unlock()

	if m.redisClient != nil {
		removed += m.sweepRedis(now)
	}
	return removed
}

func (m *Manager) Stats() Stats {
	if m == nil {
		return Stats{}
	}

	stats := Stats{
		Backend:    "memory",
		MaxEntries: m.maxEntries,
		DefaultTTL: int64(m.defaultTTL.Seconds()),
	}

	m.mu.RLock()
	stats.LocalEntries = len(m.entries)
	m.mu.RUnlock()

	if m.redisClient == nil {
		return stats
	}

	stats.Backend = "hybrid"
	ctx := context.Background()
	count, err := m.redisClient.ZCard(ctx, m.redisIndexKey()).Result()
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

func (m *Manager) enforceCapacityLocked(now time.Time) {
	if m.maxEntries <= 0 {
		return
	}

	for key, current := range m.entries {
		if current.expired(now) {
			delete(m.entries, key)
		}
	}

	for len(m.entries) > m.maxEntries {
		oldestKey := ""
		var oldestAt time.Time
		for key, current := range m.entries {
			candidateAt := current.record.UpdatedAt
			if !current.expiresAt.IsZero() && (candidateAt.IsZero() || current.expiresAt.Before(candidateAt)) {
				candidateAt = current.expiresAt
			}
			if oldestKey == "" || candidateAt.Before(oldestAt) {
				oldestKey = key
				oldestAt = candidateAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(m.entries, oldestKey)
	}
}

func (m *Manager) getFromRedis(key string) (entry, bool) {
	ctx := context.Background()
	raw, err := m.redisClient.Get(ctx, m.redisEntryKey(key)).Result()
	if err != nil {
		return entry{}, false
	}

	var persisted persistedEntry
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil {
		m.deleteFromRedis(key)
		return entry{}, false
	}
	return entry{
		record:    persisted.Record,
		expiresAt: persisted.ExpiresAt,
	}, true
}

func (m *Manager) setToRedis(key string, current entry, ttl time.Duration) {
	ctx := context.Background()
	payload, err := json.Marshal(persistedEntry{
		Record:    current.record,
		ExpiresAt: current.expiresAt,
	})
	if err != nil {
		return
	}

	indexKey := m.redisIndexKey()
	entryKey := m.redisEntryKey(key)
	score := float64(redisScoreForEntry(current))

	pipe := m.redisClient.Pipeline()
	if ttl > 0 {
		pipe.Set(ctx, entryKey, payload, ttl)
	} else {
		pipe.Set(ctx, entryKey, payload, 0)
	}
	pipe.ZAdd(ctx, indexKey, redis.Z{
		Score:  score,
		Member: key,
	})
	_, _ = pipe.Exec(ctx)

	m.trimRedis(time.Now())
}

func (m *Manager) deleteFromRedis(key string) {
	ctx := context.Background()
	pipe := m.redisClient.Pipeline()
	pipe.Del(ctx, m.redisEntryKey(key))
	pipe.ZRem(ctx, m.redisIndexKey(), key)
	_, _ = pipe.Exec(ctx)
}

func (m *Manager) trimRedis(now time.Time) {
	if m.redisClient == nil {
		return
	}
	_ = m.sweepRedis(now)

	if m.maxEntries <= 0 {
		return
	}

	ctx := context.Background()
	indexKey := m.redisIndexKey()
	count, err := m.redisClient.ZCard(ctx, indexKey).Result()
	if err != nil || count <= int64(m.maxEntries) {
		return
	}

	overflow := count - int64(m.maxEntries)
	members, err := m.redisClient.ZRange(ctx, indexKey, 0, overflow-1).Result()
	if err != nil || len(members) == 0 {
		return
	}

	pipe := m.redisClient.Pipeline()
	for _, member := range members {
		pipe.Del(ctx, m.redisEntryKey(member))
	}
	pipe.ZRem(ctx, indexKey, stringSliceToInterfaceSlice(members)...)
	_, _ = pipe.Exec(ctx)
}

func (m *Manager) sweepRedis(now time.Time) int {
	if m.redisClient == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}

	ctx := context.Background()
	indexKey := m.redisIndexKey()
	cutoff := strconv.FormatInt(now.UnixNano(), 10)
	members, err := m.redisClient.ZRangeByScore(ctx, indexKey, &redis.ZRangeBy{
		Min:   "-inf",
		Max:   cutoff,
		Count: 1024,
	}).Result()
	if err != nil || len(members) == 0 {
		return 0
	}

	pipe := m.redisClient.Pipeline()
	for _, member := range members {
		pipe.Del(ctx, m.redisEntryKey(member))
	}
	pipe.ZRem(ctx, indexKey, stringSliceToInterfaceSlice(members)...)
	_, _ = pipe.Exec(ctx)
	return len(members)
}

func (m *Manager) redisEntryKey(key string) string {
	return m.redisPrefix + ":entry:" + key
}

func (m *Manager) redisIndexKey() string {
	return m.redisPrefix + ":index"
}

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
