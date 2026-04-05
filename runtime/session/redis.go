package session

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const defaultExecutionSessionRedisPrefix = "one-hub:execution-session"
const minimumSessionRevocationTTL = time.Minute

type persistedBinding struct {
	Binding Binding `json:"binding"`
}

type redisBindingBackend struct {
	client *redis.Client
	prefix string
}

var (
	createBindingIfAbsentScript = redis.NewScript(`
local function decodeBinding(raw)
  if not raw then
    return nil
  end
  local ok, decoded = pcall(cjson.decode, raw)
  if not ok or type(decoded) ~= 'table' or type(decoded.binding) ~= 'table' then
    return nil
  end
  return decoded.binding
end

local function equivalent(a, b)
  if not a or not b then
    return false
  end
  return a.Key == b.Key and
    a.SessionKey == b.SessionKey and
    a.Scope == b.Scope and
    a.SessionID == b.SessionID and
    a.CallerNS == b.CallerNS and
    tostring(a.ChannelID or '') == tostring(b.ChannelID or '') and
    a.CompatibilityHash == b.CompatibilityHash
end

local current = redis.call('GET', KEYS[1])
if not current then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  return 1
end

local currentBinding = decodeBinding(current)
local replacement = decodeBinding(ARGV[1])
if equivalent(currentBinding, replacement) then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 2
`)
	replaceBindingIfSessionMatchesScript = redis.NewScript(`
local function decodeBinding(raw)
  if not raw then
    return nil
  end
  local ok, decoded = pcall(cjson.decode, raw)
  if not ok or type(decoded) ~= 'table' or type(decoded.binding) ~= 'table' then
    return nil
  end
  return decoded.binding
end

local function equivalent(a, b)
  if not a or not b then
    return false
  end
  return a.Key == b.Key and
    a.SessionKey == b.SessionKey and
    a.Scope == b.Scope and
    a.SessionID == b.SessionID and
    a.CallerNS == b.CallerNS and
    tostring(a.ChannelID or '') == tostring(b.ChannelID or '') and
    a.CompatibilityHash == b.CompatibilityHash
end

local current = redis.call('GET', KEYS[1])
if not current then
  return 2
end

local currentBinding = decodeBinding(current)
local replacement = decodeBinding(ARGV[1])
if equivalent(currentBinding, replacement) then
  redis.call('PEXPIRE', KEYS[1], ARGV[3])
  return 1
end
if currentBinding and currentBinding.SessionKey == ARGV[2] then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3])
  return 1
end
return 2
`)
	deleteBindingIfSessionMatchesScript = redis.NewScript(`
local function decodeBinding(raw)
  if not raw then
    return nil
  end
  local ok, decoded = pcall(cjson.decode, raw)
  if not ok or type(decoded) ~= 'table' or type(decoded.binding) ~= 'table' then
    return nil
  end
  return decoded.binding
end

local current = redis.call('GET', KEYS[1])
if not current then
  return 1
end

local currentBinding = decodeBinding(current)
if currentBinding and currentBinding.SessionKey == ARGV[1] then
  redis.call('DEL', KEYS[1])
  return 1
end
return 2
`)
	touchBindingIfSessionMatchesScript = redis.NewScript(`
local function decodeBinding(raw)
  if not raw then
    return nil
  end
  local ok, decoded = pcall(cjson.decode, raw)
  if not ok or type(decoded) ~= 'table' or type(decoded.binding) ~= 'table' then
    return nil
  end
  return decoded.binding
end

local current = redis.call('GET', KEYS[1])
if not current then
  return 2
end

local currentBinding = decodeBinding(current)
if currentBinding and currentBinding.SessionKey == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 2
`)
	deleteBindingAndRevokeIfSessionMatchesScript = redis.NewScript(`
local function decodeBinding(raw)
  if not raw then
    return nil
  end
  local ok, decoded = pcall(cjson.decode, raw)
  if not ok or type(decoded) ~= 'table' or type(decoded.binding) ~= 'table' then
    return nil
  end
  return decoded.binding
end

local current = redis.call('GET', KEYS[1])
if not current then
  return 2
end

local currentBinding = decodeBinding(current)
if currentBinding and currentBinding.SessionKey == ARGV[1] then
  redis.call('DEL', KEYS[1])
  redis.call('SET', KEYS[2], '1', 'PX', ARGV[2])
  return 1
end
return 2
`)
)

func newRedisBindingBackend(client *redis.Client, prefix string) bindingBackend {
	if client == nil {
		return nil
	}
	return &redisBindingBackend{
		client: client,
		prefix: normalizeSessionRedisPrefix(prefix),
	}
}

func (m *Manager) bindingTTLForSessionLocked(sessionKey string) time.Duration {
	if m == nil {
		return 0
	}
	if sess := m.sessions[sessionKey]; sess != nil {
		if sess.IdleTTL > 0 {
			return sess.IdleTTL
		}
	}
	return m.defaultTTL
}

func (m *Manager) revocationTTLForSessionLocked(sessionKey string) time.Duration {
	ttl := m.bindingTTLForSessionLocked(sessionKey)
	if ttl < minimumSessionRevocationTTL {
		ttl = minimumSessionRevocationTTL
	}
	return ttl
}

func (b *redisBindingBackend) ResolveBinding(ctx context.Context, bindingKey string) (*Binding, ResolveStatus) {
	if b == nil || b.client == nil {
		return nil, ResolveBackendError
	}

	bindingKey = strings.TrimSpace(bindingKey)
	if bindingKey == "" {
		return nil, ResolveMiss
	}

	raw, err := b.client.Get(ctx, b.redisBindingKey(bindingKey)).Result()
	switch {
	case err == redis.Nil:
		return nil, ResolveMiss
	case err != nil:
		return nil, ResolveBackendError
	}

	binding := decodePersistedBinding(raw)
	if binding != nil {
		return binding, ResolveHit
	}
	_ = b.client.Del(ctx, b.redisBindingKey(bindingKey)).Err()
	return nil, ResolveMiss
}

func (b *redisBindingBackend) RevocationStatus(ctx context.Context, sessionKey string) RevocationStatus {
	if b == nil || b.client == nil {
		return RevocationUnknown
	}

	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return RevocationNotRevoked
	}

	exists, err := b.client.Exists(ctx, b.redisRevocationKey(sessionKey)).Result()
	if err != nil {
		return RevocationUnknown
	}
	if exists > 0 {
		return RevocationRevoked
	}
	return RevocationNotRevoked
}

func (b *redisBindingBackend) RevocationStatuses(ctx context.Context, sessionKeys []string) ([]RevocationStatus, error) {
	statuses := make([]RevocationStatus, len(sessionKeys))
	if len(sessionKeys) == 0 {
		return statuses, nil
	}
	if b == nil || b.client == nil {
		return nil, redis.Nil
	}

	pipe := b.client.Pipeline()

	commands := make([]*redis.IntCmd, len(sessionKeys))
	for i, sessionKey := range sessionKeys {
		sessionKey = strings.TrimSpace(sessionKey)
		if sessionKey == "" {
			statuses[i] = RevocationNotRevoked
			continue
		}
		commands[i] = pipe.Exists(ctx, b.redisRevocationKey(sessionKey))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}

	for i, cmd := range commands {
		if cmd == nil {
			continue
		}
		exists, err := cmd.Result()
		if err != nil {
			return nil, err
		}
		if exists > 0 {
			statuses[i] = RevocationRevoked
			continue
		}
		statuses[i] = RevocationNotRevoked
	}
	return statuses, nil
}

func (b *redisBindingBackend) CreateBindingIfAbsent(ctx context.Context, binding *Binding, ttl time.Duration) BindingWriteStatus {
	if b == nil || b.client == nil {
		return BindingWriteBackendError
	}
	payload, ok := marshalPersistedBinding(binding)
	if !ok {
		return BindingWriteBackendError
	}
	return b.runBindingWriteScript(ctx, createBindingIfAbsentScript, []string{b.redisBindingKey(bindingKeyOf(binding))}, payload, ttl)
}

func (b *redisBindingBackend) ReplaceBindingIfSessionMatches(ctx context.Context, bindingKey, expectedSessionKey string, replacement *Binding, ttl time.Duration) BindingWriteStatus {
	bindingKey = strings.TrimSpace(bindingKey)
	expectedSessionKey = strings.TrimSpace(expectedSessionKey)
	if b == nil || b.client == nil || bindingKey == "" || expectedSessionKey == "" || replacement == nil {
		return BindingWriteBackendError
	}

	payload, ok := marshalPersistedBinding(replacement)
	if !ok {
		return BindingWriteBackendError
	}
	result, err := replaceBindingIfSessionMatchesScript.Run(ctx, b.client, []string{b.redisBindingKey(bindingKey)}, payload, expectedSessionKey, normalizeRedisTTL(ttl)).Int64()
	return translateBindingWriteResult(result, err)
}

func (b *redisBindingBackend) DeleteBindingIfSessionMatches(ctx context.Context, bindingKey, expectedSessionKey string) BindingWriteStatus {
	bindingKey = strings.TrimSpace(bindingKey)
	expectedSessionKey = strings.TrimSpace(expectedSessionKey)
	if b == nil || b.client == nil || bindingKey == "" || expectedSessionKey == "" {
		return BindingWriteBackendError
	}

	result, err := deleteBindingIfSessionMatchesScript.Run(ctx, b.client, []string{b.redisBindingKey(bindingKey)}, expectedSessionKey).Int64()
	return translateBindingWriteResult(result, err)
}

func (b *redisBindingBackend) TouchBindingIfSessionMatches(ctx context.Context, bindingKey, expectedSessionKey string, ttl time.Duration) BindingWriteStatus {
	bindingKey = strings.TrimSpace(bindingKey)
	expectedSessionKey = strings.TrimSpace(expectedSessionKey)
	if b == nil || b.client == nil || bindingKey == "" || expectedSessionKey == "" {
		return BindingWriteBackendError
	}

	result, err := touchBindingIfSessionMatchesScript.Run(ctx, b.client, []string{b.redisBindingKey(bindingKey)}, expectedSessionKey, normalizeRedisTTL(ttl)).Int64()
	return translateBindingWriteResult(result, err)
}

func (b *redisBindingBackend) DeleteBindingAndRevokeIfSessionMatches(ctx context.Context, bindingKey, expectedSessionKey string, revokeTTL time.Duration) BindingWriteStatus {
	bindingKey = strings.TrimSpace(bindingKey)
	expectedSessionKey = strings.TrimSpace(expectedSessionKey)
	if b == nil || b.client == nil || bindingKey == "" || expectedSessionKey == "" {
		return BindingWriteBackendError
	}

	result, err := deleteBindingAndRevokeIfSessionMatchesScript.Run(
		ctx,
		b.client,
		[]string{b.redisBindingKey(bindingKey), b.redisRevocationKey(expectedSessionKey)},
		expectedSessionKey,
		normalizeRedisTTL(revokeTTL),
	).Int64()
	return translateBindingWriteResult(result, err)
}

func (b *redisBindingBackend) CountBindings(ctx context.Context) int64 {
	if b == nil || b.client == nil {
		return 0
	}

	pattern := b.redisBindingKey("*")
	var (
		cursor uint64
		total  int64
	)

	for {
		keys, nextCursor, err := b.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return total
		}
		total += int64(len(keys))
		if nextCursor == 0 {
			return total
		}
		cursor = nextCursor
	}
}

func (b *redisBindingBackend) runBindingWriteScript(ctx context.Context, script *redis.Script, keys []string, payload string, ttl time.Duration) BindingWriteStatus {
	if b == nil || b.client == nil || len(keys) == 0 || strings.TrimSpace(keys[0]) == "" {
		return BindingWriteBackendError
	}
	if payload == "" {
		return BindingWriteBackendError
	}
	result, err := script.Run(ctx, b.client, keys, payload, normalizeRedisTTL(ttl)).Int64()
	return translateBindingWriteResult(result, err)
}

func (b *redisBindingBackend) redisBindingKey(bindingKey string) string {
	return b.prefix + ":binding:" + strings.TrimSpace(bindingKey)
}

func (b *redisBindingBackend) redisRevocationKey(sessionKey string) string {
	return b.prefix + ":revoked:" + strings.TrimSpace(sessionKey)
}

func bindingKeyOf(binding *Binding) string {
	if binding == nil {
		return ""
	}
	return strings.TrimSpace(binding.Key)
}

func decodePersistedBinding(raw string) *Binding {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	var persisted persistedBinding
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil {
		return nil
	}

	binding := persisted.Binding
	if strings.TrimSpace(binding.Key) == "" || strings.TrimSpace(binding.SessionKey) == "" {
		return nil
	}
	return &binding
}

func marshalPersistedBinding(binding *Binding) (string, bool) {
	if binding == nil || strings.TrimSpace(binding.Key) == "" || strings.TrimSpace(binding.SessionKey) == "" {
		return "", false
	}

	payload, err := json.Marshal(persistedBinding{Binding: *binding})
	if err != nil {
		return "", false
	}
	return string(payload), true
}

func normalizeRedisTTL(ttl time.Duration) int64 {
	if ttl <= 0 {
		ttl = time.Second
	}
	normalized := ttl.Milliseconds()
	if normalized <= 0 {
		return 1
	}
	return normalized
}

func translateBindingWriteResult(result int64, err error) BindingWriteStatus {
	if err != nil {
		return BindingWriteBackendError
	}
	if result == 1 {
		return BindingWriteApplied
	}
	return BindingWriteConditionMismatch
}

func normalizeSessionRedisPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return defaultExecutionSessionRedisPrefix
	}
	return prefix
}
