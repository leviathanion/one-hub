package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"one-api/common/config"

	"gorm.io/gorm"
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

type CredentialRotationCommitOutcome int

const (
	CredentialRotationCommitApplied CredentialRotationCommitOutcome = iota
	CredentialRotationCommitAlreadyApplied
	CredentialRotationCommitSuperseded
	CredentialRotationCommitStillFenced
)

var ErrSameCredentialCannotResolveRefreshFence = errors.New("a new credential is required to resolve the refresh fence")

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
	result := DB.WithContext(ctx).Model(&Channel{}).
		Where("id = ? AND type = ? AND credential_revision = ? AND credential_refresh_fence = ?", ticket.ChannelID, config.ChannelTypeCodex, ticket.ExpectedRevision, ticket.AttemptID).
		Updates(map[string]any{
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
	result := DB.WithContext(ctx).Model(&Channel{}).
		Where("id = ? AND type = ? AND credential_revision = ? AND credential_refresh_fence = ?", ticket.ChannelID, config.ChannelTypeCodex, ticket.ExpectedRevision, ticket.AttemptID).
		Updates(map[string]any{"credential_refresh_fence": nil, "credential_refresh_started_at": nil})
	return result.RowsAffected == 1, result.Error
}

// ReplaceChannelCredentialWithContext is the only administrative fence recovery
// operation. Repeating the old bytes cannot prove that the upstream one-time
// token was replaced, so it intentionally leaves the channel blocked.
func ReplaceChannelCredentialWithContext(ctx context.Context, channelID int, newKey string) (bool, error) {
	ctx = nonNilContext(ctx)
	if DB == nil {
		return false, fmt.Errorf("database is not initialized")
	}
	var updated bool
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row Channel
		if err := tx.Select("id", "key", "credential_revision", "credential_refresh_fence").Where("id = ?", channelID).First(&row).Error; err != nil {
			return err
		}
		if row.Key == newKey {
			if row.CredentialRefreshFence != nil {
				return ErrSameCredentialCannotResolveRefreshFence
			}
			return nil
		}
		result := tx.Model(&Channel{}).Where("id = ? AND credential_revision = ?", channelID, row.CredentialRevision).Updates(map[string]any{
			"key": newKey, "credential_revision": gorm.Expr("credential_revision + 1"),
			"credential_refresh_fence": nil, "credential_refresh_started_at": nil,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("channel credential lifecycle changed concurrently")
		}
		updated = true
		return nil
	})
	return updated, err
}

// RestoreChannelWithCredentialWithContext starts a new lifecycle incarnation.
// A deleted row that carries an unresolved fence can only be restored with new
// credential bytes; otherwise the old possibly-consumed token would become
// refreshable again.
func RestoreChannelWithCredentialWithContext(ctx context.Context, channelID int, newKey string) (bool, error) {
	ctx = nonNilContext(ctx)
	if DB == nil {
		return false, fmt.Errorf("database is not initialized")
	}
	var restored bool
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row Channel
		if err := tx.Unscoped().Select("id", "key", "credential_revision", "credential_refresh_fence", "deleted_at").Where("id = ?", channelID).First(&row).Error; err != nil {
			return err
		}
		if !row.DeletedAt.Valid {
			return nil
		}
		if row.CredentialRefreshFence != nil && row.Key == newKey {
			return ErrSameCredentialCannotResolveRefreshFence
		}
		result := tx.Unscoped().Model(&Channel{}).
			Where("id = ? AND credential_revision = ? AND deleted_at IS NOT NULL", channelID, row.CredentialRevision).
			Updates(map[string]any{
				"deleted_at": nil, "key": newKey,
				"credential_revision":      gorm.Expr("credential_revision + 1"),
				"credential_refresh_fence": nil, "credential_refresh_started_at": nil,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("channel lifecycle changed concurrently")
		}
		restored = true
		return nil
	})
	return restored, err
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
