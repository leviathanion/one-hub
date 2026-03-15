package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"
	commonredis "one-api/common/redis"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"

	"github.com/google/uuid"
)

const (
	TokenCacheKey        = "api_token:codex"
	refreshLockKeyPrefix = "codex:refresh-lock"
)

var channelRefreshLocks = struct {
	mu    sync.Mutex
	locks map[int]*channelRefreshLock
}{
	locks: make(map[int]*channelRefreshLock),
}

var (
	refreshLockTTL                     = 3 * time.Minute
	refreshLockPollInterval            = 200 * time.Millisecond
	refreshLockReleaseTimeout          = 3 * time.Second
	refreshCredentialReloadInterval    = 2 * time.Second
	legacyCredentialExpiryFallback     = time.Hour
	loadLatestChannelByID              = model.GetChannelById
	updateChannelKey                   = model.UpdateChannelKey
	acquireDistributedRefreshLockSetNX = func(ctx context.Context, key string, value any, ttl time.Duration) (bool, error) {
		client := commonredis.GetRedisClient()
		if client == nil {
			return false, fmt.Errorf("redis client is not configured")
		}
		return client.SetNX(ctx, key, value, ttl).Result()
	}
)

var releaseRefreshLockScript = commonredis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

type CodexProviderFactory struct{}

// Create CodexProvider.
func (f CodexProviderFactory) Create(channel *model.Channel) base.ProviderInterface {
	runtimeChannel := prepareChannelForProvider(channel)

	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Config:          getConfig(),
				Channel:         runtimeChannel,
				Requester:       requester.NewHTTPRequester(channelProxyValue(runtimeChannel), RequestErrorHandle("")),
				SupportResponse: true,
			},
			SupportStreamOptions: true,
		},
	}

	// Parse config.
	parseCodexConfig(provider)

	// Update RequestErrorHandle with actual token.
	if provider.Credentials != nil {
		provider.rebuildRequester()
	}

	return provider
}

// parseCodexConfig parses Codex config.
// Supports:
// 1) JSON credentials (access_token, refresh_token, etc) with auto refresh.
// 2) Plain access_token (no auto refresh).
func parseCodexConfig(provider *CodexProvider) {
	if provider == nil || provider.Channel == nil {
		return
	}
	provider.Credentials = parseCredentialsFromKey(provider.Channel.Key)
}

type CodexProvider struct {
	openai.OpenAIProvider
	Credentials *OAuth2Credentials // OAuth2 credentials (with refresh_token).
}

func prepareChannelForProvider(channel *model.Channel) *model.Channel {
	if channel == nil {
		return nil
	}

	prepared := *channel
	proxyValue := ""
	if channel.Proxy != nil {
		proxyValue = *channel.Proxy
	}
	prepared.Proxy = &proxyValue
	prepared.SetProxy()

	return &prepared
}

func channelProxyValue(channel *model.Channel) string {
	if channel == nil || channel.Proxy == nil {
		return ""
	}
	return *channel.Proxy
}

func (p *CodexProvider) rebuildRequester() {
	if p == nil {
		return
	}

	accessToken := ""
	if p.Credentials != nil {
		accessToken = p.Credentials.AccessToken
	}
	p.Requester = requester.NewHTTPRequester(channelProxyValue(p.Channel), RequestErrorHandle(accessToken))
}

func (p *CodexProvider) syncRuntimeChannel(channel *model.Channel) {
	if p == nil {
		return
	}

	if preparedChannel := prepareChannelForProvider(channel); preparedChannel != nil {
		p.Channel = preparedChannel
	}
	p.rebuildRequester()
}

func (p *CodexProvider) syncRuntimeKey(key string) {
	if p == nil {
		return
	}

	if p.Channel != nil {
		p.Channel.Key = key
	}
	p.rebuildRequester()
}

func getConfig() base.ProviderConfig {
	return base.ProviderConfig{
		BaseURL:         "https://chatgpt.com",
		ChatCompletions: "/backend-api/codex/responses",
		Responses:       "/backend-api/codex/responses",
		ModelList:       "/backend-api/models",
	}
}

// RequestErrorHandle handles upstream errors.
func RequestErrorHandle(accessToken string) requester.HttpErrorHandler {
	return func(resp *http.Response) *types.OpenAIError {
		// Read response body.
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil
		}

		// Try Codex error payload (resets_in_seconds).
		var codexErrorResp CodexErrorResponse
		if err := json.Unmarshal(bodyBytes, &codexErrorResp); err == nil && codexErrorResp.Error.Message != "" {
			openAIError := &types.OpenAIError{
				Code:    codexErrorResp.Error.Code,
				Message: codexErrorResp.Error.Message,
				Type:    codexErrorResp.Error.Type,
			}

			// Scrub sensitive info.
			if accessToken != "" {
				openAIError.Message = strings.Replace(openAIError.Message, accessToken, "xxxxx", -1)
			}

			// Parse rate-limit reset time for 429.
			if resp.StatusCode == http.StatusTooManyRequests && codexErrorResp.Error.ResetsInSeconds > 0 {
				// Compute reset timestamp.
				resetTimestamp := time.Now().Unix() + int64(codexErrorResp.Error.ResetsInSeconds)
				logger.SysLog(fmt.Sprintf("[Codex] Rate limit detected, resets in %d seconds, reset at: %s",
					codexErrorResp.Error.ResetsInSeconds, time.Unix(resetTimestamp, 0).Format(time.RFC3339)))
			}

			return openAIError
		}

		// Fallback to standard OpenAI error payload.
		openAIError := &types.OpenAIError{}
		if err := json.Unmarshal(bodyBytes, openAIError); err != nil {
			return nil
		}

		if openAIError.Message == "" {
			return nil
		}

		// Scrub sensitive info.
		if accessToken != "" {
			openAIError.Message = strings.Replace(openAIError.Message, accessToken, "xxxxx", -1)
		}

		return openAIError
	}
}

// getRequestHeadersInternal builds request headers.
func (p *CodexProvider) getRequestHeadersInternal() (map[string]string, error) {
	headers := make(map[string]string)

	// Pass through selected client headers.
	if p.Context != nil {
		p.filterAndPassthroughClientHeaders(headers)
	}

	// Apply channel ModelHeaders overrides.
	p.CommonRequestHeaders(headers)

	// Fetch token.
	token, err := p.GetToken()
	if err != nil {
		if p.Context != nil {
			logger.LogError(p.Context.Request.Context(), "Failed to get Codex token: "+err.Error())
		} else {
			logger.SysError("Failed to get Codex token: " + err.Error())
		}
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	// Set required headers.
	headers["Authorization"] = "Bearer " + token
	headers["Content-Type"] = "application/json"

	// Set chatgpt-account-id when available.
	if p.Credentials != nil && p.Credentials.AccountID != "" {
		headers["chatgpt-account-id"] = p.Credentials.AccountID
	}

	return headers, nil
}

// filterAndPassthroughClientHeaders passes through allow-listed headers.
func (p *CodexProvider) filterAndPassthroughClientHeaders(headers map[string]string) {
	if p.Context == nil {
		return
	}

	allowedKeys := []string{
		"version",
		"openai-beta",
		"session_id",
		"x-session-id", // Support x-session-id.
	}

	// Pass through allow-listed headers.
	for _, key := range allowedKeys {
		value := p.Context.Request.Header.Get(key)
		if value != "" {
			headers[key] = value
		}
	}
}

// GetRequestHeaders exposes request headers.
func (p *CodexProvider) GetRequestHeaders() map[string]string {
	headers, _ := p.getRequestHeadersInternal()
	if headers == nil {
		headers = make(map[string]string)
		p.CommonRequestHeaders(headers)
	}
	return headers
}

func (p *CodexProvider) handleTokenError(err error) *types.OpenAIErrorWithStatusCode {
	errMsg := err.Error()

	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Message: errMsg,
			Type:    "codex_token_error",
			Code:    "codex_token_error",
		},
		StatusCode: http.StatusUnauthorized,
		LocalError: false,
	}
}

func (p *CodexProvider) GetToken() (string, error) {
	var ctx context.Context
	if p.Context != nil {
		ctx = p.Context.Request.Context()
	} else {
		ctx = context.Background()
	}

	if p.Credentials == nil {
		return "", fmt.Errorf("credentials not configured")
	}

	if p.Credentials.AccessToken == "" {
		return "", fmt.Errorf("access token is empty")
	}

	// If no refresh_token, return access_token.
	if p.Credentials.RefreshToken == "" {
		return p.Credentials.AccessToken, nil
	}

	fallbackTokenBeforeRefresh := p.Credentials.AccessToken
	fallbackAccountIDBeforeRefresh := p.Credentials.AccountID
	fallbackExpiresAtBeforeRefresh := p.Credentials.ExpiresAt
	fallbackChannelBeforeRefresh := prepareChannelForProvider(p.Channel)

	// Use cache while the token remains comfortably outside the refresh lead.
	cachedToken := p.getCachedToken(3 * time.Minute)
	if cachedToken != "" {
		p.Credentials.AccessToken = cachedToken
		return cachedToken, nil
	}

	if _, err := p.refreshTokenIfNeeded(ctx, 3*time.Minute); err != nil {
		if fallbackToken := p.getCurrentValidToken(); fallbackToken != "" {
			if fallbackToken == fallbackTokenBeforeRefresh && !expiresWithinLead(fallbackExpiresAtBeforeRefresh, 0) {
				if fallbackChannelBeforeRefresh != nil {
					p.Channel = fallbackChannelBeforeRefresh
				}
				p.rebuildRequester()
			}
			if p.Context != nil {
				logger.LogWarn(ctx, fmt.Sprintf("[Codex] Token refresh failed but current access token remains valid, using fallback: %s", err.Error()))
			} else {
				logger.SysLog(fmt.Sprintf("[Codex] Token refresh failed but current access token remains valid, using fallback: %s", err.Error()))
			}
			return fallbackToken, nil
		}
		if fallbackTokenBeforeRefresh != "" && !expiresWithinLead(fallbackExpiresAtBeforeRefresh, 0) {
			p.Credentials.AccessToken = fallbackTokenBeforeRefresh
			p.Credentials.AccountID = fallbackAccountIDBeforeRefresh
			if fallbackChannelBeforeRefresh != nil {
				p.Channel = fallbackChannelBeforeRefresh
			}
			p.rebuildRequester()
			if p.Context != nil {
				logger.LogWarn(ctx, fmt.Sprintf("[Codex] Token refresh failed after credential reload but the prior access token remains valid, using fallback: %s", err.Error()))
			} else {
				logger.SysLog(fmt.Sprintf("[Codex] Token refresh failed after credential reload but the prior access token remains valid, using fallback: %s", err.Error()))
			}
			return fallbackTokenBeforeRefresh, nil
		}

		logger.LogError(ctx, fmt.Sprintf("Failed to refresh codex token: %s", err.Error()))
		return "", fmt.Errorf("failed to refresh token: %w", err)
	}

	return p.Credentials.AccessToken, nil
}

func (p *CodexProvider) refreshTokenIfNeeded(ctx context.Context, lead time.Duration) (bool, error) {
	if p == nil || p.Credentials == nil || p.Channel == nil {
		return false, fmt.Errorf("credentials not configured")
	}
	if p.Credentials.RefreshToken == "" {
		return false, nil
	}

	releaseLocalLock := acquireChannelRefreshLock(p.channelID())
	defer releaseLocalLock()

	// Another goroutine may already have refreshed and cached the token.
	if cachedToken := p.getCachedToken(lead); cachedToken != "" {
		p.Credentials.AccessToken = cachedToken
		return false, nil
	}

	if err := p.loadLatestCredentialsFromDatabase(); err != nil {
		if ctx != nil {
			logger.LogWarn(ctx, fmt.Sprintf("[Codex] Failed to load latest credentials for channel %d: %s", p.channelID(), err.Error()))
		} else {
			logger.SysError(fmt.Sprintf("[Codex] Failed to load latest credentials for channel %d: %s", p.channelID(), err.Error()))
		}
	}

	if p.Credentials.RefreshToken == "" {
		return false, nil
	}
	if !p.Credentials.NeedsRefreshWithin(lead) {
		p.cacheCurrentToken()
		return false, nil
	}

	lock, handledByPeer, err := p.acquireDistributedRefreshLock(ctx, lead)
	if err != nil {
		return false, err
	}
	if handledByPeer {
		return false, nil
	}
	if lock != nil {
		defer lock.Release()
	}

	if p.refreshNoLongerNeeded(lead, true) {
		return false, nil
	}

	proxyURL := ""
	if p.Channel != nil && p.Channel.Proxy != nil && *p.Channel.Proxy != "" {
		proxyURL = *p.Channel.Proxy
	}

	if err := p.Credentials.Refresh(ctx, proxyURL, 3); err != nil {
		return false, err
	}

	if err := p.saveCredentialsToDatabase(ctx); err != nil {
		if ctx != nil {
			logger.LogError(ctx, fmt.Sprintf("Failed to save refreshed credentials to database: %s", err.Error()))
		} else {
			logger.SysError("Failed to save refreshed credentials to database: " + err.Error())
		}
	}

	p.cacheCurrentToken()
	return true, nil
}

func (p *CodexProvider) cacheCurrentToken() {
	if p == nil || p.Channel == nil || p.Credentials == nil || p.Credentials.AccessToken == "" {
		return
	}

	cacheDuration := 55 * time.Minute
	if !p.Credentials.ExpiresAt.IsZero() {
		timeUntilExpiry := time.Until(p.Credentials.ExpiresAt)
		if timeUntilExpiry > 0 && timeUntilExpiry < cacheDuration {
			cacheDuration = timeUntilExpiry
		}
	}
	if cacheDuration <= 0 {
		return
	}

	cache.SetCache(tokenCacheKey(p.Channel.Id), cachedAccessToken{
		AccessToken: p.Credentials.AccessToken,
		ExpiresAt:   p.Credentials.ExpiresAt,
	}, cacheDuration)
}

func (p *CodexProvider) saveCredentialsToDatabase(ctx context.Context) error {
	credentialsJSON, err := p.Credentials.ToJSON()
	if err != nil {
		return fmt.Errorf("failed to serialize credentials: %w", err)
	}

	// Keep the active provider self-consistent even before the chooser reloads.
	p.syncRuntimeKey(credentialsJSON)

	if err := updateChannelKey(p.Channel.Id, credentialsJSON); err != nil {
		return fmt.Errorf("failed to update channel key: %w", err)
	}
	if err := p.loadLatestCredentialsFromDatabase(); err != nil {
		if ctx != nil {
			logger.LogWarn(ctx, fmt.Sprintf("[Codex] Failed to reload runtime channel state for channel %d after saving credentials: %s", p.Channel.Id, err.Error()))
		} else {
			logger.SysLog(fmt.Sprintf("[Codex] Failed to reload runtime channel state for channel %d after saving credentials: %s", p.Channel.Id, err.Error()))
		}
	}

	logger.LogInfo(ctx, fmt.Sprintf("[Codex] Credentials saved to database for channel %d", p.Channel.Id))
	return nil
}

func parseCredentialsFromKey(rawKey string) *OAuth2Credentials {
	key := strings.TrimSpace(rawKey)
	if key == "" {
		return nil
	}

	creds, err := FromJSON(key)
	if err != nil {
		return &OAuth2Credentials{
			AccessToken: key,
			AccountID:   extractAccountIDFromJWT(key),
		}
	}

	normalizeCredentials(creds)
	return creds
}

func normalizeCredentials(creds *OAuth2Credentials) {
	if creds == nil {
		return
	}

	if creds.ClientID == "" {
		creds.ClientID = DefaultClientID
	}

	if creds.AccountID == "" && creds.AccessToken != "" {
		if accountID := extractAccountIDFromJWT(creds.AccessToken); accountID != "" {
			creds.AccountID = accountID
		}
	}

	if creds.RefreshToken != "" && creds.ExpiresAt.IsZero() {
		creds.ExpiresAt = time.Now().Add(legacyCredentialExpiryFallback)
	}
}

func tokenCacheKey(channelID int) string {
	return fmt.Sprintf("%s:%d", TokenCacheKey, channelID)
}

type channelRefreshLock struct {
	mu   sync.Mutex
	refs int
}

type cachedAccessToken struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
}

func acquireChannelRefreshLock(channelID int) func() {
	if channelID <= 0 {
		return func() {}
	}

	channelRefreshLocks.mu.Lock()
	lock := channelRefreshLocks.locks[channelID]
	if lock == nil {
		lock = &channelRefreshLock{}
		channelRefreshLocks.locks[channelID] = lock
	}
	lock.refs++
	channelRefreshLocks.mu.Unlock()

	lock.mu.Lock()

	return func() {
		lock.mu.Unlock()

		channelRefreshLocks.mu.Lock()
		defer channelRefreshLocks.mu.Unlock()

		lock.refs--
		if lock.refs == 0 && channelRefreshLocks.locks[channelID] == lock {
			delete(channelRefreshLocks.locks, channelID)
		}
	}
}

func (p *CodexProvider) channelID() int {
	if p == nil || p.Channel == nil {
		return 0
	}
	return p.Channel.Id
}

func (p *CodexProvider) getCachedToken(lead time.Duration) string {
	if p == nil || p.Channel == nil || p.Channel.Id <= 0 {
		return ""
	}

	cacheKey := tokenCacheKey(p.Channel.Id)

	cachedEntry, err := cache.GetCache[cachedAccessToken](cacheKey)
	if err == nil {
		if cachedEntry.AccessToken == "" {
			return ""
		}
		if !expiresWithinLead(cachedEntry.ExpiresAt, lead) {
			return cachedEntry.AccessToken
		}
		return ""
	}

	cachedToken, err := cache.GetCache[string](cacheKey)
	if err != nil || cachedToken == "" {
		return ""
	}
	if p.Credentials != nil && !p.Credentials.NeedsRefreshWithin(lead) {
		return cachedToken
	}
	return ""
}

func (p *CodexProvider) getCurrentValidToken() string {
	if p == nil || p.Credentials == nil {
		return ""
	}

	if cachedToken := p.getCachedToken(0); cachedToken != "" {
		p.Credentials.AccessToken = cachedToken
		if accountID := extractAccountIDFromJWT(cachedToken); accountID != "" {
			p.Credentials.AccountID = accountID
		}
		return cachedToken
	}

	if p.Credentials.AccessToken == "" || p.Credentials.NeedsRefreshWithin(0) {
		return ""
	}
	return p.Credentials.AccessToken
}

func (p *CodexProvider) loadLatestCredentialsFromDatabase() error {
	if p == nil || p.Channel == nil || p.Channel.Id <= 0 {
		return nil
	}

	channel, err := loadLatestChannelByID(p.Channel.Id)
	if err != nil {
		return err
	}

	latestCreds := parseCredentialsFromKey(channel.Key)
	if latestCreds == nil {
		return fmt.Errorf("channel key is empty")
	}

	p.Credentials = latestCreds
	p.syncRuntimeChannel(channel)
	return nil
}

type distributedRefreshLock struct {
	key   string
	value string
}

func (l *distributedRefreshLock) Release() {
	if l == nil || l.key == "" || l.value == "" || commonredis.GetRedisClient() == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), refreshLockReleaseTimeout)
	defer cancel()

	if _, err := commonredis.ScriptRunCtx(ctx, releaseRefreshLockScript, []string{l.key}, l.value); err != nil {
		logger.SysError("[Codex] failed to release distributed refresh lock: " + err.Error())
	}
}

func (p *CodexProvider) acquireDistributedRefreshLock(ctx context.Context, lead time.Duration) (*distributedRefreshLock, bool, error) {
	if !config.RedisEnabled || commonredis.GetRedisClient() == nil || p.channelID() <= 0 {
		return nil, false, nil
	}

	requestCtx, cancel := context.WithTimeout(ensureContext(ctx), refreshLockTTL)
	defer cancel()

	lock := &distributedRefreshLock{
		key:   refreshLockKey(p.channelID()),
		value: uuid.NewString(),
	}
	nextCredentialReloadAt := time.Time{}

	for {
		acquired, err := acquireDistributedRefreshLockSetNX(requestCtx, lock.key, lock.value, refreshLockTTL)
		if err != nil {
			lockErr := fmt.Errorf("failed to acquire distributed refresh lock for channel %d: %w", p.channelID(), err)
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				if ctx != nil {
					logger.LogInfo(ctx, "[Codex] "+lockErr.Error())
				} else {
					logger.SysLog("[Codex] " + lockErr.Error())
				}
			} else if ctx != nil {
				logger.LogWarn(ctx, "[Codex] "+lockErr.Error())
			} else {
				logger.SysError("[Codex] " + lockErr.Error())
			}
			return nil, false, lockErr
		}
		if acquired {
			return lock, false, nil
		}
		shouldReloadCredentials := nextCredentialReloadAt.IsZero() || !time.Now().Before(nextCredentialReloadAt)
		if p.refreshNoLongerNeeded(lead, shouldReloadCredentials) {
			return nil, true, nil
		}
		if shouldReloadCredentials {
			nextCredentialReloadAt = time.Now().Add(refreshCredentialReloadInterval)
		}
		if err := waitForRetry(requestCtx, refreshLockPollInterval); err != nil {
			return nil, false, fmt.Errorf("waiting for another instance to finish refresh: %w", err)
		}
	}
}

func (p *CodexProvider) refreshNoLongerNeeded(lead time.Duration, reloadFromDatabase bool) bool {
	if cachedToken := p.getCachedToken(lead); cachedToken != "" {
		p.Credentials.AccessToken = cachedToken
		return true
	}
	if !reloadFromDatabase {
		return false
	}
	if err := p.loadLatestCredentialsFromDatabase(); err != nil {
		return false
	}
	if p.Credentials == nil || p.Credentials.RefreshToken == "" {
		return true
	}
	if !p.Credentials.NeedsRefreshWithin(lead) {
		p.cacheCurrentToken()
		return true
	}
	return false
}

func refreshLockKey(channelID int) string {
	return fmt.Sprintf("%s:%d", refreshLockKeyPrefix, channelID)
}

func expiresWithinLead(expiresAt time.Time, lead time.Duration) bool {
	if expiresAt.IsZero() {
		return true
	}
	if lead < 0 {
		lead = 0
	}
	return time.Now().Add(lead).After(expiresAt)
}
