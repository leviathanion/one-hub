package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"one-api/common"
	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/model"
	"one-api/providers/codex"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// Codex OAuth state cache prefix.
	CodexOAuthStateCachePrefix = "codex_oauth_state:"
	// Codex OAuth state cache duration (10 minutes).
	CodexOAuthStateCacheDuration = 10 * time.Minute
)

// CodexOAuthStateData holds OAuth state data.
type CodexOAuthStateData struct {
	ChannelID          int    `json:"channel_id"`
	CodeVerifier       string `json:"code_verifier"`
	State              string `json:"state"`
	Proxy              string `json:"proxy"` // Proxy config (JSON string).
	CreatedAt          int64  `json:"created_at"`
	CredentialRevision uint64 `json:"credential_revision"`
	CredentialFence    string `json:"credential_fence,omitempty"`
	AccountID          string `json:"account_id,omitempty"`
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
	stateData := CodexOAuthStateData{ChannelID: req.ChannelID, Proxy: req.Proxy}
	if req.ChannelID != 0 {
		channel, err := model.GetChannelByIdWithContext(c.Request.Context(), req.ChannelID)
		if err != nil {
			common.APIRespondWithError(c, http.StatusOK, err)
			return
		}
		if channel.Type != config.ChannelTypeCodex {
			common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("仅 Codex 渠道支持此授权流程"))
			return
		}
		stateData.AccountID = model.CodexCredentialAccountID(channel.Key)
		if stateData.AccountID == "" {
			common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("无法确认原渠道账号，身份字段不可原地编辑，请新建渠道"))
			return
		}
		stateData.CredentialRevision = channel.CredentialRevision
		if channel.CredentialRefreshFence != nil {
			stateData.CredentialFence = *channel.CredentialRefreshFence
		}
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
	stateData.CodeVerifier = codeVerifier
	stateData.State = state
	stateData.CreatedAt = time.Now().Unix()
	cacheKey := CodexOAuthStateCachePrefix + state
	if err := cache.SetCache(cacheKey, stateData, CodexOAuthStateCacheDuration); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	// Build OAuth authorization URL.
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", codex.DefaultClientID)
	params.Set("redirect_uri", codex.DefaultRedirectURI)
	params.Set("scope", codex.DefaultScope)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")

	authURL := fmt.Sprintf("%s?%s", codex.AuthorizeEndpoint, params.Encode())

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

	// 原子消费 state；任何消费失败均不得继续交换或回退读取。
	cacheKey := CodexOAuthStateCachePrefix + state
	stateData, err := cache.ConsumeCacheContext[CodexOAuthStateData](c.Request.Context(), cacheKey)
	now := time.Now().Unix()
	if err != nil || stateData.State != state || strings.TrimSpace(stateData.CodeVerifier) == "" || stateData.CreatedAt <= 0 || stateData.CreatedAt > now || now-stateData.CreatedAt >= int64(CodexOAuthStateCacheDuration/time.Second) {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("invalid or expired OAuth session"))
		return
	}

	// Parse auth code (URL or raw code).
	inputValue := req.CallbackURL
	if inputValue == "" {
		inputValue = req.AuthorizationCode
	}

	code, err := parseCodexCallbackURL(inputValue, state)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("failed to parse authorization code: %w", err))
		return
	}

	var ticket model.CredentialRotationTicket
	credentialSaved := false
	if stateData.ChannelID != 0 && stateData.CredentialFence == "" {
		ticket = model.CredentialRotationTicket{ChannelID: stateData.ChannelID, ExpectedRevision: stateData.CredentialRevision, AttemptID: uuid.NewString()}
		outcome, err := model.ClaimCredentialRotation(c.Request.Context(), ticket, time.Now())
		if err != nil {
			common.APIRespondWithError(c, http.StatusOK, err)
			return
		}
		if outcome != model.CredentialRotationClaimAcquired {
			common.APIRespondWithError(c, http.StatusOK, model.ErrChannelCredentialConflict)
			return
		}
		defer func() {
			if credentialSaved {
				return
			}
			// 独立授权码交换没有发送原 refresh token，失败时可释放其工作门禁。
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
			defer cancel()
			if _, err := model.CancelCredentialRotationBeforeDispatch(cleanupCtx, ticket); err != nil {
				logger.SysError(fmt.Sprintf("failed to release Codex authorization claim for channel %d: %v", ticket.ChannelID, err))
			}
		}()
	}

	// Exchange code for token (with proxy).
	tokenResp, err := exchangeCodexCodeForToken(code, stateData.CodeVerifier, stateData.State, stateData.Proxy)
	if err != nil {
		logger.SysError(fmt.Sprintf("Failed to exchange code for token: %s", err.Error()))
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("failed to exchange code for token: %w", err))
		return
	}

	// 上游即使返回 HTTP 200，也可能只含 id_token 或空载荷而未签发 access_token。
	// 此时不得构造空 access 凭据：不保存、不清除 fence、不报告成功，也不回填任何旧凭据。
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("授权结果缺少访问令牌，请重新发起授权"))
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
	if stateData.ChannelID != 0 && (stateData.AccountID == "" || accountID != stateData.AccountID) {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("授权账号与原渠道不一致，身份字段不可原地编辑，请新建渠道"))
		return
	}

	// Build credentials object.
	credentials := &codex.OAuth2Credentials{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ClientID:     codex.DefaultClientID,
		AccountID:    accountID,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}
	if tokenResp.Scope != "" {
		credentials.Scopes = strings.Fields(tokenResp.Scope)
	}

	// Serialize credentials.
	credentialsJSON, err := credentials.ToJSON()
	if err != nil {
		logger.SysError(fmt.Sprintf("Failed to serialize credentials: %s", err.Error()))
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("failed to serialize credentials: %w", err))
		return
	}
	if stateData.ChannelID != 0 {
		var saveErr error
		if stateData.CredentialFence != "" {
			saveErr = model.RecoverChannelCredentialWithContext(c.Request.Context(), model.CredentialRecoverySnapshot{
				ChannelID: stateData.ChannelID, AccountID: stateData.AccountID,
				ExpectedRevision: stateData.CredentialRevision, ExpectedFence: stateData.CredentialFence,
			}, credentialsJSON)
		} else {
			saveErr = model.ReplaceChannelCredentialWithContext(c.Request.Context(), ticket, credentialsJSON)
		}
		if saveErr != nil {
			if errors.Is(saveErr, model.ErrChannelCredentialConflict) {
				common.APIRespondWithError(c, http.StatusOK, model.ErrChannelCredentialConflict)
			} else {
				logger.SysError(fmt.Sprintf("Codex credential persistence failed for channel %d", stateData.ChannelID))
				common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("保存渠道凭据失败，请重新发起授权"))
			}
			return
		}
		credentialSaved = true
	}

	// Return success response.
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Authorization successful",
		"data": gin.H{
			"credentials":      credentialsJSON,
			"credential_saved": stateData.ChannelID != 0,
		},
	})
}

// parseCodexCallbackURL parses callback URL or raw code.
func parseCodexCallbackURL(input, expectedState string) (string, error) {
	if input == "" {
		return "", fmt.Errorf("empty input")
	}

	trimmedInput := strings.TrimSpace(input)

	// Case 1: parse full URL.
	if strings.HasPrefix(trimmedInput, "http://") || strings.HasPrefix(trimmedInput, "https://") {
		parsedURL, err := url.Parse(trimmedInput)
		if err != nil {
			return "", fmt.Errorf("invalid URL format")
		}

		query, err := url.ParseQuery(parsedURL.RawQuery)
		if err != nil {
			return "", fmt.Errorf("invalid callback query")
		}
		states := query["state"]
		if len(states) != 1 || states[0] != expectedState {
			return "", fmt.Errorf("callback state does not match OAuth session")
		}
		codes := query["code"]
		if len(codes) != 1 || codes[0] == "" {
			return "", fmt.Errorf("callback URL must contain one authorization code")
		}
		return codes[0], nil
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
	return model.CodexTokenAccountID(accessToken)
}

// exchangeCodexCodeForToken exchanges auth code for token (proxy-aware).
func exchangeCodexCodeForToken(code, codeVerifier, state, proxyURL string) (*codex.TokenRefreshResponse, error) {
	// Prepare form-encoded request body.
	requestBody := url.Values{}
	requestBody.Set("grant_type", "authorization_code")
	requestBody.Set("client_id", codex.DefaultClientID)
	requestBody.Set("code", code)
	requestBody.Set("redirect_uri", codex.DefaultRedirectURI)
	requestBody.Set("code_verifier", codeVerifier)

	// Build request.
	req, err := http.NewRequest("POST", codex.TokenEndpoint, strings.NewReader(requestBody.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// Empty key presence suppresses net/http's default "Go-http-client/1.1"
	// User-Agent on the wire. Trade-off: this relies on Go's documented
	// request serialization behavior, but keeps OAuth identity separate from
	// Codex API request identity.
	req.Header.Set("User-Agent", "")

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
		return nil, fmt.Errorf("token exchange failed with status %d", resp.StatusCode)
	}

	// Parse response.
	var tokenResp codex.TokenRefreshResponse
	if err := json.Unmarshal(bodyBytes, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &tokenResp, nil
}
