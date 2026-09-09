package model

import (
	"testing"

	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestUserInsertReadsCurrentOptionsAtEachRewardDecision(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("打开测试数据库：%v", err)
	}
	if err := db.AutoMigrate(&User{}, &Log{}, &UserGroup{}); err != nil {
		t.Fatalf("迁移测试表：%v", err)
	}

	originalDB := DB
	originalManager := config.GlobalOption
	originalBatchUpdate := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
	DB = db
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	t.Cleanup(func() {
		DB = originalDB
		config.GlobalOption = originalManager
		config.BatchUpdateEnabled = originalBatchUpdate
		config.RedisEnabled = originalRedisEnabled
	})

	manager := config.NewOptionManager()
	newUserQuota := 0
	inviteeQuota := 0
	inviterQuota := 0
	manager.RegisterInt("QuotaForNewUser", &newUserQuota)
	manager.RegisterInt("QuotaForInvitee", &inviteeQuota)
	manager.RegisterInt("QuotaForInviter", &inviterQuota)
	config.GlobalOption = manager
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{
		"QuotaForNewUser": "10",
		"QuotaForInvitee": "2",
		"QuotaForInviter": "3",
	}); err != nil {
		t.Fatalf("发布初始配置：%v", err)
	}

	inviter := &User{Username: "inviter", Quota: 100, AccessToken: "inviter-token", AffCode: "invite"}
	if err := db.Create(inviter).Error; err != nil {
		t.Fatalf("创建邀请人：%v", err)
	}

	if err := db.Callback().Create().After("gorm:create").Register("test:publish-user-reward-options", func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Name != "User" {
			return
		}
		_, _ = manager.PublishRuntimeOverrides(2, map[string]string{
			"QuotaForNewUser": "20",
			"QuotaForInvitee": "4",
			"QuotaForInviter": "5",
		})
	}); err != nil {
		t.Fatalf("注册配置更新回调：%v", err)
	}

	invitee := &User{Username: "invitee"}
	if err := invitee.Insert(inviter.Id); err != nil {
		t.Fatalf("插入受邀用户：%v", err)
	}

	var storedInvitee User
	if err := db.First(&storedInvitee, invitee.Id).Error; err != nil {
		t.Fatalf("读取受邀用户：%v", err)
	}
	if storedInvitee.Quota != 14 {
		t.Fatalf("新用户初始额度应保留为已决定的 10，邀请奖励应读取更新后的 4，实际为 %d", storedInvitee.Quota)
	}

	var storedInviter User
	if err := db.First(&storedInviter, inviter.Id).Error; err != nil {
		t.Fatalf("读取邀请人：%v", err)
	}
	if storedInviter.Quota != 105 {
		t.Fatalf("邀请人奖励应读取更新后的 5，实际额度为 %d", storedInviter.Quota)
	}
}
