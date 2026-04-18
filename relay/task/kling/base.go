package kling

import (
	"encoding/json"
	"one-api/common/surface"
	"one-api/model"
	taskbase "one-api/relay/task/base"

	KlingProvider "one-api/providers/kling"

	"github.com/gin-gonic/gin"
)

func StringError(c *gin.Context, httpCode int, code, message string) {
	surfaceErr := surface.NewLocalError(httpCode, message, code)
	surface.LogLocalError(c, surfaceErr)
	surface.TaskContract().RenderJSONError(c, surfaceErr)
}

func TaskModel2Dto(task *model.Task) *KlingProvider.KlingResponse[*KlingProvider.KlingTaskData] {
	data := &KlingProvider.KlingResponse[*KlingProvider.KlingTaskData]{}
	json.Unmarshal(task.Data, data)
	if data.Data != nil && data.Data.TaskID == "" {
		data.Data.TaskID = taskbase.TaskTrackingHandle(task)
	}

	return data
}
