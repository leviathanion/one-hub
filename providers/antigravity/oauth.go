package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"one-api/common/requester"
)

const (
	AuthEndpoint       = "https://accounts.google.com/o/oauth2/v2/auth"
	TokenEndpoint      = "https://oauth2.googleapis.com/token"
	UserInfoEndpoint   = "https://www.googleapis.com/oauth2/v2/userinfo?alt=json"
	DefaultBaseURL     = "https://daily-cloudcode-pa.googleapis.com"
	projectBaseURL     = "https://cloudcode-pa.googleapis.com"
	DefaultRedirectURI = "http://localhost:51121/oauth-callback"
	DefaultClientID    = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	oauthBodyLimit     = 1 << 20
	controlBodyLimit   = 4 << 20
)

var ErrOAuthRefreshNotDispatched = errors.New("OAuth refresh was not dispatched")

type OAuthConfig struct {
	ClientID     string
	ClientSecret string `json:"-"`
	RedirectURI  string
	UserAgent    string
}

func DefaultOAuthConfig() OAuthConfig {
	return OAuthConfig{
		ClientID:     envDefault("ANTIGRAVITY_OAUTH_CLIENT_ID", DefaultClientID),
		ClientSecret: envDefault("ANTIGRAVITY_OAUTH_CLIENT_SECRET", defaultClientSecret),
		RedirectURI:  envDefault("ANTIGRAVITY_OAUTH_REDIRECT_URI", DefaultRedirectURI),
		UserAgent:    envDefault("ANTIGRAVITY_USER_AGENT", "antigravity/2.9.1 linux/amd64"),
	}
}

func envDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// OAuthClient 仅封装 Google 控制协议；HTTP 安全与代理连接仍使用共享 requester。
type OAuthClient struct {
	config       OAuthConfig
	do           func(*http.Request) (*http.Response, error)
	pollInterval time.Duration
}

func NewOAuthClient(proxyURL string) (*OAuthClient, error) {
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return nil, errors.New("Antigravity 代理配置无效")
		}
	}
	config := DefaultOAuthConfig()
	u, err := url.Parse(config.RedirectURI)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("Antigravity OAuth redirect 配置无效")
	}
	r := requester.NewHTTPRequester(proxyURL, nil)
	return &OAuthClient{config: config, do: r.Do, pollInterval: 2 * time.Second}, nil
}

func (c *OAuthClient) AuthorizationURL(state, challenge string) string {
	query := url.Values{
		"client_id": {c.config.ClientID}, "redirect_uri": {c.config.RedirectURI},
		"response_type": {"code"}, "state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"access_type": {"offline"}, "prompt": {"consent"},
		"scope": {"https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs"},
	}
	return AuthEndpoint + "?" + query.Encode()
}

func (c *OAuthClient) ExchangeCode(ctx context.Context, code, verifier, redirectURI string) (*Credentials, error) {
	if code == "" || verifier == "" || redirectURI != c.config.RedirectURI {
		return nil, errors.New("OAuth 授权参数与会话不一致")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	creds, err := c.exchangeToken(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirectURI}}, "")
	if err != nil {
		return nil, err
	}
	return c.completeCredentials(ctx, creds)
}

func (c *OAuthClient) Refresh(ctx context.Context, previous *Credentials) (*Credentials, error) {
	if previous == nil || previous.RefreshToken == "" {
		return nil, errors.Join(ErrOAuthRefreshNotDispatched, errors.New("refresh token 缺失"))
	}
	result, err := c.exchangeToken(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {previous.RefreshToken}}, previous.RefreshToken)
	if err != nil {
		return nil, err
	}
	result.Subject, result.ProjectID, result.Email = previous.Subject, previous.ProjectID, previous.Email
	return result, nil
}

func (c *OAuthClient) exchangeToken(ctx context.Context, form url.Values, oldRefresh string) (*Credentials, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(ErrOAuthRefreshNotDispatched, err)
	}
	form.Set("client_id", c.config.ClientID)
	form.Set("client_secret", c.config.ClientSecret)
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := c.readJSON(ctx, http.MethodPost, TokenEndpoint, "", "application/x-www-form-urlencoded", []byte(form.Encode()), oauthBodyLimit, &result); err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.AccessToken) == "" || result.ExpiresIn <= 0 || result.ExpiresIn > (1<<63-1)-time.Now().Unix() || (result.TokenType != "" && !strings.EqualFold(result.TokenType, "Bearer")) {
		return nil, errors.New("Google token 响应缺少有效 access_token 或 expires_in")
	}
	if result.RefreshToken == "" {
		result.RefreshToken = oldRefresh
	}
	return &Credentials{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, ExpiresAt: time.Now().Unix() + result.ExpiresIn}, nil
}

func (c *OAuthClient) completeCredentials(ctx context.Context, creds *Credentials) (*Credentials, error) {
	var info struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if err := c.readJSON(ctx, http.MethodGet, UserInfoEndpoint, creds.AccessToken, "", nil, oauthBodyLimit, &info); err != nil {
		return nil, err
	}
	if strings.TrimSpace(info.ID) == "" {
		return nil, errors.New("Google userinfo 缺少稳定账号 ID")
	}
	creds.Subject, creds.Email = info.ID, info.Email
	project, err := c.initializeProject(ctx, creds.AccessToken)
	if err != nil {
		return nil, err
	}
	creds.ProjectID = project
	if err := creds.Validate(); err != nil {
		return nil, err
	}
	return creds, nil
}

type projectResult struct {
	Project      json.RawMessage `json:"cloudaicompanionProject"`
	AllowedTiers []struct {
		ID      string `json:"id"`
		Default bool   `json:"isDefault"`
	} `json:"allowedTiers"`
}

func projectID(raw json.RawMessage) string {
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return strings.TrimSpace(id)
	}
	var project struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &project) == nil {
		return strings.TrimSpace(project.ID)
	}
	return ""
}

func (c *OAuthClient) initializeProject(ctx context.Context, token string) (string, error) {
	metadata := map[string]string{"ideType": "ANTIGRAVITY"}
	body, _ := json.Marshal(map[string]any{"metadata": metadata})
	var load projectResult
	if err := c.readJSON(ctx, http.MethodPost, projectBaseURL+"/v1internal:loadCodeAssist", token, "application/json", body, controlBodyLimit, &load); err != nil {
		return "", err
	}
	if id := projectID(load.Project); id != "" {
		return id, nil
	}
	tier := ""
	for _, candidate := range load.AllowedTiers {
		if candidate.Default {
			tier = candidate.ID
			break
		}
	}
	if tier == "" {
		return "", errors.New("Google 未提供 project 或可用的默认 onboarding tier")
	}
	body, _ = json.Marshal(map[string]any{"tierId": tier, "metadata": metadata})
	var operation struct {
		Name     string          `json:"name"`
		Done     bool            `json:"done"`
		Response projectResult   `json:"response"`
		Error    json.RawMessage `json:"error"`
	}
	if err := c.readJSON(ctx, http.MethodPost, DefaultBaseURL+"/v1internal:onboardUser", token, "application/json", body, controlBodyLimit, &operation); err != nil {
		return "", err
	}
	for attempts := 0; ; attempts++ {
		if len(operation.Error) > 0 && string(operation.Error) != "null" {
			return "", errors.New("Google project onboarding 失败")
		}
		if operation.Done {
			if id := projectID(operation.Response.Project); id != "" {
				return id, nil
			}
			return "", errors.New("Google onboarding 完成但未返回 project")
		}
		// 只观察上游明确返回的 operation；不能再次调用 onboardUser 猜测其幂等性。
		if attempts == 10 || !validOperationName(operation.Name) {
			return "", errors.New("Google onboarding 尚未完成，请重新检查账号状态")
		}
		name := operation.Name
		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
		operation.Name, operation.Done, operation.Response, operation.Error = "", false, projectResult{}, nil
		if err := c.readJSON(ctx, http.MethodGet, DefaultBaseURL+"/v1internal/"+name, token, "", nil, controlBodyLimit, &operation); err != nil {
			return "", err
		}
		if operation.Name == "" {
			operation.Name = name
		}
	}
}

func validOperationName(name string) bool {
	if !strings.HasPrefix(name, "operations/") || len(name) <= len("operations/") {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
		for _, ch := range component {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
				return false
			}
		}
	}
	return true
}

// readJSON 从不在错误中包含上游正文、URL 或凭据，并在解析前限制完整响应大小。
func (c *OAuthClient) readJSON(ctx context.Context, method, endpoint, token, contentType string, body []byte, limit int64, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrOAuthRefreshNotDispatched, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.Join(ErrOAuthRefreshNotDispatched, errors.New("无法构造 Google 控制请求"))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.config.UserAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.do(req)
	if err != nil {
		return errors.New("Google 控制请求传输失败，执行结果不确定")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return errors.New("读取 Google 控制响应失败")
	}
	if int64(len(data)) > limit {
		return errors.New("Google 控制响应超过大小限制")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Google 控制请求返回 HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("Google 控制响应不是有效 JSON")
	}
	return nil
}
