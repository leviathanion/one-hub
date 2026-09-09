package controller

import (
	"net/http"

	"one-api/common"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

func GetAllMidjourney(c *gin.Context) {
	var params model.MJTaskQueryParams
	if err := c.ShouldBindQuery(&params); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	midjourneys, err := model.GetAllMJTasks(&params)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    midjourneys,
	})
}

func GetUserMidjourney(c *gin.Context) {
	userID := c.GetInt("id")
	tokenID := c.GetInt("token_id")
	var params model.MJTaskQueryParams
	if err := c.ShouldBindQuery(&params); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	if tokenID > 0 {
		params.TokenID = tokenID
	}

	midjourneys, err := model.GetAllUserMJTask(userID, &params)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    midjourneys,
	})
}
