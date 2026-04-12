package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/model"
)

func TestNormalizeUsageSnapshotClassifiesCanonicalWindowsAndKeepsUnknownCustom(t *testing.T) {
	credentials := &OAuth2Credentials{AccountID: "acct-from-creds"}
	body := []byte(`{
		"plan_type": "pro",
		"user_id": "user-123",
		"rate_limit": {
			"allowed": true,
			"limit_reached": false,
			"primary_window": {
				"used_percent": 25,
				"limit_window_seconds": 18000,
				"reset_after_seconds": 1800
			},
			"secondary_window": {
				"used": 9,
				"limit": 10,
				"limit_window_seconds": 43200,
				"reset_after_seconds": 7200
			}
		}
	}`)

	before := time.Now().Unix()
	snapshot, err := normalizeUsageSnapshot(42, credentials, http.StatusOK, body)
	after := time.Now().Unix()
	if err != nil {
		t.Fatalf("expected normalization to succeed, got %v", err)
	}

	if snapshot.ChannelID != 42 {
		t.Fatalf("expected channel id to be preserved, got %d", snapshot.ChannelID)
	}
	if snapshot.Account == nil || snapshot.Account.UserID != "user-123" {
		t.Fatalf("expected payload account data, got %+v", snapshot.Account)
	}
	if snapshot.Account.AccountID != "acct-from-creds" {
		t.Fatalf("expected credentials account id fallback, got %+v", snapshot.Account)
	}
	if len(snapshot.Windows) != 2 {
		t.Fatalf("expected two normalized windows, got %+v", snapshot.Windows)
	}

	fiveHourWindow := snapshot.Windows[0]
	if fiveHourWindow.WindowKey != "five_hour" || fiveHourWindow.Label != "5h" {
		t.Fatalf("expected primary window to classify as 5h, got %+v", fiveHourWindow)
	}
	if !optionalFloat64Equals(fiveHourWindow.UsedPercent, 25) || !optionalFloat64Equals(fiveHourWindow.UsageRatio, 0.25) {
		t.Fatalf("expected percentage-based usage normalization, got %+v", fiveHourWindow)
	}
	if fiveHourWindow.ResetsInSeconds != 1800 {
		t.Fatalf("expected reset_after_seconds fallback, got %+v", fiveHourWindow)
	}
	if fiveHourWindow.ResetsAt < before+1795 || fiveHourWindow.ResetsAt > after+1805 {
		t.Fatalf("expected computed reset timestamp near now+1800, got %d", fiveHourWindow.ResetsAt)
	}

	customWindow := snapshot.Windows[1]
	if customWindow.WindowKey != "custom" || customWindow.Label != "Secondary" {
		t.Fatalf("expected unknown duration to stay custom, got %+v", customWindow)
	}
	if customWindow.Used == nil || *customWindow.Used != 9 {
		t.Fatalf("expected used value to be preserved, got %+v", customWindow)
	}
	if customWindow.Limit == nil || *customWindow.Limit != 10 {
		t.Fatalf("expected limit value to be preserved, got %+v", customWindow)
	}
	if customWindow.Remaining == nil || *customWindow.Remaining != 1 {
		t.Fatalf("expected remaining to be derived from used/limit, got %+v", customWindow)
	}
	if !optionalFloat64Equals(customWindow.UsageRatio, 0.9) {
		t.Fatalf("expected ratio from absolute used/limit, got %+v", customWindow)
	}
}

func TestNormalizeUsageSnapshotPreservesUnknownWindowMetrics(t *testing.T) {
	body := []byte(`{
		"plan_type": "pro",
		"rate_limit": {
			"primary_window": {
				"limit_window_seconds": 18000,
				"reset_after_seconds": 1800
			}
		}
	}`)

	snapshot, err := normalizeUsageSnapshot(42, nil, http.StatusOK, body)
	if err != nil {
		t.Fatalf("expected normalization to succeed, got %v", err)
	}

	fiveHourWindow := getCodexUsageWindowFromSlice(snapshot.Windows, "five_hour")
	if fiveHourWindow == nil {
		t.Fatalf("expected canonical 5h window, got %+v", snapshot.Windows)
	}
	if fiveHourWindow.UsedPercent != nil || fiveHourWindow.UsageRatio != nil {
		t.Fatalf("expected unknown metrics to stay absent instead of 0, got %+v", fiveHourWindow)
	}
}

func TestNormalizeUsageSnapshotKeepsExplicitZeroUsage(t *testing.T) {
	body := []byte(`{
		"plan_type": "pro",
		"rate_limit": {
			"primary_window": {
				"used_percent": 0,
				"limit_window_seconds": 18000
			},
			"secondary_window": {
				"used": 0,
				"limit": 10,
				"limit_window_seconds": 604800
			}
		}
	}`)

	snapshot, err := normalizeUsageSnapshot(42, nil, http.StatusOK, body)
	if err != nil {
		t.Fatalf("expected normalization to succeed, got %v", err)
	}

	fiveHourWindow := getCodexUsageWindowFromSlice(snapshot.Windows, "five_hour")
	if fiveHourWindow == nil || !optionalFloat64Equals(fiveHourWindow.UsedPercent, 0) || !optionalFloat64Equals(fiveHourWindow.UsageRatio, 0) {
		t.Fatalf("expected explicit 0%% usage to remain known, got %+v", fiveHourWindow)
	}

	weeklyWindow := getCodexUsageWindowFromSlice(snapshot.Windows, "weekly")
	if weeklyWindow == nil || !optionalFloat64Equals(weeklyWindow.UsedPercent, 0) || !optionalFloat64Equals(weeklyWindow.UsageRatio, 0) {
		t.Fatalf("expected derived 0%% usage to remain known, got %+v", weeklyWindow)
	}
}

func TestGetUsageSnapshotRetriesAfterUnauthorizedByForceRefreshing(t *testing.T) {
	cache.InitCacheManager()
	logger.SetupLogger()

	serverToken := atomic.Value{}
	serverToken.Store("refreshed-token")
	requestCount := atomic.Int32{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("unexpected usage path %q", r.URL.Path)
		}

		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != serverToken.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"expired"}}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type": "pro",
			"rate_limit": {
				"allowed": true,
				"limit_reached": false,
				"primary_window": {
					"used_percent": 33,
					"limit_window_seconds": 18000,
					"resets_in_seconds": 900
				},
				"secondary_window": {
					"used_percent": 67,
					"limit_window_seconds": 604800,
					"resets_in_seconds": 3600
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

	originalRefreshCredentials := refreshOAuthCredentials
	refreshOAuthCredentials = func(creds *OAuth2Credentials, _ context.Context, _ string, _ int) error {
		creds.AccessToken = serverToken.Load().(string)
		creds.ExpiresAt = time.Now().Add(time.Hour)
		return nil
	}
	t.Cleanup(func() {
		refreshOAuthCredentials = originalRefreshCredentials
	})

	updatedKey := atomic.Value{}
	initialCreds := &OAuth2Credentials{
		AccessToken:  "expired-token",
		RefreshToken: "refresh-token",
		AccountID:    "acct-123",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	initialKey, err := initialCreds.ToJSON()
	if err != nil {
		t.Fatalf("failed to serialize initial credentials: %v", err)
	}
	updatedKey.Store(initialKey)

	originalUpdateChannelKey := updateChannelKey
	updateChannelKey = func(channelID int, key string) error {
		if channelID != 424299 {
			t.Fatalf("unexpected channel id update %d", channelID)
		}
		updatedKey.Store(key)
		return nil
	}
	t.Cleanup(func() {
		updateChannelKey = originalUpdateChannelKey
	})

	originalLoadLatestChannelByID := loadLatestChannelByID
	loadLatestChannelByID = func(channelID int) (*model.Channel, error) {
		if channelID != 424299 {
			t.Fatalf("unexpected channel id reload %d", channelID)
		}
		return &model.Channel{
			Id:      channelID,
			Key:     updatedKey.Load().(string),
			BaseURL: stringPtr(server.URL),
		}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	provider := newTestCodexProviderWithContext(t, initialKey, "", nil)
	provider.Channel.BaseURL = stringPtr(server.URL)

	snapshot, err := provider.GetUsageSnapshot(context.Background(), true)
	if err != nil {
		t.Fatalf("expected retry after forced refresh to succeed, got %v", err)
	}
	if requestCount.Load() != 2 {
		t.Fatalf("expected unauthorized request to be retried once, got %d attempts", requestCount.Load())
	}
	if snapshot == nil || snapshot.UpstreamStatus != http.StatusOK {
		t.Fatalf("expected successful usage snapshot after retry, got %+v", snapshot)
	}
	if got := getCodexUsageWindowFromSlice(snapshot.Windows, "five_hour"); got == nil || !optionalFloat64Equals(got.UsedPercent, 33) {
		t.Fatalf("expected 5h window after retry, got %+v", snapshot.Windows)
	}
	if !strings.Contains(updatedKey.Load().(string), "refreshed-token") {
		t.Fatalf("expected refreshed token to be persisted, got %s", updatedKey.Load().(string))
	}
}

func TestGetUsageSnapshotReturnsSnapshotOnUpstreamFailure(t *testing.T) {
	cache.InitCacheManager()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limit reached"}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Channel.BaseURL = stringPtr(server.URL)

	snapshot, err := provider.GetUsageSnapshot(context.Background(), true)
	if err == nil {
		t.Fatal("expected upstream failure to return an error")
	}
	if !strings.Contains(err.Error(), "rate limit reached") {
		t.Fatalf("expected upstream message to surface, got %v", err)
	}
	if snapshot == nil {
		t.Fatal("expected partial snapshot even when upstream request fails")
	}
	if snapshot.UpstreamStatus != http.StatusTooManyRequests {
		t.Fatalf("expected upstream status to be preserved, got %+v", snapshot)
	}

	rawMap, ok := snapshot.Raw.(map[string]any)
	if !ok {
		t.Fatalf("expected raw payload map, got %#v", snapshot.Raw)
	}
	if rawMap["message"] != "rate limit reached" {
		t.Fatalf("expected raw payload message, got %+v", rawMap)
	}
}

func TestGetUsagePreviewDoesNotCacheFailedSnapshot(t *testing.T) {
	cache.InitCacheManager()

	requestCount := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requestCount.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limit reached"}`))
		case 2:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"plan_type": "pro",
				"rate_limit": {
					"allowed": true,
					"limit_reached": false,
					"primary_window": {
						"used_percent": 25,
						"limit_window_seconds": 18000,
						"resets_in_seconds": 900
					}
				}
			}`))
		default:
			t.Fatalf("unexpected extra usage preview fetch #%d", requestCount.Load())
		}
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Channel.BaseURL = stringPtr(server.URL)

	preview, err := provider.GetUsagePreview(context.Background(), false)
	if err == nil {
		t.Fatal("expected first preview fetch to surface upstream failure")
	}
	if preview == nil {
		t.Fatal("expected partial preview to be returned for diagnostics")
	}
	if _, cacheErr := getCachedUsagePreview(provider.Channel.Id); cacheErr == nil {
		t.Fatal("expected failed preview fetch not to populate preview cache")
	}

	preview, err = provider.GetUsagePreview(context.Background(), false)
	if err != nil {
		t.Fatalf("expected second preview fetch to refetch successfully, got %v", err)
	}
	if preview == nil || preview.ChannelID != provider.Channel.Id {
		t.Fatalf("expected successful preview after upstream recovery, got %+v", preview)
	}
	if requestCount.Load() != 2 {
		t.Fatalf("expected upstream to be called twice because the failed preview was not cached, got %d", requestCount.Load())
	}
	if got := getCodexUsageWindowFromSlice(preview.Windows, "five_hour"); got == nil || !optionalFloat64Equals(got.UsedPercent, 25) {
		t.Fatalf("expected recovered preview windows, got %+v", preview.Windows)
	}
	if _, cacheErr := getCachedUsagePreview(provider.Channel.Id); cacheErr != nil {
		t.Fatalf("expected successful preview fetch to populate cache, got %v", cacheErr)
	}
}

func getCodexUsageWindowFromSlice(windows []CodexUsageWindow, windowKey string) *CodexUsageWindow {
	for index := range windows {
		if windows[index].WindowKey == windowKey {
			return &windows[index]
		}
	}
	return nil
}

func TestBuildUsagePreviewClonesWindowSlice(t *testing.T) {
	snapshot := &CodexUsageSnapshot{
		ChannelID: 7,
		Windows: []CodexUsageWindow{
			{WindowKey: "five_hour", Label: "5h"},
		},
	}

	preview := BuildUsagePreview(snapshot)
	if preview == nil {
		t.Fatal("expected preview to be created")
	}

	preview.Windows[0].Label = "mutated"
	if snapshot.Windows[0].Label != "5h" {
		t.Fatalf("expected preview build to clone windows slice, got %+v", snapshot.Windows)
	}
}

func TestNormalizeUsageSnapshotKeepsRawBodyWhenJSONIsInvalid(t *testing.T) {
	snapshot, err := normalizeUsageSnapshot(9, nil, http.StatusBadGateway, []byte("not-json"))
	if err == nil {
		t.Fatal("expected invalid json payload to fail normalization")
	}
	if snapshot == nil || snapshot.Raw != "not-json" {
		t.Fatalf("expected raw string fallback for invalid json, got %+v", snapshot)
	}
}

func TestExtractUsageErrorMessagePrefersNestedError(t *testing.T) {
	raw := map[string]any{
		"error": map[string]any{
			"message": "nested-message",
		},
		"message": "outer-message",
	}
	if got := extractUsageErrorMessage(raw, http.StatusForbidden); got != "nested-message" {
		t.Fatalf("expected nested error message to win, got %q", got)
	}
}

func TestNormalizeUsageSnapshotPreservesRawJSONPayload(t *testing.T) {
	body := []byte(`{"message":"hello"}`)
	snapshot, err := normalizeUsageSnapshot(3, nil, http.StatusOK, body)
	if err != nil {
		t.Fatalf("expected payload to normalize, got %v", err)
	}
	encodedRaw, marshalErr := json.Marshal(snapshot.Raw)
	if marshalErr != nil {
		t.Fatalf("failed to re-encode raw payload: %v", marshalErr)
	}
	if string(encodedRaw) != string(body) {
		t.Fatalf("expected raw payload to stay intact, got %s", string(encodedRaw))
	}
}

func optionalFloat64Equals(value *float64, want float64) bool {
	if value == nil {
		return false
	}
	return *value == want
}
