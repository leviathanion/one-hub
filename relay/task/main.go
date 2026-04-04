package task

import (
	"errors"
	"fmt"
	"net/http"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/metrics"
	"one-api/model"
	"one-api/relay/relay_util"
	"one-api/relay/task/base"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var getTaskAdaptorFunc = GetTaskAdaptor
var persistPendingTaskSnapshotFunc = func(task *model.Task) error {
	return task.Update()
}
var persistAcceptedTaskFunc = func(task *model.Task) error {
	return task.Update()
}
var persistAcceptedTaskFallbackFunc = func(task *model.Task) error {
	return task.UpdateFields(map[string]any{
		"properties": task.Properties,
		"status":     task.Status,
		"updated_at": time.Now().Unix(),
	})
}

func RelayTaskSubmit(c *gin.Context) {
	var taskErr *base.TaskError
	taskAdaptor, err := getTaskAdaptorFunc(GetRelayMode(c), c)
	if err != nil {
		taskErr = base.StringTaskError(http.StatusBadRequest, "adaptor_not_found", "adaptor not found", true)
		c.JSON(http.StatusBadRequest, taskErr)
		return
	}

	taskErr = taskAdaptor.Init()
	if taskErr != nil {
		taskAdaptor.HandleError(taskErr)
		return
	}

	taskErr = taskAdaptor.SetProvider()
	if taskErr != nil {
		taskAdaptor.HandleError(taskErr)
		return
	}

	quotaInstance, taskErr := prepareTaskAttemptQuota(c, taskAdaptor)
	if taskErr != nil {
		taskAdaptor.HandleError(taskErr)
		return
	}

	taskErr = taskAdaptor.Relay()
	if taskErr == nil {
		if err = CompletedTask(quotaInstance, taskAdaptor); err != nil {
			taskAdaptor.HandleError(base.StringTaskError(
				http.StatusInternalServerError,
				"task_persist_failed",
				"task accepted by provider but local persistence failed",
				true,
			))
			return
		}
		// 返回结果
		taskAdaptor.GinResponse()
		metrics.RecordProvider(c, 200)
		return
	}

	discardPreparedTaskAttempt(taskAdaptor.GetTask(), taskErr.Message)
	quotaInstance.Undo(c)

	retryTimes := config.RetryTimes

	if !taskAdaptor.ShouldRetry(c, taskErr) {
		logger.LogError(c.Request.Context(), fmt.Sprintf("relay error happen, status code is %d, won't retry in this case", taskErr.StatusCode))
		retryTimes = 0
	}

	channel := taskAdaptor.GetProvider().GetChannel()
	for i := retryTimes; i > 0; i-- {
		model.ChannelGroup.SetCooldowns(channel.Id, taskAdaptor.GetModelName())
		taskErr = taskAdaptor.SetProvider()
		if taskErr != nil {
			continue
		}
		quotaInstance, taskErr = prepareTaskAttemptQuota(c, taskAdaptor)
		if taskErr != nil {
			break
		}

		channel = taskAdaptor.GetProvider().GetChannel()
		logger.LogError(c.Request.Context(), fmt.Sprintf("using channel #%d(%s) to retry (remain times %d)", channel.Id, channel.Name, i))

		taskErr = taskAdaptor.Relay()
		if taskErr == nil {
			if err = CompletedTask(quotaInstance, taskAdaptor); err != nil {
				taskAdaptor.HandleError(base.StringTaskError(
					http.StatusInternalServerError,
					"task_persist_failed",
					"task accepted by provider but local persistence failed",
					true,
				))
				return
			}
			taskAdaptor.GinResponse()
			metrics.RecordProvider(c, 200)
			return
		}

		discardPreparedTaskAttempt(taskAdaptor.GetTask(), taskErr.Message)
		quotaInstance.Undo(c)
		if !taskAdaptor.ShouldRetry(c, taskErr) {
			break
		}

	}

	if taskErr != nil {
		taskAdaptor.HandleError(taskErr)
	}

}

func prepareTaskAttemptQuota(c *gin.Context, taskAdaptor base.TaskInterface) (*relay_util.Quota, *base.TaskError) {
	quotaInstance := relay_util.NewQuota(c, taskAdaptor.GetModelName(), 1000)
	quotaInstance.ForcePreConsume()
	if errWithOA := quotaInstance.PreQuotaConsumption(); errWithOA != nil {
		return nil, base.OpenAIErrToTaskErr(errWithOA)
	}
	if err := preparePendingTaskAttempt(quotaInstance, taskAdaptor); err != nil {
		discardPreparedTaskAttempt(taskAdaptor.GetTask(), err.Error())
		quotaInstance.Undo(c)
		return nil, base.StringTaskError(http.StatusInternalServerError, "task_prepare_failed", "prepare local task snapshot failed", true)
	}
	return quotaInstance, nil
}

func preparePendingTaskAttempt(quotaInstance *relay_util.Quota, taskAdaptor base.TaskInterface) error {
	task := taskAdaptor.GetTask()
	if task == nil {
		return errors.New("task is nil")
	}
	if provider := taskAdaptor.GetProvider(); provider != nil && provider.GetChannel() != nil {
		task.ChannelId = provider.GetChannel().Id
	}
	if task.ChannelId <= 0 {
		return errors.New("task channel_id is empty")
	}
	task.TaskID = ""
	task.Status = model.TaskStatusSubmitted
	task.Progress = 0
	task.FailReason = ""
	task.SubmitTime = time.Now().Unix()

	if task.ID == 0 {
		if err := task.Insert(); err != nil {
			logger.SysError(fmt.Sprintf("insert pending task placeholder error: platform=%s channel_id=%d err=%s", task.Platform, task.ChannelId, err.Error()))
			return err
		}
	}

	properties, finalQuota, err := base.BuildTaskSettlementSnapshotProperties(quotaInstance, task)
	if err != nil {
		logger.SysError(fmt.Sprintf("build task settlement snapshot error: task_id=%s platform=%s local_task_id=%d err=%s", task.TaskID, task.Platform, task.ID, err.Error()))
		return err
	}
	task.Properties = properties
	task.Quota = finalQuota

	if err := persistPendingTaskSnapshotFunc(task); err != nil {
		logger.SysError(fmt.Sprintf("persist pending task snapshot error: task_id=%s platform=%s local_task_id=%d err=%s", task.TaskID, task.Platform, task.ID, err.Error()))
		return err
	}
	return nil
}

func CompletedTask(_ *relay_util.Quota, taskAdaptor base.TaskInterface) error {
	task := taskAdaptor.GetTask()
	if task == nil {
		return errors.New("task is nil")
	}
	if task.ID == 0 {
		return errors.New("task local id is empty")
	}
	if task.ChannelId <= 0 {
		return errors.New("task channel_id is empty")
	}
	properties, err := base.MarkTaskProviderAccepted(task, task.TaskID)
	if err != nil {
		return err
	}
	task.Properties = properties
	task.Status = model.TaskStatusSubmitted
	if err := persistAcceptedTaskFunc(task); err != nil {
		logger.SysError(fmt.Sprintf("persist accepted task error: task_id=%s platform=%s local_task_id=%d err=%s", task.TaskID, task.Platform, task.ID, err.Error()))
		providerTaskID := task.TaskID
		task.TaskID = ""
		if fallbackErr := persistAcceptedTaskFallbackFunc(task); fallbackErr != nil {
			logger.SysError(fmt.Sprintf("persist accepted task fallback error: task_id=%s platform=%s local_task_id=%d err=%s", providerTaskID, task.Platform, task.ID, fallbackErr.Error()))
			task.TaskID = providerTaskID
			return fallbackErr
		}
		task.TaskID = providerTaskID
	}
	ActivateUpdateTaskBulk()
	return nil
}

func discardPreparedTaskAttempt(task *model.Task, reason string) {
	if task == nil || task.ID == 0 {
		return
	}

	if err := task.Delete(); err == nil {
		task.ID = 0
		task.TaskID = ""
		task.Properties = nil
		task.Quota = 0
		task.FailReason = ""
		task.Status = model.TaskStatusNotStart
		task.Progress = 0
		return
	}

	task.TaskID = ""
	task.Properties = nil
	task.Quota = 0
	task.FailReason = reason
	task.Status = model.TaskStatusFailure
	task.Progress = 100
	if err := task.Update(); err != nil {
		logger.SysError(fmt.Sprintf("cleanup pending task placeholder error: platform=%s local_task_id=%d err=%s", task.Platform, task.ID, err.Error()))
	}
	task.ID = 0
}

func GetRelayMode(c *gin.Context) int {
	relayMode := config.RelayModeUnknown
	path := c.Request.URL.Path
	if strings.HasPrefix(path, "/suno") {
		relayMode = config.RelayModeSuno
	} else if strings.HasPrefix(path, "/kling") {
		relayMode = config.RelayModeKling
	}

	return relayMode
}
