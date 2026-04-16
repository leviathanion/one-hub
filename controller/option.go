package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"one-api/common/config"
	commonredis "one-api/common/redis"
	"one-api/common/utils"
	"one-api/model"
	"one-api/providers/codex"
	runtimeaffinity "one-api/runtime/channelaffinity"
	"one-api/safty"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type optionUpdateRequest struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

func normalizeOptionValue(raw any) (string, error) {
	switch value := raw.(type) {
	case string:
		return value, nil
	case json.Number:
		return value.String(), nil
	case bool:
		return strconv.FormatBool(value), nil
	default:
		return "", errors.New("unsupported option value type")
	}
}

func GetOptions(c *gin.Context) {
	var options []*model.Option
	for k, v := range config.GlobalOption.GetAll() {
		if strings.HasSuffix(k, "Token") || strings.HasSuffix(k, "Secret") {
			continue
		}
		options = append(options, &model.Option{
			Key:   k,
			Value: utils.Interface2String(v),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    options,
	})
	return
}

func GetSafeTools(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    safty.GetAllSafeToolsName(),
	})
	return
}

func GetChannelAffinityCache(c *gin.Context) {
	settings := config.ChannelAffinitySettingsInstance.Clone()
	manager := runtimeaffinity.ConfigureDefault(runtimeaffinity.ManagerOptions{
		DefaultTTL:      time.Duration(settings.DefaultTTLSeconds) * time.Second,
		JanitorInterval: time.Minute,
		MaxEntries:      settings.MaxEntries,
		RedisClient:     commonredis.GetRedisClient(),
		RedisPrefix:     "one-hub:channel-affinity",
	})
	stats := manager.Stats()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"enabled":                             settings.Enabled,
			"default_ttl_seconds":                 settings.DefaultTTLSeconds,
			"max_entries":                         settings.MaxEntries,
			"rules_count":                         len(settings.Rules),
			"rules":                               settings.Rules,
			"backend":                             stats.Backend,
			"local_entries":                       stats.LocalEntries,
			"backend_entries":                     stats.BackendEntries,
			"preferred_channel_wait_milliseconds": config.PreferredChannelWaitMilliseconds,
			"preferred_channel_wait_poll_milliseconds": config.PreferredChannelWaitPollMilliseconds,
		},
	})
}

func ClearChannelAffinityCache(c *gin.Context) {
	settings := config.ChannelAffinitySettingsInstance.Clone()
	manager := runtimeaffinity.ConfigureDefault(runtimeaffinity.ManagerOptions{
		DefaultTTL:      time.Duration(settings.DefaultTTLSeconds) * time.Second,
		JanitorInterval: time.Minute,
		MaxEntries:      settings.MaxEntries,
		RedisClient:     commonredis.GetRedisClient(),
		RedisPrefix:     "one-hub:channel-affinity",
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"cleared": manager.Clear(),
		},
	})
}

func GetExecutionSessionCache(c *gin.Context) {
	stats := codex.GetExecutionSessionStats()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"backend":                 stats.Backend,
			"local_sessions":          stats.LocalSessions,
			"local_bindings":          stats.LocalBindings,
			"backend_bindings":        stats.BackendBindings,
			"max_sessions":            stats.MaxSessions,
			"max_sessions_per_caller": stats.MaxSessionsPerCaller,
			"default_ttl_seconds":     stats.DefaultTTLSeconds,
			"managed_provider":        "codex",
		},
	})
}

func UpdateOption(c *gin.Context) {
	var request optionUpdateRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.UseNumber()
	err := decoder.Decode(&request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "无效的参数",
		})
		return
	}
	value, err := normalizeOptionValue(request.Value)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "无效的参数",
		})
		return
	}
	option := model.Option{
		Key:   request.Key,
		Value: value,
	}
	if option.Key == "AutomaticEnableChannelRecoverFrequency" {
		option.Key = "AutomaticRecoverChannelsIntervalMinutes"
	}
	shouldResetAutomaticRecoverSchedule := false
	if option.Key == "AutomaticRecoverChannelsEnabled" {
		nextEnabled := option.Value == "true"
		shouldResetAutomaticRecoverSchedule = nextEnabled != config.AutomaticRecoverChannelsEnabled
	}
	switch option.Key {
	case "GitHubOAuthEnabled":
		if option.Value == "true" && config.GitHubClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 GitHub OAuth，请先填入 GitHub Client Id 以及 GitHub Client Secret！",
			})
			return
		}
	case "OIDCAuthEnabled":
		if option.Value == "true" && (config.OIDCClientId == "" || config.OIDCClientSecret == "" || config.OIDCIssuer == "" || config.OIDCScopes == "" || config.OIDCUsernameClaims == "") {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 OIDC，请先填入OIDC信息！",
			})
			return
		}
	case "EmailDomainRestrictionEnabled":
		if option.Value == "true" && len(config.EmailDomainWhitelist) == 0 {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用邮箱域名限制，请先填入限制的邮箱域名！",
			})
			return
		}
	case "WeChatAuthEnabled":
		if option.Value == "true" && config.WeChatServerAddress == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用微信登录，请先填入微信登录相关配置信息！",
			})
			return
		}
	case "TurnstileCheckEnabled":
		if option.Value == "true" && config.TurnstileSiteKey == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 Turnstile 校验，请先填入 Turnstile 校验相关配置信息！",
			})
			return
		}
	case "AutomaticRecoverChannelsEnabled":
		if option.Value == "true" && config.AutomaticRecoverChannelsIntervalMinutes <= 0 {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用后台自动恢复探测，请先将探测间隔设置为大于 0 的分钟数！",
			})
			return
		}
	case "AutomaticRecoverChannelsIntervalMinutes":
		interval, convErr := strconv.Atoi(option.Value)
		if convErr != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "自动恢复探测间隔必须是整数！",
			})
			return
		}
		if interval < 0 {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "自动恢复探测间隔不能为负数！",
			})
			return
		}
		if config.AutomaticRecoverChannelsEnabled && interval <= 0 {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "后台自动恢复探测已开启，探测间隔必须大于 0！",
			})
			return
		}
	}
	err = model.UpdateOption(option.Key, option.Value)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	if shouldResetAutomaticRecoverSchedule {
		// Only enablement transitions reset the schedule. The due check already
		// uses the live interval value, and resetting on every interval change
		// would make increasing the interval trigger an unexpected immediate probe.
		resetAutomaticRecoverSchedule()
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
	return
}
