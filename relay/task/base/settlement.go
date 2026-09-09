package base

import (
	"context"
	"errors"
	"strings"
	"time"

	"one-api/model"
)

type TaskSettlementFinalizeResult struct {
	Handled     bool
	PersistTask bool
}

// FinalizeTaskSettlement cancels the reservation because the currently
// supported async providers do not return authoritative billable usage.
func FinalizeTaskSettlement(ctx context.Context, task *model.Task) (TaskSettlementFinalizeResult, error) {
	if task == nil {
		return TaskSettlementFinalizeResult{}, errors.New("task is nil")
	}
	task.Progress = 100
	if err := finalizeTaskBillingOwner(ctx, task, 0, "cancel"); err != nil {
		return TaskSettlementFinalizeResult{Handled: true}, err
	}
	return TaskSettlementFinalizeResult{Handled: true, PersistTask: false}, nil
}

func FailTaskWithSettlement(ctx context.Context, task *model.Task, reason string) error {
	if task == nil || task.ProviderState == model.TaskProviderStateClosed {
		return nil
	}
	task.FailReason = strings.TrimSpace(reason)
	task.Progress = 100
	if !isTerminalTaskStatus(task.Status) {
		switch task.ProviderState {
		case model.TaskProviderStatePrepared:
			task.Status = model.TaskStatusLocalFailure
		case model.TaskProviderStateSubmitStarted:
			task.Status = model.TaskStatusUnknown
		default:
			task.Status = model.TaskStatusFailure
		}
	}
	return finalizeTaskBillingOwner(ctx, task, 0, "cancel")
}

func finalizeTaskBillingOwner(ctx context.Context, task *model.Task, targetQuota int64, decision string) error {
	_, err := model.FinalizeTaskBillingOwner(ctx, task, targetQuota, decision)
	return err
}

func HasTaskSettlementSnapshot(task *model.Task) bool {
	return task != nil && task.ProviderState != ""
}

func RescheduleTaskPoll(ctx context.Context, task *model.Task, delay time.Duration) error {
	if task == nil {
		return errors.New("task is nil")
	}
	_, err := model.SaveTaskPollSnapshot(ctx, task, time.Now().Add(delay))
	return err
}

func TaskTrackingHandle(task *model.Task) string {
	if task == nil || (task.ProviderState != model.TaskProviderStateAccepted && task.ProviderState != model.TaskProviderStateClosed) {
		return ""
	}
	return model.TaskProviderID(task)
}

func TaskAcceptedWithoutTrackingHandle(task *model.Task) bool {
	return task != nil && task.ProviderState == model.TaskProviderStateAccepted && model.TaskProviderID(task) == ""
}

func isTerminalTaskStatus(status model.TaskStatus) bool {
	switch status {
	case model.TaskStatusSuccess, model.TaskStatusFailure, model.TaskStatusCancel, model.TaskStatusLocalFailure, model.TaskStatusUnknown:
		return true
	default:
		return false
	}
}
