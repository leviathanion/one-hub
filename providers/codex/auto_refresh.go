package codex

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/model"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	AutoRefreshInterval = 15 * time.Minute
	AutoRefreshLead     = 20 * time.Minute
	AutoRefreshTimeout  = 10 * time.Minute
)

var autoRefreshMu sync.Mutex

type AutoRefreshStatus struct {
	Running        bool               `json:"running"`
	LastStartedAt  int64              `json:"last_started_at"`
	LastFinishedAt int64              `json:"last_finished_at"`
	LastSuccessAt  int64              `json:"last_success_at"`
	LastDurationMs int64              `json:"last_duration_ms"`
	LastResult     string             `json:"last_result"`
	LastError      string             `json:"last_error"`
	LastSummary    AutoRefreshSummary `json:"last_summary"`
	IntervalSec    int64              `json:"interval_sec"`
	LeadSec        int64              `json:"lead_sec"`
}

var (
	autoRefreshStatusMu sync.RWMutex
	autoRefreshStatus   = AutoRefreshStatus{
		IntervalSec: int64(AutoRefreshInterval / time.Second),
		LeadSec:     int64(AutoRefreshLead / time.Second),
	}

	autoRefreshRunsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "codex_auto_refresh_runs_total",
			Help: "Total number of Codex auto refresh runs.",
		},
		[]string{"result"},
	)
	autoRefreshChannelsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "codex_auto_refresh_channels_total",
			Help: "Total number of Codex channels processed by auto refresh grouped by outcome.",
		},
		[]string{"outcome"},
	)
	autoRefreshRunningGauge = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "codex_auto_refresh_running",
			Help: "Whether a Codex auto refresh run is currently active.",
		},
	)
	autoRefreshLastRunTimestamp = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "codex_auto_refresh_last_run_timestamp_seconds",
			Help: "Unix timestamp of the last Codex auto refresh start.",
		},
	)
	autoRefreshLastSuccessTimestamp = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "codex_auto_refresh_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last successful Codex auto refresh run.",
		},
	)
	autoRefreshLastDuration = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "codex_auto_refresh_last_duration_seconds",
			Help: "Duration of the last Codex auto refresh run in seconds.",
		},
	)
	autoRefreshLastSummaryGauge = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "codex_auto_refresh_last_summary",
			Help: "Summary values from the last Codex auto refresh run.",
		},
		[]string{"field"},
	)
)

type AutoRefreshSummary struct {
	Scanned               int
	Eligible              int
	Refreshed             int
	SkippedNoRefreshToken int
	SkippedNotDue         int
	Failed                int
}

func GetAutoRefreshStatus() AutoRefreshStatus {
	autoRefreshStatusMu.RLock()
	defer autoRefreshStatusMu.RUnlock()
	return autoRefreshStatus
}

func RunAutoRefreshWithTimeout(parent context.Context) AutoRefreshSummary {
	if parent == nil {
		parent = context.Background()
	}

	ctx, cancel := context.WithTimeout(parent, AutoRefreshTimeout)
	defer cancel()

	return RefreshChannelsInBackground(ctx)
}

// RefreshChannelsInBackground proactively refreshes eligible Codex OAuth channels.
func RefreshChannelsInBackground(ctx context.Context) AutoRefreshSummary {
	if !autoRefreshMu.TryLock() {
		logger.SysLog("[Codex] auto refresh is already running, skip this round")
		return AutoRefreshSummary{}
	}
	defer autoRefreshMu.Unlock()

	startedAt := time.Now()
	setAutoRefreshRunning(startedAt)
	defer func() {
		autoRefreshRunningGauge.Set(0)
	}()

	channels, err := model.GetChannelsByTypeAndStatus(config.ChannelTypeCodex, config.ChannelStatusEnabled)
	if err != nil {
		logger.SysError("[Codex] failed to load channels for auto refresh: " + err.Error())
		summary := AutoRefreshSummary{Failed: 1}
		recordAutoRefreshResult(startedAt, summary, "error", err.Error(), false)
		return summary
	}

	summary := AutoRefreshSummary{}
	firstErr := ""
	for _, channel := range channels {
		if channel == nil || strings.TrimSpace(channel.Key) == "" {
			continue
		}

		summary.Scanned++

		preparedChannel := prepareChannelForAutoRefresh(channel)
		provider, ok := CodexProviderFactory{}.Create(preparedChannel).(*CodexProvider)
		if !ok || provider == nil || provider.Credentials == nil {
			summary.Failed++
			if firstErr == "" {
				firstErr = fmt.Sprintf("failed to initialize provider for channel %d", channel.Id)
			}
			logger.SysError(fmt.Sprintf("[Codex] failed to initialize provider for channel %d", channel.Id))
			continue
		}

		if strings.TrimSpace(provider.Credentials.RefreshToken) == "" {
			summary.SkippedNoRefreshToken++
			continue
		}

		if !provider.Credentials.NeedsRefreshWithin(AutoRefreshLead) {
			summary.SkippedNotDue++
			continue
		}

		summary.Eligible++
		refreshed, refreshErr := provider.refreshTokenIfNeeded(ctx, AutoRefreshLead)
		if refreshErr != nil {
			summary.Failed++
			if firstErr == "" {
				firstErr = fmt.Sprintf("channel %d: %s", channel.Id, refreshErr.Error())
			}
			logger.SysError(fmt.Sprintf("[Codex] auto refresh failed for channel %d: %s", channel.Id, refreshErr.Error()))
			continue
		}
		if refreshed {
			summary.Refreshed++
		}
	}

	if summary.Scanned > 0 {
		logger.SysLog(fmt.Sprintf(
			"[Codex] auto refresh finished: scanned=%d eligible=%d refreshed=%d skipped_not_due=%d skipped_no_refresh_token=%d failed=%d",
			summary.Scanned,
			summary.Eligible,
			summary.Refreshed,
			summary.SkippedNotDue,
			summary.SkippedNoRefreshToken,
			summary.Failed,
		))
	}

	lastError := ""
	result := "success"
	succeeded := true
	if summary.Failed > 0 {
		result = "partial"
		succeeded = false
		lastError = firstErr
	}
	recordAutoRefreshResult(startedAt, summary, result, lastError, succeeded)

	return summary
}

func prepareChannelForAutoRefresh(channel *model.Channel) *model.Channel {
	if channel == nil {
		return nil
	}

	prepared := *channel
	proxyValue := ""
	if channel.Proxy != nil {
		proxyValue = strings.TrimSpace(*channel.Proxy)
	}
	prepared.Proxy = &proxyValue
	prepared.SetProxy()

	return &prepared
}

func setAutoRefreshRunning(startedAt time.Time) {
	autoRefreshStatusMu.Lock()
	defer autoRefreshStatusMu.Unlock()

	autoRefreshStatus.Running = true
	autoRefreshStatus.LastStartedAt = startedAt.Unix()
	autoRefreshStatus.LastResult = "running"
	autoRefreshStatus.LastError = ""
	autoRefreshRunningGauge.Set(1)
	autoRefreshLastRunTimestamp.Set(float64(startedAt.Unix()))
}

func recordAutoRefreshResult(
	startedAt time.Time,
	summary AutoRefreshSummary,
	result string,
	lastError string,
	succeeded bool,
) {
	finishedAt := time.Now()
	duration := finishedAt.Sub(startedAt)

	autoRefreshStatusMu.Lock()
	autoRefreshStatus.Running = false
	autoRefreshStatus.LastFinishedAt = finishedAt.Unix()
	autoRefreshStatus.LastDurationMs = duration.Milliseconds()
	autoRefreshStatus.LastResult = result
	autoRefreshStatus.LastError = lastError
	autoRefreshStatus.LastSummary = summary
	if succeeded {
		autoRefreshStatus.LastSuccessAt = finishedAt.Unix()
	}
	autoRefreshStatusMu.Unlock()

	autoRefreshRunsTotal.WithLabelValues(result).Inc()
	autoRefreshChannelsTotal.WithLabelValues("refreshed").Add(float64(summary.Refreshed))
	autoRefreshChannelsTotal.WithLabelValues("failed").Add(float64(summary.Failed))
	autoRefreshChannelsTotal.WithLabelValues("skipped_not_due").Add(float64(summary.SkippedNotDue))
	autoRefreshChannelsTotal.WithLabelValues("skipped_no_refresh_token").Add(float64(summary.SkippedNoRefreshToken))
	autoRefreshLastDuration.Set(duration.Seconds())
	if succeeded {
		autoRefreshLastSuccessTimestamp.Set(float64(finishedAt.Unix()))
	}
	autoRefreshLastSummaryGauge.WithLabelValues("scanned").Set(float64(summary.Scanned))
	autoRefreshLastSummaryGauge.WithLabelValues("eligible").Set(float64(summary.Eligible))
	autoRefreshLastSummaryGauge.WithLabelValues("refreshed").Set(float64(summary.Refreshed))
	autoRefreshLastSummaryGauge.WithLabelValues("skipped_not_due").Set(float64(summary.SkippedNotDue))
	autoRefreshLastSummaryGauge.WithLabelValues("skipped_no_refresh_token").Set(float64(summary.SkippedNoRefreshToken))
	autoRefreshLastSummaryGauge.WithLabelValues("failed").Set(float64(summary.Failed))
}
