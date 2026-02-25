package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"one-api/common/cache"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

const TokenCacheKey = "api_token:codex"

// OAuth2 config constants.
const (
	DefaultClientID = "pdlLIX2Y72MIl2rhLhTE9VV9bN905kBh"
	TokenEndpoint   = "https://auth0.openai.com/oauth/token"
)

type CodexProviderFactory struct{}

// Create CodexProvider.
func (f CodexProviderFactory) Create(channel *model.Channel) base.ProviderInterface {
	provider := &CodexProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Config:          getConfig(),
				Channel:         channel,
				Requester:       requester.NewHTTPRequester(*channel.Proxy, RequestErrorHandle("")),
				SupportResponse: true,
			},
			SupportStreamOptions: true,
		},
	}

	// Parse config.
	parseCodexConfig(provider)

	// Update RequestErrorHandle with actual token.
	if provider.Credentials != nil {
		provider.Requester = requester.NewHTTPRequester(*channel.Proxy, RequestErrorHandle(provider.Credentials.AccessToken))
	}

	return provider
}

// parseCodexConfig parses Codex config.
// Supports:
// 1) JSON credentials (access_token, refresh_token, etc) with auto refresh.
// 2) Plain access_token (no auto refresh).
func parseCodexConfig(provider *CodexProvider) {
	channel := provider.Channel

	if channel.Key == "" {
		return
	}

	key := strings.TrimSpace(channel.Key)

	// Try JSON credentials first.
	creds, err := FromJSON(key)
	if err == nil {
		provider.Credentials = creds

		// Default ClientID when missing.
		if provider.Credentials.ClientID == "" {
			provider.Credentials.ClientID = DefaultClientID
		}

		// Infer AccountID from access_token.
		if provider.Credentials.AccountID == "" && provider.Credentials.AccessToken != "" {
			if accountID := extractAccountIDFromJWT(provider.Credentials.AccessToken); accountID != "" {
				provider.Credentials.AccountID = accountID
			}
		}

		// Default expiry when refresh_token exists.
		if provider.Credentials.RefreshToken != "" && provider.Credentials.ExpiresAt.IsZero() {
			provider.Credentials.ExpiresAt = time.Now().Add(1 * time.Hour)
		}

		return
	}

	// Fallback to plain access_token.
	accountID := extractAccountIDFromJWT(key)
	provider.Credentials = &OAuth2Credentials{
		AccessToken: key,
		AccountID:   accountID,
	}
}

type CodexProvider struct {
	openai.OpenAIProvider
	Credentials *OAuth2Credentials // OAuth2 credentials (with refresh_token).
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

	// Use cache.
	cacheKey := fmt.Sprintf("%s:%d", TokenCacheKey, p.Channel.Id)
	cachedToken, _ := cache.GetCache[string](cacheKey)
	if cachedToken != "" {
		return cachedToken, nil
	}

	needsUpdate := false
	if p.Credentials.IsExpired() {
		proxyURL := ""
		if p.Channel.Proxy != nil && *p.Channel.Proxy != "" {
			proxyURL = *p.Channel.Proxy
		}

		if err := p.Credentials.Refresh(ctx, proxyURL, 3); err != nil {
			logger.LogError(ctx, fmt.Sprintf("Failed to refresh codex token: %s", err.Error()))
			return "", fmt.Errorf("failed to refresh token: %w", err)
		}

		needsUpdate = true
	}

	if needsUpdate {
		if err := p.saveCredentialsToDatabase(ctx); err != nil {
			logger.LogError(ctx, fmt.Sprintf("Failed to save refreshed credentials to database: %s", err.Error()))
		}
	}

	// Cache token (default 55 minutes).
	cacheDuration := 55 * time.Minute
	if !p.Credentials.ExpiresAt.IsZero() {
		timeUntilExpiry := time.Until(p.Credentials.ExpiresAt)
		if timeUntilExpiry > 0 && timeUntilExpiry < cacheDuration {
			cacheDuration = timeUntilExpiry
		}
	}

	cache.SetCache(cacheKey, p.Credentials.AccessToken, cacheDuration)

	return p.Credentials.AccessToken, nil
}

func (p *CodexProvider) saveCredentialsToDatabase(ctx context.Context) error {
	credentialsJSON, err := p.Credentials.ToJSON()
	if err != nil {
		return fmt.Errorf("failed to serialize credentials: %w", err)
	}

	if err := model.UpdateChannelKey(p.Channel.Id, credentialsJSON); err != nil {
		return fmt.Errorf("failed to update channel key: %w", err)
	}

	logger.LogInfo(ctx, fmt.Sprintf("[Codex] Credentials saved to database for channel %d", p.Channel.Id))
	return nil
}
