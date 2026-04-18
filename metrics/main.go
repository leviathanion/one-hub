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
	httpRequestsTotal        *prometheus.CounterVec
	httpRequestDuration      *prometheus.HistogramVec
	providerCounter          *prometheus.CounterVec
	panicCounter             *prometheus.CounterVec
	requestBodyDecodeCounter *prometheus.CounterVec
	requestBodyDecodedBytes  *prometheus.HistogramVec
	requestBodyDecodeOnce    sync.Once
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

func SafelyRecordMetric(f func()) {
	defer func() {
		if r := recover(); r != nil {
			RecordPanic("metrics")
		}
	}()
	f()
}
