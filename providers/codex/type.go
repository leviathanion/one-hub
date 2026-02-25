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

// IsExpired reports whether token is expired (3 minute buffer).
func (c *OAuth2Credentials) IsExpired() bool {
	if c.ExpiresAt.IsZero() {
		return true
	}

	buffer := 3 * time.Minute
	return time.Now().Add(buffer).After(c.ExpiresAt)
}

// Refresh refreshes the access token.
func (c *OAuth2Credentials) Refresh(ctx context.Context, proxyURL string, maxRetries int) error {
	if c.RefreshToken == "" {
		return fmt.Errorf("refresh token is empty")
	}

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

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff, max 30s.
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			if ctx != nil {
				logger.LogError(ctx, fmt.Sprintf("[Codex] Token refresh retry %d/%d after %v", attempt, maxRetries, backoff))
			} else {
				logger.SysLog(fmt.Sprintf("[Codex] Token refresh retry %d/%d after %v", attempt, maxRetries, backoff))
			}
			time.Sleep(backoff)
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
		req, err := http.NewRequest("POST", TokenEndpoint, strings.NewReader(data.Encode()))
		if err != nil {
			lastErr = fmt.Errorf("failed to create refresh request: %w", err)
			continue
		}

		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", "codex_cli_rs/0.38.0 (Ubuntu 22.4.0; x86_64) WindowsTerminal")
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
			c.Scopes = strings.Split(tokenResp.Scope, " ")
		}

		// Compute expiry time.
		if tokenResp.ExpiresIn > 0 {
			c.ExpiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
		}

		if ctx != nil {
			logger.LogInfo(ctx, fmt.Sprintf("[Codex] Token refreshed successfully, expires at: %s", c.ExpiresAt.Format(time.RFC3339)))
		} else {
			logger.SysLog(fmt.Sprintf("[Codex] Token refreshed successfully, expires at: %s", c.ExpiresAt.Format(time.RFC3339)))
		}
		return nil
	}

	return fmt.Errorf("token refresh failed after %d retries: %w", maxRetries, lastErr)
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
	var creds OAuth2Credentials
	if err := json.Unmarshal([]byte(jsonStr), &creds); err != nil {
		return nil, err
	}
	return &creds, nil
}
