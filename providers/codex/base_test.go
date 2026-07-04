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
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"

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

func primeCachedCredentialSnapshot(t *testing.T, channelID int, accessToken, accountID string, expiresAt time.Time, ttl time.Duration) {
	t.Helper()

	if err := cache.SetCache(tokenCacheKey(channelID), cachedAccessToken{
		AccessToken: accessToken,
		AccountID:   accountID,
		ExpiresAt:   expiresAt,
	}, ttl); err != nil {
		t.Fatalf("failed to prime credential cache: %v", err)
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

func newTestCodexProviderWithContext(t *testing.T, key string, other string, headers map[string]string) *CodexProvider {
	t.Helper()

	channel := &model.Channel{
		Id:    424299,
		Key:   key,
		Other: other,
	}
	channel.SetProxy()

	provider, ok := CodexProviderFactory{}.Create(channel).(*CodexProvider)
	if !ok || provider == nil {
		t.Fatalf("expected Codex provider instance")
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req, err := http.NewRequest(http.MethodPost, "/", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	ctx.Request = req
	ctx.Set("self_hosted", true)
	ctx.Set("responses_ws_self_hosted", true)
	provider.Context = ctx

	return provider
}

func TestBuildExecutionSessionMetadataPrefersXSessionIDOverConversationSessionID(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"session_id":   "conversation-session-123",
		"Session-Id":   "native-session-789",
		"X-Session-Id": "execution-session-456",
	})
	provider.Context.Set("token_id", 12345)

	meta, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimerealtime.RealtimeOpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected execution session metadata to build, got %v", errWithCode)
	}

	expectedBindingKey := runtimesession.BuildBindingKey("token:12345", runtimesession.BindingScopeChatRealtime, "execution-session-456")
	if meta.BindingKey != expectedBindingKey {
		t.Fatalf("expected binding key from x-session-id, got %q", meta.BindingKey)
	}
	if !meta.ClientSuppliedID {
		t.Fatal("expected explicit x-session-id to be marked as client supplied")
	}
	if meta.SessionID == "" {
		t.Fatal("expected generated upstream execution session id")
	}
	channelID, compatibilityHash, upstreamSessionID, ok := parseCodexExecutionSessionKey(meta.Key)
	if !ok {
		t.Fatalf("expected parsable execution session key, got %q", meta.Key)
	}
	if channelID != provider.Channel.Id {
		t.Fatalf("expected session key channel #%d, got #%d", provider.Channel.Id, channelID)
	}
	if compatibilityHash != provider.buildRealtimeCompatibilityHash("gpt-5", provider.readRealtimeUpstreamIdentity()) {
		t.Fatalf("expected compatibility hash to match current channel handshake policy, got %q", compatibilityHash)
	}
	if upstreamSessionID != meta.SessionID {
		t.Fatalf("expected execution key session id %q to match metadata, got %q", meta.SessionID, upstreamSessionID)
	}
}

func TestBuildExecutionSessionMetadataUsesNativeSessionID(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"session_id": "legacy-session-123",
		"Session-Id": "native-session-789",
	})
	provider.Context.Set("token_id", 12345)

	meta, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimerealtime.RealtimeOpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected execution session metadata to build, got %v", errWithCode)
	}

	expectedBindingKey := runtimesession.BuildBindingKey("token:12345", runtimesession.BindingScopeChatRealtime, "native-session-789")
	if meta.BindingKey != expectedBindingKey {
		t.Fatalf("expected binding key from native session-id, got %q", meta.BindingKey)
	}
	if !meta.ClientSuppliedID {
		t.Fatal("expected native session-id to be marked as client supplied")
	}
}

func TestBuildExecutionSessionMetadataUsesResolvedUpstreamSessionIDWhenProvided(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Context.Set("token_id", 12346)

	first, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimerealtime.RealtimeOpenOptions{
		ResolvedUpstreamSessionID: "upstream-session-456",
	})
	if errWithCode != nil {
		t.Fatalf("expected first execution session metadata to build, got %v", errWithCode)
	}
	second, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimerealtime.RealtimeOpenOptions{
		ResolvedUpstreamSessionID: "upstream-session-456",
	})
	if errWithCode != nil {
		t.Fatalf("expected second execution session metadata to build, got %v", errWithCode)
	}

	if first.SessionID != "upstream-session-456" || second.SessionID != "upstream-session-456" {
		t.Fatalf("expected resolved upstream session id to be preserved, got %q and %q", first.SessionID, second.SessionID)
	}
	if first.ClientSuppliedID || second.ClientSuppliedID {
		t.Fatal("expected resolved upstream session id not to be marked as client supplied")
	}
	if first.Key != second.Key {
		t.Fatalf("expected explicit upstream execution session key to remain stable, got %q then %q", first.Key, second.Key)
	}
}

func TestBuildExecutionSessionMetadataSeparatesCapacityNamespaceFromCallerNamespace(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"X-Session-Id": "execution-session-789",
	})
	provider.Context.Set("id", 77)
	provider.Context.Set("token_id", 12347)

	meta, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimerealtime.RealtimeOpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected execution session metadata to build, got %v", errWithCode)
	}
	if meta.CallerNS != "token:12347" {
		t.Fatalf("expected caller namespace to remain token-scoped for binding isolation, got %q", meta.CallerNS)
	}
	if meta.CapacityNS != "user:77" {
		t.Fatalf("expected capacity namespace to be user-scoped, got %q", meta.CapacityNS)
	}
}

func TestBuildExecutionSessionMetadataNormalizesFallbackCallerNamespaceFromCanonicalAuth(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	providerA := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"Authorization": "bearer sk-shared-auth-token#7#ignore",
	})
	providerB := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"x-api-key": "shared-auth-token",
	})

	metaA, errWithCode := providerA.buildExecutionSessionMetadata("gpt-5", runtimerealtime.RealtimeOpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected providerA metadata to build, got %v", errWithCode)
	}
	metaB, errWithCode := providerB.buildExecutionSessionMetadata("gpt-5", runtimerealtime.RealtimeOpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected providerB metadata to build, got %v", errWithCode)
	}

	if metaA.CallerNS == "" || metaA.CallerNS == "anonymous" {
		t.Fatalf("expected auth-derived caller namespace, got %q", metaA.CallerNS)
	}
	if metaA.CallerNS != metaB.CallerNS {
		t.Fatalf("expected caller namespace normalization to be transport-agnostic, got %q and %q", metaA.CallerNS, metaB.CallerNS)
	}
}

func TestBuildExecutionSessionMetadataRejectsInvalidSessionID(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"X-Session-Id": "bad/session",
	})

	_, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimerealtime.RealtimeOpenOptions{})
	if errWithCode == nil {
		t.Fatal("expected invalid execution session id to be rejected")
	}
	if errWithCode.Code != "invalid_session_id" {
		t.Fatalf("expected invalid_session_id code, got %v", errWithCode.Code)
	}
}

func TestBuildExecutionSessionMetadataRejectsOverlongSessionID(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"X-Session-Id": strings.Repeat("a", runtimesession.ClientSessionIDMaxLen+1),
	})

	_, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimerealtime.RealtimeOpenOptions{})
	if errWithCode == nil {
		t.Fatal("expected overlong execution session id to be rejected")
	}
	if errWithCode.Code != "invalid_session_id" {
		t.Fatalf("expected invalid_session_id code, got %v", errWithCode.Code)
	}
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

func TestGetTokenCacheHitAdoptsAccessTokenAndAccountID(t *testing.T) {
	cache.InitCacheManager()

	channelID := 424252
	cacheKey := tokenCacheKey(channelID)
	_ = cache.DeleteCache(cacheKey)
	defer cache.DeleteCache(cacheKey)

	primeCachedCredentialSnapshot(t, channelID, "fresh-access-token", "acct-fresh", time.Now().Add(30*time.Minute), time.Minute)

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: channelID},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "stale-access-token",
			AccountID:    "acct-stale",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(-time.Minute),
		},
	}

	token, err := provider.GetToken()
	if err != nil {
		t.Fatalf("expected cache hit to avoid refresh, got error: %v", err)
	}
	if token != "fresh-access-token" {
		t.Fatalf("expected cached access token, got %q", token)
	}
	if provider.Credentials.AccessToken != "fresh-access-token" || provider.Credentials.AccountID != "acct-fresh" {
		t.Fatalf("expected cached credential snapshot adoption, got token=%q account=%q", provider.Credentials.AccessToken, provider.Credentials.AccountID)
	}
}

func TestGetTokenLegacyStringCacheClearsStaleAccountIDWhenTokenHasNoAccountID(t *testing.T) {
	cache.InitCacheManager()

	channelID := 424253
	cacheKey := tokenCacheKey(channelID)
	_ = cache.DeleteCache(cacheKey)
	defer cache.DeleteCache(cacheKey)

	if err := cache.SetCache(cacheKey, "legacy-opaque-token", time.Minute); err != nil {
		t.Fatalf("failed to prime legacy string cache: %v", err)
	}

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: channelID},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "local-access-token",
			AccountID:    "acct-stale",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(30 * time.Minute),
		},
	}

	token, err := provider.GetToken()
	if err != nil {
		t.Fatalf("expected legacy cache hit to avoid refresh, got error: %v", err)
	}
	if token != "legacy-opaque-token" {
		t.Fatalf("expected legacy cached token, got %q", token)
	}
	if provider.Credentials.AccountID != "" {
		t.Fatalf("expected stale account id to be cleared when legacy token has no account id, got %q", provider.Credentials.AccountID)
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
	primeCachedCredentialSnapshot(t, channelID, "fresh-access-token", "acct-fresh", expiresAt, time.Minute)

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: channelID},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "stale-access-token",
			AccountID:    "acct-stale",
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
	if provider.Credentials.AccountID != "acct-fresh" {
		t.Fatalf("expected account id to be updated from cache, got %q", provider.Credentials.AccountID)
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

func TestLoadLatestCredentialsFromDatabaseReloadsChannelOptions(t *testing.T) {
	logger.SetupLogger()

	initialCreds := &OAuth2Credentials{
		AccessToken:  "initial-access-token",
		RefreshToken: "initial-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	initialKey, err := initialCreds.ToJSON()
	if err != nil {
		t.Fatalf("failed to serialize initial credentials: %v", err)
	}

	sharedChannel := &model.Channel{
		Id:    424253,
		Key:   initialKey,
		Other: `{"websocket_mode":"off","prompt_cache_key_strategy":"off","execution_session_ttl_seconds":60}`,
	}

	provider, ok := CodexProviderFactory{}.Create(sharedChannel).(*CodexProvider)
	if !ok || provider == nil {
		t.Fatalf("expected Codex provider instance")
	}

	if got := provider.getWebsocketMode(); got != codexWebsocketModeOff {
		t.Fatalf("expected initial websocket mode off, got %q", got)
	}
	if got := provider.getExecutionSessionTTL(); got != time.Minute {
		t.Fatalf("expected initial execution session TTL %s, got %s", time.Minute, got)
	}
	if got := provider.getPromptCacheKeyStrategy(); got != codexPromptCacheStrategyOff {
		t.Fatalf("expected initial prompt cache strategy off, got %q", got)
	}
	latestCreds := &OAuth2Credentials{
		AccessToken:  "latest-access-token",
		RefreshToken: "latest-refresh-token",
		ExpiresAt:    time.Now().Add(2 * time.Hour),
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
		return &model.Channel{
			Id:    channelID,
			Key:   latestKey,
			Other: `{"websocket_mode":"force","prompt_cache_key_strategy":"auth_header","execution_session_ttl_seconds":180}`,
		}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	if err := provider.loadLatestCredentialsFromDatabase(); err != nil {
		t.Fatalf("expected runtime channel reload to succeed, got %v", err)
	}

	if got := provider.getWebsocketMode(); got != codexWebsocketModeForce {
		t.Fatalf("expected reloaded websocket mode force, got %q", got)
	}
	if got := provider.getExecutionSessionTTL(); got != 3*time.Minute {
		t.Fatalf("expected reloaded execution session TTL %s, got %s", 3*time.Minute, got)
	}
	if got := provider.getPromptCacheKeyStrategy(); got != codexPromptCacheStrategyAuthHeader {
		t.Fatalf("expected reloaded prompt cache strategy auth_header, got %q", got)
	}
}

func TestLoadLatestCredentialsFromDatabaseReloadsChannelOptionsAfterInvalidOther(t *testing.T) {
	logger.SetupLogger()

	initialCreds := &OAuth2Credentials{
		AccessToken:  "initial-access-token",
		RefreshToken: "initial-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	initialKey, err := initialCreds.ToJSON()
	if err != nil {
		t.Fatalf("failed to serialize initial credentials: %v", err)
	}

	sharedChannel := &model.Channel{
		Id:    424254,
		Key:   initialKey,
		Other: `{"websocket_mode":`,
	}

	provider, ok := CodexProviderFactory{}.Create(sharedChannel).(*CodexProvider)
	if !ok || provider == nil {
		t.Fatalf("expected Codex provider instance")
	}

	if got := provider.getWebsocketMode(); got != codexWebsocketModeAuto {
		t.Fatalf("expected invalid initial options to fall back to auto websocket mode, got %q", got)
	}
	if !provider.channelOptionsLoaded {
		t.Fatalf("expected invalid initial options to mark cache as loaded")
	}
	if provider.channelOptions != nil {
		t.Fatalf("expected invalid initial options to leave cached options nil")
	}

	latestCreds := &OAuth2Credentials{
		AccessToken:  "latest-access-token",
		RefreshToken: "latest-refresh-token",
		ExpiresAt:    time.Now().Add(2 * time.Hour),
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
		return &model.Channel{
			Id:    channelID,
			Key:   latestKey,
			Other: `{"websocket_mode":"force","prompt_cache_key_strategy":"user_id","execution_session_ttl_seconds":240}`,
		}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	if err := provider.loadLatestCredentialsFromDatabase(); err != nil {
		t.Fatalf("expected runtime channel reload to succeed, got %v", err)
	}

	if got := provider.getWebsocketMode(); got != codexWebsocketModeForce {
		t.Fatalf("expected reloaded websocket mode force, got %q", got)
	}
	if got := provider.getExecutionSessionTTL(); got != 4*time.Minute {
		t.Fatalf("expected reloaded execution session TTL %s, got %s", 4*time.Minute, got)
	}
	if got := provider.getPromptCacheKeyStrategy(); got != codexPromptCacheStrategyUserID {
		t.Fatalf("expected reloaded prompt cache strategy user_id, got %q", got)
	}
}

func TestRefreshNoLongerNeededUsesCachedToken(t *testing.T) {
	cache.InitCacheManager()

	channelID := 424243
	cacheKey := tokenCacheKey(channelID)
	_ = cache.DeleteCache(cacheKey)
	defer cache.DeleteCache(cacheKey)

	primeCachedCredentialSnapshot(t, channelID, "shared-access-token", "acct-shared", time.Now().Add(30*time.Minute), time.Minute)

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: channelID},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "stale-access-token",
			AccountID:    "acct-stale",
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
	if provider.Credentials.AccountID != "acct-shared" {
		t.Fatalf("expected account id to be updated from cache, got %q", provider.Credentials.AccountID)
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

func TestForceRefreshTokenTreatsChangedDatabaseTokenAsPeerHandled(t *testing.T) {
	cache.InitCacheManager()

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
		return false, nil
	}
	t.Cleanup(func() {
		acquireDistributedRefreshLockSetNX = originalSetNX
	})

	latestCreds := &OAuth2Credentials{
		AccessToken:  "peer-refreshed-access-token",
		RefreshToken: "peer-refreshed-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	initialCreds := &OAuth2Credentials{
		AccessToken:  "stale-401-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-time.Minute),
	}
	latestKey, err := latestCreds.ToJSON()
	if err != nil {
		t.Fatalf("failed to serialize latest credentials: %v", err)
	}
	initialKey, err := initialCreds.ToJSON()
	if err != nil {
		t.Fatalf("failed to serialize initial credentials: %v", err)
	}

	loadCount := 0
	originalLoadLatestChannelByID := loadLatestChannelByID
	loadLatestChannelByID = func(channelID int) (*model.Channel, error) {
		loadCount++
		if channelID != 424254 {
			t.Fatalf("unexpected channel id lookup: got %d want %d", channelID, 424254)
		}
		key := latestKey
		if loadCount == 1 {
			key = initialKey
		}
		return &model.Channel{
			Id:  channelID,
			Key: key,
		}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	refreshCalls := 0
	originalRefreshCredentials := refreshOAuthCredentials
	refreshOAuthCredentials = func(creds *OAuth2Credentials, ctx context.Context, proxyURL string, maxRetries int) error {
		refreshCalls++
		return nil
	}
	t.Cleanup(func() {
		refreshOAuthCredentials = originalRefreshCredentials
	})

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: 424254},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "stale-401-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(-time.Minute),
		},
	}

	refreshed, err := provider.forceRefreshToken(context.Background())
	if err != nil {
		t.Fatalf("expected peer refresh detection to avoid an error, got %v", err)
	}
	if !refreshed {
		t.Fatalf("expected force refresh path to treat the peer update as handled")
	}
	if refreshCalls != 0 {
		t.Fatalf("expected no local refresh once another request already persisted new credentials, got %d refresh calls", refreshCalls)
	}
	if provider.Credentials.AccessToken != "peer-refreshed-access-token" {
		t.Fatalf("expected provider credentials to adopt the peer-refreshed token, got %q", provider.Credentials.AccessToken)
	}
	if cachedCredentials := provider.getCachedCredentialSnapshot(0); cachedCredentials.AccessToken != "peer-refreshed-access-token" {
		t.Fatalf("expected handled-by-peer path to recache the refreshed token, got %q", cachedCredentials.AccessToken)
	}
	if loadCount < 2 {
		t.Fatalf("expected forced refresh coordination to reload credentials before and during peer detection, got %d loads", loadCount)
	}
}

func TestForceRefreshTokenTreatsReloadedCredentialsAsPeerHandledWithoutRedis(t *testing.T) {
	cache.InitCacheManager()

	channelID := 424255
	cacheKey := tokenCacheKey(channelID)
	_ = cache.DeleteCache(cacheKey)
	defer cache.DeleteCache(cacheKey)

	latestCreds := &OAuth2Credentials{
		AccessToken:  "peer-refreshed-access-token",
		RefreshToken: "peer-refreshed-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	latestKey, err := latestCreds.ToJSON()
	if err != nil {
		t.Fatalf("failed to serialize latest credentials: %v", err)
	}

	loadCount := 0
	originalLoadLatestChannelByID := loadLatestChannelByID
	loadLatestChannelByID = func(id int) (*model.Channel, error) {
		loadCount++
		if id != channelID {
			t.Fatalf("unexpected channel id lookup: got %d want %d", id, channelID)
		}
		return &model.Channel{Id: id, Key: latestKey}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	refreshCalls := 0
	originalRefreshCredentials := refreshOAuthCredentials
	refreshOAuthCredentials = func(creds *OAuth2Credentials, ctx context.Context, proxyURL string, maxRetries int) error {
		refreshCalls++
		return nil
	}
	t.Cleanup(func() {
		refreshOAuthCredentials = originalRefreshCredentials
	})

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: channelID},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  "stale-401-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(-time.Minute),
		},
	}

	refreshed, err := provider.forceRefreshToken(context.Background())
	if err != nil {
		t.Fatalf("expected local peer detection to avoid an error, got %v", err)
	}
	if !refreshed {
		t.Fatalf("expected forced refresh to treat newly loaded credentials as handled")
	}
	if refreshCalls != 0 {
		t.Fatalf("expected no local refresh after reloading newer credentials, got %d refresh calls", refreshCalls)
	}
	if provider.Credentials.AccessToken != "peer-refreshed-access-token" {
		t.Fatalf("expected provider credentials to reload the peer-refreshed token, got %q", provider.Credentials.AccessToken)
	}
	if cachedCredentials := provider.getCachedCredentialSnapshot(0); cachedCredentials.AccessToken != "peer-refreshed-access-token" {
		t.Fatalf("expected reloaded credentials to be recached, got %q", cachedCredentials.AccessToken)
	}
	if loadCount != 1 {
		t.Fatalf("expected only the initial reload to be needed without redis coordination, got %d loads", loadCount)
	}
}

func TestForceRefreshTokenTreatsChangedRefreshStateAsPeerHandled(t *testing.T) {
	cache.InitCacheManager()

	channelID := 424256
	cacheKey := tokenCacheKey(channelID)
	_ = cache.DeleteCache(cacheKey)
	defer cache.DeleteCache(cacheKey)

	accessToken := "stable-access-token"
	latestCreds := &OAuth2Credentials{
		AccessToken:  accessToken,
		RefreshToken: "peer-rotated-refresh-token",
		ExpiresAt:    time.Now().Add(2 * time.Hour),
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
		return &model.Channel{Id: id, Key: latestKey}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	refreshCalls := 0
	originalRefreshCredentials := refreshOAuthCredentials
	refreshOAuthCredentials = func(creds *OAuth2Credentials, ctx context.Context, proxyURL string, maxRetries int) error {
		refreshCalls++
		return nil
	}
	t.Cleanup(func() {
		refreshOAuthCredentials = originalRefreshCredentials
	})

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: channelID},
			},
		},
		Credentials: &OAuth2Credentials{
			AccessToken:  accessToken,
			RefreshToken: "stale-refresh-token",
			ExpiresAt:    time.Now().Add(-time.Minute),
		},
	}

	refreshed, err := provider.forceRefreshToken(context.Background())
	if err != nil {
		t.Fatalf("expected changed refresh state to be treated as handled, got %v", err)
	}
	if !refreshed {
		t.Fatalf("expected forced refresh to stop once refresh state changed in storage")
	}
	if refreshCalls != 0 {
		t.Fatalf("expected no extra refresh when only refresh token/expiry changed, got %d refresh calls", refreshCalls)
	}
	if provider.Credentials.RefreshToken != "peer-rotated-refresh-token" {
		t.Fatalf("expected provider credentials to reload the rotated refresh token, got %q", provider.Credentials.RefreshToken)
	}
	if !provider.Credentials.ExpiresAt.Equal(latestCreds.ExpiresAt) {
		t.Fatalf("expected provider expiry to reload from storage, got %s want %s", provider.Credentials.ExpiresAt, latestCreds.ExpiresAt)
	}
	if cachedCredentials := provider.getCachedCredentialSnapshot(0); cachedCredentials.AccessToken != accessToken {
		t.Fatalf("expected unchanged access token to be recached after peer handling, got %q", cachedCredentials.AccessToken)
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

func TestIsCodexOfficialClientRequest(t *testing.T) {
	tests := []struct {
		name      string
		userAgent string
		want      bool
	}{
		{"empty", "", false},
		{"codex_cli_rs", "codex_cli_rs/0.116.0", true},
		{"codex_vscode", "codex_vscode/1.0.0", true},
		{"codex_app", "codex_app/2.1.0", true},
		{"codex_chatgpt_desktop", "codex_chatgpt_desktop/1.0", true},
		{"codex_atlas", "codex_atlas/0.5", true},
		{"codex_exec", "codex_exec/3.0", true},
		{"codex_sdk_ts", "codex_sdk_ts/0.9", true},
		{"codex desktop with version", "Codex Desktop/1.0", true},
		{"codex desktop case insensitive", "CODEX DESKTOP/2.0", true},
		{"codex slash prefix", "codex/1.0", true},
		{"codex hyphen prefix", "codex-tui/1.0", true},
		{"codex bare prefix", "CodexCanary/1.0", true},
		{"non-codex curl", "curl/8.0", false},
		{"non-codex browser", "Mozilla/5.0", false},
		{"codex substring only", "my-codex-tool/1.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCodexOfficialClientRequest(tt.userAgent); got != tt.want {
				t.Fatalf("isCodexOfficialClientRequest(%q) = %v, want %v", tt.userAgent, got, tt.want)
			}
		})
	}
}

func TestIsCodexOfficialClientOriginator(t *testing.T) {
	tests := []struct {
		name       string
		originator string
		want       bool
	}{
		{"empty", "", false},
		{"codex_cli_rs", "codex_cli_rs", true},
		{"codex_vscode", "codex_vscode", true},
		{"codex_app", "codex_app", true},
		{"codex_chatgpt_desktop", "codex_chatgpt_desktop", true},
		{"codex_atlas", "codex_atlas", true},
		{"codex_exec", "codex_exec", true},
		{"codex_sdk_ts", "codex_sdk_ts", true},
		{"codex desktop", "Codex Desktop", true},
		{"codex desktop case insensitive", "CODEX DESKTOP", true},
		{"codex slash prefix", "codex/cli", true},
		{"codex hyphen prefix", "codex-tui", true},
		{"codex bare prefix", "CodexCanary", true},
		{"non-codex", "my_client", false},
		{"codex substring only", "my-codex-client", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCodexOfficialClientOriginator(tt.originator); got != tt.want {
				t.Fatalf("isCodexOfficialClientOriginator(%q) = %v, want %v", tt.originator, got, tt.want)
			}
		})
	}
}

func TestIsCodexOfficialClientByHeaders(t *testing.T) {
	tests := []struct {
		name       string
		userAgent  string
		originator string
		want       bool
	}{
		{"both empty", "", "", false},
		{"UA official only", "codex_cli_rs/1.0", "", true},
		{"originator official only", "curl/8.0", "codex_vscode", true},
		{"both official", "codex_app/3.0", "codex_chatgpt_desktop", true},
		{"neither official", "curl/8.0", "my_client", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCodexOfficialClientByHeaders(tt.userAgent, tt.originator); got != tt.want {
				t.Fatalf("isCodexOfficialClientByHeaders(%q, %q) = %v, want %v", tt.userAgent, tt.originator, got, tt.want)
			}
		})
	}
}

func TestResolveSmartOriginatorForEffectiveUserAgent(t *testing.T) {
	tests := []struct {
		name      string
		userAgent string
		want      string
	}{
		{"empty effective UA", "", defaultNonOfficialCodexOriginator},
		{"official client UA", "codex_cli_rs/0.116.0", defaultOfficialCodexOriginator},
		{"codex-tui client UA", "codex-tui/1.0", defaultOfficialCodexOriginator},
		{"non-official client UA", "curl/8.0", defaultNonOfficialCodexOriginator},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveSmartOriginatorForEffectiveUserAgent(tt.userAgent); got != tt.want {
				t.Fatalf("resolveSmartOriginatorForEffectiveUserAgent(%q) = %q, want %q", tt.userAgent, got, tt.want)
			}
		})
	}
}
