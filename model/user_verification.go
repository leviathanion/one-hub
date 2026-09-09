package model

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"one-api/common"
	"one-api/common/config"
)

// 一次性凭据只由 SQL 持有，Redis 开关和进程数不改变消费语义。
type UserVerification struct {
	Slot      string `gorm:"type:varchar(64);primaryKey"`
	CodeHash  string `gorm:"type:varchar(64);not null"`
	UserID    int    `gorm:"not null"`
	ExpiresAt int64  `gorm:"not null;index"`
}

const verificationCapacity = 10000
const verificationCapacitySlot = "capacity"

var ErrVerificationInvalid = errors.New("验证码错误、已使用或已过期")

func verificationHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func verificationCodeHash(code string) string {
	mac := hmac.New(sha256.New, []byte(config.SessionSecret))
	_, _ = mac.Write([]byte("user-verification\x00" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

func StoreUserVerification(ctx context.Context, email, purpose, code string, userID int) error {
	email = normalizeUserEmail(email)
	if validateUserEmail(email) != nil || email == "" || code == "" || (purpose != common.EmailVerificationPurpose && purpose != common.PasswordResetPurpose) || (purpose == common.PasswordResetPurpose && userID <= 0) {
		return ErrVerificationInvalid
	}
	entry := UserVerification{Slot: verificationHash(purpose + "\x00" + email), CodeHash: verificationCodeHash(code), UserID: userID, ExpiresAt: time.Now().Add(10 * time.Minute).Unix()}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 签发时串行检查总容量；消费用条件 DELETE，无需争用这行。
		guard := UserVerification{Slot: verificationCapacitySlot}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&guard).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&guard, "slot = ?", verificationCapacitySlot).Error; err != nil {
			return err
		}
		if err := tx.Where("slot <> ? AND expires_at <= ?", verificationCapacitySlot, time.Now().Unix()).Delete(&UserVerification{}).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&UserVerification{}).Where("slot <> ? AND slot <> ?", verificationCapacitySlot, entry.Slot).Count(&count).Error; err != nil {
			return err
		}
		if count >= verificationCapacity {
			return errors.New("验证码服务繁忙，请稍后重试")
		}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "slot"}}, DoUpdates: clause.AssignmentColumns([]string{"code_hash", "user_id", "expires_at"})}).Create(&entry).Error
	})
}

func ConsumeUserVerification(ctx context.Context, email, purpose, code string) (int, error) {
	email = normalizeUserEmail(email)
	if email == "" || code == "" {
		return 0, ErrVerificationInvalid
	}
	slot, hash := verificationHash(purpose+"\x00"+email), verificationCodeHash(code)
	var entry UserVerification
	if err := DB.WithContext(ctx).Where("slot = ? AND code_hash = ? AND expires_at > ?", slot, hash, time.Now().Unix()).First(&entry).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, ErrVerificationInvalid
		}
		return 0, err
	}
	// 只删除刚验证的那版凭据，不能误删并发签发的新码。
	result := DB.WithContext(ctx).Where("slot = ? AND code_hash = ? AND user_id = ? AND expires_at = ? AND expires_at > ?", slot, hash, entry.UserID, entry.ExpiresAt, time.Now().Unix()).Delete(&UserVerification{})
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected != 1 {
		return 0, ErrVerificationInvalid
	}
	return entry.UserID, nil
}

func FindPasswordRecoveryUser(ctx context.Context, email string) (*User, error) {
	email = normalizeUserEmail(email)
	if email == "" || validateUserEmail(email) != nil {
		return nil, ErrVerificationInvalid
	}
	var users []User
	if err := DB.WithContext(ctx).Where("email = ?", email).Limit(2).Find(&users).Error; err != nil {
		return nil, err
	}
	if len(users) != 1 || users[0].Status != config.UserStatusEnabled {
		return nil, errors.New("该邮箱没有唯一可用的账户，请联系管理员")
	}
	return &users[0], nil
}

func ResetUserPassword(ctx context.Context, userID int, email, password string) error {
	email = normalizeUserEmail(email)
	if userID <= 0 || email == "" || password == "" {
		return ErrVerificationInvalid
	}
	hash, err := common.Password2Hash(password)
	if err != nil {
		return err
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", userID).Error; err != nil {
			return err
		}
		if user.Email != email || user.Status != config.UserStatusEnabled {
			return ErrVerificationInvalid
		}
		var count int64
		if err := tx.Model(&User{}).Where("email = ?", email).Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return ErrVerificationInvalid
		}
		return tx.Model(&User{}).Where("id = ?", userID).Update("password", hash).Error
	})
}
