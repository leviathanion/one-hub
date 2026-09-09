package model

import (
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"
	paytypes "one-api/payment/types"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func paymentSchemaDB(t *testing.T, migrate bool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if migrate {
		if err := db.AutoMigrate(&User{}, &Payment{}, &Order{}, &Redemption{}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
func requirePaymentUpgradeBlocked(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), paymentUpgradeGuide) {
		t.Fatalf("未提供停机升级阻断: %v", err)
	}
}
func TestPaymentUpgradePrerequisitesEmptyDatabase(t *testing.T) {
	db := paymentSchemaDB(t, false)
	if err := ValidatePaymentUpgradePrerequisites(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&User{}, &Payment{}, &Order{}, &Redemption{}); err != nil {
		t.Fatal(err)
	}
	if err := CheckPaymentOrderSchema(db); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePaymentOrderData(db); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePaymentUpgradePrerequisites(db); err != nil {
		t.Fatal(err)
	}
}
func TestPaymentUpgradePrerequisitesRejectHistoricalMissingFacts(t *testing.T) {
	for _, table := range []string{"payments", "orders"} {
		t.Run(table, func(t *testing.T) {
			db := paymentSchemaDB(t, false)
			if err := db.Exec("CREATE TABLE " + table + " (id INTEGER PRIMARY KEY)").Error; err != nil {
				t.Fatal(err)
			}
			if err := ValidatePaymentUpgradePrerequisites(db); err != nil {
				t.Fatalf("空旧表不应阻断: %v", err)
			}
			if err := db.Exec("INSERT INTO " + table + " (id) VALUES (1)").Error; err != nil {
				t.Fatal(err)
			}
			requirePaymentUpgradeBlocked(t, ValidatePaymentUpgradePrerequisites(db))
			if db.Migrator().HasColumn(table, "identity") {
				t.Fatal("检查擅自新增历史事实列")
			}
		})
	}
	t.Run("used_redemption", func(t *testing.T) {
		db := paymentSchemaDB(t, false)
		if err := db.Exec("CREATE TABLE redemptions (id INTEGER PRIMARY KEY, status INTEGER)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO redemptions (id, status) VALUES (1, 3)").Error; err != nil {
			t.Fatal(err)
		}
		requirePaymentUpgradeBlocked(t, ValidatePaymentUpgradePrerequisites(db))
	})
}
func TestPaymentOrderSchemaRequiresUniqueIndexesAndPaidCheck(t *testing.T) {
	for _, name := range []string{"idx_orders_trade_no", "idx_order_request", "idx_order_transaction"} {
		t.Run(name, func(t *testing.T) {
			db := paymentSchemaDB(t, true)
			if err := db.Migrator().DropIndex(&Order{}, name); err != nil {
				t.Fatal(err)
			}
			requirePaymentUpgradeBlocked(t, CheckPaymentOrderSchema(db))
		})
	}
	t.Run("index_must_be_unique", func(t *testing.T) {
		db := paymentSchemaDB(t, true)
		if err := db.Migrator().DropIndex(&Order{}, "idx_order_transaction"); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE INDEX idx_order_transaction ON orders (transaction_namespace,provider_transaction_id)").Error; err != nil {
			t.Fatal(err)
		}
		requirePaymentUpgradeBlocked(t, CheckPaymentOrderSchema(db))
	})
	t.Run("index_must_cover_namespace", func(t *testing.T) {
		db := paymentSchemaDB(t, true)
		if err := db.Migrator().DropIndex(&Order{}, "idx_order_transaction"); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE UNIQUE INDEX idx_order_transaction ON orders (provider_transaction_id)").Error; err != nil {
			t.Fatal(err)
		}
		requirePaymentUpgradeBlocked(t, CheckPaymentOrderSchema(db))
	})
	t.Run("paid_check", func(t *testing.T) {
		db := paymentSchemaDB(t, true)
		if err := db.Migrator().DropConstraint(&Order{}, "chk_order_paid"); err != nil {
			t.Fatal(err)
		}
		requirePaymentUpgradeBlocked(t, CheckPaymentOrderSchema(db))
	})
}
func insertPaymentSchemaFacts(t *testing.T, db *gorm.DB) Order {
	t.Helper()
	identity := paytypes.GatewayIdentity{Kind: "wxpay", Environment: "live", MerchantAccount: "merchant", AppBinding: "app", ProtocolProfile: "wxpay.native-v3.v1"}
	payment := Payment{ID: 1, Type: "wxpay", UUID: "schema-payment", Name: "fixture", Currency: CurrencyTypeCNY, Identity: identity, TransactionNamespace: paytypes.Namespace("wxpay", "live", "merchant"), CredentialRevision: 1, DefaultProduct: "wxpay.native", SetupStatus: "ready"}
	if err := db.Create(&payment).Error; err != nil {
		t.Fatal(err)
	}
	order := Order{ID: 1, UserId: 1, GatewayId: payment.ID, TradeNo: "schema-order", Quota: 100, OrderCurrency: CurrencyTypeCNY, RequestKey: "schema-request", RequestFingerprint: "fixture", TransactionNamespace: payment.TransactionNamespace, Identity: identity, ProductCode: "wxpay.native", ExpectedAmountMinor: 29, PreparationMode: paytypes.ServerCreate, PreparationState: paytypes.PreparationReady, LocalDisplayUntil: time.Now().Add(time.Hour), WindowState: paytypes.WindowOpen, PaymentState: paytypes.PaymentUnconfirmed}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	return order
}
func TestPaymentOrderSchemaRejectsMissingIdentityAndBadPaidData(t *testing.T) {
	for _, broken := range []string{"identity", "empty_transaction", "bad_paid"} {
		t.Run(broken, func(t *testing.T) {
			db := paymentSchemaDB(t, true)
			order := insertPaymentSchemaFacts(t, db)
			if err := CheckPaymentOrderSchema(db); err != nil {
				t.Fatalf("正常冻结数据被拒绝: %v", err)
			}
			if err := ValidatePaymentOrderData(db); err != nil {
				t.Fatalf("正常历史数据被拒绝: %v", err)
			}
			var err error
			switch broken {
			case "identity":
				err = db.Model(&Order{}).Where("id = ?", order.ID).Update("identity", "{}").Error
			case "empty_transaction":
				err = db.Model(&Order{}).Where("id = ?", order.ID).Update("provider_transaction_id", "").Error
			case "bad_paid":
				if err := db.Exec("PRAGMA ignore_check_constraints = ON").Error; err != nil {
					t.Fatal(err)
				}
				err = db.Model(&Order{}).Where("id = ?", order.ID).Updates(map[string]any{"payment_state": paytypes.PaymentPaid, "provider_transaction_id": "tx", "confirmed_amount_minor": 30, "confirmed_currency": "CNY", "evidence_summary": "fixture"}).Error
				if resetErr := db.Exec("PRAGMA ignore_check_constraints = OFF").Error; resetErr != nil {
					t.Fatal(resetErr)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckPaymentOrderSchema(db); err != nil {
				t.Fatalf("readiness 不应扫描历史数据：%v", err)
			}
			requirePaymentUpgradeBlocked(t, ValidatePaymentOrderData(db))
		})
	}
}
func TestPaymentOrderPaidConstraintRejectsMissingOrDifferentMoney(t *testing.T) {
	for _, field := range []string{"different_amount", "missing_amount", "missing_confirmed_currency", "missing_order_currency"} {
		t.Run(field, func(t *testing.T) {
			db := paymentSchemaDB(t, true)
			order := insertPaymentSchemaFacts(t, db)
			update := map[string]any{"payment_state": paytypes.PaymentPaid, "provider_transaction_id": "tx", "confirmed_amount_minor": 29, "confirmed_currency": "CNY"}
			switch field {
			case "different_amount":
				update["confirmed_amount_minor"] = 30
			case "missing_amount":
				update["confirmed_amount_minor"] = nil
			case "missing_confirmed_currency":
				update["confirmed_currency"] = nil
			case "missing_order_currency":
				update["order_currency"] = nil
			}
			if err := db.Model(&Order{}).Where("id = ?", order.ID).Updates(update).Error; err == nil {
				t.Fatal("SQL paid CHECK 接受了缺失或不相等金额")
			}
		})
	}
}

func TestPaymentUpgradeUnusedRedemptionsCanAddRedeemerColumn(t *testing.T) {
	db := paymentSchemaDB(t, false)
	if err := db.Exec("CREATE TABLE redemptions (id INTEGER PRIMARY KEY, status INTEGER)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO redemptions (id,status) VALUES (?,?)", 1, config.RedemptionCodeStatusEnabled).Error; err != nil {
		t.Fatal(err)
	}
	if err := ValidatePaymentUpgradePrerequisites(db); err != nil {
		t.Fatalf("未使用兑换码不应触发历史兑换者签收阻断: %v", err)
	}
	if db.Migrator().HasColumn(&Redemption{}, "redeemed_by_user_id") {
		t.Fatal("preflight 不应修改结构")
	}
}

func TestPaymentOrderSchemaRequiresRedemptionFacts(t *testing.T) {
	db := paymentSchemaDB(t, true)
	if err := db.Migrator().DropColumn(&Redemption{}, "redeemed_by_user_id"); err != nil {
		t.Fatal(err)
	}
	requirePaymentUpgradeBlocked(t, CheckPaymentOrderSchema(db))
}

func TestPaymentOrderDataRejectsUnknownRedeemer(t *testing.T) {
	for _, redeemer := range []*int{nil, new(int)} {
		name := "NULL"
		if redeemer != nil {
			name = "zero"
		}
		t.Run(name, func(t *testing.T) {
			db := paymentSchemaDB(t, true)
			redemption := Redemption{Id: 1, UserId: 99, Key: "redeemer-fixture", Status: config.RedemptionCodeStatusEnabled, Quota: 100, RedeemedByUserID: redeemer}
			if err := db.Create(&redemption).Error; err != nil {
				t.Fatal(err)
			}
			if err := ValidatePaymentOrderData(db); err != nil {
				t.Fatalf("未使用兑换码不要求兑换者: %v", err)
			}
			if err := db.Model(&redemption).Update("status", config.RedemptionCodeStatusUsed).Error; err != nil {
				t.Fatal(err)
			}
			if err := CheckPaymentOrderSchema(db); err != nil {
				t.Fatalf("readiness 不应扫描兑换记录: %v", err)
			}
			requirePaymentUpgradeBlocked(t, ValidatePaymentOrderData(db))
			if err := db.First(&redemption, redemption.Id).Error; err != nil {
				t.Fatal(err)
			}
			if redemption.RedeemedByUserID != nil && *redemption.RedeemedByUserID != 0 {
				t.Fatal("检查不应把创建者当作兑换者回填")
			}
			if err := db.Model(&redemption).Updates(map[string]any{"redeemed_by_user_id": 8, "redeemed_time": time.Now().Unix()}).Error; err != nil {
				t.Fatal(err)
			}
			if err := ValidatePaymentOrderData(db); err != nil {
				t.Fatalf("有实际兑换者的记录被拒绝: %v", err)
			}
		})
	}
}
