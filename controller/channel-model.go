package controller

import (
	"net/http"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"

	"github.com/gin-gonic/gin"
)

func GetModelList(c *gin.Context) {
	channel := &model.Channel{}
	err := c.ShouldBindJSON(channel)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	keys := providers.ChannelCredentialInputs(channel)
	channel.Key = keys[0]
	explicitCredential := channel.Key != ""

	if channel.Key == "" {
		if channel.Id == 0 {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "key is required",
			})
			return
		}

		channel, err = model.GetChannelById(channel.Id)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	}

	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	if err := providers.PrepareChannelCredentialInput(channel); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}
	if explicitCredential && channel.Id > 0 && providers.UsesWholeCredentialInput(channel) {
		persisted, err := model.GetChannelByIdWithContext(c.Request.Context(), channel.Id)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
			return
		}
		if err := providers.PrepareChannelCredentialInput(persisted); err != nil || persisted.Type != channel.Type || persisted.Key != channel.Key {
			// 未保存的凭据不能借已有 ID 刷新或覆盖数据库；相同凭据仍沿用持久化 fence。
			channel.Id = 0
		}
	}

	provider := providers.GetProvider(channel, c)
	if provider == nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "provider not found",
		})
		return
	}

	modelProvider, ok := provider.(providersBase.ModelListInterface)
	if !ok {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "channel not implemented",
		})
		return
	}

	modelList, err := modelProvider.GetModelList()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	// 去除重复的模型名称
	uniqueModels := removeDuplicates(modelList)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    uniqueModels,
	})
}

// 辅助函数：去除切片中的重复元素
func removeDuplicates(slice []string) []string {
	keys := make(map[string]bool)
	list := []string{}
	for _, entry := range slice {
		if _, value := keys[entry]; !value {
			keys[entry] = true
			list = append(list, entry)
		}
	}
	return list
}
