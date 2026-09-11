package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common/requester"
)

// responsesHTTPIO 只拥有本次 HTTP 交付的停止和时限，不观察业务终态。
type responsesHTTPIO struct {
	ctx         context.Context
	cancel      context.CancelFunc
	writer      gin.ResponseWriter
	controller  *http.ResponseController
	idle        time.Duration
	deadline    time.Time
	mu          sync.Mutex
	stopped     bool
	closed      bool
	stopContext func() bool
}

func newResponsesHTTPIO(c *gin.Context) (*responsesHTTPIO, error) {
	idle, lifetime := requester.LongStreamTimeouts()
	writer := http.ResponseWriter(c.Writer)
	for {
		wrapped, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		writer = wrapped.Unwrap()
	}
	controller := http.NewResponseController(writer)
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), lifetime)
	deadline, _ := ctx.Deadline()
	owner := &responsesHTTPIO{ctx: ctx, cancel: cancel, writer: c.Writer, controller: controller, idle: idle, deadline: deadline}
	owner.stopContext = context.AfterFunc(ctx, owner.Stop)
	return owner, nil
}

func (owner *responsesHTTPIO) Stop() {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.closed {
		return
	}
	owner.stopped = true
	owner.cancel()
	_ = owner.controller.SetWriteDeadline(time.Now())
}

func (owner *responsesHTTPIO) arm() error {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.stopped || owner.closed {
		return context.Canceled
	}
	if err := owner.ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(owner.idle)
	if deadline.After(owner.deadline) {
		deadline = owner.deadline
	}
	return owner.controller.SetWriteDeadline(deadline)
}

func (owner *responsesHTTPIO) WriteEvent(raw string) error {
	if err := owner.arm(); err != nil {
		return err
	}
	if _, err := io.WriteString(owner.writer, raw); err != nil {
		owner.Stop()
		return err
	}
	if err := owner.arm(); err != nil {
		return err
	}
	owner.writer.WriteHeaderNow()
	if err := owner.controller.Flush(); err != nil {
		owner.Stop()
		return err
	}
	return nil
}

func (owner *responsesHTTPIO) WatchStream(stream requester.StreamReaderInterface[string]) func() {
	ctx := requester.StreamReadContext(stream)
	if ctx == nil {
		return func() {}
	}
	stop := context.AfterFunc(ctx, func() {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, io.EOF) {
			owner.Stop()
		}
	})
	return func() { stop() }
}

func (owner *responsesHTTPIO) Close() {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.closed = true
	owner.stopContext()
	owner.cancel()
	if !owner.stopped {
		_ = owner.controller.SetWriteDeadline(time.Time{})
	}
}
