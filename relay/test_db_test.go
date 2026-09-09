package relay

import (
	"testing"

	"one-api/internal/testutil/sqlitetest"
	"one-api/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupRelayTestDB(t *testing.T, models ...any) *gorm.DB {
	t.Helper()
	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	// 消费结算同时读取分组规则。
	if err := testDB.AutoMigrate(&model.UserGroup{}); err != nil {
		t.Fatal(err)
	}
	if len(models) > 0 {
		if err := testDB.AutoMigrate(models...); err != nil {
			t.Fatalf("expected test database schema migration, got %v", err)
		}
	}
	originalDB := model.DB
	model.GlobalUserGroupRatio.Lock()
	originalGroups := model.GlobalUserGroupRatio.UserGroup
	groups := make(map[string]*model.UserGroup, len(originalGroups)+1)
	for key, value := range originalGroups {
		groups[key] = value
	}
	groups["default"] = &model.UserGroup{Symbol: "default", Ratio: 1}
	model.GlobalUserGroupRatio.UserGroup = groups
	model.GlobalUserGroupRatio.Unlock()
	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.UserGroup = originalGroups
		model.GlobalUserGroupRatio.Unlock()
		if sqlDB, dbErr := testDB.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	return testDB
}
