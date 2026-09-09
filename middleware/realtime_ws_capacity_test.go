package middleware

import (
	"net/http"
	"testing"

	commonredis "one-api/common/redis"

	"github.com/redis/go-redis/v9"
)

func resetRealtimeConnectionLimiterForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		realtimeWSConnectionLimiter.Lock()
		defer realtimeWSConnectionLimiter.Unlock()
		stopResponsesWSLimiter(realtimeWSConnectionLimiter.limiter)
		stopResponsesWSLimiter(realtimeWSConnectionLimiter.fallbackLimiter)
		realtimeWSConnectionLimiter.limiter = nil
		realtimeWSConnectionLimiter.fallbackLimiter = nil
		realtimeWSConnectionLimiter.warnedInProcess = false
		realtimeWSConnectionLimiter.warnedRedisFallback = false
	}
	reset()
	t.Cleanup(reset)
}

func TestRealtimeConnectionLimitSharesUserAcrossTokensAndKeepsNamespacesSeparate(t *testing.T) {
	resetRealtimeConnectionLimiterForTest(t)
	resetResponsesWSConnectionLimiterForTest(t)
	setRedisEnabledForTest(t, false)
	setViperForTest(t, "realtime_ws.connect_per_user_per_minute", 1)
	setViperForTest(t, "responses_ws.connect_per_credential_per_minute", 1)
	first := newResponsesWSCapacityTestContext(101, 7, "default")
	if apiErr := AllowRealtimeConnectionAttempt(first); apiErr != nil {
		t.Fatalf("first connection: %v", apiErr)
	}
	second := newResponsesWSCapacityTestContext(102, 7, "default")
	apiErr := AllowRealtimeConnectionAttempt(second)
	if apiErr == nil || apiErr.Code != "realtime_ws_connection_rate_limited" || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("token change bypassed the user bucket: %#v", apiErr)
	}
	if apiErr := AllowRealtimeConnectionAttempt(newResponsesWSCapacityTestContext(103, 8, "default")); apiErr != nil {
		t.Fatalf("other user must have its own bucket: %v", apiErr)
	}
	if apiErr := AllowResponsesWSConnectionAttempt(first); apiErr != nil {
		t.Fatalf("realtime consumed Responses bucket: %v", apiErr)
	}
}

func TestRealtimeConnectionLimitRedisSharesExactUserKey(t *testing.T) {
	resetRealtimeConnectionLimiterForTest(t)
	fake := useResponsesWSFakeRedis(t)
	setViperForTest(t, "realtime_ws.connect_per_user_per_minute", 1)
	ctx := newResponsesWSCapacityTestContext(101, 7, "default")
	if apiErr := AllowRealtimeConnectionAttempt(ctx); apiErr != nil {
		t.Fatal(apiErr)
	}
	if got := fake.value("{realtime-ws-connect:user:7}:count"); got != 1 {
		t.Fatalf("persistent user bucket count = %d; %s", got, fake.debugState())
	}
	// 新的本地 limiter 实例仍读取相同 Redis 桶。
	otherInstance := websocketConnectionLimiterState{name: "RealtimeWS"}
	t.Cleanup(func() {
		stopResponsesWSLimiter(otherInstance.limiter)
		stopResponsesWSLimiter(otherInstance.fallbackLimiter)
	})
	if otherInstance.allow(1, "realtime-ws-connect:user:7") {
		t.Fatal("second instance bypassed the shared bucket")
	}
}

func TestRealtimeConnectionLimitRedisUnavailableHasBoundedFallback(t *testing.T) {
	resetRealtimeConnectionLimiterForTest(t)
	setRedisEnabledForTest(t, true)
	previous := commonredis.RDB
	commonredis.RDB = nil
	t.Cleanup(func() { commonredis.RDB = previous })
	setViperForTest(t, "realtime_ws.connect_per_user_per_minute", 1)
	ctx := newResponsesWSCapacityTestContext(101, 7, "default")
	if apiErr := AllowRealtimeConnectionAttempt(ctx); apiErr != nil {
		t.Fatalf("first local fallback attempt: %v", apiErr)
	}
	if apiErr := AllowRealtimeConnectionAttempt(ctx); apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("Redis failure removed connection protection: %#v", apiErr)
	}
}

func TestRealtimeConnectionLimitRedisCommandFailureHasBoundedFallback(t *testing.T) {
	resetRealtimeConnectionLimiterForTest(t)
	setRedisEnabledForTest(t, true)
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = client.Close()
	previous := commonredis.RDB
	commonredis.RDB = client
	t.Cleanup(func() { commonredis.RDB = previous })
	setViperForTest(t, "realtime_ws.connect_per_user_per_minute", 1)
	ctx := newResponsesWSCapacityTestContext(101, 7, "default")
	if apiErr := AllowRealtimeConnectionAttempt(ctx); apiErr != nil {
		t.Fatalf("first command-error fallback attempt: %v", apiErr)
	}
	if apiErr := AllowRealtimeConnectionAttempt(ctx); apiErr == nil || apiErr.Code != "realtime_ws_connection_rate_limited" {
		t.Fatalf("Redis command failure bypassed local rate limit: %#v", apiErr)
	}
}
