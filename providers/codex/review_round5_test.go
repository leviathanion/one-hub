package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
)

func credentialRound5TestKey(t *testing.T, accessToken, refreshToken string) string {
	t.Helper()
	key, err := (&OAuth2Credentials{AccessToken: accessToken, RefreshToken: refreshToken}).ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestUsagePreviewWritesEachCacheEntryOnce(t *testing.T) {
	cache.InitCacheManager()
	originalRedis := config.RedisEnabled
	config.RedisEnabled = false
	t.Cleanup(func() { config.RedisEnabled = originalRedis })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true}}`))
	}))
	defer server.Close()
	originalClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalClient })

	baseURL := server.URL
	channel := &model.Channel{Id: 99505, Type: config.ChannelTypeCodex, Key: `{"access_token":"token"}`, BaseURL: &baseURL}
	generation, err := usageCacheGeneration(context.Background(), channel.Id)
	if err != nil {
		t.Fatal(err)
	}
	originalSet := setCodexUsageCache
	var previewWrites, detailWrites atomic.Int32
	setCodexUsageCache = func(ctx context.Context, key string, value any, ttl time.Duration) error {
		switch {
		case strings.HasPrefix(key, usagePreviewCacheKeyPrefix+":"):
			previewWrites.Add(1)
		case strings.HasPrefix(key, usageDetailCacheKeyPrefix+":"):
			detailWrites.Add(1)
		}
		return originalSet(ctx, key, value, ttl)
	}
	t.Cleanup(func() { setCodexUsageCache = originalSet })

	provider := CodexProviderFactory{}.Create(channel).(*CodexProvider)
	if _, err := provider.GetUsagePreview(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if previewWrites.Load() != 1 || detailWrites.Load() != 1 {
		t.Fatalf("fresh preview fetch must write detail/preview once, got detail=%d preview=%d", detailWrites.Load(), previewWrites.Load())
	}

	fingerprint := codexUsageChannelFingerprint(channel)
	if err := cache.DeleteCache(usagePreviewCacheKey(channel.Id, generation, fingerprint)); err != nil {
		t.Fatal(err)
	}
	previewWrites.Store(0)
	detailWrites.Store(0)
	if _, err := provider.GetUsagePreview(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if previewWrites.Load() != 1 || detailWrites.Load() != 0 {
		t.Fatalf("detail-cache reuse must only backfill preview once, got detail=%d preview=%d", detailWrites.Load(), previewWrites.Load())
	}
}
