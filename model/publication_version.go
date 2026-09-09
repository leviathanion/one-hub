package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const publicationCommitProbeDeadline = 2 * time.Second

func publicationCommitProbeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), publicationCommitProbeDeadline)
}

func lockPublicationReload(ctx context.Context, mutex *sync.Mutex) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if mutex.TryLock() {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if mutex.TryLock() {
				return nil
			}
		}
	}
}

const (
	PublicationOwnerPrice     = "price"
	PublicationOwnerOptions   = "options"
	PublicationOwnerUserGroup = "user_group"
)

var ErrPublicationVersionConflict = errors.New("publication version conflict")

// PublicationVersion 保存价格、运行选项和用户组策略各自的 SQL 发布版本。
type PublicationVersion struct {
	Owner   string `gorm:"primaryKey;size:32"`
	Version int64  `gorm:"not null"`
}

func EnsurePublicationVersionRows(tx *gorm.DB) error {
	if tx == nil {
		return errors.New("database is required")
	}
	for _, owner := range []string{PublicationOwnerPrice, PublicationOwnerOptions, PublicationOwnerUserGroup} {
		row := PublicationVersion{Owner: owner, Version: 1}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		version, err := ReadPublicationVersion(context.Background(), tx, owner)
		if err != nil {
			return err
		}
		if version < 1 {
			return fmt.Errorf("publication owner %s has invalid version %d", owner, version)
		}
	}
	return nil
}

func ReadPublicationVersion(ctx context.Context, tx *gorm.DB, owner string) (int64, error) {
	if tx == nil {
		return 0, errors.New("database is required")
	}
	if owner != PublicationOwnerPrice && owner != PublicationOwnerOptions && owner != PublicationOwnerUserGroup {
		return 0, fmt.Errorf("unknown publication owner %q", owner)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var row PublicationVersion
	if err := tx.WithContext(ctx).Where("owner = ?", owner).Take(&row).Error; err != nil {
		return 0, err
	}
	if row.Version < 1 {
		return 0, fmt.Errorf("publication owner %s has invalid version %d", owner, row.Version)
	}
	return row.Version, nil
}

func BumpPublicationVersionCAS(ctx context.Context, tx *gorm.DB, owner string, expected int64) (int64, error) {
	if tx == nil {
		return 0, errors.New("database is required")
	}
	if expected < 1 {
		return 0, fmt.Errorf("expected publication version must be positive")
	}
	if owner != PublicationOwnerPrice && owner != PublicationOwnerOptions && owner != PublicationOwnerUserGroup {
		return 0, fmt.Errorf("unknown publication owner %q", owner)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result := tx.WithContext(ctx).Model(&PublicationVersion{}).
		Where("owner = ? AND version = ?", owner, expected).
		Update("version", gorm.Expr("version + 1"))
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected != 1 {
		return 0, ErrPublicationVersionConflict
	}
	return expected + 1, nil
}

func CheckPublicationVersionSchema(ctx context.Context) error {
	if DB == nil {
		return errors.New("database is required")
	}
	migrator := DB.WithContext(ctx).Migrator()
	if !migrator.HasTable(&PublicationVersion{}) {
		return errors.New("publication_versions table is missing")
	}
	for _, owner := range []string{PublicationOwnerPrice, PublicationOwnerOptions, PublicationOwnerUserGroup} {
		if _, err := ReadPublicationVersion(ctx, DB, owner); err != nil {
			return fmt.Errorf("publication owner %s is unavailable: %w", owner, err)
		}
	}
	return nil
}
