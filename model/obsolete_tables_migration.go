package model

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func dropObsoleteTablesAfterMigration() *gormigrate.Migration {
	return &gormigrate.Migration{ID: "202609090009", Migrate: func(db *gorm.DB) error {
		// 新 ID 覆盖已执行过导入但仍保留旧表的部署。重新核对每条目标记录，
		// 不把迁移版本号本身当作数据完整性的证明。
		columns, err := databaseColumnNames(db, "midjourneys")
		if err != nil {
			return err
		}
		if columns != nil {
			stmt := &gorm.Statement{DB: db}
			if err := stmt.Parse(&Midjourney{}); err != nil {
				return err
			}
			for column := range columns {
				if stmt.Schema.FieldsByDBName[column] == nil {
					return fmt.Errorf("midjourneys.%s 尚未纳入数据迁移，保留旧表并停止清理", column)
				}
			}
			if err := migrateHistoricalMidjourney().Migrate(db); err != nil {
				return err
			}
		}
		// 数据事务先提交，再执行 DDL；MySQL DROP TABLE 不能依赖事务回滚。
		// 只清理明确废弃的表。abilities 是旧路由派生数据，现由 channels 构建。
		for _, table := range []string{"midjourneys", "abilities"} {
			if !db.Migrator().HasTable(table) {
				continue
			}
			if err := db.Exec("DROP TABLE ?", clause.Table{Name: table}).Error; err != nil {
				return fmt.Errorf("清理旧表 %s 失败，可修复原因后重启继续: %w", table, err)
			}
		}
		return nil
	}, Rollback: startupMigrationRollback}
}
