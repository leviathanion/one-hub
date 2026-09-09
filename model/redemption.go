package model

import (
	"errors"
	"fmt"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/utils"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Redemption struct {
	Id               int    `json:"id"`
	UserId           int    `json:"user_id"`
	Key              string `json:"key" gorm:"type:char(32);uniqueIndex"`
	Status           int    `json:"status" gorm:"default:1"`
	Name             string `json:"name" gorm:"index"`
	Quota            int    `json:"quota" gorm:"default:100"`
	CreatedTime      int64  `json:"created_time" gorm:"bigint"`
	RedeemedTime     int64  `json:"redeemed_time" gorm:"bigint"`
	RedeemedByUserID *int   `json:"redeemed_by_user_id" gorm:"index"`
	Count            int    `json:"count" gorm:"-:all"` // only for api request
}

var allowedRedemptionslOrderFields = map[string]bool{
	"id":            true,
	"name":          true,
	"status":        true,
	"quota":         true,
	"created_time":  true,
	"redeemed_time": true,
}

func GetRedemptionsList(params *GenericParams) (*DataResult[Redemption], error) {
	var redemptions []*Redemption
	db := DB
	if params.Keyword != "" {
		db = db.Where("id = ? or name LIKE ?", utils.String2Int(params.Keyword), params.Keyword+"%")
	}

	return PaginateAndOrder[Redemption](db, &params.PaginationParams, &redemptions, allowedRedemptionslOrderFields)
}

func GetRedemptionById(id int) (*Redemption, error) {
	if id == 0 {
		return nil, errors.New("id 为空！")
	}
	redemption := Redemption{Id: id}
	err := DB.First(&redemption, "id = ?", id).Error
	return &redemption, err
}

func Redeem(key string, userId int, ip string) (quota int, err error) {
	if key == "" {
		return 0, errors.New("未提供兑换码")
	}
	if userId == 0 {
		return 0, errors.New("无效的 user id")
	}
	redemption := &Redemption{}

	keyCol := "`key`"
	if common.UsingPostgreSQL {
		keyCol = `"key"`
	}

	err = DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(keyCol+" = ?", key).First(redemption).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("无效的兑换码")
			}
			return err
		}
		if redemption.Status != config.RedemptionCodeStatusEnabled {
			return errors.New("该兑换码已被使用")
		}
		user, err := CreditUserRecharge(tx, userId, int64(redemption.Quota))
		if err != nil {
			return err
		}
		if user.DeletedAt.Valid || user.Status != config.UserStatusEnabled {
			return errors.New("兑换用户不可用")
		}
		redemption.RedeemedTime = utils.GetTimestamp()
		redemption.Status = config.RedemptionCodeStatusUsed
		redemption.RedeemedByUserID = &userId
		result := tx.Model(&Redemption{}).Where("id = ? AND status = ?", redemption.Id, config.RedemptionCodeStatusEnabled).
			Updates(map[string]any{"redeemed_time": redemption.RedeemedTime, "redeemed_by_user_id": userId, "status": redemption.Status})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("该兑换码已被使用")
		}
		return nil
	})
	if err != nil {
		return 0, errors.New("兑换失败，" + err.Error())
	}

	RecordQuotaLog(userId, LogTypeTopup, redemption.Quota, ip, fmt.Sprintf("通过兑换码充值 %s", common.LogQuota(redemption.Quota)))
	return redemption.Quota, nil
}

func (redemption *Redemption) Insert() error {
	if redemption.Quota <= 0 {
		return errors.New("兑换额度必须大于 0")
	}
	redemption.Status = config.RedemptionCodeStatusEnabled
	redemption.RedeemedTime = 0
	redemption.RedeemedByUserID = nil
	return DB.Create(redemption).Error
}

// Update Make sure your token's fields is completed, because this will update non-zero values
func (redemption *Redemption) Update() error {
	if redemption.Quota <= 0 || (redemption.Status != config.RedemptionCodeStatusEnabled && redemption.Status != config.RedemptionCodeStatusDisabled) {
		return errors.New("兑换码状态或额度无效，已兑换记录不可修改")
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var current Redemption
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", redemption.Id).Error; err != nil {
			return err
		}
		if current.Status == config.RedemptionCodeStatusUsed || current.RedeemedByUserID != nil {
			return errors.New("兑换码已经兑换")
		}
		return tx.Model(&current).Select("name", "status", "quota").Updates(redemption).Error
	})
}

func (redemption *Redemption) Delete() error {
	var err error
	err = DB.Delete(redemption).Error
	return err
}

func DeleteRedemptionById(id int) (err error) {
	if id == 0 {
		return errors.New("id 为空！")
	}
	redemption := Redemption{Id: id}
	err = DB.Where(redemption).First(&redemption).Error
	if err != nil {
		return err
	}
	return redemption.Delete()
}

type RedemptionStatistics struct {
	Count  int64 `json:"count"`
	Quota  int64 `json:"quota"`
	Status int   `json:"status"`
}

func GetStatisticsRedemption() (redemptionStatistics []*RedemptionStatistics, err error) {
	err = DB.Model(&Redemption{}).Select("status", "count(*) as count", "sum(quota) as quota").Where("status != ?", 2).Group("status").Scan(&redemptionStatistics).Error
	return redemptionStatistics, err
}

type RedemptionStatisticsGroup struct {
	Date      string `json:"date"`
	Quota     int64  `json:"quota"`
	UserCount int64  `json:"user_count"`
}

func GetStatisticsRedemptionByPeriod(startTimestamp, endTimestamp int64) (redemptionStatistics []*RedemptionStatisticsGroup, err error) {
	groupSelect := getTimestampGroupsSelect("redeemed_time", "day", "date")

	err = DB.Raw(`
		SELECT `+groupSelect+`,
		sum(quota) as quota,
		count(distinct redeemed_by_user_id) as user_count
		FROM redemptions
		WHERE status=3
		AND redeemed_time BETWEEN ? AND ?
		GROUP BY date
		ORDER BY date
	`, startTimestamp, endTimestamp).Scan(&redemptionStatistics).Error

	return redemptionStatistics, err
}
