package model

import (
	"context"
	"errors"
	"fmt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"one-api/common/config"
	"one-api/payment/types"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func useOrderPaymentTestDB(t *testing.T) {
	t.Helper()
	original := DB
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "payments.db")+"?_busy_timeout=10000&_txlock=immediate"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&User{}, &Order{}, &UserGroup{}); err != nil {
		t.Fatal(err)
	}
	DB = db
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(8)
	t.Cleanup(func() { DB = original; _ = sqlDB.Close() })
}
func paymentFixture(t *testing.T) (*Order, types.PaymentObservation) {
	t.Helper()
	user := User{Id: 1, Username: "payment-user", Password: "password123", AccessToken: "payment-access-token", Status: config.UserStatusEnabled, Quota: 10, Group: "default"}
	if err := DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	identity := types.GatewayIdentity{Kind: "test", Environment: "test", MerchantAccount: "merchant", ProtocolProfile: "v1"}
	order := &Order{UserId: 1, GatewayId: 7, TradeNo: "trade-1", RequestKey: "request-1", RequestFingerprint: "frozen", Quota: 100, OrderCurrency: CurrencyTypeCNY, ExpectedAmountMinor: 29, TransactionNamespace: types.Namespace("test", "merchant"), Identity: identity, ProductCode: "test.checkout", PreparationState: types.PreparationReady, PreparationMode: types.ServerCreate, PaymentState: types.PaymentUnconfirmed, WindowState: types.WindowOpen, LocalDisplayUntil: time.Now().Add(time.Hour)}
	if err := DB.Create(order).Error; err != nil {
		t.Fatal(err)
	}
	total := order.Money()
	e := types.PaymentObservation{Source: types.SourceVerifiedCallback, GatewayID: 7, Identity: identity, TransactionNamespace: order.TransactionNamespace, TradeNo: order.TradeNo, State: types.ObservationSucceeded, ProviderTransactionID: "transaction-1", OrderTotal: &total, VerificationRef: "signed-v1"}
	return order, e
}
func assertPaymentCredit(t *testing.T, quota int, paid bool) {
	t.Helper()
	var user User
	if err := DB.Unscoped().First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	var order Order
	if err := DB.First(&order, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != quota || (order.PaymentState == types.PaymentPaid) != paid {
		t.Fatalf("user=%+v order=%+v", user, order)
	}
}
func TestCompleteOrderPaymentConcurrentExactlyOnce(t *testing.T) {
	useOrderPaymentTestDB(t)
	_, e := paymentFixture(t)
	originalOptions := config.GlobalOption
	manager := config.NewOptionManager()
	perUnit := float64(1)
	manager.RegisterFloat("QuotaPerUnit", &perUnit)
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = originalOptions })
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"QuotaPerUnit": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := DB.Create(&UserGroup{Symbol: "paid", Promotion: true, Min: 150}).Error; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var applied atomic.Int32
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, newly, err := CompleteOrderPayment(context.Background(), e)
			if newly {
				applied.Add(1)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if applied.Load() != 1 {
		t.Fatalf("applied=%d", applied.Load())
	}
	e.Source = types.SourceAuthenticatedQuery
	e.ProviderEventID = "another-event"
	if _, newly, err := CompleteOrderPayment(context.Background(), e); err != nil || newly {
		t.Fatalf("query duplicate newly=%v err=%v", newly, err)
	}
	assertPaymentCredit(t, 110, true)
	if group, err := GetUserGroup(1); err != nil || group != "default" {
		t.Fatalf("100 额度支付被重复计入晋级: group=%s err=%v", group, err)
	}
}
func TestCompleteOrderPaymentRejectsIncompleteAndConflictingEvidence(t *testing.T) {
	for _, field := range []string{"gateway", "namespace", "merchant", "environment", "amount", "currency", "transaction", "verification", "source"} {
		t.Run(field, func(t *testing.T) {
			useOrderPaymentTestDB(t)
			_, e := paymentFixture(t)
			switch field {
			case "gateway":
				e.GatewayID++
			case "namespace":
				e.TransactionNamespace = "other"
			case "merchant":
				e.Identity.MerchantAccount = "other"
			case "environment":
				e.Identity.Environment = "live"
			case "amount":
				e.OrderTotal.Minor--
			case "currency":
				e.OrderTotal.Currency = "USD"
			case "transaction":
				e.ProviderTransactionID = ""
			case "verification":
				e.VerificationRef = ""
			case "source":
				e.Source = "browser_return"
			}
			if _, newly, err := CompleteOrderPayment(context.Background(), e); err == nil || newly {
				t.Fatal("invalid evidence accepted")
			}
			assertPaymentCredit(t, 10, false)
		})
	}
}
func TestCompleteOrderPaymentAcceptsLateAndHistoricalOwner(t *testing.T) {
	useOrderPaymentTestDB(t)
	order, e := paymentFixture(t)
	if err := DB.Model(order).Updates(map[string]any{"window_state": types.WindowLocalExpired, "local_display_until": time.Now().Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := DB.Delete(&User{}, 1).Error; err != nil {
		t.Fatal(err)
	}
	if _, newly, err := CompleteOrderPayment(context.Background(), e); err != nil || !newly {
		t.Fatalf("late payment newly=%v err=%v", newly, err)
	}
	assertPaymentCredit(t, 110, true)
}
func TestCompleteOrderPaymentNamespaceUniqueConstraint(t *testing.T) {
	useOrderPaymentTestDB(t)
	order, e := paymentFixture(t)
	other := *order
	other.ID = 0
	other.TradeNo = "trade-2"
	other.RequestKey = "request-2"
	other.GatewayId = 8
	other.ProductCode = "test.other"
	if err := DB.Create(&other).Error; err != nil {
		t.Fatalf("multiple NULL refs: %v", err)
	}
	if _, _, err := CompleteOrderPayment(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	e.TradeNo = other.TradeNo
	e.GatewayID = other.GatewayId
	if _, newly, err := CompleteOrderPayment(context.Background(), e); err == nil || newly {
		t.Fatal("duplicate supplier transaction credited twice")
	}
	assertPaymentCredit(t, 110, true)
	e.TransactionNamespace = types.Namespace("test", "another-merchant")
	other.TransactionNamespace = e.TransactionNamespace
	if err := DB.Model(&other).Update("transaction_namespace", e.TransactionNamespace).Error; err != nil {
		t.Fatal(err)
	}
	if _, newly, err := CompleteOrderPayment(context.Background(), e); err != nil || !newly {
		t.Fatalf("distinct namespace denied: %v", err)
	}
}
func TestCompleteOrderPaymentRollsBackEveryCreditField(t *testing.T) {
	useOrderPaymentTestDB(t)
	_, e := paymentFixture(t)
	if err := DB.Exec("CREATE TRIGGER reject_user_credit BEFORE UPDATE ON users BEGIN SELECT RAISE(FAIL, 'forced credit failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, newly, err := CompleteOrderPayment(context.Background(), e); err == nil || newly {
		t.Fatal("failed credit acknowledged")
	}
	assertPaymentCredit(t, 10, false)
	if err := DB.Exec("DROP TRIGGER reject_user_credit").Error; err != nil {
		t.Fatal(err)
	}
	if _, newly, err := CompleteOrderPayment(context.Background(), e); err != nil || !newly {
		t.Fatal(fmt.Sprint(newly, err))
	}
	assertPaymentCredit(t, 110, true)
}
func TestPaidConstraintRejectsMissingConfirmation(t *testing.T) {
	useOrderPaymentTestDB(t)
	order, _ := paymentFixture(t)
	if err := DB.Model(order).Updates(map[string]any{"payment_state": types.PaymentPaid, "provider_transaction_id": "transaction"}).Error; err == nil {
		t.Fatal("SQL accepted paid without confirmed amount")
	}
}

func TestPaymentResourceBindingConcurrentObservationAndPreparation(t *testing.T) {
	useOrderPaymentTestDB(t)
	order, e := paymentFixture(t)
	if err := DB.Model(order).Updates(map[string]any{"preparation_state": types.PreparationClaimed, "preparation_claim_id": "original-claim"}).Error; err != nil {
		t.Fatal(err)
	}
	e.State = types.ObservationUnpaid
	e.ProviderResourceRef = "resource-from-query"
	e.ProviderTransactionID = ""
	e.OrderTotal = nil
	start := make(chan struct{})
	results := make(chan struct {
		resource string
		err      error
	}, 2)
	go func() {
		<-start
		_, err := SavePaymentObservation(context.Background(), e, time.Now())
		results <- struct {
			resource string
			err      error
		}{e.ProviderResourceRef, err}
	}()
	go func() {
		<-start
		until := time.Now().Add(time.Hour)
		result := types.PrepareResult{Outcome: types.PreparationReady, ProviderResourceRef: "resource-from-create", NextAction: types.NextAction{Kind: "redirect", Redirect: &types.RedirectAction{URL: "https://pay.example"}, ValidUntil: &until}}
		err := SavePreparationResult(context.Background(), order, "original-claim", result)
		results <- struct {
			resource string
			err      error
		}{result.ProviderResourceRef, err}
	}()
	close(start)
	winner := ""
	conflicts := 0
	for range 2 {
		result := <-results
		if result.err == nil {
			if winner != "" {
				t.Fatal("two different resources were both bound")
			}
			winner = result.resource
		} else if errors.Is(result.err, ErrPaymentOrderConflict) {
			conflicts++
		} else {
			t.Fatal(result.err)
		}
	}
	if winner == "" || conflicts != 1 {
		t.Fatalf("winner=%q conflicts=%d", winner, conflicts)
	}
	persisted, err := FindPaymentOrder(context.Background(), order.TradeNo)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ProviderResourceRef != winner {
		t.Fatalf("binding overwritten: resource=%q winner=%q", persisted.ProviderResourceRef, winner)
	}
}
