package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	commonredis "one-api/common/redis"
	"one-api/internal/testutil/fakeredis"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestTokenGroupPermissionsUseSQLAndReuseOnePrincipalRead(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.UserGroup{}, &model.PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	oldDB, oldGroups := model.DB, model.GlobalUserGroupRatio
	model.DB, model.GlobalUserGroupRatio = db, &model.UserGroupRatio{}
	t.Cleanup(func() { model.DB, model.GlobalUserGroupRatio = oldDB, oldGroups; _ = sqlDB.Close() })
	if err := model.EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	for _, group := range []model.UserGroup{{Symbol: "old", Ratio: 1}, {Symbol: "paid", Ratio: 2}, {Symbol: "public", Public: true, Ratio: 3}} {
		if err := db.Create(&group).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.User{Id: 1, Username: "group-user", Group: "paid"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.GlobalUserGroupRatio.Load(); err != nil {
		t.Fatal(err)
	}
	server, err := fakeredis.Start()
	if err != nil {
		t.Fatal(err)
	}
	oldRedisEnabled, oldRedis := config.RedisEnabled, commonredis.RDB
	config.RedisEnabled, commonredis.RDB = true, server.Client()
	cache.InitCacheManager()
	t.Cleanup(func() {
		_ = commonredis.RDB.Close()
		_ = server.Close()
		config.RedisEnabled, commonredis.RDB = oldRedisEnabled, oldRedis
		cache.InitCacheManager()
	})
	if err := cache.SetCache(fmt.Sprintf("user_group:%d", 1), "old", time.Minute); err != nil {
		t.Fatal(err)
	}
	reads := 0
	const callback = "test:token-group-authoritative-read"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			reads++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateTokenGroups(context.Background(), 1, "paid", "public"); err != nil {
		t.Fatalf("SQL新权益被旧缓存拒绝: %v", err)
	}
	if reads != 1 {
		t.Fatalf("同一权限决策读取用户 %d 次", reads)
	}
	_ = db.Callback().Query().Remove(callback)
	if err := validateTokenGroups(context.Background(), 1, "old"); err == nil {
		t.Fatal("旧缓存授予不再拥有的私有分组")
	}

	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.AddError(errors.New("injected SQL failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateTokenGroups(context.Background(), 1, "public"); err == nil {
		t.Fatal("SQL失败仍用缓存或公开组放行")
	}
	_ = db.Callback().Query().Remove(callback)
	if err := db.Migrator().DropTable(&model.PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := validateTokenGroups(context.Background(), 1, "paid"); err == nil {
		t.Fatal("策略不可用仍允许授权")
	}
}
