package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/authutil"
	commonconfig "one-api/common/config"
	"one-api/common/groupctx"
	ratelimit "one-api/common/limit"
	"one-api/common/logger"
	"one-api/common/redis"
	"one-api/common/utils"
	"one-api/metrics"
	"one-api/types"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
)

const (
	defaultResponsesWSConnectPerCredentialPerMinute = 600
	defaultResponsesWSPendingPerCredential          = 96
	defaultResponsesWSPendingPerUser                = 96
	defaultResponsesWSPendingPerGroup               = 192
	defaultResponsesWSPendingGlobal                 = 512
	defaultResponsesWSPendingBytesPerCredential     = 64 << 20
	defaultResponsesWSPendingBytesPerUser           = 64 << 20
	defaultResponsesWSPendingBytesPerGroup          = 128 << 20
	defaultResponsesWSPendingBytesGlobal            = 256 << 20
	defaultResponsesWSActivePerCredential           = 16
	defaultResponsesWSActivePerUser                 = 16
	defaultResponsesWSActivePerGroup                = 32
	defaultResponsesWSActiveGlobal                  = 128
	responsesWSConnectLimiterKeyPrefix              = "responses-ws-connect:"
	responsesWSActiveLeaseKeyPrefix                 = "responses-ws-active:"
)

var responsesWSActiveLeaseTTL = 2 * time.Minute

type ResponsesWSLease interface {
	Release()
	Lost() <-chan struct{}
}

type ResponsesWSByteLease interface {
	TryAcquire(bytes int) bool
	Release()
}

type responsesWSStopper interface {
	Stop()
}

type responsesWSLease struct {
	release func()
	once    sync.Once
	lost    <-chan struct{}
}

func (l *responsesWSLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.release != nil {
			l.release()
		}
	})
}

func (l *responsesWSLease) Lost() <-chan struct{} {
	if l == nil {
		return nil
	}
	return l.lost
}

func newResponsesWSLease(release func(), lost <-chan struct{}) *responsesWSLease {
	return &responsesWSLease{
		release: release,
		lost:    lost,
	}
}

var responsesWSCapacity = struct {
	sync.Mutex
	pendingByCredential      map[string]int
	pendingByUser            map[string]int
	pendingByGroup           map[string]int
	pendingGlobal            int
	pendingBytesByCredential map[string]int64
	pendingBytesByUser       map[string]int64
	pendingBytesByGroup      map[string]int64
	pendingBytesGlobal       int64
	activeByCredential       map[string]int
	activeByUser             map[string]int
	activeByGroup            map[string]int
	activeGlobal             int
}{
	pendingByCredential:      make(map[string]int),
	pendingByUser:            make(map[string]int),
	pendingByGroup:           make(map[string]int),
	pendingBytesByCredential: make(map[string]int64),
	pendingBytesByUser:       make(map[string]int64),
	pendingBytesByGroup:      make(map[string]int64),
	activeByCredential:       make(map[string]int),
	activeByUser:             make(map[string]int),
	activeByGroup:            make(map[string]int),
}

type websocketConnectionLimiterState struct {
	name                string
	recordRedisFallback func(string)
	sync.Mutex
	configuredLimit     int
	redisEnabled        bool
	limiter             ratelimit.RateLimiter
	fallbackLimiter     ratelimit.RateLimiter
	warnedInProcess     bool
	warnedRedisFallback bool
}

var responsesWSConnectionLimiter = websocketConnectionLimiterState{
	name:                "ResponsesWS",
	recordRedisFallback: metrics.RecordResponsesWSConnectionLimiterRedisFallback,
}

func AllowResponsesWSConnectionAttempt(c *gin.Context) *types.OpenAIErrorWithStatusCode {
	configuredLimit := responsesWSConfiguredLimit("responses_ws.connect_per_credential_per_minute", defaultResponsesWSConnectPerCredentialPerMinute)
	if configuredLimit == -1 {
		return nil
	}

	identity, apiErr := responsesWSCapacityIdentityFromContext(c)
	if apiErr != nil {
		return apiErr
	}
	keys := []string{responsesWSConnectLimiterKeyPrefix + "user:" + identity.user}
	if identity.credential != identity.user {
		keys = append(keys, responsesWSConnectLimiterKeyPrefix+"token:"+identity.credential)
	}
	for _, key := range keys {
		if responsesWSConnectionAttemptAllowed(configuredLimit, key) {
			continue
		}
		metrics.RecordResponsesWSConnectionRateLimited(responsesWSMetricGroup(c), identity.credentialKind)
		return common.StringErrorWrapperLocal("too many responses websocket connection attempts", "responses_ws_connection_rate_limited", http.StatusTooManyRequests)
	}
	return nil
}

func AcquireResponsesWSPendingSlot(c *gin.Context) (ResponsesWSLease, *types.OpenAIErrorWithStatusCode) {
	identity, apiErr := responsesWSCapacityIdentityFromContext(c)
	if apiErr != nil {
		return nil, apiErr
	}
	tokenLimit := responsesWSConfiguredLimit("responses_ws.pending_per_credential", defaultResponsesWSPendingPerCredential)
	userLimit := responsesWSConfiguredLimit("responses_ws.pending_per_user", defaultResponsesWSPendingPerUser)
	tokenLimit = responsesWSTightenLimit(tokenLimit, userLimit)
	groupLimit := responsesWSConfiguredLimit("responses_ws.pending_per_group", defaultResponsesWSPendingPerGroup)
	globalLimit := responsesWSConfiguredLimit("responses_ws.pending_global", defaultResponsesWSPendingGlobal)
	return acquireResponsesWSPendingLocalLease(identity, tokenLimit, userLimit, groupLimit, globalLimit)
}

type responsesWSPendingByteLease struct {
	mu              sync.Mutex
	identity        responsesWSCapacityIdentity
	credentialLimit int64
	userLimit       int64
	groupLimit      int64
	globalLimit     int64
	reserved        int64
	released        bool
}

func AcquireResponsesWSPendingByteLease(c *gin.Context) (ResponsesWSByteLease, *types.OpenAIErrorWithStatusCode) {
	identity, apiErr := responsesWSCapacityIdentityFromContext(c)
	if apiErr != nil {
		return nil, apiErr
	}
	credentialLimit := responsesWSConfiguredByteLimit("responses_ws.pending_bytes_per_credential", defaultResponsesWSPendingBytesPerCredential)
	userLimit := responsesWSConfiguredByteLimit("responses_ws.pending_bytes_per_user", defaultResponsesWSPendingBytesPerUser)
	credentialLimit = responsesWSTightenByteLimit(credentialLimit, userLimit)
	return &responsesWSPendingByteLease{
		identity:        identity,
		credentialLimit: credentialLimit,
		userLimit:       userLimit,
		groupLimit:      responsesWSConfiguredByteLimit("responses_ws.pending_bytes_per_group", defaultResponsesWSPendingBytesPerGroup),
		globalLimit:     responsesWSConfiguredByteLimit("responses_ws.pending_bytes_global", defaultResponsesWSPendingBytesGlobal),
	}, nil
}

func (l *responsesWSPendingByteLease) TryAcquire(bytes int) bool {
	if l == nil || bytes < 0 {
		return false
	}
	if bytes == 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return false
	}
	want := int64(bytes)
	responsesWSCapacity.Lock()
	defer responsesWSCapacity.Unlock()
	if !responsesWSByteCapacityAvailable(responsesWSCapacity.pendingBytesByCredential[l.identity.credential], want, l.credentialLimit) ||
		!responsesWSByteCapacityAvailable(responsesWSCapacity.pendingBytesByUser[l.identity.user], want, l.userLimit) ||
		!responsesWSByteCapacityAvailable(responsesWSCapacity.pendingBytesByGroup[l.identity.group], want, l.groupLimit) ||
		!responsesWSByteCapacityAvailable(responsesWSCapacity.pendingBytesGlobal, want, l.globalLimit) {
		return false
	}
	responsesWSCapacity.pendingBytesByCredential[l.identity.credential] += want
	responsesWSCapacity.pendingBytesByUser[l.identity.user] += want
	responsesWSCapacity.pendingBytesByGroup[l.identity.group] += want
	responsesWSCapacity.pendingBytesGlobal += want
	l.reserved += want
	return true
}

func (l *responsesWSPendingByteLease) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	responsesWSCapacity.Lock()
	defer responsesWSCapacity.Unlock()
	decrementResponsesWSByteCounter(responsesWSCapacity.pendingBytesByCredential, l.identity.credential, l.reserved)
	decrementResponsesWSByteCounter(responsesWSCapacity.pendingBytesByUser, l.identity.user, l.reserved)
	decrementResponsesWSByteCounter(responsesWSCapacity.pendingBytesByGroup, l.identity.group, l.reserved)
	responsesWSCapacity.pendingBytesGlobal -= l.reserved
	if responsesWSCapacity.pendingBytesGlobal < 0 {
		responsesWSCapacity.pendingBytesGlobal = 0
	}
	l.reserved = 0
}

func responsesWSByteCapacityAvailable(current, want, limit int64) bool {
	return limit < 0 || want <= limit-current
}

func decrementResponsesWSByteCounter(counters map[string]int64, key string, bytes int64) {
	remaining := counters[key] - bytes
	if remaining <= 0 {
		delete(counters, key)
		return
	}
	counters[key] = remaining
}

func AcquireResponsesWSActiveLease(c *gin.Context) (ResponsesWSLease, *types.OpenAIErrorWithStatusCode) {
	identity, apiErr := responsesWSCapacityIdentityFromContext(c)
	if apiErr != nil {
		return nil, apiErr
	}
	credentialLimit := responsesWSConfiguredLimit("responses_ws.active_per_credential", defaultResponsesWSActivePerCredential)
	userLimit := responsesWSConfiguredLimit("responses_ws.active_per_user", defaultResponsesWSActivePerUser)
	credentialLimit = responsesWSTightenLimit(credentialLimit, userLimit)
	groupLimit := responsesWSConfiguredLimit("responses_ws.active_per_group", defaultResponsesWSActivePerGroup)
	globalLimit := responsesWSConfiguredLimit("responses_ws.active_global", defaultResponsesWSActiveGlobal)

	if commonconfig.RedisEnabled {
		if redis.GetRedisClient() == nil {
			metrics.RecordResponsesWSConnectionLimiterRedisFallback("active_lease_redis_client_unavailable")
			if !responsesWSActiveLeaseRedisFailOpen() {
				responsesWSWarnRedisFallbackOnce("ResponsesWS active lease Redis client is unavailable; rejecting new active leases")
				return nil, common.StringErrorWrapperLocal("responses websocket active lease backend unavailable", "responses_ws_active_lease_backend_unavailable", http.StatusServiceUnavailable)
			}
			responsesWSWarnRedisFallbackOnce("ResponsesWS active lease Redis client is unavailable; using in-process limiter")
			return acquireResponsesWSActiveLocalLease(identity, credentialLimit, userLimit, groupLimit, globalLimit)
		}
		lease, apiErr, fallback := acquireResponsesWSActiveRedisLease(identity, credentialLimit, userLimit, groupLimit, globalLimit)
		if !fallback {
			return lease, apiErr
		}
	}
	return acquireResponsesWSActiveLocalLease(identity, credentialLimit, userLimit, groupLimit, globalLimit)
}

type responsesWSCapacityIdentity struct {
	credential     string
	credentialKind string
	user           string
	group          string
}

func acquireResponsesWSPendingLocalLease(identity responsesWSCapacityIdentity, credentialLimit, userLimit, groupLimit, globalLimit int) (ResponsesWSLease, *types.OpenAIErrorWithStatusCode) {
	responsesWSCapacity.Lock()
	defer responsesWSCapacity.Unlock()
	if credentialLimit >= 0 && responsesWSCapacity.pendingByCredential[identity.credential] >= credentialLimit {
		return nil, common.StringErrorWrapperLocal("too many pending responses websocket handshakes", "responses_ws_pending_slot_exceeded", http.StatusTooManyRequests)
	}
	if userLimit >= 0 && responsesWSCapacity.pendingByUser[identity.user] >= userLimit {
		return nil, common.StringErrorWrapperLocal("too many pending responses websocket handshakes for user", "responses_ws_pending_user_limit_exceeded", http.StatusTooManyRequests)
	}
	if groupLimit >= 0 && responsesWSCapacity.pendingByGroup[identity.group] >= groupLimit {
		return nil, common.StringErrorWrapperLocal("responses websocket group pending limit reached", "responses_ws_pending_group_limit_exceeded", http.StatusServiceUnavailable)
	}
	if globalLimit >= 0 && responsesWSCapacity.pendingGlobal >= globalLimit {
		return nil, common.StringErrorWrapperLocal("responses websocket global pending limit reached", "responses_ws_pending_global_limit_exceeded", http.StatusServiceUnavailable)
	}
	responsesWSCapacity.pendingByCredential[identity.credential]++
	responsesWSCapacity.pendingByUser[identity.user]++
	responsesWSCapacity.pendingByGroup[identity.group]++
	responsesWSCapacity.pendingGlobal++
	return newResponsesWSLease(func() {
		responsesWSCapacity.Lock()
		defer responsesWSCapacity.Unlock()
		decrementResponsesWSCounter(responsesWSCapacity.pendingByCredential, identity.credential)
		decrementResponsesWSCounter(responsesWSCapacity.pendingByUser, identity.user)
		decrementResponsesWSCounter(responsesWSCapacity.pendingByGroup, identity.group)
		if responsesWSCapacity.pendingGlobal > 0 {
			responsesWSCapacity.pendingGlobal--
		}
	}, nil), nil
}

func acquireResponsesWSActiveLocalLease(identity responsesWSCapacityIdentity, credentialLimit, userLimit, groupLimit, globalLimit int) (ResponsesWSLease, *types.OpenAIErrorWithStatusCode) {
	responsesWSCapacity.Lock()
	defer responsesWSCapacity.Unlock()

	if credentialLimit >= 0 && responsesWSCapacity.activeByCredential[identity.credential] >= credentialLimit {
		return nil, common.StringErrorWrapperLocal("responses websocket active connection limit reached", "responses_ws_active_credential_limit_exceeded", http.StatusTooManyRequests)
	}
	if userLimit >= 0 && responsesWSCapacity.activeByUser[identity.user] >= userLimit {
		return nil, common.StringErrorWrapperLocal("responses websocket user active connection limit reached", "responses_ws_active_user_limit_exceeded", http.StatusTooManyRequests)
	}
	if groupLimit >= 0 && responsesWSCapacity.activeByGroup[identity.group] >= groupLimit {
		return nil, common.StringErrorWrapperLocal("responses websocket group active connection limit reached", "responses_ws_active_group_limit_exceeded", http.StatusServiceUnavailable)
	}
	if globalLimit >= 0 && responsesWSCapacity.activeGlobal >= globalLimit {
		return nil, common.StringErrorWrapperLocal("responses websocket global active connection limit reached", "responses_ws_active_global_limit_exceeded", http.StatusServiceUnavailable)
	}

	responsesWSCapacity.activeByCredential[identity.credential]++
	responsesWSCapacity.activeByUser[identity.user]++
	responsesWSCapacity.activeByGroup[identity.group]++
	responsesWSCapacity.activeGlobal++

	return newResponsesWSLease(func() {
		responsesWSCapacity.Lock()
		defer responsesWSCapacity.Unlock()
		decrementResponsesWSCounter(responsesWSCapacity.activeByCredential, identity.credential)
		decrementResponsesWSCounter(responsesWSCapacity.activeByUser, identity.user)
		decrementResponsesWSCounter(responsesWSCapacity.activeByGroup, identity.group)
		if responsesWSCapacity.activeGlobal > 0 {
			responsesWSCapacity.activeGlobal--
		}
	}, nil), nil
}

type responsesWSRedisCapacityCounter struct {
	key     string
	limit   int
	status  int
	code    string
	message string
}

const acquireResponsesWSRedisLeaseScriptSource = `
	local key = KEYS[1]
	local member = ARGV[1]
	local now_ms = tonumber(ARGV[2])
	local expires_at_ms = tonumber(ARGV[3])
	local ttl_ms = tonumber(ARGV[4])
	local limit = tonumber(ARGV[5])
	redis.call("ZREMRANGEBYSCORE", key, "-inf", now_ms)
	if redis.call("ZCARD", key) >= limit then
		return 0
	end
	redis.call("ZADD", key, expires_at_ms, member)
	redis.call("PEXPIRE", key, ttl_ms)
	return 1
`

const heartbeatResponsesWSRedisLeaseScriptSource = `
	local key = KEYS[1]
	local member = ARGV[1]
	local now_ms = tonumber(ARGV[2])
	local expires_at_ms = tonumber(ARGV[3])
	local ttl_ms = tonumber(ARGV[4])
	redis.call("ZREMRANGEBYSCORE", key, "-inf", now_ms)
	if not redis.call("ZSCORE", key, member) then
		return 0
	end
	redis.call("ZADD", key, "XX", expires_at_ms, member)
	redis.call("PEXPIRE", key, ttl_ms)
	return 1
`

const releaseResponsesWSRedisLeaseScriptSource = `
	local removed = redis.call("ZREM", KEYS[1], ARGV[1])
	if redis.call("ZCARD", KEYS[1]) == 0 then
		redis.call("DEL", KEYS[1])
	end
	return removed
`

var acquireResponsesWSRedisLeaseScript = redis.NewScript(acquireResponsesWSRedisLeaseScriptSource)
var heartbeatResponsesWSRedisLeaseScript = redis.NewScript(heartbeatResponsesWSRedisLeaseScriptSource)
var releaseResponsesWSRedisLeaseScript = redis.NewScript(releaseResponsesWSRedisLeaseScriptSource)

var recordResponsesWSActiveLeaseLost = metrics.RecordResponsesWSActiveLeaseLost

func ProbeResponsesWSActiveLeaseBackend(ctx context.Context) (retErr error) {
	client := redis.GetRedisClient()
	if client == nil {
		return errors.New("responses websocket active lease Redis client is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	leaseTTL := 5 * time.Second
	now := time.Now()
	probeID := "readiness:" + utils.GetUUID()
	key := responsesWSActiveLeaseKeyPrefix + probeID
	status, err := acquireResponsesWSRedisLeaseScript.Run(
		ctx,
		client,
		[]string{key},
		probeID,
		now.UnixMilli(),
		now.Add(leaseTTL).UnixMilli(),
		leaseTTL.Milliseconds(),
		1,
	).Int64()
	if err != nil {
		return fmt.Errorf("responses websocket active lease acquire probe: %w", err)
	}
	if status != 1 {
		return errors.New("responses websocket active lease acquire probe was rejected")
	}
	defer func() {
		_, releaseErr := releaseResponsesWSRedisLeaseScript.Run(ctx, client, []string{key}, probeID).Int64()
		if retErr == nil && releaseErr != nil {
			retErr = fmt.Errorf("responses websocket active lease release probe: %w", releaseErr)
		}
	}()
	now = time.Now()
	status, err = heartbeatResponsesWSRedisLeaseScript.Run(
		ctx,
		client,
		[]string{key},
		probeID,
		now.UnixMilli(),
		now.Add(leaseTTL).UnixMilli(),
		leaseTTL.Milliseconds(),
	).Int64()
	if err != nil {
		return fmt.Errorf("responses websocket active lease heartbeat probe: %w", err)
	}
	if status != 1 {
		return errors.New("responses websocket active lease heartbeat probe lost its member")
	}
	return nil
}

func acquireResponsesWSActiveRedisLease(identity responsesWSCapacityIdentity, credentialLimit, userLimit, groupLimit, globalLimit int) (ResponsesWSLease, *types.OpenAIErrorWithStatusCode, bool) {
	counters := []responsesWSRedisCapacityCounter{
		{
			key:     responsesWSActiveLeaseKeyPrefix + "credential:" + identity.credential,
			limit:   credentialLimit,
			status:  http.StatusTooManyRequests,
			code:    "responses_ws_active_credential_limit_exceeded",
			message: "responses websocket active connection limit reached",
		},
		{
			key:     responsesWSActiveLeaseKeyPrefix + "user:" + identity.user,
			limit:   userLimit,
			status:  http.StatusTooManyRequests,
			code:    "responses_ws_active_user_limit_exceeded",
			message: "responses websocket user active connection limit reached",
		},
		{
			key:     responsesWSActiveLeaseKeyPrefix + "group:" + identity.group,
			limit:   groupLimit,
			status:  http.StatusServiceUnavailable,
			code:    "responses_ws_active_group_limit_exceeded",
			message: "responses websocket group active connection limit reached",
		},
		{
			key:     responsesWSActiveLeaseKeyPrefix + "global",
			limit:   globalLimit,
			status:  http.StatusServiceUnavailable,
			code:    "responses_ws_active_global_limit_exceeded",
			message: "responses websocket global active connection limit reached",
		},
	}
	client := redis.GetRedisClient()
	ctx := context.Background()
	acquired := make([]string, 0, len(counters))
	leaseTTL := responsesWSActiveLeaseTTL
	leaseID := utils.GetUUID()
	for _, counter := range counters {
		if counter.limit < 0 {
			continue
		}
		attempted := append(append([]string(nil), acquired...), counter.key)
		now := time.Now()
		status, err := acquireResponsesWSRedisLeaseScript.Run(
			ctx,
			client,
			[]string{counter.key},
			leaseID,
			now.UnixMilli(),
			now.Add(leaseTTL).UnixMilli(),
			leaseTTL.Milliseconds(),
			counter.limit,
		).Int64()
		if err != nil {
			releaseResponsesWSRedisLeases(ctx, attempted, leaseID)
			metrics.RecordResponsesWSConnectionLimiterRedisFallback("active_lease_redis_error")
			if !responsesWSActiveLeaseRedisFailOpen() {
				responsesWSWarnRedisFallbackOnce("ResponsesWS active lease Redis error; rejecting new active leases: " + err.Error())
				return nil, common.StringErrorWrapperLocal("responses websocket active lease backend unavailable", "responses_ws_active_lease_backend_unavailable", http.StatusServiceUnavailable), false
			}
			responsesWSWarnRedisFallbackOnce("ResponsesWS active lease Redis error; using in-process limiter: " + err.Error())
			return nil, nil, true
		}
		if status != 1 {
			releaseResponsesWSRedisLeases(ctx, attempted, leaseID)
			return nil, common.StringErrorWrapperLocal(counter.message, counter.code, counter.status), false
		}
		acquired = attempted
	}
	done := make(chan struct{})
	lost := make(chan struct{})
	go heartbeatResponsesWSRedisLeases(acquired, leaseID, leaseTTL, done, lost)
	return newResponsesWSLease(func() {
		close(done)
		releaseResponsesWSRedisLeases(context.Background(), acquired, leaseID)
	}, lost), nil, false
}

func responsesWSActiveLeaseRedisFailOpen() bool {
	return commonconfig.ResponsesWSActiveLeaseRedisFailOpen()
}

func heartbeatResponsesWSRedisLeases(keys []string, leaseID string, leaseTTL time.Duration, done <-chan struct{}, lost chan<- struct{}) {
	if len(keys) == 0 {
		return
	}
	var lostOnce sync.Once
	signalLost := func(reason string, err error) {
		lostOnce.Do(func() {
			reason = strings.TrimSpace(reason)
			if reason == "" {
				reason = "unknown"
			}
			recordResponsesWSActiveLeaseLost(reason)
			message := fmt.Sprintf("responses websocket active Redis lease lost: reason=%s affected_connections=1 lease_keys=%d", reason, len(keys))
			if err != nil {
				message += " err=" + err.Error()
			}
			logger.LogError(context.Background(), message)
			if lost != nil {
				close(lost)
			}
		})
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			signalLost("heartbeat_panic", fmt.Errorf("%v", recovered))
		}
	}()
	tickerInterval := leaseTTL / 3
	if tickerInterval <= 0 {
		tickerInterval = time.Second
	}
	ticker := time.NewTicker(tickerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			client := redis.GetRedisClient()
			if client == nil {
				signalLost("redis_client_unavailable", nil)
				return
			}
			for _, key := range keys {
				timeout := leaseTTL / 2
				if timeout <= 0 {
					timeout = time.Second
				}
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				now := time.Now()
				status, err := heartbeatResponsesWSRedisLeaseScript.Run(
					ctx,
					client,
					[]string{key},
					leaseID,
					now.UnixMilli(),
					now.Add(leaseTTL).UnixMilli(),
					leaseTTL.Milliseconds(),
				).Int64()
				cancel()
				if err != nil {
					signalLost("heartbeat_error", err)
					return
				}
				if status != 1 {
					signalLost("lease_member_missing", nil)
					return
				}
			}
		}
	}
}

func releaseResponsesWSRedisLeases(ctx context.Context, keys []string, leaseID string) {
	client := redis.GetRedisClient()
	if client == nil {
		return
	}
	for _, key := range keys {
		if _, err := releaseResponsesWSRedisLeaseScript.Run(ctx, client, []string{key}, leaseID).Int64(); err != nil {
			logger.LogError(ctx, "responses websocket redis lease release failed: "+err.Error())
		}
	}
}

func responsesWSConnectionAttemptAllowed(configuredLimit int, key string) bool {
	return responsesWSConnectionLimiter.allow(configuredLimit, key)
}

func (state *websocketConnectionLimiterState) allow(configuredLimit int, key string) bool {
	limiter, fallback := state.limiters(configuredLimit)
	if commonconfig.RedisEnabled && redis.GetRedisClient() == nil {
		state.recordFallback("redis_client_unavailable")
		state.warnRedisFallbackOnce(state.name + " connection limiter Redis client is unavailable; using in-process limiter for handshake protection")
		return fallback.Allow(key)
	}
	if aware, ok := limiter.(ratelimit.ErrorAwareRateLimiter); ok {
		allowed, err := aware.AllowNWithError(key, 1)
		if err == nil {
			return allowed
		}
		if commonconfig.RedisEnabled {
			// Redis 故障期间使用有限本地计数，只保证当前实例的建连限额。
			state.recordFallback("redis_error")
			state.warnRedisFallbackOnce(state.name + " connection limiter Redis error; using in-process limiter for handshake protection: " + err.Error())
			return fallback.Allow(key)
		}
		return false
	}
	return limiter.Allow(key)
}

func (state *websocketConnectionLimiterState) limiters(configuredLimit int) (ratelimit.RateLimiter, ratelimit.RateLimiter) {
	redisEnabled := commonconfig.RedisEnabled
	warnInProcess := false

	state.Lock()
	if state.limiter == nil ||
		state.configuredLimit != configuredLimit ||
		state.redisEnabled != redisEnabled {
		stopResponsesWSLimiter(state.limiter)
		stopResponsesWSLimiter(state.fallbackLimiter)
		state.configuredLimit = configuredLimit
		state.redisEnabled = redisEnabled
		state.limiter = ratelimit.NewAPILimiter(configuredLimit)
		state.fallbackLimiter = newResponsesWSInProcessAPILimiter(configuredLimit)
		if !redisEnabled && !state.warnedInProcess {
			state.warnedInProcess = true
			warnInProcess = true
		}
	}
	limiter := state.limiter
	fallback := state.fallbackLimiter
	state.Unlock()

	if warnInProcess {
		responsesWSLogWarn(state.name + " connection limiter is using in-process storage because Redis is disabled; limits are not shared across instances")
	}
	return limiter, fallback
}

func stopResponsesWSLimiter(limiter ratelimit.RateLimiter) {
	if stopper, ok := limiter.(responsesWSStopper); ok && stopper != nil {
		stopper.Stop()
	}
}

func newResponsesWSInProcessAPILimiter(rpm int) ratelimit.RateLimiter {
	if rpm < ratelimit.RPMThreshold {
		return ratelimit.NewMemoryLimiter(rpm, rpm, time.Minute, false)
	}
	ratePerSecond := float64(rpm) / 60
	perSecond := int(ratePerSecond)
	if perSecond < 1 {
		perSecond = 1
	}
	return ratelimit.NewMemoryLimiter(perSecond, rpm, time.Minute, true)
}

func responsesWSWarnRedisFallbackOnce(message string) {
	responsesWSConnectionLimiter.warnRedisFallbackOnce(message)
}

func (state *websocketConnectionLimiterState) recordFallback(reason string) {
	if state.recordRedisFallback != nil {
		state.recordRedisFallback(reason)
	}
}

func (state *websocketConnectionLimiterState) warnRedisFallbackOnce(message string) {
	state.Lock()
	if state.warnedRedisFallback {
		state.Unlock()
		return
	}
	state.warnedRedisFallback = true
	state.Unlock()
	responsesWSLogWarn(message)
}

func WarnResponsesWSAnonymousCapacityBucketIfEnabled() {
	if viper.GetBool("responses_ws.allow_anonymous_capacity_bucket") {
		responsesWSLogWarn("responses_ws.allow_anonymous_capacity_bucket=true is intended only for local diagnostics; anonymous websocket capacity is shared")
	}
}

func responsesWSLogWarn(message string) {
	if logger.Logger != nil {
		logger.Logger.Warn("[SYS] | " + message)
	}
}

func responsesWSSysLog(message string) {
	if logger.Logger != nil {
		logger.SysLog(message)
	}
}

func responsesWSMetricGroup(c *gin.Context) string {
	group := strings.TrimSpace(groupctx.CurrentRoutingGroup(c))
	if group == "" && c != nil {
		group = strings.TrimSpace(c.GetString("group"))
	}
	if group == "" {
		group = "default"
	}
	return group
}

func decrementResponsesWSCounter(counters map[string]int, key string) {
	current := counters[key]
	if current <= 1 {
		delete(counters, key)
		return
	}
	counters[key] = current - 1
}

func responsesWSConfiguredLimit(key string, fallback int) int {
	if viper.IsSet(key) {
		configured := viper.GetInt(key)
		if configured == -1 {
			responsesWSSysLog(fmt.Sprintf("%s explicitly set to unlimited (-1)", key))
			return -1
		}
		if configured > 0 {
			return configured
		}
	}
	return fallback
}

func responsesWSConfiguredByteLimit(key string, fallback int64) int64 {
	if viper.IsSet(key) {
		configured := viper.GetInt64(key)
		if configured == -1 {
			return -1
		}
		if configured > 0 {
			return configured
		}
	}
	return fallback
}

func responsesWSTightenLimit(limit, ownerLimit int) int {
	if ownerLimit < 0 {
		return limit
	}
	if limit < 0 || limit > ownerLimit {
		return ownerLimit
	}
	return limit
}

func responsesWSTightenByteLimit(limit, ownerLimit int64) int64 {
	if ownerLimit < 0 {
		return limit
	}
	if limit < 0 || limit > ownerLimit {
		return ownerLimit
	}
	return limit
}

func responsesWSCredentialIdentity(c *gin.Context) (string, string, *types.OpenAIErrorWithStatusCode) {
	if c != nil {
		if tokenID := c.GetInt("token_id"); tokenID > 0 {
			return "token:" + strconv.Itoa(tokenID), "token", nil
		}
		if userID := c.GetInt("id"); userID > 0 {
			return "user:" + strconv.Itoa(userID), "user", nil
		}
		if namespace := authutil.StableRequestCredentialNamespace(c.Request); namespace != "" {
			return namespace, "auth_namespace", nil
		}
		if viper.GetBool("responses_ws.allow_anonymous_capacity_bucket") {
			return "anonymous", "anonymous", nil
		}
	}
	return "", "", common.StringErrorWrapperLocal("responses websocket credential namespace is required", "responses_ws_credential_required", http.StatusUnauthorized)
}

func responsesWSCapacityIdentityFromContext(c *gin.Context) (responsesWSCapacityIdentity, *types.OpenAIErrorWithStatusCode) {
	credential, kind, apiErr := responsesWSCredentialIdentity(c)
	if apiErr != nil {
		return responsesWSCapacityIdentity{}, apiErr
	}
	user := credential
	if c != nil {
		if userID := c.GetInt("id"); userID > 0 {
			user = "user:" + strconv.Itoa(userID)
		}
	}
	return responsesWSCapacityIdentity{
		credential:     credential,
		credentialKind: kind,
		user:           user,
		group:          responsesWSMetricGroup(c),
	}, nil
}
