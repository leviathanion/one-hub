package controller

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"one-api/common"
	"one-api/common/cache"
	"one-api/common/logger"
	"one-api/providers/codex"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

const (
	// Codex OAuth state cache prefix.
	CodexOAuthStateCachePrefix = "codex_oauth_state:"
	// Codex OAuth state cache duration (10 minutes).
	CodexOAuthStateCacheDuration = 10 * time.Minute
)

// Codex OAuth configuration constants.
const (
	CodexAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	CodexTokenURL     = "https://auth.openai.com/oauth/token"
	CodexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexRedirectURI  = "http://localhost:1455/auth/callback"
	CodexScopes       = "openid profile email offline_access"
)

// CodexOAuthStateData holds OAuth state data.
type CodexOAuthStateData struct {
	ChannelID    int    `json:"channel_id"`
	CodeVerifier string `json:"code_verifier"`
	State        string `json:"state"`
	Proxy        string `json:"proxy"` // Proxy config (JSON string).
	CreatedAt    int64  `json:"created_at"`
}

// StartCodexOAuthRequest starts OAuth flow.
type StartCodexOAuthRequest struct {
	ChannelID int    `json:"channel_id"` // Optional, 0 when new.
	Proxy     string `json:"proxy"`      // Optional proxy config (JSON string).
}

// generateCodexCodeVerifier creates a PKCE code verifier.
func generateCodexCodeVerifier() (string, error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b), nil
}

// generateCodexCodeChallenge creates a PKCE code challenge.
func generateCodexCodeChallenge(verifier string) string {
	hash := sha256.Sum256([]byte(verifier))
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(hash[:])
}

// StartCodexOAuth starts Codex OAuth flow.
// POST /api/codex/oauth/start
func StartCodexOAuth(c *gin.Context) {
	var req StartCodexOAuthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	// Generate random state.
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("failed to generate state: %w", err))
		return
	}
	state := base64.URLEncoding.EncodeToString(stateBytes)

	// Generate PKCE code verifier.
	codeVerifier, err := generateCodexCodeVerifier()
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("failed to generate code verifier: %w", err))
		return
	}

	// Generate code challenge.
	codeChallenge := generateCodexCodeChallenge(codeVerifier)

	// Store state in cache (with proxy).
	stateData := CodexOAuthStateData{
		ChannelID:    req.ChannelID,
		CodeVerifier: codeVerifier,
		State:        state,
		Proxy:        req.Proxy, // Store proxy for token exchange.
		CreatedAt:    time.Now().Unix(),
	}
	cacheKey := CodexOAuthStateCachePrefix + state
	cache.SetCache(cacheKey, stateData, CodexOAuthStateCacheDuration)

	// Build OAuth authorization URL.
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", CodexClientID)
	params.Set("redirect_uri", CodexRedirectURI)
	params.Set("scope", CodexScopes)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")

	authURL := fmt.Sprintf("%s?%s", CodexAuthorizeURL, params.Encode())

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"auth_url":   authURL,
			"state":      state,
			"session_id": state, // Use state as session_id.
			"instructions": []string{
				"1. Open the authorization link and sign in.",
				"2. Approve the requested permissions.",
				"3. Copy the full callback URL from the browser.",
				"4. Paste the full callback URL below.",
			},
		},
	})
}

// ExchangeCodexCodeRequest exchanges the auth code.
type ExchangeCodexCodeRequest struct {
	SessionID         string `json:"session_id"`         // session_id (state)
	AuthorizationCode string `json:"authorization_code"` // auth code or full callback URL
	CallbackURL       string `json:"callback_url"`       // full callback URL (optional)
}

// CodexOAuthCallback handles submitted auth code.
// POST /api/codex/oauth/exchange-code
func CodexOAuthCallback(c *gin.Context) {
	var req ExchangeCodexCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	if req.SessionID == "" || (req.AuthorizationCode == "" && req.CallbackURL == "") {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("session_id and authorization_code (or callback_url) are required"))
		return
	}

	state := req.SessionID

	// Load state data from cache.
	cacheKey := CodexOAuthStateCachePrefix + state
	stateData, err := cache.GetCache[CodexOAuthStateData](cacheKey)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("invalid or expired OAuth session"))
		return
	}

	// Delete used state.
	cache.DeleteCache(cacheKey)

	// Parse auth code (URL or raw code).
	inputValue := req.CallbackURL
	if inputValue == "" {
		inputValue = req.AuthorizationCode
	}

	code, err := parseCodexCallbackURL(inputValue)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("failed to parse authorization code: %w", err))
		return
	}

	// Exchange code for token (with proxy).
	tokenResp, err := exchangeCodexCodeForToken(code, stateData.CodeVerifier, stateData.State, stateData.Proxy)
	if err != nil {
		logger.SysError(fmt.Sprintf("Failed to exchange code for token: %s", err.Error()))
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("failed to exchange code for token: %w", err))
		return
	}

	// Extract account_id from id_token (fallback to access_token).
	accountID := ""
	if tokenResp.IDToken != "" {
		accountID = extractAccountIDFromToken(tokenResp.IDToken)
	}
	if accountID == "" && tokenResp.AccessToken != "" {
		accountID = extractAccountIDFromToken(tokenResp.AccessToken)
	}

	// Build credentials object.
	credentials := &codex.OAuth2Credentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ClientID:     CodexClientID,
		AccountID:    accountID,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}

	// Serialize credentials.
	credentialsJSON, err := credentials.ToJSON()
	if err != nil {
		logger.SysError(fmt.Sprintf("Failed to serialize credentials: %s", err.Error()))
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("failed to serialize credentials: %w", err))
		return
	}

	// Return success response.
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Authorization successful",
		"data": gin.H{
			"credentials": credentialsJSON,
		},
	})
}

// parseCodexCallbackURL parses callback URL or raw code.
func parseCodexCallbackURL(input string) (string, error) {
	if input == "" {
		return "", fmt.Errorf("empty input")
	}

	trimmedInput := strings.TrimSpace(input)

	// Case 1: parse full URL.
	if strings.HasPrefix(trimmedInput, "http://") || strings.HasPrefix(trimmedInput, "https://") {
		parsedURL, err := url.Parse(trimmedInput)
		if err != nil {
			return "", fmt.Errorf("invalid URL format: %w", err)
		}

		code := parsedURL.Query().Get("code")
		if code == "" {
			return "", fmt.Errorf("code parameter not found in callback URL")
		}

		return code, nil
	}

	// Case 2: raw code (may include fragments).
	cleanedCode := strings.Split(strings.Split(trimmedInput, "#")[0], "&")[0]

	// Validate code format.
	if len(cleanedCode) < 10 {
		return "", fmt.Errorf("authorization code too short")
	}

	return cleanedCode, nil
}

// extractAccountIDFromToken extracts account_id from JWT.
func extractAccountIDFromToken(accessToken string) string {
	// Parse JWT without signature verification.
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	token, _, err := parser.ParseUnverified(accessToken, jwt.MapClaims{})
	if err != nil {
		logger.SysError(fmt.Sprintf("Failed to parse JWT: %s", err.Error()))
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

// exchangeCodexCodeForToken exchanges auth code for token (proxy-aware).
func exchangeCodexCodeForToken(code, codeVerifier, state, proxyURL string) (*codex.TokenRefreshResponse, error) {
	// Prepare form-encoded request body.
	requestBody := url.Values{}
	requestBody.Set("grant_type", "authorization_code")
	requestBody.Set("client_id", CodexClientID)
	requestBody.Set("code", code)
	requestBody.Set("redirect_uri", CodexRedirectURI)
	requestBody.Set("code_verifier", codeVerifier)

	// Build request.
	req, err := http.NewRequest("POST", CodexTokenURL, strings.NewReader(requestBody.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "codex_cli_rs/0.38.0 (Ubuntu 22.4.0; x86_64) WindowsTerminal")
	req.Header.Set("Accept", "application/json")

	// Create HTTP client.
	client := &http.Client{Timeout: 30 * time.Second}

	// Apply proxy when set.
	if proxyURL != "" {
		proxyURLParsed, err := url.Parse(proxyURL)
		if err == nil {
			client.Transport = &http.Transport{
				Proxy: http.ProxyURL(proxyURLParsed),
			}
			logger.SysLog(fmt.Sprintf("Using proxy for Codex token exchange: %s", proxyURL))
		} else {
			logger.SysError(fmt.Sprintf("Failed to parse proxy URL: %s", err.Error()))
		}
	}

	// Send request.
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Read response.
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check response status.
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// Parse response.
	var tokenResp codex.TokenRefreshResponse
	if err := json.Unmarshal(bodyBytes, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &tokenResp, nil
}
