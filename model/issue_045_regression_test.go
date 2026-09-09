package model

import (
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/migrator"
)

func TestFixI045_PaymentSchemaAcrossDatabases(t *testing.T) {
	for _, layout := range []string{"fresh", "reordered_physical_columns"} {
		t.Run(layout, func(t *testing.T) {
			forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
				if layout == "reordered_physical_columns" {
					// PostgreSQL 驱动查询不承诺索引列顺序；旧表物理顺序可以不同于模型字段顺序。
					if err := db.Exec("CREATE TABLE orders (id INTEGER PRIMARY KEY, request_key VARCHAR(128) NOT NULL, provider_transaction_id VARCHAR(255))").Error; err != nil {
						t.Fatal(err)
					}
				}
				if err := ValidatePaymentUpgradePrerequisites(db); err != nil {
					t.Fatal(err)
				}
				migrateI045PaymentSchema(t, db)
				indexes, err := db.Migrator().GetIndexes(&Order{})
				if err != nil {
					t.Fatal(err)
				}
				for _, index := range indexes {
					if index.Name() == "idx_order_request" || index.Name() == "idx_order_transaction" {
						t.Logf("真实驱动 %s 返回列: %v", index.Name(), index.Columns())
					}
				}
				for repeat := 0; repeat < 2; repeat++ {
					if err := CheckPaymentOrderSchema(db); err != nil {
						t.Fatalf("完整支付结构被拒绝: %v", err)
					}
				}
				if err := ValidatePaymentOrderData(db); err != nil {
					t.Fatal(err)
				}
				assertI045PaymentUniqueness(t, db)
			})
		})
	}
}

func TestFixI045_RejectsIncorrectUniqueIndexesAcrossDatabases(t *testing.T) {
	for _, spec := range []struct {
		model   any
		table   string
		name    string
		columns []string
	}{
		{&Payment{}, "payments", "idx_payments_uuid", []string{"uuid"}},
		{&Order{}, "orders", "idx_orders_trade_no", []string{"trade_no"}},
		{&Order{}, "orders", "idx_order_request", []string{"user_id", "request_key"}},
		{&Order{}, "orders", "idx_order_transaction", []string{"transaction_namespace", "provider_transaction_id"}},
	} {
		for _, defect := range []string{"not_unique", "missing_column", "extra_column", "missing_index"} {
			t.Run(spec.name+"/"+defect, func(t *testing.T) {
				forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
					migrateI045PaymentSchema(t, db)
					if err := CheckPaymentOrderSchema(db); err != nil {
						t.Fatalf("正常基线结构被拒绝: %v", err)
					}
					if err := db.Migrator().DropIndex(spec.model, spec.name); err != nil {
						t.Fatal(err)
					}
					if defect != "missing_index" {
						columns := slices.Clone(spec.columns)
						unique := "UNIQUE "
						switch defect {
						case "not_unique":
							unique = ""
						case "missing_column":
							columns = columns[:len(columns)-1]
							if len(columns) == 0 {
								columns = []string{"id"}
							}
						case "extra_column":
							columns = append(columns, "created_at")
						}
						// 标识符仅来自上面的固定测试夹具。
						if err := db.Exec("CREATE " + unique + "INDEX " + spec.name + " ON " + spec.table + " (" + strings.Join(columns, ",") + ")").Error; err != nil {
							t.Fatal(err)
						}
					}
					err := CheckPaymentOrderSchema(db)
					requirePaymentUpgradeBlocked(t, err)
					if !strings.Contains(err.Error(), spec.name) {
						t.Fatalf("未明确指出错误索引 %s: %v", spec.name, err)
					}
				})
			})
		}
	}
}

func TestFixI045_ColumnComparisonPreservesDriverMetadata(t *testing.T) {
	for _, tc := range []struct {
		name     string
		actual   []string
		required []string
		want     bool
	}{
		{"request_forward", []string{"user_id", "request_key"}, []string{"user_id", "request_key"}, true},
		{"request_reversed", []string{"request_key", "user_id"}, []string{"user_id", "request_key"}, true},
		{"transaction_forward", []string{"transaction_namespace", "provider_transaction_id"}, []string{"transaction_namespace", "provider_transaction_id"}, true},
		{"transaction_reversed", []string{"provider_transaction_id", "transaction_namespace"}, []string{"transaction_namespace", "provider_transaction_id"}, true},
		{"uuid", []string{"uuid"}, []string{"uuid"}, true},
		{"trade_no", []string{"trade_no"}, []string{"trade_no"}, true},
		{"wrong_single_column", []string{"id"}, []string{"uuid"}, false},
		{"missing_column", []string{"request_key"}, []string{"user_id", "request_key"}, false},
		{"extra_column", []string{"user_id", "request_key", "id"}, []string{"user_id", "request_key"}, false},
		{"duplicate_replaces_required_column", []string{"request_key", "request_key"}, []string{"user_id", "request_key"}, false},
		{"extra_duplicate", []string{"request_key", "user_id", "request_key"}, []string{"user_id", "request_key"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index := &migrator.Index{ColumnList: slices.Clone(tc.actual)}
			required := slices.Clone(tc.required)
			if got := paymentIndexColumnsEqual(index.Columns(), required); got != tc.want {
				t.Fatalf("列组合匹配结果 = %v, want %v", got, tc.want)
			}
			if !slices.Equal(index.Columns(), tc.actual) || !slices.Equal(required, tc.required) {
				t.Fatalf("比较修改了元数据: actual=%v required=%v", index.Columns(), required)
			}
		})
	}
}

func migrateI045PaymentSchema(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(&User{}, &Payment{}, &Order{}, &Redemption{}); err != nil {
		t.Fatal(err)
	}
}

func assertI045PaymentUniqueness(t *testing.T, db *gorm.DB) {
	t.Helper()
	order := insertPaymentSchemaFacts(t, db)
	second := order
	second.ID, second.TradeNo, second.RequestKey = 2, "i045-second-order", "i045-second-request"
	if err := db.Create(&second).Error; err != nil {
		t.Fatalf("同命名空间的未确认订单应允许两个 NULL 交易号: %v", err)
	}
	duplicateRequest := order
	duplicateRequest.ID, duplicateRequest.TradeNo = 3, "i045-duplicate-request"
	if err := db.Create(&duplicateRequest).Error; !IsUniqueConstraintError(err) {
		t.Fatalf("重复 user_id/request_key 必须被唯一约束拒绝: %v", err)
	}
	duplicateTrade := order
	duplicateTrade.ID, duplicateTrade.RequestKey = 3, "i045-duplicate-trade"
	if err := db.Create(&duplicateTrade).Error; !IsUniqueConstraintError(err) {
		t.Fatalf("重复 trade_no 必须被唯一约束拒绝: %v", err)
	}
	if err := db.Model(&order).Update("provider_transaction_id", "i045-provider-transaction").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&second).Update("provider_transaction_id", "i045-provider-transaction").Error; !IsUniqueConstraintError(err) {
		t.Fatalf("同命名空间的重复上游交易号必须被拒绝: %v", err)
	}
	var payment Payment
	if err := db.First(&payment, order.GatewayId).Error; err != nil {
		t.Fatal(err)
	}
	payment.ID = 2
	if err := db.Create(&payment).Error; !IsUniqueConstraintError(err) {
		t.Fatalf("重复支付网关 UUID 必须被唯一约束拒绝: %v", err)
	}
}
