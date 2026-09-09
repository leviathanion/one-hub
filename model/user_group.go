package model

import (
	"fmt"
	"gorm.io/gorm"
	"math"
	"one-api/common/config"
	"one-api/common/limit"
	"one-api/common/logger"
	"one-api/common/redis"
	"sync"
)

type UserGroup struct {
	Id        int     `json:"id"`
	Symbol    string  `json:"symbol" gorm:"type:varchar(50);uniqueIndex"`
	Name      string  `json:"name" gorm:"type:varchar(50)"`
	Ratio     float64 `json:"ratio" gorm:"type:decimal(10,2); default:1"`      // 倍率
	APIRate   int     `json:"api_rate" gorm:"default:600"`                     // 每分组允许的请求数
	Public    bool    `json:"public" form:"public" gorm:"default:false"`       // 是否为公开分组，如果是，则可以被用户在令牌中选择
	Promotion bool    `json:"promotion" form:"promotion" gorm:"default:false"` // 是否是自动升级用户组， 如果是则用户充值金额满足条件自动升级
	Min       int     `json:"min" form:"min" gorm:"default:0"`                 // 晋级条件最小值
	Max       int     `json:"max" form:"max" gorm:"default:0"`                 // 晋级条件最大值
	Enable    *bool   `json:"enable" form:"enable" gorm:"default:true"`        // 是否启用
}

type SearchUserGroupParams struct {
	UserGroup
	PaginationParams
}

var allowedUserGroupOrderFields = map[string]bool{
	"id":     true,
	"name":   true,
	"enable": true,
}

func GetUserGroupsList(params *SearchUserGroupParams) (*DataResult[UserGroup], error) {
	var userGroups []*UserGroup
	db := DB

	if params.Name != "" {
		db = db.Where("name LIKE ?", params.Name+"%")
	}

	if params.Enable != nil {
		db = db.Where("enable = ?", *params.Enable)
	}

	return PaginateAndOrder(db, &params.PaginationParams, &userGroups, allowedUserGroupOrderFields)
}

func GetUserGroupsById(id int) (*UserGroup, error) {
	var userGroup UserGroup
	err := DB.Where("id = ?", id).First(&userGroup).Error
	return &userGroup, err
}

func GetUserGroupsAll(isPublic bool) ([]*UserGroup, error) {
	var userGroups []*UserGroup

	db := DB.Where("enable = ?", true)
	if isPublic {
		db = db.Where("public = ?", true)
	}

	err := db.Find(&userGroups).Error
	return userGroups, err
}

func (c *UserGroup) Create() error {
	if err := c.validateRatio(); err != nil {
		return err
	}
	return mutateUserGroupPolicy(func(tx *gorm.DB) error {
		// 创建时用非空指针保留显式零，同时保留原表结构和其他字段的默认值。
		return tx.Table(tx.NamingStrategy.TableName("UserGroup")).Create(&struct {
			*UserGroup
			Ratio *float64
		}{c, &c.Ratio}).Error
	})
}

func (c *UserGroup) Update() error {
	if err := c.validateRatio(); err != nil {
		return err
	}
	return mutateUserGroupPolicy(func(tx *gorm.DB) error {
		return tx.Select("name", "ratio", "public", "api_rate", "promotion", "min", "max").Updates(c).Error
	})
}

func (c *UserGroup) validateRatio() error {
	if math.IsNaN(c.Ratio) || math.IsInf(c.Ratio, 0) || c.Ratio < 0 {
		return fmt.Errorf("分组倍率必须是有限的非负数")
	}
	return nil
}

func (c *UserGroup) Delete() error {
	return mutateUserGroupPolicy(func(tx *gorm.DB) error {
		return tx.Delete(c).Error
	})
}

func ChangeUserGroupEnable(id int, enable bool) error {
	return mutateUserGroupPolicy(func(tx *gorm.DB) error {
		return tx.Model(&UserGroup{}).Where("id = ?", id).Update("enable", enable).Error
	})
}

type UserGroupRatio struct {
	sync.RWMutex
	UserGroup   map[string]*UserGroup
	APILimiter  map[string]limit.RateLimiter
	PublicGroup []string

	reloadMu         sync.Mutex
	publishedVersion int64
	databaseHead     int64
	lastSyncError    string
}

var GlobalUserGroupRatio = &UserGroupRatio{}

func stopUserGroupAPILimiters(limiters map[string]limit.RateLimiter) {
	for symbol, limiter := range limiters {
		if stopper, ok := limiter.(interface{ Stop() }); ok && stopper != nil {
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						logger.SysError(fmt.Sprintf("panic stopping api limiter for group %s: %v", symbol, recovered))
					}
				}()
				stopper.Stop()
			}()
		}
	}
}

func (cgrm *UserGroupRatio) GetBySymbol(symbol string) *UserGroup {
	cgrm.RLock()
	defer cgrm.RUnlock()

	if symbol == "" {
		return nil
	}

	userGroupRatio, ok := cgrm.UserGroup[symbol]
	if !ok {
		return nil
	}

	return userGroupRatio
}

func (cgrm *UserGroupRatio) GetByTokenUserGroup(tokenGroup, userGroup string) *UserGroup {
	if tokenGroup != "" {
		return cgrm.GetBySymbol(tokenGroup)
	}

	return cgrm.GetBySymbol(userGroup)
}

func (cgrm *UserGroupRatio) GetAll() map[string]*UserGroup {
	cgrm.RLock()
	defer cgrm.RUnlock()

	return cgrm.UserGroup
}

func (cgrm *UserGroupRatio) GetAPIRate(symbol string) int {
	userGroup := cgrm.GetBySymbol(symbol)
	if userGroup == nil {
		return 0
	}

	return userGroup.APIRate
}

func (cgrm *UserGroupRatio) GetPublicGroupList() []string {
	cgrm.RLock()
	defer cgrm.RUnlock()

	return cgrm.PublicGroup
}

func (cgrm *UserGroupRatio) GetAPILimiter(symbol string) limit.RateLimiter {
	cgrm.RLock()
	defer cgrm.RUnlock()

	limiter, ok := cgrm.APILimiter[symbol]
	if !ok {
		return nil
	}

	return limiter
}

func CheckAndUpgradeUserGroup(userId int, rechargeAmount int) error {

	user := &User{}
	err := DB.Where("id = ?", userId).First(user).Error
	if err != nil {
		return err
	}

	cumulativeAmount := user.Quota + user.UsedQuota + rechargeAmount

	var promotionGroups []*UserGroup
	err = DB.Where("promotion = ? AND enable = ?", true, true).Find(&promotionGroups).Error
	if err != nil {
		return err
	}

	var targetGroup *UserGroup
	for _, group := range promotionGroups {
		var minQuota = (float64)(group.Min) * config.QuotaPerUnit
		var maxQuota = (float64)(group.Max) * config.QuotaPerUnit
		if (float64)(cumulativeAmount) >= minQuota && (group.Max == 0 || (float64)(cumulativeAmount) < maxQuota) {

			if targetGroup == nil || group.Min > targetGroup.Min {
				targetGroup = group
			}
		}
	}

	if targetGroup != nil && targetGroup.Symbol != user.Group {

		err = DB.Model(&User{}).Where("id = ?", userId).Update("group", targetGroup.Symbol).Error
		if err != nil {
			return err
		}

		if config.RedisEnabled {
			redis.RedisDel(fmt.Sprintf(UserGroupCacheKey, userId))
		}
	}

	return nil
}
