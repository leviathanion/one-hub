package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	TaskPlatformOpenAIBatch = "openai_batch"
	BatchTaskLimit          = 32
	BatchTrackingWindow     = 48 * time.Hour
	BatchTaskRetention      = 90 * 24 * time.Hour
	BatchDataMaxBytes       = 8 << 20
)

var ErrOpenAIBatchCapacity = errors.New("batch task capacity exhausted")

func checkOpenAIBatchCapacity(tx *gorm.DB, userID int) error {
	var count int64
	if err := tx.Model(&Task{}).Where("user_id = ? AND platform = ? AND provider_state <> ?", userID, TaskPlatformOpenAIBatch, TaskProviderStateClosed).Count(&count).Error; err != nil {
		return err
	}
	if count >= BatchTaskLimit {
		return ErrOpenAIBatchCapacity
	}
	return nil
}

// 接受事实、Batch 归属与派生容量在同一事务提交，不镜像上游业务状态。
func AcceptOpenAIBatchSubmission(ctx context.Context, task *Task, batchID string, batchReservationID uint64, derivedReservationIDs []uint64) (TaskMutationResult, error) {
	if task == nil || task.Platform != TaskPlatformOpenAIBatch || batchReservationID == 0 || len(derivedReservationIDs) > ResourceOwnerLimit {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, ErrTaskIdentity
	}
	return acceptTaskSubmissionWithOwnerAndHook(ctx, task, batchID, "", nil, func(tx *gorm.DB) error {
		var durable Task
		if err := tx.Where("id = ? AND owner_id = ? AND platform = ?", task.ID, task.OwnerID, TaskPlatformOpenAIBatch).Take(&durable).Error; err != nil {
			return err
		}
		if durable.UserId != task.UserId || durable.TokenID != task.TokenID || durable.ChannelId != task.ChannelId || durable.ProviderNamespace != task.ProviderNamespace || durable.ProviderTaskScopeIncarnation != task.ProviderTaskScopeIncarnation {
			return ErrTaskIdentity
		}
		ids := make([]uint64, 0, 1+len(derivedReservationIDs))
		ids = append(ids, batchReservationID)
		ids = append(ids, derivedReservationIDs...)
		seen := make(map[uint64]bool, len(ids))
		for _, id := range ids {
			if id == 0 || seen[id] {
				return ErrTaskIdentity
			}
			seen[id] = true
		}
		var slots []ResourceOwner
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Find(&slots).Error; err != nil {
			return err
		}
		if len(slots) != len(ids) {
			return ErrResourceReservationExpired
		}
		for _, slot := range slots {
			if slot.UserID != durable.UserId || slot.TokenID != durable.TokenID || slot.ChannelID != durable.ChannelId || slot.ProviderNamespace != durable.ProviderNamespace || slot.ProviderScope != durable.ProviderTaskScopeIncarnation || slot.TaskOwnerID == nil || *slot.TaskOwnerID != durable.OwnerID || slot.Slot == nil || slot.Phase != ResourceOwnerReserved {
				return ErrTaskIdentity
			}
			if slot.ID == batchReservationID && (slot.Kind != "batch" || *slot.Slot != "batch") {
				return ErrTaskIdentity
			}
		}
		repo := NewResourceOwnerRepository(tx)
		if _, err := repo.Bind(ctx, batchReservationID, durable.UserId, batchID); err != nil {
			return err
		}
		if len(derivedReservationIDs) != 0 {
			return repo.TransferToTask(ctx, durable.UserId, durable.OwnerID, derivedReservationIDs...)
		}
		return nil
	})
}

// 仅持久化有界计费事实；收尾释放未使用预留，真实产出的 owner 继续留存。
func FinalizeOpenAIBatchBillingOwner(ctx context.Context, task *Task, targetQuota int64, decision string, data []byte) (BillingBalanceResult, error) {
	if task == nil || task.Platform != TaskPlatformOpenAIBatch || len(data) > BatchDataMaxBytes || !json.Valid(data) {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, ErrTaskBillingState
	}
	candidate := *task
	candidate.Data = append(datatypes.JSON(nil), data...)
	result, err := finalizeTaskBillingOwner(ctx, &candidate, targetQuota, decision, func(tx *gorm.DB, durable *Task, _ int64) error {
		if durable.Platform != TaskPlatformOpenAIBatch {
			return ErrTaskIdentity
		}
		return NewResourceOwnerRepository(tx).ReleaseTaskSlots(ctx, durable.UserId, durable.OwnerID)
	})
	if result.Outcome == BillingBalanceCommitted {
		*task = candidate
	}
	return result, err
}

func GetOpenAIBatchTask(ctx context.Context, ownerID string) (*Task, error) {
	if DB == nil || strings.TrimSpace(ownerID) == "" {
		return nil, ErrTaskBillingState
	}
	var task Task
	if err := DB.WithContext(normalizeModelContext(ctx)).Where("owner_id = ? AND platform = ?", ownerID, TaskPlatformOpenAIBatch).First(&task).Error; err != nil {
		return nil, err
	}
	if task.OwnerID != ownerID {
		return nil, ErrTaskIdentity
	}
	return &task, nil
}

// CAS 保护本地观察事实，旧 poll 不能覆盖新证据或已经结算的决定。
func SaveOpenAIBatchSnapshot(ctx context.Context, task *Task, next time.Time) (TaskMutationResult, error) {
	if DB == nil || task == nil || task.ID <= 0 || task.OwnerID == "" || task.Platform != TaskPlatformOpenAIBatch || task.ProviderState != TaskProviderStateAccepted || isTaskTerminalStatus(task.Status) || len(task.Data) > BatchDataMaxBytes || !json.Valid(task.Data) {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, ErrTaskBillingState
	}
	if next.IsZero() {
		next = time.Now().Add(TaskPollInterval)
	}
	now := time.Now().Unix()
	update := DB.WithContext(normalizeModelContext(ctx)).Model(&Task{}).Where("id = ? AND owner_id = ? AND platform = ? AND provider_state = ? AND version = ?", task.ID, task.OwnerID, TaskPlatformOpenAIBatch, TaskProviderStateAccepted, task.Version).Updates(map[string]any{
		"status": task.Status, "fail_reason": task.FailReason, "progress": task.Progress,
		"submit_time": task.SubmitTime, "start_time": task.StartTime, "finish_time": task.FinishTime,
		"data": task.Data, "next_action_at": next.Unix(), "updated_at": now, "version": gorm.Expr("version + 1"),
	})
	if update.Error == nil && update.RowsAffected == 1 {
		task.Version++
		task.NextActionAt = next.Unix()
		task.UpdatedAt = now
		return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: true}, nil
	}
	durable, readErr := loadTaskOwnerForRecovery(ctx, task.OwnerID)
	expected := *task
	expected.Version++
	// 已有比较器只比较 Task 快照与规范化 JSON，不解释协议字段。
	if readErr == nil && durable.ID == task.ID && durable.Platform == TaskPlatformOpenAIBatch && sameBackgroundResponseEvidence(durable, &expected, next.Unix()) {
		*task = *durable
		return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: true}, nil
	}
	if readErr != nil {
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: true}, errors.Join(update.Error, readErr)
	}
	if update.Error != nil && durable.Version != task.Version {
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: true}, update.Error
	}
	return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: true}, errors.Join(update.Error, ErrTaskBillingState)
}

func DeleteExpiredOpenAIBatchTasks(ctx context.Context, now time.Time) (int64, error) {
	if DB == nil {
		return 0, errors.New("database is not initialized")
	}
	if now.IsZero() {
		now = time.Now()
	}
	result := DB.WithContext(normalizeModelContext(ctx)).Where("platform = ? AND provider_state = ? AND owner_closed_at <= ? AND charged_quota IS NOT NULL AND settlement_decision IN ?", TaskPlatformOpenAIBatch, TaskProviderStateClosed, now.Add(-BatchTaskRetention).Unix(), []string{"confirm", "cancel"}).Delete(&Task{})
	return result.RowsAffected, result.Error
}
