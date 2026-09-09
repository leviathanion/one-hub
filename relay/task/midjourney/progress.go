package midjourney

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"one-api/common/requester"
	"one-api/model"
	provider "one-api/providers/midjourney"
	taskbase "one-api/relay/task/base"
)

const maxMidjourneyPollBodyBytes = 2 << 20

var (
	midjourneyTaskPollDeadline          = 30 * time.Second
	midjourneyTaskOwnerMutationDeadline = 5 * time.Second
)

type Task struct{}

func (*Task) UpdateTaskStatus(ctx context.Context, taskChannelM map[int][]string, taskM map[string]*model.Task) error {
	var joined error
	for channelID, taskIDs := range taskChannelM {
		if err := updateTaskBatch(ctx, channelID, taskIDs, taskM); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func updateTaskBatch(ctx context.Context, channelID int, taskIDs []string, taskMap map[string]*model.Task) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	queryCtx, cancelQuery := midjourneyTaskQueryContext(ctx)
	defer cancelQuery()
	channel, err := model.GetChannelIncarnationByID(queryCtx, channelID)
	if err != nil {
		return fmt.Errorf("load Midjourney owner channel %d: %w", channelID, err)
	}
	if channel.BaseURL == nil || strings.TrimSpace(*channel.BaseURL) == "" {
		return fmt.Errorf("Midjourney owner channel %d has no base URL", channelID)
	}
	body, err := json.Marshal(map[string]any{"ids": taskIDs})
	if err != nil {
		return err
	}
	proxy := ""
	if channel.Proxy != nil {
		proxy = *channel.Proxy
	}
	httpRequester := requester.NewHTTPRequester(proxy, nil)
	req, err := httpRequester.NewRequest(http.MethodPost, strings.TrimRight(*channel.BaseURL, "/")+"/mj/task/list-by-condition",
		httpRequester.WithContext(queryCtx),
		httpRequester.WithHeader(map[string]string{"Content-Type": "application/json", "mj-api-secret": channel.Key}),
		httpRequester.WithBody(bytes.NewReader(body)),
	)
	if err != nil {
		return err
	}
	resp, apiErr := httpRequester.SendRequestRaw(req)
	if apiErr != nil {
		return apiErr
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxMidjourneyPollBodyBytes+1))
	if err != nil {
		return err
	}
	if len(responseBody) > maxMidjourneyPollBodyBytes {
		return errors.New("Midjourney poll response exceeds 2 MiB")
	}
	var items []provider.MidjourneyDto
	if err := json.Unmarshal(responseBody, &items); err != nil {
		return fmt.Errorf("decode Midjourney poll response: %w", err)
	}

	returned := make(map[string]struct{}, len(items))
	for _, item := range items {
		owner := taskMap[item.MjId]
		if owner == nil {
			continue
		}
		returned[item.MjId] = struct{}{}
		applySnapshot(owner, item)
		switch owner.Status {
		case model.TaskStatusSuccess, model.TaskStatusFailure, model.TaskStatusCancel:
			owner.Progress = 100
			if _, err := finalizeMidjourneyTaskSettlement(ctx, owner); err != nil {
				return fmt.Errorf("finalize Midjourney task %s: %w", item.MjId, err)
			}
		default:
			if _, err := saveMidjourneyTaskPollSnapshot(ctx, owner, time.Now().Add(model.TaskPollInterval)); err != nil {
				return fmt.Errorf("save Midjourney task %s: %w", item.MjId, err)
			}
		}
	}
	for _, taskID := range taskIDs {
		if _, ok := returned[taskID]; ok {
			continue
		}
		owner := taskMap[taskID]
		if owner == nil {
			continue
		}
		// 本次查询缺失不是供应商终态；保留已接受任务，继续观察。
		if err := rescheduleMidjourneyTaskPoll(ctx, owner, 30*time.Second); err != nil {
			return fmt.Errorf("reschedule missing Midjourney task %s: %w", taskID, err)
		}
	}
	return nil
}

func midjourneyTaskQueryContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, midjourneyTaskPollDeadline)
}

func midjourneyTaskMutationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), midjourneyTaskOwnerMutationDeadline)
}

func rescheduleMidjourneyTaskPoll(parent context.Context, task *model.Task, delay time.Duration) error {
	mutationCtx, cancel := midjourneyTaskMutationContext(parent)
	err := taskbase.RescheduleTaskPoll(mutationCtx, task, delay)
	cancel()
	return err
}

func finalizeMidjourneyTaskSettlement(parent context.Context, task *model.Task) (taskbase.TaskSettlementFinalizeResult, error) {
	mutationCtx, cancel := midjourneyTaskMutationContext(parent)
	result, err := taskbase.FinalizeTaskSettlement(mutationCtx, task)
	cancel()
	return result, err
}

func saveMidjourneyTaskPollSnapshot(parent context.Context, task *model.Task, next time.Time) (model.TaskMutationResult, error) {
	mutationCtx, cancel := midjourneyTaskMutationContext(parent)
	result, err := model.SaveTaskPollSnapshot(mutationCtx, task, next)
	cancel()
	return result, err
}

func applySnapshot(owner *model.Task, item provider.MidjourneyDto) {
	if owner == nil {
		return
	}
	view := model.MidjourneyFromTask(owner)
	view.MjId = model.TaskProviderID(owner)
	view.PromptEn = item.PromptEn
	view.State = item.State
	view.ImageUrl = item.ImageUrl
	view.Description = item.Description
	if properties, err := json.Marshal(item.Properties); err == nil {
		view.Properties = string(properties)
	}
	if buttons, err := json.Marshal(item.Buttons); err == nil {
		view.Buttons = string(buttons)
	}
	if status := model.TaskStatus(strings.ToUpper(strings.TrimSpace(item.Status))); status != "" {
		owner.Status = status
	}
	owner.FailReason = item.FailReason
	if item.SubmitTime != 0 {
		owner.SubmitTime = item.SubmitTime
	}
	if item.StartTime != 0 {
		owner.StartTime = item.StartTime
	}
	if item.FinishTime != 0 {
		owner.FinishTime = item.FinishTime
	}
	progress := strings.TrimSuffix(strings.TrimSpace(item.Progress), "%")
	if parsed, err := strconv.Atoi(progress); err == nil && parsed >= 0 && parsed <= 100 {
		owner.Progress = parsed
	}
	view.Progress = fmt.Sprintf("%d%%", owner.Progress)
	view.Status = string(owner.Status)
	view.FailReason = owner.FailReason
	view.SubmitTime = owner.SubmitTime
	view.StartTime = owner.StartTime
	view.FinishTime = owner.FinishTime
	owner.Data = model.EncodeMidjourneyTaskData(view)
}

// Compile-time assertion keeps the background adapter on the same aggregate
// contract as Suno and Kling.
var _ taskbase.TaskProgressInterface = (*Task)(nil)
