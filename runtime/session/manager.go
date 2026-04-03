package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type CleanupFunc func(*ExecutionSession)

var ErrCapacityExceeded = errors.New("execution session capacity exceeded")
var ErrCallerCapacityExceeded = errors.New("execution session caller capacity exceeded")

type ManagerOptions struct {
	DefaultTTL           time.Duration
	JanitorInterval      time.Duration
	Cleanup              CleanupFunc
	MaxSessions          int
	MaxSessionsPerCaller int
	RedisClient          *redis.Client
	RedisPrefix          string
}

type Stats struct {
	Backend              string `json:"backend"`
	LocalSessions        int    `json:"local_sessions"`
	LocalBindings        int    `json:"local_bindings"`
	BackendBindings      int64  `json:"backend_bindings"`
	MaxSessions          int    `json:"max_sessions"`
	MaxSessionsPerCaller int    `json:"max_sessions_per_caller"`
	DefaultTTLSeconds    int64  `json:"default_ttl_seconds"`
}

type Manager struct {
	mu            sync.RWMutex
	sessions      map[string]*ExecutionSession
	bindings      map[string]*Binding
	index         map[string]map[string]struct{}
	capacityIndex map[string]map[string]struct{}

	defaultTTL           time.Duration
	janitorInterval      time.Duration
	maxSessions          int
	maxSessionsPerCaller int
	cleanup              CleanupFunc
	redisClient          *redis.Client
	redisPrefix          string
	backend              bindingBackend
	stopCh               chan struct{}
	stopOnce             sync.Once
}

type pendingBindingDelete struct {
	bindingKey string
	sessionKey string
}

func NewManager(defaultTTL, janitorInterval time.Duration, cleanup CleanupFunc) *Manager {
	return NewManagerWithOptions(ManagerOptions{
		DefaultTTL:      defaultTTL,
		JanitorInterval: janitorInterval,
		Cleanup:         cleanup,
	})
}

func NewManagerWithOptions(options ManagerOptions) *Manager {
	m := &Manager{
		sessions:             make(map[string]*ExecutionSession),
		bindings:             make(map[string]*Binding),
		index:                make(map[string]map[string]struct{}),
		capacityIndex:        make(map[string]map[string]struct{}),
		defaultTTL:           options.DefaultTTL,
		janitorInterval:      options.JanitorInterval,
		maxSessions:          options.MaxSessions,
		maxSessionsPerCaller: options.MaxSessionsPerCaller,
		cleanup:              options.Cleanup,
		redisClient:          options.RedisClient,
		redisPrefix:          normalizeSessionRedisPrefix(options.RedisPrefix),
		backend:              newRedisBindingBackend(options.RedisClient, options.RedisPrefix),
		stopCh:               make(chan struct{}),
	}

	if options.JanitorInterval > 0 {
		go m.runJanitor()
	}

	return m
}

func (m *Manager) GetOrCreate(meta Metadata) (*ExecutionSession, bool, error) {
	now := time.Now()
	toCleanup := make([]*ExecutionSession, 0, 1)
	pendingDeletes := make([]pendingBindingDelete, 0, 1)

	m.mu.Lock()
	sess, created, err := m.getOrCreateLocked(meta, now, &toCleanup, &pendingDeletes)
	m.mu.Unlock()

	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)
	return sess, created, err
}

func (m *Manager) AcquireOrCreate(meta Metadata) (*ExecutionSession, bool, func(), error) {
	now := time.Now()
	toCleanup := make([]*ExecutionSession, 0, 1)
	pendingDeletes := make([]pendingBindingDelete, 0, 1)

	m.mu.Lock()
	sess, created, err := m.getOrCreateLocked(meta, now, &toCleanup, &pendingDeletes)
	if err != nil {
		m.mu.Unlock()
		m.applyPendingBindingDeletes(pendingDeletes)
		m.cleanupSessions(toCleanup)
		return nil, false, nil, err
	}
	sess.reserveLease()
	m.mu.Unlock()

	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)

	var releaseOnce sync.Once
	return sess, created, func() {
		releaseOnce.Do(func() {
			sess.releaseLease()
		})
	}, nil
}

func (m *Manager) AcquireExisting(sessionKey string) (*ExecutionSession, func(), bool) {
	if strings.TrimSpace(sessionKey) == "" {
		return nil, nil, false
	}

	now := time.Now()
	toCleanup := make([]*ExecutionSession, 0, 1)
	pendingDeletes := make([]pendingBindingDelete, 0, 1)

	m.mu.Lock()
	sess := m.sessions[sessionKey]
	if sess == nil {
		m.mu.Unlock()
		return nil, nil, false
	}
	if m.sessionExpiredLocked(sess, now) {
		if removed, deletes := m.deleteSessionLocked(sessionKey); removed != nil {
			toCleanup = append(toCleanup, removed)
			pendingDeletes = append(pendingDeletes, deletes...)
		}
		m.mu.Unlock()
		m.applyPendingBindingDeletes(pendingDeletes)
		m.cleanupSessions(toCleanup)
		return nil, nil, false
	}
	sess.reserveLease()
	m.mu.Unlock()

	var releaseOnce sync.Once
	return sess, func() {
		releaseOnce.Do(func() {
			sess.releaseLease()
		})
	}, true
}

func (m *Manager) GetOrCreateBound(meta Metadata) (*ExecutionSession, bool, *Binding, error) {
	now := time.Now()
	toCleanup := make([]*ExecutionSession, 0, 1)
	pendingDeletes := make([]pendingBindingDelete, 0, 1)

	m.mu.Lock()
	binding := m.liveLocalBindingLocked(meta.BindingKey, now, &toCleanup, &pendingDeletes)
	if binding != nil && binding.SessionKey != meta.Key {
		copyBinding := *binding
		m.mu.Unlock()
		m.applyPendingBindingDeletes(pendingDeletes)
		m.cleanupSessions(toCleanup)
		return nil, false, &copyBinding, nil
	}

	sess, created, err := m.getOrCreateLocked(meta, now, &toCleanup, &pendingDeletes)
	if err != nil {
		m.mu.Unlock()
		m.applyPendingBindingDeletes(pendingDeletes)
		m.cleanupSessions(toCleanup)
		return nil, false, nil, err
	}
	if strings.TrimSpace(meta.BindingKey) != "" {
		if strings.TrimSpace(sess.BindingKey) == "" {
			sess.BindingKey = meta.BindingKey
		}
		if binding == nil || binding.SessionKey != sess.Key {
			m.upsertBindingLocked(sess)
		}
	}
	m.mu.Unlock()

	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)
	return sess, created, nil, nil
}

func (m *Manager) AcquireOrCreateBound(meta Metadata) (*ExecutionSession, bool, *Binding, func(), error) {
	now := time.Now()
	toCleanup := make([]*ExecutionSession, 0, 1)
	pendingDeletes := make([]pendingBindingDelete, 0, 1)

	m.mu.Lock()
	binding := m.liveLocalBindingLocked(meta.BindingKey, now, &toCleanup, &pendingDeletes)
	if binding != nil && binding.SessionKey != meta.Key {
		copyBinding := *binding
		m.mu.Unlock()
		m.applyPendingBindingDeletes(pendingDeletes)
		m.cleanupSessions(toCleanup)
		return nil, false, &copyBinding, nil, nil
	}

	sess, created, err := m.getOrCreateLocked(meta, now, &toCleanup, &pendingDeletes)
	if err != nil {
		m.mu.Unlock()
		m.applyPendingBindingDeletes(pendingDeletes)
		m.cleanupSessions(toCleanup)
		return nil, false, nil, nil, err
	}
	if strings.TrimSpace(meta.BindingKey) != "" {
		if strings.TrimSpace(sess.BindingKey) == "" {
			sess.BindingKey = meta.BindingKey
		}
		if binding == nil || binding.SessionKey != sess.Key {
			m.upsertBindingLocked(sess)
		}
	}
	sess.reserveLease()
	m.mu.Unlock()

	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)

	var releaseOnce sync.Once
	return sess, created, nil, func() {
		releaseOnce.Do(func() {
			sess.releaseLease()
		})
	}, nil
}

func (m *Manager) Delete(key string) *ExecutionSession {
	pendingDeletes := make([]pendingBindingDelete, 0, 1)

	m.mu.Lock()
	sess, deletes := m.deleteSessionLocked(key)
	pendingDeletes = append(pendingDeletes, deletes...)
	m.mu.Unlock()

	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions([]*ExecutionSession{sess})
	return sess
}

func (m *Manager) DeleteBinding(bindingKey string) *Binding {
	if strings.TrimSpace(bindingKey) == "" {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	binding := m.bindings[bindingKey]
	if binding == nil {
		return nil
	}
	delete(m.bindings, bindingKey)
	m.removeBindingIndexLocked(binding.SessionKey, bindingKey)
	copyBinding := *binding
	return &copyBinding
}

func (m *Manager) InvalidateBinding(bindingKey string) *Binding {
	if strings.TrimSpace(bindingKey) == "" {
		return nil
	}

	now := time.Now()
	toCleanup := make([]*ExecutionSession, 0, 1)
	pendingDeletes := make([]pendingBindingDelete, 0, 1)
	var (
		invalidated   *Binding
		revocationTTL time.Duration
	)

	m.mu.Lock()
	binding := m.liveLocalBindingLocked(bindingKey, now, &toCleanup, &pendingDeletes)
	if binding != nil {
		copyBinding := *binding
		invalidated = &copyBinding
		revocationTTL = m.revocationTTLForSessionLocked(binding.SessionKey)

		if removed, deletes := m.deleteSessionLocked(binding.SessionKey); removed != nil {
			toCleanup = append(toCleanup, removed)
			pendingDeletes = append(pendingDeletes, deletes...)
		} else {
			if current := m.bindings[bindingKey]; current != nil && current.SessionKey == binding.SessionKey {
				delete(m.bindings, bindingKey)
				m.removeBindingIndexLocked(current.SessionKey, bindingKey)
			}
		}
	}
	m.mu.Unlock()

	if invalidated != nil && strings.TrimSpace(invalidated.SessionKey) != "" {
		m.DeleteBindingAndRevokeIfSessionMatches(invalidated.Key, invalidated.SessionKey, revocationTTL)
	}
	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)
	return invalidated
}

func (m *Manager) DeleteIf(key string, expected *ExecutionSession) *ExecutionSession {
	if key == "" || expected == nil {
		return nil
	}

	m.mu.Lock()
	sess := m.sessions[key]
	if sess != expected {
		m.mu.Unlock()
		return nil
	}
	pendingDeletes := make([]pendingBindingDelete, 0, 1)
	sess, deletes := m.deleteSessionLocked(key)
	pendingDeletes = append(pendingDeletes, deletes...)
	m.mu.Unlock()

	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions([]*ExecutionSession{sess})
	return sess
}

func (m *Manager) Close() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
}

func (m *Manager) ConfigureRedis(client *redis.Client, prefix string) {
	if m == nil {
		return
	}

	m.mu.Lock()
	m.redisClient = client
	m.redisPrefix = normalizeSessionRedisPrefix(prefix)
	m.backend = newRedisBindingBackend(client, prefix)
	m.mu.Unlock()
}

func (m *Manager) Stats() Stats {
	if m == nil {
		return Stats{}
	}

	m.mu.RLock()
	stats := Stats{
		Backend:              "memory",
		LocalSessions:        len(m.sessions),
		LocalBindings:        len(m.bindings),
		MaxSessions:          m.maxSessions,
		MaxSessionsPerCaller: m.maxSessionsPerCaller,
		DefaultTTLSeconds:    int64(m.defaultTTL / time.Second),
	}
	hasRedis := m.backend != nil
	m.mu.RUnlock()

	if hasRedis {
		stats.Backend = "redis"
		stats.BackendBindings = m.backend.CountBindings(context.Background())
	}

	return stats
}

func (m *Manager) Sweep(now time.Time) int {
	if now.IsZero() {
		now = time.Now()
	}

	toCleanup := make([]*ExecutionSession, 0)
	pendingDeletes := make([]pendingBindingDelete, 0)

	m.mu.Lock()
	m.collectExpiredSessionsLocked(now, &toCleanup, &pendingDeletes)
	m.mu.Unlock()

	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)
	return len(toCleanup)
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

func (m *Manager) Resolve(bindingKey string) (*Binding, bool) {
	binding, status := m.ResolveBinding(bindingKey)
	return binding, status == ResolveHit
}

func (m *Manager) ResolveBinding(bindingKey string) (*Binding, ResolveStatus) {
	if strings.TrimSpace(bindingKey) == "" {
		return nil, ResolveMiss
	}

	if m == nil {
		return nil, ResolveMiss
	}

	if m.backend != nil {
		return m.backend.ResolveBinding(context.Background(), bindingKey)
	}

	now := time.Now()
	toCleanup := make([]*ExecutionSession, 0, 1)
	pendingDeletes := make([]pendingBindingDelete, 0, 1)

	m.mu.Lock()
	binding := m.liveLocalBindingLocked(bindingKey, now, &toCleanup, &pendingDeletes)
	if binding == nil {
		m.mu.Unlock()
		m.applyPendingBindingDeletes(pendingDeletes)
		m.cleanupSessions(toCleanup)
		return nil, ResolveMiss
	}
	copyBinding := *binding
	m.mu.Unlock()

	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)
	return &copyBinding, ResolveHit
}

func (m *Manager) ResolveLocal(bindingKey string) (*Binding, bool) {
	if strings.TrimSpace(bindingKey) == "" {
		return nil, false
	}

	now := time.Now()
	toCleanup := make([]*ExecutionSession, 0, 1)
	pendingDeletes := make([]pendingBindingDelete, 0, 1)

	m.mu.Lock()
	binding := m.liveLocalBindingLocked(bindingKey, now, &toCleanup, &pendingDeletes)
	if binding == nil {
		m.mu.Unlock()
		m.applyPendingBindingDeletes(pendingDeletes)
		m.cleanupSessions(toCleanup)
		return nil, false
	}
	copyBinding := *binding
	m.mu.Unlock()

	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)
	return &copyBinding, true
}

func (m *Manager) CheckRevocation(sessionKey string) RevocationStatus {
	if strings.TrimSpace(sessionKey) == "" {
		return RevocationNotRevoked
	}
	if m == nil || m.backend == nil {
		return RevocationNotRevoked
	}
	return m.backend.RevocationStatus(context.Background(), sessionKey)
}

func (m *Manager) CreateBindingIfAbsent(binding *Binding, ttl time.Duration) BindingWriteStatus {
	if binding == nil {
		return BindingWriteBackendError
	}
	if m == nil || m.backend == nil {
		return BindingWriteApplied
	}
	return m.backend.CreateBindingIfAbsent(context.Background(), binding, ttl)
}

func (m *Manager) ReplaceBindingIfSessionMatches(bindingKey, expectedSessionKey string, replacement *Binding, ttl time.Duration) BindingWriteStatus {
	if replacement == nil {
		return BindingWriteBackendError
	}
	if m == nil || m.backend == nil {
		return BindingWriteApplied
	}
	return m.backend.ReplaceBindingIfSessionMatches(context.Background(), bindingKey, expectedSessionKey, replacement, ttl)
}

func (m *Manager) DeleteBindingIfSessionMatches(bindingKey, expectedSessionKey string) BindingWriteStatus {
	if m == nil || m.backend == nil {
		return BindingWriteApplied
	}
	return m.backend.DeleteBindingIfSessionMatches(context.Background(), bindingKey, expectedSessionKey)
}

func (m *Manager) TouchBindingIfSessionMatches(bindingKey, expectedSessionKey string, ttl time.Duration) BindingWriteStatus {
	if m == nil || m.backend == nil {
		return BindingWriteApplied
	}
	return m.backend.TouchBindingIfSessionMatches(context.Background(), bindingKey, expectedSessionKey, ttl)
}

func (m *Manager) DeleteBindingAndRevokeIfSessionMatches(bindingKey, expectedSessionKey string, revokeTTL time.Duration) BindingWriteStatus {
	if m == nil || m.backend == nil {
		return BindingWriteApplied
	}
	return m.backend.DeleteBindingAndRevokeIfSessionMatches(context.Background(), bindingKey, expectedSessionKey, revokeTTL)
}

func (m *Manager) RevocationTTLForSession(sessionKey string) time.Duration {
	if m == nil {
		return minimumSessionRevocationTTL
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.revocationTTLForSessionLocked(sessionKey)
}

func (m *Manager) TouchBinding(bindingKey, sessionKey string, channelID int) {
	if bindingKey == "" || sessionKey == "" {
		return
	}

	var (
		ttl    time.Duration
		status BindingWriteStatus
		exec   *ExecutionSession
	)

	m.mu.Lock()
	binding, ok := m.bindings[bindingKey]
	if !ok || binding == nil || binding.SessionKey != sessionKey {
		m.mu.Unlock()
		return
	}
	if channelID > 0 {
		binding.ChannelID = channelID
	}
	binding.UpdatedAt = time.Now()
	exec = m.sessions[sessionKey]
	if exec != nil {
		exec.SharedStateUncertain = false
	}
	ttl = m.bindingTTLForSessionLocked(sessionKey)
	m.mu.Unlock()

	status = m.TouchBindingIfSessionMatches(bindingKey, sessionKey, ttl)
	if status == BindingWriteApplied {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if current := m.bindings[bindingKey]; current != nil && current.SessionKey == sessionKey {
		switch status {
		case BindingWriteConditionMismatch:
			delete(m.bindings, bindingKey)
			m.removeBindingIndexLocked(sessionKey, bindingKey)
		case BindingWriteBackendError:
			if exec = m.sessions[sessionKey]; exec != nil {
				exec.SharedStateUncertain = true
			}
		}
	}
}

func (m *Manager) upsertBindingLocked(sess *ExecutionSession) {
	if m == nil || sess == nil || strings.TrimSpace(sess.BindingKey) == "" {
		return
	}
	bindingKey := sess.BindingKey
	if existing := m.bindings[bindingKey]; existing != nil && existing.SessionKey != "" && existing.SessionKey != sess.Key {
		m.removeBindingIndexLocked(existing.SessionKey, bindingKey)
	}

	m.bindings[bindingKey] = sess.BuildBinding()
	m.addBindingIndexLocked(sess.Key, bindingKey)
}

func (m *Manager) getOrCreateLocked(meta Metadata, now time.Time, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) (*ExecutionSession, bool, error) {
	if sess := m.sessions[meta.Key]; sess != nil {
		if !m.sessionExpiredLocked(sess, now) {
			return sess, false, nil
		}
		if removed, deletes := m.deleteSessionLocked(meta.Key); removed != nil {
			*toCleanup = append(*toCleanup, removed)
			*pendingDeletes = append(*pendingDeletes, deletes...)
		}
	}

	if err := m.ensureCallerCapacityLocked(capacityNamespaceForMetadata(meta), now, toCleanup, pendingDeletes); err != nil {
		return nil, false, err
	}
	if err := m.ensureCapacityLocked(now, toCleanup, pendingDeletes); err != nil {
		return nil, false, err
	}

	sess := NewExecutionSession(meta)
	m.sessions[meta.Key] = sess
	m.addCapacityIndexLocked(sess)
	m.upsertBindingLocked(sess)
	return sess, true, nil
}

func (m *Manager) liveLocalBindingLocked(bindingKey string, now time.Time, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) *Binding {
	if strings.TrimSpace(bindingKey) == "" {
		return nil
	}

	binding := m.bindings[bindingKey]
	if binding == nil {
		return nil
	}

	sess := m.sessions[binding.SessionKey]
	if sess == nil {
		m.removeBindingIndexLocked(binding.SessionKey, bindingKey)
		delete(m.bindings, bindingKey)
		return nil
	}

	if !m.sessionExpiredLocked(sess, now) {
		return binding
	}

	if removed, deletes := m.deleteSessionLocked(binding.SessionKey); removed != nil {
		*toCleanup = append(*toCleanup, removed)
		*pendingDeletes = append(*pendingDeletes, deletes...)
	}
	return nil
}

func (m *Manager) sessionExpiredLocked(sess *ExecutionSession, now time.Time) bool {
	if sess == nil {
		return true
	}
	if m.CheckRevocation(sess.Key) == RevocationRevoked {
		return true
	}

	if !sess.TryLock() {
		return false
	}
	expired := sess.IsExpired(now, m.defaultTTL)
	sess.Unlock()
	return expired
}

func (m *Manager) collectExpiredSessionsLocked(now time.Time, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) {
	for key, sess := range m.sessions {
		if !m.sessionExpiredLocked(sess, now) {
			continue
		}
		if removed, deletes := m.deleteSessionLocked(key); removed != nil {
			*toCleanup = append(*toCleanup, removed)
			*pendingDeletes = append(*pendingDeletes, deletes...)
		}
	}
}

func (m *Manager) ensureCapacityLocked(now time.Time, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) error {
	if m == nil || m.maxSessions <= 0 {
		return nil
	}
	if len(m.sessions) < m.maxSessions {
		return nil
	}

	m.collectExpiredSessionsLocked(now, toCleanup, pendingDeletes)
	if len(m.sessions) < m.maxSessions {
		return nil
	}
	return ErrCapacityExceeded
}

func (m *Manager) ensureCallerCapacityLocked(capacityNS string, now time.Time, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) error {
	if m == nil || m.maxSessionsPerCaller <= 0 || strings.TrimSpace(capacityNS) == "" {
		return nil
	}
	if len(m.capacityIndex[capacityNS]) < m.maxSessionsPerCaller {
		return nil
	}

	m.collectExpiredSessionsLocked(now, toCleanup, pendingDeletes)
	if len(m.capacityIndex[capacityNS]) < m.maxSessionsPerCaller {
		return nil
	}
	return ErrCallerCapacityExceeded
}

func (m *Manager) deleteSessionLocked(sessionKey string) (*ExecutionSession, []pendingBindingDelete) {
	if sessionKey == "" {
		return nil, nil
	}

	sess := m.sessions[sessionKey]
	delete(m.sessions, sessionKey)
	if sess != nil {
		m.removeCapacityIndexLocked(capacityNamespaceForSession(sess), sessionKey)
	}
	return sess, m.deleteBindingsForSessionLocked(sessionKey)
}

func (m *Manager) deleteBindingsForSessionLocked(sessionKey string) []pendingBindingDelete {
	if sessionKey == "" {
		return nil
	}
	bindingKeys := m.index[sessionKey]
	delete(m.index, sessionKey)
	pendingDeletes := make([]pendingBindingDelete, 0, len(bindingKeys))
	for bindingKey := range bindingKeys {
		if binding := m.bindings[bindingKey]; binding != nil && binding.SessionKey == sessionKey {
			delete(m.bindings, bindingKey)
			pendingDeletes = append(pendingDeletes, pendingBindingDelete{
				bindingKey: bindingKey,
				sessionKey: sessionKey,
			})
		}
	}
	return pendingDeletes
}

func (m *Manager) addBindingIndexLocked(sessionKey, bindingKey string) {
	if sessionKey == "" || bindingKey == "" {
		return
	}
	bindingKeys := m.index[sessionKey]
	if bindingKeys == nil {
		bindingKeys = make(map[string]struct{})
		m.index[sessionKey] = bindingKeys
	}
	bindingKeys[bindingKey] = struct{}{}
}

func (m *Manager) removeBindingIndexLocked(sessionKey, bindingKey string) {
	if sessionKey == "" || bindingKey == "" {
		return
	}
	bindingKeys := m.index[sessionKey]
	if len(bindingKeys) == 0 {
		delete(m.index, sessionKey)
		return
	}
	delete(bindingKeys, bindingKey)
	if len(bindingKeys) == 0 {
		delete(m.index, sessionKey)
	}
}

func (m *Manager) addCapacityIndexLocked(sess *ExecutionSession) {
	capacityNS := capacityNamespaceForSession(sess)
	if m == nil || sess == nil || strings.TrimSpace(capacityNS) == "" || sess.Key == "" {
		return
	}
	sessionKeys := m.capacityIndex[capacityNS]
	if sessionKeys == nil {
		sessionKeys = make(map[string]struct{})
		m.capacityIndex[capacityNS] = sessionKeys
	}
	sessionKeys[sess.Key] = struct{}{}
}

func (m *Manager) removeCapacityIndexLocked(capacityNS, sessionKey string) {
	if m == nil || strings.TrimSpace(capacityNS) == "" || sessionKey == "" {
		return
	}
	sessionKeys := m.capacityIndex[capacityNS]
	if len(sessionKeys) == 0 {
		delete(m.capacityIndex, capacityNS)
		return
	}
	delete(sessionKeys, sessionKey)
	if len(sessionKeys) == 0 {
		delete(m.capacityIndex, capacityNS)
	}
}

func capacityNamespaceForMetadata(meta Metadata) string {
	if trimmed := strings.TrimSpace(meta.CapacityNS); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(meta.CallerNS)
}

func capacityNamespaceForSession(sess *ExecutionSession) string {
	if sess == nil {
		return ""
	}
	if trimmed := strings.TrimSpace(sess.CapacityNS); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(sess.CallerNS)
}

func (m *Manager) applyPendingBindingDeletes(pendingDeletes []pendingBindingDelete) {
	if m == nil || len(pendingDeletes) == 0 {
		return
	}
	for _, pending := range pendingDeletes {
		if strings.TrimSpace(pending.bindingKey) == "" || strings.TrimSpace(pending.sessionKey) == "" {
			continue
		}
		m.DeleteBindingIfSessionMatches(pending.bindingKey, pending.sessionKey)
	}
}

func (m *Manager) cleanupSessions(sessions []*ExecutionSession) {
	if m.cleanup == nil {
		return
	}

	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		m.cleanup(sess)
	}
}
