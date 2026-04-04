package suno

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/logger"
	"one-api/metrics"
	"one-api/model"
	"one-api/providers"
	sunoProvider "one-api/providers/suno"
	"one-api/relay/task/base"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

type SunoTask struct {
	base.TaskBase
	Action   string
	Request  *sunoProvider.SunoSubmitReq
	Provider *sunoProvider.SunoProvider
}

func (t *SunoTask) HandleError(err *base.TaskError) {
	StringError(t.C, err.StatusCode, err.Code, err.Message)
}

func (t *SunoTask) Init() *base.TaskError {
	t.Action = strings.ToUpper(t.C.Param("action"))

	// 解析
	if err := common.UnmarshalBodyReusable(t.C, &t.Request); err != nil {
		return base.StringTaskError(http.StatusBadRequest, "invalid_request", err.Error(), true)
	}

	err := t.actionValidate()
	if err != nil {
		return base.StringTaskError(http.StatusBadRequest, "invalid_request", err.Error(), true)
	}

	err = t.HandleOriginTaskID()
	if err != nil {
		return base.StringTaskError(http.StatusInternalServerError, "get_origin_task_failed", err.Error(), true)
	}

	t.InitTask()
	if t.Task != nil {
		t.Task.Action = t.Action
	}

	return nil
}

func (t *SunoTask) SetProvider() *base.TaskError {
	// 开始通过模型查询渠道
	provider, err := t.GetProviderByModel()
	if err != nil {
		return base.StringTaskError(http.StatusServiceUnavailable, "provider_not_found", err.Error(), true)
	}

	sunoProvider, ok := provider.(*sunoProvider.SunoProvider)
	if !ok {
		return base.StringTaskError(http.StatusServiceUnavailable, "provider_not_found", "provider not found", true)
	}

	t.Provider = sunoProvider
	t.BaseProvider = provider

	return nil
}

func (t *SunoTask) Relay() *base.TaskError {
	resp, err := t.Provider.Submit(t.Action, t.Request)
	if err != nil {
		return base.OpenAIErrToTaskErr(err)
	}

	if !resp.IsSuccess() {
		return base.StringTaskError(http.StatusInternalServerError, "submit_failed", resp.Message, false)
	}

	if resp.Data == nil || strings.TrimSpace(*resp.Data) == "" {
		return base.StringTaskError(http.StatusInternalServerError, "submit_failed", "provider submit response missing task id", false)
	}

	t.Response = resp
	t.Task.TaskID = strings.TrimSpace(*resp.Data)
	t.Task.ChannelId = t.Provider.Channel.Id

	return nil
}

func (t *SunoTask) actionValidate() (err error) {
	switch t.Action {
	case sunoProvider.SunoActionMusic:
		if t.Request.Mv == "" {
			t.Request.Mv = "chirp-v3-0"
		}
		t.OriginalModel = t.Request.Mv
	case sunoProvider.SunoActionLyrics:
		if t.Request.Prompt == "" {
			err = fmt.Errorf("prompt_empty")
			return
		}
		t.OriginalModel = "suno_lyrics"
	default:
		err = fmt.Errorf("invalid_action")
		return
	}

	if t.Request.ContinueClipId != "" {
		if t.Request.TaskID == "" {
			err = fmt.Errorf("task id is empty")
			return
		}
		t.OriginTaskID = t.Request.TaskID
	}

	return
}

func (t *SunoTask) ShouldRetry(c *gin.Context, err *base.TaskError) bool {
	if err == nil {
		return false
	}

	metrics.RecordProvider(c, err.StatusCode)

	if err.LocalError {
		return false
	}

	if _, ok := t.C.Get("specific_channel_id"); ok {
		return false
	}

	if err.StatusCode == http.StatusTooManyRequests {
		return true
	}

	if err.StatusCode == 307 {
		return true
	}

	if err.StatusCode/100 == 5 {
		// 超时不重试
		if err.StatusCode == 504 || err.StatusCode == 524 {
			return false
		}
		return true
	}

	return true
}

func (t *SunoTask) UpdateTaskStatus(ctx context.Context, taskChannelM map[int][]string, taskM map[string]*model.Task) error {
	for channelId, taskIds := range taskChannelM {
		err := updateSunoTaskAll(ctx, channelId, taskIds, taskM)
		if err != nil {
			logger.LogError(ctx, fmt.Sprintf("渠道 #%d 更新异步任务失败: %s", channelId, err.Error()))
		}
	}
	return nil
}

func updateSunoTaskAll(ctx context.Context, channelId int, taskIds []string, taskM map[string]*model.Task) error {
	logger.LogWarn(ctx, fmt.Sprintf("渠道 #%d 未完成的任务有: %d", channelId, len(taskIds)))
	if len(taskIds) == 0 {
		return nil
	}

	channel := model.ChannelGroup.GetChannel(channelId)
	if channel == nil {
		reason := fmt.Sprintf("获取渠道信息失败，请联系管理员，渠道ID：%d", channelId)
		for _, taskID := range taskIds {
			task := taskM[taskID]
			if task == nil {
				continue
			}
			if err := base.FailTaskWithSettlement(ctx, task, reason); err != nil {
				logger.SysError(fmt.Sprintf("UpdateTask error: %v", err))
			}
		}
		return fmt.Errorf("channel not found")
	}

	providers := providers.GetProvider(channel, nil)
	sunoProvider, ok := providers.(*sunoProvider.SunoProvider)
	if !ok {
		for _, taskID := range taskIds {
			task := taskM[taskID]
			if task == nil {
				continue
			}
			if err := base.FailTaskWithSettlement(ctx, task, "获取供应商失败，请联系管理员"); err != nil {
				logger.SysError(fmt.Sprintf("UpdateTask error: %v", err))
			}
		}
		return fmt.Errorf("provider not found")
	}

	resp, errWithCode := sunoProvider.GetFetchs(taskIds)
	if errWithCode != nil {
		logger.SysError(fmt.Sprintf("Get Task Do req error: %v", errWithCode))
	}

	if !resp.IsSuccess() {
		return fmt.Errorf("渠道 #%d 未完成的任务有: %d, 报错: %s", channelId, len(taskIds), resp.Message)
	}

	for _, responseItem := range *resp.Data {
		task := taskM[responseItem.TaskID]
		if !checkTaskNeedUpdate(task, responseItem) {
			continue
		}
		task.TaskID = responseItem.TaskID

		task.Status = lo.If(model.TaskStatus(responseItem.Status) != "", model.TaskStatus(responseItem.Status)).Else(task.Status)
		task.FailReason = lo.If(responseItem.FailReason != "", responseItem.FailReason).Else(task.FailReason)
		task.SubmitTime = lo.If(responseItem.SubmitTime != 0, responseItem.SubmitTime).Else(task.SubmitTime)
		task.StartTime = lo.If(responseItem.StartTime != 0, responseItem.StartTime).Else(task.StartTime)
		task.FinishTime = lo.If(responseItem.FinishTime != 0, responseItem.FinishTime).Else(task.FinishTime)

		if responseItem.FailReason != "" || task.Status == model.TaskStatusFailure {
			logger.LogError(ctx, task.TaskID+" 构建失败，"+task.FailReason)
			settleResult, settleErr := base.FinalizeTaskSettlement(ctx, task, false)
			if settleErr != nil {
				logger.LogError(ctx, "finalize failed task settlement: "+settleErr.Error())
				continue
			}
			if !settleResult.Handled {
				quota := task.Quota
				if quota > 0 {
					err := model.IncreaseUserQuota(task.UserId, quota)
					if err != nil {
						logger.LogError(ctx, "fail to increase user quota: "+err.Error())
					}
					logContent := fmt.Sprintf("异步任务执行失败 %s，补偿 %s", task.TaskID, common.LogQuota(quota))
					model.RecordLog(task.UserId, model.LogTypeSystem, logContent)
				}
			}
			if !settleResult.PersistTask {
				continue
			}
			task.Progress = 100
		}

		if responseItem.Status == model.TaskStatusSuccess {
			settleResult, settleErr := base.FinalizeTaskSettlement(ctx, task, true)
			if settleErr != nil {
				logger.LogError(ctx, "finalize success task settlement: "+settleErr.Error())
				continue
			}
			if !settleResult.PersistTask {
				continue
			}
			task.Progress = 100
		}

		task.Data = responseItem.Data
		err := task.Update()
		if err != nil {
			logger.SysError("UpdateTask task error: " + err.Error())
		}
	}
	return nil
}

func checkTaskNeedUpdate(oldTask *model.Task, newTask sunoProvider.SunoDataResponse) bool {

	if oldTask.SubmitTime != newTask.SubmitTime {
		return true
	}
	if oldTask.StartTime != newTask.StartTime {
		return true
	}
	if oldTask.FinishTime != newTask.FinishTime {
		return true
	}
	if string(oldTask.Status) != newTask.Status {
		return true
	}
	if oldTask.FailReason != newTask.FailReason {
		return true
	}
	if oldTask.FinishTime != newTask.FinishTime {
		return true
	}

	if (oldTask.Status == model.TaskStatusFailure || oldTask.Status == model.TaskStatusSuccess) && oldTask.Progress != 100 {
		return true
	}

	oldData, _ := json.Marshal(oldTask.Data)
	newData, _ := json.Marshal(newTask.Data)

	sort.Slice(oldData, func(i, j int) bool {
		return oldData[i] < oldData[j]
	})
	sort.Slice(newData, func(i, j int) bool {
		return newData[i] < newData[j]
	})

	return string(oldData) != string(newData)
}
