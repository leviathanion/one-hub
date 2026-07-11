package codex

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"

	"one-api/common"
	"one-api/common/logger"
	"one-api/model"
)

const autoRefreshConcurrency = 8

var (
	codexBackgroundWorkerSlots = make(chan struct{}, autoRefreshConcurrency)
	errCodexChannelWorkerPanic = errors.New("codex channel worker panic")
)

func codexWorkerCount(total int) int {
	if total <= 0 {
		return 0
	}
	if total < autoRefreshConcurrency {
		return total
	}
	return autoRefreshConcurrency
}

type codexChannelCursor struct {
	// Loaders return id DESC. boundary is the last ID in the continuously started
	// prefix, not an array offset, so inserts/deletes cannot move the resume point.
	boundary atomic.Int64
}

func (c *codexChannelCursor) rotate(channels []*model.Channel) []*model.Channel {
	if c == nil || len(channels) < 2 {
		return channels
	}
	boundary := c.boundary.Load()
	if boundary <= 0 {
		return channels
	}
	start := 0
	for index, channel := range channels {
		if channel != nil && int64(channel.Id) < boundary {
			start = index
			break
		}
	}
	if start == 0 {
		return channels
	}
	rotated := make([]*model.Channel, 0, len(channels))
	rotated = append(rotated, channels[start:]...)
	rotated = append(rotated, channels[:start]...)
	return rotated
}

func (c *codexChannelCursor) advance(channels []*model.Channel, processedPrefix int) {
	if c == nil || processedPrefix <= 0 || processedPrefix > len(channels) {
		return
	}
	channel := channels[processedPrefix-1]
	if channel != nil && channel.Id > 0 {
		c.boundary.Store(int64(channel.Id))
	}
}

type codexChannelWorkerResult struct {
	Processed     int
	StartedPrefix int
	Skipped       int
	Panicked      int
	Err           error
	FirstPanicErr error
}

func (r codexChannelWorkerResult) FailureCount() int {
	return r.Skipped + r.Panicked
}

func codexChannelWorkerSkippedError(result codexChannelWorkerResult) string {
	if result.Skipped <= 0 {
		return ""
	}
	if result.Err == nil {
		return fmt.Sprintf("channel worker skipped %d channel(s)", result.Skipped)
	}
	return fmt.Sprintf("channel worker canceled: %s; skipped %d channel(s)", result.Err.Error(), result.Skipped)
}

func codexChannelWorkerError(result codexChannelWorkerResult) string {
	parts := make([]string, 0, 2)
	if result.FirstPanicErr != nil {
		parts = append(parts, result.FirstPanicErr.Error())
	}
	if skippedErr := codexChannelWorkerSkippedError(result); skippedErr != "" {
		parts = append(parts, skippedErr)
	}
	return strings.Join(parts, "; ")
}

func runCodexChannelWorkers(ctx context.Context, channels []*model.Channel, workerCount int, fn func(context.Context, *model.Channel)) codexChannelWorkerResult {
	if len(channels) == 0 || fn == nil {
		return codexChannelWorkerResult{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if workerCount <= 0 {
		workerCount = 1
	}
	if workerCount > len(channels) {
		workerCount = len(channels)
	}

	type indexedChannel struct {
		index   int
		channel *model.Channel
	}
	jobs := make(chan indexedChannel)
	started := make([]atomic.Bool, len(channels))
	var processed atomic.Int32
	var panicked atomic.Int32
	var panicMu sync.Mutex
	var firstPanicErr error
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					continue
				}

				releaseSlot, err := acquireCodexBackgroundWorkerSlot(ctx)
				if err != nil {
					continue
				}

				// The round context owns the deadline. A separate per-channel deadline
				// would truncate OAuth's full request/retry/backoff budget.
				started[job.index].Store(true)
				processed.Add(1)
				jobErr := runCodexChannelWorkerJob(ctx, job.channel, fn)
				releaseSlot()

				if jobErr != nil {
					panicked.Add(1)
					panicMu.Lock()
					if firstPanicErr == nil {
						firstPanicErr = jobErr
					}
					panicMu.Unlock()
				}
			}
		}()
	}

sendLoop:
	for index, channel := range channels {
		select {
		case <-ctx.Done():
			break sendLoop
		case jobs <- indexedChannel{index: index, channel: channel}:
		}
	}
	close(jobs)
	wg.Wait()

	startedPrefix := longestStartedPrefix(started)
	result := codexChannelWorkerResult{
		Processed:     int(processed.Load()),
		StartedPrefix: startedPrefix,
		Panicked:      int(panicked.Load()),
		FirstPanicErr: firstPanicErr,
	}
	if err := ctx.Err(); err != nil {
		result.Err = err
		result.Skipped = len(channels) - result.Processed
		if result.Skipped < 0 {
			result.Skipped = 0
		}
	}
	return result
}

func longestStartedPrefix(started []atomic.Bool) int {
	prefix := 0
	for prefix < len(started) && started[prefix].Load() {
		prefix++
	}
	return prefix
}

func acquireCodexBackgroundWorkerSlot(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case codexBackgroundWorkerSlots <- struct{}{}:
		return func() { <-codexBackgroundWorkerSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func runCodexChannelWorkerJob(ctx context.Context, channel *model.Channel, fn func(context.Context, *model.Channel)) (err error) {
	defer func() {
		if r := recover(); r != nil {
			channelID := 0
			if channel != nil {
				channelID = channel.Id
			}
			panicText := common.RedactSensitiveText(fmt.Sprint(r))
			logger.SysError(fmt.Sprintf("[Codex] channel worker panic on channel %d: %s, stack: %s", channelID, panicText, string(debug.Stack())))
			err = fmt.Errorf("%w on channel %d: %s", errCodexChannelWorkerPanic, channelID, panicText)
		}
	}()
	fn(ctx, channel)
	return nil
}
