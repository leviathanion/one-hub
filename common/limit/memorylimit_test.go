package limit

import (
	"testing"
	"time"
)

func TestMemoryLimiterStopIsIdempotentAndNilSafe(t *testing.T) {
	var nilLimiter *MemoryLimiter
	nilLimiter.Stop()

	limiter := NewMemoryLimiter(1, 1, time.Second, false)
	limiter.Stop()
	limiter.Stop()
}
