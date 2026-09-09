package model

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

const (
	TaskPollInterval     = 15 * time.Second
	TaskPollFenceWindow  = 45 * time.Second
	TaskProgressPageSize = 200
)

// ListDueTaskOwners performs a stable keyset scan. Activation may reduce
// latency, but every owner becomes discoverable from durable next_action_at.
func ListDueTaskOwners(ctx context.Context, now time.Time, afterNextActionAt, afterID int64, limit int) ([]*Task, error) {
	if DB == nil {
		return nil, errors.New("task database is unavailable")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if limit <= 0 || limit > TaskProgressPageSize {
		limit = TaskProgressPageSize
	}
	query := DB.WithContext(normalizeModelContext(ctx)).
		Where("provider_state IN ? AND next_action_at <= ?", []TaskProviderState{TaskProviderStatePrepared, TaskProviderStateSubmitStarted, TaskProviderStateAccepted}, now.Unix())
	if afterNextActionAt > 0 || afterID > 0 {
		query = query.Where("next_action_at > ? OR (next_action_at = ? AND id > ?)", afterNextActionAt, afterNextActionAt, afterID)
	}
	var tasks []*Task
	err := query.Order("next_action_at ASC, id ASC").Limit(limit).Find(&tasks).Error
	return tasks, err
}

// ClaimTaskPoll advances version before provider I/O. The returned task version
// is the fence that every result—terminal or not—must match.
func ClaimTaskPoll(ctx context.Context, task *Task, now time.Time, fenceWindow time.Duration) (TaskMutationResult, error) {
	if DB == nil || task == nil || task.ID <= 0 || task.ProviderState != TaskProviderStateAccepted {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, ErrTaskBillingState
	}
	if now.IsZero() {
		now = time.Now()
	}
	if fenceWindow <= 0 {
		fenceWindow = TaskPollFenceWindow
	}
	nextActionAt := now.Add(fenceWindow).Unix()
	result := DB.WithContext(normalizeModelContext(ctx)).Model(&Task{}).
		Where("id = ? AND owner_id = ? AND provider_state = ? AND version = ? AND next_action_at <= ?", task.ID, task.OwnerID, TaskProviderStateAccepted, task.Version, now.Unix()).
		Updates(map[string]any{
			"version":        gorm.Expr("version + 1"),
			"next_action_at": nextActionAt,
			"updated_at":     now.Unix(),
		})
	if result.Error != nil {
		durable, readErr := loadTaskOwnerForRecovery(ctx, task.OwnerID)
		if readErr == nil && durable.ProviderState == TaskProviderStateAccepted && durable.Version == task.Version+1 && durable.NextActionAt == nextActionAt {
			*task = *durable
			return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: true}, nil
		}
		if readErr == nil {
			return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: true}, result.Error
		}
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: true}, errors.Join(result.Error, readErr)
	}
	if result.RowsAffected != 1 {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: true}, ErrTaskBillingState
	}
	task.Version++
	task.NextActionAt = nextActionAt
	task.UpdatedAt = now.Unix()
	return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: true}, nil
}

// SaveTaskPollSnapshot persists only provider evidence and scheduling fields.
// It never saves the whole aggregate, so a late non-terminal result cannot
// revive a closed owner or erase settlement data.
func SaveTaskPollSnapshot(ctx context.Context, task *Task, next time.Time) (TaskMutationResult, error) {
	if DB == nil || task == nil || task.ID <= 0 || task.ProviderState != TaskProviderStateAccepted || isTaskTerminalStatus(task.Status) {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, ErrTaskBillingState
	}
	if next.IsZero() {
		next = time.Now().Add(TaskPollInterval)
	}
	updates := map[string]any{
		"status":         task.Status,
		"fail_reason":    task.FailReason,
		"progress":       task.Progress,
		"submit_time":    task.SubmitTime,
		"start_time":     task.StartTime,
		"finish_time":    task.FinishTime,
		"data":           task.Data,
		"next_action_at": next.Unix(),
		"updated_at":     time.Now().Unix(),
	}
	result := DB.WithContext(normalizeModelContext(ctx)).Model(&Task{}).
		Where("id = ? AND owner_id = ? AND provider_state = ? AND version = ?", task.ID, task.OwnerID, TaskProviderStateAccepted, task.Version).
		Updates(updates)
	if result.Error != nil {
		durable, readErr := loadTaskOwnerForRecovery(ctx, task.OwnerID)
		if readErr == nil && sameTaskPollSnapshot(durable, task, next.Unix()) {
			*task = *durable
			return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: true}, nil
		}
		if readErr == nil {
			return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: true}, result.Error
		}
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: true}, errors.Join(result.Error, readErr)
	}
	if result.RowsAffected != 1 {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: true}, ErrTaskBillingState
	}
	task.NextActionAt = next.Unix()
	return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: true}, nil
}

func sameTaskPollSnapshot(durable, observed *Task, nextActionAt int64) bool {
	return durable != nil && observed != nil && durable.ProviderState == TaskProviderStateAccepted && durable.Version == observed.Version && durable.Status == observed.Status && durable.FailReason == observed.FailReason && durable.Progress == observed.Progress && durable.SubmitTime == observed.SubmitTime && durable.StartTime == observed.StartTime && durable.FinishTime == observed.FinishTime && string(durable.Data) == string(observed.Data) && durable.NextActionAt == nextActionAt
}
