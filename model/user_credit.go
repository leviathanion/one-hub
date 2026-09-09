package model

import (
	"errors"
	"math"

	"one-api/common/config"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CreditUserRecharge 仅供订单或兑换 owner 在自身完成事务内调用。
// owner 先锁定自身记录并判定首次完成；本函数不创建独立的入账命令。
// 历史订单允许给软删除的原用户入账，新兑换在同一事务内确认用户仍有效。
func CreditUserRecharge(tx *gorm.DB, userID int, quota int64) (*User, error) {
	if tx == nil || quota <= 0 {
		return nil, ErrUserQuotaChangeRejected
	}
	return changeUserQuotaInTransaction(tx.Unscoped(), userID, quota)
}

// changeUserQuotaInTransaction 统一处理充值、奖励和管理增减；调用方拥有事务及提交后的缓存失效。
// 消费结算复用 applyUserQuotaChange，不在这里重复锁定用户。
func changeUserQuotaInTransaction(tx *gorm.DB, userID int, quotaDelta int64) (*User, error) {
	if tx == nil || userID <= 0 || quotaDelta < int64(math.MinInt) || quotaDelta > int64(math.MaxInt) {
		return nil, ErrUserQuotaChangeRejected
	}
	// 隔离各条查询的 statement，保留调用方事务及软删除作用域。
	tx = tx.Session(&gorm.Session{})
	var user User
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", userID).Error; err != nil {
		return nil, err
	}
	_, err := applyUserQuotaChange(tx, &user, quotaDelta, 0)
	return &user, err
}

// applyUserQuotaChange 在调用方持有用户行锁及事务时更新余额、最终消费和分组。
// usedDelta 仅来自已取得结算执行权的最终消费，预扣不能作为已消费。
func applyUserQuotaChange(tx *gorm.DB, user *User, quotaDelta, usedDelta int64) (bool, error) {
	if quotaDelta < int64(math.MinInt) || quotaDelta > int64(math.MaxInt) || usedDelta < 0 || usedDelta > int64(math.MaxInt) {
		return false, ErrUserQuotaChangeRejected
	}
	tx = tx.Session(&gorm.Session{})
	if (quotaDelta > 0 && user.Quota > math.MaxInt-int(quotaDelta)) ||
		(quotaDelta < 0 && user.Quota < math.MinInt-int(quotaDelta)) {
		return false, ErrUserQuotaChangeRejected
	}
	if user.UsedQuota > math.MaxInt-int(usedDelta) {
		return false, ErrUserQuotaChangeRejected
	}
	previousGroup := user.Group
	user.Quota += int(quotaDelta)
	user.UsedQuota += int(usedDelta)
	// 内存余额已包含本次变动，等价于变动前 Quota + UsedQuota + rechargeAmount，不能再加一次增量。
	// 分别转换后相加，避免余额与历史消费之和发生整数溢出。
	cumulativeAmount := float64(user.Quota) + float64(user.UsedQuota)
	var groups []UserGroup
	if err := tx.Where("promotion = ? AND enable = ?", true, true).Order("min DESC, id ASC").Find(&groups).Error; err != nil {
		return false, err
	}
	quotaPerUnit := config.GlobalOption.RuntimeSnapshot().Float64("QuotaPerUnit", config.QuotaPerUnit)
	for _, group := range groups {
		if cumulativeAmount >= float64(group.Min)*quotaPerUnit && (group.Max == 0 || cumulativeAmount < float64(group.Max)*quotaPerUnit) {
			user.Group = group.Symbol
			break
		}
	}
	updates := map[string]any{}
	if quotaDelta != 0 {
		updates["quota"] = user.Quota
	}
	if usedDelta != 0 {
		updates["used_quota"] = user.UsedQuota
	}
	if previousGroup != user.Group {
		updates["group"] = user.Group
	}
	if len(updates) == 0 {
		return false, nil
	}
	result := tx.Model(&User{}).Where("id = ?", user.Id).Updates(updates)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected != 1 {
		return false, errors.New("用户余额更新失败")
	}
	return true, nil
}

// RecalculateUserGroup 只重算组，返回同一事务观察到的前后分组供管理审计。
func RecalculateUserGroup(id int) (before, after string, err error) {
	if id <= 0 {
		return "", "", errors.New("无效的用户 ID")
	}
	err = DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", id).Error; err != nil {
			return err
		}
		before = user.Group
		if _, err := applyUserQuotaChange(tx, &user, 0, 0); err != nil {
			return err
		}
		after = user.Group
		return nil
	})
	return
}
