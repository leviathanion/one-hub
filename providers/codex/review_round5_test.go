package codex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestPendingCredentialRecoverySerializesProviders(t *testing.T) {
	channelID := 99501
	expected := credentialRound5TestKey(t, "old", "old-refresh")
	rotated := credentialRound5TestKey(t, "rotated", "rotated-refresh")
	rememberPendingCredentialCommit(channelID, expected, rotated)
	t.Cleanup(func() { clearPendingCredentialCommit(channelID, rotated) })

	var dbMu sync.Mutex
	dbKey := expected
	var casCalls atomic.Int32
	originalCAS, originalLoad := compareAndSetChannelKey, loadLatestChannelByID
	compareAndSetChannelKey = func(_ context.Context, _ int, oldKey, newKey string) (bool, error) {
		casCalls.Add(1)
		dbMu.Lock()
		defer dbMu.Unlock()
		if dbKey != oldKey {
			return false, nil
		}
		time.Sleep(10 * time.Millisecond)
		dbKey = newKey
		return true, nil
	}
	loadLatestChannelByID = func(context.Context, int) (*model.Channel, error) {
		dbMu.Lock()
		defer dbMu.Unlock()
		return &model.Channel{Id: channelID, Key: dbKey}, nil
	}
	t.Cleanup(func() {
		compareAndSetChannelKey = originalCAS
		loadLatestChannelByID = originalLoad
	})

	providers := []*CodexProvider{
		CodexProviderFactory{}.Create(&model.Channel{Id: channelID, Key: expected}).(*CodexProvider),
		CodexProviderFactory{}.Create(&model.Channel{Id: channelID, Key: expected}).(*CodexProvider),
	}
	start := make(chan struct{})
	errs := make(chan error, len(providers))
	for _, provider := range providers {
		go func(p *CodexProvider) {
			<-start
			errs <- p.commitPendingCredentials(context.Background())
		}(provider)
	}
	close(start)
	for range providers {
		if err := <-errs; err != nil {
			t.Fatalf("pending recovery failed: %v", err)
		}
	}
	if got := casCalls.Load(); got != 1 {
		t.Fatalf("expected exactly one pending CAS, got %d", got)
	}
}

func TestCredentialCASMissReloadFailureRetainsDirtyPendingRotation(t *testing.T) {
	channelID := 99502
	expected := credentialRound5TestKey(t, "old", "old-refresh")
	provider := CodexProviderFactory{}.Create(&model.Channel{Id: channelID, Key: expected}).(*CodexProvider)
	rotated := credentialRound5TestKey(t, "rotated", "rotated-refresh")
	provider.credentialExpectedKey = expected
	provider.credentialRotatedKey = rotated
	provider.credentialDirty = true
	rememberPendingCredentialCommit(channelID, expected, rotated)
	t.Cleanup(func() { clearPendingCredentialCommit(channelID, rotated) })

	originalCAS, originalLoad := compareAndSetChannelKey, loadLatestChannelByID
	compareAndSetChannelKey = func(context.Context, int, string, string) (bool, error) { return false, nil }
	loadLatestChannelByID = func(context.Context, int) (*model.Channel, error) {
		return nil, errors.New("database unavailable")
	}
	t.Cleanup(func() {
		compareAndSetChannelKey = originalCAS
		loadLatestChannelByID = originalLoad
	})

	if err := provider.persistRefreshedCredentials(context.Background()); !errors.Is(err, errCodexCredentialCASConflict) {
		t.Fatalf("expected unresolved CAS conflict, got %v", err)
	}
	if !provider.hasDirtyCredentials() {
		t.Fatal("reload failure must retain dirty state")
	}
	if _, ok := getPendingCredentialCommit(channelID); !ok {
		t.Fatal("reload failure must retain pending rotation")
	}
	if provider.getCurrentValidToken(context.Background()) != "" {
		t.Fatal("dirty rotated token must not be returned or cached")
	}
}

func TestCredentialCASMissAdoptsAlreadyCommittedRotation(t *testing.T) {
	channelID := 99503
	expected := credentialRound5TestKey(t, "old", "old-refresh")
	rotated := credentialRound5TestKey(t, "rotated", "rotated-refresh")
	provider := CodexProviderFactory{}.Create(&model.Channel{Id: channelID, Key: expected}).(*CodexProvider)
	provider.Credentials = parseCredentialsFromKey(rotated)
	provider.credentialExpectedKey = expected
	provider.credentialRotatedKey = rotated
	provider.credentialDirty = true
	rememberPendingCredentialCommit(channelID, expected, rotated)
	t.Cleanup(func() { clearPendingCredentialCommit(channelID, rotated) })

	originalCAS, originalLoad := compareAndSetChannelKey, loadLatestChannelByID
	compareAndSetChannelKey = func(context.Context, int, string, string) (bool, error) { return false, nil }
	loadLatestChannelByID = func(context.Context, int) (*model.Channel, error) {
		return &model.Channel{Id: channelID, Key: rotated}, nil
	}
	t.Cleanup(func() {
		compareAndSetChannelKey = originalCAS
		loadLatestChannelByID = originalLoad
	})

	if err := provider.persistRefreshedCredentials(context.Background()); err != nil {
		t.Fatalf("already committed rotation must be idempotent success: %v", err)
	}
	if provider.hasDirtyCredentials() {
		t.Fatal("idempotent success must clear dirty state")
	}
	if _, ok := getPendingCredentialCommit(channelID); ok {
		t.Fatal("idempotent success must clear pending state")
	}
	if provider.Credentials == nil || provider.Credentials.AccessToken != "rotated" {
		t.Fatalf("expected rotated credentials to be adopted, got %+v", provider.Credentials)
	}
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
