package model

import (
	"context"
	"errors"
	"strings"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/utils"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 资料命令只携带本次明确修改的字段，不接受持久化 User 快照。
type UserProfilePatch struct {
	DisplayName *string `json:"display_name"`
	Password    *string `json:"password"`
}

type UserAdminPatch struct {
	Id int `json:"id"`
	UserProfilePatch
	Username *string `json:"username"`
	Email    *string `json:"email"`
	Group    *string `json:"group"`
	Role     *int    `json:"role"`
	Status   *int    `json:"status"`
}

func profileUpdates(p UserProfilePatch) (map[string]any, error) {
	fields := map[string]any{}
	if p.DisplayName != nil {
		if err := common.Validate.Var(*p.DisplayName, "max=20"); err != nil {
			return nil, errors.New("显示名称过长")
		}
		fields["display_name"] = *p.DisplayName
	}
	if p.Password != nil && *p.Password != "" {
		if err := common.Validate.Struct(&User{Password: *p.Password}); err != nil {
			return nil, err
		}
		hash, err := common.Password2Hash(*p.Password)
		if err != nil {
			return nil, err
		}
		fields["password"] = hash
	}
	return fields, nil
}

func UpdateUserProfile(id int, p UserProfilePatch) error {
	fields, err := profileUpdates(p)
	if err != nil {
		return err
	}
	return updateUserFields(id, fields)
}

func UpdateUserByAdmin(ctx context.Context, actorID int, p UserAdminPatch) error {
	fields, err := profileUpdates(p.UserProfilePatch)
	if err != nil {
		return err
	}
	if p.Username != nil {
		if err := validateUsername(*p.Username); err != nil {
			return err
		}
		fields["username"] = *p.Username
	}
	if p.Email != nil {
		if err := validateUserEmail(*p.Email); err != nil {
			return err
		}
		fields["email"] = *p.Email
	}
	if p.Group != nil {
		fields["group"] = *p.Group
	}
	if p.Role != nil {
		fields["role"] = *p.Role
	}
	if p.Status != nil {
		fields["status"] = *p.Status
	}
	err = withAdminUser(ctx, actorID, p.Id, func(tx *gorm.DB, actor, target *User) error {
		return applyAdminUserFields(tx, actor, target, fields)
	})
	_, emailChanged := fields["email"]
	_, roleChanged := fields["role"]
	if err == nil && (emailChanged || roleChanged) {
		refreshRootEmail(p.Id)
	}
	return err
}

func validateUsername(username string) error {
	if strings.TrimSpace(username) == "" || common.Validate.Var(username, "max=12") != nil {
		return errors.New("用户名不能为空且长度不能超过 12")
	}
	return nil
}

func validateUserEmail(email string) error {
	if email != "" && common.Validate.Var(email, "email,max=50") != nil {
		return errors.New("邮箱地址无效")
	}
	return nil
}

// 授权与修改使用同一组被锁定的用户；按 ID 排序，避免交叉管理时锁序反转。
func withAdminUser(ctx context.Context, actorID, targetID int, apply func(*gorm.DB, *User, *User) error) error {
	if actorID <= 0 || targetID <= 0 {
		return errors.New("无效的用户 ID")
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ids := []int{actorID}
		if targetID != actorID {
			ids = append(ids, targetID)
		}
		if len(ids) == 2 && ids[0] > ids[1] {
			ids[0], ids[1] = ids[1], ids[0]
		}
		var actor, target *User
		for _, id := range ids {
			var user User
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", id).Error; err != nil {
				return err
			}
			if id == actorID {
				actor = &user
			}
			if id == targetID {
				target = &user
			}
		}
		if actor.Status != config.UserStatusEnabled || actor.Role < config.RoleAdminUser || (actor.Role != config.RoleRootUser && actor.Role <= target.Role) {
			return errors.New("无权管理该用户")
		}
		return apply(tx, actor, target)
	})
}

func applyAdminUserFields(tx *gorm.DB, actor, target *User, fields map[string]any) error {
	nextRole, nextStatus := target.Role, target.Status
	if value, ok := fields["role"].(int); ok {
		nextRole = value
	}
	if value, ok := fields["status"].(int); ok {
		nextStatus = value
	}
	if nextRole == config.RoleRootUser && nextStatus != config.UserStatusEnabled {
		return errors.New("超级管理员必须保持启用")
	}
	if role, ok := fields["role"].(int); ok {
		switch role {
		case config.RoleCommonUser, config.RoleReliableUser, config.RoleAdminUser, config.RoleRootUser:
		default:
			return errors.New("无效的用户角色")
		}
		if (actor.Role != config.RoleRootUser && role >= actor.Role) || (target.Role == config.RoleRootUser && role != target.Role) {
			return errors.New("无权修改该角色")
		}
	}
	if status, ok := fields["status"].(int); ok {
		if status != config.UserStatusEnabled && status != config.UserStatusDisabled {
			return errors.New("无效的用户状态")
		}
		if target.Role == config.RoleRootUser && status != config.UserStatusEnabled {
			return errors.New("不能禁用超级管理员")
		}
	}
	if group, ok := fields["group"].(string); ok {
		var count int64
		if err := tx.Model(&UserGroup{}).Where("symbol = ? AND enable = ?", group, true).Count(&count).Error; err != nil {
			return err
		}
		if group == "" || count != 1 {
			return errors.New("用户组不存在或已停用")
		}
	}
	if len(fields) == 0 {
		return nil
	}
	normalizeUserIdentityFields(fields)
	return userIdentityError(tx.Model(&User{}).Where("id = ?", target.Id).Updates(fields).Error)
}

func ManageUserByAdmin(ctx context.Context, actorID, targetID int, action string) (*User, error) {
	var result User
	err := withAdminUser(ctx, actorID, targetID, func(tx *gorm.DB, actor, target *User) error {
		result = *target
		fields := map[string]any{}
		switch action {
		case "enable":
			fields["status"] = config.UserStatusEnabled
		case "disable":
			fields["status"] = config.UserStatusDisabled
		case "promote":
			if actor.Role != config.RoleRootUser {
				return errors.New("只有超级管理员可以设置管理员")
			}
			fields["role"] = config.RoleAdminUser
		case "demote":
			fields["role"] = config.RoleCommonUser
		case "set_reliable":
			fields["role"] = config.RoleReliableUser
		case "delete":
			if target.Role == config.RoleRootUser {
				return errors.New("不能删除超级管理员")
			}
			if err := tx.Model(target).Update("username", target.Username+"_del_"+utils.GetRandomString(6)).Error; err != nil {
				return err
			}
			return tx.Delete(target).Error
		default:
			return errors.New("未知的管理操作")
		}
		if err := applyAdminUserFields(tx, actor, target, fields); err != nil {
			return err
		}
		return tx.First(&result, "id = ?", target.Id).Error
	})
	return &result, err
}

func ChangeUserQuotaByAdmin(ctx context.Context, actorID, targetID, delta int) (before, after string, err error) {
	err = withAdminUser(ctx, actorID, targetID, func(tx *gorm.DB, actor, target *User) error {
		before = target.Group
		if _, err := applyUserQuotaChange(tx, target, int64(delta), 0); err != nil {
			return err
		}
		after = target.Group
		return nil
	})
	return
}

func refreshRootEmail(id int) {
	if user, err := GetUserById(id, false); err == nil && user.Role == config.RoleRootUser {
		config.RootUserEmail = user.Email
	}
}

func updateUserFields(id int, fields map[string]any) error {
	if id <= 0 {
		return errors.New("无效的用户 ID")
	}
	if len(fields) == 0 {
		_, err := GetUserById(id, false)
		return err
	}
	result := DB.Model(&User{}).Where("id = ?", id).Updates(fields)
	if result.Error != nil {
		return userIdentityError(result.Error)
	}
	// MySQL 对重复提交同一值可返回零行；存在性与是否发生修改是不同事实。
	if result.RowsAffected == 0 {
		if _, err := GetUserById(id, false); err != nil {
			return err
		}
	}
	return nil
}

// RecordUserLogin 读取当前主体并只写登录字段，不能恢复旧的组、角色或启用状态。
func RecordUserLogin(ctx context.Context, id int, ip string) (*User, error) {
	var user User
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", id).Error; err != nil {
			return err
		}
		if user.Status != config.UserStatusEnabled {
			return errors.New("用户已被禁用")
		}
		user.LastLoginTime, user.LastLoginIp = time.Now().Unix(), ip
		return tx.Model(&User{}).Where("id = ?", id).Updates(map[string]any{"last_login_time": user.LastLoginTime, "last_login_ip": ip}).Error
	})
	return &user, err
}

type UserIdentityPatch struct {
	GitHubID      *string
	GitHubIDNew   *int
	OIDCId        *string
	OIDCIssuer    string
	WeChatId      *string
	LarkId        *string
	TelegramID    *int64
	EmailIfEmpty  string
	AvatarIfEmpty string
}

func UpdateUserIdentity(id int, p UserIdentityPatch) error {
	fields := map[string]any{}
	if p.GitHubID != nil {
		fields["github_id"] = *p.GitHubID
	}
	if p.GitHubIDNew != nil {
		fields["github_id_new"] = *p.GitHubIDNew
	}
	if p.OIDCId != nil {
		fields["oidc_id"] = *p.OIDCId
	}
	if p.WeChatId != nil {
		fields["wechat_id"] = *p.WeChatId
	}
	if p.LarkId != nil {
		fields["lark_id"] = *p.LarkId
	}
	if p.TelegramID != nil {
		fields["telegram_id"] = *p.TelegramID
	}
	if p.AvatarIfEmpty != "" {
		fields["avatar_url"] = gorm.Expr("CASE WHEN avatar_url = '' OR avatar_url IS NULL THEN ? ELSE avatar_url END", p.AvatarIfEmpty)
	}
	return updateUserBindings(id, fields, p.EmailIfEmpty, p.OIDCIssuer)
}

func updateUserBindings(id int, fields map[string]any, emailIfEmpty, oidcIssuer string) error {
	emailIfEmpty = normalizeUserEmail(emailIfEmpty)
	if id <= 0 {
		return errors.New("无效的用户 ID")
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		if fields["oidc_id"] != nil {
			if err := lockOIDCIssuer(tx, oidcIssuer); err != nil {
				return err
			}
		}
		var user User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", id).Error; err != nil {
			return err
		}
		if user.Status != config.UserStatusEnabled {
			return errors.New("用户已被禁用")
		}
		current := map[string]any{"oidc_id": user.OidcId, "github_id_new": user.GitHubIdNew, "wechat_id": user.WeChatId, "lark_id": user.LarkId, "telegram_id": user.TelegramId}
		for key, old := range current {
			if value, ok := fields[key]; ok && value != nil {
				if err := validateIdentityPatchValue(key, value); err != nil {
					return err
				}
				if old != "" && old != 0 && old != int64(0) && old != value {
					return errors.New("已有其他身份绑定，请先解绑")
				}
			}
		}
		if len(fields) > 0 {
			if err := tx.Model(&User{}).Where("id = ?", id).Updates(fields).Error; err != nil {
				return err
			}
		}
		if user.Email == "" && emailIfEmpty != "" && validateUserEmail(emailIfEmpty) == nil {
			// 非关键资料补全的唯一冲突只回滚补全，不撤销已验证的身份绑定。
			err := tx.Transaction(func(inner *gorm.DB) error {
				return inner.Model(&User{}).Where("id = ?", id).Update("email", emailIfEmpty).Error
			})
			if err != nil && !IsUniqueConstraintError(err) {
				return err
			}
		}
		return nil
	})
	if err == nil && emailIfEmpty != "" {
		refreshRootEmail(id)
	}
	return userIdentityError(err)
}

func UnbindUserIdentity(id int, kind string) error {
	fields := map[string]any{}
	switch kind {
	case "github":
		fields["github_id"], fields["github_id_new"] = "", nil
	case "wechat":
		fields["wechat_id"] = nil
	case "lark":
		fields["lark_id"] = nil
	case "oidc":
		fields["oidc_id"] = nil
	case "telegram":
		fields["telegram_id"] = nil
	default:
		return errors.New("未知的绑定类型")
	}
	return updateUserBindings(id, fields, "", "")
}

func SetUserEmail(id int, email string) error {
	email = normalizeUserEmail(email)
	if err := validateUserEmail(email); err != nil {
		return err
	}
	fields := map[string]any{"email": email}
	normalizeUserIdentityFields(fields)
	err := updateUserBindings(id, fields, "", "")
	if err == nil {
		refreshRootEmail(id)
	}
	return err
}
func SetUserAccessToken(id int, token string) error {
	return updateUserFields(id, map[string]any{"access_token": token})
}
func SetUserTelegramID(id int, telegramID int64) error {
	if telegramID == 0 {
		return UnbindUserIdentity(id, "telegram")
	}
	return UpdateUserIdentity(id, UserIdentityPatch{TelegramID: &telegramID})
}
func EnsureUserAffCode(id int) (string, error) {
	if id <= 0 {
		return "", errors.New("无效的用户 ID")
	}
	code := utils.GetRandomString(4)
	if err := DB.Model(&User{}).Where("id = ? AND (aff_code = '' OR aff_code IS NULL)", id).Update("aff_code", code).Error; err != nil {
		return "", err
	}
	var user User
	if err := DB.Select("aff_code").First(&user, "id = ?", id).Error; err != nil {
		return "", err
	}
	return user.AffCode, nil
}
