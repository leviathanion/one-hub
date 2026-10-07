package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"one-api/common/logger"
	"one-api/internal/catalogmigration"
	"one-api/internal/catalogschema"

	"gorm.io/gorm"
)

// InitializeModelCatalogSchema runs before any general migration, seed or worker.
// Only the master initializes an empty database or upgrades a complete old catalog.
func InitializeModelCatalogSchema(db *gorm.DB, master bool) error {
	empty, err := catalogschema.IsEmpty(db)
	if err != nil {
		return err
	}
	if empty {
		if !master {
			return errors.New("模型目录尚未初始化，请先启动主实例完成建库")
		}
		return nil
	}
	if err := catalogschema.Validate(db); err == nil {
		return nil
	} else if !master {
		return fmt.Errorf("副实例不执行目录迁移，请等待主实例完成升级: %w", err)
	}
	report, err := catalogmigration.Run(db, true)
	if errors.Is(err, catalogmigration.ErrAlreadyMigrated) {
		return catalogschema.Validate(db)
	}
	if report.Applied || len(report.Conflicts) > 0 || len(report.Dangling) > 0 {
		// The report contains only counts and model identities, never channel credentials.
		encoded, _ := json.Marshal(report)
		logger.SysLog("模型目录迁移: " + string(encoded))
	}
	if err != nil {
		return fmt.Errorf("模型目录启动迁移失败: %w", err)
	}
	return catalogschema.Validate(db)
}

func ValidateModelCatalogSchema(db *gorm.DB) error {
	return catalogschema.Validate(db)
}

func EnsureModelInfoIdentitySchema(db *gorm.DB) error {
	return catalogschema.EnsureIdentity(db)
}
