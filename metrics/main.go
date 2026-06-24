package metrics

import (
	"strconv"
	"sync"
	"time"

	"one-api/common/config"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	httpRequestsTotal                *prometheus.CounterVec
	httpRequestDuration              *prometheus.HistogramVec
	providerCounter                  *prometheus.CounterVec
	panicCounter                     *prometheus.CounterVec
	requestBodyDecodeCounter         *prometheus.CounterVec
	requestBodyDecodedBytes          *prometheus.HistogramVec
	responsesWSConnectLimit          *prometheus.CounterVec
	responsesWSRedisFallback         *prometheus.CounterVec
	responsesWSEventPostTimeout      *prometheus.CounterVec
	responsesWSPreconsumeForced      *prometheus.CounterVec
	responsesWSPreconsumeLatency     *prometheus.HistogramVec
	responsesWSPreconsumeFloor       *prometheus.HistogramVec
	responsesWSPreconsumeSettle      *prometheus.CounterVec
	responsesWSSettlementConflict    *prometheus.CounterVec
	responsesWSAttemptReplayDecision *prometheus.CounterVec
	responsesWSAttemptReplayExecuted *prometheus.CounterVec
	responsesWSAttemptReplayBlocked  *prometheus.CounterVec
	usageObservedUnbilled            *prometheus.CounterVec
	requestBodyDecodeOnce            sync.Once
)

func requestBodyDecodedBytesBuckets() []float64 {
	return buildRequestBodyDecodedBytesBuckets(config.RequestBodyDecodeMaxDecodedBytes)
}

func buildRequestBodyDecodedBytesBuckets(maxDecodedBytes int64) []float64 {
	const (
		start  = 512.0
		factor = 2.0
	)

	buckets := []float64{start}
	for buckets[len(buckets)-1] < float64(maxDecodedBytes) {
		buckets = append(buckets, buckets[len(buckets)-1]*factor)
	}
	return buckets
}

func init() {
	// 1. 监控请求
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of http requests.",
		},
		[]string{"method", "path", "code"},
	)
	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Duration of HTTP requests in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path", "code"},
	)

	// 2. 监控渠道
	providerCounter = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "provider_requests_total",
			Help: "Total number of provider requests.",
		},
		[]string{"channel_type", "channel_id", "model", "type"},
	)

	// 3. 监控 panic
	panicCounter = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "app_panics_total",
			Help: "Total number of panics in the application.",
		},
		[]string{"type"},
	)

	requestBodyDecodeCounter = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_request_body_decode_total",
			Help: "Total number of request body decode attempts.",
		},
		[]string{"encoding", "outcome"},
	)
	responsesWSConnectLimit = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "responses_ws_connection_rate_limited_total",
			Help: "Total number of Responses WebSocket connection attempts rejected by the connection rate limiter.",
		},
		[]string{"group", "credential_kind"},
	)
	responsesWSRedisFallback = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "responses_ws_connection_limiter_redis_fallback_total",
			Help: "Total number of Responses WebSocket connection limiter fail-open fallbacks from Redis to in-process storage.",
		},
		[]string{"reason"},
	)
	responsesWSEventPostTimeout = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "responses_ws_event_post_timeout_total",
			Help: "Total number of bounded reliable Responses WebSocket actor event post timeouts.",
		},
		[]string{"event_type"},
	)
	responsesWSPreconsumeForced = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "responses_ws_preconsume_forced_total",
			Help: "Total number of Responses WebSocket attempts that force quota pre-consumption.",
		},
		[]string{"outcome"},
	)
	responsesWSPreconsumeLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "responses_ws_preconsume_latency_ms",
			Help:    "Latency of Responses WebSocket forced quota pre-consumption in milliseconds.",
			Buckets: []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000},
		},
		[]string{"outcome"},
	)
	responsesWSPreconsumeFloor = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "responses_ws_preconsume_floor_quota",
			Help:    "Pre-consumed floor quota for Responses WebSocket attempts.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 18),
		},
		[]string{"outcome"},
	)
	responsesWSPreconsumeSettle = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "responses_ws_preconsume_settlement_total",
			Help: "Total number of Responses WebSocket pre-consume reserve settlements.",
		},
		[]string{"action"},
	)
	responsesWSSettlementConflict = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "responses_ws_settlement_conflict_total",
			Help: "Total number of Responses WebSocket settlement evidence conflicts.",
		},
		[]string{"kind"},
	)
	responsesWSAttemptReplayDecision = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "responses_ws_attempt_replay_decision_total",
			Help: "Total number of Responses WebSocket attempt replay decisions.",
		},
		[]string{"decision", "origin", "status", "barrier", "failure"},
	)
	responsesWSAttemptReplayExecuted = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "responses_ws_attempt_replay_executed_total",
			Help: "Total number of Responses WebSocket attempt replays executed after rollback.",
		},
		[]string{"origin", "status", "failure"},
	)
	responsesWSAttemptReplayBlocked = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "responses_ws_attempt_replay_blocked_total",
			Help: "Total number of Responses WebSocket attempt replay decisions blocked by a barrier.",
		},
		[]string{"barrier", "origin", "status"},
	)
	usageObservedUnbilled = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "usage_observed_unbilled",
			Help: "Total number of provider usage observations intentionally excluded from settlement because pricing is not configured.",
		},
		[]string{"source", "model"},
	)
}

func InitRequestBodyDecodeMetrics() {
	requestBodyDecodeOnce.Do(func() {
		// Trade-off: only this histogram is initialized after config load because
		// its bucket layout depends on the runtime decode limit. Keeping the rest
		// in package init avoids a broader metrics bootstrap refactor.
		requestBodyDecodedBytes = promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "http_request_body_decoded_bytes",
				Help:    "Size of decoded request bodies in bytes.",
				Buckets: requestBodyDecodedBytesBuckets(),
			},
			[]string{"encoding"},
		)
	})
}

// 记录 HTTP 请求
func RecordHttp(c *gin.Context, duration time.Duration) {
	SafelyRecordMetric(func() {
		statusCode := strconv.Itoa(c.Writer.Status())

		httpRequestsTotal.WithLabelValues(
			c.Request.Method,
			c.FullPath(),
			statusCode,
		).Inc()

		httpRequestDuration.WithLabelValues(
			c.Request.Method,
			c.FullPath(),
			statusCode,
		).Observe(duration.Seconds())
	})
}

// 记录渠道请求
func RecordProvider(c *gin.Context, statusCode int) {
	model := c.GetString("original_model")

	if model == "" {
		return
	}

	channelType := c.GetInt("channel_type")
	channelId := c.GetInt("channel_id")

	SafelyRecordMetric(func() {
		providerCounter.WithLabelValues(
			strconv.Itoa(channelType),
			strconv.Itoa(channelId),
			model,
			strconv.Itoa(statusCode),
		).Inc()
	})
}

// 记录 panic
func RecordPanic(panicType string) {
	panicCounter.WithLabelValues(panicType).Inc()
}

func RecordRequestBodyDecode(encoding, outcome string, decodedBytes int) {
	SafelyRecordMetric(func() {
		InitRequestBodyDecodeMetrics()
		requestBodyDecodeCounter.WithLabelValues(encoding, outcome).Inc()
		if outcome == "success" && decodedBytes >= 0 && requestBodyDecodedBytes != nil {
			requestBodyDecodedBytes.WithLabelValues(encoding).Observe(float64(decodedBytes))
		}
	})
}

func RecordResponsesWSConnectionRateLimited(group, credentialKind string) {
	SafelyRecordMetric(func() {
		responsesWSConnectLimit.WithLabelValues(group, credentialKind).Inc()
	})
}

func RecordResponsesWSConnectionLimiterRedisFallback(reason string) {
	SafelyRecordMetric(func() {
		responsesWSRedisFallback.WithLabelValues(reason).Inc()
	})
}

func RecordResponsesWSEventPostTimeout(eventType string) {
	SafelyRecordMetric(func() {
		responsesWSEventPostTimeout.WithLabelValues(eventType).Inc()
	})
}

func RecordResponsesWSPreconsumeForced(outcome string, latency time.Duration, floorQuota int) {
	SafelyRecordMetric(func() {
		responsesWSPreconsumeForced.WithLabelValues(outcome).Inc()
		responsesWSPreconsumeLatency.WithLabelValues(outcome).Observe(float64(latency.Milliseconds()))
		if floorQuota > 0 {
			responsesWSPreconsumeFloor.WithLabelValues(outcome).Observe(float64(floorQuota))
		}
	})
}

func RecordResponsesWSPreconsumeSettlement(action string) {
	SafelyRecordMetric(func() {
		responsesWSPreconsumeSettle.WithLabelValues(action).Inc()
	})
}

func RecordResponsesWSSettlementConflict(kind string) {
	SafelyRecordMetric(func() {
		responsesWSSettlementConflict.WithLabelValues(kind).Inc()
	})
}

func RecordResponsesWSAttemptReplayDecision(decision, origin string, status int, barrier, failure string) {
	SafelyRecordMetric(func() {
		responsesWSAttemptReplayDecision.WithLabelValues(decision, origin, strconv.Itoa(status), barrier, failure).Inc()
	})
}

func RecordResponsesWSAttemptReplayExecuted(origin string, status int, failure string) {
	SafelyRecordMetric(func() {
		responsesWSAttemptReplayExecuted.WithLabelValues(origin, strconv.Itoa(status), failure).Inc()
	})
}

func RecordResponsesWSAttemptReplayBlocked(barrier, origin string, status int) {
	SafelyRecordMetric(func() {
		responsesWSAttemptReplayBlocked.WithLabelValues(barrier, origin, strconv.Itoa(status)).Inc()
	})
}

func RecordUsageObservedUnbilled(source, model string) {
	SafelyRecordMetric(func() {
		usageObservedUnbilled.WithLabelValues(source, model).Inc()
	})
}

func SafelyRecordMetric(f func()) {
	defer func() {
		if r := recover(); r != nil {
			RecordPanic("metrics")
		}
	}()
	f()
}
