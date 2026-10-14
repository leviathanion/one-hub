package controller

import (
	"errors"
	"net/http"
	"one-api/common"
	"one-api/common/providerendpoint"
	"one-api/common/utils"
	"one-api/model"
	"one-api/providers"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

func GetChannelsList(c *gin.Context) {
	var params model.SearchChannelsParams
	if err := c.ShouldBindQuery(&params); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	channels, err := model.GetChannelsList(&params)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    channels,
	})
}

func GetChannel(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	channel, err := model.GetChannelById(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    channel,
	})
}

func AddChannel(c *gin.Context) {
	channel := model.Channel{}
	err := c.ShouldBindJSON(&channel)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	tagExists, err := model.ChannelTagExists(channel.Tag)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	if tagExists {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "标签已存在，请使用标签的新增渠道入口",
		})
		return
	}
	channel.CreatedTime = utils.GetTimestamp()
	channels := buildChannelsForCreate(channel)
	if len(channels) == 0 {
		if err := providers.PrepareChannelCredentialInput(&channel); err != nil {
			common.APIRespondWithError(c, http.StatusOK, err)
			return
		}
	}
	for i := range channels {
		if err := providers.PrepareChannelCredentialInput(&channels[i]); err != nil {
			common.APIRespondWithError(c, http.StatusOK, err)
			return
		}
	}
	err = model.BatchInsertChannels(channels)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

func buildChannelsForCreate(channel model.Channel) []model.Channel {
	keys := splitChannelKeysForCreate(channel)
	baseUrls := splitBaseURLsForCreate(channel)
	channels := make([]model.Channel, 0, len(keys))

	for index, key := range keys {
		if key == "" {
			continue
		}
		localChannel := channel
		localChannel.Key = key
		if index > 0 {
			localChannel.Name = localChannel.Name + "_" + strconv.Itoa(index+1)
		}

		if len(baseUrls) > index && baseUrls[index] != "" {
			localChannel.BaseURL = &baseUrls[index]
		} else if len(baseUrls) > 0 {
			localChannel.BaseURL = &baseUrls[0]
		}

		channels = append(channels, localChannel)
	}

	return channels
}

func splitChannelKeysForCreate(channel model.Channel) []string {
	return providers.ChannelCredentialInputs(&channel)
}

func splitBaseURLsForCreate(channel model.Channel) []string {
	if channel.BaseURL == nil || *channel.BaseURL == "" {
		return nil
	}
	return strings.Split(*channel.BaseURL, "\n")
}

func DeleteChannel(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	channel := model.Channel{Id: id}
	err := channel.Delete()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

func DeleteChannelTag(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	err := model.DeleteChannelTag(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

func DeleteDisabledChannel(c *gin.Context) {
	rows, err := model.DeleteDisabledChannel()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    rows,
	})
}

func UpdateChannel(c *gin.Context) {
	var request model.ChannelEditRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	if err := prepareEditedChannelCredential(c, &request); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	err := request.Update()
	if err != nil {
		status := http.StatusOK
		if errors.Is(err, model.ErrChannelVersionConflict) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    request.Channel,
	})
}

func prepareEditedChannelCredential(c *gin.Context, request *model.ChannelEditRequest) error {
	_, keySubmitted := request.SubmittedFields["key"]
	_, typeSubmitted := request.SubmittedFields["type"]
	if !keySubmitted && !typeSubmitted {
		return nil
	}
	persisted, err := model.GetChannelByIdWithContext(c.Request.Context(), request.Id)
	if err != nil {
		return err
	}
	if !keySubmitted && (!typeSubmitted || request.Type == persisted.Type) {
		return nil
	}
	if typeSubmitted {
		persisted.Type = request.Type
	}
	if keySubmitted {
		persisted.Key = request.Key
	}
	if err := providers.PrepareChannelCredentialInput(persisted); err != nil {
		return err
	}
	if keySubmitted {
		request.Key = persisted.Key
	}
	return nil
}

func BatchUpdateChannelsAzureApi(c *gin.Context) {
	var params model.BatchChannelsParams
	err := c.ShouldBindJSON(&params)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	if params.Ids == nil || len(params.Ids) == 0 {
		common.APIRespondWithError(c, http.StatusOK, errors.New("ids不能为空"))
		return
	}
	var count int64
	count, err = model.BatchUpdateChannelsAzureApi(&params)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data":    count,
		"success": true,
		"message": "更新成功",
	})
}

func BatchDelModelChannels(c *gin.Context) {
	var params model.BatchDelModelChannelsParams
	err := c.ShouldBindJSON(&params)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	if params.Ids == nil || len(params.Ids) == 0 {
		common.APIRespondWithError(c, http.StatusOK, errors.New("ids不能为空"))
		return
	}

	var count int64
	count, err = model.BatchDelModelChannelsWithContext(c.Request.Context(), &params)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data":    count,
		"success": true,
		"message": "更新成功",
	})
}

func BatchDeleteChannel(c *gin.Context) {
	var params model.BatchChannelsParams
	err := c.ShouldBindJSON(&params)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	if params.Ids == nil || len(params.Ids) == 0 {
		common.APIRespondWithError(c, http.StatusOK, errors.New("ids不能为空"))
		return
	}

	count, err := model.BatchDeleteChannel(params.Ids)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    count,
	})
}

func GetChannelEndpoints(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": providerendpoint.Definitions()})
}
