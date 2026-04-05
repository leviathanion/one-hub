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
var errRestartDecision = errors.New("execution session restart decision")

const defaultExecutionSessionRevocationTimeout = 200 * time.Millisecond
const finalLatestTruthReobserveBudget = 2

type ManagerOptions struct {
	DefaultTTL           time.Duration
	JanitorInterval      time.Duration
	Cleanup              CleanupFunc
	MaxSessions          int
	MaxSessionsPerCaller int
	RedisClient          *redis.Client
	RedisPrefix          string
	RevocationTimeout    time.Duration
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
	remoteMu      sync.RWMutex

	defaultTTL           time.Duration
	janitorInterval      time.Duration
	maxSessions          int
	maxSessionsPerCaller int
	cleanup              CleanupFunc
	remoteConfig         managerRemoteConfig
	stopCh               chan struct{}
	stopOnce             sync.Once
}

type pendingBindingDelete struct {
	bindingKey string
	sessionKey string
}

type managerRemoteConfig struct {
	backend           bindingBackend
	revocationTimeout time.Duration
}

type revocationCandidate struct {
	sessionKey string
	session    *ExecutionSession
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
		remoteConfig: managerRemoteConfig{
			backend:           newRedisBindingBackend(options.RedisClient, options.RedisPrefix),
			revocationTimeout: normalizeRevocationTimeout(options.RevocationTimeout),
		},
		stopCh: make(chan struct{}),
	}

	if options.JanitorInterval > 0 {
		go m.runJanitor()
	}

	return m
}

func normalizeRevocationTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultExecutionSessionRevocationTimeout
	}
	return timeout
}

func (m *Manager) remoteConfigSnapshot() managerRemoteConfig {
	if m == nil {
		return managerRemoteConfig{revocationTimeout: defaultExecutionSessionRevocationTimeout}
	}
	m.remoteMu.RLock()
	defer m.remoteMu.RUnlock()
	return m.remoteConfig
}

func (m *Manager) configureRemote(client *redis.Client, prefix string, revocationTimeout time.Duration) {
	if m == nil {
		return
	}
	m.remoteMu.Lock()
	m.remoteConfig = managerRemoteConfig{
		backend:           newRedisBindingBackend(client, prefix),
		revocationTimeout: normalizeRevocationTimeout(revocationTimeout),
	}
	m.remoteMu.Unlock()
}

func (m *Manager) revocationContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), normalizeRevocationTimeout(timeout))
}

func (m *Manager) checkRevocationWithConfig(config managerRemoteConfig, sessionKey string) RevocationStatus {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return RevocationNotRevoked
	}
	if m == nil || config.backend == nil {
		return RevocationNotRevoked
	}
	ctx, cancel := m.revocationContext(config.revocationTimeout)
	defer cancel()
	return config.backend.RevocationStatus(ctx, sessionKey)
}

func (m *Manager) revocationStatusesWithConfig(config managerRemoteConfig, sessionKeys []string) []RevocationStatus {
	statuses := make([]RevocationStatus, len(sessionKeys))
	if len(sessionKeys) == 0 {
		return statuses
	}
	if m == nil || config.backend == nil {
		for i := range statuses {
			statuses[i] = RevocationNotRevoked
		}
		return statuses
	}

	if bulk, ok := config.backend.(bulkRevocationBackend); ok {
		ctx, cancel := m.revocationContext(config.revocationTimeout)
		result, err := bulk.RevocationStatuses(ctx, sessionKeys)
		cancel()
		if err == nil && len(result) == len(sessionKeys) {
			return result
		}
		for i := range statuses {
			statuses[i] = RevocationUnknown
		}
		return statuses
	}

	for i, sessionKey := range sessionKeys {
		statuses[i] = m.checkRevocationWithConfig(config, sessionKey)
	}
	return statuses
}

func (m *Manager) GetOrCreate(meta Metadata) (*ExecutionSession, bool, error) {
	sess, created, _, err := m.getOrCreateInternal(meta, false)
	return sess, created, err
}

func (m *Manager) AcquireOrCreate(meta Metadata) (*ExecutionSession, bool, func(), error) {
	return m.getOrCreateInternal(meta, true)
}

func (m *Manager) AcquireExisting(sessionKey string) (*ExecutionSession, func(), bool) {
	if strings.TrimSpace(sessionKey) == "" {
		return nil, nil, false
	}

	config := m.remoteConfigSnapshot()
	now := time.Now()
	toCleanup := make([]*ExecutionSession, 0, 1)
	pendingDeletes := make([]pendingBindingDelete, 0, 1)

	m.mu.Lock()
	sess := m.liveLocalSessionLocked(sessionKey, now, &toCleanup, &pendingDeletes)
	if sess == nil {
		m.mu.Unlock()
		m.applyPendingBindingDeletes(pendingDeletes)
		m.cleanupSessions(toCleanup)
		return nil, nil, false
	}
	if config.backend == nil {
		sess.reserveLease()
		m.mu.Unlock()
		return sess, newLeaseRelease(sess), true
	}
	expected := sess
	m.mu.Unlock()

	status := m.checkRevocationWithConfig(config, sessionKey)

	m.mu.Lock()
	sess = m.sessions[sessionKey]
	if sess != expected {
		m.mu.Unlock()
		m.applyPendingBindingDeletes(pendingDeletes)
		m.cleanupSessions(toCleanup)
		return nil, nil, false
	}
	if status == RevocationRevoked || m.sessionLocallyExpiredLocked(sess, time.Now()) {
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

	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)
	return sess, newLeaseRelease(sess), true
}

func (m *Manager) GetOrCreateBound(meta Metadata) (*ExecutionSession, bool, *Binding, error) {
	sess, created, conflict, _, err := m.getOrCreateBoundInternal(meta, false)
	return sess, created, conflict, err
}

func (m *Manager) AcquireOrCreateBound(meta Metadata) (*ExecutionSession, bool, *Binding, func(), error) {
	return m.getOrCreateBoundInternal(meta, true)
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

func (m *Manager) ConfigureRemote(client *redis.Client, prefix string, revocationTimeout time.Duration) {
	m.configureRemote(client, prefix, revocationTimeout)
}

func (m *Manager) ConfigureRedis(client *redis.Client, prefix string) {
	if m == nil {
		return
	}
	config := m.remoteConfigSnapshot()
	m.configureRemote(client, prefix, config.revocationTimeout)
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
	m.mu.RUnlock()

	config := m.remoteConfigSnapshot()
	if config.backend != nil {
		stats.Backend = "redis"
		stats.BackendBindings = config.backend.CountBindings(context.Background())
	}

	return stats
}

func (m *Manager) Sweep(now time.Time) int {
	if now.IsZero() {
		now = time.Now()
	}

	toCleanup := make([]*ExecutionSession, 0)
	pendingDeletes := make([]pendingBindingDelete, 0)
	m.collectExpiredSessions(now, m.remoteConfigSnapshot(), &toCleanup, &pendingDeletes)

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

	config := m.remoteConfigSnapshot()
	if config.backend != nil {
		return config.backend.ResolveBinding(context.Background(), bindingKey)
	}
	binding, pendingDeletes, toCleanup := m.resolveLiveLocalBinding(bindingKey, config)
	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)
	if binding == nil {
		return nil, ResolveMiss
	}
	return binding, ResolveHit
}

func (m *Manager) ResolveLocal(bindingKey string) (*Binding, bool) {
	if strings.TrimSpace(bindingKey) == "" {
		return nil, false
	}
	binding, pendingDeletes, toCleanup := m.resolveLiveLocalBinding(bindingKey, m.remoteConfigSnapshot())
	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)
	return binding, binding != nil
}

func (m *Manager) CheckRevocation(sessionKey string) RevocationStatus {
	return m.checkRevocationWithConfig(m.remoteConfigSnapshot(), sessionKey)
}

func (m *Manager) CreateBindingIfAbsent(binding *Binding, ttl time.Duration) BindingWriteStatus {
	if binding == nil {
		return BindingWriteBackendError
	}
	config := m.remoteConfigSnapshot()
	if m == nil || config.backend == nil {
		return BindingWriteApplied
	}
	return config.backend.CreateBindingIfAbsent(context.Background(), binding, ttl)
}

func (m *Manager) ReplaceBindingIfSessionMatches(bindingKey, expectedSessionKey string, replacement *Binding, ttl time.Duration) BindingWriteStatus {
	if replacement == nil {
		return BindingWriteBackendError
	}
	config := m.remoteConfigSnapshot()
	if m == nil || config.backend == nil {
		return BindingWriteApplied
	}
	return config.backend.ReplaceBindingIfSessionMatches(context.Background(), bindingKey, expectedSessionKey, replacement, ttl)
}

func (m *Manager) DeleteBindingIfSessionMatches(bindingKey, expectedSessionKey string) BindingWriteStatus {
	config := m.remoteConfigSnapshot()
	if m == nil || config.backend == nil {
		return BindingWriteApplied
	}
	return config.backend.DeleteBindingIfSessionMatches(context.Background(), bindingKey, expectedSessionKey)
}

func (m *Manager) TouchBindingIfSessionMatches(bindingKey, expectedSessionKey string, ttl time.Duration) BindingWriteStatus {
	config := m.remoteConfigSnapshot()
	if m == nil || config.backend == nil {
		return BindingWriteApplied
	}
	return config.backend.TouchBindingIfSessionMatches(context.Background(), bindingKey, expectedSessionKey, ttl)
}

func (m *Manager) DeleteBindingAndRevokeIfSessionMatches(bindingKey, expectedSessionKey string, revokeTTL time.Duration) BindingWriteStatus {
	config := m.remoteConfigSnapshot()
	if m == nil || config.backend == nil {
		return BindingWriteApplied
	}
	return config.backend.DeleteBindingAndRevokeIfSessionMatches(context.Background(), bindingKey, expectedSessionKey, revokeTTL)
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

func (m *Manager) liveLocalBindingLocked(bindingKey string, now time.Time, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) *Binding {
	if strings.TrimSpace(bindingKey) == "" {
		return nil
	}

	binding := m.bindings[bindingKey]
	if binding == nil {
		return nil
	}

	sess := m.liveLocalSessionLocked(binding.SessionKey, now, toCleanup, pendingDeletes)
	if sess == nil {
		if current := m.bindings[bindingKey]; current != nil && current.SessionKey == binding.SessionKey {
			m.removeBindingIndexLocked(binding.SessionKey, bindingKey)
			delete(m.bindings, bindingKey)
		}
		return nil
	}
	return binding
}

func (m *Manager) liveLocalSessionLocked(sessionKey string, now time.Time, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) *ExecutionSession {
	if strings.TrimSpace(sessionKey) == "" {
		return nil
	}
	sess := m.sessions[sessionKey]
	if sess == nil {
		return nil
	}
	if !m.sessionLocallyExpiredLocked(sess, now) {
		return sess
	}
	m.appendDeletedSessionLocked(sessionKey, toCleanup, pendingDeletes)
	return nil
}

func (m *Manager) sessionLocallyExpiredLocked(sess *ExecutionSession, now time.Time) bool {
	if sess == nil {
		return true
	}
	if !sess.TryLock() {
		return false
	}
	expired := sess.IsExpired(now, m.defaultTTL)
	sess.Unlock()
	return expired
}

func (m *Manager) collectExpiredSessionsLocked(now time.Time, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) []revocationCandidate {
	candidates := make([]revocationCandidate, 0, len(m.sessions))
	for key, sess := range m.sessions {
		if !m.sessionLocallyExpiredLocked(sess, now) {
			candidates = append(candidates, revocationCandidate{sessionKey: key, session: sess})
			continue
		}
		m.appendDeletedSessionLocked(key, toCleanup, pendingDeletes)
	}
	return candidates
}

func (m *Manager) collectExpiredSessions(now time.Time, config managerRemoteConfig, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) {
	if m == nil {
		return
	}

	m.mu.Lock()
	candidates := m.collectExpiredSessionsLocked(now, toCleanup, pendingDeletes)
	m.mu.Unlock()

	if config.backend == nil || len(candidates) == 0 {
		return
	}

	sessionKeys := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		sessionKeys = append(sessionKeys, candidate.sessionKey)
	}
	statuses := m.revocationStatusesWithConfig(config, sessionKeys)

	m.mu.Lock()
	defer m.mu.Unlock()

	for i, candidate := range candidates {
		if i >= len(statuses) {
			break
		}
		current := m.sessions[candidate.sessionKey]
		if current != candidate.session {
			continue
		}
		if statuses[i] != RevocationRevoked && !m.sessionLocallyExpiredLocked(current, now) {
			continue
		}
		m.appendDeletedSessionLocked(candidate.sessionKey, toCleanup, pendingDeletes)
	}
}

func (m *Manager) appendDeletedSessionLocked(sessionKey string, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) *ExecutionSession {
	removed, deletes := m.deleteSessionLocked(sessionKey)
	if removed != nil {
		*toCleanup = append(*toCleanup, removed)
		*pendingDeletes = append(*pendingDeletes, deletes...)
	}
	return removed
}

func newLeaseRelease(sess *ExecutionSession) func() {
	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			sess.releaseLease()
		})
	}
}

func observeBudgetForAttempt(allowRestart bool) int {
	if allowRestart {
		return 1
	}
	return finalLatestTruthReobserveBudget
}

func bindingObservationSessionKey(binding *Binding) string {
	if binding == nil {
		return ""
	}
	return strings.TrimSpace(binding.SessionKey)
}

func bindingMatchesObservedSessionKey(current *Binding, observedSessionKey string) bool {
	observedSessionKey = strings.TrimSpace(observedSessionKey)
	if observedSessionKey == "" {
		return current == nil
	}
	return current != nil && current.SessionKey == observedSessionKey
}

func (m *Manager) observeReusableSession(sessionKey string, config managerRemoteConfig, reserveLease bool, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) (*ExecutionSession, bool) {
	now := time.Now()
	m.mu.Lock()
	sess := m.liveLocalSessionLocked(sessionKey, now, toCleanup, pendingDeletes)
	if sess == nil {
		m.mu.Unlock()
		return nil, false
	}
	if config.backend == nil {
		if reserveLease {
			sess.reserveLease()
		}
		m.mu.Unlock()
		return sess, false
	}

	expected := sess
	m.mu.Unlock()
	status := m.checkRevocationWithConfig(config, sessionKey)

	m.mu.Lock()
	current := m.sessions[sessionKey]
	if current != expected {
		m.mu.Unlock()
		return nil, true
	}
	if status == RevocationRevoked || m.sessionLocallyExpiredLocked(current, time.Now()) {
		m.appendDeletedSessionLocked(sessionKey, toCleanup, pendingDeletes)
		m.mu.Unlock()
		return nil, false
	}
	if reserveLease {
		// Reserve under m.mu so sweep cannot delete the exact object we are about
		// to return before the lease becomes visible.
		current.reserveLease()
	}
	m.mu.Unlock()
	return current, false
}

func (m *Manager) observeReusableSessionLatestTruth(sessionKey string, config managerRemoteConfig, reserveLease bool, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) *ExecutionSession {
	for observe := 0; observe < finalLatestTruthReobserveBudget; observe++ {
		sess, changed := m.observeReusableSession(sessionKey, config, reserveLease, toCleanup, pendingDeletes)
		if !changed {
			return sess
		}
	}
	return nil
}

func (m *Manager) observeLiveLocalBinding(bindingKey string, config managerRemoteConfig, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) (*Binding, bool) {
	if strings.TrimSpace(bindingKey) == "" {
		return nil, false
	}

	now := time.Now()
	m.mu.Lock()
	binding := m.liveLocalBindingLocked(bindingKey, now, toCleanup, pendingDeletes)
	if binding == nil {
		m.mu.Unlock()
		return nil, false
	}
	if config.backend == nil {
		copyBinding := *binding
		m.mu.Unlock()
		return &copyBinding, false
	}

	expectedSessionKey := binding.SessionKey
	expectedSession := m.sessions[expectedSessionKey]
	m.mu.Unlock()

	status := m.checkRevocationWithConfig(config, expectedSessionKey)

	m.mu.Lock()
	currentBinding := m.bindings[bindingKey]
	if currentBinding == nil || currentBinding.SessionKey != expectedSessionKey || m.sessions[expectedSessionKey] != expectedSession {
		m.mu.Unlock()
		return nil, true
	}
	if status == RevocationRevoked || m.sessionLocallyExpiredLocked(expectedSession, time.Now()) {
		m.appendDeletedSessionLocked(expectedSessionKey, toCleanup, pendingDeletes)
		m.mu.Unlock()
		return nil, false
	}
	copyBinding := *currentBinding
	m.mu.Unlock()
	return &copyBinding, false
}

func (m *Manager) observeLiveLocalBindingLatestTruth(bindingKey string, config managerRemoteConfig, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) *Binding {
	for observe := 0; observe < finalLatestTruthReobserveBudget; observe++ {
		binding, changed := m.observeLiveLocalBinding(bindingKey, config, toCleanup, pendingDeletes)
		if !changed {
			return binding
		}
	}
	return nil
}

func (m *Manager) getOrCreateInternal(meta Metadata, reserveLease bool) (*ExecutionSession, bool, func(), error) {
	config := m.remoteConfigSnapshot()
	toCleanup := make([]*ExecutionSession, 0, 1)
	pendingDeletes := make([]pendingBindingDelete, 0, 1)

	var (
		sess    *ExecutionSession
		created bool
		err     error
	)

	for attempt := 0; attempt < 2; attempt++ {
		sess, created, err = m.getOrCreateAttempt(meta, config, reserveLease, attempt == 0, &toCleanup, &pendingDeletes)
		if err != errRestartDecision {
			break
		}
	}

	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)
	if err != nil {
		return nil, false, nil, err
	}
	if !reserveLease {
		return sess, created, nil, nil
	}
	return sess, created, newLeaseRelease(sess), nil
}

func (m *Manager) getOrCreateAttempt(meta Metadata, config managerRemoteConfig, reserveLease, allowRestart bool, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) (*ExecutionSession, bool, error) {
	capacityNS := capacityNamespaceForMetadata(meta)
	for observe := 0; observe < observeBudgetForAttempt(allowRestart); observe++ {
		if sess, changed := m.observeReusableSession(meta.Key, config, reserveLease, toCleanup, pendingDeletes); changed {
			if allowRestart {
				return nil, false, errRestartDecision
			}
			continue
		} else if sess != nil {
			return sess, false, nil
		}

		m.mu.Lock()
		needsCollect := (m.maxSessions > 0 && len(m.sessions) >= m.maxSessions) ||
			(m.maxSessionsPerCaller > 0 && strings.TrimSpace(capacityNS) != "" && len(m.capacityIndex[capacityNS]) >= m.maxSessionsPerCaller)
		m.mu.Unlock()

		if needsCollect {
			// Phase 1 removes locally expired sessions under m.mu; phase 2 checks
			// revocation outside the lock. Remote binding deletes still happen after
			// unlock, so collect shortens the critical section without pretending the
			// whole cleanup path is constant-time wall clock.
			m.collectExpiredSessions(time.Now(), config, toCleanup, pendingDeletes)
		}

		m.mu.Lock()
		current := m.liveLocalSessionLocked(meta.Key, time.Now(), toCleanup, pendingDeletes)
		if current != nil {
			if config.backend != nil {
				m.mu.Unlock()
				if allowRestart {
					return nil, false, errRestartDecision
				}
				continue
			}
			if reserveLease {
				current.reserveLease()
			}
			m.mu.Unlock()
			return current, false, nil
		}
		m.mu.Unlock()

		m.mu.Lock()
		if m.maxSessionsPerCaller > 0 && strings.TrimSpace(capacityNS) != "" && len(m.capacityIndex[capacityNS]) >= m.maxSessionsPerCaller {
			m.mu.Unlock()
			return nil, false, ErrCallerCapacityExceeded
		}
		if m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
			m.mu.Unlock()
			return nil, false, ErrCapacityExceeded
		}
		sess := NewExecutionSession(meta)
		m.sessions[meta.Key] = sess
		m.addCapacityIndexLocked(sess)
		m.upsertBindingLocked(sess)
		if reserveLease {
			sess.reserveLease()
		}
		m.mu.Unlock()
		return sess, true, nil
	}

	if config.backend != nil {
		if sess := m.observeReusableSessionLatestTruth(meta.Key, config, reserveLease, toCleanup, pendingDeletes); sess != nil {
			return sess, false, nil
		}
	}

	m.mu.Lock()
	sess := m.liveLocalSessionLocked(meta.Key, time.Now(), toCleanup, pendingDeletes)
	if sess != nil {
		if config.backend != nil {
			m.mu.Unlock()
			if latest := m.observeReusableSessionLatestTruth(meta.Key, config, reserveLease, toCleanup, pendingDeletes); latest != nil {
				return latest, false, nil
			}
			m.mu.Lock()
			sess = m.liveLocalSessionLocked(meta.Key, time.Now(), toCleanup, pendingDeletes)
		}
	}
	if sess != nil {
		if reserveLease {
			sess.reserveLease()
		}
		m.mu.Unlock()
		return sess, false, nil
	}
	if m.maxSessionsPerCaller > 0 && strings.TrimSpace(capacityNS) != "" && len(m.capacityIndex[capacityNS]) >= m.maxSessionsPerCaller {
		m.mu.Unlock()
		return nil, false, ErrCallerCapacityExceeded
	}
	if m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
		m.mu.Unlock()
		return nil, false, ErrCapacityExceeded
	}
	sess = NewExecutionSession(meta)
	m.sessions[meta.Key] = sess
	m.addCapacityIndexLocked(sess)
	m.upsertBindingLocked(sess)
	if reserveLease {
		sess.reserveLease()
	}
	m.mu.Unlock()
	return sess, true, nil
}

func (m *Manager) getOrCreateBoundInternal(meta Metadata, reserveLease bool) (*ExecutionSession, bool, *Binding, func(), error) {
	config := m.remoteConfigSnapshot()
	toCleanup := make([]*ExecutionSession, 0, 1)
	pendingDeletes := make([]pendingBindingDelete, 0, 1)

	var (
		sess     *ExecutionSession
		created  bool
		conflict *Binding
		err      error
	)

	for attempt := 0; attempt < 2; attempt++ {
		sess, created, conflict, err = m.getOrCreateBoundAttempt(meta, config, reserveLease, attempt == 0, &toCleanup, &pendingDeletes)
		if err != errRestartDecision {
			break
		}
	}

	m.applyPendingBindingDeletes(pendingDeletes)
	m.cleanupSessions(toCleanup)
	if err != nil {
		return nil, false, nil, nil, err
	}
	if conflict != nil || !reserveLease {
		return sess, created, conflict, nil, nil
	}
	return sess, created, nil, newLeaseRelease(sess), nil
}

func (m *Manager) getOrCreateBoundAttempt(meta Metadata, config managerRemoteConfig, reserveLease, allowRestart bool, toCleanup *[]*ExecutionSession, pendingDeletes *[]pendingBindingDelete) (*ExecutionSession, bool, *Binding, error) {
	capacityNS := capacityNamespaceForMetadata(meta)
	hasBinding := strings.TrimSpace(meta.BindingKey) != ""

	for observe := 0; observe < observeBudgetForAttempt(allowRestart); observe++ {
		binding, bindingChanged := m.observeLiveLocalBinding(meta.BindingKey, config, toCleanup, pendingDeletes)
		if bindingChanged {
			if allowRestart {
				return nil, false, nil, errRestartDecision
			}
			continue
		}
		if binding != nil && binding.SessionKey != meta.Key {
			return nil, false, binding, nil
		}
		observedBindingSessionKey := bindingObservationSessionKey(binding)

		sess, changed := m.observeReusableSession(meta.Key, config, false, toCleanup, pendingDeletes)
		if changed {
			if allowRestart {
				return nil, false, nil, errRestartDecision
			}
			continue
		}
		if sess == nil {
			m.mu.Lock()
			needsCollect := (m.maxSessions > 0 && len(m.sessions) >= m.maxSessions) ||
				(m.maxSessionsPerCaller > 0 && strings.TrimSpace(capacityNS) != "" && len(m.capacityIndex[capacityNS]) >= m.maxSessionsPerCaller)
			m.mu.Unlock()

			if needsCollect {
				m.collectExpiredSessions(time.Now(), config, toCleanup, pendingDeletes)
			}

			m.mu.Lock()
			current := m.liveLocalSessionLocked(meta.Key, time.Now(), toCleanup, pendingDeletes)
			m.mu.Unlock()
			if current != nil {
				if config.backend != nil {
					if allowRestart {
						return nil, false, nil, errRestartDecision
					}
					continue
				}
				sess = current
			}
		}

		if sess == nil {
			m.mu.Lock()
			currentBinding := m.liveLocalBindingLocked(meta.BindingKey, time.Now(), toCleanup, pendingDeletes)
			if config.backend != nil && !bindingMatchesObservedSessionKey(currentBinding, observedBindingSessionKey) {
				m.mu.Unlock()
				if allowRestart {
					return nil, false, nil, errRestartDecision
				}
				continue
			}
			if currentBinding != nil && currentBinding.SessionKey != meta.Key {
				copyBinding := *currentBinding
				m.mu.Unlock()
				if allowRestart {
					return nil, false, nil, errRestartDecision
				}
				return nil, false, &copyBinding, nil
			}
			if m.maxSessionsPerCaller > 0 && strings.TrimSpace(capacityNS) != "" && len(m.capacityIndex[capacityNS]) >= m.maxSessionsPerCaller {
				m.mu.Unlock()
				return nil, false, nil, ErrCallerCapacityExceeded
			}
			if m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
				m.mu.Unlock()
				return nil, false, nil, ErrCapacityExceeded
			}

			sess = NewExecutionSession(meta)
			m.sessions[meta.Key] = sess
			m.addCapacityIndexLocked(sess)
			m.upsertBindingLocked(sess)
			if reserveLease {
				sess.reserveLease()
			}
			m.mu.Unlock()
			return sess, true, nil, nil
		}

		m.mu.Lock()
		currentBinding := m.liveLocalBindingLocked(meta.BindingKey, time.Now(), toCleanup, pendingDeletes)
		if config.backend != nil && !bindingMatchesObservedSessionKey(currentBinding, observedBindingSessionKey) {
			m.mu.Unlock()
			if allowRestart {
				return nil, false, nil, errRestartDecision
			}
			continue
		}
		if currentBinding != nil && currentBinding.SessionKey != meta.Key {
			copyBinding := *currentBinding
			m.mu.Unlock()
			if allowRestart {
				return nil, false, nil, errRestartDecision
			}
			return nil, false, &copyBinding, nil
		}
		if m.sessions[meta.Key] != sess {
			m.mu.Unlock()
			if allowRestart {
				return nil, false, nil, errRestartDecision
			}
			continue
		}
		if hasBinding {
			if strings.TrimSpace(sess.BindingKey) == "" {
				sess.BindingKey = meta.BindingKey
			}
			if currentBinding == nil || currentBinding.SessionKey != sess.Key {
				m.upsertBindingLocked(sess)
			}
		}
		if reserveLease {
			sess.reserveLease()
		}
		m.mu.Unlock()
		return sess, false, nil, nil
	}

	for finalObserve := 0; finalObserve < observeBudgetForAttempt(false); finalObserve++ {
		observedBindingSessionKey := ""
		if config.backend != nil {
			binding := m.observeLiveLocalBindingLatestTruth(meta.BindingKey, config, toCleanup, pendingDeletes)
			if binding != nil && binding.SessionKey != meta.Key {
				return nil, false, binding, nil
			}
			observedBindingSessionKey = bindingObservationSessionKey(binding)
		}

		m.mu.Lock()
		currentBinding := m.liveLocalBindingLocked(meta.BindingKey, time.Now(), toCleanup, pendingDeletes)
		if config.backend != nil && !bindingMatchesObservedSessionKey(currentBinding, observedBindingSessionKey) {
			m.mu.Unlock()
			continue
		}
		if currentBinding != nil && currentBinding.SessionKey != meta.Key {
			copyBinding := *currentBinding
			m.mu.Unlock()
			return nil, false, &copyBinding, nil
		}
		sess := m.liveLocalSessionLocked(meta.Key, time.Now(), toCleanup, pendingDeletes)
		if config.backend != nil {
			m.mu.Unlock()
			latest := m.observeReusableSessionLatestTruth(meta.Key, config, false, toCleanup, pendingDeletes)
			m.mu.Lock()
			currentBinding = m.liveLocalBindingLocked(meta.BindingKey, time.Now(), toCleanup, pendingDeletes)
			if !bindingMatchesObservedSessionKey(currentBinding, observedBindingSessionKey) {
				m.mu.Unlock()
				continue
			}
			if currentBinding != nil && currentBinding.SessionKey != meta.Key {
				copyBinding := *currentBinding
				m.mu.Unlock()
				return nil, false, &copyBinding, nil
			}
			if latest != nil && m.sessions[meta.Key] == latest {
				sess = latest
			} else {
				sess = m.liveLocalSessionLocked(meta.Key, time.Now(), toCleanup, pendingDeletes)
			}
		}
		if sess == nil {
			needsCollect := (m.maxSessions > 0 && len(m.sessions) >= m.maxSessions) ||
				(m.maxSessionsPerCaller > 0 && strings.TrimSpace(capacityNS) != "" && len(m.capacityIndex[capacityNS]) >= m.maxSessionsPerCaller)
			m.mu.Unlock()

			if needsCollect {
				m.collectExpiredSessions(time.Now(), config, toCleanup, pendingDeletes)
			}

			m.mu.Lock()
			sess = m.liveLocalSessionLocked(meta.Key, time.Now(), toCleanup, pendingDeletes)
			currentBinding = m.liveLocalBindingLocked(meta.BindingKey, time.Now(), toCleanup, pendingDeletes)
			if config.backend != nil && !bindingMatchesObservedSessionKey(currentBinding, observedBindingSessionKey) {
				m.mu.Unlock()
				continue
			}
			if currentBinding != nil && currentBinding.SessionKey != meta.Key {
				copyBinding := *currentBinding
				m.mu.Unlock()
				return nil, false, &copyBinding, nil
			}
			if sess != nil {
				if hasBinding && strings.TrimSpace(sess.BindingKey) == "" {
					sess.BindingKey = meta.BindingKey
					m.upsertBindingLocked(sess)
				}
				if reserveLease {
					sess.reserveLease()
				}
				m.mu.Unlock()
				return sess, false, nil, nil
			}
			if m.maxSessionsPerCaller > 0 && strings.TrimSpace(capacityNS) != "" && len(m.capacityIndex[capacityNS]) >= m.maxSessionsPerCaller {
				m.mu.Unlock()
				return nil, false, nil, ErrCallerCapacityExceeded
			}
			if m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
				m.mu.Unlock()
				return nil, false, nil, ErrCapacityExceeded
			}

			sess = NewExecutionSession(meta)
			m.sessions[meta.Key] = sess
			m.addCapacityIndexLocked(sess)
			m.upsertBindingLocked(sess)
			if reserveLease {
				sess.reserveLease()
			}
			m.mu.Unlock()
			return sess, true, nil, nil
		}

		if hasBinding {
			if strings.TrimSpace(sess.BindingKey) == "" {
				sess.BindingKey = meta.BindingKey
			}
			if currentBinding == nil || currentBinding.SessionKey != sess.Key {
				m.upsertBindingLocked(sess)
			}
		}
		if reserveLease {
			sess.reserveLease()
		}
		m.mu.Unlock()
		return sess, false, nil, nil
	}

	m.mu.Lock()
	currentBinding := m.liveLocalBindingLocked(meta.BindingKey, time.Now(), toCleanup, pendingDeletes)
	if currentBinding != nil && currentBinding.SessionKey != meta.Key {
		copyBinding := *currentBinding
		m.mu.Unlock()
		return nil, false, &copyBinding, nil
	}
	sess := m.liveLocalSessionLocked(meta.Key, time.Now(), toCleanup, pendingDeletes)
	if sess == nil {
		if m.maxSessionsPerCaller > 0 && strings.TrimSpace(capacityNS) != "" && len(m.capacityIndex[capacityNS]) >= m.maxSessionsPerCaller {
			m.mu.Unlock()
			return nil, false, nil, ErrCallerCapacityExceeded
		}
		if m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
			m.mu.Unlock()
			return nil, false, nil, ErrCapacityExceeded
		}

		sess = NewExecutionSession(meta)
		m.sessions[meta.Key] = sess
		m.addCapacityIndexLocked(sess)
		m.upsertBindingLocked(sess)
		if reserveLease {
			sess.reserveLease()
		}
		m.mu.Unlock()
		return sess, true, nil, nil
	}
	if hasBinding {
		if strings.TrimSpace(sess.BindingKey) == "" {
			sess.BindingKey = meta.BindingKey
		}
		if currentBinding == nil || currentBinding.SessionKey != sess.Key {
			m.upsertBindingLocked(sess)
		}
	}
	if reserveLease {
		sess.reserveLease()
	}
	m.mu.Unlock()
	return sess, false, nil, nil
}

func (m *Manager) resolveLiveLocalBinding(bindingKey string, config managerRemoteConfig) (*Binding, []pendingBindingDelete, []*ExecutionSession) {
	toCleanup := make([]*ExecutionSession, 0, 1)
	pendingDeletes := make([]pendingBindingDelete, 0, 1)

	for attempt := 0; attempt < 2; attempt++ {
		binding, changed := m.observeLiveLocalBinding(bindingKey, config, &toCleanup, &pendingDeletes)
		if !changed {
			return binding, pendingDeletes, toCleanup
		}
	}
	return nil, pendingDeletes, toCleanup
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
