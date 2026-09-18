package model

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"one-api/common/config"
	paytypes "one-api/payment/types"
)

const paymentUpgradeGuide = "docs/dev/payment-order-architecture.md"

var paymentOrderRequiredColumns = []string{
	"id", "user_id", "gateway_id", "trade_no", "quota", "order_currency",
	"request_key", "request_fingerprint", "transaction_namespace", "identity", "product_code",
	"expected_amount_minor", "preparation_mode", "preparation_state",
	"preparation_claim_id", "preparation_claimed_at", "preparation_deadline", "prepared_with_revision",
	"create_input", "next_action", "provider_resource_ref", "local_display_until", "provider_payable_until",
	"expiry_source", "window_state", "payment_state", "provider_transaction_id", "confirmed_amount_minor",
	"confirmed_currency", "evidence_summary", "next_query_at", "query_count", "last_observed_at",
	"last_provider_status", "last_error_code", "close_state", "query_by_merchant_ref", "query_by_resource_ref",
}
var paymentBindingRequiredColumns = []string{
	"id", "uuid", "type", "currency", "config", "identity", "transaction_namespace",
	"credential_revision", "default_product", "setup_status",
}

// 新增的查询观察元数据可以从“尚未观察”开始；不属于历史身份或资金事实。
var paymentOrderObservationColumns = map[string]bool{
	"next_query_at": true, "query_count": true, "last_observed_at": true,
	"last_provider_status": true, "last_error_code": true,
}

type paymentSchemaTable struct {
	model   any
	columns []string
}

func paymentSchemaTables() []paymentSchemaTable {
	return []paymentSchemaTable{{&Payment{}, paymentBindingRequiredColumns}, {&Order{}, paymentOrderRequiredColumns}}
}

func paymentUpgradeError(format string, args ...any) error {
	return fmt.Errorf("支付升级未通过：%s；保持停机并按 %s 完成数据签收", fmt.Sprintf(format, args...), paymentUpgradeGuide)
}

// ValidatePaymentUpgradePrerequisites 必须在 AutoMigrate 前运行，防止缺少历史支付身份时以列默认值冒充升级。
// 已存在的列和数值不能证明历史资金已签收；该责任仍属于停机升级记录。
func ValidatePaymentUpgradePrerequisites(db *gorm.DB) error {
	if db == nil {
		return errors.New("支付升级检查需要数据库")
	}
	for _, table := range paymentSchemaTables() {
		if !db.Migrator().HasTable(table.model) {
			continue
		}
		var found int
		if err := db.Unscoped().Model(table.model).Select("1").Limit(1).Scan(&found).Error; err != nil {
			return err
		}
		if found == 0 {
			continue
		}
		for _, column := range table.columns {
			if _, order := table.model.(*Order); order && paymentOrderObservationColumns[column] {
				continue
			}
			if !db.Migrator().HasColumn(table.model, column) {
				return paymentUpgradeError("历史 %T 数据缺少 %s", table.model, column)
			}
		}
	}
	// 兑换者是独立资金事实；存在已使用旧兑换码时也不能用创建者或 NULL 自动初始化。
	if db.Migrator().HasTable(&Redemption{}) && !db.Migrator().HasColumn(&Redemption{}, "redeemed_by_user_id") {
		var found int
		if err := db.Unscoped().Model(&Redemption{}).Where("status = ?", config.RedemptionCodeStatusUsed).Select("1").Limit(1).Scan(&found).Error; err != nil {
			return err
		}
		if found != 0 {
			return paymentUpgradeError("历史已使用兑换码缺少实际兑换者")
		}
	}
	return nil
}

// CheckPaymentOrderSchema 仅检查最终结构，每表最多读取一行；可用于 readiness，不扫描历史数据。
func CheckPaymentOrderSchema(db *gorm.DB) error {
	if db == nil {
		return errors.New("支付结构检查需要数据库")
	}
	if err := requireProjectionColumnsRemoved(db, "orders", orderProjectionColumns); err != nil {
		return paymentUpgradeError("%v", err)
	}
	if err := requireProjectionColumnsRemoved(db, "payments", paymentProjectionColumns); err != nil {
		return paymentUpgradeError("%v", err)
	}
	migrator := db.Session(&gorm.Session{Logger: logger.Discard}).Migrator()
	tables := append(paymentSchemaTables(), paymentSchemaTable{&Redemption{}, []string{"id", "status", "quota", "redeemed_by_user_id", "redeemed_time"}})
	for _, table := range tables {
		rows, err := db.Unscoped().Model(table.model).Select(table.columns).Limit(1).Rows()
		if err != nil {
			return paymentUpgradeError("%T 结构缺少必要字段：%v", table.model, err)
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	for _, owner := range []struct {
		model    any
		required map[string][]string
	}{
		{&Payment{}, map[string][]string{"idx_payments_uuid": {"uuid"}}},
		{&Order{}, map[string][]string{"idx_orders_trade_no": {"trade_no"}, "idx_order_request": {"user_id", "request_key"}, "idx_order_transaction": {"transaction_namespace", "provider_transaction_id"}}},
	} {
		indexes, err := migrator.GetIndexes(owner.model)
		if err != nil {
			return fmt.Errorf("读取支付唯一索引: %w", err)
		}
		for name, columns := range owner.required {
			valid := false
			for _, index := range indexes {
				if index.Name() != name {
					continue
				}
				unique, known := index.Unique()
				valid = known && unique && paymentIndexColumnsEqual(index.Columns(), columns)
			}
			if !valid {
				return paymentUpgradeError("缺少正确的唯一索引 %s", name)
			}
		}
	}
	columns, err := migrator.ColumnTypes(&Order{})
	if err != nil {
		return err
	}
	for _, column := range columns {
		if column.Name() == "provider_transaction_id" {
			nullable, known := column.Nullable()
			if !known || !nullable {
				return paymentUpgradeError("provider_transaction_id 必须允许 NULL，未取得交易号时不能保存空字符串")
			}
		}
	}
	if !migrator.HasConstraint(&Order{}, "chk_order_paid") {
		return paymentUpgradeError("缺少支付成功证据约束 chk_order_paid")
	}
	return nil
}

func paymentIndexColumnsEqual(actual, required []string) bool {
	if len(actual) != len(required) {
		return false
	}
	// 唯一性取决于完整列组合；驱动返回顺序不属于约束，复制后排序以保留元数据。
	actual, required = slices.Clone(actual), slices.Clone(required)
	slices.Sort(actual)
	slices.Sort(required)
	return slices.Equal(actual, required)
}

// ValidatePaymentOrderData 在结构升级完成后仅于启动时检查历史数据；不作为 readiness 探针。
func ValidatePaymentOrderData(db *gorm.DB) error {
	if db == nil {
		return errors.New("支付数据检查需要数据库")
	}
	var unknownRedeemer int
	if err := db.Model(&Redemption{}).Where("status = ? AND (redeemed_by_user_id IS NULL OR redeemed_by_user_id <= 0)", config.RedemptionCodeStatusUsed).Select("1").Limit(1).Scan(&unknownRedeemer).Error; err != nil {
		return err
	}
	if unknownRedeemer != 0 {
		return paymentUpgradeError("历史已使用兑换码缺少实际兑换者，不能用创建者推断")
	}
	return checkPaymentUpgradeData(db)
}

func validPaymentIdentity(identity paytypes.GatewayIdentity) bool {
	return strings.TrimSpace(identity.Kind) != "" && strings.TrimSpace(identity.Environment) != "" && strings.TrimSpace(identity.MerchantAccount) != "" && strings.TrimSpace(identity.ProtocolProfile) != ""
}

func checkPaymentUpgradeData(db *gorm.DB) error {
	var bindings []Payment
	if err := db.Unscoped().Select(paymentBindingRequiredColumns).FindInBatches(&bindings, 200, func(_ *gorm.DB, _ int) error {
		for _, binding := range bindings {
			if !validPaymentIdentity(binding.Identity) || binding.Type != binding.Identity.Kind || strings.TrimSpace(binding.TransactionNamespace) == "" || strings.TrimSpace(binding.UUID) == "" || strings.TrimSpace(binding.DefaultProduct) == "" || binding.CredentialRevision <= 0 || binding.SetupStatus == "" {
				return paymentUpgradeError("网关 %d 缺少有效绑定身份或版本", binding.ID)
			}
			if _, err := paytypes.CurrencyExponent(string(binding.Currency)); err != nil {
				return paymentUpgradeError("网关 %d 币种无效", binding.ID)
			}
		}
		return nil
	}).Error; err != nil {
		return err
	}
	var orders []Order
	return db.Unscoped().Select("id", "user_id", "gateway_id", "trade_no", "quota", "order_currency", "request_key", "request_fingerprint", "transaction_namespace", "identity", "product_code", "expected_amount_minor", "preparation_mode", "preparation_state", "local_display_until", "window_state", "payment_state", "provider_transaction_id", "confirmed_amount_minor", "confirmed_currency", "evidence_summary").FindInBatches(&orders, 200, func(_ *gorm.DB, _ int) error {
		for _, order := range orders {
			if !validPaymentIdentity(order.Identity) || order.TransactionNamespace == "" || order.UserId <= 0 || order.GatewayId <= 0 || order.TradeNo == "" || order.ProductCode == "" || order.RequestKey == "" || order.RequestFingerprint == "" || order.Money().Validate() != nil || order.Quota <= 0 || order.LocalDisplayUntil.IsZero() || order.PreparationMode == "" || order.PreparationState == "" || order.WindowState == "" || order.PaymentState == "" {
				return paymentUpgradeError("订单 %d 缺少有效身份、冻结金额或状态", order.ID)
			}
			if order.ProviderTransactionID != nil && strings.TrimSpace(*order.ProviderTransactionID) == "" {
				return paymentUpgradeError("订单 %d 的空交易号必须使用 NULL", order.ID)
			}
			if order.PaymentState == paytypes.PaymentPaid && (order.ProviderTransactionID == nil || order.ConfirmedAmountMinor == nil || *order.ConfirmedAmountMinor != order.ExpectedAmountMinor || order.ConfirmedCurrency != string(order.OrderCurrency) || strings.TrimSpace(order.EvidenceSummary) == "") {
				return paymentUpgradeError("成功订单 %d 缺少等额支付证据", order.ID)
			}
		}
		return nil
	}).Error
}
