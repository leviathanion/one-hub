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
	Observe(time.Duration)
}

type slowHandleLogObserver struct{}

func (slowHandleLogObserver) Observe(elapsed time.Duration) {
	logWarnf("wsconn: slow Pump.Handle observed: %s", elapsed)
}

var (
	slowHandleObserverMu sync.RWMutex
	slowHandleRecorder   slowHandleObserver = slowHandleLogObserver{}
)

func observeSlowHandle(elapsed time.Duration) {
	slowHandleObserverMu.RLock()
	recorder := slowHandleRecorder
	slowHandleObserverMu.RUnlock()
	if recorder != nil {
		recorder.Observe(elapsed)
	}
}

func logWarnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if logger.Logger != nil {
		logger.LogWarn(context.Background(), msg)
		return
	}
	log.Print(msg)
}
