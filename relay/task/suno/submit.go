package suno

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/logger"
	"one-api/model"
	"one-api/providers"
	sunoProvider "one-api/providers/suno"
	"one-api/relay/task/base"
	"sort"
	"strings"
	"time"

	"github.com/samber/lo"
)

type SunoTask struct {
	base.TaskBase
	Action   string
	Request  *sunoProvider.SunoSubmitReq
	Provider *sunoProvider.SunoProvider
}

var (
	sunoTaskPollDeadline          = 30 * time.Second
	sunoTaskOwnerMutationDeadline = 5 * time.Second
)

func (t *SunoTask) HandleError(err *base.TaskError) {
	StringError(t.C, err.StatusCode, err.Code, err.Message)
}

func (t *SunoTask) Init() *base.TaskError {
	t.Action = strings.ToUpper(t.C.Param("action"))

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
	resp, err := t.Provider.Submit(t.C.Request.Context(), t.Action, t.Request)
	if err != nil {
		return base.OpenAIErrToTaskErr(err)
	}

	if !resp.IsSuccess() {
		return base.RejectedTaskError(http.StatusBadGateway, "submit_rejected", resp.Message)
	}

	if resp.Data == nil || strings.TrimSpace(*resp.Data) == "" {
		return base.StringTaskError(http.StatusInternalServerError, "submit_failed", "provider submit response missing task id", false)
	}

	t.Response = resp
	model.SetTaskProviderID(t.Task, *resp.Data)
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
	queryCtx, cancelQuery := sunoTaskQueryContext(ctx)
	defer cancelQuery()
	channel, err := model.GetChannelIncarnationByID(queryCtx, channelId)
	if err != nil {
		return fmt.Errorf("load owner channel incarnation %d: %w", channelId, err)
	}
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		return fmt.Errorf("invalid owner channel incarnation %d: %w", channelId, err)
	}

	providers := providers.GetProvider(channel, nil)
	sunoProvider, ok := providers.(*sunoProvider.SunoProvider)
	if !ok {
		return fmt.Errorf("provider not found")
	}

	resp, errWithCode := sunoProvider.GetFetchs(queryCtx, taskIds)
	if errWithCode != nil {
		return fmt.Errorf("poll provider tasks: %v", errWithCode)
	}

	if resp == nil || !resp.IsSuccess() || resp.Data == nil {
		message := "empty provider response"
		if resp != nil {
			message = resp.Message
		}
		return fmt.Errorf("渠道 #%d 未完成的任务有: %d, 报错: %s", channelId, len(taskIds), message)
	}

	returned := make(map[string]struct{}, len(*resp.Data))
	for _, responseItem := range *resp.Data {
		task := taskM[responseItem.TaskID]
		if task == nil {
			continue
		}
		returned[responseItem.TaskID] = struct{}{}
		if !checkTaskNeedUpdate(task, responseItem) {
			rescheduleSunoTaskPoll(ctx, task, model.TaskPollInterval)
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
			_, settleErr := finalizeSunoTaskSettlement(ctx, task)
			if settleErr != nil {
				logger.LogError(ctx, "finalize failed task settlement: "+settleErr.Error())
			}
			continue
		}

		if responseItem.Status == model.TaskStatusSuccess {
			_, settleErr := finalizeSunoTaskSettlement(ctx, task)
			if settleErr != nil {
				logger.LogError(ctx, "finalize success task settlement: "+settleErr.Error())
			}
			continue
		}

		if err := saveSunoTaskPollSnapshot(ctx, task, time.Now().Add(model.TaskPollInterval)); err != nil {
			logger.SysError("save Suno task poll snapshot: " + err.Error())
		}
	}
	for _, taskID := range taskIds {
		if _, ok := returned[taskID]; ok {
			continue
		}
		rescheduleSunoTaskPoll(ctx, taskM[taskID], 30*time.Second)
	}
	return nil
}

func sunoTaskQueryContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, sunoTaskPollDeadline)
}

func sunoTaskMutationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), sunoTaskOwnerMutationDeadline)
}

func rescheduleSunoTaskPoll(parent context.Context, task *model.Task, delay time.Duration) {
	mutationCtx, cancel := sunoTaskMutationContext(parent)
	if err := base.RescheduleTaskPoll(mutationCtx, task, delay); err != nil {
		logger.SysError(fmt.Sprintf("reschedule Suno task poll: %v", err))
	}
	cancel()
}

func finalizeSunoTaskSettlement(parent context.Context, task *model.Task) (base.TaskSettlementFinalizeResult, error) {
	mutationCtx, cancel := sunoTaskMutationContext(parent)
	result, err := base.FinalizeTaskSettlement(mutationCtx, task)
	cancel()
	return result, err
}

func saveSunoTaskPollSnapshot(parent context.Context, task *model.Task, next time.Time) error {
	mutationCtx, cancel := sunoTaskMutationContext(parent)
	_, err := model.SaveTaskPollSnapshot(mutationCtx, task, next)
	cancel()
	return err
}

func checkTaskNeedUpdate(oldTask *model.Task, newTask sunoProvider.SunoDataResponse) bool {
	if oldTask == nil {
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
