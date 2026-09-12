package kling

import (
	"encoding/json"
	"one-api/common/providerresponse"
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
	if data.Code != 0 {
		data.Message = providerresponse.SanitizeErrorText(data.Message)
	}
	if data.Data != nil && data.Data.TaskStatus == "failed" {
		data.Data.TaskStatusMsg = providerresponse.SanitizeErrorText(data.Data.TaskStatusMsg)
	}
	if data.Data != nil && data.Data.TaskID == "" {
		data.Data.TaskID = taskbase.TaskTrackingHandle(task)
	}

	return data
}
