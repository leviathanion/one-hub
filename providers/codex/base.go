package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"one-api/providers/codex/wire"
	"one-api/providers/openai"
	"one-api/types"

	"github.com/google/uuid"
)

const (
	TokenCacheKey                     = "api_token:codex"
	refreshLockKeyPrefix              = "codex:refresh-lock"
	defaultUserAgent                  = "codex-tui/0.135.0 (Arch Linux Rolling Release; x86_64) foot (codex-tui; 0.135.0)"
	defaultOfficialCodexOriginator    = "codex-tui"
	defaultNonOfficialCodexOriginator = "pi"
	// Upstream may rotate the refresh token before the request context is canceled.
	// Give the mandatory DB commit a small independent budget: this may outlive the
	// caller briefly, but avoids losing the only valid rotated credential.
	refreshCredentialPersistTimeout = 5 * time.Second
)

var errCodexCredentialPersistence = errors.New("persist refreshed Codex credentials")

var (
	ErrCredentialRefreshInProgress       = errors.New("credential_refresh_in_progress")
	ErrCredentialRefreshUnresolved       = errors.New("credential_refresh_unresolved")
	ErrCredentialReauthorizationRequired = errors.New("credential_reauthorization_required")
	ErrCredentialRefreshSuperseded       = errors.New("credential_refresh_superseded")
)

const (
	codexPromptCacheStrategyAuto       = "auto"
	codexPromptCacheStrategyOff        = "off"
	codexPromptCacheStrategySessionID  = "session_id"
	codexPromptCacheStrategyTokenID    = "token_id"
	codexPromptCacheStrategyUserID     = "user_id"
	codexPromptCacheStrategyAuthHeader = "auth_header"

	codexWebsocketModeAuto  = "auto"
	codexWebsocketModeForce = "force"
	codexWebsocketModeOff   = "off"
)

type codexChannelOptions struct {
	PromptCacheKeyStrategy        string `json:"prompt_cache_key_strategy"`
	WebsocketMode                 string `json:"websocket_mode"`
	SelfHosted                    bool   `json:"self_hosted"`
	ResponsesWSSelfHosted         bool   `json:"responses_ws_self_hosted"`
	ExecutionSessionTTLSeconds    int    `json:"execution_session_ttl_seconds"`
	WebsocketRetryCooldownSeconds int    `json:"websocket_retry_cooldown_seconds"`
}

func DefaultUserAgent() string {
	return defaultUserAgent
}

func normalizeCodexModelName(modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if strings.HasPrefix(modelName, "gpt-5-") && modelName != "gpt-5-codex" {
		return "gpt-5"
	}
	return modelName
}

var channelRefreshLocks = struct {
	mu    sync.Mutex
	locks map[int]*channelRefreshLock
}{
	locks: make(map[int]*channelRefreshLock),
}

var (
	refreshLockTTL                  = 3 * time.Minute
	refreshLockPollInterval         = 200 * time.Millisecond
	refreshLockReleaseTimeout       = 3 * time.Second
	refreshCredentialReloadInterval = 2 * time.Second
	legacyCredentialExpiryFallback  = time.Hour
	loadLatestChannelByID           = model.GetChannelByIdWithContext
	compareAndSetChannelKey         = model.CompareAndSetChannelKeyWithContext
	refreshOAuthCredentials         = func(creds *OAuth2Credentials, ctx context.Context, proxyURL string) error {
		return creds.Refresh(ctx, proxyURL)
	}
	claimCredentialRotation            = model.ClaimCredentialRotation
	commitCredentialRotation           = model.CommitCredentialRotation
	cancelCredentialRotation           = model.CancelCredentialRotationBeforeDispatch
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

	runtimeMu               sync.RWMutex
	credentialPersistenceMu sync.Mutex
	credentialDirty         bool
	credentialExpectedKey   string
	credentialRotatedKey    string
	channelOptionsMu        sync.Mutex
	channelOptions          *codexChannelOptions
	channelOptionsLoaded    bool
	officialPolicyMu        sync.Mutex
	officialPolicyLoaded    bool
	officialPolicyKey       string
	officialPolicy          wire.ChannelPolicy
	officialPolicyErr       error
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
	p.runtimeMu.Lock()
	defer p.runtimeMu.Unlock()
	p.rebuildRequesterLocked()
}

func (p *CodexProvider) rebuildRequesterLocked() {
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
		p.runtimeMu.Lock()
		p.Channel = preparedChannel
		p.rebuildRequesterLocked()
		p.runtimeMu.Unlock()

		p.channelOptionsMu.Lock()
		p.channelOptions = nil
		p.channelOptionsLoaded = false
		p.channelOptionsMu.Unlock()
		p.officialPolicyMu.Lock()
		p.officialPolicyLoaded = false
		p.officialPolicyKey = ""
		p.officialPolicy = wire.ChannelPolicy{}
		p.officialPolicyErr = nil
		p.officialPolicyMu.Unlock()
		return
	}
	p.rebuildRequester()
}

func (p *CodexProvider) GetChannel() *model.Channel {
	return p.codexChannel()
}

func (p *CodexProvider) codexChannel() *model.Channel {
	if p == nil {
		return nil
	}
	p.runtimeMu.RLock()
	defer p.runtimeMu.RUnlock()
	return p.Channel
}

func (p *CodexProvider) codexRequester() *requester.HTTPRequester {
	if p == nil {
		return nil
	}
	p.runtimeMu.RLock()
	defer p.runtimeMu.RUnlock()
	return p.Requester
}

func (p *CodexProvider) codexPreCost() int {
	channel := p.codexChannel()
	if channel == nil {
		return 0
	}
	return channel.PreCost
}

func (p *CodexProvider) GetBaseURL() string {
	if channel := p.codexChannel(); channel != nil && channel.GetBaseURL() != "" {
		return channel.GetBaseURL()
	}
	if p == nil {
		return ""
	}
	return p.Config.BaseURL
}

func (p *CodexProvider) GetFullRequestURL(requestURL string, _ string) string {
	baseURL := strings.TrimSuffix(p.GetBaseURL(), "/")
	return fmt.Sprintf("%s%s", baseURL, requestURL)
}

func (p *CodexProvider) syncRuntimeKey(key string) {
	if p == nil {
		return
	}

	p.runtimeMu.Lock()
	if p.Channel != nil {
		p.Channel.Key = key
	}
	p.rebuildRequesterLocked()
	p.runtimeMu.Unlock()
}

func (p *CodexProvider) getChannelOptions() *codexChannelOptions {
	if p == nil {
		return nil
	}
	channel := p.codexChannel()
	if channel == nil || strings.TrimSpace(channel.Other) == "" {
		return nil
	}

	p.channelOptionsMu.Lock()
	defer p.channelOptionsMu.Unlock()

	if p.channelOptionsLoaded {
		return p.channelOptions
	}

	p.channelOptionsLoaded = true

	rawOptions, err := channel.GetOtherMap()
	if err != nil {
		logger.LogError(p.channelLogContext(), fmt.Sprintf("failed to parse Codex channel Other JSON for channel #%d(%s): %v", channel.Id, channel.Name, err))
		return nil
	}
	if len(rawOptions) == 0 {
		return nil
	}

	payload, err := json.Marshal(rawOptions)
	if err != nil {
		logger.LogError(p.channelLogContext(), fmt.Sprintf("failed to normalize Codex channel Other JSON for channel #%d(%s): %v", channel.Id, channel.Name, err))
		return nil
	}

	var options codexChannelOptions
	if err := json.Unmarshal(payload, &options); err != nil {
		logger.LogError(p.channelLogContext(), fmt.Sprintf("failed to decode Codex channel options for channel #%d(%s): %v", channel.Id, channel.Name, err))
		return nil
	}

	p.channelOptions = &options
	return p.channelOptions
}

func (p *CodexProvider) channelLogContext() context.Context {
	if p != nil && p.Context != nil && p.Context.Request != nil {
		return p.Context.Request.Context()
	}
	return context.Background()
}

func getConfig() base.ProviderConfig {
	return base.ProviderConfig{
		BaseURL:         "https://chatgpt.com",
		ChatCompletions: "/backend-api/codex/responses",
		ChatRealtime:    "/backend-api/codex/responses",
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

		// Try Codex error payload (resets_at/resets_in_seconds).
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

			now := time.Now()
			if retryAfter := codexUsageLimitRetryAfter(resp.StatusCode, codexErrorResp.Error, now); retryAfter != nil {
				resetAt := now.Add(*retryAfter)
				logger.SysLog(fmt.Sprintf("[Codex] Usage limit detected, resets in %d seconds, reset at: %s",
					int(retryAfter.Seconds()), resetAt.Format(time.RFC3339)))
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

func codexUsageLimitRetryAfter(statusCode int, detail CodexErrorDetail, now time.Time) *time.Duration {
	if statusCode < http.StatusBadRequest || !strings.EqualFold(strings.TrimSpace(detail.Type), "usage_limit_reached") {
		return nil
	}
	if detail.ResetsAt > 0 {
		resetAt := time.Unix(detail.ResetsAt, 0)
		if resetAt.After(now) {
			retryAfter := resetAt.Sub(now)
			return &retryAfter
		}
	}
	if detail.ResetsInSeconds > 0 {
		retryAfter := time.Duration(detail.ResetsInSeconds) * time.Second
		return &retryAfter
	}
	if detail.ResetsIn > 0 {
		retryAfter := time.Duration(detail.ResetsIn) * time.Second
		return &retryAfter
	}
	return nil
}

func (p *CodexProvider) applyCommonRequestHeaders(headers *codexHeaderBag) {
	if headers == nil {
		return
	}

	if p.Context != nil {
		headers.Set("Content-Type", p.Context.Request.Header.Get("Content-Type"))
		headers.Set("Accept", p.Context.Request.Header.Get("Accept"))
	}

	if channel := p.codexChannel(); channel != nil {
		customHeaders, err := channel.GetModelHeadersMap()
		if err == nil {
			for key, value := range customHeaders {
				// 与 base.CommonRequestHeaders 一致：model_headers 不允许覆盖凭证/
				// 路由头、hop-by-hop 头和 WebSocket handshake 协议头。
				if _, blocked := base.ProtectedModelHeaderReason(key); blocked {
					continue
				}
				headers.Set(key, value)
			}
		}
	}

	headers.SetIfAbsent("Content-Type", "application/json")
}

func (p *CodexProvider) requestContext() context.Context {
	if p != nil && p.Context != nil && p.Context.Request != nil {
		return p.Context.Request.Context()
	}
	return context.Background()
}

func (p *CodexProvider) getRequestHeaderBag() (*codexHeaderBag, error) {
	return p.getRequestHeaderBagWithContext(p.requestContext())
}

func (p *CodexProvider) getRequestHeaderBagWithContext(ctx context.Context) (*codexHeaderBag, error) {
	headers := newCodexHeaderBag()

	// Pass through selected client headers.
	if p.Context != nil {
		p.filterAndPassthroughClientHeaders(headers)
	}

	// Apply channel ModelHeaders overrides.
	p.applyCommonRequestHeaders(headers)

	// Fetch token using the operation context. Background refreshes do not carry a
	// Gin context, so falling back to p.GetToken() here would make their timeout
	// ineffective while waiting for an OAuth refresh.
	token, err := p.getToken(ctx)
	if err != nil {
		if p.Context != nil {
			logger.LogError(ensureContext(ctx), "Failed to get Codex token: "+err.Error())
		} else {
			logger.SysError("Failed to get Codex token: " + err.Error())
		}
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	// Set required headers.
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Content-Type", "application/json")

	// Set chatgpt-account-id when available.
	if p.Credentials != nil && p.Credentials.AccountID != "" {
		headers.Set("chatgpt-account-id", p.Credentials.AccountID)
	}

	return headers, nil
}

// getRequestHeadersInternal builds request headers.
func (p *CodexProvider) getRequestHeadersInternal() (map[string]string, error) {
	headers, err := p.getRequestHeaderBag()
	if err != nil {
		return nil, err
	}
	return headers.Map(), nil
}

// filterAndPassthroughClientHeaders passes through allow-listed headers.
func (p *CodexProvider) filterAndPassthroughClientHeaders(headers *codexHeaderBag) {
	if p.Context == nil || headers == nil {
		return
	}

	allowedKeys := []string{
		"version",
		"openai-beta",
		"session_id",
		"x-session-id", // Support x-session-id.
		"x-codex-turn-metadata",
		"x-client-request-id",
		"x-codex-turn-state",
		"x-responsesapi-include-timing-metrics",
		"x-codex-beta-features",
		"originator",
		"user-agent",
	}

	// Pass through allow-listed headers.
	for _, key := range allowedKeys {
		value := p.Context.Request.Header.Get(key)
		if value != "" {
			headers.Set(key, value)
		}
	}
}

// GetRequestHeaders exposes request headers.
func (p *CodexProvider) GetRequestHeaders() map[string]string {
	headers, err := p.getRequestHeaderBag()
	if err == nil && headers != nil {
		return headers.Map()
	}

	fallback := newCodexHeaderBag()
	p.applyCommonRequestHeaders(fallback)
	return fallback.Map()
}

func (p *CodexProvider) handleTokenError(_ error) *types.OpenAIErrorWithStatusCode {
	// Keep the client-facing token error static. Refresh failures may contain
	// provider response bodies or credential fragments; detailed diagnostics stay
	// on the server-side logging path.
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Message: "Codex token refresh failed; please check channel OAuth credentials",
			Type:    "codex_token_error",
			Code:    "codex_token_error",
		},
		StatusCode: http.StatusUnauthorized,
		LocalError: false,
	}
}

func (p *CodexProvider) GetToken() (string, error) {
	return p.getToken(p.requestContext())
}

func (p *CodexProvider) getToken(ctx context.Context) (string, error) {
	ctx = ensureContext(ctx)

	if err := p.commitPendingCredentials(ctx); err != nil {
		return "", fmt.Errorf("failed to commit pending credentials: %w", err)
	}
	if p.Credentials == nil {
		return "", fmt.Errorf("credentials not configured")
	}
	// A prior OAuth rotation may have succeeded while its DB commit failed. Retry
	// that commit before inspecting, caching, or returning any token state.
	if p.hasDirtyCredentials() {
		if err := p.persistRefreshedCredentials(ctx); err != nil {
			return "", fmt.Errorf("failed to refresh token: %w", err)
		}
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
	fallbackChannelBeforeRefresh := prepareChannelForProvider(p.codexChannel())

	// Use cache while the token remains comfortably outside the refresh lead.
	cachedCredentials := p.getCachedCredentialSnapshot(ctx, 3*time.Minute)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if cachedCredentials.AccessToken != "" {
		p.adoptCachedCredentials(cachedCredentials)
		return cachedCredentials.AccessToken, nil
	}

	if _, err := p.refreshTokenIfNeeded(ctx, 3*time.Minute); err != nil {
		if errors.Is(err, errCodexCredentialPersistence) {
			return "", fmt.Errorf("failed to refresh token: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if fallbackToken := p.getCurrentValidToken(ctx); fallbackToken != "" {
			if fallbackToken == fallbackTokenBeforeRefresh && !expiresWithinLead(fallbackExpiresAtBeforeRefresh, 0) {
				if fallbackChannelBeforeRefresh != nil {
					p.syncRuntimeChannel(fallbackChannelBeforeRefresh)
				} else {
					p.rebuildRequester()
				}
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
				p.syncRuntimeChannel(fallbackChannelBeforeRefresh)
			} else {
				p.rebuildRequester()
			}
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

func (p *CodexProvider) refreshTokenIfNeeded(ctx context.Context, lead time.Duration) (refreshed bool, returnErr error) {
	defer func() {
		returnErr = p.sanitizeRefreshError(returnErr)
	}()
	return p.rotateOnce(ctx, lead, false, credentialVersionSnapshot{})
}

func (p *CodexProvider) forceRefreshToken(ctx context.Context) (refreshed bool, returnErr error) {
	defer func() {
		returnErr = p.sanitizeRefreshError(returnErr)
	}()
	if p == nil || p.Credentials == nil {
		return false, fmt.Errorf("credentials not configured")
	}
	previousCredentialsVersion := credentialsVersion(p.Credentials)
	// Trade-off: once upstream has replied 401/403 for this token, we prefer to stop
	// serving the cached token immediately even if that briefly hurts cache hit rate.
	// Safety wins here, but every handled-by-peer path below must recache the latest
	// token so this deliberate invalidation does not leave the channel cold.
	if err := cache.DeleteCacheManyContext(ctx, []string{
		tokenCacheKey(p.channelID()),
		tokenCacheKeyV2(p.channelID(), p.codexChannel().Key),
	}); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, err
		}
		if ctx != nil {
			logger.LogWarn(ctx, fmt.Sprintf("[Codex] failed to clear token cache for forced refresh on channel %d: %s", p.channelID(), err.Error()))
		} else {
			logger.SysError(fmt.Sprintf("[Codex] failed to clear token cache for forced refresh on channel %d: %s", p.channelID(), err.Error()))
		}
	}

	return p.rotateOnce(ctx, 0, true, previousCredentialsVersion)
}

// rotateOnce is the sole persisted-channel OAuth rotation protocol.  Redis and
// process-local journals are intentionally absent: only the durable row fence
// decides whether the old refresh token may be dispatched.
func (p *CodexProvider) rotateOnce(ctx context.Context, lead time.Duration, forced bool, previous credentialVersionSnapshot) (bool, error) {
	if p == nil || p.codexChannel() == nil {
		return false, fmt.Errorf("credentials not configured")
	}
	ctx = ensureContext(ctx)
	reason := "normal"
	if forced {
		reason = "forced"
	}
	release, err := acquireChannelRefreshLock(ctx, p.channelID())
	if err != nil {
		return false, fmt.Errorf("waiting for channel refresh lock: %w", err)
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Persisted channels always run the fence protocol. A DB-less provider exists
	// only in isolated embedders/tests and has no shared authority to fence; retain
	// the historical in-process path for that non-production shape.
	if model.DB == nil {
		return p.rotateWithoutDurableAuthority(ctx, lead, forced, previous)
	}

	// Claim must be based on the authoritative credential and revision.  A load
	// failure is fail-closed and therefore guarantees zero OAuth calls.
	if p.channelID() > 0 {
		if err := p.loadLatestCredentialsFromDatabase(ctx); err != nil {
			return false, fmt.Errorf("load authoritative credential before refresh: %w", err)
		}
	}
	channel := p.codexChannel()
	if p.Credentials == nil || strings.TrimSpace(p.Credentials.RefreshToken) == "" {
		return false, nil
	}
	if forced && credentialsVersion(p.Credentials) != previous {
		p.cacheCurrentToken(ctx)
		return true, nil
	}
	if forced && p.credentialsChangedSince(ctx, previous, true) {
		p.cacheCurrentToken(ctx)
		return true, nil
	}
	if !forced {
		if cached := p.getCachedCredentialSnapshot(ctx, lead); cached.AccessToken != "" {
			p.adoptCachedCredentials(cached)
			return false, nil
		}
		if !p.Credentials.NeedsRefreshWithin(lead) {
			p.cacheCurrentToken(ctx)
			return false, nil
		}
	}
	if channel.Id <= 0 {
		rotated := cloneOAuth2Credentials(p.Credentials)
		if err := refreshOAuthCredentials(rotated, ctx, channelProxyValue(channel)); err != nil {
			return false, err
		}
		p.Credentials = rotated
		return true, nil
	}
	if channel.CredentialRefreshFence != nil {
		credentialRotationClaims.WithLabelValues("busy").Inc()
		return false, credentialFenceError(channel)
	}

	ticket := model.CredentialRotationTicket{ChannelID: channel.Id, AttemptID: uuid.NewString(), ExpectedRevision: channel.CredentialRevision}
	claim, err := claimCredentialRotation(ctx, ticket, time.Now())
	if err != nil {
		credentialRotationClaims.WithLabelValues("error").Inc()
		return false, fmt.Errorf("claim credential refresh fence: %w", err)
	}
	switch claim {
	case model.CredentialRotationClaimBusy:
		credentialRotationClaims.WithLabelValues("busy").Inc()
		return false, ErrCredentialRefreshInProgress
	case model.CredentialRotationClaimSuperseded:
		credentialRotationClaims.WithLabelValues("superseded").Inc()
		return false, ErrCredentialRefreshSuperseded
	case model.CredentialRotationClaimAcquired:
		credentialRotationClaims.WithLabelValues("acquired").Inc()
	default:
		return false, fmt.Errorf("unknown credential refresh claim outcome %d", claim)
	}

	rotated := cloneOAuth2Credentials(p.Credentials)
	exchangeErr := refreshOAuthCredentials(rotated, ctx, channelProxyValue(channel))
	if exchangeErr != nil {
		if !errors.Is(exchangeErr, ErrOAuthRefreshNotDispatched) {
			credentialRotations.WithLabelValues("ambiguous", reason).Inc()
			credentialRotationUnresolved.WithLabelValues("oauth_ambiguous").Inc()
			return false, errors.Join(ErrCredentialReauthorizationRequired, exchangeErr)
		}
		// Only an explicit pre-dispatch classification may clear the fence.
		cancelCtx, cancelFence := context.WithTimeout(context.WithoutCancel(ctx), refreshCredentialPersistTimeout)
		_, cancelErr := cancelCredentialRotation(cancelCtx, ticket)
		cancelFence()
		if cancelErr != nil {
			credentialRotations.WithLabelValues("cancel_error", reason).Inc()
			return false, errors.Join(exchangeErr, fmt.Errorf("cancel pre-dispatch refresh fence: %w", cancelErr))
		}
		credentialRotations.WithLabelValues("not_dispatched", reason).Inc()
		return false, exchangeErr
	}
	rotatedKey, err := rotated.ToJSON()
	if err != nil {
		// A successful exchange followed by an unusable local representation is
		// ambiguous.  The fence must remain durable.
		credentialRotations.WithLabelValues("ambiguous", reason).Inc()
		credentialRotationUnresolved.WithLabelValues("serialization").Inc()
		return false, errors.Join(ErrCredentialReauthorizationRequired, fmt.Errorf("serialize rotated credential: %w", err))
	}

	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshCredentialPersistTimeout)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		outcome, commitErr := commitCredentialRotation(commitCtx, ticket, rotatedKey)
		switch outcome {
		case model.CredentialRotationCommitApplied, model.CredentialRotationCommitAlreadyApplied:
			credentialRotations.WithLabelValues("committed", reason).Inc()
			p.Credentials = rotated
			p.syncRuntimeKey(rotatedKey)
			p.cacheCurrentToken(ctx)
			return true, nil
		case model.CredentialRotationCommitSuperseded:
			credentialRotations.WithLabelValues("superseded", reason).Inc()
			_ = p.loadLatestCredentialsFromDatabase(commitCtx)
			return false, errors.Join(ErrCredentialRefreshSuperseded, commitErr)
		case model.CredentialRotationCommitStillFenced:
			if attempt == 2 || commitCtx.Err() != nil {
				credentialRotationCommitRetries.WithLabelValues("exhausted").Inc()
				credentialRotationUnresolved.WithLabelValues("commit_exhausted").Inc()
				return false, errors.Join(errCodexCredentialPersistence, ErrCredentialReauthorizationRequired, commitErr)
			}
			credentialRotationCommitRetries.WithLabelValues("retry").Inc()
			if err := waitForRetry(commitCtx, time.Duration(attempt+1)*50*time.Millisecond); err != nil {
				return false, errors.Join(errCodexCredentialPersistence, ErrCredentialReauthorizationRequired, err)
			}
		}
	}
	return false, errors.Join(errCodexCredentialPersistence, ErrCredentialReauthorizationRequired)
}

func (p *CodexProvider) rotateWithoutDurableAuthority(ctx context.Context, lead time.Duration, forced bool, previous credentialVersionSnapshot) (bool, error) {
	if err := p.commitPendingCredentialsLocked(ctx); err != nil {
		return false, err
	}
	if p.Credentials == nil || p.Credentials.RefreshToken == "" {
		return false, nil
	}
	if p.hasDirtyCredentials() {
		if err := p.persistRefreshedCredentials(ctx); err != nil {
			return false, err
		}
	}
	if !forced {
		if cached := p.getCachedCredentialSnapshot(ctx, lead); cached.AccessToken != "" {
			p.adoptCachedCredentials(cached)
			return false, nil
		}
	}
	if err := p.loadLatestCredentialsFromDatabase(ctx); err != nil {
		return false, err
	}
	if forced && credentialsVersion(p.Credentials) != previous {
		p.cacheCurrentToken(ctx)
		return true, nil
	}
	if forced && p.credentialsChangedSince(ctx, previous, true) {
		p.cacheCurrentToken(ctx)
		return true, nil
	}
	if !forced && !p.Credentials.NeedsRefreshWithin(lead) {
		p.cacheCurrentToken(ctx)
		return false, nil
	}
	if !forced && p.refreshNoLongerNeeded(ctx, lead, true) {
		return false, nil
	}
	if err := p.refreshCredentials(ctx); err != nil {
		return false, err
	}
	if err := p.persistRefreshedCredentials(ctx); err != nil {
		return false, err
	}
	p.cacheCurrentToken(ctx)
	return true, nil
}

func credentialFenceError(channel *model.Channel) error {
	if channel != nil && channel.CredentialRefreshStartedAt != nil && time.Since(time.Unix(*channel.CredentialRefreshStartedAt, 0)) <= refreshCredentialPersistTimeout {
		return ErrCredentialRefreshInProgress
	}
	return errors.Join(ErrCredentialRefreshUnresolved, ErrCredentialReauthorizationRequired)
}

func (p *CodexProvider) sanitizeRefreshError(err error) error {
	if err == nil || p == nil || p.Credentials == nil {
		return err
	}
	clientID := p.Credentials.ClientID
	if clientID == "" {
		clientID = DefaultClientID
	}
	return sanitizeTokenRefreshError(err, p.Credentials, clientID)
}

func (p *CodexProvider) refreshCredentials(ctx context.Context) error {
	proxyURL := ""
	channel := p.codexChannel()
	if channel != nil && channel.Proxy != nil && *channel.Proxy != "" {
		proxyURL = *channel.Proxy
	}
	p.credentialPersistenceMu.Lock()
	defer p.credentialPersistenceMu.Unlock()
	if p.Credentials == nil {
		return fmt.Errorf("credentials not configured")
	}
	expectedKey := ""
	if channel != nil {
		expectedKey = channel.Key
	}
	channelID := p.channelID()
	if channelID > 0 && expectedKey == "" {
		return fmt.Errorf("durable channel key is empty")
	}
	if err := requireUnambiguousCredentialRefresh(channelID, expectedKey); err != nil {
		return err
	}

	// Never mutate the active credential object before the rotated value is
	// durable. Error paths therefore keep serving neither a hidden new token nor a
	// cache entry peers cannot reproduce.
	rotatedCredentials := cloneOAuth2Credentials(p.Credentials)
	if err := refreshOAuthCredentials(rotatedCredentials, ctx, proxyURL); err != nil {
		if errors.Is(err, ErrOAuthRefreshOutcomeAmbiguous) {
			rememberAmbiguousCredentialRefresh(channelID, expectedKey)
		}
		return err
	}
	rotatedKey, err := rotatedCredentials.ToJSON()
	if err != nil {
		return fmt.Errorf("serialize rotated credentials: %w", err)
	}

	// Id-less providers have no durable authority and are used only by isolated
	// callers. Persisted channels always journal before returning from this method.
	if channelID <= 0 {
		p.Credentials = rotatedCredentials
		return nil
	}

	p.credentialExpectedKey = expectedKey
	p.credentialRotatedKey = rotatedKey
	p.credentialDirty = true
	if err := rememberPendingCredentialCommit(channelID, expectedKey, rotatedKey); err != nil {
		return fmt.Errorf("record rotated credentials for recovery: %w", err)
	}
	return nil
}

func (p *CodexProvider) cacheCurrentToken(ctx context.Context) {
	if p.hasDirtyCredentials() {
		return
	}
	channel := p.codexChannel()
	if channel == nil || p.Credentials == nil || p.Credentials.AccessToken == "" {
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

	_ = cache.SetCacheContext(ctx, tokenCacheKeyV2(channel.Id, channel.Key), cachedAccessToken{
		AccessToken: p.Credentials.AccessToken,
		AccountID:   cachedAccountID(p.Credentials),
		ExpiresAt:   p.Credentials.ExpiresAt,
	}, cacheDuration)
}

func (p *CodexProvider) hasDirtyCredentials() bool {
	if p == nil {
		return false
	}
	p.credentialPersistenceMu.Lock()
	defer p.credentialPersistenceMu.Unlock()
	return p.credentialDirty
}

func (p *CodexProvider) persistRefreshedCredentials(operationCtx context.Context) error {
	p.credentialPersistenceMu.Lock()
	defer p.credentialPersistenceMu.Unlock()
	if !p.credentialDirty {
		return nil
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ensureContext(operationCtx)), refreshCredentialPersistTimeout)
	defer cancel()
	if err := p.saveCredentialsToDatabase(persistCtx); err != nil {
		return fmt.Errorf("%w: %w", errCodexCredentialPersistence, err)
	}
	p.credentialDirty = false
	p.credentialExpectedKey = ""
	p.credentialRotatedKey = ""
	return nil
}

func (p *CodexProvider) saveCredentialsToDatabase(ctx context.Context) error {
	channel := p.codexChannel()
	if channel == nil || channel.Id <= 0 {
		return fmt.Errorf("channel not configured")
	}
	channelID := channel.Id
	expectedKey := p.credentialExpectedKey
	rotatedKey := p.credentialRotatedKey
	if expectedKey == "" || rotatedKey == "" {
		return fmt.Errorf("pending credential commit is not configured")
	}
	rotatedCredentials, err := parseRotatedCredentials(rotatedKey)
	if err != nil {
		return err
	}

	updated, err := compareAndSetChannelKey(ensureContext(ctx), channelID, expectedKey, rotatedKey)
	if err != nil {
		return fmt.Errorf("failed to compare-and-set channel key: %w", err)
	}
	if !updated {
		// A CAS miss is ambiguous until durable state is reloaded. In particular, a
		// peer may have committed this exact rotation. Never clear dirty/pending or
		// expose the rotated token while that reload is unavailable.
		if reloadErr := p.loadLatestCredentialsFromDatabase(ctx); reloadErr != nil {
			return fmt.Errorf("%w; reload latest credentials: %v", errCodexCredentialCASConflict, reloadErr)
		}
		latest := p.codexChannel()
		if latest != nil && latest.Key == rotatedKey {
			clearPendingCredentialCommit(channelID, rotatedKey)
			p.credentialDirty = false
			p.credentialExpectedKey = ""
			p.credentialRotatedKey = ""
			return nil
		}
		if latest != nil && latest.Key != expectedKey {
			// A different durable value is an intentional/manual winner. The reload
			// already adopted it, so the stale pending rotation can be discarded.
			clearPendingCredentialCommit(channelID, rotatedKey)
			p.credentialDirty = false
			p.credentialExpectedKey = ""
			p.credentialRotatedKey = ""
			return errCodexCredentialCASConflict
		}
		// A spurious miss with the expected value still in the DB is unresolved.
		// The active provider continues to hold the durable old credential; the
		// rotated value remains only in the recovery journal.
		return errCodexCredentialCASConflict
	}
	clearPendingCredentialCommit(channelID, rotatedKey)
	// Publish runtime credentials only after durable storage accepts them.
	p.Credentials = rotatedCredentials
	p.syncRuntimeKey(rotatedKey)

	logger.LogInfo(ctx, fmt.Sprintf("[Codex] Credentials saved to database for channel %d", channelID))
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

func parseRotatedCredentials(rawKey string) (*OAuth2Credentials, error) {
	credentials, err := FromJSON(strings.TrimSpace(rawKey))
	if err != nil {
		return nil, fmt.Errorf("pending rotated credentials are invalid: %w", err)
	}
	normalizeCredentials(credentials)
	if strings.TrimSpace(credentials.AccessToken) == "" || strings.TrimSpace(credentials.RefreshToken) == "" {
		return nil, fmt.Errorf("pending rotated credentials are incomplete")
	}
	return credentials, nil
}

func cloneOAuth2Credentials(credentials *OAuth2Credentials) *OAuth2Credentials {
	if credentials == nil {
		return nil
	}
	cloned := *credentials
	cloned.Scopes = append([]string(nil), credentials.Scopes...)
	return &cloned
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

// tokenCacheKey is the legacy v1 key retained only for rolling-upgrade deletion.
func tokenCacheKey(channelID int) string {
	return fmt.Sprintf("%s:%d", TokenCacheKey, channelID)
}

func tokenCacheKeyV2(channelID int, durableRuntimeKey string) string {
	digest := sha256.Sum256([]byte(durableRuntimeKey))
	return fmt.Sprintf("%s:v2:%d:%s", TokenCacheKey, channelID, hex.EncodeToString(digest[:]))
}

type channelRefreshLock struct {
	gate chan struct{}
	refs int
}

type cachedAccessToken struct {
	AccessToken string    `json:"access_token"`
	AccountID   string    `json:"account_id,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
}

func (c cachedAccessToken) accountID() string {
	if accountID := strings.TrimSpace(c.AccountID); accountID != "" {
		return accountID
	}
	return extractAccountIDFromJWT(c.AccessToken)
}

type cachedCredentialSnapshot struct {
	AccessToken string
	AccountID   string
	ExpiresAt   time.Time
}

func cachedAccountID(creds *OAuth2Credentials) string {
	if creds == nil {
		return ""
	}
	if accountID := strings.TrimSpace(creds.AccountID); accountID != "" {
		return accountID
	}
	return extractAccountIDFromJWT(creds.AccessToken)
}

type credentialVersionSnapshot struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
}

func acquireChannelRefreshLock(ctx context.Context, channelID int) (func(), error) {
	if channelID <= 0 {
		return func() {}, nil
	}
	ctx = ensureContext(ctx)

	channelRefreshLocks.mu.Lock()
	lock := channelRefreshLocks.locks[channelID]
	if lock == nil {
		lock = &channelRefreshLock{gate: make(chan struct{}, 1)}
		lock.gate <- struct{}{}
		channelRefreshLocks.locks[channelID] = lock
	}
	lock.refs++
	channelRefreshLocks.mu.Unlock()

	cleanupReference := func() {
		channelRefreshLocks.mu.Lock()
		defer channelRefreshLocks.mu.Unlock()
		lock.refs--
		if lock.refs == 0 && channelRefreshLocks.locks[channelID] == lock {
			delete(channelRefreshLocks.locks, channelID)
		}
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			lock.gate <- struct{}{}
			cleanupReference()
		})
	}

	// Preserve the fast path even when the operation context has just been
	// canceled: callers may still discover that a peer already refreshed and use
	// the cached token without doing context-bound work. Only waiting for a held
	// lock needs to be cancelable.
	select {
	case <-lock.gate:
		return release, nil
	default:
	}

	if err := ctx.Err(); err != nil {
		cleanupReference()
		return nil, err
	}

	select {
	case <-lock.gate:
		if err := ctx.Err(); err != nil {
			lock.gate <- struct{}{}
			cleanupReference()
			return nil, err
		}
		return release, nil
	case <-ctx.Done():
		cleanupReference()
		return nil, ctx.Err()
	}
}

func (p *CodexProvider) channelID() int {
	channel := p.codexChannel()
	if channel == nil {
		return 0
	}
	return channel.Id
}

func credentialsVersion(creds *OAuth2Credentials) credentialVersionSnapshot {
	if creds == nil {
		return credentialVersionSnapshot{}
	}

	expiresAt := int64(0)
	if !creds.ExpiresAt.IsZero() {
		expiresAt = creds.ExpiresAt.UTC().Unix()
	}

	return credentialVersionSnapshot{
		AccessToken:  strings.TrimSpace(creds.AccessToken),
		RefreshToken: strings.TrimSpace(creds.RefreshToken),
		ExpiresAt:    expiresAt,
	}
}

func (p *CodexProvider) credentialsChangedSince(ctx context.Context, previous credentialVersionSnapshot, reloadFromDatabase bool) bool {
	if reloadFromDatabase {
		if err := p.loadLatestCredentialsFromDatabase(ctx); err != nil {
			return false
		}
	}

	current := credentialsVersion(p.Credentials)
	if current == (credentialVersionSnapshot{}) {
		return false
	}

	return current != previous
}

func (p *CodexProvider) getCachedCredentialSnapshot(ctx context.Context, lead time.Duration) cachedCredentialSnapshot {
	channel := p.codexChannel()
	if channel == nil || channel.Id <= 0 {
		return cachedCredentialSnapshot{}
	}

	// The credential digest is part of the physical key. An old in-flight
	// provider may still write after a manual update/delete, but a provider built
	// from the new durable/runtime key can never observe that write.
	cacheKey := tokenCacheKeyV2(channel.Id, channel.Key)

	cachedEntry, err := cache.GetCacheContext[cachedAccessToken](ctx, cacheKey)
	if err == nil {
		if cachedEntry.AccessToken == "" {
			return cachedCredentialSnapshot{}
		}
		if !expiresWithinLead(cachedEntry.ExpiresAt, lead) {
			return cachedCredentialSnapshot{
				AccessToken: cachedEntry.AccessToken,
				AccountID:   cachedEntry.accountID(),
				ExpiresAt:   cachedEntry.ExpiresAt,
			}
		}
		return cachedCredentialSnapshot{}
	}

	cachedToken, err := cache.GetCacheContext[string](ctx, cacheKey)
	if err != nil || cachedToken == "" {
		return cachedCredentialSnapshot{}
	}
	if p.Credentials != nil && !p.Credentials.NeedsRefreshWithin(lead) {
		return cachedCredentialSnapshot{
			AccessToken: cachedToken,
			AccountID:   extractAccountIDFromJWT(cachedToken),
		}
	}
	return cachedCredentialSnapshot{}
}

func (p *CodexProvider) adoptCachedCredentials(snapshot cachedCredentialSnapshot) {
	if p == nil || p.Credentials == nil || strings.TrimSpace(snapshot.AccessToken) == "" {
		return
	}
	p.Credentials.AccessToken = strings.TrimSpace(snapshot.AccessToken)
	p.Credentials.AccountID = strings.TrimSpace(snapshot.AccountID)
}

func (p *CodexProvider) getCurrentValidToken(ctx context.Context) string {
	if p == nil || p.Credentials == nil || p.hasDirtyCredentials() {
		return ""
	}

	if cachedCredentials := p.getCachedCredentialSnapshot(ctx, 0); cachedCredentials.AccessToken != "" {
		p.adoptCachedCredentials(cachedCredentials)
		return cachedCredentials.AccessToken
	}

	if p.Credentials.AccessToken == "" || p.Credentials.NeedsRefreshWithin(0) {
		return ""
	}
	return p.Credentials.AccessToken
}

func (p *CodexProvider) loadLatestCredentialsFromDatabase(ctx context.Context) error {
	channelSnapshot := p.codexChannel()
	if channelSnapshot == nil || channelSnapshot.Id <= 0 {
		return nil
	}

	channel, err := loadLatestChannelByID(ensureContext(ctx), channelSnapshot.Id)
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
		if p.refreshNoLongerNeeded(requestCtx, lead, shouldReloadCredentials) {
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

func (p *CodexProvider) acquireDistributedForceRefreshLock(ctx context.Context, previousCredentialsVersion credentialVersionSnapshot) (*distributedRefreshLock, bool, error) {
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
			return nil, false, fmt.Errorf("failed to acquire distributed forced refresh lock for channel %d: %w", p.channelID(), err)
		}
		if acquired {
			return lock, false, nil
		}

		shouldReloadCredentials := nextCredentialReloadAt.IsZero() || !time.Now().Before(nextCredentialReloadAt)
		if shouldReloadCredentials {
			if p.credentialsChangedSince(requestCtx, previousCredentialsVersion, true) {
				p.cacheCurrentToken(requestCtx)
				return nil, true, nil
			}
			nextCredentialReloadAt = time.Now().Add(refreshCredentialReloadInterval)
		}

		if err := waitForRetry(requestCtx, refreshLockPollInterval); err != nil {
			return nil, false, fmt.Errorf("waiting for another instance to finish forced refresh: %w", err)
		}
	}
}

func (p *CodexProvider) refreshNoLongerNeeded(ctx context.Context, lead time.Duration, reloadFromDatabase bool) bool {
	if cachedCredentials := p.getCachedCredentialSnapshot(ctx, lead); cachedCredentials.AccessToken != "" {
		p.adoptCachedCredentials(cachedCredentials)
		return true
	}
	if !reloadFromDatabase {
		return false
	}
	if err := p.loadLatestCredentialsFromDatabase(ctx); err != nil {
		return false
	}
	if p.Credentials == nil || p.Credentials.RefreshToken == "" {
		return true
	}
	if !p.Credentials.NeedsRefreshWithin(lead) {
		p.cacheCurrentToken(ctx)
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
