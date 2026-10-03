package model

import (
	"fmt"
	"one-api/common/credentials"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// This is an offline, one-way migration, never a runtime compatibility path.
// Backfill commits before destructive DDL so interrupted MySQL DDL can resume.
func migrateChannelBusinessData() *gormigrate.Migration {
	return &gormigrate.Migration{ID: "202610030001", Migrate: func(db *gorm.DB) error {
		names, err := databaseColumnNames(db, "channels")
		if err != nil || names == nil {
			return err
		}
		columns := []string{"credential_revision", "credential_refresh_fence", "credential_refresh_started_at"}
		present := func(name string) bool { return names[projectionIdentifierKey(db.Dialector.Name(), name)] }
		if err := addStartupMigrationColumns(db, "channels", &struct {
			Version uint64         `gorm:"not null;default:0"`
			BizData datatypes.JSON `gorm:"column:bizdata;type:json"`
		}{}); err != nil {
			return err
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			selection := []string{"id", "version", "bizdata"}
			for _, column := range columns {
				if present(column) {
					selection = append(selection, column)
				}
			}
			lastID := 0
			for {
				var rows []struct {
					ID                         int
					Version                    uint64
					BizData                    datatypes.JSON `gorm:"column:bizdata"`
					CredentialRevision         uint64
					CredentialRefreshFence     *string
					CredentialRefreshStartedAt *int64
				}
				if err := tx.Table("channels").Select(selection).Where("id > ?", lastID).Order("id").Limit(200).Find(&rows).Error; err != nil {
					return err
				}
				if len(rows) == 0 {
					break
				}
				for _, row := range rows {
					lastID = row.ID
					refresh, err := credentials.ReadRefresh(row.BizData)
					if err != nil {
						return fmt.Errorf("channel %d: %w", row.ID, err)
					}
					if row.CredentialRefreshFence != nil {
						if refresh != nil && refresh.AttemptID != *row.CredentialRefreshFence {
							return fmt.Errorf("channel %d: conflicting refresh state", row.ID)
						}
						if refresh == nil {
							refresh = &credentials.Refresh{AttemptID: *row.CredentialRefreshFence}
						}
						if row.CredentialRefreshStartedAt != nil {
							refresh.StartedAt = *row.CredentialRefreshStartedAt
						}
					}
					data, err := credentials.WriteRefresh(row.BizData, refresh)
					if err != nil {
						return fmt.Errorf("channel %d: %w", row.ID, err)
					}
					version := max(row.Version, row.CredentialRevision)
					if err := tx.Table("channels").Where("id = ?", row.ID).Updates(map[string]any{"version": version, "bizdata": datatypes.JSON(data)}).Error; err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
		// Timestamp first, then fence, then revision: after any interrupted DDL,
		// the already committed JSON remains the authoritative migrated state.
		for _, column := range []string{columns[2], columns[1], columns[0]} {
			if present(column) {
				if err := db.Exec("ALTER TABLE ? DROP COLUMN ?", clause.Table{Name: "channels"}, clause.Column{Name: column}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	}, Rollback: startupMigrationRollback}
}
