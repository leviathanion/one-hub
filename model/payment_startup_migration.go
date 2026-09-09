package model

import (
	"fmt"
	"strings"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	paytypes "one-api/payment/types"
)

type startupOrderColumns struct {
	ExpectedAmountMinor   *int64
	ProviderTransactionID *string `gorm:"type:varchar(255)"`
}

// 仅转换订单自身已经保存的金额和交易引用，不读取当前网关来猜测历史商户。
func migrateHistoricalPaymentRepresentations() *gormigrate.Migration {
	return &gormigrate.Migration{ID: "202609090004", Migrate: func(db *gorm.DB) error {
		names, err := databaseColumnNames(db, "orders")
		if err != nil || names == nil {
			return err
		}
		if !names["order_amount"] && !names["gateway_no"] && !names["provider_transaction_id"] {
			return nil
		}
		if err := addStartupMigrationColumns(db, "orders", &startupOrderColumns{}); err != nil {
			return err
		}
		return db.Transaction(func(tx *gorm.DB) error {
			var rows []struct {
				ID                    int64
				OrderAmount           *string
				OrderCurrency         string
				GatewayNo             *string
				ExpectedAmountMinor   *int64
				ProviderTransactionID *string
			}
			return tx.Table("orders").FindInBatches(&rows, 200, func(_ *gorm.DB, _ int) error {
				for _, row := range rows {
					updates := make(map[string]any)
					if row.OrderAmount != nil {
						exponent, err := paytypes.CurrencyExponent(row.OrderCurrency)
						if err != nil {
							return startupRowError("orders", row.ID, "历史金额缺少受支持的币种")
						}
						amount, err := decimal.NewFromString(*row.OrderAmount)
						minor := amount.Shift(int32(exponent))
						if err != nil || !minor.Equal(minor.Truncate(0)) || minor.Sign() <= 0 || !minor.Equal(decimal.NewFromInt(minor.IntPart())) {
							return startupRowError("orders", row.ID, "历史金额不是可无损表示的正整数最小货币单位")
						}
						if row.ExpectedAmountMinor == nil {
							updates["expected_amount_minor"] = minor.IntPart()
						} else if *row.ExpectedAmountMinor != minor.IntPart() {
							return projectionConflict("orders", row.ID, "order_amount")
						}
					}
					if row.GatewayNo != nil && *row.GatewayNo != "" {
						if strings.TrimSpace(*row.GatewayNo) == "" {
							return startupRowError("orders", row.ID, "历史交易号只有空白")
						}
						if row.ProviderTransactionID == nil || *row.ProviderTransactionID == "" {
							updates["provider_transaction_id"] = *row.GatewayNo
						} else if *row.ProviderTransactionID != *row.GatewayNo {
							return projectionConflict("orders", row.ID, "gateway_no")
						}
					} else if row.ProviderTransactionID != nil && *row.ProviderTransactionID == "" {
						updates["provider_transaction_id"] = nil
					}
					if len(updates) != 0 {
						if err := tx.Table("orders").Where("id = ?", row.ID).Updates(updates).Error; err != nil {
							return fmt.Errorf("订单 %d 表示迁移失败: %w", row.ID, err)
						}
					}
				}
				return nil
			}).Error
		})
	}, Rollback: startupMigrationRollback}
}
