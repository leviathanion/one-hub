package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"one-api/common"
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
	ResetsAt        int64  `json:"resets_at,omitempty"`         // Absolute reset timestamp.
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

// ErrOAuthRefreshOutcomeAmbiguous means the OAuth server may have consumed and
// rotated the submitted one-time refresh token, but the replacement credential
// could not be recovered from the response. Repeating the exchange with the old
// token is unsafe; the channel requires explicit reauthorization.
var ErrOAuthRefreshOutcomeAmbiguous = errors.New("OAuth refresh outcome is ambiguous")

// ErrOAuthRefreshNotDispatched is the only outcome that permits the durable
// fence to be canceled automatically.  Every error after client.Do starts is
// conservatively ambiguous unless the provider publishes a stronger contract.
var ErrOAuthRefreshNotDispatched = errors.New("OAuth refresh was not dispatched")

const (
	tokenRefreshErrorBodyLogLimit    = 1024
	tokenRefreshResponseBodyMaxBytes = 1 << 20
)

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

// Refresh exchanges the current refresh token exactly once.
//
// A refresh-token exchange is not safely retryable after a request may have
// reached the OAuth server: the server can rotate the one-time refresh token even
// when its response is lost. Retries belong at the maintenance-operation level,
// after durable state has been inspected, never inside this HTTP exchange.
//
// Trade-off: transport failures are classified conservatively as ambiguous even
// when the server may not have received the request. That can require manual
// reauthorization instead of an automatic retry, but it cannot destroy a valid
// replacement token by replaying the old one. A successful response is bounded
// before parsing; an oversized/unusable 200 is ambiguous for the same reason.
func (c *OAuth2Credentials) Refresh(ctx context.Context, proxyURL string) error {
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

	if err := ctx.Err(); err != nil {
		return sanitizeTokenRefreshError(fmt.Errorf("%w: token refresh canceled: %v", ErrOAuthRefreshNotDispatched, err), c, clientID)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	if proxyURL != "" {
		proxyURLParsed, err := url.Parse(proxyURL)
		if err != nil {
			return sanitizeTokenRefreshError(fmt.Errorf("%w: invalid token refresh proxy: %v", ErrOAuthRefreshNotDispatched, err), c, clientID)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURLParsed)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return sanitizeTokenRefreshError(fmt.Errorf("%w: failed to create refresh request: %v", ErrOAuthRefreshNotDispatched, err), c, clientID)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	// Empty key presence suppresses net/http's default "Go-http-client/1.1".
	req.Header.Set("User-Agent", "")

	resp, err := client.Do(req)
	if err != nil {
		return sanitizeTokenRefreshError(fmt.Errorf("%w: failed to receive response: %v", ErrOAuthRefreshOutcomeAmbiguous, err), c, clientID)
	}
	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, tokenRefreshResponseBodyMaxBytes+1))
	_ = resp.Body.Close()
	if readErr != nil {
		return sanitizeTokenRefreshError(fmt.Errorf("%w while reading response: %v", ErrOAuthRefreshOutcomeAmbiguous, readErr), c, clientID)
	}
	if len(bodyBytes) > tokenRefreshResponseBodyMaxBytes {
		return fmt.Errorf("%w: response with status %d exceeded %d bytes", ErrOAuthRefreshOutcomeAmbiguous, resp.StatusCode, tokenRefreshResponseBodyMaxBytes)
	}

	if resp.StatusCode != http.StatusOK {
		logTokenRefreshErrorBody(ctx, hasContext, resp.StatusCode, bodyBytes, c, clientID)
		var errResp TokenRefreshError
		if isTokenRefreshErrorJSON(bodyBytes, &errResp) {
			errorType := redactTokenRefreshSecrets(errResp.Error, c, clientID)
			description := redactTokenRefreshSecrets(errResp.ErrorDescription, c, clientID)
			if isNonRetryableError(errResp.Error) {
				return fmt.Errorf("%w: token refresh failed (non-retryable): %s - %s", ErrOAuthRefreshOutcomeAmbiguous, errorType, description)
			}
			return fmt.Errorf("%w: token refresh failed: %s - %s", ErrOAuthRefreshOutcomeAmbiguous, errorType, description)
		}
		return fmt.Errorf("%w: token refresh failed with status %d: non-json response", ErrOAuthRefreshOutcomeAmbiguous, resp.StatusCode)
	}

	var tokenResp TokenRefreshResponse
	if err := json.Unmarshal(bodyBytes, &tokenResp); err != nil {
		return sanitizeTokenRefreshError(fmt.Errorf("%w: successful response was unusable: %v", ErrOAuthRefreshOutcomeAmbiguous, err), c, clientID)
	}
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return fmt.Errorf("%w: successful response omitted access_token", ErrOAuthRefreshOutcomeAmbiguous)
	}
	if strings.TrimSpace(tokenResp.RefreshToken) == "" {
		return fmt.Errorf("%w: successful response omitted rotated refresh_token", ErrOAuthRefreshOutcomeAmbiguous)
	}

	c.AccessToken = tokenResp.AccessToken
	c.RefreshToken = tokenResp.RefreshToken
	if tokenResp.TokenType != "" {
		c.TokenType = tokenResp.TokenType
	}
	if accountID := extractAccountIDFromJWT(tokenResp.AccessToken); accountID != "" {
		c.AccountID = accountID
	}
	if tokenResp.Scope != "" {
		c.Scopes = strings.Fields(tokenResp.Scope)
	}
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

func isTokenRefreshErrorJSON(bodyBytes []byte, errResp *TokenRefreshError) bool {
	if errResp == nil {
		return false
	}
	if err := json.Unmarshal(bodyBytes, errResp); err != nil {
		return false
	}
	return strings.TrimSpace(errResp.Error) != "" || strings.TrimSpace(errResp.ErrorDescription) != ""
}

func logTokenRefreshErrorBody(ctx context.Context, hasContext bool, statusCode int, bodyBytes []byte, creds *OAuth2Credentials, clientID string) {
	snippet := tokenRefreshErrorBodyLogSnippet(bodyBytes, creds, clientID)
	message := fmt.Sprintf("[Codex] Token refresh endpoint returned status %d body=%q", statusCode, snippet)
	if len(bodyBytes) > tokenRefreshErrorBodyLogLimit {
		message += " truncated=true"
	}
	if hasContext {
		logger.LogError(ctx, message)
		return
	}
	logger.SysError(message)
}

func tokenRefreshErrorBodyLogSnippet(bodyBytes []byte, creds *OAuth2Credentials, clientID string) string {
	if len(bodyBytes) > tokenRefreshErrorBodyLogLimit {
		bodyBytes = bodyBytes[:tokenRefreshErrorBodyLogLimit]
	}
	text := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, string(bodyBytes))
	for _, secret := range tokenRefreshKnownSecrets(creds, clientID) {
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	text = common.RedactSensitiveAssignments(text)
	if len(text) > tokenRefreshErrorBodyLogLimit {
		text = text[:tokenRefreshErrorBodyLogLimit]
	}
	return text
}

func redactTokenRefreshSecrets(text string, creds *OAuth2Credentials, clientID string) string {
	for _, secret := range tokenRefreshKnownSecrets(creds, clientID) {
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	return common.RedactSensitiveAssignments(text)
}

type sanitizedTokenRefreshError struct {
	message string
	cause   error
}

func (e *sanitizedTokenRefreshError) Error() string { return e.message }

// Is preserves classification (including context cancellation/deadline and
// caller sentinels) without making the secret-bearing cause reachable through
// errors.Unwrap or errors.As.
func (e *sanitizedTokenRefreshError) Is(target error) bool {
	return e != nil && errors.Is(e.cause, target)
}

func sanitizeTokenRefreshError(err error, creds *OAuth2Credentials, clientID string) error {
	if err == nil {
		return nil
	}
	return &sanitizedTokenRefreshError{
		message: redactTokenRefreshSecrets(err.Error(), creds, clientID),
		cause:   err,
	}
}

func tokenRefreshKnownSecrets(creds *OAuth2Credentials, clientID string) []string {
	accessToken := ""
	refreshToken := ""
	if creds != nil {
		accessToken = creds.AccessToken
		refreshToken = creds.RefreshToken
	}

	values := make([]string, 0, 3)
	seen := map[string]struct{}{}
	for _, value := range []string{clientID, accessToken, refreshToken} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	// A shorter credential can be a substring of a longer one. Replacing the
	// containing value first prevents a secret suffix from surviving redaction.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
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
