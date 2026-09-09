package controller

import (
	"context"
	"net/http"
	"time"

	"one-api/common/config"
	"one-api/middleware"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

type readinessData struct {
	Database    string                 `json:"database"`
	Redis       string                 `json:"redis"`
	Pricing     string                 `json:"pricing"`
	Options     string                 `json:"options"`
	UserGroups  userGroupReadiness     `json:"user_groups"`
	ResponsesWS map[string]interface{} `json:"responses_ws"`
}

type userGroupReadiness struct {
	State string `json:"state"`
	model.UserGroupPublicationStatus
}

var probeResponsesWSActiveLeaseBackend = middleware.ProbeResponsesWSActiveLeaseBackend

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
		Pricing:  "ok",
		Options:  "ok",
		ResponsesWS: map[string]interface{}{
			"active_lease_backend": "local",
		},
	}
	addResponsesWSLivenessReadiness(&data)
	ready := true
	groupErr := model.EnsureUserGroupPolicyAvailable(ctx)
	data.UserGroups = userGroupReadiness{State: "ok", UserGroupPublicationStatus: model.GlobalUserGroupRatio.PublicationStatus()}
	if groupErr != nil {
		data.UserGroups.State = "unavailable"
		ready = false
	}
	optionProbeCtx, optionCancel := context.WithTimeout(ctx, 2*time.Second)
	optionErr := model.CheckOptionsPublication(optionProbeCtx)
	optionCancel()
	if optionErr != nil {
		data.Options = "unavailable"
		ready = false
	}

	if !databaseReady(ctx) {
		data.Database = "unavailable"
		ready = false
	}
	if requiredModels, err := config.PricingRequiredModels(); err != nil {
		data.Pricing = "invalid_required_models"
		ready = false
	} else {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = model.CheckPricePublication(probeCtx, requiredModels)
		cancel()
		if err != nil {
			data.Pricing = "unavailable"
			ready = false
		} else if model.PricingInstance.IsDegraded() {
			data.Pricing = "degraded"
		}
	}

	if !config.RedisEnabled {
		return data, ready
	}

	data.Redis = "ok"
	data.ResponsesWS["active_lease_backend"] = "redis"
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	probeErr := probeResponsesWSActiveLeaseBackend(probeCtx)
	cancel()
	if probeErr == nil {
		data.ResponsesWS["active_lease_probe"] = "ok"
		return data, ready
	}
	data.ResponsesWS["active_lease_probe"] = "unavailable"

	if config.ResponsesWSActiveLeaseRedisFailOpen() {
		data.Redis = "degraded_fail_open"
		data.ResponsesWS["active_lease_backend"] = "redis_degraded_fail_open"
		return data, ready
	}

	data.Redis = "unavailable"
	ready = false
	return data, ready
}

func addResponsesWSLivenessReadiness(data *readinessData) {
	if data == nil || data.ResponsesWS == nil {
		return
	}
	degraded := make([]string, 0, 4)
	if config.ResponsesWebsocketClientPongMissTimeout() <= 0 {
		degraded = append(degraded, "client_pong_miss")
	}
	if config.ResponsesWebsocketClientInboundActivityTimeout() <= 0 {
		degraded = append(degraded, "client_inbound_activity")
	}
	if config.ResponsesWSActiveTurnTimeout() <= 0 {
		degraded = append(degraded, "provider_inactivity")
	}
	if config.ResponsesWSMaxLifetime() <= 0 {
		degraded = append(degraded, "max_lifetime")
	}
	if len(degraded) == 0 {
		data.ResponsesWS["liveness"] = "ok"
		return
	}
	data.ResponsesWS["liveness"] = "degraded"
	data.ResponsesWS["liveness_disabled"] = degraded
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
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return false
	}
	if model.CheckResponseOwnerSchema(pingCtx) != nil {
		return false
	}
	if model.CheckTaskOwnerSchema(pingCtx) != nil {
		return false
	}
	if model.CheckPublicationVersionSchema(pingCtx) != nil {
		return false
	}
	if model.CheckPaymentOrderSchema(model.DB.WithContext(pingCtx)) != nil {
		return false
	}
	return true
}
