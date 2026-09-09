package kling

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/logger"
	"one-api/model"
	"one-api/providers"
	KlingProvider "one-api/providers/kling"
	"one-api/relay/task/base"
	"one-api/types"
	"sort"
	"strings"
	"time"

	"github.com/samber/lo"
)

type KlingTask struct {
	base.TaskBase
	TaskId   string
	Request  *KlingProvider.KlingTask
	Provider *KlingProvider.KlingProvider

	Class  string
	Action string
}

const (
	klingTaskPollDeadline          = 30 * time.Second
	klingTaskChannelLookupDeadline = 5 * time.Second
	klingTaskOwnerMutationDeadline = 5 * time.Second
)

func (t *KlingTask) HandleError(err *base.TaskError) {
	StringError(t.C, err.StatusCode, err.Code, err.Message)
}

func (t *KlingTask) Init() *base.TaskError {
	// 解析
	if err := common.UnmarshalBodyReusable(t.C, &t.Request); err != nil {
		return base.StringTaskError(http.StatusBadRequest, "invalid_request", err.Error(), true)
	}
	if t.Request == nil {
		return base.StringTaskError(http.StatusBadRequest, "invalid_request", "request body must be an object", true)
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

func (t *KlingTask) SetProvider() *base.TaskError {
	// 开始通过模型查询渠道
	provider, err := t.GetProviderByModel()
	if err != nil {
		return base.StringTaskError(http.StatusServiceUnavailable, "provider_not_found", err.Error(), true)
	}

	KlingProvider, ok := provider.(*KlingProvider.KlingProvider)
	if !ok {
		return base.StringTaskError(http.StatusServiceUnavailable, "provider_not_found", "provider not found", true)
	}

	t.Provider = KlingProvider
	t.BaseProvider = provider

	return nil
}

func (t *KlingTask) Relay() *base.TaskError {
	resp, err := t.Provider.Submit(t.C.Request.Context(), t.Class, t.Action, t.Request)
	if err != nil {
		return base.OpenAIErrToTaskErr(err)
	}
	if resp.Code != 0 {
		return base.RejectedTaskError(http.StatusBadGateway, "submit_rejected", resp.Message)
	}
	taskID := strings.TrimSpace(resp.Data.TaskID)
	if taskID == "" {
		return base.StringTaskError(http.StatusInternalServerError, "submit_failed", "provider submit response missing task id", false)
	}

	t.Response = resp
	model.SetTaskProviderID(t.Task, taskID)
	t.Task.ChannelId = t.Provider.Channel.Id

	return nil
}

func (t *KlingTask) actionValidate() (err error) {
	class := t.C.Param("class")
	action := t.C.Param("action")

	if class != "videos" {
		err = fmt.Errorf("class is not videos")
		return
	}

	if action != "text2video" && action != "image2video" {
		err = fmt.Errorf("action is not text2video or image2video")
		return
	}

	if t.Request.Duration == "" {
		t.Request.Duration = "5"
	}

	if t.Request.Duration != "5" && t.Request.Duration != "10" {
		err = fmt.Errorf("duration is not 5 or 10")
		return
	}

	t.Class = class
	t.Action = action

	if t.Request.ModelName == "" {
		t.Request.ModelName = "kling-v1"
	}

	if t.Request.Mode == "" {
		t.Request.Mode = "std"
	}

	t.OriginalModel = fmt.Sprintf("kling-video_%s_%s_%s", t.Request.ModelName, t.Request.Mode, t.Request.Duration)

	return
}

func (t *KlingTask) UpdateTaskStatus(ctx context.Context, taskChannelM map[int][]string, taskM map[string]*model.Task) error {
	for channelId, taskIds := range taskChannelM {
		err := updateKlingTaskAll(ctx, channelId, taskIds, taskM)
		if err != nil {
			logger.LogError(ctx, fmt.Sprintf("渠道 #%d 更新异步任务失败: %s", channelId, err.Error()))
		}
	}
	return nil
}

func updateKlingTaskAll(ctx context.Context, channelId int, taskIds []string, taskM map[string]*model.Task) error {
	if ctx == nil {
		ctx = context.Background()
	}
	logger.LogWarn(ctx, fmt.Sprintf("渠道 #%d 未完成的任务有: %d", channelId, len(taskIds)))
	if len(taskIds) == 0 {
		return nil
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	channelCtx, cancelChannel := klingTaskChannelContext(ctx)
	channel, err := model.GetChannelIncarnationByID(channelCtx, channelId)
	cancelChannel()
	if err != nil {
		return fmt.Errorf("load owner channel incarnation %d: %w", channelId, err)
	}
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		return fmt.Errorf("invalid owner channel incarnation %d: %w", channelId, err)
	}

	providers := providers.GetProvider(channel, nil)
	KlingProvider, ok := providers.(*KlingProvider.KlingProvider)
	if !ok {
		return fmt.Errorf("provider not found")
	}

	for _, providerTaskID := range taskIds {
		if err := ctx.Err(); err != nil {
			return err
		}
		task := taskM[providerTaskID]
		if task == nil {
			continue
		}
		queryCtx, cancelQuery := klingTaskQueryContext(ctx)
		resp, errWithCode := KlingProvider.GetFetch(queryCtx, "videos", task.Action, providerTaskID)
		cancelQuery()
		if errWithCode != nil {
			logger.SysError(fmt.Sprintf("Get Task %s Do req error: %v", providerTaskID, errWithCode))
			rescheduleKlingTaskPoll(ctx, task, 30*time.Second)
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}

		if resp == nil || !resp.IsSuccess() || resp.Data == nil {
			message := "empty provider response"
			if resp != nil {
				message = resp.Message
			}
			logger.SysError(fmt.Sprintf("Get Task %s Fetch error: %v", providerTaskID, message))
			rescheduleKlingTaskPoll(ctx, task, 30*time.Second)
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}

		responseItem := resp.Data
		if responseItem.TaskID != "" && responseItem.TaskID != providerTaskID {
			logger.SysError(fmt.Sprintf("Get Task %s returned mismatched provider id %s", providerTaskID, responseItem.TaskID))
			rescheduleKlingTaskPoll(ctx, task, 30*time.Second)
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}
		responseItem.TaskID = providerTaskID
		if !checkTaskNeedUpdate(task, responseItem) {
			rescheduleKlingTaskPoll(ctx, task, model.TaskPollInterval)
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}
		model.SetTaskProviderID(task, responseItem.TaskID)

		task.Status = lo.If(model.TaskStatus(responseItem.Status) != "", model.TaskStatus(responseItem.Status)).Else(task.Status)
		task.FailReason = lo.If(responseItem.FailReason != "", responseItem.FailReason).Else(task.FailReason)
		task.SubmitTime = lo.If(responseItem.SubmitTime != 0, responseItem.SubmitTime).Else(task.SubmitTime)
		task.StartTime = lo.If(responseItem.StartTime != 0, responseItem.StartTime).Else(task.StartTime)
		task.FinishTime = lo.If(responseItem.FinishTime != 0, responseItem.FinishTime).Else(task.FinishTime)
		task.Data = responseItem.Data

		if task.Status == model.TaskStatusFailure {
			logger.LogError(ctx, model.TaskProviderID(task)+" 构建失败，"+task.FailReason)
			_, settleErr := finalizeKlingTaskSettlement(ctx, task)
			if settleErr != nil {
				logger.LogError(ctx, "finalize failed task settlement: "+settleErr.Error())
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}

		if responseItem.Status == model.TaskStatusSuccess {
			_, settleErr := finalizeKlingTaskSettlement(ctx, task)
			if settleErr != nil {
				logger.LogError(ctx, "finalize success task settlement: "+settleErr.Error())
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}

		if err := saveKlingTaskPollSnapshot(ctx, task, time.Now().Add(model.TaskPollInterval)); err != nil {
			logger.SysError("save Kling task poll snapshot: " + err.Error())
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	return nil
}

func klingTaskQueryContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, klingTaskPollDeadline)
}

func klingTaskChannelContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, klingTaskChannelLookupDeadline)
}

func klingTaskMutationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), klingTaskOwnerMutationDeadline)
}

func rescheduleKlingTaskPoll(parent context.Context, task *model.Task, delay time.Duration) {
	mutationCtx, cancel := klingTaskMutationContext(parent)
	err := base.RescheduleTaskPoll(mutationCtx, task, delay)
	cancel()
	if err != nil {
		logger.SysError(fmt.Sprintf("reschedule Kling task poll: %v", err))
	}
}

func finalizeKlingTaskSettlement(parent context.Context, task *model.Task) (base.TaskSettlementFinalizeResult, error) {
	mutationCtx, cancel := klingTaskMutationContext(parent)
	result, err := base.FinalizeTaskSettlement(mutationCtx, task)
	cancel()
	return result, err
}

func saveKlingTaskPollSnapshot(parent context.Context, task *model.Task, next time.Time) error {
	mutationCtx, cancel := klingTaskMutationContext(parent)
	_, err := model.SaveTaskPollSnapshot(mutationCtx, task, next)
	cancel()
	return err
}

func checkTaskNeedUpdate(oldTask *model.Task, newTask *types.TaskDto) bool {
	if oldTask == nil || newTask == nil {
		return false
	}
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
