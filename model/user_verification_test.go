package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
	"one-api/common"
)

func TestUserVerificationExpiryAndFailedResetDoNotPermitReuse(t *testing.T) {
	useUserCreditDB(t)
	if err := DB.AutoMigrate(&UserVerification{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	email := "reset@example.com"
	if err := SetUserEmail(1, email); err != nil {
		t.Fatal(err)
	}
	if err := StoreUserVerification(ctx, email, common.PasswordResetPurpose, "expired", 1); err != nil {
		t.Fatal(err)
	}
	if err := DB.Model(&UserVerification{}).Where("slot = ?", verificationHash(common.PasswordResetPurpose+"\x00"+email)).Update("expires_at", time.Now().Unix()-1).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ConsumeUserVerification(ctx, email, common.PasswordResetPurpose, "expired"); !errors.Is(err, ErrVerificationInvalid) {
		t.Fatalf("过期凭据被接受: %v", err)
	}
	if err := StoreUserVerification(ctx, email, common.PasswordResetPurpose, "usable", 1); err != nil {
		t.Fatal(err)
	}
	id, err := ConsumeUserVerification(ctx, email, common.PasswordResetPurpose, "usable")
	if err != nil {
		t.Fatal(err)
	}
	const hook = "test:password-storage-error"
	if err := DB.Callback().Update().Before("gorm:update").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.AddError(errors.New("password storage failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(hook) })
	if err := ResetUserPassword(ctx, id, email, "changed-password"); err == nil {
		t.Fatal("SQL 失败仍返回成功")
	}
	if _, err := ConsumeUserVerification(ctx, email, common.PasswordResetPurpose, "usable"); !errors.Is(err, ErrVerificationInvalid) {
		t.Fatal("失败后旧凭据重新可用")
	}
	user := readCreditUser(t, 1)
	if user.Password != "password123" {
		t.Fatal("失败写入修改了密码")
	}
}

func TestPasswordRecoveryRejectsHistoricalDuplicateOwners(t *testing.T) {
	useUserCreditDB(t)
	if err := DB.Migrator().DropIndex(&User{}, "uidx_users_email"); err != nil {
		t.Fatal(err)
	}
	if err := DB.Model(&User{}).Where("id IN ?", []int{1, 2}).Update("email", "duplicate@example.com").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := FindPasswordRecoveryUser(context.Background(), "duplicate@example.com"); err == nil {
		t.Fatal("历史重复邮箱仍可签发重置凭据")
	}
	if err := ResetUserPassword(context.Background(), 1, "duplicate@example.com", "changed-password"); !errors.Is(err, ErrVerificationInvalid) {
		t.Fatalf("历史冲突仍修改密码: %v", err)
	}
	for _, id := range []int{1, 2} {
		if user := readCreditUser(t, id); user.Password != "password123" {
			t.Fatalf("历史冲突修改用户 %d 密码", id)
		}
	}
}
