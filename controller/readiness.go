package controller

import (
	"context"
	"net/http"
	"time"

	"one-api/common/config"
	commonredis "one-api/common/redis"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

type readinessData struct {
	Database    string                 `json:"database"`
	Redis       string                 `json:"redis"`
	ResponsesWS map[string]interface{} `json:"responses_ws"`
}

func Readyz(c *gin.Context) {
	data, ready := readinessStatus(c.Request.Context())
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, gin.H{
		"success": ready,
		"data":    data,
	})
}

func readinessStatus(ctx context.Context) (readinessData, bool) {
	data := readinessData{
		Database: "ok",
		Redis:    "disabled",
		ResponsesWS: map[string]interface{}{
			"bridge_open_timeout_enabled": config.ResponsesWSBridgeOpenTimeout() > 0,
			"active_lease_backend":        "local",
		},
	}
	ready := true

	if !databaseReady(ctx) {
		data.Database = "unavailable"
		ready = false
	}

	if !config.RedisEnabled {
		return data, ready
	}

	data.Redis = "ok"
	data.ResponsesWS["active_lease_backend"] = "redis"
	if redisReady(ctx) {
		return data, ready
	}

	if config.ResponsesWSActiveLeaseRedisFailOpen() {
		data.Redis = "degraded_fail_open"
		data.ResponsesWS["active_lease_backend"] = "redis_degraded_fail_open"
		return data, ready
	}

	data.Redis = "unavailable"
	ready = false
	return data, ready
}

func databaseReady(ctx context.Context) bool {
	if model.DB == nil {
		return false
	}
	sqlDB, err := model.DB.DB()
	if err != nil {
		return false
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return sqlDB.PingContext(pingCtx) == nil
}

func redisReady(ctx context.Context) bool {
	client := commonredis.GetRedisClient()
	if client == nil {
		return false
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return client.Ping(pingCtx).Err() == nil
}
