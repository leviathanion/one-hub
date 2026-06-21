package controller

import (
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

func GetSubscription(c *gin.Context) {
	var remainQuota int
	var usedQuota int
	var err error
	var token *model.Token
	var expiredTime int64

	tokenId := c.GetInt("token_id")
	token, err = model.GetTokenById(tokenId)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("获取信息失败: %v", err))
		return
	}

	if token.UnlimitedQuota {
		userId := c.GetInt("id")
		remainQuota, usedQuota, err = model.GetUserQuotaFields(userId)
		if err != nil {
			common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("获取用户信息失败: %v", err))

			return
		}
	} else {
		expiredTime = token.ExpiredTime
		remainQuota = token.RemainQuota
		usedQuota = token.UsedQuota
	}

	if expiredTime <= 0 {
		expiredTime = 0
	}

	quota := remainQuota + usedQuota
	amount := float64(quota)
	if config.DisplayInCurrencyEnabled {
		amount /= config.QuotaPerUnit
	}

	subscription := OpenAISubscriptionResponse{
		Object:             "billing_subscription",
		HasPaymentMethod:   true,
		SoftLimitUSD:       amount,
		HardLimitUSD:       amount,
		SystemHardLimitUSD: amount,
		AccessUntil:        expiredTime,
	}
	c.JSON(200, subscription)
}

func GetUsage(c *gin.Context) {
	var quota int
	var err error
	var token *model.Token

	tokenId := c.GetInt("token_id")
	token, err = model.GetTokenById(tokenId)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("获取信息失败: %v", err))
		return
	}

	if token.UnlimitedQuota {
		userId := c.GetInt("id")
		_, usedQuota, err := model.GetUserQuotaFields(userId)
		if err != nil {
			common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("获取用户信息失败: %v", err))

			return
		}
		quota = usedQuota
	} else {
		quota = token.UsedQuota
	}

	amount := float64(quota)
	if config.DisplayInCurrencyEnabled {
		amount /= config.QuotaPerUnit
	}
	usage := OpenAIUsageResponse{
		Object:     "list",
		TotalUsage: amount * 100,
	}
	c.JSON(200, usage)
}
