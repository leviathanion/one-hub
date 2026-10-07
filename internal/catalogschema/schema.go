// Package catalogschema defines the target model catalog identity constraints.
package catalogschema

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const ModelIndex = "idx_model_info_model"

// Validate permits an empty database, but never upgrades an existing database.
func Validate(db *gorm.DB) error {
	empty, err := IsEmpty(db)
	if err != nil {
		return err
	}
	if empty {
		return nil
	}
	if !db.Migrator().HasTable("model_info") || !db.Migrator().HasTable("prices") || !db.Migrator().HasTable("model_owned_by") ||
		!db.Migrator().HasColumn("model_info", "owned_by_id") || db.Migrator().HasColumn("prices", "channel_type") {
		return fmt.Errorf("模型目录结构不是目标版本；请由主实例在服务启动前迁移；若已发生部分迁移，请先恢复备份")
	}
	if err := ValidateIdentity(db); err != nil {
		return fmt.Errorf("模型目录结构不完整，请恢复备份并重新启动主实例: %w", err)
	}
	return nil
}

// IsEmpty distinguishes a new installation from an incomplete existing schema.
func IsEmpty(db *gorm.DB) (bool, error) {
	tables, err := db.Migrator().GetTables()
	if err != nil {
		return false, fmt.Errorf("读取目录结构失败: %w", err)
	}
	for _, table := range tables {
		if !strings.HasPrefix(table, "sqlite_") {
			return false, nil
		}
	}
	return true, nil
}

func ValidateIdentity(db *gorm.DB) error {
	valid, err := validIdentityIndex(db)
	if err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("缺少模型名唯一约束 %s", ModelIndex)
	}
	if db.Dialector.Name() == "mysql" {
		var collation string
		if err := db.Raw("SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'model_info' AND COLUMN_NAME = 'model'").Scan(&collation).Error; err != nil {
			return err
		}
		if collation != "utf8mb4_bin" {
			return fmt.Errorf("model_info.model 必须使用 utf8mb4_bin 排序规则")
		}
	}
	return nil
}

// EnsureIdentity is called during new database initialization or catalog migration.
func EnsureIdentity(db *gorm.DB) error {
	if db.Dialector.Name() == "mysql" {
		var collation string
		if err := db.Raw("SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'model_info' AND COLUMN_NAME = 'model'").Scan(&collation).Error; err != nil {
			return err
		}
		if collation != "utf8mb4_bin" {
			if err := db.Exec("ALTER TABLE model_info MODIFY COLUMN model varchar(100) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin").Error; err != nil {
				return err
			}
		}
	}
	if db.Migrator().HasIndex("model_info", ModelIndex) {
		valid, err := validIdentityIndex(db)
		if err != nil {
			return err
		}
		if !valid {
			if err := db.Migrator().DropIndex("model_info", ModelIndex); err != nil {
				return err
			}
		}
	}
	if !db.Migrator().HasIndex("model_info", ModelIndex) {
		if err := db.Exec("CREATE UNIQUE INDEX idx_model_info_model ON model_info (model)").Error; err != nil {
			return err
		}
	}
	return ValidateIdentity(db)
}

func validIdentityIndex(db *gorm.DB) (bool, error) {
	// Inspect the actual constraint, including partial/prefix indexes: its name
	// alone does not guarantee uniqueness for every complete model name.
	switch db.Dialector.Name() {
	case "sqlite":
		var indexes []struct {
			Name    string
			Unique  bool
			Partial bool
		}
		if err := db.Raw("PRAGMA index_list('model_info')").Scan(&indexes).Error; err != nil {
			return false, err
		}
		for _, index := range indexes {
			if index.Name == ModelIndex && index.Unique && !index.Partial {
				var columns []struct {
					Name string
					Coll string
					Key  bool
				}
				if err := db.Raw("PRAGMA index_xinfo('idx_model_info_model')").Scan(&columns).Error; err != nil {
					return false, err
				}
				keys := 0
				valid := true
				for _, column := range columns {
					if column.Key {
						keys++
						valid = valid && column.Name == "model" && strings.EqualFold(column.Coll, "BINARY")
					}
				}
				return valid && keys == 1, nil
			}
		}
		return false, nil
	case "mysql":
		var columns []struct {
			ColumnName string
			NonUnique  int
			SubPart    *int
		}
		if err := db.Raw("SELECT COLUMN_NAME AS column_name, NON_UNIQUE AS non_unique, SUB_PART AS sub_part FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'model_info' AND INDEX_NAME = ? ORDER BY SEQ_IN_INDEX", ModelIndex).Scan(&columns).Error; err != nil {
			return false, err
		}
		return len(columns) == 1 && columns[0].ColumnName == "model" && columns[0].NonUnique == 0 && columns[0].SubPart == nil, nil
	case "postgres":
		var valid bool
		err := db.Raw(`SELECT EXISTS (
			SELECT 1 FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
			WHERE i.indrelid = 'model_info'::regclass AND c.relname = ?
			AND i.indisunique AND i.indisvalid AND i.indnkeyatts = 1
			AND i.indpred IS NULL AND i.indexprs IS NULL AND a.attname = 'model'
		)`, ModelIndex).Scan(&valid).Error
		return valid, err
	default:
		return false, fmt.Errorf("不支持的数据库类型 %s", db.Dialector.Name())
	}
}
