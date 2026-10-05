package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/utils"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"

	"github.com/gin-gonic/gin"
)

func primeCachedToken(t *testing.T, channelID int, accessToken string, expiresAt time.Time, ttl time.Duration) {
	primeCachedTokenForKey(t, channelID, "", accessToken, expiresAt, ttl)
}

func primeCachedTokenForKey(t *testing.T, channelID int, durableKey, accessToken string, expiresAt time.Time, ttl time.Duration) {
	t.Helper()

	cacheKey := tokenCacheKeyV2(channelID, durableKey)
	if err := cache.SetCache(cacheKey, cachedAccessToken{
		AccessToken: accessToken,
		ExpiresAt:   expiresAt,
	}, ttl); err != nil {
		t.Fatalf("failed to prime cache: %v", err)
	}
	t.Cleanup(func() { _ = cache.DeleteCache(cacheKey) })
}

func primeCachedCredentialSnapshot(t *testing.T, channelID int, accessToken, accountID string, expiresAt time.Time, ttl time.Duration) {
	t.Helper()

	cacheKey := tokenCacheKeyV2(channelID, "")
	if err := cache.SetCache(cacheKey, cachedAccessToken{
		AccessToken: accessToken,
		AccountID:   accountID,
		ExpiresAt:   expiresAt,
	}, ttl); err != nil {
		t.Fatalf("failed to prime credential cache: %v", err)
	}
	t.Cleanup(func() { _ = cache.DeleteCache(cacheKey) })
}

func stubLatestChannelByIDForTest(t *testing.T, channelID int, creds *OAuth2Credentials) {
	t.Helper()

	key, err := creds.ToJSON()
	if err != nil {
		t.Fatalf("failed to serialize credentials: %v", err)
	}

	originalLoadLatestChannelByID := loadLatestChannelByID
	loadLatestChannelByID = func(_ context.Context, id int) (*model.Channel, error) {
		if id != channelID {
			t.Fatalf("unexpected channel id lookup: got %d want %d", id, channelID)
		}
		return &model.Channel{Id: id, Type: config.ChannelTypeCodex, Key: key}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})
}

func stubTokenRefreshFailure(t *testing.T) {
	t.Helper()
	originalRefresh := refreshOAuthCredentials
	refreshOAuthCredentials = func(*OAuth2Credentials, context.Context, string) error {
		return errors.New("refresh unavailable")
	}
	t.Cleanup(func() { refreshOAuthCredentials = originalRefresh })
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
	provider.Context = ctx

	return provider
}

func enableCodexResponsesWSSelfHostedForTest(t *testing.T, provider *CodexProvider) {
	t.Helper()
	provider.Channel.Other = mergeCodexTestOther(t, provider.Channel.Other, map[string]any{
		"responses_ws_self_hosted": true,
	})
}

func mergeCodexTestOther(t *testing.T, raw string, fields map[string]any) string {
	t.Helper()
	other := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &other); err != nil {
			t.Fatalf("decode Codex test Other: %v", err)
		}
	}
	for key, value := range fields {
		other[key] = value
	}
	encoded, err := json.Marshal(other)
	if err != nil {
		t.Fatalf("encode Codex test Other: %v", err)
	}
	return string(encoded)
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
	if compatibilityHash != requireRealtimeCompatibilityHash(t, provider, "gpt-5", provider.readRealtimeUpstreamIdentity()) {
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
	stubTokenRefreshFailure(t)

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
	loadLatestChannelByID = func(_ context.Context, id int) (*model.Channel, error) {
		if id != channelID {
			t.Fatalf("unexpected channel id lookup: got %d want %d", id, channelID)
		}
		proxy := "http://proxy.example/%s"
		return &model.Channel{Id: id, Type: config.ChannelTypeCodex, Key: latestKey, Proxy: &proxy}, nil
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

	token, err := provider.getToken(context.Background())
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
	useCodexFenceDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()
	stubTokenRefreshFailure(t)

	channelID := 424249
	latestCredentials := &OAuth2Credentials{
		AccessToken:  "expired-db-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-time.Minute),
	}
	latestKey, err := latestCredentials.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	primeCachedTokenForKey(t, channelID, latestKey, "cached-still-valid-token", time.Now().Add(2*time.Minute), time.Minute)
	stubLatestChannelByIDForTest(t, channelID, latestCredentials)

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

	token, err := provider.getToken(context.Background())
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

func TestGetTokenIgnoresNonStructuredV2CachePayload(t *testing.T) {
	useCodexFenceDB(t)
	cache.InitCacheManager()

	channelID := 424253
	credentials := &OAuth2Credentials{
		AccessToken:  "local-access-token",
		AccountID:    "acct-stale",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(30 * time.Minute),
	}
	durableKey, err := credentials.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	cacheKey := tokenCacheKeyV2(channelID, durableKey)
	_ = cache.DeleteCache(cacheKey)
	t.Cleanup(func() { _ = cache.DeleteCache(cacheKey) })

	if err := cache.SetCache(cacheKey, "invalid-string-payload", time.Minute); err != nil {
		t.Fatalf("failed to prime invalid v2 payload: %v", err)
	}
	stubLatestChannelByIDForTest(t, channelID, credentials)

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Channel: &model.Channel{Id: channelID, Key: durableKey},
			},
		},
		Credentials: cloneOAuth2Credentials(credentials),
	}

	token, err := provider.GetToken()
	if err != nil {
		t.Fatalf("expected valid local token, got error: %v", err)
	}
	if token != "local-access-token" {
		t.Fatalf("non-structured v2 payload must not become a credential, got %q", token)
	}
	entry, err := cache.GetCache[cachedAccessToken](cacheKey)
	if err != nil || entry.AccessToken != "local-access-token" {
		t.Fatalf("valid local credential did not replace invalid v2 payload: entry=%+v err=%v", entry, err)
	}
}

func TestForceRefreshClearsCurrentFingerprintBeforeAuthorityReloadFailure(t *testing.T) {
	originalRedisEnabled := config.RedisEnabled
	config.RedisEnabled = false
	t.Cleanup(func() { config.RedisEnabled = originalRedisEnabled })
	cache.InitCacheManager()

	const channelID = 424257
	credentials := &OAuth2Credentials{
		AccessToken:  "provider-rejected-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	durableKey, err := credentials.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	primeCachedTokenForKey(t, channelID, durableKey, credentials.AccessToken, credentials.ExpiresAt, time.Minute)

	originalLoad := loadLatestChannelByID
	loadLatestChannelByID = func(context.Context, int) (*model.Channel, error) {
		return nil, errors.New("database unavailable")
	}
	t.Cleanup(func() { loadLatestChannelByID = originalLoad })

	provider := CodexProviderFactory{}.Create(&model.Channel{Id: channelID, Key: durableKey}).(*CodexProvider)
	if refreshed, err := provider.forceRefreshToken(context.Background()); refreshed || err == nil {
		t.Fatalf("authority reload failure must stop forced refresh: refreshed=%t err=%v", refreshed, err)
	}
	if _, err := cache.GetCache[cachedAccessToken](tokenCacheKeyV2(channelID, durableKey)); !errors.Is(err, cache.CacheNotFound) {
		t.Fatalf("provider-rejected fingerprint remained cached after forced refresh: %v", err)
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled operation to surface, got %v", err)
	}
}

func TestGetTokenCanceledContextStopsCacheReadWithoutPublishing(t *testing.T) {
	cache.InitCacheManager()
	credentials := &OAuth2Credentials{
		AccessToken:  "must-not-be-published",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(30 * time.Minute),
	}
	key, err := credentials.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	channel := &model.Channel{Id: 424241, Key: key}
	provider := CodexProviderFactory{}.Create(channel).(*CodexProvider)
	cacheKey := tokenCacheKeyV2(channel.Id, channel.Key)
	_ = cache.DeleteCache(cacheKey)
	t.Cleanup(func() { _ = cache.DeleteCache(cacheKey) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	token, err := provider.getToken(ctx)
	if !errors.Is(err, context.Canceled) || token != "" {
		t.Fatalf("canceled token read = %q, %v; want empty token, context.Canceled", token, err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("canceled token cache read took %s", elapsed)
	}
	if _, cacheErr := cache.GetCache[cachedAccessToken](cacheKey); !errors.Is(cacheErr, cache.CacheNotFound) {
		t.Fatalf("canceled operation published token: %v", cacheErr)
	}
}

func TestRefreshTokenIfNeededCanceledContextDoesNotReadCachedToken(t *testing.T) {
	cache.InitCacheManager()

	channelID := 424242
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

	started := time.Now()
	refreshed, err := provider.refreshTokenIfNeeded(ctx, 3*time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled token cache read returned %v, want context.Canceled", err)
	}
	if refreshed {
		t.Fatal("canceled token cache read must not report a refresh")
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("canceled token cache read took %s", elapsed)
	}
	if provider.Credentials.AccessToken != "stale-access-token" || provider.Credentials.AccountID != "acct-stale" {
		t.Fatalf("canceled token cache read published cached credentials: %+v", provider.Credentials)
	}
}

func TestRefreshTokenIfNeededIgnoresCachedTokenWithinLead(t *testing.T) {
	cache.InitCacheManager()

	channelID := 424248
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled refresh error, got %v", err)
	}
	if provider.Credentials.AccessToken != "initial-access-token" {
		t.Fatalf("canceled cache read must not adopt or reload credentials, got %q", provider.Credentials.AccessToken)
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
	loadLatestChannelByID = func(_ context.Context, channelID int) (*model.Channel, error) {
		if channelID != sharedChannel.Id {
			t.Fatalf("unexpected channel id lookup: got %d want %d", channelID, sharedChannel.Id)
		}
		proxy := "http://proxy.example/%s"
		return &model.Channel{
			Type:  config.ChannelTypeCodex,
			Id:    channelID,
			Key:   latestKey,
			Proxy: &proxy,
		}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	if err := provider.loadLatestCredentialsFromDatabase(context.Background()); err != nil {
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
		Other: `{"execution_session_ttl_seconds":60}`,
	}

	provider, ok := CodexProviderFactory{}.Create(sharedChannel).(*CodexProvider)
	if !ok || provider == nil {
		t.Fatalf("expected Codex provider instance")
	}

	if got := provider.getExecutionSessionTTL(); got != time.Minute {
		t.Fatalf("expected initial execution session TTL %s, got %s", time.Minute, got)
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
	loadLatestChannelByID = func(_ context.Context, channelID int) (*model.Channel, error) {
		if channelID != sharedChannel.Id {
			t.Fatalf("unexpected channel id lookup: got %d want %d", channelID, sharedChannel.Id)
		}
		return &model.Channel{
			Type:  config.ChannelTypeCodex,
			Id:    channelID,
			Key:   latestKey,
			Other: `{"execution_session_ttl_seconds":180}`,
		}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	if err := provider.loadLatestCredentialsFromDatabase(context.Background()); err != nil {
		t.Fatalf("expected runtime channel reload to succeed, got %v", err)
	}

	if got := provider.getExecutionSessionTTL(); got != 3*time.Minute {
		t.Fatalf("expected reloaded execution session TTL %s, got %s", 3*time.Minute, got)
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
		Other: `{"execution_session_ttl_seconds":`,
	}

	provider, ok := CodexProviderFactory{}.Create(sharedChannel).(*CodexProvider)
	if !ok || provider == nil {
		t.Fatalf("expected Codex provider instance")
	}

	provider.getChannelOptions()
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
	loadLatestChannelByID = func(_ context.Context, channelID int) (*model.Channel, error) {
		if channelID != sharedChannel.Id {
			t.Fatalf("unexpected channel id lookup: got %d want %d", channelID, sharedChannel.Id)
		}
		return &model.Channel{
			Type:  config.ChannelTypeCodex,
			Id:    channelID,
			Key:   latestKey,
			Other: `{"execution_session_ttl_seconds":240}`,
		}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	if err := provider.loadLatestCredentialsFromDatabase(context.Background()); err != nil {
		t.Fatalf("expected runtime channel reload to succeed, got %v", err)
	}

	if got := provider.getExecutionSessionTTL(); got != 4*time.Minute {
		t.Fatalf("expected reloaded execution session TTL %s, got %s", 4*time.Minute, got)
	}
}

func TestForceRefreshTokenTreatsReloadedCredentialsAsPeerHandledWithoutRedis(t *testing.T) {
	useCodexFenceDB(t)
	cache.InitCacheManager()

	channelID := 424255
	latestCreds := &OAuth2Credentials{
		AccessToken:  "peer-refreshed-access-token",
		RefreshToken: "peer-refreshed-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	latestKey, err := latestCreds.ToJSON()
	if err != nil {
		t.Fatalf("failed to serialize latest credentials: %v", err)
	}
	t.Cleanup(func() { _ = cache.DeleteCache(tokenCacheKeyV2(channelID, latestKey)) })

	loadCount := 0
	originalLoadLatestChannelByID := loadLatestChannelByID
	loadLatestChannelByID = func(_ context.Context, id int) (*model.Channel, error) {
		loadCount++
		if id != channelID {
			t.Fatalf("unexpected channel id lookup: got %d want %d", id, channelID)
		}
		return &model.Channel{Id: id, Type: config.ChannelTypeCodex, Key: latestKey}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	refreshCalls := 0
	originalRefreshCredentials := refreshOAuthCredentials
	refreshOAuthCredentials = func(creds *OAuth2Credentials, ctx context.Context, proxyURL string) error {
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
	if cachedCredentials := provider.getCachedCredentialSnapshot(context.Background(), 0); cachedCredentials.AccessToken != "peer-refreshed-access-token" {
		t.Fatalf("expected reloaded credentials to be recached, got %q", cachedCredentials.AccessToken)
	}
	if loadCount != 1 {
		t.Fatalf("expected only the initial reload to be needed without redis coordination, got %d loads", loadCount)
	}
}

func TestForceRefreshTokenTreatsChangedRefreshStateAsPeerHandled(t *testing.T) {
	useCodexFenceDB(t)
	cache.InitCacheManager()

	channelID := 424256
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
	t.Cleanup(func() { _ = cache.DeleteCache(tokenCacheKeyV2(channelID, latestKey)) })

	originalLoadLatestChannelByID := loadLatestChannelByID
	loadLatestChannelByID = func(_ context.Context, id int) (*model.Channel, error) {
		if id != channelID {
			t.Fatalf("unexpected channel id lookup: got %d want %d", id, channelID)
		}
		return &model.Channel{Id: id, Type: config.ChannelTypeCodex, Key: latestKey}, nil
	}
	t.Cleanup(func() {
		loadLatestChannelByID = originalLoadLatestChannelByID
	})

	refreshCalls := 0
	originalRefreshCredentials := refreshOAuthCredentials
	refreshOAuthCredentials = func(creds *OAuth2Credentials, ctx context.Context, proxyURL string) error {
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
	if cachedCredentials := provider.getCachedCredentialSnapshot(context.Background(), 0); cachedCredentials.AccessToken != accessToken {
		t.Fatalf("expected unchanged access token to be recached after peer handling, got %q", cachedCredentials.AccessToken)
	}
}

func TestAcquireChannelRefreshLockCleansUpUnusedEntry(t *testing.T) {
	channelID := 424244

	channelRefreshLocks.mu.Lock()
	delete(channelRefreshLocks.locks, channelID)
	channelRefreshLocks.mu.Unlock()

	release, err := acquireChannelRefreshLock(context.Background(), channelID)
	if err != nil {
		t.Fatalf("expected channel refresh lock acquisition to succeed, got %v", err)
	}

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

func TestAcquireChannelRefreshLockHonorsCancellationWhileWaiting(t *testing.T) {
	channelID := 424246

	channelRefreshLocks.mu.Lock()
	delete(channelRefreshLocks.locks, channelID)
	channelRefreshLocks.mu.Unlock()

	release, err := acquireChannelRefreshLock(context.Background(), channelID)
	if err != nil {
		t.Fatalf("expected first lock acquisition to succeed, got %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if waitingRelease, waitErr := acquireChannelRefreshLock(ctx, channelID); !errors.Is(waitErr, context.DeadlineExceeded) {
		if waitingRelease != nil {
			waitingRelease()
		}
		release()
		t.Fatalf("expected waiting acquisition to honor its deadline, got %v", waitErr)
	}

	release()
	channelRefreshLocks.mu.Lock()
	_, ok := channelRefreshLocks.locks[channelID]
	channelRefreshLocks.mu.Unlock()
	if ok {
		t.Fatal("expected canceled waiter and released holder to clean up lock entry")
	}
}
