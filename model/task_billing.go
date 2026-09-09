package model

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"one-api/common/config"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrTaskBillingState = errors.New("task billing owner state conflict")
	ErrTaskProviderID   = errors.New("provider task id is invalid")
	ErrTaskIdentity     = errors.New("task identity conflicts with existing ownership")
)

type TaskMutationOutcome string

const (
	TaskMutationApplied              TaskMutationOutcome = "applied"
	TaskMutationDefinitelyNotApplied TaskMutationOutcome = "definitely_not_applied"
	TaskMutationCommitUnknown        TaskMutationOutcome = "commit_unknown"
)

type TaskMutationResult struct {
	Outcome         TaskMutationOutcome
	CommitAttempted bool
}

const taskOwnerRecoveryTimeout = 5 * time.Second

func CreateTaskBillingOwner(ctx context.Context, task *Task) (BillingBalanceResult, error) {
	return createTaskBillingOwner(ctx, task, false)
}

// CreateTaskBillingOwnerForBoundChannel admits a child operation on the exact
// channel incarnation already authorized by a user-owned parent Task. Ordinary
// new Tasks must use CreateTaskBillingOwner and therefore require an enabled
// channel.
func CreateTaskBillingOwnerForBoundChannel(ctx context.Context, task *Task) (BillingBalanceResult, error) {
	return createTaskBillingOwner(ctx, task, true)
}

func createTaskBillingOwner(ctx context.Context, task *Task, allowBoundChannelIncarnation bool) (BillingBalanceResult, error) {
	if task == nil || task.UserId <= 0 || task.TokenID <= 0 || task.ChannelId <= 0 || task.ReservedQuota < 0 || strings.TrimSpace(task.Platform) == "" || strings.TrimSpace(task.ProviderNamespace) == "" || strings.TrimSpace(task.ProviderTaskScopeIncarnation) == "" || strings.TrimSpace(task.RequestFingerprint) == "" {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, errors.New("task billing owner is incomplete")
	}
	if err := task.BeforeCreate(nil); err != nil {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, err
	}
	tx, result, err := beginTaskBillingTransaction(ctx)
	if err != nil {
		return result, err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback().Error
		}
	}()
	var channel Channel
	channelQuery := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", task.ChannelId)
	if allowBoundChannelIncarnation {
		channelQuery = channelQuery.Unscoped()
	} else {
		channelQuery = channelQuery.Where("status = ?", config.ChannelStatusEnabled)
	}
	if err := channelQuery.First(&channel).Error; err != nil {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, err
	}
	tokenApplied, err := ApplyBillingReserveInTransaction(tx, task.UserId, task.TokenID, task.ReservedQuota)
	if err != nil {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, err
	}
	now, err := currentDatabaseUnix(tx)
	if err != nil {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, err
	}
	task.ProviderState = TaskProviderStatePrepared
	task.TaskID = nil
	task.SubmissionClaimID = ""
	task.AcceptanceRecordedAt = nil
	task.Version = 0
	task.TokenQuotaApplied = tokenApplied
	task.NextActionAt = now + int64((15*time.Minute)/time.Second)
	task.SubmitStartedAt = nil
	task.OwnerClosedAt = nil
	task.ChargedQuota = nil
	task.SettlementDecision = ""
	task.BalanceApplyOutcome = ""
	task.CreatedAt = now
	task.UpdatedAt = now
	if err := tx.Create(task).Error; err != nil {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, err
	}

	result.TokenQuotaApplied = tokenApplied
	result.CommitAttempted = true
	result.Outcome = BillingBalanceCommitUnknown
	rollback = false
	if err := tx.Commit().Error; err != nil {
		durable, readErr := loadTaskOwnerForRecovery(ctx, task.OwnerID)
		if readErr == nil && samePreparedTaskOwner(durable, task) {
			*task = *durable
			result.Outcome = BillingBalanceCommitted
			return result, nil
		}
		if errors.Is(readErr, gorm.ErrRecordNotFound) {
			result.Outcome = BillingBalanceDefinitelyRolledBack
		}
		return result, errors.Join(err, readErr)
	}
	result.Outcome = BillingBalanceCommitted
	return result, nil
}

func ClaimTaskSubmission(ctx context.Context, task *Task, claimID string) (TaskMutationResult, error) {
	claimID = strings.TrimSpace(claimID)
	if task == nil || task.ID <= 0 || strings.TrimSpace(task.OwnerID) == "" || claimID == "" {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, ErrTaskBillingState
	}
	tx, _, err := beginTaskBillingTransaction(ctx)
	if err != nil {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, err
	}
	now, err := currentDatabaseUnix(tx)
	if err != nil {
		_ = tx.Rollback().Error
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, err
	}
	update := tx.Model(&Task{}).Where("id = ? AND owner_id = ? AND provider_state = ? AND version = ? AND submission_claim_id = ''", task.ID, task.OwnerID, TaskProviderStatePrepared, task.Version).Updates(map[string]any{
		"provider_state":      TaskProviderStateSubmitStarted,
		"submission_claim_id": claimID,
		"submit_started_at":   now,
		"next_action_at":      now + int64((15*time.Minute)/time.Second),
		"version":             gorm.Expr("version + 1"),
		"updated_at":          now,
	})
	if update.Error != nil || update.RowsAffected != 1 {
		_ = tx.Rollback().Error
		if update.Error != nil {
			return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, update.Error
		}
		return classifyTaskSubmissionClaim(ctx, task, claimID, nil)
	}
	result := TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: true}
	if err := tx.Commit().Error; err != nil {
		return classifyTaskSubmissionClaim(ctx, task, claimID, err)
	}
	result.Outcome = TaskMutationApplied
	task.ProviderState = TaskProviderStateSubmitStarted
	task.SubmissionClaimID = claimID
	task.SubmitStartedAt = &now
	task.NextActionAt = now + int64((15*time.Minute)/time.Second)
	task.Version++
	return result, nil
}

func AcceptTaskSubmission(ctx context.Context, task *Task, providerTaskID string) (TaskMutationResult, error) {
	return acceptTaskSubmission(ctx, task, providerTaskID, "")
}

// AcceptTaskSubmissionWithLegacyFingerprint permits a caller with a bounded
// migration contract to recognize an owner written with the previous request
// fingerprint. The alternate value is only considered after the normal
// provider/public identity conflict path and must be a SHA-256 hex digest.
func AcceptTaskSubmissionWithLegacyFingerprint(ctx context.Context, task *Task, providerTaskID, legacyFingerprint string) (TaskMutationResult, error) {
	legacyFingerprint = strings.TrimSpace(legacyFingerprint)
	if !isTaskFingerprint(legacyFingerprint) {
		legacyFingerprint = ""
	}
	return acceptTaskSubmission(ctx, task, providerTaskID, legacyFingerprint)
}

func acceptTaskSubmission(ctx context.Context, task *Task, providerTaskID, legacyFingerprint string) (TaskMutationResult, error) {
	providerTaskID = strings.TrimSpace(providerTaskID)
	if task == nil || task.ID <= 0 || strings.TrimSpace(task.OwnerID) == "" || strings.TrimSpace(task.SubmissionClaimID) == "" || providerTaskID == "" || len(providerTaskID) > 191 || strings.TrimSpace(task.ProviderNamespace) == "" || strings.TrimSpace(task.ProviderTaskScopeIncarnation) == "" {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, ErrTaskProviderID
	}
	tx, _, err := beginTaskBillingTransaction(ctx)
	if err != nil {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, err
	}
	now, err := currentDatabaseUnix(tx)
	if err != nil {
		_ = tx.Rollback().Error
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, err
	}
	update := tx.Model(&Task{}).Where("id = ? AND owner_id = ? AND provider_state = ? AND submission_claim_id = ? AND version = ? AND acceptance_recorded_at IS NULL", task.ID, task.OwnerID, TaskProviderStateSubmitStarted, task.SubmissionClaimID, task.Version).Updates(map[string]any{
		"provider_state":         TaskProviderStateAccepted,
		"task_id":                providerTaskID,
		"status":                 TaskStatusSubmitted,
		"acceptance_recorded_at": now,
		"next_action_at":         now,
		"version":                gorm.Expr("version + 1"),
		"updated_at":             now,
	})
	if update.Error != nil || update.RowsAffected != 1 {
		_ = tx.Rollback().Error
		if update.Error != nil {
			if IsUniqueConstraintError(update.Error) {
				return resolveTaskAcceptanceIdentityConflict(ctx, task, providerTaskID, update.Error, legacyFingerprint)
			}
			return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, update.Error
		}
		return classifyTaskAcceptance(ctx, task, providerTaskID, nil)
	}
	result := TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: true}
	if err := tx.Commit().Error; err != nil {
		return classifyTaskAcceptance(ctx, task, providerTaskID, err)
	}
	result.Outcome = TaskMutationApplied
	task.ProviderState = TaskProviderStateAccepted
	SetTaskProviderID(task, providerTaskID)
	task.Status = TaskStatusSubmitted
	task.AcceptanceRecordedAt = &now
	task.NextActionAt = now
	task.Version++
	return result, nil
}

func resolveTaskAcceptanceIdentityConflict(ctx context.Context, provisional *Task, providerTaskID string, constraintErr error, legacyFingerprint string) (TaskMutationResult, error) {
	if DB == nil || provisional == nil {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, fmt.Errorf("%w: %v", ErrTaskIdentity, constraintErr)
	}
	db := DB.WithContext(normalizeModelContext(ctx))
	var physical, public Task
	physicalErr := db.Where("provider_namespace = ? AND provider_task_scope_incarnation = ? AND task_id = ?", provisional.ProviderNamespace, provisional.ProviderTaskScopeIncarnation, providerTaskID).First(&physical).Error
	publicErr := db.Where("platform = ? AND user_id = ? AND task_id = ?", provisional.Platform, provisional.UserId, providerTaskID).First(&public).Error
	var existing *Task
	switch {
	case physicalErr == nil && publicErr == nil && physical.ID == public.ID:
		existing = &physical
	case physicalErr == nil && errors.Is(publicErr, gorm.ErrRecordNotFound):
		existing = &physical
	case publicErr == nil && errors.Is(physicalErr, gorm.ErrRecordNotFound):
		existing = &public
	case physicalErr != nil && !errors.Is(physicalErr, gorm.ErrRecordNotFound):
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown}, errors.Join(constraintErr, physicalErr)
	case publicErr != nil && !errors.Is(publicErr, gorm.ErrRecordNotFound):
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown}, errors.Join(constraintErr, publicErr)
	}

	closed := *provisional
	SetTaskProviderID(&closed, "")
	closed.Status = TaskStatusLocalFailure
	closed.FailReason = "provider task identity conflicts with existing ownership"
	closed.Progress = 100
	closeResult, closeErr := FinalizeTaskBillingOwner(ctx, &closed, 0, "cancel")
	if closeErr != nil || closeResult.Outcome != BillingBalanceCommitted {
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: closeResult.CommitAttempted}, errors.Join(constraintErr, closeErr)
	}
	*provisional = closed

	sameRequestFingerprint := existing != nil && existing.RequestFingerprint == provisional.RequestFingerprint
	legacyFingerprintMatch := existing != nil && legacyFingerprint != "" && existing.RequestFingerprint == legacyFingerprint
	sameOwnerScope := existing != nil && existing.UserId == provisional.UserId && existing.Platform == provisional.Platform && existing.ProviderNamespace == provisional.ProviderNamespace && existing.ProviderTaskScopeIncarnation == provisional.ProviderTaskScopeIncarnation
	if legacyFingerprintMatch {
		// The legacy digest predates channel/action fields, so require those
		// durable operation fields before accepting its compatibility path.
		sameOwnerScope = sameOwnerScope && existing.ChannelId == provisional.ChannelId && existing.Action == provisional.Action
	}
	reusable := existing != nil && existing.ProviderState != TaskProviderStatePrepared && existing.ProviderState != TaskProviderStateSubmitStarted && existing.AcceptanceRecordedAt != nil && sameOwnerScope && (sameRequestFingerprint || legacyFingerprintMatch)
	if reusable {
		if existing.RequestFingerprint == legacyFingerprint && existing.RequestFingerprint != provisional.RequestFingerprint {
			if err := upgradeTaskRequestFingerprint(ctx, existing, provisional.RequestFingerprint); err != nil {
				return TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: true}, err
			}
		}
		*provisional = *existing
		return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: true}, nil
	}
	return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: true}, fmt.Errorf("%w: provider_task_id=%s", ErrTaskIdentity, providerTaskID)
}

func upgradeTaskRequestFingerprint(ctx context.Context, existing *Task, newFingerprint string) error {
	if DB == nil || existing == nil || existing.ID <= 0 || strings.TrimSpace(existing.OwnerID) == "" || !isTaskFingerprint(newFingerprint) {
		return ErrTaskIdentity
	}
	newFingerprint = strings.TrimSpace(newFingerprint)
	oldFingerprint := existing.RequestFingerprint
	if oldFingerprint == newFingerprint {
		return nil
	}
	db := DB.WithContext(normalizeModelContext(ctx))
	update := db.Model(&Task{}).
		Where("id = ? AND owner_id = ? AND request_fingerprint = ?", existing.ID, existing.OwnerID, oldFingerprint).
		Update("request_fingerprint", newFingerprint)
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected == 1 {
		existing.RequestFingerprint = newFingerprint
		return nil
	}
	var durable Task
	if err := db.Where("id = ? AND owner_id = ?", existing.ID, existing.OwnerID).First(&durable).Error; err != nil {
		return err
	}
	if durable.RequestFingerprint == newFingerprint {
		*existing = durable
		return nil
	}
	return fmt.Errorf("%w: request fingerprint upgrade conflict", ErrTaskIdentity)
}

func isTaskFingerprint(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func classifyTaskSubmissionClaim(ctx context.Context, task *Task, claimID string, mutationErr error) (TaskMutationResult, error) {
	durable, readErr := loadTaskOwnerForRecovery(ctx, task.OwnerID)
	if readErr == nil && durable.ProviderState == TaskProviderStateSubmitStarted && durable.SubmissionClaimID == claimID {
		*task = *durable
		return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: mutationErr != nil}, nil
	}
	if readErr == nil && durable.ProviderState == TaskProviderStatePrepared && durable.SubmissionClaimID == "" {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: mutationErr != nil}, mutationErr
	}
	if readErr != nil {
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: mutationErr != nil}, errors.Join(mutationErr, readErr)
	}
	return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: mutationErr != nil}, errors.Join(mutationErr, ErrTaskBillingState)
}

func classifyTaskAcceptance(ctx context.Context, task *Task, providerTaskID string, mutationErr error) (TaskMutationResult, error) {
	durable, readErr := loadTaskOwnerForRecovery(ctx, task.OwnerID)
	if readErr == nil && (durable.ProviderState == TaskProviderStateAccepted || durable.ProviderState == TaskProviderStateClosed) && durable.SubmissionClaimID == task.SubmissionClaimID && durable.ProviderNamespace == task.ProviderNamespace && durable.ProviderTaskScopeIncarnation == task.ProviderTaskScopeIncarnation && TaskProviderID(durable) == providerTaskID && durable.AcceptanceRecordedAt != nil {
		*task = *durable
		return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: mutationErr != nil}, nil
	}
	if readErr == nil && durable.ProviderState == TaskProviderStateSubmitStarted && durable.SubmissionClaimID == task.SubmissionClaimID && durable.AcceptanceRecordedAt == nil && (TaskProviderID(durable) == "" || TaskProviderID(durable) == providerTaskID) {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: mutationErr != nil}, mutationErr
	}
	if readErr != nil {
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: mutationErr != nil}, errors.Join(mutationErr, readErr)
	}
	return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: mutationErr != nil}, errors.Join(mutationErr, ErrTaskBillingState)
}

// PreserveTaskSubmissionHandle records diagnostic identity without claiming
// provider acceptance. It is used only after an Accept commit-unknown so a
// later UNKNOWN closure does not lose the already returned provider handle.
func PreserveTaskSubmissionHandle(ctx context.Context, task *Task, providerTaskID string) (TaskMutationResult, error) {
	providerTaskID = strings.TrimSpace(providerTaskID)
	if DB == nil || task == nil || task.ID <= 0 || task.SubmissionClaimID == "" || providerTaskID == "" {
		return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied}, ErrTaskProviderID
	}
	result := DB.WithContext(normalizeModelContext(ctx)).Model(&Task{}).
		Where("id = ? AND owner_id = ? AND provider_state = ? AND submission_claim_id = ? AND acceptance_recorded_at IS NULL AND (task_id IS NULL OR task_id = '' OR task_id = ?)", task.ID, task.OwnerID, TaskProviderStateSubmitStarted, task.SubmissionClaimID, providerTaskID).
		Update("task_id", providerTaskID)
	if result.Error != nil {
		durable, readErr := loadTaskOwnerForRecovery(ctx, task.OwnerID)
		if readErr == nil && durable.SubmissionClaimID == task.SubmissionClaimID && TaskProviderID(durable) == providerTaskID {
			*task = *durable
			return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: true}, nil
		}
		if readErr == nil {
			return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: true}, result.Error
		}
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: true}, errors.Join(result.Error, readErr)
	}
	if result.RowsAffected == 1 {
		SetTaskProviderID(task, providerTaskID)
		return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: true}, nil
	}
	durable, err := loadTaskOwnerForRecovery(ctx, task.OwnerID)
	if err == nil && durable.SubmissionClaimID == task.SubmissionClaimID && TaskProviderID(durable) == providerTaskID {
		*task = *durable
		return TaskMutationResult{Outcome: TaskMutationApplied, CommitAttempted: true}, nil
	}
	if err != nil {
		return TaskMutationResult{Outcome: TaskMutationCommitUnknown, CommitAttempted: true}, err
	}
	return TaskMutationResult{Outcome: TaskMutationDefinitelyNotApplied, CommitAttempted: true}, ErrTaskBillingState
}

func loadTaskOwnerForRecovery(ctx context.Context, ownerID string) (*Task, error) {
	if DB == nil || strings.TrimSpace(ownerID) == "" {
		return nil, gorm.ErrRecordNotFound
	}
	parent := normalizeModelContext(ctx)
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), taskOwnerRecoveryTimeout)
	defer cancel()
	var durable Task
	err := DB.WithContext(recoveryCtx).Where("owner_id = ?", ownerID).First(&durable).Error
	return &durable, err
}

func samePreparedTaskOwner(durable, expected *Task) bool {
	return durable != nil && expected != nil && durable.OwnerID == expected.OwnerID && durable.ProviderState == TaskProviderStatePrepared && durable.UserId == expected.UserId && durable.TokenID == expected.TokenID && durable.ChannelId == expected.ChannelId && durable.Platform == expected.Platform && durable.ProviderNamespace == expected.ProviderNamespace && durable.ProviderTaskScopeIncarnation == expected.ProviderTaskScopeIncarnation && durable.RequestFingerprint == expected.RequestFingerprint && durable.ReservedQuota == expected.ReservedQuota
}

func FinalizeTaskBillingOwner(ctx context.Context, task *Task, targetQuota int64, decision string) (finalResult BillingBalanceResult, finalErr error) {
	if task == nil || task.ID <= 0 || targetQuota < 0 || !isTaskTerminalStatus(task.Status) || (decision != "cancel" && decision != "confirm") || (decision == "cancel" && targetQuota != 0) {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, ErrTaskBillingState
	}
	tx, result, err := beginTaskBillingTransaction(ctx)
	if err != nil {
		return result, err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback().Error
		}
	}()
	var durable Task
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&durable, task.ID).Error; err != nil {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, err
	}
	if durable.ProviderState == TaskProviderStateClosed {
		*task = durable
		_ = tx.Rollback().Error
		rollback = false
		return BillingBalanceResult{Outcome: BillingBalanceCommitted}, nil
	}
	if durable.Version != task.Version {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, ErrTaskBillingState
	}
	canCancel := decision == "cancel"
	if durable.ProviderState != TaskProviderStateAccepted && !canCancel {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, ErrTaskBillingState
	}

	charged := targetQuota
	balanceOutcome := "no_write_required"
	tokenApplied, balanceChanged, applyErr := applyBillingSettlementBalancesInTransaction(tx, durable.UserId, durable.TokenID, durable.ReservedQuota, targetQuota, durable.TokenQuotaApplied)
	if applyErr != nil {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, applyErr
	}
	result.TokenQuotaApplied = tokenApplied
	if balanceChanged {
		balanceOutcome = "applied"
	}
	now, err := currentDatabaseUnix(tx)
	if err != nil {
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, err
	}
	updates := map[string]any{
		"provider_state":        TaskProviderStateClosed,
		"status":                task.Status,
		"fail_reason":           task.FailReason,
		"progress":              task.Progress,
		"data":                  task.Data,
		"submit_time":           task.SubmitTime,
		"start_time":            task.StartTime,
		"finish_time":           task.FinishTime,
		"charged_quota":         charged,
		"settlement_decision":   decision,
		"balance_apply_outcome": balanceOutcome,
		"owner_closed_at":       now,
		"next_action_at":        0,
		"version":               gorm.Expr("version + 1"),
		"updated_at":            now,
	}
	if providerTaskID := TaskProviderID(task); providerTaskID != "" && TaskProviderID(&durable) == "" {
		updates["task_id"] = providerTaskID
	}
	if update := tx.Model(&Task{}).Where("id = ? AND owner_id = ? AND provider_state = ? AND version = ?", durable.ID, durable.OwnerID, durable.ProviderState, durable.Version).Updates(updates); update.Error != nil || update.RowsAffected != 1 {
		if update.Error != nil {
			return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, update.Error
		}
		return BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, ErrTaskBillingState
	}
	result.CommitAttempted = true
	result.Outcome = BillingBalanceCommitUnknown
	rollback = false
	if err := tx.Commit().Error; err != nil {
		durable, readErr := loadTaskOwnerForRecovery(ctx, task.OwnerID)
		if readErr == nil && durable.ProviderState == TaskProviderStateClosed && durable.SettlementDecision == decision && durable.ChargedQuota != nil && *durable.ChargedQuota == charged {
			*task = *durable
			result.Outcome = BillingBalanceCommitted
			return result, nil
		}
		if readErr == nil && durable.ProviderState != TaskProviderStateClosed && durable.Version == task.Version {
			result.Outcome = BillingBalanceDefinitelyRolledBack
		}
		return result, errors.Join(err, readErr)
	}
	result.Outcome = BillingBalanceCommitted
	task.ProviderState = TaskProviderStateClosed
	task.ChargedQuota = &charged
	task.SettlementDecision = decision
	task.BalanceApplyOutcome = balanceOutcome
	task.OwnerClosedAt = &now
	task.NextActionAt = 0
	task.Version++
	return result, nil
}

func isTaskTerminalStatus(status TaskStatus) bool {
	switch status {
	case TaskStatusSuccess, TaskStatusFailure, TaskStatusCancel, TaskStatusLocalFailure, TaskStatusUnknown:
		return true
	default:
		return false
	}
}

func beginTaskBillingTransaction(ctx context.Context) (*gorm.DB, BillingBalanceResult, error) {
	if DB == nil {
		return nil, BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, errors.New("billing database is unavailable")
	}
	tx := DB.WithContext(normalizeModelContext(ctx)).Begin()
	if tx.Error != nil {
		return nil, BillingBalanceResult{Outcome: BillingBalanceDefinitelyRolledBack}, tx.Error
	}
	return tx, BillingBalanceResult{}, nil
}

func currentDatabaseUnix(tx *gorm.DB) (int64, error) {
	if tx == nil {
		return 0, errors.New("database transaction is required")
	}
	query := "SELECT UNIX_TIMESTAMP()"
	switch tx.Dialector.Name() {
	case "sqlite":
		query = "SELECT CAST(strftime('%s','now') AS INTEGER)"
	case "postgres":
		query = "SELECT CAST(EXTRACT(EPOCH FROM CURRENT_TIMESTAMP) AS BIGINT)"
	}
	var now int64
	if err := tx.Raw(query).Scan(&now).Error; err != nil {
		return 0, err
	}
	if now <= 0 {
		return 0, fmt.Errorf("database returned invalid current time %d", now)
	}
	return now, nil
}

func normalizeModelContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
