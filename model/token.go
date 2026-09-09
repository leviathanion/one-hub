package model

import (
	"context"
	"errors"
	"fmt"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/database"
	"one-api/common/logger"
	"one-api/common/redis"
	"one-api/common/utils"

	"gorm.io/gorm"
)

var (
	ErrTokenNotFound          = errors.New("令牌不存在")
	ErrTokenExpired           = errors.New("令牌已过期")
	ErrTokenQuotaExhausted    = errors.New("令牌额度已用尽")
	ErrTokenStatusUnavailable = errors.New("令牌状态不可用")
	ErrTokenInvalid           = errors.New("无效的令牌")
	ErrTokenQuotaGet          = errors.New("获取令牌额度失败")
	ErrTokenQuotaConflict     = errors.New("令牌额度已发生变化，请刷新后重试")
	ErrTokenQuotaInsufficient = errors.New("token quota is not enough")
)

type Token struct {
	Id             int            `json:"id"`
	UserId         int            `json:"user_id"`
	Key            string         `json:"key" gorm:"type:varchar(59);uniqueIndex"`
	Status         int            `json:"status" gorm:"default:1"`
	Name           string         `json:"name" gorm:"index" `
	CreatedTime    int64          `json:"created_time" gorm:"bigint"`
	AccessedTime   int64          `json:"accessed_time" gorm:"bigint"`
	ExpiredTime    int64          `json:"expired_time" gorm:"bigint;default:-1"` // -1 means never expired
	RemainQuota    int            `json:"remain_quota" gorm:"default:0"`
	UnlimitedQuota bool           `json:"unlimited_quota" gorm:"default:false"`
	UsedQuota      int            `json:"used_quota" gorm:"default:0"` // used quota
	Group          string         `json:"group" gorm:"default:''"`
	BackupGroup    string         `json:"backup_group" gorm:"default:''"`
	DeletedAt      gorm.DeletedAt `json:"-" gorm:"index"`

	Setting database.JSONType[TokenSetting] `json:"setting" form:"setting" gorm:"type:json"`
}

var allowedTokenOrderFields = map[string]bool{
	"id":           true,
	"name":         true,
	"status":       true,
	"expired_time": true,
	"created_time": true,
	"remain_quota": true,
	"used_quota":   true,
}

// 添加 AfterCreate 钩子方法
func (token *Token) AfterCreate(tx *gorm.DB) (err error) {
	tokenKey, err := common.GenerateToken(token.Id, token.UserId)
	if err != nil {
		return err
	}

	// 更新 key 字段
	return tx.Model(token).Update("key", tokenKey).Error
}

type TokenSetting struct {
	Heartbeat  HeartbeatSetting `json:"heartbeat,omitempty"`
	Limits     LimitsConfig     `json:"limits,omitempty"`
	BillingTag *string          `json:"billing_tag,omitempty"` // 费用标签，用于按分组统计费用，仅可信内部员工和管理员可见
}

type HeartbeatSetting struct {
	Enabled        bool `json:"enabled"`
	TimeoutSeconds int  `json:"timeout_seconds"`
}

type LimitsConfig struct {
	LimitModelSetting LimitModelSetting `json:"limit_model_setting,omitempty"`
	LimitsIPSetting   LimitsIPSetting   `json:"limits_ip_setting,omitempty"`
}

type LimitModelSetting struct {
	Enabled bool     `json:"enabled"`
	Models  []string `json:"models"`
}

type LimitsIPSetting struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist"`
}

func GetUserTokensList(userId int, params *GenericParams) (*DataResult[Token], error) {
	var tokens []*Token
	db := DB.Where("user_id = ?", userId)

	if params.Keyword != "" {
		db = db.Where("name LIKE ?", params.Keyword+"%")
	}

	return PaginateAndOrder(db, &params.PaginationParams, &tokens, allowedTokenOrderFields)
}

// AdminSearchTokensParams 管理员搜索令牌的参数
type AdminSearchTokensParams struct {
	GenericParams
	UserId  int `form:"user_id"`
	TokenId int `form:"token_id"`
}

// TokenWithOwner 包含令牌信息和所属用户信息
type TokenWithOwner struct {
	Token
	OwnerName string `json:"owner_name"` // 用户名称（优先显示 display_name，其次 username）
}

// GetTokensListByAdmin 管理员查询令牌列表（可按用户ID或令牌ID查询）
func GetTokensListByAdmin(params *AdminSearchTokensParams) (*DataResult[TokenWithOwner], error) {
	var tokens []*Token
	db := DB.Model(&Token{})

	// 按用户ID筛选
	if params.UserId > 0 {
		db = db.Where("user_id = ?", params.UserId)
	}

	// 按令牌ID筛选
	if params.TokenId > 0 {
		db = db.Where("id = ?", params.TokenId)
	}

	// 按关键词搜索名称
	if params.Keyword != "" {
		db = db.Where("name LIKE ?", params.Keyword+"%")
	}

	result, err := PaginateAndOrder(db, &params.PaginationParams, &tokens, allowedTokenOrderFields)
	if err != nil {
		return nil, err
	}

	// 收集所有用户ID
	userIds := make([]int, 0)
	userIdMap := make(map[int]bool)
	for _, token := range *result.Data {
		if !userIdMap[token.UserId] {
			userIds = append(userIds, token.UserId)
			userIdMap[token.UserId] = true
		}
	}

	// 批量查询用户信息
	userNameMap := make(map[int]string)
	if len(userIds) > 0 {
		var users []User
		DB.Select("id, username, display_name").Where("id IN ?", userIds).Find(&users)
		for _, user := range users {
			name := user.DisplayName
			if name == "" {
				name = user.Username
			}
			userNameMap[user.Id] = name
		}
	}

	// 构建带用户信息的结果
	tokensWithOwner := make([]*TokenWithOwner, len(*result.Data))
	for i, token := range *result.Data {
		tokensWithOwner[i] = &TokenWithOwner{
			Token:     *token,
			OwnerName: userNameMap[token.UserId],
		}
	}

	return &DataResult[TokenWithOwner]{
		Data:       &tokensWithOwner,
		TotalCount: result.TotalCount,
	}, nil
}

func GetTokenModel(key string) (token *Token, err error) {
	if key == "" {
		return nil, ErrTokenInvalid
	}

	var userId int
	var tokenId int
	validUser := false

	switch len(key) {
	case 48:
		validUser = true
		if config.RedisEnabled {
			exists, _ := redis.RedisSIsMember(OldUserTokensCacheKey, key)
			if !exists {
				return nil, ErrTokenInvalid
			}
		}
	case 59:
		tokenId, userId, err = common.ValidateToken(key)
		if err != nil || userId == 0 || tokenId == 0 {
			return nil, ErrTokenInvalid
		}
		if userEnabled, err := CacheIsUserEnabled(userId); err != nil || !userEnabled {
			return nil, ErrTokenInvalid
		}
	default:
		return nil, ErrTokenInvalid
	}

	token, err = CacheGetTokenByKey(key)
	if err != nil {
		maskedKey := key[:3] + "*********" + key[len(key)-3:]
		logger.SysError(fmt.Sprintf("DB Not Found: userId=%d, tokenId=%d, key=%s, err=%s", userId, tokenId, maskedKey, err.Error()))
		return nil, ErrTokenInvalid
	}

	if validUser {
		if userEnabled, err := CacheIsUserEnabled(token.UserId); err != nil || !userEnabled {
			return nil, ErrTokenInvalid
		}
	}

	return token, nil
}

func ValidateUserToken(key string) (token *Token, err error) {
	token, err = GetTokenModel(key)
	if err != nil {
		return nil, err
	}

	if token.Status != config.TokenStatusEnabled {
		switch token.Status {
		case config.TokenStatusExhausted:
			return nil, ErrTokenQuotaExhausted
		case config.TokenStatusExpired:
			return nil, ErrTokenExpired
		default:
			return nil, ErrTokenStatusUnavailable
		}
	}

	if token.ExpiredTime != -1 && token.ExpiredTime < utils.GetTimestamp() {
		return nil, ErrTokenExpired
	}

	return token, nil
}

func GetTokenByIds(id int, userId int) (*Token, error) {
	if id == 0 || userId == 0 {
		return nil, errors.New("id 或 userId 为空！")
	}
	token := Token{Id: id, UserId: userId}
	err := DB.First(&token, "id = ? and user_id = ?", id, userId).Error
	return &token, err
}

func GetTokenById(id int) (*Token, error) {
	return GetTokenByIdWithContext(context.Background(), id)
}

func GetTokenByIdWithContext(ctx context.Context, id int) (*Token, error) {
	if id == 0 {
		return nil, errors.New("id 为空！")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var token Token
	err := DB.WithContext(ctx).First(&token, "id = ?", id).Error
	return &token, err
}

func GetTokenByName(name string, userId int) (*Token, error) {
	if name == "" {
		return nil, errors.New("name 为空！")
	}
	token := Token{Name: name}
	err := DB.First(&token, "user_id = ? and name = ?", userId, name).Error
	return &token, err
}

func GetTokenByKey(key string) (*Token, error) {
	keyCol := "`key`"
	if common.UsingPostgreSQL {
		keyCol = `"key"`
	}

	var token Token

	err := DB.Where(keyCol+" = ?", key).First(&token).Error
	return &token, err
}

func (token *Token) Insert() error {
	err := DB.Create(token).Error
	return err
}

// UpdateMutableFields updates token metadata and optionally applies a quota
// compare-and-swap. UserId is deliberately absent: a token incarnation keeps
// the principal it was created for. Full edits pass the quota value observed by
// the caller so concurrent reserve/settlement deltas cause a conflict instead
// of being overwritten. Status-only edits pass nil.
func (token *Token) UpdateMutableFields(expectedRemainQuota *int) error {
	updates := map[string]any{
		"name":            token.Name,
		"status":          token.Status,
		"expired_time":    token.ExpiredTime,
		"unlimited_quota": token.UnlimitedQuota,
		"group":           token.Group,
		"backup_group":    token.BackupGroup,
		"setting":         token.Setting,
	}
	db := DB.Model(&Token{}).Where("id = ?", token.Id)
	if expectedRemainQuota != nil {
		db = db.Where("remain_quota = ?", *expectedRemainQuota)
		updates["remain_quota"] = token.RemainQuota
	}
	result := db.Updates(updates)
	err := result.Error
	if err == nil && expectedRemainQuota != nil && result.RowsAffected == 0 {
		err = ErrTokenQuotaConflict
	}
	// 防止Redis缓存不生效，直接删除
	if err == nil && config.RedisEnabled {
		redis.RedisDel(fmt.Sprintf(UserTokensKey, token.Key))
	}

	return err
}

func (token *Token) SelectUpdate() error {
	// This can update zero values
	return DB.Model(token).Select("accessed_time", "status").Updates(token).Error
}

func (token *Token) Delete() error {
	err := DB.Delete(token).Error
	return err
}

func DeleteTokenById(id int, userId int) (err error) {
	// Why we need userId here? In case user want to delete other's token.
	if id == 0 || userId == 0 {
		return errors.New("id 或 userId 为空！")
	}
	token := Token{Id: id, UserId: userId}
	err = DB.Where(token).First(&token).Error
	if err != nil {
		return err
	}
	err = token.Delete()

	if err == nil && config.RedisEnabled {
		redis.RedisDel(fmt.Sprintf(UserTokensKey, token.Key))
	}

	return err
}
