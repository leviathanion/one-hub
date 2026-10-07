package model

import (
	"path/filepath"
	"testing"

	"one-api/common/config"

	"github.com/spf13/viper"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPaymentUpgradePreservesHistoricalBalancesWithoutCumulativeColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "historical.db")
	historical, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := historical.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := historical.Exec("CREATE TABLE users (id INTEGER PRIMARY KEY, password TEXT NOT NULL, quota INTEGER, used_quota INTEGER)").Error; err != nil {
		t.Fatal(err)
	}
	if err := historical.Exec("INSERT INTO users (id, password, quota, used_quota) VALUES (1, 'fixture', 321, 123)").Error; err != nil {
		t.Fatal(err)
	}
	// 本测试只验证支付历史升级，目录已由停机迁移转换为目标结构。
	if err := historical.AutoMigrate(&Price{}, &ModelOwnedBy{}, &ModelInfo{}); err != nil {
		t.Fatal(err)
	}
	previousDB, previousMaster := DB, config.IsMasterNode
	viper.Reset()
	viper.Set("sqlite_path", path)
	config.IsMasterNode = true
	t.Cleanup(func() {
		if DB != nil && DB != previousDB {
			if current, err := DB.DB(); err == nil {
				_ = current.Close()
			}
		}
		DB, config.IsMasterNode = previousDB, previousMaster
		viper.Reset()
	})
	if err := InitDB(); err != nil {
		t.Fatalf("已有余额和消费字段应能直接升级: %v", err)
	}
	if historical.Migrator().HasColumn(&User{}, "recharged_quota") {
		t.Fatal("迁移不应新增累计字段")
	}
	var row struct{ Quota, UsedQuota int }
	if err := historical.Table("users").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Quota != 321 || row.UsedQuota != 123 {
		t.Fatalf("preflight changed historical money: %+v", row)
	}
}
