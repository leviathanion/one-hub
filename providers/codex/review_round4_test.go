package codex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
)

func TestUsageDetailEntryReadFailureFailsOpenWithoutCacheWrite(t *testing.T) {
	cache.InitCacheManager()
	channelID := 99002
	baseURL := ""
	channel := &model.Channel{Id: channelID, Type: config.ChannelTypeCodex, Key: `{"access_token":"token"}`, BaseURL: &baseURL}
	generation, err := usageCacheGeneration(context.Background(), channelID)
	if err != nil {
		t.Fatal(err)
	}
	key := usageDetailCacheKey(channelID, generation, codexUsageChannelFingerprint(channel))
	if err := cache.SetCache(key, "wrong-entry-type", time.Minute); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true}}`))
	}))
	defer server.Close()
	baseURL = server.URL
	// Fingerprint changed with BaseURL; corrupt the actual key.
	key = usageDetailCacheKey(channelID, generation, codexUsageChannelFingerprint(channel))
	if err := cache.SetCache(key, "wrong-entry-type", time.Minute); err != nil {
		t.Fatal(err)
	}
	originalClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	originalSet := setCodexUsageCache
	writes := atomic.Int32{}
	setCodexUsageCache = func(context.Context, string, any, time.Duration) error { writes.Add(1); return nil }
	t.Cleanup(func() { requester.HTTPClient = originalClient; setCodexUsageCache = originalSet })
	provider := CodexProviderFactory{}.Create(channel).(*CodexProvider)
	snapshot, err := provider.GetUsageSnapshot(context.Background(), false)
	if err != nil || snapshot == nil || snapshot.PlanType != "pro" {
		t.Fatalf("entry cache failure must fail open: snapshot=%+v err=%v", snapshot, err)
	}
	if writes.Load() != 0 {
		t.Fatalf("cache read failure must suppress entry writes, got %d writes", writes.Load())
	}
}

func TestUsagePreviewEntryReadFailureFailsOpenWithoutCacheWrite(t *testing.T) {
	cache.InitCacheManager()
	channelID := 99004
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"plan_type":"team","rate_limit":{"allowed":true}}`))
	}))
	defer server.Close()
	baseURL := server.URL
	channel := &model.Channel{Id: channelID, Type: config.ChannelTypeCodex, Key: `{"access_token":"token"}`, BaseURL: &baseURL}
	generation, err := usageCacheGeneration(context.Background(), channelID)
	if err != nil {
		t.Fatal(err)
	}
	key := usagePreviewCacheKey(channelID, generation, codexUsageChannelFingerprint(channel))
	if err := cache.SetCache(key, "wrong-entry-type", time.Minute); err != nil {
		t.Fatal(err)
	}
	originalClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	originalSet := setCodexUsageCache
	writes := atomic.Int32{}
	setCodexUsageCache = func(context.Context, string, any, time.Duration) error { writes.Add(1); return nil }
	t.Cleanup(func() { requester.HTTPClient = originalClient; setCodexUsageCache = originalSet })
	provider := CodexProviderFactory{}.Create(channel).(*CodexProvider)
	preview, err := provider.GetUsagePreview(context.Background(), false)
	if err != nil || preview == nil || preview.PlanType != "team" {
		t.Fatalf("preview cache failure must fail open: preview=%+v err=%v", preview, err)
	}
	if writes.Load() != 0 {
		t.Fatalf("preview cache read failure must suppress entry writes, got %d writes", writes.Load())
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestCredentialCASConflictAdoptsManualUpdateAndNeverCachesRotation(t *testing.T) {
	cache.InitCacheManager()
	channelID := 99003
	old := &OAuth2Credentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Minute)}
	oldKey, _ := old.ToJSON()
	manual := &OAuth2Credentials{AccessToken: "manual", RefreshToken: "manual-refresh", ExpiresAt: time.Now().Add(time.Hour)}
	manualKey, _ := manual.ToJSON()
	provider := CodexProviderFactory{}.Create(&model.Channel{Id: channelID, Key: oldKey}).(*CodexProvider)
	originalRefresh := refreshOAuthCredentials
	refreshOAuthCredentials = func(creds *OAuth2Credentials, _ context.Context, _ string) error {
		creds.AccessToken, creds.RefreshToken, creds.ExpiresAt = "stale-rotated", "stale-refresh", time.Now().Add(time.Hour)
		return nil
	}
	originalCAS := compareAndSetChannelKey
	compareAndSetChannelKey = func(_ context.Context, _ int, expected, _ string) (bool, error) {
		if expected != oldKey {
			t.Fatalf("unexpected CAS expected key")
		}
		return false, nil
	}
	originalLoad := loadLatestChannelByID
	loads := atomic.Int32{}
	loadLatestChannelByID = func(context.Context, int) (*model.Channel, error) {
		if loads.Add(1) <= 2 {
			return &model.Channel{Id: channelID, Key: oldKey}, nil
		}
		return &model.Channel{Id: channelID, Key: manualKey}, nil
	}
	t.Cleanup(func() {
		refreshOAuthCredentials = originalRefresh
		compareAndSetChannelKey = originalCAS
		loadLatestChannelByID = originalLoad
	})
	_, err := provider.refreshTokenIfNeeded(context.Background(), 3*time.Minute)
	if !errors.Is(err, errCodexCredentialCASConflict) {
		t.Fatalf("expected explicit CAS conflict, got %v", err)
	}
	if provider.Credentials == nil || provider.Credentials.AccessToken != "manual" {
		t.Fatalf("manual DB credentials must win, got %+v", provider.Credentials)
	}
	if _, cacheErr := cache.GetCache[cachedAccessToken](tokenCacheKeyV2(channelID, manualKey)); !errors.Is(cacheErr, cache.CacheNotFound) {
		t.Fatalf("stale rotation must not be cached under the manual fingerprint: %v", cacheErr)
	}
}
