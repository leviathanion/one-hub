package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"one-api/common/config"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

// CredentialRotationTicket is capability-like: only the attempt that installed
// a fence at this lifecycle revision can cancel or commit it.
type CredentialRotationTicket struct {
	ChannelID        int
	AttemptID        string
	ExpectedRevision uint64
}

type CredentialRotationSnapshot struct {
	ChannelID int
	Type      int
	Key       string
	Revision  uint64
	Fence     *string
	StartedAt *int64
	Deleted   bool
}

type CredentialRotationClaimOutcome int

const (
	CredentialRotationClaimAcquired CredentialRotationClaimOutcome = iota
	CredentialRotationClaimBusy
	CredentialRotationClaimSuperseded
)

var ErrChannelCredentialConflict = errors.New("渠道凭证已被并发更新，请重新授权")

type CredentialRecoverySnapshot struct {
	ChannelID        int
	AccountID        string
	ExpectedRevision uint64
	ExpectedFence    string
}

// RecoverChannelCredentialWithContext 仅供已验证同账号的独立授权码交换结果调用。
// 恢复既有 fence 不授予普通刷新任务抢占权；账号检查与完整快照 CAS 在同一事务中。
func RecoverChannelCredentialWithContext(ctx context.Context, expected CredentialRecoverySnapshot, newKey string) error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	if expected.ChannelID <= 0 || strings.TrimSpace(expected.ExpectedFence) == "" || expected.AccountID == "" || CodexCredentialAccountID(newKey) != expected.AccountID {
		return ErrChannelCredentialConflict
	}
	// 凭据值会出现在 CAS 条件和更新参数中，不能进入 GORM 的错误 SQL 日志。
	err := DB.WithContext(nonNilContext(ctx)).Session(&gorm.Session{Logger: DB.Logger.LogMode(gormlogger.Silent)}).Transaction(func(tx *gorm.DB) error {
		var channel Channel
		conditions := "id = ? AND type = ? AND credential_revision = ? AND credential_refresh_fence = ?"
		args := []any{expected.ChannelID, config.ChannelTypeCodex, expected.ExpectedRevision, expected.ExpectedFence}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(conditions, args...).First(&channel).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrChannelCredentialConflict
			}
			return err
		}
		if channel.CredentialRefreshFence == nil || *channel.CredentialRefreshFence != expected.ExpectedFence || CodexCredentialAccountID(channel.Key) != expected.AccountID {
			return ErrChannelCredentialConflict
		}
		result := tx.Model(&Channel{}).Where(conditions, args...).Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: channel.Key}).Updates(map[string]any{
			"key": newKey, "credential_revision": gorm.Expr("credential_revision + 1"),
			"credential_refresh_fence": nil, "credential_refresh_started_at": nil,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrChannelCredentialConflict
		}
		return nil
	})
	if err == nil {
		ChannelGroup.failClosedChannels([]int{expected.ChannelID})
	}
	return err
}

// ReplaceChannelCredentialWithContext 只供已验证同账号的服务端授权结果调用。
// ticket 必须在授权码交换前取得，普通刷新和重新授权使用同一执行 fence。
func ReplaceChannelCredentialWithContext(ctx context.Context, ticket CredentialRotationTicket, newKey string) error {
	outcome, err := CommitCredentialRotation(ctx, ticket, newKey)
	if err != nil {
		return err
	}
	if outcome != CredentialRotationCommitApplied && outcome != CredentialRotationCommitAlreadyApplied {
		return ErrChannelCredentialConflict
	}
	return nil
}

type CredentialRotationCommitOutcome int

const (
	CredentialRotationCommitApplied CredentialRotationCommitOutcome = iota
	CredentialRotationCommitAlreadyApplied
	CredentialRotationCommitSuperseded
	CredentialRotationCommitStillFenced
)

func LoadCredentialRotationSnapshot(ctx context.Context, channelID int) (CredentialRotationSnapshot, error) {
	ctx = nonNilContext(ctx)
	if DB == nil {
		return CredentialRotationSnapshot{}, fmt.Errorf("database is not initialized")
	}
	var row Channel
	err := DB.WithContext(ctx).Unscoped().Select("id", "type", "key", "credential_revision", "credential_refresh_fence", "credential_refresh_started_at", "deleted_at").Where("id = ?", channelID).First(&row).Error
	if err != nil {
		return CredentialRotationSnapshot{}, err
	}
	return CredentialRotationSnapshot{
		ChannelID: row.Id, Type: row.Type, Key: row.Key, Revision: row.CredentialRevision,
		Fence: row.CredentialRefreshFence, StartedAt: row.CredentialRefreshStartedAt,
		Deleted: row.DeletedAt.Valid,
	}, nil
}

func ClaimCredentialRotation(ctx context.Context, ticket CredentialRotationTicket, startedAt time.Time) (CredentialRotationClaimOutcome, error) {
	ctx = nonNilContext(ctx)
	if DB == nil {
		return CredentialRotationClaimSuperseded, fmt.Errorf("database is not initialized")
	}
	if ticket.ChannelID <= 0 || strings.TrimSpace(ticket.AttemptID) == "" {
		return CredentialRotationClaimSuperseded, fmt.Errorf("invalid credential rotation ticket")
	}
	started := startedAt.Unix()
	result := DB.WithContext(ctx).Model(&Channel{}).
		Where("id = ? AND type = ? AND credential_revision = ? AND credential_refresh_fence IS NULL", ticket.ChannelID, config.ChannelTypeCodex, ticket.ExpectedRevision).
		Updates(map[string]any{"credential_refresh_fence": ticket.AttemptID, "credential_refresh_started_at": started})
	if result.Error == nil && result.RowsAffected == 1 {
		return CredentialRotationClaimAcquired, nil
	}

	// Both a zero-row CAS and an ambiguous DB response are classified from the
	// authoritative row.  A lost success response is safe only when the complete
	// ticket identity is visible.
	snapshot, reloadErr := LoadCredentialRotationSnapshot(ctx, ticket.ChannelID)
	if reloadErr != nil {
		if result.Error != nil {
			return CredentialRotationClaimSuperseded, errors.Join(result.Error, reloadErr)
		}
		return CredentialRotationClaimSuperseded, reloadErr
	}
	if !snapshot.Deleted && snapshot.Type == config.ChannelTypeCodex && snapshot.Revision == ticket.ExpectedRevision {
		if snapshot.Fence != nil && *snapshot.Fence == ticket.AttemptID {
			return CredentialRotationClaimAcquired, nil
		}
		if snapshot.Fence != nil {
			return CredentialRotationClaimBusy, result.Error
		}
	}
	return CredentialRotationClaimSuperseded, result.Error
}

func CommitCredentialRotation(ctx context.Context, ticket CredentialRotationTicket, rotatedKey string) (CredentialRotationCommitOutcome, error) {
	ctx = nonNilContext(ctx)
	if DB == nil {
		return CredentialRotationCommitStillFenced, fmt.Errorf("database is not initialized")
	}
	if strings.TrimSpace(rotatedKey) == "" {
		return CredentialRotationCommitSuperseded, fmt.Errorf("rotated credential is empty")
	}
	query := DB.WithContext(ctx).Session(&gorm.Session{Logger: DB.Logger.LogMode(gormlogger.Silent)}).Model(&Channel{}).
		Where("id = ? AND type = ? AND credential_revision = ?", ticket.ChannelID, config.ChannelTypeCodex, ticket.ExpectedRevision)
	query = query.Where("credential_refresh_fence = ?", ticket.AttemptID)
	result := query.Updates(map[string]any{
		"key": rotatedKey, "credential_revision": gorm.Expr("credential_revision + 1"),
		"credential_refresh_fence": nil, "credential_refresh_started_at": nil,
	})
	if result.Error == nil && result.RowsAffected == 1 {
		ChannelGroup.failClosedChannels([]int{ticket.ChannelID})
		return CredentialRotationCommitApplied, nil
	}
	snapshot, reloadErr := LoadCredentialRotationSnapshot(ctx, ticket.ChannelID)
	if reloadErr != nil {
		if result.Error != nil {
			return CredentialRotationCommitStillFenced, errors.Join(result.Error, reloadErr)
		}
		return CredentialRotationCommitStillFenced, reloadErr
	}
	if !snapshot.Deleted && snapshot.Type == config.ChannelTypeCodex && snapshot.Revision == ticket.ExpectedRevision+1 && snapshot.Fence == nil && snapshot.Key == rotatedKey {
		ChannelGroup.failClosedChannels([]int{ticket.ChannelID})
		return CredentialRotationCommitAlreadyApplied, nil
	}
	if !snapshot.Deleted && snapshot.Type == config.ChannelTypeCodex && snapshot.Revision == ticket.ExpectedRevision && snapshot.Fence != nil && *snapshot.Fence == ticket.AttemptID {
		return CredentialRotationCommitStillFenced, result.Error
	}
	return CredentialRotationCommitSuperseded, result.Error
}

func CancelCredentialRotationBeforeDispatch(ctx context.Context, ticket CredentialRotationTicket) (bool, error) {
	ctx = nonNilContext(ctx)
	if DB == nil {
		return false, fmt.Errorf("database is not initialized")
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		result := DB.WithContext(ctx).Model(&Channel{}).
			Where("id = ? AND type = ? AND credential_revision = ? AND credential_refresh_fence = ?", ticket.ChannelID, config.ChannelTypeCodex, ticket.ExpectedRevision, ticket.AttemptID).
			Updates(map[string]any{"credential_refresh_fence": nil, "credential_refresh_started_at": nil})
		lastErr = result.Error
		snapshot, readErr := LoadCredentialRotationSnapshot(ctx, ticket.ChannelID)
		if readErr == nil {
			if !snapshot.Deleted && snapshot.Type == config.ChannelTypeCodex && snapshot.Revision == ticket.ExpectedRevision && snapshot.Fence == nil {
				return true, nil
			}
			if snapshot.Deleted || snapshot.Type != config.ChannelTypeCodex || snapshot.Revision != ticket.ExpectedRevision || snapshot.Fence == nil || *snapshot.Fence != ticket.AttemptID {
				return false, lastErr
			}
		}
		if lastErr == nil && readErr == nil {
			lastErr = errors.New("credential refresh fence remained after cancellation CAS")
		} else {
			lastErr = errors.Join(lastErr, readErr)
		}
		if attempt == 2 || ctx.Err() != nil || !IsRetryableDatabaseError(lastErr) {
			return false, lastErr
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
	return false, lastErr
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
