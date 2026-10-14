package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"one-api/common"
	"one-api/common/authutil"
	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/credentials"
	"one-api/model"
	"one-api/providers/antigravity"

	"github.com/gin-gonic/gin"
)

const (
	antigravityOAuthStatePrefix = "antigravity_oauth_state:"
	antigravityOAuthStateTTL    = 10 * time.Minute
	antigravityOAuthStateLimit  = 4 << 10
)

// 只保存授权操作快照；token 结果不进入缓存。
type antigravityOAuthState struct {
	AdminID         int    `json:"admin_id"`
	State           string `json:"state"`
	CodeVerifier    string `json:"code_verifier"`
	RedirectURI     string `json:"redirect_uri"`
	Proxy           string `json:"proxy"`
	CreatedAt       int64  `json:"created_at"`
	Operation       string `json:"operation"`
	ChannelID       int    `json:"channel_id"`
	ChannelType     int    `json:"channel_type"`
	Version         uint64 `json:"version"`
	Subject         string `json:"subject,omitempty"`
	ProjectID       string `json:"project_id,omitempty"`
	CredentialFence string `json:"credential_fence,omitempty"`
}

func antigravityOAuthCacheKey(adminID int, state string) string {
	return antigravityOAuthStatePrefix + strconv.Itoa(adminID) + ":" + state
}

// StartAntigravityOAuth 固定管理员、操作和连接快照，授权页面只接收随机 state。
func StartAntigravityOAuth(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var request struct {
		ChannelID int    `json:"channel_id"`
		Proxy     string `json:"proxy"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	adminID := c.GetInt("id")
	if adminID <= 0 || request.ChannelID < 0 {
		common.APIRespondWithError(c, http.StatusOK, errors.New("无效的管理员或渠道"))
		return
	}
	state := antigravityOAuthState{
		AdminID: adminID, Operation: "create", ChannelID: request.ChannelID,
		ChannelType: config.ChannelTypeAntigravity, CreatedAt: time.Now().Unix(),
		RedirectURI: antigravity.DefaultOAuthConfig().RedirectURI,
	}
	proxyChannel := &model.Channel{Proxy: &request.Proxy}
	if request.ChannelID != 0 {
		channel, err := model.GetChannelByIdWithContext(c.Request.Context(), request.ChannelID)
		if err != nil {
			common.APIRespondWithError(c, http.StatusOK, err)
			return
		}
		if channel.Type != config.ChannelTypeAntigravity {
			common.APIRespondWithError(c, http.StatusOK, errors.New("仅 Antigravity 渠道支持此授权流程"))
			return
		}
		state.Operation, state.Version = "edit", channel.Version
		proxyChannel = channel
		refresh, err := credentials.ReadRefresh(channel.InternalState)
		if err != nil {
			common.APIRespondWithError(c, http.StatusOK, err)
			return
		}
		previous, credentialErr := antigravity.ParseCredentials(channel.Key)
		if credentialErr == nil {
			state.Subject, state.ProjectID = previous.Subject, previous.ProjectID
		}
		if refresh != nil {
			if credentialErr != nil {
				common.APIRespondWithError(c, http.StatusOK, errors.New("无法确认待恢复渠道的账号和项目"))
				return
			}
			state.Operation, state.CredentialFence = "recover", refresh.AttemptID
		}
	}
	proxyChannel.SetProxy()
	if proxyChannel.Proxy != nil {
		state.Proxy = *proxyChannel.Proxy
	}
	client, err := antigravity.NewOAuthClient(state.Proxy)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		common.APIRespondWithError(c, http.StatusOK, errors.New("无法生成授权会话"))
		return
	}
	state.State = base64.RawURLEncoding.EncodeToString(stateBytes)
	state.CodeVerifier, err = authutil.GeneratePKCEVerifier()
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, errors.New("无法生成授权校验值"))
		return
	}
	rawState, err := json.Marshal(state)
	if err != nil || len(rawState) > antigravityOAuthStateLimit {
		common.APIRespondWithError(c, http.StatusOK, errors.New("授权配置过大"))
		return
	}
	if err := cache.SetCache(antigravityOAuthCacheKey(adminID, state.State), state, antigravityOAuthStateTTL); err != nil {
		common.APIRespondWithError(c, http.StatusOK, errors.New("无法保存授权会话，请重试"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": gin.H{
		"auth_url":   client.AuthorizationURL(state.State, authutil.PKCEChallenge(state.CodeVerifier)),
		"session_id": state.State, "operation": state.Operation, "expected_version": state.Version,
		"instructions": []string{"打开授权链接并登录 Google 账号。", "授权后复制浏览器中的完整回调 URL，粘贴到当前表单。"},
	}})
}

// AntigravityOAuthCallback 一次消费会话；任何交换歧义都要求重新授权。
func AntigravityOAuthCallback(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var request struct {
		SessionID   string `json:"session_id"`
		CallbackURL string `json:"callback_url"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	adminID := c.GetInt("id")
	if adminID <= 0 || request.SessionID == "" || len(request.SessionID) > 128 || strings.TrimSpace(request.CallbackURL) == "" {
		common.APIRespondWithError(c, http.StatusOK, errors.New("请提供授权会话和完整回调 URL"))
		return
	}
	state, err := cache.ConsumeCacheContext[antigravityOAuthState](c.Request.Context(), antigravityOAuthCacheKey(adminID, request.SessionID))
	now := time.Now().Unix()
	if err != nil || state.AdminID != adminID || state.State != request.SessionID || state.CodeVerifier == "" || state.RedirectURI == "" || state.ChannelType != config.ChannelTypeAntigravity || state.CreatedAt <= 0 || state.CreatedAt > now || now-state.CreatedAt >= int64(antigravityOAuthStateTTL/time.Second) {
		common.APIRespondWithError(c, http.StatusOK, errors.New("授权会话无效、已使用或已过期，请重新开始"))
		return
	}
	code, err := authutil.ParseOAuthCallback(request.CallbackURL, state.State, state.RedirectURI)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	expected, err := antigravityOAuthSnapshot(c.Request.Context(), state)
	if err != nil {
		common.APIRespondWithError(c, http.StatusConflict, err)
		return
	}
	client, err := antigravity.NewOAuthClient(state.Proxy)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	credential, err := client.ExchangeCode(c.Request.Context(), code, state.CodeVerifier, state.RedirectURI)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	if state.Operation == "recover" {
		// OAuth 已完成，客户端断开不能中止这次有界持久化收尾。
		commitContext, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
		defer cancel()
		if err := antigravity.RecoverCredentials(commitContext, expected, state.CredentialFence, credential); err != nil {
			common.APIRespondWithError(c, http.StatusConflict, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": gin.H{
			"operation": "recover", "channel_id": state.ChannelID, "version": state.Version + 1, "credential_saved": true,
		}})
		return
	}
	key, err := credential.ToJSON()
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": gin.H{
		"key": key, "email": credential.Email, "project_id": credential.ProjectID,
		"operation": state.Operation, "channel_id": state.ChannelID, "expected_version": state.Version,
	}})
}

func antigravityOAuthSnapshot(ctx context.Context, state antigravityOAuthState) (credentials.Snapshot, error) {
	if state.Operation == "create" && state.ChannelID == 0 && state.CredentialFence == "" {
		return credentials.Snapshot{}, nil
	}
	if (state.Operation != "edit" && state.Operation != "recover") || state.ChannelID <= 0 {
		return credentials.Snapshot{}, credentials.ErrConflict
	}
	current, err := antigravity.CredentialRotation.Store.Load(ctx, state.ChannelID)
	if err != nil {
		return credentials.Snapshot{}, err
	}
	if current.Deleted || current.Type != state.ChannelType || current.Version != state.Version {
		return credentials.Snapshot{}, credentials.ErrConflict
	}
	refresh, err := credentials.ReadRefresh(current.InternalState)
	if err != nil {
		return credentials.Snapshot{}, err
	}
	if state.Operation == "edit" {
		if state.CredentialFence != "" || refresh != nil {
			return credentials.Snapshot{}, credentials.ErrConflict
		}
		return current, nil
	}
	if refresh == nil || refresh.AttemptID != state.CredentialFence {
		return credentials.Snapshot{}, credentials.ErrConflict
	}
	previous, err := antigravity.ParseCredentials(current.Key)
	if err != nil || previous.Subject != state.Subject || previous.ProjectID != state.ProjectID {
		return credentials.Snapshot{}, fmt.Errorf("待恢复账号或项目已改变，请重新授权")
	}
	return current, nil
}
