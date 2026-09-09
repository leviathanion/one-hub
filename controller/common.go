package controller

import (
	"context"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/notify"
	"one-api/model"
	"one-api/types"
	"strings"

	"github.com/gin-gonic/gin"
)

func ShouldDisableChannel(channelType int, err *types.OpenAIErrorWithStatusCode) bool {
	options := config.GlobalOption.RuntimeSnapshot()
	if !options.Bool("AutomaticDisableChannelEnabled", config.AutomaticDisableChannelEnabled) || err == nil || err.LocalError {
		return false
	}

	// 状态码检查
	if err.StatusCode == http.StatusUnauthorized {
		return true
	}
	if err.StatusCode == http.StatusForbidden && channelType == config.ChannelTypeGemini {
		return true
	}

	if err.ProviderQuotaExhausted || err.ProviderAuthRejected ||
		common.ProviderErrorIsQuotaExhausted(err.OpenAIError) || common.ProviderErrorIsAuthRejected(err.OpenAIError) {
		return true
	}
	if err.ProviderRateLimited {
		return false
	}

	code := strings.ToLower(common.OpenAIErrorCodeText(err.OpenAIError.Code))
	errType := strings.ToLower(strings.TrimSpace(err.OpenAIError.Type))
	if common.ProviderErrorIsRateLimited(err.OpenAIError) || code == "invalid_request_error" || errType == "invalid_request_error" {
		return false
	}
	message := err.OpenAIError.Message
	currentKeywords := strings.Split(common.DisableChannelKeywordsInstance.GetKeywords(), "\n")
	for _, keyword := range options.Strings("DisableChannelKeywords", currentKeywords, "\n") {
		if strings.Contains(message, keyword) {
			return true
		}
	}
	return false
}

// disable & notify
func DisableChannel(channelId int, channelName string, reason string, sendNotify bool) {
	model.UpdateChannelStatusById(channelId, config.ChannelStatusAutoDisabled)
	if !sendNotify {
		return
	}

	subject := fmt.Sprintf("通道「%s」（#%d）已被禁用", channelName, channelId)
	content := fmt.Sprintf("通道「%s」（#%d）已被禁用，原因：%s", channelName, channelId, reason)
	notify.Send(subject, content)
}

func AutoDisableChannel(channelId int, channelName string, reason string, sendNotify bool, notifyContexts ...context.Context) (bool, error) {
	updated, err := model.UpdateChannelStatusIfCurrent(channelId, config.ChannelStatusEnabled, config.ChannelStatusAutoDisabled)
	if err != nil || !updated {
		return updated, err
	}
	if !sendNotify {
		return true, nil
	}

	subject := fmt.Sprintf("通道「%s」（#%d）已被禁用", channelName, channelId)
	content := fmt.Sprintf("通道「%s」（#%d）已被禁用，原因：%s", channelName, channelId, reason)
	if len(notifyContexts) > 0 {
		notify.SendContext(notifyContexts[0], subject, content)
	} else {
		notify.Send(subject, content)
	}
	return true, nil
}

// enable & notify
func EnableChannel(channelId int, channelName string, sendNotify bool) {
	model.UpdateChannelStatusById(channelId, config.ChannelStatusEnabled)
	if !sendNotify {
		return
	}

	subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
	content := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
	notify.Send(subject, content)
}

func AutoEnableChannel(channelId int, channelName string, sendNotify bool) (bool, error) {
	updated, err := model.UpdateChannelStatusIfCurrent(channelId, config.ChannelStatusAutoDisabled, config.ChannelStatusEnabled)
	if err != nil || !updated {
		return updated, err
	}
	if !sendNotify {
		return true, nil
	}

	subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
	content := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
	notify.Send(subject, content)
	return true, nil
}

func RelayNotFound(c *gin.Context) {
	err := types.OpenAIError{
		Message: fmt.Sprintf("Invalid URL (%s %s)", c.Request.Method, c.Request.URL.Path),
		Type:    "invalid_request_error",
		Param:   "",
		Code:    "",
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": err,
	})
}
