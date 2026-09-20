package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const (
	TaskPlatformOpenAIResponsesBackground = "openai_responses_background"
	BackgroundResponseTaskLimit           = 32
	BackgroundResponseTrackingWindow      = 24 * time.Hour
	BackgroundResponseAccessRetention     = 24 * time.Hour
	BackgroundResponseTaskRetention       = 90 * 24 * time.Hour
	BackgroundResponseEvidenceMaxBytes    = 64 << 10
)

var ErrBackgroundResponseCapacity = errors.New("background response task capacity exhausted")

func sameResponseTaskOwner(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func checkBackgroundResponseCapacity(tx *gorm.DB, userID int) error {
	var count int64
	if err := tx.Model(&Task{}).Where("user_id = ? AND platform = ? AND provider_state <> ?", userID, TaskPlatformOpenAIResponsesBackground, TaskProviderStateClosed).Count(&count).Error; err != nil {
		return err
	}
	if count >= BackgroundResponseTaskLimit {
		return ErrBackgroundResponseCapacity
	}
	return nil
}

// NewBackgroundResponseOwner retains only authorization metadata. Its deadline
// is not a claim about upstream response availability. Explicit store=true uses
// the adapter's ordinary stored retention policy.
func NewBackgroundResponseOwner(responseID string, userID, tokenID, channelID int, now time.Time, store bool, identity ...string) (*ResponseOwner, error) {
	if now.IsZero() {
		now = time.Now()
	}
	owner, err := NewResponseOwner(responseID, userID, tokenID, channelID, now, identity...)
	if err != nil {
		return nil, err
	}
	if !store {
		owner.ExpiresAt = now.Add(BackgroundResponseTrackingWindow + BackgroundResponseAccessRetention)
	}
	return owner, nil
}

// AcceptBackgroundResponseSubmission binds both identities in the acceptance
// transaction. A failed owner insert cannot leave an accepted task behind.
func AcceptBackgroundResponseSubmission(ctx context.Context, task *Task, responseID string, owner *ResponseOwner) (TaskMutationResult, error) {
	if task == nil || owner == nil || task.Platform != TaskPlatformOpenAIResponsesBackground || owner.ResponseID != responseID || task.UserId != owner.UserID || task.TokenID != owner.TokenID || task.ChannelId != owner.ChannelID || task.OwnerID == "" || (owner.TaskOwnerID != nil && *owner.TaskOwnerID != task.OwnerID) {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, ErrTaskIdentity
	}
	id := task.OwnerID
	owner.TaskOwnerID = &id
	return acceptTaskSubmissionWithOwner(ctx, task, responseID, "", owner)
}

// FinalizeBackgroundResponseBillingOwner stores the caller's minimal billing
// evidence with the same Task version and balance transaction. It never stores
// the upstream output. Duplicate/late finalizers return the durable decision.
func FinalizeBackgroundResponseBillingOwner(ctx context.Context, task *Task, targetQuota int64, decision string, evidence []byte) (BillingBalanceResult, error) {
	if task == nil || task.Platform != TaskPlatformOpenAIResponsesBackground || len(evidence) > BackgroundResponseEvidenceMaxBytes || !json.Valid(evidence) {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, ErrTaskBillingState
	}
	candidate := *task
	candidate.Data = append(datatypes.JSON(nil), evidence...)
	result, err := finalizeTaskBillingOwner(ctx, &candidate, targetQuota, decision, func(tx *gorm.DB, durable *Task, now int64) error {
		if durable.Platform != TaskPlatformOpenAIResponsesBackground {
			return ErrTaskBillingState
		}
		deadline := time.Unix(now, 0).Add(BackgroundResponseAccessRetention)
		return tx.Model(&ResponseOwner{}).Where("task_owner_id = ? AND expires_at < ?", durable.OwnerID, deadline).Update("expires_at", deadline).Error
	})
	if result.Outcome == BillingBalanceCommitted {
		*task = candidate
	}
	return result, err
}

func GetBackgroundResponseTask(ctx context.Context, ownerID string) (*Task, error) {
	if DB == nil || strings.TrimSpace(ownerID) == "" {
		return nil, ErrTaskBillingState
	}
	var task Task
	err := DB.WithContext(normalizeModelContext(ctx)).Where("owner_id = ? AND platform = ?", ownerID, TaskPlatformOpenAIResponsesBackground).First(&task).Error
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// Cleanup never touches live reservations or response authorization records.
func DeleteExpiredBackgroundResponseTasks(ctx context.Context, now time.Time) (int64, error) {
	if DB == nil {
		return 0, errors.New("database is not initialized")
	}
	if now.IsZero() {
		now = time.Now()
	}
	result := DB.WithContext(normalizeModelContext(ctx)).Where("platform = ? AND provider_state = ? AND owner_closed_at <= ? AND charged_quota IS NOT NULL AND settlement_decision IN ?", TaskPlatformOpenAIResponsesBackground, TaskProviderStateClosed, now.Add(-BackgroundResponseTaskRetention).Unix(), []string{"cancel", "confirm"}).Delete(&Task{})
	return result.RowsAffected, result.Error
}

// SaveBackgroundResponseEvidence 将观察证据与调度时间写入同一版本。HTTP、
// SSE 和 poll 都必须消费这个 fence，旧观察不能覆盖已经提交的新证据。
func SaveBackgroundResponseEvidence(ctx context.Context, task *Task, next time.Time) (TaskMutationResult, error) {
	if DB == nil || task == nil || task.ID <= 0 || task.OwnerID == "" || task.Platform != TaskPlatformOpenAIResponsesBackground || task.ProviderState != TaskProviderStateAccepted || isTaskTerminalStatus(task.Status) || len(task.Data) > BackgroundResponseEvidenceMaxBytes || !json.Valid(task.Data) {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, ErrTaskBillingState
	}
	if next.IsZero() {
		next = time.Now().Add(TaskPollInterval)
	}
	now := time.Now().Unix()
	result := DB.WithContext(normalizeModelContext(ctx)).Model(&Task{}).
		Where("id = ? AND owner_id = ? AND platform = ? AND provider_state = ? AND version = ?", task.ID, task.OwnerID, TaskPlatformOpenAIResponsesBackground, TaskProviderStateAccepted, task.Version).
		Updates(map[string]any{
			"status": task.Status, "fail_reason": task.FailReason, "progress": task.Progress,
			"submit_time": task.SubmitTime, "start_time": task.StartTime, "finish_time": task.FinishTime,
			"data": task.Data, "next_action_at": next.Unix(), "updated_at": now, "version": gorm.Expr("version + 1"),
		})
	if result.Error == nil && result.RowsAffected == 1 {
		task.Version++
		task.NextActionAt = next.Unix()
		task.UpdatedAt = now
		return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: true}, nil
	}
	// 单语句提交结果不明确时仅回读，不重发上游或再次覆写证据。也允许
	// 同一个旧版本、同一证据的本地重入确认先前已成功提交。
	durable, readErr := loadTaskOwnerForRecovery(ctx, task.OwnerID)
	expected := *task
	expected.Version++
	if readErr == nil && durable.ID == task.ID && durable.Platform == TaskPlatformOpenAIResponsesBackground && sameBackgroundResponseEvidence(durable, &expected, next.Unix()) {
		*task = *durable
		return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: true}, nil
	}
	if readErr != nil {
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: true}, errors.Join(result.Error, readErr)
	}
	if result.Error != nil && durable.Version != task.Version {
		// 更晚的 writer 已推进版本，不能反推本次是否曾提交成功。
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: true}, result.Error
	}
	return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: true}, errors.Join(result.Error, ErrTaskBillingState)
}

// MySQL JSON 等存储可能规范化对象键顺序；回读确认比较结构，保留数字精度。
func sameBackgroundResponseEvidence(durable, expected *Task, next int64) bool {
	if durable == nil || expected == nil {
		return false
	}
	if !bytes.Equal(durable.Data, expected.Data) {
		var stored, observed any
		left := json.NewDecoder(bytes.NewReader(durable.Data))
		left.UseNumber()
		right := json.NewDecoder(bytes.NewReader(expected.Data))
		right.UseNumber()
		if left.Decode(&stored) != nil || right.Decode(&observed) != nil || !reflect.DeepEqual(stored, observed) {
			return false
		}
	}
	projected := *expected
	projected.Data = durable.Data
	return sameTaskPollSnapshot(durable, &projected, next)
}
