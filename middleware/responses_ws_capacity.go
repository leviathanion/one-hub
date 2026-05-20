package middleware

import (
	"context"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/authutil"
	commonconfig "one-api/common/config"
	"one-api/common/groupctx"
	ratelimit "one-api/common/limit"
	"one-api/common/logger"
	"one-api/common/redis"
	"one-api/common/requester"
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
	defaultResponsesWSConnectPerCredentialPerMinute = 300
	defaultResponsesWSPendingPerCredential          = 16
	defaultResponsesWSActivePerCredential           = 32
	defaultResponsesWSActivePerGroup                = 128
	defaultResponsesWSActiveGlobal                  = 1024
	responsesWSConnectLimiterKeyPrefix              = "responses-ws-connect:"
	responsesWSActiveLeaseKeyPrefix                 = "responses-ws-active:"
)

var responsesWSActiveLeaseTTL = 2 * time.Minute

type ResponsesWSLease interface {
	Release()
	Lost() <-chan struct{}
}

type responsesWSStopper interface {
	Stop()
}

type responsesWSLease struct {
	guard *requester.WSActiveCounterGuard
	lost  <-chan struct{}
}

func (l *responsesWSLease) Release() {
	if l == nil {
		return
	}
	l.guard.Release()
}

func (l *responsesWSLease) Lost() <-chan struct{} {
	if l == nil {
		return nil
	}
	return l.lost
}

func newResponsesWSLease(release func(), lost <-chan struct{}) *responsesWSLease {
	return &responsesWSLease{
		guard: requester.NewWSActiveCounterGuard(release),
		lost:  lost,
	}
}

var responsesWSCapacity = struct {
	sync.Mutex
	pendingByCredential map[string]int
	activeByCredential  map[string]int
	activeByGroup       map[string]int
	activeGlobal        int
}{
	pendingByCredential: make(map[string]int),
	activeByCredential:  make(map[string]int),
	activeByGroup:       make(map[string]int),
}

var responsesWSConnectionLimiter = struct {
	sync.Mutex
	configuredLimit     int
	redisEnabled        bool
	limiter             ratelimit.RateLimiter
	fallbackLimiter     ratelimit.RateLimiter
	warnedInProcess     bool
	warnedRedisFallback bool
}{}

func AllowResponsesWSConnectionAttempt(c *gin.Context) *types.OpenAIErrorWithStatusCode {
	configuredLimit := responsesWSConfiguredLimit("responses_ws.connect_per_credential_per_minute", defaultResponsesWSConnectPerCredentialPerMinute)
	if configuredLimit == -1 {
		return nil
	}

	credential, credentialKind, apiErr := responsesWSCredentialIdentity(c)
	if apiErr != nil {
		return apiErr
	}

	key := responsesWSConnectLimiterKeyPrefix + credential
	if responsesWSConnectionAttemptAllowed(configuredLimit, key) {
		return nil
	}

	metrics.RecordResponsesWSConnectionRateLimited(responsesWSMetricGroup(c), credentialKind)
	return common.StringErrorWrapperLocal("too many responses websocket connection attempts", "responses_ws_connection_rate_limited", http.StatusTooManyRequests)
}

func AcquireResponsesWSPendingSlot(c *gin.Context) (ResponsesWSLease, *types.OpenAIErrorWithStatusCode) {
	limit := responsesWSConfiguredLimit("responses_ws.pending_per_credential", defaultResponsesWSPendingPerCredential)
	credential, apiErr := responsesWSCredentialKey(c)
	if apiErr != nil {
		return nil, apiErr
	}
	return acquireResponsesWSCounter(
		responsesWSCapacity.pendingByCredential,
		credential,
		limit,
		http.StatusTooManyRequests,
		"responses_ws_pending_slot_exceeded",
		"too many pending responses websocket handshakes",
	)
}

func AcquireResponsesWSActiveLease(c *gin.Context) (ResponsesWSLease, *types.OpenAIErrorWithStatusCode) {
	credential, apiErr := responsesWSCredentialKey(c)
	if apiErr != nil {
		return nil, apiErr
	}
	group := responsesWSMetricGroup(c)
	credentialLimit := responsesWSConfiguredLimit("responses_ws.active_per_credential", defaultResponsesWSActivePerCredential)
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
			return acquireResponsesWSActiveLocalLease(credential, group, credentialLimit, groupLimit, globalLimit)
		}
		lease, apiErr, fallback := acquireResponsesWSActiveRedisLease(credential, group, credentialLimit, groupLimit, globalLimit)
		if !fallback {
			return lease, apiErr
		}
	}
	return acquireResponsesWSActiveLocalLease(credential, group, credentialLimit, groupLimit, globalLimit)
}

func acquireResponsesWSActiveLocalLease(credential, group string, credentialLimit, groupLimit, globalLimit int) (ResponsesWSLease, *types.OpenAIErrorWithStatusCode) {
	responsesWSCapacity.Lock()
	defer responsesWSCapacity.Unlock()

	if credentialLimit >= 0 && responsesWSCapacity.activeByCredential[credential] >= credentialLimit {
		return nil, common.StringErrorWrapperLocal("responses websocket active connection limit reached", "responses_ws_active_credential_limit_exceeded", http.StatusTooManyRequests)
	}
	if groupLimit >= 0 && responsesWSCapacity.activeByGroup[group] >= groupLimit {
		return nil, common.StringErrorWrapperLocal("responses websocket group active connection limit reached", "responses_ws_active_group_limit_exceeded", http.StatusServiceUnavailable)
	}
	if globalLimit >= 0 && responsesWSCapacity.activeGlobal >= globalLimit {
		return nil, common.StringErrorWrapperLocal("responses websocket global active connection limit reached", "responses_ws_active_global_limit_exceeded", http.StatusServiceUnavailable)
	}

	responsesWSCapacity.activeByCredential[credential]++
	responsesWSCapacity.activeByGroup[group]++
	responsesWSCapacity.activeGlobal++

	return newResponsesWSLease(func() {
		responsesWSCapacity.Lock()
		defer responsesWSCapacity.Unlock()
		decrementResponsesWSCounter(responsesWSCapacity.activeByCredential, credential)
		decrementResponsesWSCounter(responsesWSCapacity.activeByGroup, group)
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

var releaseResponsesWSRedisCounterScript = redis.NewScript(`
	local current = redis.call("DECR", KEYS[1])
	if current <= 0 then
		redis.call("DEL", KEYS[1])
	end
	return current
`)

func acquireResponsesWSActiveRedisLease(credential, group string, credentialLimit, groupLimit, globalLimit int) (ResponsesWSLease, *types.OpenAIErrorWithStatusCode, bool) {
	counters := []responsesWSRedisCapacityCounter{
		{
			key:     responsesWSActiveLeaseKeyPrefix + "credential:" + credential,
			limit:   credentialLimit,
			status:  http.StatusTooManyRequests,
			code:    "responses_ws_active_credential_limit_exceeded",
			message: "responses websocket active connection limit reached",
		},
		{
			key:     responsesWSActiveLeaseKeyPrefix + "group:" + group,
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
	for _, counter := range counters {
		if counter.limit < 0 {
			continue
		}
		current, err := client.Incr(ctx, counter.key).Result()
		if err != nil {
			releaseResponsesWSRedisCounters(ctx, acquired)
			metrics.RecordResponsesWSConnectionLimiterRedisFallback("active_lease_redis_error")
			if !responsesWSActiveLeaseRedisFailOpen() {
				responsesWSWarnRedisFallbackOnce("ResponsesWS active lease Redis error; rejecting new active leases: " + err.Error())
				return nil, common.StringErrorWrapperLocal("responses websocket active lease backend unavailable", "responses_ws_active_lease_backend_unavailable", http.StatusServiceUnavailable), false
			}
			responsesWSWarnRedisFallbackOnce("ResponsesWS active lease Redis error; using in-process limiter: " + err.Error())
			return nil, nil, true
		}
		acquired = append(acquired, counter.key)
		if err := client.Expire(ctx, counter.key, responsesWSActiveLeaseTTL).Err(); err != nil {
			releaseResponsesWSRedisCounters(ctx, acquired)
			metrics.RecordResponsesWSConnectionLimiterRedisFallback("active_lease_expire_error")
			if !responsesWSActiveLeaseRedisFailOpen() {
				responsesWSWarnRedisFallbackOnce("ResponsesWS active lease Redis expire error; rejecting new active leases: " + err.Error())
				return nil, common.StringErrorWrapperLocal("responses websocket active lease backend unavailable", "responses_ws_active_lease_backend_unavailable", http.StatusServiceUnavailable), false
			}
			responsesWSWarnRedisFallbackOnce("ResponsesWS active lease Redis expire error; using in-process limiter: " + err.Error())
			return nil, nil, true
		}
		if current > int64(counter.limit) {
			releaseResponsesWSRedisCounters(ctx, acquired)
			return nil, common.StringErrorWrapperLocal(counter.message, counter.code, counter.status), false
		}
	}
	done := make(chan struct{})
	lost := make(chan struct{})
	go heartbeatResponsesWSRedisCounters(acquired, done, lost)
	return newResponsesWSLease(func() {
		close(done)
		releaseResponsesWSRedisCounters(context.Background(), acquired)
	}, lost), nil, false
}

func responsesWSActiveLeaseRedisFailOpen() bool {
	if !viper.IsSet("responses_ws.active_lease_redis_fail_open") {
		return true
	}
	return viper.GetBool("responses_ws.active_lease_redis_fail_open")
}

func heartbeatResponsesWSRedisCounters(keys []string, done <-chan struct{}, lost chan<- struct{}) {
	if len(keys) == 0 {
		return
	}
	var lostOnce sync.Once
	signalLost := func() {
		lostOnce.Do(func() {
			if lost != nil {
				close(lost)
			}
		})
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.LogError(context.Background(), fmt.Sprintf("responses websocket redis lease heartbeat panic: %v", recovered))
			signalLost()
		}
	}()
	ticker := time.NewTicker(responsesWSActiveLeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			client := redis.GetRedisClient()
			if client == nil {
				signalLost()
				return
			}
			for _, key := range keys {
				timeout := responsesWSActiveLeaseTTL / 2
				if timeout <= 0 {
					timeout = time.Second
				}
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				ok, err := client.Expire(ctx, key, responsesWSActiveLeaseTTL).Result()
				cancel()
				if err != nil || !ok {
					signalLost()
					return
				}
			}
		}
	}
}

func releaseResponsesWSRedisCounters(ctx context.Context, keys []string) {
	client := redis.GetRedisClient()
	if client == nil {
		return
	}
	for _, key := range keys {
		if _, err := releaseResponsesWSRedisCounterScript.Run(ctx, client, []string{key}).Int64(); err != nil {
			logger.LogError(ctx, "responses websocket redis lease release failed: "+err.Error())
			if current, decrErr := client.Decr(ctx, key).Result(); decrErr == nil && current <= 0 {
				_ = client.Del(ctx, key).Err()
			}
		}
	}
}

func responsesWSConnectionAttemptAllowed(configuredLimit int, key string) bool {
	limiter, fallback := responsesWSConnectionAttemptLimiters(configuredLimit)
	if limiter == nil {
		return true
	}
	if commonconfig.RedisEnabled && redis.GetRedisClient() == nil {
		metrics.RecordResponsesWSConnectionLimiterRedisFallback("redis_client_unavailable")
		responsesWSWarnRedisFallbackOnce("ResponsesWS connection limiter Redis client is unavailable; using in-process limiter for handshake protection")
		return fallback == nil || fallback.Allow(key)
	}
	if aware, ok := limiter.(ratelimit.ErrorAwareRateLimiter); ok {
		allowed, err := aware.AllowNWithError(key, 1)
		if err == nil {
			return allowed
		}
		if commonconfig.RedisEnabled {
			// Trade-off: Redis outages temporarily lose cross-instance sharing for
			// this handshake limiter, but fail-open avoids turning infrastructure
			// jitter into a full /v1/responses WebSocket outage.
			metrics.RecordResponsesWSConnectionLimiterRedisFallback("redis_error")
			responsesWSWarnRedisFallbackOnce("ResponsesWS connection limiter Redis error; using in-process limiter for handshake protection: " + err.Error())
			return fallback == nil || fallback.Allow(key)
		}
		return false
	}
	return limiter.Allow(key)
}

func responsesWSConnectionAttemptLimiters(configuredLimit int) (ratelimit.RateLimiter, ratelimit.RateLimiter) {
	redisEnabled := commonconfig.RedisEnabled
	warnInProcess := false

	responsesWSConnectionLimiter.Lock()
	if responsesWSConnectionLimiter.limiter == nil ||
		responsesWSConnectionLimiter.configuredLimit != configuredLimit ||
		responsesWSConnectionLimiter.redisEnabled != redisEnabled {
		stopResponsesWSLimiter(responsesWSConnectionLimiter.limiter)
		stopResponsesWSLimiter(responsesWSConnectionLimiter.fallbackLimiter)
		responsesWSConnectionLimiter.configuredLimit = configuredLimit
		responsesWSConnectionLimiter.redisEnabled = redisEnabled
		responsesWSConnectionLimiter.limiter = ratelimit.NewAPILimiter(configuredLimit)
		responsesWSConnectionLimiter.fallbackLimiter = newResponsesWSInProcessAPILimiter(configuredLimit)
		if !redisEnabled && !responsesWSConnectionLimiter.warnedInProcess {
			responsesWSConnectionLimiter.warnedInProcess = true
			warnInProcess = true
		}
	}
	limiter := responsesWSConnectionLimiter.limiter
	fallback := responsesWSConnectionLimiter.fallbackLimiter
	responsesWSConnectionLimiter.Unlock()

	if warnInProcess {
		responsesWSLogWarn("ResponsesWS connection limiter is using in-process storage because Redis is disabled; limits are not shared across instances")
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
	responsesWSConnectionLimiter.Lock()
	if responsesWSConnectionLimiter.warnedRedisFallback {
		responsesWSConnectionLimiter.Unlock()
		return
	}
	responsesWSConnectionLimiter.warnedRedisFallback = true
	responsesWSConnectionLimiter.Unlock()
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

func acquireResponsesWSCounter(counters map[string]int, key string, limit int, status int, code string, message string) (ResponsesWSLease, *types.OpenAIErrorWithStatusCode) {
	responsesWSCapacity.Lock()
	defer responsesWSCapacity.Unlock()

	if limit >= 0 && counters[key] >= limit {
		return nil, common.StringErrorWrapperLocal(message, code, status)
	}
	counters[key]++
	return newResponsesWSLease(func() {
		responsesWSCapacity.Lock()
		defer responsesWSCapacity.Unlock()
		decrementResponsesWSCounter(counters, key)
	}, nil), nil
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

func responsesWSCredentialKey(c *gin.Context) (string, *types.OpenAIErrorWithStatusCode) {
	key, _, apiErr := responsesWSCredentialIdentity(c)
	return key, apiErr
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
