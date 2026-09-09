package controller

import (
	"net/http"
	"one-api/common"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

func GetGroups(c *gin.Context) {
	groupNames := make([]string, 0)

	userGroup := model.GlobalUserGroupRatio.GetAll()

	for symbol := range userGroup {
		groupNames = append(groupNames, symbol)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    groupNames,
	})
}

func GetUserGroupRatio(c *gin.Context) {
	userId := c.GetInt("id")
	userSymbol := ""

	if userId > 0 {
		user, err := model.GetUserRoutingState(c.Request.Context(), userId)
		if err != nil {
			common.APIRespondWithError(c, http.StatusServiceUnavailable, err)
			return
		}
		userSymbol = user.Group
	}
	if err := model.EnsureUserGroupPolicyAvailable(c.Request.Context()); err != nil {
		common.APIRespondWithError(c, http.StatusServiceUnavailable, err)
		return
	}

	groupRatio := model.GlobalUserGroupRatio.GetAll()
	UserGroup := make(map[string]*model.UserGroup)
	for k, v := range groupRatio {
		if v.Public || k == userSymbol {
			UserGroup[k] = v
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    UserGroup,
	})
}
