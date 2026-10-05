package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

func GetChannelsTagList(c *gin.Context) {
	tag := c.Param("tag")
	if tag == "" {
		common.APIRespondWithError(c, http.StatusOK, errors.New("tag is required"))
		return
	}

	channelsTag, err := model.GetChannelsTagList(tag)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    channelsTag,
	})
}

func GetChannelsTagAllList(c *gin.Context) {
	channelTags, err := model.GetChannelsTagAllList()
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    channelTags,
	})
}

func GetChannelsTag(c *gin.Context) {
	tag := c.Param("tag")
	if tag == "" {
		common.AbortWithMessage(c, http.StatusOK, "tag is required")
		return
	}
	channel, err := model.GetChannelsTag(tag)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    channel,
	})
}

func UpdateChannelsTag(c *gin.Context) {
	tag := c.Param("tag")
	if tag == "" {
		common.AbortWithMessage(c, http.StatusOK, "tag is required")
		return
	}
	rawBody, err := c.GetRawData()
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	submittedFields, err := model.ParseChannelTagSubmittedFields(rawBody)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	var request struct {
		model.Channel
		ExpectedVersions map[int]uint64 `json:"expected_versions"`
	}
	if err := json.Unmarshal(rawBody, &request); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	if request.ExpectedVersions == nil {
		common.APIRespondWithError(c, http.StatusOK, errors.New("expected_versions is required"))
		return
	}
	if _, ok := submittedFields["internal_state"]; ok {
		common.APIRespondWithError(c, http.StatusOK, errors.New("internal_state is server-owned"))
		return
	}
	err = model.UpdateChannelsTagWithSubmittedFields(tag, &request.Channel, submittedFields, model.ChannelUpdateOptions{AllowIdentityChange: true, ExpectedVersions: request.ExpectedVersions})
	if err != nil {
		status := http.StatusOK
		if errors.Is(err, model.ErrChannelVersionConflict) {
			status = http.StatusConflict
		}
		common.APIRespondWithError(c, status, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

func AddChannelToTag(c *gin.Context) {
	tag := c.Param("tag")
	if tag == "" {
		common.AbortWithMessage(c, http.StatusOK, "tag is required")
		return
	}

	channel := model.Channel{}
	if err := c.ShouldBindJSON(&channel); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	addedChannel, err := model.AddChannelToTag(tag, &channel)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    addedChannel,
	})
}

func DeleteChannelsTag(c *gin.Context) {
	tag := c.Param("tag")
	if tag == "" {
		common.AbortWithMessage(c, http.StatusOK, "tag is required")
		return
	}
	err := model.DeleteChannelsTag(tag, false)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

func DeleteDisabledChannelsTag(c *gin.Context) {
	tag := c.Param("tag")
	if tag == "" {
		common.AbortWithMessage(c, http.StatusOK, "tag is required")
		return
	}
	err := model.DeleteChannelsTag(tag, true)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
}

type UpdateChannelsTagParams struct {
	Type  string `json:"type"`
	Value int    `json:"value"`
}

func UpdateChannelsTagPriority(c *gin.Context) {
	tag := c.Param("tag")
	if tag == "" {
		common.AbortWithMessage(c, http.StatusOK, "tag is required")
		return
	}

	var params UpdateChannelsTagParams
	err := c.ShouldBindJSON(&params)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	switch params.Type {
	case "priority":
		err = model.UpdateChannelsTagPriorityWithContext(c.Request.Context(), tag, params.Value)
		if err != nil {
			common.APIRespondWithError(c, http.StatusOK, err)
			return
		}
	default:
		common.AbortWithMessage(c, http.StatusOK, "invalid type")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

func ChangeChannelsTagStatus(c *gin.Context) {
	tag := c.Param("tag")
	status := c.Param("status")
	if tag == "" {
		common.AbortWithMessage(c, http.StatusOK, "tag is required")
		return
	}

	var statusInt int
	switch status {
	case "enable":
		statusInt = config.TokenStatusEnabled
	case "disable":
		statusInt = config.TokenStatusDisabled
	}

	if statusInt == 0 {
		common.AbortWithMessage(c, http.StatusOK, "invalid status")
		return
	}

	err := model.ChangeChannelsTagStatusWithContext(c.Request.Context(), tag, statusInt)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}
