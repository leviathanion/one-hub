package codex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"
	commonredis "one-api/common/redis"
	"one-api/common/utils"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

func primeCachedToken(t *testing.T, channelID int, accessToken string, expiresAt time.Time, ttl time.Duration) {
	t.Helper()

	if err := cache.SetCache(tokenCacheKey(channelID), cachedAccessToken{
		AccessToken: accessToken,
		ExpiresAt:   expiresAt,
	}, ttl); err != nil {
		t.Fatalf("failed to prime cache: %v", err)
	}
}

func stubLatestChannelByIDForTest(t *testing.T, channelID int, creds *OAuth2Credentials) {
	t.Helper()

	key, err := creds.ToJSON()
	if err != nil {
		t.Fatalf("failed to serialize credentials: %v", err)
	}

	originalLoadLatestChannelByID := loadLatestChannelByID
	loadLatestChannelByID = func(id int) (*model.Channel, error) {
		if id != channelID {
			t.Fatalf("unexpected channel id lookup: got %d want %d", id, channelID)
		}
		return &model.Channel{Id: id, Key: key}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})
}

func newCanceledGinContext(t *testing.T) *gin.Context {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	req, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx.Request = req.WithContext(requestCtx)
	return ctx
}

func requesterProxyAddr(t *testing.T, provider *CodexProvider) string {
	t.Helper()

	if provider == nil || provider.Requester == nil {
		t.Fatalf("expected provider requester to be initialized")
	}

	req, err := provider.Requester.NewRequest(http.MethodGet, "https://example.com")
	if err != nil {
		t.Fatalf("failed to build requester probe request: %v", err)
	}

	if proxyAddr, ok := req.Context().Value(utils.ProxyHTTPAddrKey).(string); ok {
		return proxyAddr
	}
	if proxyAddr, ok := req.Context().Value(utils.ProxySock5AddrKey).(string); ok {
		return proxyAddr
	}
	return ""
}

func TestGetTokenFallsBackToStillValidAccessTokenWhenRefreshFails(t *testing.T) {
	cache.InitCacheManager()
	logger.SetupLogger()

	channelID := 424250
	latestCreds := &OAuth2Credentials{
		AccessToken:  "expired-db-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-time.Minute),
	}
	latestKey, err := latestCreds.ToJSON()
	if err != nil {
		t.Fatalf("failed to serialize latest credentials: %v", err)
	}

	originalLoadLatestChannelByID := loadLatestChannelByID
	loadLatestChannelByID = func(id int) (*model.Channel, error) {
		if id != channelID {
			t.Fatalf("unexpected channel id lookup: got %d want %d", id, channelID)
		}
		proxy := "http://proxy.example/%s"
		return &model.Channel{Id: id, Key: latestKey, Proxy: &proxy}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	initialProxy := "http://proxy.example/%s"
	initialChannel := &model.Channel{Id: channelID, Key: "still-valid-key", Proxy: &initialProxy}
	initialChannel.SetProxy()

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Context: newCanceledGinContext(t),
				Channel: prepareChannelForProvider(initialChannel),
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "still-valid-access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(2 * time.Minute),
		},
	}

	token, err := provider.GetToken()
	if err != nil {
		t.Fatalf("expected near-expiry token fallback, got error: %v", err)
	}
	if token != "still-valid-access-token" {
		t.Fatalf("expected current access token fallback, got %q", token)
	}
	if provider.Channel == nil || provider.Channel.Proxy == nil || *provider.Channel.Proxy != *initialChannel.Proxy {
		t.Fatalf("expected fallback to restore the original proxy, got %v", provider.Channel)
	}
	if proxyAddr := requesterProxyAddr(t, provider); proxyAddr != *initialChannel.Proxy {
		t.Fatalf("expected requester proxy %q after fallback, got %q", *initialChannel.Proxy, proxyAddr)
	}
}

func TestGetTokenFallsBackToStillValidCachedTokenWhenRefreshFails(t *testing.T) {
	cache.InitCacheManager()
	logger.SetupLogger()

	channelID := 424249
	cacheKey := tokenCacheKey(channelID)
	_ = cache.DeleteCache(cacheKey)
	defer cache.DeleteCache(cacheKey)

	primeCachedToken(t, channelID, "cached-still-valid-token", time.Now().Add(2*time.Minute), time.Minute)
	stubLatestChannelByIDForTest(t, channelID, &OAuth2Credentials{
		AccessToken:  "expired-db-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-time.Minute),
	})

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Context: newCanceledGinContext(t),
				Channel: &model.Channel{Id: channelID},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "expired-local-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(-time.Minute),
		},
	}

	token, err := provider.GetToken()
	if err != nil {
		t.Fatalf("expected cached near-expiry token fallback, got error: %v", err)
	}
	if token != "cached-still-valid-token" {
		t.Fatalf("expected cached token fallback, got %q", token)
	}
	if provider.Credentials.AccessToken != "cached-still-valid-token" {
		t.Fatalf("expected provider credentials to adopt cached token, got %q", provider.Credentials.AccessToken)
	}
}

func TestGetTokenStillFailsWhenTokenAlreadyExpired(t *testing.T) {
	logger.SetupLogger()

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Context: newCanceledGinContext(t),
				Channel: &model.Channel{},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "expired-access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(-time.Minute),
		},
	}

	token, err := provider.GetToken()
	if err == nil {
		t.Fatalf("expected expired token path to return refresh error")
	}
	if token != "" {
		t.Fatalf("expected no token on expired credential, got %q", token)
	}
	if !strings.Contains(err.Error(), "failed to refresh token") {
		t.Fatalf("expected refresh failure to surface, got %v", err)
	}
}

func TestRefreshTokenIfNeededUsesCachedTokenWhenOutsideLead(t *testing.T) {
	cache.InitCacheManager()

	channelID := 424242
	cacheKey := tokenCacheKey(channelID)
	_ = cache.DeleteCache(cacheKey)
	defer cache.DeleteCache(cacheKey)

	expiresAt := time.Now().Add(30 * time.Minute)
	primeCachedToken(t, channelID, "fresh-access-token", expiresAt, time.Minute)

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: channelID},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "stale-access-token",
			RefreshToken: "stale-refresh-token",
			ExpiresAt:    time.Now().Add(-time.Minute),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	refreshed, err := provider.refreshTokenIfNeeded(ctx, 3*time.Minute)
	if err != nil {
		t.Fatalf("expected cached token to avoid refresh, got error: %v", err)
	}
	if refreshed {
		t.Fatalf("expected cached token path to skip refresh")
	}
	if provider.Credentials.AccessToken != "fresh-access-token" {
		t.Fatalf("expected access token to be updated from cache, got %q", provider.Credentials.AccessToken)
	}
}

func TestRefreshTokenIfNeededIgnoresCachedTokenWithinLead(t *testing.T) {
	cache.InitCacheManager()

	channelID := 424248
	cacheKey := tokenCacheKey(channelID)
	_ = cache.DeleteCache(cacheKey)
	defer cache.DeleteCache(cacheKey)

	expiresAt := time.Now().Add(5 * time.Minute)
	primeCachedToken(t, channelID, "cached-access-token", expiresAt, time.Minute)

	latestCreds := &OAuth2Credentials{
		AccessToken:  "db-access-token",
		RefreshToken: "db-refresh-token",
		ExpiresAt:    expiresAt,
	}
	stubLatestChannelByIDForTest(t, channelID, latestCreds)

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: channelID},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "initial-access-token",
			RefreshToken: "initial-refresh-token",
			ExpiresAt:    time.Now().Add(time.Hour),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	refreshed, err := provider.refreshTokenIfNeeded(ctx, 20*time.Minute)
	if err == nil {
		t.Fatalf("expected refresh attempt once cached token enters the lead window")
	}
	if refreshed {
		t.Fatalf("expected failed refresh attempt to report refreshed=false")
	}
	if !strings.Contains(err.Error(), "token refresh canceled") {
		t.Fatalf("expected canceled refresh error, got %v", err)
	}
	if provider.Credentials.AccessToken != "db-access-token" {
		t.Fatalf("expected database credentials to replace stale cache path, got %q", provider.Credentials.AccessToken)
	}
}

func TestParseCredentialsFromKeyAppliesLegacyExpiryFallback(t *testing.T) {
	start := time.Now()
	creds := parseCredentialsFromKey(`{
		"access_token":"access",
		"refresh_token":"refresh"
	}`)
	if creds == nil {
		t.Fatalf("expected credentials to be parsed")
	}
	if creds.ExpiresAt.IsZero() {
		t.Fatalf("expected missing expiry to receive a fallback")
	}
	if creds.ExpiresAt.Before(start.Add(50*time.Minute)) || creds.ExpiresAt.After(start.Add(70*time.Minute)) {
		t.Fatalf("expected fallback expiry about one hour ahead, got %s", creds.ExpiresAt.Format(time.RFC3339))
	}
	if creds.ClientID != DefaultClientID {
		t.Fatalf("expected default client id %q, got %q", DefaultClientID, creds.ClientID)
	}
}

func TestCreateClonesRuntimeChannelAndKeepsSharedStateUntouched(t *testing.T) {
	proxyTemplate := "http://proxy.example/%s"
	sharedChannel := &model.Channel{
		Id:    424251,
		Key:   "old-key",
		Proxy: &proxyTemplate,
	}
	sharedChannel.SetProxy()
	sharedProxy := *sharedChannel.Proxy

	provider, ok := CodexProviderFactory{}.Create(sharedChannel).(*CodexProvider)
	if !ok || provider == nil {
		t.Fatalf("expected Codex provider instance")
	}
	if provider.Channel == sharedChannel {
		t.Fatalf("expected provider channel to be detached from shared chooser state")
	}

	latestCreds := &OAuth2Credentials{
		AccessToken:  "latest-access-token",
		RefreshToken: "latest-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	latestKey, err := latestCreds.ToJSON()
	if err != nil {
		t.Fatalf("failed to serialize latest credentials: %v", err)
	}

	originalLoadLatestChannelByID := loadLatestChannelByID
	loadLatestChannelByID = func(channelID int) (*model.Channel, error) {
		if channelID != sharedChannel.Id {
			t.Fatalf("unexpected channel id lookup: got %d want %d", channelID, sharedChannel.Id)
		}
		proxy := "http://proxy.example/%s"
		return &model.Channel{
			Id:    channelID,
			Key:   latestKey,
			Proxy: &proxy,
		}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	if err := provider.loadLatestCredentialsFromDatabase(); err != nil {
		t.Fatalf("expected runtime channel reload to succeed, got %v", err)
	}

	if sharedChannel.Key != "old-key" {
		t.Fatalf("expected shared channel key to remain unchanged, got %q", sharedChannel.Key)
	}
	if sharedChannel.Proxy == nil || *sharedChannel.Proxy != sharedProxy {
		t.Fatalf("expected shared channel proxy to remain unchanged, got %v", sharedChannel.Proxy)
	}
	if provider.Channel == sharedChannel {
		t.Fatalf("expected reloaded provider channel to remain detached")
	}
	if provider.Channel.Key != latestKey {
		t.Fatalf("expected provider runtime key to reload from database, got %q", provider.Channel.Key)
	}

	expectedProxy := "http://proxy.example/%s"
	expectedChannel := &model.Channel{Key: latestKey, Proxy: &expectedProxy}
	expectedChannel.SetProxy()
	if provider.Channel.Proxy == nil || *provider.Channel.Proxy != *expectedChannel.Proxy {
		t.Fatalf("expected runtime proxy to be recomputed from the latest key, got %v", provider.Channel.Proxy)
	}
	if proxyAddr := requesterProxyAddr(t, provider); proxyAddr != *expectedChannel.Proxy {
		t.Fatalf("expected requester proxy %q, got %q", *expectedChannel.Proxy, proxyAddr)
	}
}

func TestSaveCredentialsToDatabaseReloadsRuntimeChannelState(t *testing.T) {
	logger.SetupLogger()

	proxyTemplate := "http://proxy.example/%s"
	sharedChannel := &model.Channel{
		Id:    424252,
		Key:   "old-key",
		Proxy: &proxyTemplate,
	}
	sharedChannel.SetProxy()
	sharedProxy := *sharedChannel.Proxy

	provider, ok := CodexProviderFactory{}.Create(sharedChannel).(*CodexProvider)
	if !ok || provider == nil {
		t.Fatalf("expected Codex provider instance")
	}
	provider.Credentials = &OAuth2Credentials{
		AccessToken:  "new-access-token",
		RefreshToken: "new-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	}

	latestKey, err := provider.Credentials.ToJSON()
	if err != nil {
		t.Fatalf("failed to serialize credentials: %v", err)
	}

	var savedKey string
	originalUpdateChannelKey := updateChannelKey
	updateChannelKey = func(channelID int, key string) error {
		if channelID != sharedChannel.Id {
			t.Fatalf("unexpected channel key update: got %d want %d", channelID, sharedChannel.Id)
		}
		savedKey = key
		return nil
	}
	t.Cleanup(func() {
		updateChannelKey = originalUpdateChannelKey
	})

	originalLoadLatestChannelByID := loadLatestChannelByID
	loadLatestChannelByID = func(channelID int) (*model.Channel, error) {
		if channelID != sharedChannel.Id {
			t.Fatalf("unexpected channel id lookup: got %d want %d", channelID, sharedChannel.Id)
		}
		proxy := "http://proxy.example/%s"
		return &model.Channel{
			Id:    channelID,
			Key:   latestKey,
			Proxy: &proxy,
		}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	if err := provider.saveCredentialsToDatabase(context.Background()); err != nil {
		t.Fatalf("expected credentials save to succeed, got %v", err)
	}
	if savedKey != latestKey {
		t.Fatalf("expected updated key %q, got %q", latestKey, savedKey)
	}
	if sharedChannel.Key != "old-key" {
		t.Fatalf("expected shared channel key to remain unchanged, got %q", sharedChannel.Key)
	}
	if sharedChannel.Proxy == nil || *sharedChannel.Proxy != sharedProxy {
		t.Fatalf("expected shared channel proxy to remain unchanged, got %v", sharedChannel.Proxy)
	}
	if provider.Channel.Key != latestKey {
		t.Fatalf("expected provider channel key to match saved credentials, got %q", provider.Channel.Key)
	}

	expectedProxy := "http://proxy.example/%s"
	expectedChannel := &model.Channel{Key: latestKey, Proxy: &expectedProxy}
	expectedChannel.SetProxy()
	if provider.Channel.Proxy == nil || *provider.Channel.Proxy != *expectedChannel.Proxy {
		t.Fatalf("expected provider channel proxy to be reloaded from database, got %v", provider.Channel.Proxy)
	}
	if proxyAddr := requesterProxyAddr(t, provider); proxyAddr != *expectedChannel.Proxy {
		t.Fatalf("expected requester proxy %q, got %q", *expectedChannel.Proxy, proxyAddr)
	}
}

func TestRefreshNoLongerNeededUsesCachedToken(t *testing.T) {
	cache.InitCacheManager()

	channelID := 424243
	cacheKey := tokenCacheKey(channelID)
	_ = cache.DeleteCache(cacheKey)
	defer cache.DeleteCache(cacheKey)

	primeCachedToken(t, channelID, "shared-access-token", time.Now().Add(30*time.Minute), time.Minute)

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: channelID},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "stale-access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(-time.Minute),
		},
	}

	if !provider.refreshNoLongerNeeded(3*time.Minute, false) {
		t.Fatalf("expected cached token to satisfy refresh wait path")
	}
	if provider.Credentials.AccessToken != "shared-access-token" {
		t.Fatalf("expected access token to be updated from cache, got %q", provider.Credentials.AccessToken)
	}
}

func TestAcquireDistributedRefreshLockFailsClosedOnRedisError(t *testing.T) {
	cache.InitCacheManager()
	logger.SetupLogger()

	originalRedisEnabled := config.RedisEnabled
	config.RedisEnabled = true
	t.Cleanup(func() {
		config.RedisEnabled = originalRedisEnabled
	})

	originalRedisClient := commonredis.RDB
	commonredis.RDB = redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	t.Cleanup(func() {
		commonredis.RDB = originalRedisClient
	})

	originalSetNX := acquireDistributedRefreshLockSetNX
	acquireDistributedRefreshLockSetNX = func(ctx context.Context, key string, value any, ttl time.Duration) (bool, error) {
		return false, errors.New("redis unavailable")
	}
	t.Cleanup(func() {
		acquireDistributedRefreshLockSetNX = originalSetNX
	})

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: 424245},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(-time.Minute),
		},
	}

	lock, handledByPeer, err := provider.acquireDistributedRefreshLock(context.Background(), 3*time.Minute)
	if err == nil {
		t.Fatalf("expected redis error to fail closed")
	}
	if lock != nil {
		t.Fatalf("expected no distributed lock on redis failure")
	}
	if handledByPeer {
		t.Fatalf("expected redis failure to stop refresh instead of pretending a peer handled it")
	}
}

func TestAcquireDistributedRefreshLockSetNXReturnsErrorWhenRedisClientMissing(t *testing.T) {
	originalRedisClient := commonredis.RDB
	commonredis.RDB = nil
	t.Cleanup(func() {
		commonredis.RDB = originalRedisClient
	})

	acquired, err := acquireDistributedRefreshLockSetNX(context.Background(), "codex:refresh-lock:test", "token", time.Second)
	if err == nil {
		t.Fatalf("expected missing redis client to return an error")
	}
	if acquired {
		t.Fatalf("expected missing redis client to avoid acquiring a lock")
	}
}

func TestAcquireDistributedRefreshLockLogsTimeoutAsInfo(t *testing.T) {
	cache.InitCacheManager()
	logger.SetupLogger()

	originalRedisEnabled := config.RedisEnabled
	config.RedisEnabled = true
	t.Cleanup(func() {
		config.RedisEnabled = originalRedisEnabled
	})

	originalRedisClient := commonredis.RDB
	commonredis.RDB = redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	t.Cleanup(func() {
		commonredis.RDB = originalRedisClient
	})

	originalSetNX := acquireDistributedRefreshLockSetNX
	acquireDistributedRefreshLockSetNX = func(ctx context.Context, key string, value any, ttl time.Duration) (bool, error) {
		return false, context.DeadlineExceeded
	}
	t.Cleanup(func() {
		acquireDistributedRefreshLockSetNX = originalSetNX
	})

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: 424247},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(-time.Minute),
		},
	}

	beforeLogs, err := logger.GetLatestLogs(500)
	if err != nil {
		t.Fatalf("failed to read logs: %v", err)
	}

	_, _, err = provider.acquireDistributedRefreshLock(context.Background(), 3*time.Minute)
	if err == nil {
		t.Fatalf("expected lock wait to surface timeout")
	}

	afterLogs, err := logger.GetLatestLogs(500)
	if err != nil {
		t.Fatalf("failed to read logs: %v", err)
	}
	if len(afterLogs) <= len(beforeLogs) {
		t.Fatalf("expected timeout path to append a log entry")
	}

	lastLog := afterLogs[len(afterLogs)-1]
	if lastLog.Level != "INFO" {
		t.Fatalf("expected timeout to log at INFO level, got %s", lastLog.Level)
	}
	if !strings.Contains(lastLog.Message, "failed to acquire distributed refresh lock for channel 424247") {
		t.Fatalf("unexpected timeout log message: %q", lastLog.Message)
	}
}

func TestAcquireDistributedRefreshLockTimesOutAndThrottlesDatabaseReloads(t *testing.T) {
	cache.InitCacheManager()
	logger.SetupLogger()

	originalRedisEnabled := config.RedisEnabled
	config.RedisEnabled = true
	t.Cleanup(func() {
		config.RedisEnabled = originalRedisEnabled
	})

	originalRedisClient := commonredis.RDB
	commonredis.RDB = redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	t.Cleanup(func() {
		commonredis.RDB = originalRedisClient
	})

	originalTTL := refreshLockTTL
	originalPollInterval := refreshLockPollInterval
	originalReloadInterval := refreshCredentialReloadInterval
	refreshLockTTL = 40 * time.Millisecond
	refreshLockPollInterval = 2 * time.Millisecond
	refreshCredentialReloadInterval = 15 * time.Millisecond
	t.Cleanup(func() {
		refreshLockTTL = originalTTL
		refreshLockPollInterval = originalPollInterval
		refreshCredentialReloadInterval = originalReloadInterval
	})

	originalSetNX := acquireDistributedRefreshLockSetNX
	acquireDistributedRefreshLockSetNX = func(ctx context.Context, key string, value any, ttl time.Duration) (bool, error) {
		return false, nil
	}
	t.Cleanup(func() {
		acquireDistributedRefreshLockSetNX = originalSetNX
	})

	loadCount := 0
	expiredAt := time.Now().Add(-time.Minute).Format(time.RFC3339)
	originalLoadLatestChannelByID := loadLatestChannelByID
	loadLatestChannelByID = func(channelID int) (*model.Channel, error) {
		loadCount++
		return &model.Channel{
			Id: channelID,
			Key: `{
				"access_token":"access",
				"refresh_token":"refresh",
				"expires_at":"` + expiredAt + `"
			}`,
		}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: 424246},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(-time.Minute),
		},
	}

	start := time.Now()
	lock, handledByPeer, err := provider.acquireDistributedRefreshLock(context.Background(), 3*time.Minute)
	if err == nil {
		t.Fatalf("expected lock wait to stop after timeout")
	}
	if lock != nil {
		t.Fatalf("expected no lock to be acquired while another node holds it")
	}
	if handledByPeer {
		t.Fatalf("expected timeout path instead of peer refresh completion")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("expected lock wait timeout to cap the wait, took %v", elapsed)
	}

	maxExpectedReloads := 1 + int(refreshLockTTL/refreshCredentialReloadInterval) + 2
	if loadCount > maxExpectedReloads {
		t.Fatalf("expected database reloads to be throttled, got %d (> %d)", loadCount, maxExpectedReloads)
	}
}

func TestAcquireChannelRefreshLockCleansUpUnusedEntry(t *testing.T) {
	channelID := 424244

	channelRefreshLocks.mu.Lock()
	delete(channelRefreshLocks.locks, channelID)
	channelRefreshLocks.mu.Unlock()

	release := acquireChannelRefreshLock(channelID)

	channelRefreshLocks.mu.Lock()
	if _, ok := channelRefreshLocks.locks[channelID]; !ok {
		channelRefreshLocks.mu.Unlock()
		t.Fatalf("expected lock entry to exist while held")
	}
	channelRefreshLocks.mu.Unlock()

	release()

	channelRefreshLocks.mu.Lock()
	_, ok := channelRefreshLocks.locks[channelID]
	channelRefreshLocks.mu.Unlock()
	if ok {
		t.Fatalf("expected lock entry to be cleaned up after release")
	}
}
