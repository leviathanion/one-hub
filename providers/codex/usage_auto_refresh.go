package codex

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/model"
)

const (
	UsageAutoRefreshInterval  = AutoRefreshInterval
	UsageAutoRefreshTimeout   = 10 * time.Minute
	usageAutoRefreshJitterMax = 5 * time.Second
	usageImportWarmupLimit    = 100
	// Scheduled OAuth and usage maintenance intentionally remain serial. A round
	// can consume both 10-minute budgets and make the overlapping 15-minute tick
	// skip. A value written at the start of the prior round must therefore survive
	// until the end of the next effective round: two intervals, both budgets, and
	// jitter/grace. The trade-off is retaining stale previews longer after failures.
	usagePreviewCacheTTL = 2*UsageAutoRefreshInterval + AutoRefreshTimeout + UsageAutoRefreshTimeout + usageAutoRefreshJitterMax + time.Minute
)

var (
	usageAutoRefreshMu   sync.Mutex
	usageWarmupBatchGate = make(chan struct{}, 1)
)

type UsageAutoRefreshStatus struct {
	Running        bool                    `json:"running"`
	LastStartedAt  int64                   `json:"last_started_at"`
	LastFinishedAt int64                   `json:"last_finished_at"`
	LastSuccessAt  int64                   `json:"last_success_at"`
	LastDurationMs int64                   `json:"last_duration_ms"`
	LastResult     string                  `json:"last_result"`
	LastError      string                  `json:"last_error"`
	LastSummary    UsageAutoRefreshSummary `json:"last_summary"`
	IntervalSec    int64                   `json:"interval_sec"`
}

type UsageAutoRefreshSummary struct {
	Scanned   int
	Eligible  int
	Refreshed int
	Failed    int
}

type usageRefreshChannelResult struct {
	UsageAutoRefreshSummary
	FirstErr string
}

var (
	usageAutoRefreshStatusMu sync.RWMutex
	usageAutoRefreshStatus   = UsageAutoRefreshStatus{
		IntervalSec: int64(UsageAutoRefreshInterval / time.Second),
	}
	loadUsageAutoRefreshChannels = loadAutoRefreshChannels
	nowForUsageAutoRefresh       = time.Now
	usageAutoRefreshJitter       = randomUsageAutoRefreshJitter
	usageAutoRefreshCursor       codexChannelCursor
)

func GetUsageAutoRefreshStatus() UsageAutoRefreshStatus {
	usageAutoRefreshStatusMu.RLock()
	defer usageAutoRefreshStatusMu.RUnlock()
	return usageAutoRefreshStatus
}

func RunUsageAutoRefreshWithTimeout(parent context.Context) UsageAutoRefreshSummary {
	if parent == nil {
		parent = context.Background()
	}

	ctx, cancel := context.WithTimeout(parent, UsageAutoRefreshTimeout)
	defer cancel()

	return RefreshUsageSnapshotsInBackground(ctx)
}

func RefreshUsageSnapshotsInBackground(ctx context.Context) UsageAutoRefreshSummary {
	if ctx == nil {
		ctx = context.Background()
	}
	if !usageAutoRefreshMu.TryLock() {
		logger.SysLog("[Codex] usage auto refresh is already running, skip this round")
		return UsageAutoRefreshSummary{}
	}
	defer usageAutoRefreshMu.Unlock()

	startedAt := nowForUsageAutoRefresh()
	setUsageAutoRefreshRunning(startedAt)

	channels, err := loadUsageAutoRefreshChannels(ctx)
	if err != nil {
		logger.SysError("[Codex] failed to load channels for usage auto refresh: " + err.Error())
		summary := UsageAutoRefreshSummary{Failed: 1}
		recordUsageAutoRefreshResult(startedAt, summary, "error", err.Error(), false)
		return summary
	}

	channels = usageAutoRefreshCursor.rotate(channels)
	summary, firstErr, workerResult := refreshUsageSnapshotsForChannels(ctx, channels)
	usageAutoRefreshCursor.advance(channels, workerResult.StartedPrefix)
	result := "success"
	succeeded := true
	if summary.Failed > 0 {
		result = "partial"
		succeeded = false
	}
	recordUsageAutoRefreshResult(startedAt, summary, result, firstErr, succeeded)

	if summary.Scanned > 0 {
		logger.SysLog(fmt.Sprintf(
			"[Codex] usage auto refresh finished: scanned=%d eligible=%d refreshed=%d failed=%d",
			summary.Scanned,
			summary.Eligible,
			summary.Refreshed,
			summary.Failed,
		))
	}

	return summary
}

// RefreshUsageSnapshotsForChannelsWithTimeout warms usage snapshots for known channels.
// It is used after bulk auth-file imports, where every new account should be fetched
// once regardless of its schedule state.
// TryAcquireUsageWarmupBatch applies non-blocking admission before an import
// spawns its best-effort warm-up goroutine. A busy batch is skipped; scheduled
// maintenance will fill the cache later.
func TryAcquireUsageWarmupBatch() (func(), bool) {
	select {
	case usageWarmupBatchGate <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-usageWarmupBatchGate }) }, true
	default:
		return nil, false
	}
}

// LimitImmediateUsageWarmup bounds only best-effort post-import work; every
// channel remains imported and overflow is picked up by scheduled maintenance.
func LimitImmediateUsageWarmup(channels []*model.Channel) []*model.Channel {
	if len(channels) <= usageImportWarmupLimit {
		return channels
	}
	return channels[:usageImportWarmupLimit]
}

func RefreshUsageSnapshotsForChannelsWithTimeout(parent context.Context, channels []*model.Channel) UsageAutoRefreshSummary {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, UsageAutoRefreshTimeout)
	defer cancel()

	summary, firstErr, _ := refreshUsageSnapshotsForChannels(ctx, channels)
	if summary.Failed > 0 {
		logger.SysError(fmt.Sprintf("[Codex] imported usage warm-up finished with %d failure(s): %s", summary.Failed, firstErr))
	}
	return summary
}

func refreshUsageSnapshotsForChannels(ctx context.Context, channels []*model.Channel) (UsageAutoRefreshSummary, string, codexChannelWorkerResult) {
	summary := UsageAutoRefreshSummary{}
	firstErr := ""
	var summaryMu sync.Mutex

	workerResult := runCodexChannelWorkers(ctx, channels, codexWorkerCount(len(channels)), func(workerCtx context.Context, channel *model.Channel) {
		result := refreshUsageSnapshotForChannel(workerCtx, channel)
		summaryMu.Lock()
		summary.Scanned += result.Scanned
		summary.Eligible += result.Eligible
		summary.Refreshed += result.Refreshed
		summary.Failed += result.Failed
		if firstErr == "" && result.FirstErr != "" {
			firstErr = result.FirstErr
		}
		summaryMu.Unlock()
	})
	if workerFailures := workerResult.FailureCount(); workerFailures > 0 {
		summary.Failed += workerFailures
		if firstErr == "" {
			firstErr = codexChannelWorkerError(workerResult)
		}
	}

	return summary, firstErr, workerResult
}

func refreshUsageSnapshotForChannel(ctx context.Context, channel *model.Channel) usageRefreshChannelResult {
	result := usageRefreshChannelResult{}
	if channel == nil || strings.TrimSpace(channel.Key) == "" {
		return result
	}
	result.Scanned = 1

	result.Eligible = 1
	if err := sleepUsageAutoRefreshJitter(ctx); err != nil {
		result.Failed = 1
		result.FirstErr = fmt.Sprintf("channel %d: %s", channel.Id, err.Error())
		return result
	}

	preparedChannel := prepareChannelForAutoRefresh(channel)
	provider, ok := CodexProviderFactory{}.Create(preparedChannel).(*CodexProvider)
	if !ok || provider == nil {
		result.Failed = 1
		result.FirstErr = fmt.Sprintf("failed to initialize provider for channel %d", channel.Id)
		logger.SysError(fmt.Sprintf("[Codex] failed to initialize usage provider for channel %d", channel.Id))
		return result
	}
	if requester.HTTPClient == nil {
		result.Failed = 1
		result.FirstErr = fmt.Sprintf("channel %d: HTTP client is not configured", channel.Id)
		return result
	}

	// Capture the generation before upstream I/O. A reset can then rotate the
	// namespace while this fetch is running without allowing its late write back.
	generation, generationErr := usageCacheGeneration(ctx, preparedChannel.Id)
	if generationErr != nil {
		result.Failed = 1
		result.FirstErr = fmt.Sprintf("channel %d: usage cache unavailable: %s", channel.Id, generationErr.Error())
		logger.SysError("[Codex] " + result.FirstErr)
		return result
	}

	// Unlike interactive reads, background work has no value if it cannot publish
	// its preview. Fail before upstream I/O rather than reporting a false refresh.
	// Background refresh only owns the list preview. Fetch directly so it does not
	// populate the short-lived detail cache or perform duplicate cache writes.
	snapshot, err := provider.fetchUsageSnapshot(ctx)
	if err != nil {
		result.Failed = 1
		result.FirstErr = fmt.Sprintf("channel %d: %s", channel.Id, err.Error())
		logger.SysError(fmt.Sprintf("[Codex] usage auto refresh failed for channel %d: %s", channel.Id, err.Error()))
		return result
	}
	if snapshot == nil {
		result.Failed = 1
		result.FirstErr = fmt.Sprintf("channel %d: empty usage snapshot", channel.Id)
		logger.SysError(fmt.Sprintf("[Codex] usage auto refresh returned empty snapshot for channel %d", channel.Id))
		return result
	}

	// Keep only the low-cost list projection alive across refresh intervals. Cache
	// entries carry a channel fingerprint, so an in-flight request from an older
	// channel configuration can never be served after that channel changes.
	preview := BuildUsagePreview(snapshot)
	if cacheErr := cacheUsagePreviewForGeneration(ctx, provider.codexChannel(), preview, generation, usagePreviewCacheTTL); cacheErr != nil {
		result.Failed = 1
		result.FirstErr = fmt.Sprintf("channel %d: %s", channel.Id, cacheErr.Error())
		logger.SysError(fmt.Sprintf("[Codex] usage auto refresh cache failed for channel %d: %s", channel.Id, cacheErr.Error()))
		return result
	}
	result.Refreshed = 1
	return result
}

func sleepUsageAutoRefreshJitter(ctx context.Context) error {
	delay := usageAutoRefreshJitter()
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func randomUsageAutoRefreshJitter() time.Duration {
	if usageAutoRefreshJitterMax <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(usageAutoRefreshJitterMax)))
}

func setUsageAutoRefreshRunning(startedAt time.Time) {
	usageAutoRefreshStatusMu.Lock()
	defer usageAutoRefreshStatusMu.Unlock()

	usageAutoRefreshStatus.Running = true
	usageAutoRefreshStatus.LastStartedAt = startedAt.Unix()
	usageAutoRefreshStatus.LastResult = "running"
	usageAutoRefreshStatus.LastError = ""
}

func recordUsageAutoRefreshResult(
	startedAt time.Time,
	summary UsageAutoRefreshSummary,
	result string,
	lastError string,
	succeeded bool,
) {
	finishedAt := nowForUsageAutoRefresh()
	duration := finishedAt.Sub(startedAt)

	usageAutoRefreshStatusMu.Lock()
	usageAutoRefreshStatus.Running = false
	usageAutoRefreshStatus.LastFinishedAt = finishedAt.Unix()
	usageAutoRefreshStatus.LastDurationMs = duration.Milliseconds()
	usageAutoRefreshStatus.LastResult = result
	usageAutoRefreshStatus.LastError = lastError
	usageAutoRefreshStatus.LastSummary = summary
	if succeeded {
		usageAutoRefreshStatus.LastSuccessAt = finishedAt.Unix()
	}
	usageAutoRefreshStatusMu.Unlock()
}

// ChannelsForUsageRefresh returns enabled, inserted Codex channels that can be
// accessed immediately after an import.
func ChannelsForUsageRefresh(channels []model.Channel) []*model.Channel {
	results := make([]*model.Channel, 0, len(channels))
	for index := range channels {
		if channels[index].Type != config.ChannelTypeCodex || channels[index].Status != config.ChannelStatusEnabled || channels[index].Id <= 0 {
			continue
		}
		results = append(results, &channels[index])
	}
	return results
}
