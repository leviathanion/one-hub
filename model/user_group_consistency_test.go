package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"

	"gorm.io/gorm"
	"one-api/common/config"
)

func useGroupSettlementDB(t *testing.T, initial, threshold int) {
	t.Helper()
	useUserCreditDB(t)
	if err := DB.AutoMigrate(&Token{}); err != nil {
		t.Fatal(err)
	}
	if err := DB.Model(&UserGroup{}).Where("symbol = ?", "paid").Update("min", threshold).Error; err != nil {
		t.Fatal(err)
	}
	if err := DB.Create(&UserGroup{Symbol: "lower", Promotion: true, Max: threshold}).Error; err != nil {
		t.Fatal(err)
	}
	if err := ChangeUserQuota(1, initial-20); err != nil {
		t.Fatal(err)
	}
	if err := DB.Session(&gorm.Session{SkipHooks: true}).Create(&Token{Id: 1, UserId: 1, Key: "consistency-token", RemainQuota: 1000, ExpiredTime: -1}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestGroupConvergesAfterReserveRechargeAndSettlement(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, final := range []int64{-1, 0, 50, 100, 150} {
			t.Run(fmt.Sprintf("batch=%v/final=%d", batch, final), func(t *testing.T) {
				useGroupSettlementDB(t, 200, 150)
				oldBatch := config.BatchUpdateEnabled
				config.BatchUpdateEnabled = batch
				t.Cleanup(func() { config.BatchUpdateEnabled = oldBatch })
				reserve, err := ApplyBillingReserve(context.Background(), 1, 1, 100)
				if err != nil {
					t.Fatal(err)
				}
				if err := ChangeUserQuota(1, 10); err != nil {
					t.Fatal(err)
				}
				if u := readCreditUser(t, 1); u.Group != "lower" {
					t.Fatalf("未覆盖预扣期间暂时降组: %+v", u)
				}
				charged := final
				if final < 0 {
					charged = 0
					_, err = ApplyBillingRefund(context.Background(), 1, 1, reserve.TokenQuotaApplied, 100)
				} else {
					_, err = ApplyBillingSettlementBalances(context.Background(), 1, 1, 100, final, reserve.TokenQuotaApplied)
				}
				if err != nil {
					t.Fatal(err)
				}
				assertGroupBalance(t, 210-int(charged), int(charged), "paid")
				// 批量刷新不能再次累计最终消费。
				batchUpdate()
				assertGroupBalance(t, 210-int(charged), int(charged), "paid")
			})
		}
	}
}

func assertGroupBalance(t *testing.T, quota, used int, group string) {
	t.Helper()
	u := readCreditUser(t, 1)
	if u.Quota != quota || u.UsedQuota != used || u.Group != group {
		t.Fatalf("期望余额=%d 消费=%d 组=%s，实际=%+v", quota, used, group, u)
	}
}

func TestGroupConvergesWithConcurrentConfirmAndCancel(t *testing.T) {
	useGroupSettlementDB(t, 400, 300)
	for range 2 {
		if _, err := ApplyBillingReserve(context.Background(), 1, 1, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := ChangeUserQuota(1, -50); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := ApplyBillingSettlementBalances(context.Background(), 1, 1, 100, 80, true)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := ApplyBillingRefund(context.Background(), 1, 1, true, 100)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertGroupBalance(t, 270, 80, "paid")
}

func TestGroupSettlementNoReservationAndFailuresAreAtomic(t *testing.T) {
	useGroupSettlementDB(t, 200, 150)
	if _, err := ApplyBillingSettlementBalances(context.Background(), 1, 1, 0, 50, false); err != nil {
		t.Fatal(err)
	}
	assertGroupBalance(t, 150, 50, "paid")
	const hook = "test:group-settlement-fail"
	if err := DB.Callback().Update().Before("gorm:update").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "tokens" {
			tx.AddError(errors.New("token 写入失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(hook) })
	if _, err := ApplyBillingSettlementBalances(context.Background(), 1, 1, 0, 50, false); err == nil {
		t.Fatal("错误未传播")
	}
	assertGroupBalance(t, 150, 50, "paid")
}

func TestUserUsedQuotaOverflowRollsBackSettlement(t *testing.T) {
	useGroupSettlementDB(t, 200, 150)
	if err := DB.Model(&User{}).Where("id = ?", 1).Update("used_quota", math.MaxInt).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyBillingSettlementBalances(context.Background(), 1, 1, 0, 1, false); err == nil {
		t.Fatal("累计溢出仍结算")
	}
	assertGroupBalance(t, 200, math.MaxInt, "paid")
}

func TestRecalculateGroupDoesNotChangeMoneyAndIsRepeatable(t *testing.T) {
	useGroupSettlementDB(t, 200, 150)
	if err := DB.Model(&User{}).Where("id = ?", 1).Update("group", "lower").Error; err != nil {
		t.Fatal(err)
	}
	before, after, err := RecalculateUserGroup(1)
	if err != nil || before != "lower" || after != "paid" {
		t.Fatalf("重算: %s %s %v", before, after, err)
	}
	before, after, err = RecalculateUserGroup(1)
	if err != nil || before != "paid" || after != "paid" {
		t.Fatalf("重复重算: %s %s %v", before, after, err)
	}
	assertGroupBalance(t, 200, 0, "paid")
}

func TestInvitePromotionSurvivesLoginAndUnrelatedUpdates(t *testing.T) {
	useUserCreditDB(t)
	newQuota, inviteeQuota, inviterQuota := 100, 50, 0
	config.GlobalOption.RegisterInt("QuotaForNewUser", &newQuota)
	config.GlobalOption.RegisterInt("QuotaForInvitee", &inviteeQuota)
	config.GlobalOption.RegisterInt("QuotaForInviter", &inviterQuota)
	if _, err := config.GlobalOption.PublishRuntimeOverrides(2, map[string]string{"QuotaPerUnit": "1", "QuotaForNewUser": "100", "QuotaForInvitee": "50", "QuotaForInviter": "0"}); err != nil {
		t.Fatal(err)
	}
	user := &User{Username: "invited"}
	if err := user.Insert(2); err != nil {
		t.Fatal(err)
	}
	if user.Group == "paid" {
		t.Fatal("复现需要保留奖励前的返回对象")
	}
	logged, err := RecordUserLogin(context.Background(), user.Id, "127.0.0.1")
	if err != nil || logged.Group != "paid" {
		t.Fatalf("登录撤销晋级: %+v %v", logged, err)
	}
	name, github, email := "新名字", "github-user", "new@example.com"
	if err := UpdateUserProfile(user.Id, UserProfilePatch{DisplayName: &name}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateUserIdentity(user.Id, UserIdentityPatch{GitHubID: &github, EmailIfEmpty: email}); err != nil {
		t.Fatal(err)
	}
	if err := UnbindUserIdentity(user.Id, "github"); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureUserAffCode(user.Id); err != nil {
		t.Fatal(err)
	}
	stored := readCreditUser(t, user.Id)
	if stored.Quota != 150 || stored.Group != "paid" || stored.DisplayName != name || stored.Email != email || stored.GitHubId != "" {
		t.Fatalf("更新字段边界错误: %+v", stored)
	}
	if err := DB.Model(&User{}).Where("id = ?", user.Id).Update("status", config.UserStatusDisabled).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RecordUserLogin(context.Background(), user.Id, "127.0.0.2"); err == nil {
		t.Fatal("旧用户快照使已禁用用户登录成功")
	}
}

func TestRootEmailBindingKeepsRuntimeContactCurrent(t *testing.T) {
	useUserCreditDB(t)
	old := config.RootUserEmail
	t.Cleanup(func() { config.RootUserEmail = old })
	if err := DB.Model(&User{}).Where("id = ?", 1).Update("role", config.RoleRootUser).Error; err != nil {
		t.Fatal(err)
	}
	if err := SetUserEmail(1, "root@example.com"); err != nil {
		t.Fatal(err)
	}
	if config.RootUserEmail != "root@example.com" {
		t.Fatalf("根用户邮箱未同步: %s", config.RootUserEmail)
	}
}
