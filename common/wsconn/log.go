package wsconn

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"one-api/common/logger"
)

type slowHandleObserver interface {
	Observe(context.Context, time.Duration)
}

type slowHandleLogObserver struct{}

func (slowHandleLogObserver) Observe(ctx context.Context, elapsed time.Duration) {
	logDebugf(ctx, "wsconn: slow Pump.Handle observed: %s", elapsed)
}

var (
	slowHandleObserverMu sync.RWMutex
	slowHandleRecorder   slowHandleObserver = slowHandleLogObserver{}
)

func observeSlowHandle(ctx context.Context, elapsed time.Duration) {
	slowHandleObserverMu.RLock()
	recorder := slowHandleRecorder
	slowHandleObserverMu.RUnlock()
	if recorder != nil {
		recorder.Observe(ctx, elapsed)
	}
}

func logWarnf(ctx context.Context, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if logger.Logger != nil {
		logger.LogWarn(ctx, msg)
		return
	}
	log.Print(msg)
}

func logDebugf(ctx context.Context, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if logger.Logger != nil {
		logger.LogDebug(ctx, msg)
		return
	}
	logger.LogDebug(ctx, msg)
}
