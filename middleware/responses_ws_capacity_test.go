package middleware

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	ratelimit "one-api/common/limit"
	"one-api/common/logger"
	commonredis "one-api/common/redis"
	"one-api/model"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

func resetResponsesWSCapacityForTest(t *testing.T) {
	t.Helper()
	responsesWSCapacity.Lock()
	responsesWSCapacity.pendingByCredential = make(map[string]int)
	responsesWSCapacity.activeByCredential = make(map[string]int)
	responsesWSCapacity.activeByGroup = make(map[string]int)
	responsesWSCapacity.activeGlobal = 0
	responsesWSCapacity.Unlock()
	t.Cleanup(func() {
		responsesWSCapacity.Lock()
		defer responsesWSCapacity.Unlock()
		responsesWSCapacity.pendingByCredential = make(map[string]int)
		responsesWSCapacity.activeByCredential = make(map[string]int)
		responsesWSCapacity.activeByGroup = make(map[string]int)
		responsesWSCapacity.activeGlobal = 0
	})
}

func resetResponsesWSConnectionLimiterForTest(t *testing.T) {
	t.Helper()
	responsesWSConnectionLimiter.Lock()
	responsesWSConnectionLimiter.configuredLimit = 0
	responsesWSConnectionLimiter.redisEnabled = false
	responsesWSConnectionLimiter.limiter = nil
	responsesWSConnectionLimiter.fallbackLimiter = nil
	responsesWSConnectionLimiter.warnedInProcess = false
	responsesWSConnectionLimiter.warnedRedisFallback = false
	responsesWSConnectionLimiter.Unlock()
	t.Cleanup(func() {
		responsesWSConnectionLimiter.Lock()
		defer responsesWSConnectionLimiter.Unlock()
		responsesWSConnectionLimiter.configuredLimit = 0
		responsesWSConnectionLimiter.redisEnabled = false
		responsesWSConnectionLimiter.limiter = nil
		responsesWSConnectionLimiter.fallbackLimiter = nil
		responsesWSConnectionLimiter.warnedInProcess = false
		responsesWSConnectionLimiter.warnedRedisFallback = false
	})
}

func setViperForTest(t *testing.T, key string, value int) {
	t.Helper()
	previous := viper.Get(key)
	viper.Set(key, value)
	t.Cleanup(func() {
		viper.Set(key, previous)
	})
}

func setViperBoolForTest(t *testing.T, key string, value bool) {
	t.Helper()
	previous := viper.Get(key)
	viper.Set(key, value)
	t.Cleanup(func() {
		viper.Set(key, previous)
	})
}

func setRedisEnabledForTest(t *testing.T, value bool) {
	t.Helper()
	previous := config.RedisEnabled
	config.RedisEnabled = value
	t.Cleanup(func() {
		config.RedisEnabled = previous
	})
}

func setResponsesWSActiveLeaseTTLForTest(t *testing.T, value time.Duration) {
	t.Helper()
	previous := responsesWSActiveLeaseTTL
	responsesWSActiveLeaseTTL = value
	t.Cleanup(func() {
		responsesWSActiveLeaseTTL = previous
	})
}

type responsesWSFakeRedis struct {
	listener net.Listener
	done     chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	values   map[string]int64
	expires  map[string]time.Time
	commands []string
	errors   []string
}

func startResponsesWSFakeRedis(t *testing.T) *responsesWSFakeRedis {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start fake redis listener: %v", err)
	}
	fake := &responsesWSFakeRedis{
		listener: listener,
		done:     make(chan struct{}),
		values:   make(map[string]int64),
		expires:  make(map[string]time.Time),
	}
	fake.wg.Add(1)
	go fake.acceptLoop()
	t.Cleanup(func() {
		close(fake.done)
		_ = fake.listener.Close()
		fake.wg.Wait()
	})
	return fake
}

func (f *responsesWSFakeRedis) addr() string {
	return f.listener.Addr().String()
}

func (f *responsesWSFakeRedis) acceptLoop() {
	defer f.wg.Done()
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.done:
				return
			default:
				return
			}
		}
		f.wg.Add(1)
		go f.handleConn(conn)
	}
}

func (f *responsesWSFakeRedis) handleConn(conn net.Conn) {
	defer f.wg.Done()
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		args, err := readResponsesWSFakeRedisArray(reader)
		if err != nil {
			f.recordError(err)
			return
		}
		if len(args) == 0 {
			_, _ = io.WriteString(conn, "-ERR empty command\r\n")
			continue
		}
		f.handleCommand(conn, args)
	}
}

func readResponsesWSFakeRedisArray(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("expected RESP array, got %q", line)
	}
	count, err := strconv.Atoi(strings.TrimPrefix(line, "*"))
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, count)
	for i := 0; i < count; i++ {
		bulkHeader, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		bulkHeader = strings.TrimSpace(bulkHeader)
		if !strings.HasPrefix(bulkHeader, "$") {
			return nil, fmt.Errorf("expected RESP bulk string, got %q", bulkHeader)
		}
		size, err := strconv.Atoi(strings.TrimPrefix(bulkHeader, "$"))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(reader, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func (f *responsesWSFakeRedis) handleCommand(conn net.Conn, args []string) {
	command := strings.ToLower(args[0])
	f.recordCommand(command)
	switch command {
	case "hello":
		_, _ = io.WriteString(conn, "*14\r\n$6\r\nserver\r\n$5\r\nredis\r\n$7\r\nversion\r\n$5\r\n7.2.0\r\n$5\r\nproto\r\n:2\r\n$2\r\nid\r\n:1\r\n$4\r\nmode\r\n$10\r\nstandalone\r\n$4\r\nrole\r\n$6\r\nmaster\r\n$7\r\nmodules\r\n*0\r\n")
	case "ping":
		_, _ = io.WriteString(conn, "+PONG\r\n")
	case "client", "select":
		_, _ = io.WriteString(conn, "+OK\r\n")
	case "incr":
		if len(args) < 2 {
			_, _ = io.WriteString(conn, "-ERR missing key\r\n")
			return
		}
		value := f.incr(args[1])
		_, _ = fmt.Fprintf(conn, ":%d\r\n", value)
	case "decr":
		if len(args) < 2 {
			_, _ = io.WriteString(conn, "-ERR missing key\r\n")
			return
		}
		value := f.decr(args[1])
		_, _ = fmt.Fprintf(conn, ":%d\r\n", value)
	case "expire":
		if len(args) < 3 {
			_, _ = io.WriteString(conn, "-ERR missing args\r\n")
			return
		}
		seconds, _ := strconv.Atoi(args[2])
		if f.expire(args[1], time.Duration(seconds)*time.Second) {
			_, _ = io.WriteString(conn, ":1\r\n")
		} else {
			_, _ = io.WriteString(conn, ":0\r\n")
		}
	case "del":
		deleted := f.del(args[1:]...)
		_, _ = fmt.Fprintf(conn, ":%d\r\n", deleted)
	default:
		_, _ = io.WriteString(conn, "+OK\r\n")
	}
}

func (f *responsesWSFakeRedis) recordCommand(command string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
}

func (f *responsesWSFakeRedis) recordError(err error) {
	if err == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, err.Error())
}

func (f *responsesWSFakeRedis) debugState() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf("commands=%v errors=%v values=%v", f.commands, f.errors, f.values)
}

func (f *responsesWSFakeRedis) incr(key string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupExpiredLocked()
	f.values[key]++
	return f.values[key]
}

func (f *responsesWSFakeRedis) decr(key string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupExpiredLocked()
	f.values[key]--
	return f.values[key]
}

func (f *responsesWSFakeRedis) expire(key string, ttl time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ttl <= 0 {
		delete(f.values, key)
		delete(f.expires, key)
		return true
	}
	if _, ok := f.values[key]; !ok {
		delete(f.expires, key)
		return false
	}
	f.expires[key] = time.Now().Add(ttl)
	return true
}

func (f *responsesWSFakeRedis) del(keys ...string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var deleted int64
	for _, key := range keys {
		if _, ok := f.values[key]; ok {
			deleted++
		}
		delete(f.values, key)
		delete(f.expires, key)
	}
	return deleted
}

func (f *responsesWSFakeRedis) value(key string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupExpiredLocked()
	return f.values[key]
}

func (f *responsesWSFakeRedis) valuePrefix(prefix string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupExpiredLocked()
	var total int64
	for key, value := range f.values {
		if strings.HasPrefix(key, prefix) {
			total += value
		}
	}
	return total
}

func (f *responsesWSFakeRedis) expirePrefix(prefix string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.values {
		if strings.HasPrefix(key, prefix) {
			delete(f.values, key)
			delete(f.expires, key)
		}
	}
}

func (f *responsesWSFakeRedis) cleanupExpiredLocked() {
	now := time.Now()
	for key, expiresAt := range f.expires {
		if !expiresAt.IsZero() && !expiresAt.After(now) {
			delete(f.values, key)
			delete(f.expires, key)
		}
	}
}

func useResponsesWSFakeRedis(t *testing.T) *responsesWSFakeRedis {
	t.Helper()
	fake := startResponsesWSFakeRedis(t)
	previousClient := commonredis.RDB
	previousEnabled := config.RedisEnabled
	client := redis.NewClient(&redis.Options{Addr: fake.addr(), Protocol: 2, DisableIdentity: true})
	commonredis.RDB = client
	config.RedisEnabled = true
	t.Cleanup(func() {
		_ = client.Close()
		commonredis.RDB = previousClient
		config.RedisEnabled = previousEnabled
	})
	return fake
}

func newResponsesWSCapacityTestContext(tokenID int, userID int, group string) *gin.Context {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_id", tokenID)
	ctx.Set("id", userID)
	ctx.Set("group", group)
	return ctx
}

func TestAllowResponsesWSConnectionAttemptLimitsSameToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	resetResponsesWSConnectionLimiterForTest(t)
	setRedisEnabledForTest(t, false)
	setViperForTest(t, "responses_ws.connect_per_credential_per_minute", 2)

	ctx := newResponsesWSCapacityTestContext(101, 7, "default")
	if apiErr := AllowResponsesWSConnectionAttempt(ctx); apiErr != nil {
		t.Fatalf("expected first connection attempt to pass, got %v", apiErr)
	}
	if apiErr := AllowResponsesWSConnectionAttempt(ctx); apiErr != nil {
		t.Fatalf("expected second connection attempt to pass, got %v", apiErr)
	}
	apiErr := AllowResponsesWSConnectionAttempt(ctx)
	if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Code != "responses_ws_connection_rate_limited" {
		t.Fatalf("expected third connection attempt to be rate limited, got %#v", apiErr)
	}
}

func TestAllowResponsesWSConnectionAttemptDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	resetResponsesWSConnectionLimiterForTest(t)
	setRedisEnabledForTest(t, false)
	setViperForTest(t, "responses_ws.connect_per_credential_per_minute", -1)

	ctx := newResponsesWSCapacityTestContext(101, 7, "default")
	for i := 0; i < 10; i++ {
		if apiErr := AllowResponsesWSConnectionAttempt(ctx); apiErr != nil {
			t.Fatalf("expected disabled connection limiter to pass attempt %d, got %v", i+1, apiErr)
		}
	}
}

func TestAllowResponsesWSConnectionAttemptCredentialIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	resetResponsesWSConnectionLimiterForTest(t)
	setRedisEnabledForTest(t, false)
	setViperForTest(t, "responses_ws.connect_per_credential_per_minute", 1)

	tokenA := newResponsesWSCapacityTestContext(101, 7, "default")
	tokenB := newResponsesWSCapacityTestContext(102, 7, "default")
	userOnly := newResponsesWSCapacityTestContext(0, 7, "default")
	authNamespace := newResponsesWSCapacityTestContext(0, 0, "default")
	authNamespace.Request.Header.Set("Authorization", "Bearer sk-auth-a")

	for name, ctx := range map[string]*gin.Context{
		"token-a":        tokenA,
		"token-b":        tokenB,
		"user-only":      userOnly,
		"auth-namespace": authNamespace,
	} {
		if apiErr := AllowResponsesWSConnectionAttempt(ctx); apiErr != nil {
			t.Fatalf("expected isolated %s first attempt to pass, got %v", name, apiErr)
		}
	}
	if apiErr := AllowResponsesWSConnectionAttempt(tokenA); apiErr == nil {
		t.Fatalf("expected token-a second attempt to be limited")
	}
	if apiErr := AllowResponsesWSConnectionAttempt(tokenB); apiErr == nil {
		t.Fatalf("expected token-b second attempt to be limited independently")
	}
	if apiErr := AllowResponsesWSConnectionAttempt(userOnly); apiErr == nil {
		t.Fatalf("expected user-only second attempt to be limited independently")
	}
	if apiErr := AllowResponsesWSConnectionAttempt(authNamespace); apiErr == nil {
		t.Fatalf("expected auth namespace second attempt to be limited independently")
	}
}

func TestAllowResponsesWSConnectionAttemptAnonymousBucketShared(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	resetResponsesWSConnectionLimiterForTest(t)
	setRedisEnabledForTest(t, false)
	setViperForTest(t, "responses_ws.connect_per_credential_per_minute", 1)
	setViperBoolForTest(t, "responses_ws.allow_anonymous_capacity_bucket", true)

	anonymousA := newResponsesWSCapacityTestContext(0, 0, "default")
	anonymousB := newResponsesWSCapacityTestContext(0, 0, "default")
	if apiErr := AllowResponsesWSConnectionAttempt(anonymousA); apiErr != nil {
		t.Fatalf("expected first anonymous attempt to pass, got %v", apiErr)
	}
	apiErr := AllowResponsesWSConnectionAttempt(anonymousB)
	if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected second anonymous request to share and exhaust anonymous bucket, got %#v", apiErr)
	}
}

func TestAllowResponsesWSConnectionAttemptDoesNotConsumeAPIRPM(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	resetResponsesWSConnectionLimiterForTest(t)
	setRedisEnabledForTest(t, false)
	setViperForTest(t, "responses_ws.connect_per_credential_per_minute", 1)

	originalAPILimiter := model.GlobalUserGroupRatio.APILimiter
	model.GlobalUserGroupRatio.Lock()
	model.GlobalUserGroupRatio.APILimiter = map[string]ratelimit.RateLimiter{
		"default": ratelimit.NewMemoryLimiter(1, 1, time.Minute, false),
	}
	model.GlobalUserGroupRatio.Unlock()
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.APILimiter = originalAPILimiter
		model.GlobalUserGroupRatio.Unlock()
	})

	ctx := newResponsesWSCapacityTestContext(101, 7, "default")
	if apiErr := AllowResponsesWSConnectionAttempt(ctx); apiErr != nil {
		t.Fatalf("expected handshake limiter to pass, got %v", apiErr)
	}
	if apiErr := AllowCurrentUserRequest(ctx); apiErr != nil {
		t.Fatalf("expected first API RPM allowance to remain available after handshake limiter, got %v", apiErr)
	}
	if apiErr := AllowCurrentUserRequest(ctx); apiErr == nil {
		t.Fatal("expected second API RPM allowance to be exhausted by turn-level accounting")
	}
}

func TestAllowCurrentUserRequestRejectsMissingUserID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	originalAPILimiter := model.GlobalUserGroupRatio.APILimiter
	model.GlobalUserGroupRatio.Lock()
	model.GlobalUserGroupRatio.APILimiter = map[string]ratelimit.RateLimiter{
		"default": ratelimit.NewMemoryLimiter(1, 1, time.Minute, false),
	}
	model.GlobalUserGroupRatio.Unlock()
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.APILimiter = originalAPILimiter
		model.GlobalUserGroupRatio.Unlock()
	})

	ctx := newResponsesWSCapacityTestContext(101, 0, "default")
	apiErr := AllowCurrentUserRequest(ctx)
	if apiErr == nil || apiErr.Code != "invalid_user" || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected missing user id to be rejected before rate limit key construction, got %#v", apiErr)
	}
}

func TestAllowResponsesWSConnectionAttemptRedisUnavailableFallsBackInProcess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	resetResponsesWSConnectionLimiterForTest(t)
	setRedisEnabledForTest(t, true)
	setViperForTest(t, "responses_ws.connect_per_credential_per_minute", 1)

	ctx := newResponsesWSCapacityTestContext(101, 7, "default")
	if apiErr := AllowResponsesWSConnectionAttempt(ctx); apiErr != nil {
		t.Fatalf("expected Redis-unavailable first attempt to fall back in-process, got %v", apiErr)
	}
	apiErr := AllowResponsesWSConnectionAttempt(ctx)
	if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected in-process fallback to enforce the local limit, got %#v", apiErr)
	}
}

func TestAcquireResponsesWSPendingSlotReleaseIsIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponsesWSCapacityForTest(t)
	setViperForTest(t, "responses_ws.pending_per_credential", 1)

	ctx := newResponsesWSCapacityTestContext(101, 7, "default")
	lease, apiErr := AcquireResponsesWSPendingSlot(ctx)
	if apiErr != nil {
		t.Fatalf("expected first pending slot acquisition to pass, got %v", apiErr)
	}
	if _, apiErr := AcquireResponsesWSPendingSlot(ctx); apiErr == nil {
		t.Fatalf("expected second pending slot for same credential to be rejected")
	}

	lease.Release()
	lease.Release()

	lease, apiErr = AcquireResponsesWSPendingSlot(ctx)
	if apiErr != nil {
		t.Fatalf("expected idempotent release to free pending slot, got %v", apiErr)
	}
	lease.Release()
}

func TestAcquireResponsesWSPendingSlotUnlimited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponsesWSCapacityForTest(t)
	setViperForTest(t, "responses_ws.pending_per_credential", -1)

	ctx := newResponsesWSCapacityTestContext(101, 7, "default")
	leases := make([]ResponsesWSLease, 0, 16)
	for i := 0; i < 16; i++ {
		lease, apiErr := AcquireResponsesWSPendingSlot(ctx)
		if apiErr != nil {
			t.Fatalf("expected -1 to disable pending limit, got %v at attempt %d", apiErr, i+1)
		}
		leases = append(leases, lease)
	}
	for _, lease := range leases {
		lease.Release()
	}
}

func TestAcquireResponsesWSActiveLeaseConcurrentCredentialLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponsesWSCapacityForTest(t)
	setViperForTest(t, "responses_ws.active_per_credential", 2)
	setViperForTest(t, "responses_ws.active_per_group", -1)
	setViperForTest(t, "responses_ws.active_global", -1)

	const workers = 16
	var successes int32
	leases := make(chan ResponsesWSLease, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := newResponsesWSCapacityTestContext(303, 7, "default")
			lease, apiErr := AcquireResponsesWSActiveLease(ctx)
			if apiErr != nil {
				return
			}
			atomic.AddInt32(&successes, 1)
			leases <- lease
		}()
	}
	wg.Wait()
	close(leases)

	if got := atomic.LoadInt32(&successes); got != 2 {
		t.Fatalf("expected concurrent active credential limit to allow exactly 2 leases, got %d", got)
	}
	for lease := range leases {
		lease.Release()
	}

	ctx := newResponsesWSCapacityTestContext(303, 7, "default")
	lease, apiErr := AcquireResponsesWSActiveLease(ctx)
	if apiErr != nil {
		t.Fatalf("expected released concurrent leases to free capacity, got %v", apiErr)
	}
	lease.Release()
}

func TestAcquireResponsesWSActiveLeaseRejectsAnonymousByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponsesWSCapacityForTest(t)
	setViperBoolForTest(t, "responses_ws.allow_anonymous_capacity_bucket", false)

	ctx := newResponsesWSCapacityTestContext(0, 0, "")
	if lease, apiErr := AcquireResponsesWSActiveLease(ctx); apiErr == nil {
		if lease != nil {
			lease.Release()
		}
		t.Fatal("expected anonymous active lease to be rejected by default")
	}
}

func TestAcquireResponsesWSActiveLeaseAllowsAnonymousWhenConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponsesWSCapacityForTest(t)
	setViperBoolForTest(t, "responses_ws.allow_anonymous_capacity_bucket", true)

	ctx := newResponsesWSCapacityTestContext(0, 0, "")
	lease, apiErr := AcquireResponsesWSActiveLease(ctx)
	if apiErr != nil {
		t.Fatalf("expected explicit anonymous capacity bucket to be allowed, got %v", apiErr)
	}
	lease.Release()
}

func TestAcquireResponsesWSActiveLeaseHoldsCapacityUntilRelease(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponsesWSCapacityForTest(t)
	setViperForTest(t, "responses_ws.active_per_credential", 1)
	setViperForTest(t, "responses_ws.active_per_group", 0)
	setViperForTest(t, "responses_ws.active_global", 0)

	ctx := newResponsesWSCapacityTestContext(202, 7, "default")
	lease, apiErr := AcquireResponsesWSActiveLease(ctx)
	if apiErr != nil {
		t.Fatalf("expected first active lease acquisition to pass, got %v", apiErr)
	}
	if _, apiErr := AcquireResponsesWSActiveLease(ctx); apiErr == nil {
		t.Fatalf("expected active lease to remain held until explicit release")
	}

	lease.Release()
	lease.Release()

	lease, apiErr = AcquireResponsesWSActiveLease(ctx)
	if apiErr != nil {
		t.Fatalf("expected idempotent release to free active lease, got %v", apiErr)
	}
	lease.Release()
}

func TestAcquireResponsesWSActiveLeaseUsesRedisSharedCounters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	resetResponsesWSCapacityForTest(t)
	fakeRedis := useResponsesWSFakeRedis(t)
	setViperForTest(t, "responses_ws.active_per_credential", 1)
	setViperForTest(t, "responses_ws.active_per_group", -1)
	setViperForTest(t, "responses_ws.active_global", -1)

	ctx := newResponsesWSCapacityTestContext(909, 7, "default")
	lease, apiErr := AcquireResponsesWSActiveLease(ctx)
	if apiErr != nil {
		t.Fatalf("expected first Redis active lease to pass, got %v", apiErr)
	}
	if got := fakeRedis.valuePrefix(responsesWSActiveLeaseKeyPrefix); got == 0 {
		t.Fatalf("expected Redis active lease counters to be used; fake redis state: %s", fakeRedis.debugState())
	}
	if _, apiErr := AcquireResponsesWSActiveLease(ctx); apiErr == nil || apiErr.Code != "responses_ws_active_credential_limit_exceeded" {
		t.Fatalf("expected Redis shared credential counter to reject second lease, got %#v", apiErr)
	}

	lease.Release()
	lease.Release()
	if got := fakeRedis.value(responsesWSActiveLeaseKeyPrefix + "credential:token:909"); got != 0 {
		t.Fatalf("expected idempotent release to clear Redis active credential counter, got %d", got)
	}

	lease, apiErr = AcquireResponsesWSActiveLease(ctx)
	if apiErr != nil {
		t.Fatalf("expected Redis active lease to be available after release, got %v", apiErr)
	}
	lease.Release()
}

func TestAcquireResponsesWSActiveLeaseRedisTTLExpiryRecoversCapacity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	resetResponsesWSCapacityForTest(t)
	fakeRedis := useResponsesWSFakeRedis(t)
	setViperForTest(t, "responses_ws.active_per_credential", 1)
	setViperForTest(t, "responses_ws.active_per_group", -1)
	setViperForTest(t, "responses_ws.active_global", -1)

	ctx := newResponsesWSCapacityTestContext(910, 7, "default")
	firstLease, apiErr := AcquireResponsesWSActiveLease(ctx)
	if apiErr != nil {
		t.Fatalf("expected first Redis active lease to pass, got %v", apiErr)
	}
	if got := fakeRedis.valuePrefix(responsesWSActiveLeaseKeyPrefix); got == 0 {
		t.Fatalf("expected Redis active lease counters to be used; fake redis state: %s", fakeRedis.debugState())
	}
	fakeRedis.expirePrefix(responsesWSActiveLeaseKeyPrefix)

	secondLease, apiErr := AcquireResponsesWSActiveLease(ctx)
	if apiErr != nil {
		t.Fatalf("expected expired Redis active lease key to recover capacity, got %v", apiErr)
	}
	secondLease.Release()
	firstLease.Release()
}

func TestAcquireResponsesWSActiveLeaseSignalsLossWhenRedisLeaseDisappears(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	resetResponsesWSCapacityForTest(t)
	fakeRedis := useResponsesWSFakeRedis(t)
	setResponsesWSActiveLeaseTTLForTest(t, 90*time.Millisecond)
	setViperForTest(t, "responses_ws.active_per_credential", 1)
	setViperForTest(t, "responses_ws.active_per_group", -1)
	setViperForTest(t, "responses_ws.active_global", -1)

	ctx := newResponsesWSCapacityTestContext(911, 7, "default")
	lease, apiErr := AcquireResponsesWSActiveLease(ctx)
	if apiErr != nil {
		t.Fatalf("expected Redis active lease to be acquired, got %v", apiErr)
	}
	lost := lease.Lost()
	if lost == nil {
		t.Fatal("expected Redis-backed lease to expose a loss channel")
	}

	fakeRedis.expirePrefix(responsesWSActiveLeaseKeyPrefix)

	select {
	case <-lost:
	case <-time.After(time.Second):
		t.Fatal("expected lease loss to be signaled after Redis lease expiry")
	}

	lease.Release()
}

func TestAcquireResponsesWSActiveLeaseRedisUnavailableFailOpenUsesLocalLimiter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	resetResponsesWSCapacityForTest(t)
	setRedisEnabledForTest(t, true)
	setViperBoolForTest(t, "responses_ws.active_lease_redis_fail_open", true)
	setViperForTest(t, "responses_ws.active_per_credential", 1)
	setViperForTest(t, "responses_ws.active_per_group", -1)
	setViperForTest(t, "responses_ws.active_global", -1)

	previousRedisClient := commonredis.RDB
	commonredis.RDB = nil
	t.Cleanup(func() {
		commonredis.RDB = previousRedisClient
	})

	ctx := newResponsesWSCapacityTestContext(912, 7, "default")
	lease, apiErr := AcquireResponsesWSActiveLease(ctx)
	if apiErr != nil {
		t.Fatalf("expected Redis-unavailable active lease to fail open into local limiter, got %v", apiErr)
	}
	if lease.Lost() != nil {
		t.Fatal("expected fail-open local active lease not to expose a Redis loss channel")
	}
	if _, apiErr := AcquireResponsesWSActiveLease(ctx); apiErr == nil || apiErr.Code != "responses_ws_active_credential_limit_exceeded" {
		t.Fatalf("expected local fallback limiter to enforce credential capacity, got %#v", apiErr)
	}
	lease.Release()
}

func TestAcquireResponsesWSActiveLeaseRedisUnavailableFailClosedRejects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	resetResponsesWSCapacityForTest(t)
	setRedisEnabledForTest(t, true)
	setViperBoolForTest(t, "responses_ws.active_lease_redis_fail_open", false)
	setViperForTest(t, "responses_ws.active_per_credential", 1)
	setViperForTest(t, "responses_ws.active_per_group", -1)
	setViperForTest(t, "responses_ws.active_global", -1)

	previousRedisClient := commonredis.RDB
	commonredis.RDB = nil
	t.Cleanup(func() {
		commonredis.RDB = previousRedisClient
	})

	ctx := newResponsesWSCapacityTestContext(913, 7, "default")
	lease, apiErr := AcquireResponsesWSActiveLease(ctx)
	if lease != nil {
		lease.Release()
	}
	if apiErr == nil || apiErr.StatusCode != http.StatusServiceUnavailable || apiErr.Code != "responses_ws_active_lease_backend_unavailable" {
		t.Fatalf("expected fail-closed Redis-unavailable active lease rejection, got %#v", apiErr)
	}
}
