package wxpay

import (
	"context"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	"one-api/payment/types"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// 从真实 SDK 通知验签得到的观察进入实际 SQL 入账事务，重复通知不能再次增额。
func assertNotificationCreditsOnce(t *testing.T, observation types.PaymentObservation) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.UserGroup{}, &model.Order{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	oldDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = oldDB; _ = sqlDB.Close() })
	user := model.User{Id: 1, Username: "wx-rotation", Password: "fixture", AccessToken: "fixture", Status: config.UserStatusEnabled, Quota: 10, Group: "default"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	order := model.Order{UserId: 1, GatewayId: observation.GatewayID, TradeNo: observation.TradeNo, Quota: 100, RequestKey: "wx-rotation", RequestFingerprint: "fixture", TransactionNamespace: observation.TransactionNamespace, Identity: observation.Identity, ProductCode: ProductNative, ExpectedAmountMinor: 29, OrderCurrency: model.CurrencyTypeCNY, PreparationMode: types.ServerCreate, PreparationState: types.PreparationReady, PaymentState: types.PaymentUnconfirmed, WindowState: types.WindowOpen, LocalDisplayUntil: time.Now().Add(time.Hour)}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		settled, newly, err := model.CompleteOrderPayment(context.Background(), observation)
		if err != nil || newly != (attempt == 0) || settled.PaymentState != types.PaymentPaid {
			t.Fatalf("settled=%+v newly=%v err=%v", settled, newly, err)
		}
	}
	if err := db.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 110 {
		t.Fatalf("重复支付通知改变了资金: quota=%d", user.Quota)
	}
}
