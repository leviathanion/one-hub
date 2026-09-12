package suno

import (
	"fmt"
	"one-api/common/providerresponse"
	"one-api/common/surface"
	"one-api/model"
	taskbase "one-api/relay/task/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func StringError(c *gin.Context, httpCode int, code, message string) {
	surfaceErr := surface.NewLocalError(httpCode, message, code)
	surface.LogLocalError(c, surfaceErr)
	surface.TaskContract().RenderJSONError(c, surfaceErr)
}

func TaskModel2Dto(task *model.Task) *types.TaskDto {
	progress := fmt.Sprintf("%d%%", task.Progress)

	taskDto := &types.TaskDto{
		TaskID:     taskbase.TaskTrackingHandle(task),
		Action:     task.Action,
		Status:     string(task.Status),
		FailReason: providerresponse.SanitizeErrorText(task.FailReason),
		SubmitTime: task.SubmitTime,
		StartTime:  task.StartTime,
		FinishTime: task.FinishTime,
		Progress:   progress,
		Data:       task.Data,
	}

	return taskDto
}
