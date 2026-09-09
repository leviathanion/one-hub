package model

import (
	"context"
	"testing"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"one-api/internal/testutil/sqlitetest"
)

func TestRetiredRuntimeOptionsStartupMigrationRunsOnce(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	rows := []Option{
		{Key: "ChatCacheEnabled", Value: "true"},
		{Key: "ChatCacheExpireMinute", Value: "30"},
		{Key: "ClaudeBudgetTokensPercentage", Value: "0.8"},
		{Key: "ClaudeDefaultMaxTokens", Value: `{"default":8192}`},
		{Key: "SystemName", Value: "保留站点名称"},
		{Key: "UnknownFutureOption", Value: "保留未确认废弃的配置"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	var selected []*gormigrate.Migration
	for _, migration := range afterAutoMigrateMigrations() {
		if migration.ID == "202609100001" {
			selected = append(selected, migration)
		}
	}
	if len(selected) != 1 {
		t.Fatalf("启动迁移应注册一次清理，实际 %d 次", len(selected))
	}
	runner := gormigrate.New(db, gormigrate.DefaultOptions, selected)
	if err := runner.Migrate(); err != nil {
		t.Fatal(err)
	}
	var remaining []Option
	if err := db.Order("key").Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0] != rows[4] || remaining[1] != rows[5] {
		t.Fatalf("应仅清理四个已废弃配置，剩余：%+v", remaining)
	}
	version, err := ReadPublicationVersion(context.Background(), db, PublicationOwnerOptions)
	if err != nil || version != 2 {
		t.Fatalf("清理后应发布版本 2，实际 %d，错误 %v", version, err)
	}
	// 回插旧键作为探针，确认后续启动跳过已完成迁移，而非每次重新清理。
	if err := db.Create(&rows[0]).Error; err != nil {
		t.Fatal(err)
	}
	if err := runner.Migrate(); err != nil {
		t.Fatal(err)
	}
	var retained Option
	if err := db.Where("key = ?", rows[0].Key).Take(&retained).Error; err != nil {
		t.Fatalf("已完成的清理迁移被重复执行：%v", err)
	}
	version, err = ReadPublicationVersion(context.Background(), db, PublicationOwnerOptions)
	if err != nil || version != 2 {
		t.Fatalf("后续启动不应推进配置版本，实际 %d，错误 %v", version, err)
	}
}
