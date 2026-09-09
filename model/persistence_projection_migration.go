package model

import (
	"fmt"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	paytypes "one-api/payment/types"
)

var orderProjectionColumns = []string{"order_amount", "gateway_no", "status", "currency_exponent"}
var paymentProjectionColumns = []string{"protocol_profile"}
var taskProjectionColumns = []string{"prepared_at", "quota"}

// 所有表先核对再删列；MySQL 的 DDL 不能依赖事务回滚。
// 中途 DDL 失败后可重跑，已删列不再参与核对，资金及执行事实从不回填或重放。
func consolidatePersistenceProjections() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: "202609090003",
		Migrate: func(db *gorm.DB) error {
			for _, check := range []func(*gorm.DB) error{validateOrderProjections, validatePaymentProjections, validateTaskProjections} {
				if err := check(db); err != nil {
					return err
				}
			}
			if err := db.Transaction(mapClosedOrderWindows); err != nil {
				return err
			}
			for _, table := range []struct {
				name    string
				columns []string
			}{{"orders", orderProjectionColumns}, {"payments", paymentProjectionColumns}, {"tasks", taskProjectionColumns}} {
				columns, err := existingProjectionColumns(db, table.name, table.columns)
				if err != nil {
					return err
				}
				for _, column := range columns {
					if err := db.Exec("ALTER TABLE ? DROP COLUMN ?", clause.Table{Name: table.name}, clause.Column{Name: column}).Error; err != nil {
						return err
					}
				}
			}
			return nil
		},
		Rollback: func(*gorm.DB) error {
			return fmt.Errorf("派生字段已停止持久化；回滚须停写并恢复匹配的数据库备份和程序，不能启动旧 writer")
		},
	}
}

// 用驱动列元数据精确匹配，避免 SQLite HasColumn 的 SQL 文本匹配把 charged_quota 当作 quota。
func databaseColumnNames(db *gorm.DB, table string) (map[string]bool, error) {
	tables, err := db.Migrator().GetTables()
	if err != nil {
		return nil, err
	}
	actualTable := ""
	for _, candidate := range tables {
		if projectionIdentifierKey(db.Dialector.Name(), candidate) == projectionIdentifierKey(db.Dialector.Name(), table) {
			actualTable = candidate
			break
		}
	}
	if actualTable == "" {
		return nil, nil
	}
	columns, err := db.Migrator().ColumnTypes(actualTable)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(columns))
	for _, column := range columns {
		names[projectionIdentifierKey(db.Dialector.Name(), column.Name())] = true
	}
	return names, nil
}

func projectionIdentifierKey(dialect, name string) string {
	if dialect != "sqlite" {
		return name
	}
	// SQLite 仅对 ASCII 标识符忽略大小写，不折叠不同的 Unicode 名称。
	key := []byte(name)
	for i, b := range key {
		if b >= 'A' && b <= 'Z' {
			key[i] = b + ('a' - 'A')
		}
	}
	return string(key)
}

func projectionColumnSelection(names ...string) clause.Select {
	columns := make([]clause.Column, 0, len(names))
	for _, name := range names {
		// SQLite 返回声明时的列名；显式别名保证 GORM 能将大写历史列读入迁移结构。
		columns = append(columns, clause.Column{Name: name, Alias: name})
	}
	return clause.Select{Columns: columns}
}

func existingProjectionColumns(db *gorm.DB, table string, columns []string) ([]string, error) {
	names, err := databaseColumnNames(db, table)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, column := range columns {
		if names[projectionIdentifierKey(db.Dialector.Name(), column)] {
			result = append(result, column)
		}
	}
	return result, nil
}

func requireProjectionColumnsRemoved(db *gorm.DB, table string, columns []string) error {
	remaining, err := existingProjectionColumns(db, table, columns)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return fmt.Errorf("%s 仍有派生列 %v，请停写并运行主节点迁移", table, remaining)
	}
	return nil
}

func projectionConflict(table string, id int64, column string) error {
	return fmt.Errorf("%s 记录 %d 的 %s 与唯一事实来源不一致，请停写并核查历史数据后重跑迁移", table, id, column)
}

func projectionSourcesPresent(db *gorm.DB, table string, columns []string) (bool, error) {
	var found int
	if err := db.Table(table).Select("1").Limit(1).Scan(&found).Error; err != nil {
		return false, err
	}
	if found == 0 {
		return false, nil
	}
	names, err := databaseColumnNames(db, table)
	if err != nil {
		return false, err
	}
	for _, column := range columns {
		if !names[projectionIdentifierKey(db.Dialector.Name(), column)] {
			return false, fmt.Errorf("%s 存在历史记录但缺少 %s，请先完成历史任务或支付事实迁移；不自动推断资金状态", table, column)
		}
	}
	return true, nil
}

func validateOrderProjections(db *gorm.DB) error {
	columns, err := existingProjectionColumns(db, "orders", orderProjectionColumns)
	if err != nil || len(columns) == 0 {
		return err
	}
	sources := []string{"id", "expected_amount_minor", "order_currency", "payment_state", "provider_transaction_id", "window_state", "preparation_state"}
	if present, err := projectionSourcesPresent(db, "orders", sources); err != nil || !present {
		return err
	}
	// 使用独立迁移结构读取旧列，运行时模型不再解释它们。
	var rows []struct {
		ID                    int64
		ExpectedAmountMinor   int64
		OrderCurrency         string
		PaymentState          string
		WindowState           string
		PreparationState      string
		ProviderTransactionID *string
		OrderAmount           *string
		GatewayNo             *string
		Status                *string
		CurrencyExponent      *int
	}
	selected := append(sources, columns...)
	return db.Table("orders").Clauses(projectionColumnSelection(selected...)).FindInBatches(&rows, 200, func(_ *gorm.DB, _ int) error {
		for _, row := range rows {
			exponent, err := paytypes.CurrencyExponent(row.OrderCurrency)
			if err != nil {
				return projectionConflict("orders", row.ID, "order_currency")
			}
			if row.CurrencyExponent != nil && *row.CurrencyExponent != exponent {
				return projectionConflict("orders", row.ID, "currency_exponent")
			}
			if row.OrderAmount != nil {
				// SQLite 的 NUMERIC affinity 可能把 1.00 返回为 1；十进制解析不引入浮点容差。
				money, err := decimal.NewFromString(*row.OrderAmount)
				if err != nil || !money.Shift(int32(exponent)).Equal(decimal.NewFromInt(row.ExpectedAmountMinor)) {
					return projectionConflict("orders", row.ID, "order_amount")
				}
			}
			transaction := ""
			if row.ProviderTransactionID != nil {
				transaction = *row.ProviderTransactionID
			}
			if row.GatewayNo != nil && *row.GatewayNo != transaction {
				return projectionConflict("orders", row.ID, "gateway_no")
			}
			if row.Status != nil {
				paid := row.PaymentState == paytypes.PaymentPaid
				switch OrderStatus(*row.Status) {
				case OrderStatusSuccess:
					if !paid {
						return projectionConflict("orders", row.ID, "status")
					}
				case OrderStatusPending:
					if paid {
						return projectionConflict("orders", row.ID, "status")
					}
				case OrderStatusClosed:
					if (row.PaymentState != paytypes.PaymentUnconfirmed && row.PaymentState != paytypes.PaymentProcessing) ||
						(row.WindowState != paytypes.WindowOpen && row.WindowState != paytypes.WindowLocalExpired && row.WindowState != paytypes.WindowProviderClosed) {
						return projectionConflict("orders", row.ID, "status")
					}
				case OrderStatusFailed:
					// 失败原因不能从旧 failed 字符串猜测；已有准备拒绝事实才能投影为 failed。
					if (Order{PaymentState: row.PaymentState, WindowState: row.WindowState, PreparationState: row.PreparationState}).PublicStatus() != OrderStatusFailed {
						return projectionConflict("orders", row.ID, "status")
					}
				default:
					return projectionConflict("orders", row.ID, "status")
				}
			}
		}
		return nil
	}).Error
}

func mapClosedOrderWindows(db *gorm.DB) error {
	columns, err := existingProjectionColumns(db, "orders", []string{"status"})
	if err != nil || len(columns) == 0 {
		return err
	}
	if present, err := projectionSourcesPresent(db, "orders", []string{"id", "payment_state", "window_state"}); err != nil || !present {
		return err
	}
	// 旧超时关闭仅表示本地付款入口结束，保留上游已关闭的更强事实及所有付款事实。
	return db.Table("orders").Where("status = ? AND payment_state <> ? AND window_state <> ?", OrderStatusClosed, paytypes.PaymentPaid, paytypes.WindowProviderClosed).
		Update("window_state", paytypes.WindowLocalExpired).Error
}

func validatePaymentProjections(db *gorm.DB) error {
	columns, err := existingProjectionColumns(db, "payments", paymentProjectionColumns)
	if err != nil || len(columns) == 0 {
		return err
	}
	if present, err := projectionSourcesPresent(db, "payments", []string{"id", "identity"}); err != nil || !present {
		return err
	}
	var rows []struct {
		ID              int64
		Identity        paytypes.GatewayIdentity `gorm:"serializer:json"`
		ProtocolProfile *string
	}
	return db.Table("payments").Clauses(projectionColumnSelection("id", "identity", "protocol_profile")).FindInBatches(&rows, 200, func(_ *gorm.DB, _ int) error {
		for _, row := range rows {
			if row.ProtocolProfile != nil && *row.ProtocolProfile != row.Identity.ProtocolProfile {
				return projectionConflict("payments", row.ID, "protocol_profile")
			}
		}
		return nil
	}).Error
}

func validateTaskProjections(db *gorm.DB) error {
	columns, err := existingProjectionColumns(db, "tasks", taskProjectionColumns)
	if err != nil || len(columns) == 0 {
		return err
	}
	sources := []string{"id", "created_at", "charged_quota"}
	if present, err := projectionSourcesPresent(db, "tasks", sources); err != nil || !present {
		return err
	}
	var rows []struct {
		ID           int64
		CreatedAt    int64
		ChargedQuota *int64
		PreparedAt   *int64
		Quota        *int64
	}
	selected := append(sources, columns...)
	return db.Table("tasks").Clauses(projectionColumnSelection(selected...)).FindInBatches(&rows, 200, func(_ *gorm.DB, _ int) error {
		for _, row := range rows {
			if row.PreparedAt != nil && *row.PreparedAt != row.CreatedAt {
				return projectionConflict("tasks", row.ID, "prepared_at")
			}
			charged := int64(0)
			if row.ChargedQuota != nil {
				charged = *row.ChargedQuota
			}
			if row.Quota != nil && *row.Quota != charged {
				return projectionConflict("tasks", row.ID, "quota")
			}
		}
		return nil
	}).Error
}
