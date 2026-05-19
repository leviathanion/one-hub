package model

import (
	"testing"

	"one-api/common/limit"
	"one-api/common/logger"

	"go.uber.org/zap"
)

type stoppingRateLimiter struct {
	stopped     bool
	panicOnStop bool
}

func (l *stoppingRateLimiter) Allow(string) bool {
	return true
}

func (l *stoppingRateLimiter) AllowN(string, int) bool {
	return true
}

func (l *stoppingRateLimiter) GetCurrentRate(string) (int, error) {
	return 0, nil
}

func (l *stoppingRateLimiter) Stop() {
	l.stopped = true
	if l.panicOnStop {
		panic("stop failed")
	}
}

func TestStopUserGroupAPILimitersRecoversAndContinues(t *testing.T) {
	logger.Logger = zap.NewNop()
	panicLimiter := &stoppingRateLimiter{panicOnStop: true}
	nextLimiter := &stoppingRateLimiter{}

	limiters := map[string]limit.RateLimiter{
		"panic": panicLimiter,
		"next":  nextLimiter,
	}

	stopUserGroupAPILimiters(limiters)

	if !panicLimiter.stopped {
		t.Fatal("expected panicking limiter stop to be attempted")
	}
	if !nextLimiter.stopped {
		t.Fatal("expected limiter cleanup to continue after a stop panic")
	}
}
