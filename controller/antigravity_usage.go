package controller

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/model"
	"one-api/providers/antigravity"

	"github.com/gin-gonic/gin"
)

var (
	loadAntigravityUsageChannel = model.GetChannelByIdWithContext
	fetchAntigravityUsage       = func(ctx context.Context, channel *model.Channel) (*antigravity.UsageSnapshot, error) {
		provider := antigravity.AntigravityProviderFactory{}.Create(channel).(*antigravity.AntigravityProvider)
		return provider.GetUsageSnapshot(ctx)
	}
)

// GetAntigravityChannelUsage 只在管理员主动查询时读取上游，不建立额度副本或调度状态。
func GetAntigravityChannelUsage(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.APIRespondWithError(c, http.StatusOK, errors.New("渠道 ID 无效"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	channel, err := loadAntigravityUsageChannel(ctx, id)
	if err != nil || channel == nil {
		common.APIRespondWithError(c, http.StatusOK, errors.New("渠道不存在或无法读取"))
		return
	}
	if channel.Type != config.ChannelTypeAntigravity {
		common.APIRespondWithError(c, http.StatusOK, errors.New("该渠道不是 Antigravity 渠道"))
		return
	}
	if strings.TrimSpace(channel.Key) == "" {
		common.APIRespondWithError(c, http.StatusOK, errors.New("Antigravity 渠道尚未授权"))
		return
	}
	snapshot, err := fetchAntigravityUsage(ctx, channel)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": snapshot})
}
