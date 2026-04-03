package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"one-api/common/logger"

	"github.com/golang-jwt/jwt/v5"
)

// CodexErrorResponse represents Codex error payloads.
type CodexErrorResponse struct {
	Error CodexErrorDetail `json:"error"`
}

// CodexErrorDetail holds Codex error details.
type CodexErrorDetail struct {
	Message         string `json:"message"`
	Type            string `json:"type"`
	Code            any    `json:"code,omitempty"`
	ResetsInSeconds int    `json:"resets_in_seconds,omitempty"` // 429 reset time (seconds).
	ResetsIn        int    `json:"resets_in,omitempty"`         // Fallback field.
}

// OAuth2Credentials holds OAuth2 credentials.
type OAuth2Credentials struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ClientID     string    `json:"client_id,omitempty"`
	AccountID    string    `json:"account_id,omitempty"` // ChatGPT Account ID from token.
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
}

// TokenRefreshResponse is the OAuth2 refresh response.
type TokenRefreshResponse struct {
	IDToken      string `json:"id_token,omitempty"` // ID Token (returned on auth code exchange).
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope,omitempty"`
}

// TokenRefreshError is the OAuth2 error response.
type TokenRefreshError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type oauth2CredentialsPayload struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	ClientID     string   `json:"client_id,omitempty"`
	AccountID    string   `json:"account_id,omitempty"`
	ExpiresAt    any      `json:"expires_at,omitempty"`
	TokenType    string   `json:"token_type,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
}

// IsExpired reports whether token is expired (3 minute buffer).
func (c *OAuth2Credentials) IsExpired() bool {
	return c.NeedsRefreshWithin(3 * time.Minute)
}

// NeedsRefreshWithin reports whether the token should be refreshed within the given lead time.
func (c *OAuth2Credentials) NeedsRefreshWithin(lead time.Duration) bool {
	if c.ExpiresAt.IsZero() {
		return true
	}
	if lead < 0 {
		lead = 0
	}
	return time.Now().Add(lead).After(c.ExpiresAt)
}

// Refresh refreshes the access token.
func (c *OAuth2Credentials) Refresh(ctx context.Context, proxyURL string, maxRetries int) error {
	if c.RefreshToken == "" {
		return fmt.Errorf("refresh token is empty")
	}
	hasContext := ctx != nil
	ctx = ensureContext(ctx)

	// Default client_id when missing.
	clientID := c.ClientID
	if clientID == "" {
		clientID = DefaultClientID
	}

	// Prepare request body.
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("client_id", clientID)
	data.Set("refresh_token", c.RefreshToken)
	if scope := joinedScopes(c.Scopes); scope != "" {
		data.Set("scope", scope)
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("token refresh canceled: %w", err)
		}
		if attempt > 0 {
			// Exponential backoff, max 30s.
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			if hasContext {
				logger.LogError(ctx, fmt.Sprintf("[Codex] Token refresh retry %d/%d after %v", attempt, maxRetries, backoff))
			} else {
				logger.SysLog(fmt.Sprintf("[Codex] Token refresh retry %d/%d after %v", attempt, maxRetries, backoff))
			}
			if err := waitForRetry(ctx, backoff); err != nil {
				return fmt.Errorf("token refresh canceled during backoff: %w", err)
			}
		}

		// Create HTTP client.
		client := &http.Client{
			Timeout: 30 * time.Second,
		}

		// Apply proxy when set.
		if proxyURL != "" {
			proxyURLParsed, err := url.Parse(proxyURL)
			if err == nil {
				client.Transport = &http.Transport{
					Proxy: http.ProxyURL(proxyURLParsed),
				}
			}
		}

		// Send refresh request.
		req, err := http.NewRequestWithContext(ctx, "POST", TokenEndpoint, strings.NewReader(data.Encode()))
		if err != nil {
			lastErr = fmt.Errorf("failed to create refresh request: %w", err)
			continue
		}

		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", defaultUserAgent)
		req.Header.Set("Accept", "application/json, text/plain, */*")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("failed to send refresh request: %w", err)
			continue
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read refresh response: %w", err)
			continue
		}

		// Check response status.
		if resp.StatusCode != http.StatusOK {
			// Parse error response.
			var errResp TokenRefreshError
			if err := json.Unmarshal(bodyBytes, &errResp); err == nil {
				// Abort on non-retryable errors.
				if isNonRetryableError(errResp.Error) {
					return fmt.Errorf("token refresh failed (non-retryable): %s - %s", errResp.Error, errResp.ErrorDescription)
				}
				lastErr = fmt.Errorf("token refresh failed: %s - %s", errResp.Error, errResp.ErrorDescription)
			} else {
				lastErr = fmt.Errorf("token refresh failed with status %d: %s", resp.StatusCode, string(bodyBytes))
			}
			continue
		}

		// Parse success response.
		var tokenResp TokenRefreshResponse
		if err := json.Unmarshal(bodyBytes, &tokenResp); err != nil {
			lastErr = fmt.Errorf("failed to parse refresh response: %w", err)
			continue
		}

		// Update credentials.
		c.AccessToken = tokenResp.AccessToken
		if tokenResp.RefreshToken != "" {
			c.RefreshToken = tokenResp.RefreshToken
		}
		if tokenResp.TokenType != "" {
			c.TokenType = tokenResp.TokenType
		}

		// Extract account_id from access_token.
		if accountID := extractAccountIDFromJWT(tokenResp.AccessToken); accountID != "" {
			c.AccountID = accountID
		}

		// Parse scope.
		if tokenResp.Scope != "" {
			c.Scopes = strings.Fields(tokenResp.Scope)
		}

		// Compute expiry time.
		if tokenResp.ExpiresIn > 0 {
			c.ExpiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
		}

		if hasContext {
			logger.LogInfo(ctx, fmt.Sprintf("[Codex] Token refreshed successfully, expires at: %s", c.ExpiresAt.Format(time.RFC3339)))
		} else {
			logger.SysLog(fmt.Sprintf("[Codex] Token refreshed successfully, expires at: %s", c.ExpiresAt.Format(time.RFC3339)))
		}
		return nil
	}

	return fmt.Errorf("token refresh failed after %d retries: %w", maxRetries, lastErr)
}

func ensureContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func joinedScopes(scopes []string) string {
	if len(scopes) == 0 {
		return ""
	}

	filtered := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		filtered = append(filtered, scope)
	}
	return strings.Join(filtered, " ")
}

// extractAccountIDFromJWT extracts account_id from JWT access_token.
func extractAccountIDFromJWT(accessToken string) string {
	// Parse JWT without signature verification.
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	token, _, err := parser.ParseUnverified(accessToken, jwt.MapClaims{})
	if err != nil {
		return ""
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return ""
	}

	// Extract https://api.openai.com/auth.chatgpt_account_id.
	authClaims, ok := claims["https://api.openai.com/auth"].(map[string]interface{})
	if !ok {
		return ""
	}

	accountID, ok := authClaims["chatgpt_account_id"].(string)
	if !ok {
		return ""
	}

	return accountID
}

// isNonRetryableError reports non-retryable errors.
func isNonRetryableError(errorType string) bool {
	nonRetryableErrors := []string{
		"invalid_grant",
		"invalid_client",
		"unauthorized_client",
		"access_denied",
		"unsupported_grant_type",
		"invalid_scope",
	}

	for _, e := range nonRetryableErrors {
		if errorType == e {
			return true
		}
	}
	return false
}

// ToJSON serializes credentials.
func (c *OAuth2Credentials) ToJSON() (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// FromJSON deserializes credentials.
func FromJSON(jsonStr string) (*OAuth2Credentials, error) {
	var payload oauth2CredentialsPayload
	decoder := json.NewDecoder(strings.NewReader(jsonStr))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}

	creds := &OAuth2Credentials{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		ClientID:     payload.ClientID,
		AccountID:    payload.AccountID,
		TokenType:    payload.TokenType,
		Scopes:       payload.Scopes,
	}
	if ts, ok := parseExpiryValue(payload.ExpiresAt); ok {
		creds.ExpiresAt = ts
	}

	if creds.ExpiresAt.IsZero() {
		var raw map[string]any
		decoder = json.NewDecoder(strings.NewReader(jsonStr))
		decoder.UseNumber()
		if err := decoder.Decode(&raw); err == nil {
			creds.ExpiresAt = parseCredentialExpiry(raw)
		}
	}

	return creds, nil
}

func parseCredentialExpiry(raw map[string]any) time.Time {
	for _, key := range []string{"expires_at", "expired", "expire", "expiresAt", "expiry", "expires"} {
		value, ok := raw[key]
		if !ok {
			continue
		}

		if ts, ok := parseExpiryValue(value); ok {
			return ts
		}
	}

	return time.Time{}
}

func parseExpiryValue(value any) (time.Time, bool) {
	switch v := value.(type) {
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return time.Time{}, false
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if ts, err := time.Parse(layout, text); err == nil {
				return ts, true
			}
		}
	case json.Number:
		if unix, err := v.Int64(); err == nil && unix > 0 {
			return time.Unix(unix, 0), true
		}
	}

	return time.Time{}, false
}
