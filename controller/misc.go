package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/stmp"
	"one-api/common/telegram"
	"one-api/model"
	"strings"

	"github.com/gin-gonic/gin"
)

func GetStatus(c *gin.Context) {
	options := config.GlobalOption.RuntimeSnapshot()
	telegramBot := ""
	if telegram.TGEnabled {
		telegramBot = telegram.TGBot.User.Username
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"version":             config.Version,
			"start_time":          config.StartTime,
			"email_verification":  options.Bool("EmailVerificationEnabled", config.EmailVerificationEnabled),
			"github_oauth":        options.Bool("GitHubOAuthEnabled", config.GitHubOAuthEnabled),
			"github_client_id":    options.String("GitHubClientId", config.GitHubClientId),
			"oidc_auth":           options.Bool("OIDCAuthEnabled", config.OIDCAuthEnabled),
			"lark_login":          options.Bool("LarkAuthEnabled", config.LarkAuthEnabled),
			"lark_client_id":      options.String("LarkClientId", config.LarkClientId),
			"system_name":         options.String("SystemName", config.SystemName),
			"logo":                options.String("Logo", config.Logo),
			"language":            config.Language,
			"footer_html":         options.String("Footer", config.Footer),
			"analytics_code":      options.String("AnalyticsCode", config.AnalyticsCode),
			"wechat_qrcode":       options.String("WeChatAccountQRCodeImageURL", config.WeChatAccountQRCodeImageURL),
			"wechat_login":        options.Bool("WeChatAuthEnabled", config.WeChatAuthEnabled),
			"server_address":      options.String("ServerAddress", config.ServerAddress),
			"turnstile_check":     options.Bool("TurnstileCheckEnabled", config.TurnstileCheckEnabled),
			"turnstile_site_key":  options.String("TurnstileSiteKey", config.TurnstileSiteKey),
			"top_up_link":         options.String("TopUpLink", config.TopUpLink),
			"chat_link":           options.String("ChatLink", config.ChatLink),
			"quota_per_unit":      options.Float64("QuotaPerUnit", config.QuotaPerUnit),
			"display_in_currency": options.Bool("DisplayInCurrencyEnabled", config.DisplayInCurrencyEnabled),
			"telegram_bot":        telegramBot,
			"mj_notify_enabled":   options.Bool("MjNotifyEnabled", config.MjNotifyEnabled),
			"chat_links":          options.String("ChatLinks", config.ChatLinks),
			"PaymentUSDRate":      options.Float64("PaymentUSDRate", config.PaymentUSDRate),
			"PaymentMinAmount":    options.Int("PaymentMinAmount", config.PaymentMinAmount),
			"RechargeDiscount":    options.String("RechargeDiscount", config.RechargeDiscount),
			"EnableSafe":          options.Bool("EnableSafe", config.EnableSafe),
			"SafeToolName":        options.String("SafeToolName", config.SafeToolName),
			"SafeKeyWords":        options.Strings("SafeKeyWords", config.SafeKeyWords, "\n"),
			"UserInvoiceMonth":    config.UserInvoiceMonth,
			"UptimeDomain":        config.UPTIMEKUMA_DOMAIN,
			"UptimePageName":      config.UPTIMEKUMA_STATUS_PAGE_NAME,
			"UptimeEnabled":       config.UPTIMEKUMA_ENABLE,
		},
	})
}

func GetNotice(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    config.GlobalOption.RuntimeSnapshot().String("Notice", ""),
	})
}

func GetAbout(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    config.GlobalOption.RuntimeSnapshot().String("About", ""),
	})
}

func GetHomePageContent(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    config.GlobalOption.RuntimeSnapshot().String("HomePageContent", ""),
	})
}

func SendEmailVerification(c *gin.Context) {
	options := config.GlobalOption.RuntimeSnapshot()
	email := c.Query("email")
	if err := common.Validate.Var(email, "required,email"); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无效的参数",
		})
		return
	}
	if options.Bool("EmailDomainRestrictionEnabled", config.EmailDomainRestrictionEnabled) {
		allowed := false
		for _, domain := range options.Strings("EmailDomainWhitelist", config.EmailDomainWhitelist, ",") {
			if strings.HasSuffix(email, "@"+domain) {
				allowed = true
				break
			}
		}
		if !allowed {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "管理员启用了邮箱域名白名单，您的邮箱地址的域名不在白名单中",
			})
			return
		}
	}
	if model.IsEmailAlreadyTaken(email) {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "邮箱地址已被占用",
		})
		return
	}
	code := common.GenerateVerificationCode(6)
	if err := model.StoreUserVerification(c.Request.Context(), email, common.EmailVerificationPurpose, code, 0); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	err := stmp.SendVerificationCodeEmail(email, code)
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
	})
}

func SendPasswordResetEmail(c *gin.Context) {
	options := config.GlobalOption.RuntimeSnapshot()
	email := c.Query("email")
	if err := common.Validate.Var(email, "required,email"); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无效的参数",
		})
		return
	}

	user, err := model.FindPasswordRecoveryUser(c.Request.Context(), email)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	userName := user.DisplayName
	if userName == "" {
		userName = user.Username
	}

	code := common.GenerateVerificationCode(0)
	if err := model.StoreUserVerification(c.Request.Context(), email, common.PasswordResetPurpose, code, user.Id); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	link := fmt.Sprintf("%s/user/reset?email=%s&token=%s", options.String("ServerAddress", config.ServerAddress), email, code)
	err = stmp.SendPasswordResetEmail(userName, email, link)

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
	})
}

type PasswordResetRequest struct {
	Email string `json:"email"`
	Token string `json:"token"`
}

func ResetPassword(c *gin.Context) {
	var req PasswordResetRequest
	err := json.NewDecoder(c.Request.Body).Decode(&req)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无效的参数",
		})
		return
	}

	if req.Email == "" || req.Token == "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无效的参数",
		})
		return
	}
	userID, err := model.ConsumeUserVerification(c.Request.Context(), req.Email, common.PasswordResetPurpose, req.Token)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "重置链接非法或已过期",
		})
		return
	}
	password := common.GenerateVerificationCode(12)
	err = model.ResetUserPassword(c.Request.Context(), userID, req.Email, password)
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
		"data":    password,
	})
}
