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
	for k, v := range config.GlobalOption.GetPublic() {
		options = append(options, &model.Option{
			Key:   k,
			Value: utils.Interface2String(v),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    options,
		"meta": gin.H{
			"sensitive_options": config.GlobalOption.GetSensitiveStatuses(),
		},
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
	if err := decodeOptionRequest(c, &request); err != nil {
		writeInvalidOptionRequest(c)
		return
	}

	value, err := normalizeOptionValue(request.Value)
	if err != nil {
		writeInvalidOptionRequest(c)
		return
	}

	prepared, err := config.PrepareOptionUpdates([]config.OptionUpdate{{
		Key:   request.Key,
		Value: value,
	}}, config.OptionGroupValidationAllowIncrementalRepair)
	if err != nil {
		writeOptionUpdateFailure(c, err)
		return
	}

	if err := persistOptionUpdates(prepared); err != nil {
		writeOptionUpdateFailure(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}
