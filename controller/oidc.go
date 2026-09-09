package controller

import (
	"errors"
	"net/http"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/oidc"
	"one-api/common/utils"
	"one-api/model"
)

func OIDCEndpoint(c *gin.Context) {
	if !config.GlobalOption.RuntimeSnapshot().Bool("OIDCAuthEnabled", config.OIDCAuthEnabled) {
		c.JSON(http.StatusOK, gin.H{
			"message": "管理员未开启通过OIDC登录",
			"success": false,
		})
		return
	}
	oidcConfig, err := oidc.GetOIDCConfigInstanceWithContext(c.Request.Context())
	if err != nil {
		logger.SysError("获取 OIDC 配置失败, err: " + err.Error())
		c.JSON(http.StatusOK, gin.H{
			"message": "获取 OIDC 配置失败",
			"success": false,
		})
		return
	}

	session := sessions.Default(c)
	state := utils.GetRandomString(12)
	session.Set("oauth_state", state)
	loginURL := oidcConfig.LoginURL(state)
	err = session.Save()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    loginURL,
	})
}

// OIDCAuth 通过OIDC登录
// 使用签发方与 subject 确定身份；用户名只用于新用户资料。
func OIDCAuth(c *gin.Context) {
	if !config.GlobalOption.RuntimeSnapshot().Bool("OIDCAuthEnabled", config.OIDCAuthEnabled) {
		c.JSON(http.StatusOK, gin.H{
			"message": "管理员未开启通过OIDC登录",
			"success": false,
		})
		return
	}

	// 验证state参数
	session := sessions.Default(c)
	state := c.Query("state")
	if state == "" || session.Get("oauth_state") == nil || state != session.Get("oauth_state").(string) {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"message": "state is empty or not same",
		})
		return
	}

	// 获取OIDC配置
	oidcConfig, err := oidc.GetOIDCConfigInstanceWithContext(c.Request.Context())
	if err != nil {
		logger.SysError("获取 OIDC 配置失败, err: " + err.Error())
		c.JSON(http.StatusOK, gin.H{
			"message": "获取 OIDC 配置失败",
			"success": false,
		})
		return
	}

	// 处理授权码并获取token
	code := c.Query("code")
	ctx := c.Request.Context()
	token, err := oidcConfig.OAuth2Config.Exchange(ctx, code)
	if err != nil {
		c.String(http.StatusBadRequest, "Failed to exchange token: %v", err)
		return
	}

	// 验证ID Token
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "缺少 ID Token"})
		return
	}
	idToken, err := oidcConfig.Verifier.Verify(ctx, rawIDToken)
	if err != nil {
		c.String(http.StatusBadRequest, "Failed to verify ID token: %v", err)
		return
	}

	// 检测OIDC用户ID
	if idToken.Subject == "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "ID Token 中没有 Subject",
		})
		return
	}

	// 解析用户信息
	claims := make(map[string]interface{})
	if err := idToken.Claims(&claims); err != nil {
		c.String(http.StatusBadRequest, "Failed to parse claims: %v", err)
		return
	}

	// 获取用户名
	usernameClaim := config.GlobalOption.RuntimeSnapshot().String("OIDCUsernameClaims", config.OIDCUsernameClaims)
	username, ok := claims[usernameClaim].(string)
	if !ok || username == "" {
		c.JSON(http.StatusOK, gin.H{
			"message": "用户没有OIDC登录权限",
			"success": false,
		})
		return
	}

	// 初始化用户对象
	user := model.User{
		Username: username,
		OidcId:   idToken.Subject,
	}

	// 已登录主体显式绑定；否则只按签发方作用域内的稳定 subject 登录。
	if localID, ok := currentSessionUserID(c); ok {
		if err := model.UpdateUserIdentity(localID, model.UserIdentityPatch{OIDCId: &user.OidcId, OIDCIssuer: idToken.Issuer}); err != nil {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "bind"})
		return
	}
	existing, err := model.FindUserByOIDC(c.Request.Context(), idToken.Issuer, idToken.Subject)
	if err == nil {
		setupLogin(existing, c)
		return
	}

	// OIDCid查询失败，则尝试通过username查询
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		logger.SysError("查询用户错误: " + err.Error())
		c.JSON(http.StatusOK, gin.H{
			"message": err.Error(),
			"success": false,
		})
		return
	}

	// 用户不存在，尝试注册
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		logger.SysError("查询用户错误: " + err.Error())
		c.JSON(http.StatusOK, gin.H{
			"message": err.Error(),
			"success": false,
		})
		return
	}

	// 注册新用户
	if !config.GlobalOption.RuntimeSnapshot().Bool("RegisterEnabled", config.RegisterEnabled) {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "管理员关闭了新用户注册",
		})
		return
	}

	// 检测邀请码
	var inviterId int
	affCode := c.Query("aff")
	if affCode != "" {
		inviterId, _ = model.GetUserIdByAffCode(affCode)
	}
	if inviterId > 0 {
		user.InviterId = inviterId
	}
	// 填充用户信息并创建账户
	user.Username = username
	if email, ok := claims["email"]; ok && email != nil {
		user.Email, _ = email.(string)
	}
	if displayName, ok := claims["displayName"]; ok && displayName != nil {
		user.DisplayName, _ = displayName.(string)
	}
	if avatarUrl, ok := claims["avatar"]; ok && avatarUrl != nil {
		user.AvatarUrl, _ = avatarUrl.(string)
	}
	user.OidcId = idToken.Subject
	user.OIDCIssuer = idToken.Issuer
	user.Role = config.RoleCommonUser
	user.Status = config.UserStatusEnabled

	if err := user.Insert(inviterId); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	setupLogin(&user, c)
}
