package model

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// 先以可空列接住历史数据，回填成功后由正式模型建立最终约束。
// 只增加缺失列，不改变已经上线的列定义；DDL 中断后可重跑。
func addStartupMigrationColumns(db *gorm.DB, table string, columns any) error {
	names, err := databaseColumnNames(db, table)
	if err != nil || names == nil {
		return err
	}
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(columns); err != nil {
		return err
	}
	for _, field := range stmt.Schema.Fields {
		if field.DBName == "" || names[projectionIdentifierKey(db.Dialector.Name(), field.DBName)] {
			continue
		}
		if err := db.Table(table).Migrator().AddColumn(columns, field.Name); err != nil {
			return fmt.Errorf("启动迁移增加 %s.%s 失败: %w", table, field.DBName, err)
		}
	}
	return nil
}

func startupMigrationRollback(*gorm.DB) error {
	return fmt.Errorf("历史数据已转换；回滚须停止写入并恢复匹配的数据库备份和程序")
}

func migrateIdenticalPrices() *gormigrate.Migration {
	return &gormigrate.Migration{ID: "202609090006", Migrate: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&Price{}) {
			return nil
		}
		return db.Transaction(func(tx *gorm.DB) error {
			var prices []Price
			if err := tx.Find(&prices).Error; err != nil {
				return err
			}
			// 价格内容先校验，规范化后重名但原始名称不同的记录不能静默覆盖。
			seen := make(map[string]string)
			for _, price := range prices {
				original := price.Model
				if err := price.prepareForPersistence(); err != nil {
					return fmt.Errorf("历史价格 %q 无效: %w", original, err)
				}
				if previous, ok := seen[price.Model]; ok && previous != original {
					return fmt.Errorf("历史价格 %q 规范化后重名，请核对价格后再启动", price.Model)
				}
				seen[price.Model] = original
			}
			var duplicates []string
			if err := tx.Table("prices").Select("model").Group("model").Having("COUNT(*) > 1").Scan(&duplicates).Error; err != nil {
				return err
			}
			for _, name := range duplicates {
				var rows []map[string]any
				if err := tx.Table("prices").Where("model = ?", name).Find(&rows).Error; err != nil {
					return err
				}
				if len(rows) < 2 {
					continue
				}
				for _, row := range rows[1:] {
					if !reflect.DeepEqual(rows[0], row) {
						return fmt.Errorf("历史价格 %q 存在内容不同的重复记录，请核对后再启动", name)
					}
				}
				if err := tx.Table("prices").Where("model = ?", name).Delete(&Price{}).Error; err != nil {
					return err
				}
				// 连同未知旧列一起保留，避免仅按当前模型重建而损失历史信息。
				if err := tx.Table("prices").Create(rows[0]).Error; err != nil {
					return err
				}
			}
			return nil
		})
	}, Rollback: startupMigrationRollback}
}

func startupRowError(table string, id int64, reason string) error {
	return fmt.Errorf("%s 记录 %d 无法自动迁移：%s；请核查历史事实后重新启动", table, id, strings.TrimSpace(reason))
}
