package model

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"

	"one-api/common/config"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func useUserCreditDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "credit.db")+"?_busy_timeout=5000&_txlock=immediate"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&User{}, &UserGroup{}, &Redemption{}, &Log{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(8)
	oldDB, oldOptions, oldRedis := DB, config.GlobalOption, config.RedisEnabled
	DB, config.RedisEnabled = db, false
	manager := config.NewOptionManager()
	perUnit := float64(1)
	manager.RegisterFloat("QuotaPerUnit", &perUnit)
	config.GlobalOption = manager
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"QuotaPerUnit": "1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { DB, config.GlobalOption, config.RedisEnabled = oldDB, oldOptions, oldRedis; _ = sqlDB.Close() })
	for _, user := range []User{
		{Id: 1, Username: "buyer", Password: "password123", AccessToken: "buyer-access", AffCode: "buyer", Quota: 20, Group: "default"},
		{Id: 2, Username: "creator", Password: "password123", AccessToken: "creator-access", AffCode: "creator", Group: "default"},
	} {
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	enabled := true
	if err := db.Create(&UserGroup{Symbol: "paid", Name: "付费组", Promotion: true, Min: 150, Enable: &enabled}).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func creditUserInOwnerTransaction(t *testing.T, quota int64) {
	t.Helper()
	if err := DB.Transaction(func(tx *gorm.DB) error { _, err := CreditUserRecharge(tx, 1, quota); return err }); err != nil {
		t.Fatal(err)
	}
}

func readCreditUser(t *testing.T, id int) User {
	t.Helper()
	var user User
	if err := DB.Unscoped().First(&user, id).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func TestUserQuotaChangesCountBalanceAndUsageOnce(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, source := range []string{"recharge", "redemption", "reward", "admin"} {
			t.Run(fmt.Sprintf("%s/batch=%v", source, batch), func(t *testing.T) {
				useUserCreditDB(t)
				originalBatch := config.BatchUpdateEnabled
				config.BatchUpdateEnabled = batch
				t.Cleanup(func() { config.BatchUpdateEnabled = originalBatch })
				if err := DB.Model(&User{}).Where("id = ?", 1).Updates(map[string]any{"quota": 30, "used_quota": 70}).Error; err != nil {
					t.Fatal(err)
				}
				if err := DB.Model(&UserGroup{}).Where("symbol = ?", "paid").Update("min", 200).Error; err != nil {
					t.Fatal(err)
				}
				for step := range 2 {
					var err error
					switch source {
					case "recharge":
						creditUserInOwnerTransaction(t, 50)
					case "redemption":
						code := newCreditRedemption(t, fmt.Sprintf("count-once-%d", step), 50)
						_, err = Redeem(code.Key, 1, "test")
					case "reward":
						err = IncreaseUserQuota(1, 50)
					case "admin":
						err = ChangeUserQuota(1, 50)
					}
					if err != nil {
						t.Fatal(err)
					}
					wantGroup := "default"
					if step == 1 {
						wantGroup = "paid"
					}
					user := readCreditUser(t, 1)
					if user.Quota != 80+50*step || user.UsedQuota != 70 || user.Group != wantGroup {
						t.Fatalf("第 %d 次变动应只计一次，组=%s，实际=%+v", step+1, wantGroup, user)
					}
				}
			})
		}
	}
}

func TestAdminDebitImmediatelyRechecksPromotionRanges(t *testing.T) {
	useUserCreditDB(t)
	if err := DB.Create(&UserGroup{Symbol: "lower", Promotion: true, Min: 0, Max: 150}).Error; err != nil {
		t.Fatal(err)
	}
	if err := DB.Model(&User{}).Where("id = ?", 1).Updates(map[string]any{"quota": 100, "used_quota": 70, "group": "paid"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := ChangeUserQuota(1, -20); err != nil {
		t.Fatal(err)
	}
	if user := readCreditUser(t, 1); user.Quota != 80 || user.UsedQuota != 70 || user.Group != "paid" {
		t.Fatalf("扣减后仍达到下限应保留组: %+v", user)
	}
	if err := ChangeUserQuota(1, -1); err != nil {
		t.Fatal(err)
	}
	if user := readCreditUser(t, 1); user.Quota != 79 || user.UsedQuota != 70 || user.Group != "lower" {
		t.Fatalf("扣减应立即按新总额匹配组，不能增加已消费: %+v", user)
	}
}

func TestUserQuotaChangeFailureKeepsBalanceAndGroup(t *testing.T) {
	db := useUserCreditDB(t)
	const callback = "test:promotion-unavailable"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "user_groups" {
			tx.AddError(errors.New("晋级规则查询失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	for _, delta := range []int{200, -10} {
		if err := ChangeUserQuota(1, delta); err == nil {
			t.Fatal("规则查询失败仍修改余额")
		}
		if user := readCreditUser(t, 1); user.Quota != 20 || user.Group != "default" {
			t.Fatalf("失败留下部分更新: %+v", user)
		}
	}
}

func TestUserQuotaChangeRejectsOverflowAndUnavailableUser(t *testing.T) {
	for _, tc := range []struct{ balance, delta int }{{math.MaxInt, 1}, {math.MinInt, -1}} {
		t.Run(fmt.Sprint(tc.delta), func(t *testing.T) {
			useUserCreditDB(t)
			if err := DB.Model(&User{}).Where("id = ?", 1).Update("quota", tc.balance).Error; err != nil {
				t.Fatal(err)
			}
			if err := ChangeUserQuota(1, tc.delta); !errors.Is(err, ErrUserQuotaChangeRejected) {
				t.Fatalf("溢出未拒绝: %v", err)
			}
			if user := readCreditUser(t, 1); user.Quota != tc.balance || user.Group != "default" {
				t.Fatalf("溢出改变余额或组: %+v", user)
			}
		})
	}
	useUserCreditDB(t)
	if err := DB.Delete(&User{}, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := ChangeUserQuota(1, 100); err == nil {
		t.Fatal("普通增减不应修改已删除用户")
	}
	if err := ChangeUserQuota(999, 100); err == nil {
		t.Fatal("不存在的用户增减成功")
	}
}

func TestUserCreditPreservesPromotionRanges(t *testing.T) {
	useUserCreditDB(t)
	enabled := true
	if err := DB.Model(&UserGroup{}).Where("symbol = ?", "paid").Update("max", 200).Error; err != nil {
		t.Fatal(err)
	}
	if err := DB.Create(&UserGroup{Symbol: "higher", Promotion: true, Min: 200, Max: 0, Enable: &enabled}).Error; err != nil {
		t.Fatal(err)
	}
	creditUserInOwnerTransaction(t, 130)
	if got := readCreditUser(t, 1).Group; got != "paid" {
		t.Fatalf("min 应包含: %s", got)
	}
	creditUserInOwnerTransaction(t, 50)
	if got := readCreditUser(t, 1).Group; got != "higher" {
		t.Fatalf("max 应排除、更高 min 优先: %s", got)
	}
	creditUserInOwnerTransaction(t, 1000)
	if got := readCreditUser(t, 1).Group; got != "higher" {
		t.Fatalf("max=0 应无上限: %s", got)
	}
}

func TestRegistrationRewardPromotesInCreationTransaction(t *testing.T) {
	for _, failPromotion := range []bool{false, true} {
		t.Run(fmt.Sprint(failPromotion), func(t *testing.T) {
			db := useUserCreditDB(t)
			newUserQuota := 150
			config.GlobalOption.RegisterInt("QuotaForNewUser", &newUserQuota)
			if _, err := config.GlobalOption.PublishRuntimeOverrides(2, map[string]string{"QuotaForNewUser": "150", "QuotaPerUnit": "1"}); err != nil {
				t.Fatal(err)
			}
			const callback = "test:registration-promotion-failure"
			if failPromotion {
				if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "user_groups" {
						tx.AddError(errors.New("晋级查询失败"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
			}
			user := &User{Username: "new-user"}
			err := user.Insert(0)
			if failPromotion {
				if err == nil {
					t.Fatal("晋级失败却注册成功")
				}
				var count int64
				if err := db.Model(&User{}).Where("username = ?", user.Username).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("失败保留了部分注册: count=%d err=%v", count, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if user := readCreditUser(t, user.Id); user.Quota != 150 || user.Group != "paid" {
				t.Fatalf("注册奖励未同步晋级: %+v", user)
			}
		})
	}
}

func newCreditRedemption(t *testing.T, key string, quota int) *Redemption {
	t.Helper()
	code := &Redemption{Key: key, UserId: 2, Name: "兑换", Quota: quota}
	if err := code.Insert(); err != nil {
		t.Fatal(err)
	}
	return code
}

func TestRedeemOwnsCreditAndCannotBeReactivated(t *testing.T) {
	useUserCreditDB(t)
	creditUserInOwnerTransaction(t, 100)
	code := newCreditRedemption(t, "credit-code", 50)
	if quota, err := Redeem(code.Key, 1, "test"); err != nil || quota != 50 {
		t.Fatalf("兑换: quota=%d err=%v", quota, err)
	}
	user := readCreditUser(t, 1)
	if user.Quota != 170 || user.Group != "paid" {
		t.Fatalf("兑换应完整入账: %+v", user)
	}
	if got := readCreditUser(t, 2).Quota; got != 0 {
		t.Fatalf("创建人获得额度: %d", got)
	}
	if _, err := Redeem(code.Key, 1, "test"); err == nil {
		t.Fatal("重复兑换成功")
	}
	stored, err := GetRedemptionById(code.Id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RedeemedByUserID == nil || *stored.RedeemedByUserID != 1 || stored.RedeemedTime == 0 {
		t.Fatalf("实际兑换归属丢失: %+v", stored)
	}
	// 模拟管理员在兑换前读出的旧对象晚提交，不能覆盖已消费额度或重新启用。
	code.Status, code.Quota = config.RedemptionCodeStatusEnabled, 500
	if err := code.Update(); err == nil {
		t.Fatal("管理编辑重新激活已兑码")
	}
	stored, _ = GetRedemptionById(code.Id)
	if stored.Status != config.RedemptionCodeStatusUsed || stored.Quota != 50 {
		t.Fatalf("管理编辑覆盖消费事实: %+v", stored)
	}
}

func TestRedeemRollsBackUserAndOwnershipOnRequiredFailure(t *testing.T) {
	for _, failedTable := range []string{"user_groups", "users", "redemptions"} {
		t.Run(failedTable, func(t *testing.T) {
			db := useUserCreditDB(t)
			creditUserInOwnerTransaction(t, 100)
			code := newCreditRedemption(t, "rollback-code", 50)
			callback := "test:credit-failure"
			fail := func(tx *gorm.DB) {
				if tx.Statement.Table == failedTable {
					tx.AddError(errors.New("injected credit failure"))
				}
			}
			if failedTable == "user_groups" {
				db.Callback().Query().Before("gorm:query").Register(callback, fail)
			} else {
				db.Callback().Update().Before("gorm:update").Register(callback, fail)
			}
			if _, err := Redeem(code.Key, 1, "test"); err == nil {
				t.Fatal("注入失败却兑换成功")
			}
			db.Callback().Query().Remove(callback)
			db.Callback().Update().Remove(callback)
			user := readCreditUser(t, 1)
			if user.Quota != 120 || user.Group != "default" {
				t.Fatalf("资金或分组未回滚: %+v", user)
			}
			stored, _ := GetRedemptionById(code.Id)
			if stored.Status != config.RedemptionCodeStatusEnabled || stored.RedeemedByUserID != nil || stored.RedeemedTime != 0 {
				t.Fatalf("兑换归属未回滚: %+v", stored)
			}
			if _, err := Redeem(code.Key, 1, "test"); err != nil {
				t.Fatalf("失败修复后兑换: %v", err)
			}
		})
	}
}

func TestConcurrentRechargeAndRedemptionSerializeUserTotal(t *testing.T) {
	db := useUserCreditDB(t)
	secondDB, err := gorm.Open(sqlite.Open(db.Dialector.(*sqlite.Dialector).DSN), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	secondSQL, err := secondDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondSQL.Close() })
	code := newCreditRedemption(t, "concurrent-code", 100)
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs <- secondDB.Transaction(func(tx *gorm.DB) error { _, err := CreditUserRecharge(tx, 1, 100); return err })
	}()
	go func() { defer wg.Done(); <-start; _, err := Redeem(code.Key, 1, "test"); errs <- err }()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	user := readCreditUser(t, 1)
	if user.Quota != 220 || user.Group != "paid" {
		t.Fatalf("并发累计或晋级丢失: %+v", user)
	}
}

func TestConcurrentRedeemCreditsOnce(t *testing.T) {
	useUserCreditDB(t)
	code := newCreditRedemption(t, "single-code", 150)
	start := make(chan struct{})
	results := make(chan error, 8)
	for range 8 {
		go func() { <-start; _, err := Redeem(code.Key, 1, "test"); results <- err }()
	}
	close(start)
	successes := 0
	for range 8 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("同一码成功次数: %d", successes)
	}
	user := readCreditUser(t, 1)
	if user.Quota != 170 {
		t.Fatalf("重复入账: %+v", user)
	}
}

func TestHistoricalCreditKeepsOriginalUserAndNewRedeemRequiresActiveUser(t *testing.T) {
	for _, unavailable := range []string{"soft_deleted", "disabled", "missing"} {
		t.Run(unavailable, func(t *testing.T) {
			useUserCreditDB(t)
			code := newCreditRedemption(t, fmt.Sprintf("code-%s", unavailable), 100)
			switch unavailable {
			case "soft_deleted":
				if err := DB.Delete(&User{}, 1).Error; err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := DB.Model(&User{}).Where("id = ?", 1).Update("status", config.UserStatusDisabled).Error; err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := DB.Unscoped().Delete(&User{}, 1).Error; err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Redeem(code.Key, 1, "test"); err == nil {
				t.Fatal("不可用用户获得新兑换")
			}
			if unavailable != "missing" {
				creditUserInOwnerTransaction(t, 100)
				if got := readCreditUser(t, 1).Quota; got != 120 {
					t.Fatalf("历史入账归属错误: %d", got)
				}
			}
		})
	}
}
