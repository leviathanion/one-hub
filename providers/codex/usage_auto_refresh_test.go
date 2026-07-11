package codex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/model"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestUsageAutoRefreshRedactsEchoedCredentialSecrets(t *testing.T) {
	const accessToken = "usage-access-secret"
	const refreshToken = "usage-refresh-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Authorization: Bearer ` + accessToken + ` refresh=` + refreshToken + `"}}`))
	}))
	defer server.Close()

	originalClient := requester.HTTPClient
	originalGeneration := usageCacheGeneration
	originalJitter := usageAutoRefreshJitter
	originalLogger := logger.Logger
	core, observedLogs := observer.New(zapcore.ErrorLevel)
	requester.HTTPClient = server.Client()
	usageCacheGeneration = func(context.Context, int) (string, error) { return "redaction-generation", nil }
	usageAutoRefreshJitter = func() time.Duration { return 0 }
	logger.Logger = zap.New(core)
	t.Cleanup(func() {
		requester.HTTPClient = originalClient
		usageCacheGeneration = originalGeneration
		usageAutoRefreshJitter = originalJitter
		logger.Logger = originalLogger
	})

	channel := &model.Channel{
		Id:      21001,
		Type:    config.ChannelTypeCodex,
		Key:     `{"access_token":"` + accessToken + `","refresh_token":"` + refreshToken + `"}`,
		BaseURL: stringPtr(server.URL),
	}
	cache.InitCacheManager()
	primeCachedTokenForKey(t, channel.Id, channel.Key, accessToken, time.Now().Add(time.Hour), time.Hour)
	provider := CodexProviderFactory{}.Create(channel).(*CodexProvider)
	snapshot, fetchErr := provider.fetchUsageSnapshot(context.Background())
	if fetchErr == nil || snapshot == nil || snapshot.UpstreamStatus != http.StatusTooManyRequests {
		t.Fatalf("expected sanitized upstream 429, snapshot=%+v err=%v", snapshot, fetchErr)
	}
	for _, secret := range []string{accessToken, refreshToken} {
		if strings.Contains(fetchErr.Error(), secret) {
			t.Fatalf("usage error leaked %q: %q", secret, fetchErr.Error())
		}
	}

	result := refreshUsageSnapshotForChannel(context.Background(), channel)
	if result.Failed != 1 || result.FirstErr == "" {
		t.Fatalf("expected failed refresh with a returned error, got %+v", result)
	}
	if !strings.Contains(result.FirstErr, "[redacted]") {
		t.Fatalf("expected returned error to retain redaction marker, got %q", result.FirstErr)
	}
	for _, secret := range []string{accessToken, refreshToken} {
		if strings.Contains(result.FirstErr, secret) {
			t.Fatalf("returned error leaked %q: %q", secret, result.FirstErr)
		}
	}
	logs := observedLogs.All()
	if len(logs) == 0 {
		t.Fatal("expected background refresh failure to be logged")
	}
	for _, entry := range logs {
		for _, secret := range []string{accessToken, refreshToken} {
			if strings.Contains(entry.Message, secret) {
				t.Fatalf("background log leaked %q: %q", secret, entry.Message)
			}
		}
	}
}

func TestUsageRequestCancellationStopsGenerationLookup(t *testing.T) {
	originalGeneration := usageCacheGeneration
	originalClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	started := make(chan struct{})
	usageCacheGeneration = func(ctx context.Context, _ int) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	t.Cleanup(func() {
		usageCacheGeneration = originalGeneration
		requester.HTTPClient = originalClient
	})

	baseURL := "http://127.0.0.1:1"
	channel := &model.Channel{Id: 99701, Type: config.ChannelTypeCodex, Key: `{"access_token":"token"}`, BaseURL: &baseURL}
	provider := CodexProviderFactory{}.Create(channel).(*CodexProvider)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := provider.GetUsageSnapshot(ctx, false)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("request cancellation returned %v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("request kept waiting for usage generation after cancellation")
	}
}

func TestUsagePreviewCancellationDoesNotReturnCachedEntry(t *testing.T) {
	cache.InitCacheManager()
	originalGeneration := usageCacheGeneration
	originalClient := requester.HTTPClient
	usageCacheGeneration = func(context.Context, int) (string, error) { return "cancel-entry-generation", nil }
	requester.HTTPClient = &http.Client{}
	t.Cleanup(func() {
		usageCacheGeneration = originalGeneration
		requester.HTTPClient = originalClient
	})

	baseURL := "http://127.0.0.1:1"
	channel := &model.Channel{Id: 99703, Type: config.ChannelTypeCodex, Key: `{"access_token":"token"}`, BaseURL: &baseURL}
	preview := &CodexUsagePreview{ChannelID: channel.Id}
	if err := cacheUsagePreviewForGeneration(context.Background(), channel, preview, "cancel-entry-generation", time.Minute); err != nil {
		t.Fatal(err)
	}
	provider := CodexProviderFactory{}.Create(channel).(*CodexProvider)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got, err := provider.GetUsagePreview(ctx, false); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("canceled cached preview read = %#v, %v; want nil, context.Canceled", got, err)
	}
}

func TestUsageBackgroundCancellationStopsCacheWriteAndCountsFailure(t *testing.T) {
	originalGeneration := usageCacheGeneration
	originalJitter := usageAutoRefreshJitter
	originalSet := setCodexUsageCache
	originalClient := requester.HTTPClient
	writeStarted := make(chan struct{})
	usageCacheGeneration = func(context.Context, int) (string, error) { return "write-cancel-generation", nil }
	usageAutoRefreshJitter = func() time.Duration { return 0 }
	setCodexUsageCache = func(ctx context.Context, _ string, _ any, _ time.Duration) error {
		close(writeStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true}}`))
	}))
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		server.Close()
		usageCacheGeneration = originalGeneration
		usageAutoRefreshJitter = originalJitter
		setCodexUsageCache = originalSet
		requester.HTTPClient = originalClient
	})

	baseURL := server.URL
	channel := &model.Channel{Id: 99704, Type: config.ChannelTypeCodex, Key: `{"access_token":"token"}`, BaseURL: &baseURL}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan usageRefreshChannelResult, 1)
	go func() { done <- refreshUsageSnapshotForChannel(ctx, channel) }()
	<-writeStarted
	started := time.Now()
	cancel()
	select {
	case result := <-done:
		if result.Refreshed != 0 || result.Failed != 1 || !strings.Contains(result.FirstErr, context.Canceled.Error()) {
			t.Fatalf("canceled background write result = %+v", result)
		}
		if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
			t.Fatalf("canceled background cache write took %s", elapsed)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("background usage refresh kept waiting after cache write cancellation")
	}
}

func TestUsageCronCancellationStopsGenerationLookup(t *testing.T) {
	originalGeneration := usageCacheGeneration
	originalJitter := usageAutoRefreshJitter
	originalClient := requester.HTTPClient
	started := make(chan struct{})
	usageCacheGeneration = func(ctx context.Context, _ int) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	usageAutoRefreshJitter = func() time.Duration { return 0 }
	requester.HTTPClient = &http.Client{}
	t.Cleanup(func() {
		usageCacheGeneration = originalGeneration
		usageAutoRefreshJitter = originalJitter
		requester.HTTPClient = originalClient
	})

	channel := &model.Channel{Id: 99702, Type: config.ChannelTypeCodex, Key: `{"access_token":"token"}`}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan usageRefreshChannelResult, 1)
	go func() { done <- refreshUsageSnapshotForChannel(ctx, channel) }()
	<-started
	cancel()
	select {
	case result := <-done:
		if result.Failed != 1 || !strings.Contains(result.FirstErr, context.Canceled.Error()) {
			t.Fatalf("cron cancellation result = %+v", result)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("cron worker kept waiting for usage generation after cancellation")
	}
}

func TestUsagePreviewTTLCoversSkippedOverlappingMaintenanceRound(t *testing.T) {
	minimum := 2*UsageAutoRefreshInterval + AutoRefreshTimeout + UsageAutoRefreshTimeout + usageAutoRefreshJitterMax
	if usagePreviewCacheTTL <= minimum {
		t.Fatalf("preview TTL %s must exceed worst overlapping-round interval %s with grace", usagePreviewCacheTTL, minimum)
	}
}

func TestUsageWarmupBatchAdmissionIsNonBlockingAndBounded(t *testing.T) {
	release, ok := TryAcquireUsageWarmupBatch()
	if !ok {
		t.Fatal("expected first warm-up batch to acquire admission")
	}
	if secondRelease, secondOK := TryAcquireUsageWarmupBatch(); secondOK {
		secondRelease()
		t.Fatal("expected concurrent warm-up batch to be skipped")
	}
	release()
	if nextRelease, nextOK := TryAcquireUsageWarmupBatch(); !nextOK {
		t.Fatal("expected admission to reopen after batch completion")
	} else {
		nextRelease()
	}
}

func TestUsageRunnerCancellationReachesChannelLoader(t *testing.T) {
	originalLoadChannels := loadUsageAutoRefreshChannels
	loaderCanceled := make(chan struct{})
	loadUsageAutoRefreshChannels = func(ctx context.Context) ([]*model.Channel, error) {
		<-ctx.Done()
		close(loaderCanceled)
		return nil, ctx.Err()
	}
	t.Cleanup(func() { loadUsageAutoRefreshChannels = originalLoadChannels })

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	summary := RunUsageAutoRefreshWithTimeout(parent)
	if summary.Failed != 1 {
		t.Fatalf("expected canceled loader to fail the round, got %+v", summary)
	}
	select {
	case <-loaderCanceled:
	default:
		t.Fatal("expected round cancellation to reach usage channel loader")
	}
}

func TestRefreshUsageSnapshotsInBackgroundMarksCanceledDispatchPartial(t *testing.T) {
	usageAutoRefreshStatusMu.Lock()
	usageAutoRefreshStatus = UsageAutoRefreshStatus{
		LastSuccessAt: 123,
		IntervalSec:   int64(UsageAutoRefreshInterval / time.Second),
	}
	usageAutoRefreshStatusMu.Unlock()

	originalLoadChannels := loadUsageAutoRefreshChannels
	loadUsageAutoRefreshChannels = func(context.Context) ([]*model.Channel, error) {
		return []*model.Channel{
			{Id: 1, Key: `{"access_token":"token-1"}`},
			{Id: 2, Key: `{"access_token":"token-2"}`},
		}, nil
	}
	t.Cleanup(func() {
		loadUsageAutoRefreshChannels = originalLoadChannels
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	summary := RefreshUsageSnapshotsInBackground(ctx)
	if summary.Failed != 2 {
		t.Fatalf("expected canceled dispatch to fail both channels, got %+v", summary)
	}

	status := GetUsageAutoRefreshStatus()
	if status.LastResult != "partial" {
		t.Fatalf("expected partial result, got %q", status.LastResult)
	}
	if status.LastSuccessAt != 123 {
		t.Fatalf("expected last success timestamp to remain unchanged, got %d", status.LastSuccessAt)
	}
	if !strings.Contains(status.LastError, "context canceled") || !strings.Contains(status.LastError, "skipped 2") {
		t.Fatalf("expected cancellation details in last error, got %q", status.LastError)
	}
}

func TestRefreshUsageSnapshotsForChannelsWarmsCacheEveryRun(t *testing.T) {
	if logger.Logger == nil {
		logger.SetupLogger()
	}
	cache.InitCacheManager()

	originalJitter := usageAutoRefreshJitter
	usageAutoRefreshJitter = func() time.Duration { return 0 }
	t.Cleanup(func() {
		usageAutoRefreshJitter = originalJitter
	})

	var requestCount int
	var requestMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		requestCount++
		requestMu.Unlock()

		if r.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type": "pro",
			"rate_limit": {
				"allowed": true,
				"primary_window": {
					"used": 1,
					"limit": 10,
					"limit_window_seconds": 18000,
					"resets_at": 2600
				}
			}
		}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	baseURL := server.URL
	channel := &model.Channel{
		Id:      99,
		Type:    config.ChannelTypeCodex,
		Status:  config.ChannelStatusEnabled,
		Name:    "codex",
		Key:     `{"access_token":"access-token"}`,
		BaseURL: &baseURL,
	}

	summary, firstErr, _ := refreshUsageSnapshotsForChannels(context.Background(), []*model.Channel{channel})
	if firstErr != "" {
		t.Fatalf("expected no first error, got %q", firstErr)
	}
	if summary.Scanned != 1 || summary.Eligible != 1 || summary.Refreshed != 1 || summary.Failed != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	summary, firstErr, _ = refreshUsageSnapshotsForChannels(context.Background(), []*model.Channel{channel})
	if firstErr != "" {
		t.Fatalf("expected no second first error, got %q", firstErr)
	}
	if summary.Scanned != 1 || summary.Eligible != 1 || summary.Refreshed != 1 || summary.Failed != 0 {
		t.Fatalf("unexpected second summary: %+v", summary)
	}

	preview, err := getCachedUsagePreviewForChannel(channel)
	if err != nil {
		t.Fatalf("expected preview cache to be warmed, got %v", err)
	}
	if preview.ChannelID != channel.Id || len(preview.Windows) != 1 {
		t.Fatalf("unexpected cached preview: %+v", preview)
	}
	if _, err := getCachedUsageSnapshotForChannel(channel); !errors.Is(err, cache.CacheNotFound) {
		t.Fatalf("expected background refresh to leave detail cache cold, got %v", err)
	}

	requestMu.Lock()
	gotRequests := requestCount
	requestMu.Unlock()
	if gotRequests != 2 {
		t.Fatalf("expected every run to refresh upstream usage, got %d requests", gotRequests)
	}
}

func TestLimitImmediateUsageWarmupDoesNotLimitImportedChannels(t *testing.T) {
	channels := make([]*model.Channel, usageImportWarmupLimit+37)
	for i := range channels {
		channels[i] = &model.Channel{Id: i + 1}
	}
	limited := LimitImmediateUsageWarmup(channels)
	if len(limited) != usageImportWarmupLimit {
		t.Fatalf("expected immediate warm-up limit %d, got %d", usageImportWarmupLimit, len(limited))
	}
	if len(channels) != usageImportWarmupLimit+37 {
		t.Fatalf("warm-up selection must not alter imported count, got %d", len(channels))
	}
}

func TestChannelsForUsageRefreshSelectsOnlyEnabledInsertedCodexChannels(t *testing.T) {
	channels := []model.Channel{
		{Id: 1, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled},
		{Id: 2, Type: config.ChannelTypeCodex, Status: config.ChannelStatusManuallyDisabled},
		{Id: 3, Type: config.ChannelTypeOpenAI, Status: config.ChannelStatusEnabled},
		{Id: 0, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled},
	}

	selected := ChannelsForUsageRefresh(channels)
	if len(selected) != 1 || selected[0] != &channels[0] {
		t.Fatalf("expected only enabled inserted Codex channel, got %+v", selected)
	}
}

func TestSleepUsageAutoRefreshJitterHonorsConfiguredDelay(t *testing.T) {
	originalJitter := usageAutoRefreshJitter
	usageAutoRefreshJitter = func() time.Duration { return 20 * time.Millisecond }
	t.Cleanup(func() {
		usageAutoRefreshJitter = originalJitter
	})

	startedAt := time.Now()
	if err := sleepUsageAutoRefreshJitter(context.Background()); err != nil {
		t.Fatalf("expected jitter sleep to succeed, got %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed < 15*time.Millisecond {
		t.Fatalf("expected jitter sleep to delay refresh, elapsed %v", elapsed)
	}
}
