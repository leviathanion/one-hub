package model

import (
	"context"
	"errors"
	"fmt"
	"one-api/common/config"
	"strings"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 单签发方绑定固定在既有 OIDCIssuer 选项中。登录、绑定及配置修改共用行锁。
func lockOIDCIssuer(tx *gorm.DB, issuer string) error {
	if issuer == "" {
		return errors.New("缺少 OIDC 签发方")
	}
	var option Option
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: "OIDCIssuer"}).First(&option).Error; err != nil {
		return err
	}
	if option.Value != issuer {
		return errors.New("OIDC 签发方已变更，请重新登录；已有绑定须先迁移")
	}
	return nil
}

func FindUserByOIDC(ctx context.Context, issuer, subject string) (*User, error) {
	if subject == "" {
		return nil, errors.New("OIDC subject 为空")
	}
	var user User
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockOIDCIssuer(tx, issuer); err != nil {
			return err
		}
		return tx.Where("oidc_id = ?", subject).First(&user).Error
	})
	return &user, err
}

func validateOIDCIssuerMutation(tx *gorm.DB, desired string, removePin bool) error {
	option := Option{Key: "OIDCIssuer", Value: config.GlobalOption.RuntimeSnapshot().String("OIDCIssuer", config.OIDCIssuer)}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&option).Error; err != nil {
		return err
	}
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: "OIDCIssuer"}).First(&option).Error
	if err != nil {
		return err
	}
	previous := option.Value
	if previous == desired && !removePin {
		return nil
	}
	// MySQL 的普通 COUNT 可能使用事务早期快照；锁定读才能看到等待期间新提交的绑定。
	var bound User
	result := tx.Unscoped().Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("oidc_id IS NOT NULL AND oidc_id <> ''").Limit(1).Find(&bound)
	if err := result.Error; err != nil {
		return err
	}
	if result.RowsAffected != 0 {
		return &config.OptionValidationError{Key: "OIDCIssuer", Message: "已有 OIDC 绑定，须先完成身份迁移才能更换或移除签发方"}
	}
	return nil
}

func ValidateUserManagementSchema(db *gorm.DB) error {
	if !db.Migrator().HasTable(&UserVerification{}) {
		return errors.New("用户管理升级尚未完成：缺少验证码表")
	}
	for _, column := range uniqueUserIdentityColumns {
		if !db.Migrator().HasIndex(&User{}, "uidx_users_"+column) {
			return fmt.Errorf("用户管理升级尚未完成：缺少 %s 唯一约束", column)
		}
	}
	return nil
}

var ErrIdentityOccupied = errors.New("邮箱或第三方身份已被占用")

var uniqueUserIdentityColumns = []string{"email", "oidc_id", "github_id_new", "wechat_id", "telegram_id", "lark_id"}

func normalizeUserIdentityFields(fields map[string]any) {
	if email, ok := fields["email"].(string); ok {
		fields["email"] = normalizeUserEmail(email)
	}
	for _, key := range uniqueUserIdentityColumns {
		if value, ok := fields[key]; ok && (value == "" || value == 0 || value == int64(0)) {
			fields[key] = nil
		}
	}
}

func normalizeUserEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func ensureUserIdentityCollations(tx *gorm.DB) error {
	if tx.Dialector.Name() != "mysql" {
		return nil
	}
	for _, column := range []string{"oidc_id", "wechat_id", "lark_id"} {
		if tx.Migrator().HasColumn(&User{}, column) {
			var collation string
			if err := tx.Raw("SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'users' AND COLUMN_NAME = ?", column).Scan(&collation).Error; err != nil {
				return err
			}
			if collation == "utf8mb4_bin" {
				continue
			}
			if err := tx.Exec("ALTER TABLE users MODIFY COLUMN " + column + " varchar(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL DEFAULT NULL").Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func userIdentityError(err error) error {
	if err != nil && IsUniqueConstraintError(err) {
		return ErrIdentityOccupied
	}
	return err
}

// 在 AutoMigrate 创建唯一索引之前盘点并清理未绑定值。冲突不自动认领。
func migrateUserIdentityUniqueness() *gormigrate.Migration {
	return &gormigrate.Migration{ID: "202609090001", Migrate: func(tx *gorm.DB) error {
		if !tx.Migrator().HasTable(&User{}) {
			return nil
		}
		var bound int64
		if tx.Migrator().HasColumn(&User{}, "oidc_id") {
			if err := tx.Table("users").Where("oidc_id IS NOT NULL AND oidc_id <> ''").Count(&bound).Error; err != nil {
				return err
			}
		}
		if bound > 0 {
			var issuer Option
			if err := tx.Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: "OIDCIssuer"}).First(&issuer).Error; err != nil || issuer.Value == "" {
				return errors.New("已有 OIDC 绑定但缺少持久化签发方，请核查原签发方并设置 OIDCIssuer 后升级")
			}
		}
		if err := ensureUserIdentityCollations(tx); err != nil {
			return err
		}
		for _, column := range uniqueUserIdentityColumns {
			if !tx.Migrator().HasColumn(&User{}, column) {
				continue
			}
			zero := any("")
			if column == "github_id_new" || column == "telegram_id" {
				zero = 0
			}
			var conflict struct{ Count int64 }
			group := column
			if column == "email" {
				group = "LOWER(TRIM(email))"
			}
			if err := tx.Table("users").Select("COUNT(*) AS count").Where(column+" IS NOT NULL AND "+column+" <> ?", zero).Group(group).Having("COUNT(*) > 1").Limit(1).Scan(&conflict).Error; err != nil {
				return err
			}
			if conflict.Count > 1 {
				return fmt.Errorf("users.%s 存在重复身份，请完成归属核查后再升级", column)
			}
		}
		if tx.Migrator().HasColumn(&User{}, "email") {
			if err := tx.Exec("UPDATE users SET email = LOWER(TRIM(email)) WHERE email IS NOT NULL").Error; err != nil {
				return err
			}
		}
		for _, column := range uniqueUserIdentityColumns {
			if !tx.Migrator().HasColumn(&User{}, column) {
				continue
			}
			zero := any("")
			if column == "github_id_new" || column == "telegram_id" {
				zero = 0
			}
			if err := tx.Table("users").Where(column+" = ?", zero).Update(column, nil).Error; err != nil {
				return err
			}
		}
		return nil
	}}
}

func validateIdentityPatchValue(column string, value any) error {
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return errors.New("第三方身份不能为空")
		}
	case int:
		if v <= 0 {
			return errors.New("第三方身份无效")
		}
	case int64:
		if v <= 0 {
			return errors.New("第三方身份无效")
		}
	default:
		return fmt.Errorf("无效的身份字段 %s", column)
	}
	return nil
}
