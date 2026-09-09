package controller

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"net/url"
	"one-api/common/config"
	"one-api/model"
	"one-api/payment"
	"one-api/payment/gateway/epay"
	"one-api/payment/types"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func useOrderCallbackTestDB(t *testing.T) {
	t.Helper()
	original := model.DB
	pool := payment.Resources
	payment.Resources = payment.NewResourcePool(context.Background())
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "callback.db")+"?_busy_timeout=10000&_txlock=immediate"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.Payment{}, &model.User{}, &model.Order{}, &model.UserGroup{}, &model.Log{}); err != nil {
		t.Fatal(err)
	}
	model.DB = db
	sqlDB, _ := db.DB()
	t.Cleanup(func() { payment.Resources.Close(); payment.Resources = pool; model.DB = original; _ = sqlDB.Close() })
}
func callbackFixture(t *testing.T, includeUser bool) (*model.Payment, *model.Order, *epay.Client) {
	t.Helper()
	configJSON := `{"protocol_profile":"epay.form-md5.v1","pay_domain":"https://epay.example","partner_id":"merchant","key":"test-secret","pay_type":"wxpay"}`
	binding, err := (&epay.Factory{}).ValidateConfig(context.Background(), types.GatewayConfigInput{Config: configJSON, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	p := &model.Payment{ID: 7, UUID: "payment-uuid", Type: "epay", Name: "测试支付", Currency: model.CurrencyTypeCNY, Identity: binding.Identity, TransactionNamespace: binding.TransactionNamespace, DefaultProduct: binding.DefaultProduct, DefaultMethod: binding.DefaultMethod, Config: binding.Config, Enable: &enabled, SetupStatus: "ready", CredentialRevision: 1}
	if err := model.DB.Create(p).Error; err != nil {
		t.Fatal(err)
	}
	if includeUser {
		user := model.User{Id: 1, Username: "callback-user", Password: "password123", AccessToken: "callback-access", Quota: 10, Group: "default", Status: config.UserStatusEnabled}
		if err := model.DB.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	order := &model.Order{UserId: 1, GatewayId: 7, TradeNo: "trade-1", RequestKey: "key-1", RequestFingerprint: "frozen", Quota: 100, OrderCurrency: model.CurrencyTypeCNY, ExpectedAmountMinor: 61898, Identity: p.Identity, TransactionNamespace: p.TransactionNamespace, ProductCode: p.DefaultProduct, PreparationState: types.PreparationReady, PreparationMode: types.BrowserHandoff, PaymentState: types.PaymentUnconfirmed, WindowState: types.WindowOpen, LocalDisplayUntil: time.Now().Add(time.Hour)}
	if err := model.DB.Create(order).Error; err != nil {
		t.Fatal(err)
	}
	return p, order, &epay.Client{Key: "test-secret"}
}
func signedCallback(client *epay.Client, money, status string) url.Values {
	params := map[string]string{"pid": "merchant", "out_trade_no": "trade-1", "trade_no": "gateway-1", "money": money, "trade_status": status, "type": "wxpay", "sign_type": "MD5"}
	params["sign"] = client.Sign(params)
	values := url.Values{}
	for key, value := range params {
		values.Set(key, value)
	}
	return values
}
func callbackRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Any("/api/payment/notify/:uuid", PaymentCallback)
	r.GET("/api/user/order/status", func(c *gin.Context) { c.Set("id", 1); CheckOrderStatus(c) })
	return r
}
func TestPaymentCallbackRealAdapterCommitsBeforeACKAndSurvivesHistoricalGateway(t *testing.T) {
	useOrderCallbackTestDB(t)
	p, order, signer := callbackFixture(t, true)
	if err := model.DB.Model(p).Update("enable", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Delete(p).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Model(order).Updates(map[string]any{"window_state": types.WindowLocalExpired, "local_display_until": time.Now().Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	r := callbackRouter()
	for range 2 {
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/payment/notify/payment-uuid?"+signedCallback(signer, "618.98", "TRADE_SUCCESS").Encode(), nil))
		if recorder.Code != http.StatusOK || recorder.Body.String() != "success" {
			t.Fatalf("callback status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var user model.User
		model.DB.First(&user, 1)
		model.DB.First(order, order.ID)
		if user.Quota != 110 || order.PaymentState != types.PaymentPaid {
			t.Fatalf("ACK before committed credit: user=%+v order=%+v", user, order)
		}
	}
}
func TestPaymentCallbackDBFailureRejectsThenRedeliverySucceeds(t *testing.T) {
	useOrderCallbackTestDB(t)
	_, order, signer := callbackFixture(t, true)
	if err := model.DB.Exec("CREATE TRIGGER reject_credit BEFORE UPDATE ON users BEGIN SELECT RAISE(FAIL, 'forced'); END").Error; err != nil {
		t.Fatal(err)
	}
	r := callbackRouter()
	call := func() string {
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/payment/notify/payment-uuid?"+signedCallback(signer, "618.98", "TRADE_SUCCESS").Encode(), nil))
		return recorder.Body.String()
	}
	if got := call(); got != "fail" {
		t.Fatalf("failed transaction ack=%s", got)
	}
	model.DB.First(order, order.ID)
	if order.PaymentState == types.PaymentPaid {
		t.Fatal("failed transaction marked paid")
	}
	model.DB.Exec("DROP TRIGGER reject_credit")
	if got := call(); got != "success" {
		t.Fatalf("redelivery ack=%s", got)
	}
}
func TestPaymentCallbackAmountMismatchAndUnsignedReturnCannotCredit(t *testing.T) {
	useOrderCallbackTestDB(t)
	_, _, signer := callbackFixture(t, true)
	r := callbackRouter()
	for _, query := range []string{signedCallback(signer, "618.97", "TRADE_SUCCESS").Encode(), "trade_no=trade-1&success=true"} {
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/payment/notify/payment-uuid?"+query, nil))
		if recorder.Body.String() != "fail" {
			t.Fatalf("invalid evidence ACK=%s", recorder.Body.String())
		}
	}
	var user model.User
	model.DB.First(&user, 1)
	if user.Quota != 10 {
		t.Fatal("forged or wrong amount credited")
	}
}
func TestOrderStatusUsesOwnedSQLAndNeverReturnsExpiredAction(t *testing.T) {
	useOrderCallbackTestDB(t)
	_, order, _ := callbackFixture(t, true)
	until := time.Now().Add(-time.Second)
	action := types.NextAction{Kind: "form", Form: &types.FormAction{Method: "POST", ActionURL: "https://pay.example", Fields: map[string]string{"money": "618.98"}}, ValidUntil: &until}
	encoded, _ := json.Marshal(action)
	model.DB.Model(order).Updates(map[string]any{"next_action": string(encoded), "local_display_until": until})
	r := callbackRouter()
	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/user/order/status?trade_no=trade-1", nil))
	var response struct {
		Success bool              `json:"success"`
		Data    payment.OrderView `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Success || response.Data.NextAction.Kind != "none" || response.Data.WindowState != types.WindowLocalExpired || response.Data.OrderTotal.Minor != 61898 || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%s", recorder.Body.String())
	}
	if _, err := payment.Status(context.Background(), 2, order.TradeNo); err == nil {
		t.Fatal("cross-user status allowed")
	}
}

func TestPaymentMetadataPatchDoesNotRequireBrokenCredentialsOrOverwriteIdentity(t *testing.T) {
	useOrderCallbackTestDB(t)
	p, _, _ := callbackFixture(t, true)
	if err := model.DB.Model(p).Updates(map[string]any{"config": "invalid-config", "setup_status": "failed"}).Error; err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.PUT("/api/payment/", UpdatePayment)
	for _, body := range []string{`{"id":7,"name":"修复通知","notify_domain":"https://fixed.example"}`, `{"id":7,"enable":false}`, `{"id":7,"sort":0}`} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/api/payment/", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		var response struct {
			Success bool `json:"success"`
		}
		_ = json.Unmarshal(recorder.Body.Bytes(), &response)
		if !response.Success {
			t.Fatalf("partial metadata rejected: %s", recorder.Body.String())
		}
	}
	stored, err := model.GetPaymentByID(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Config != "invalid-config" || stored.CredentialRevision != 1 || !stored.Identity.Equal(p.Identity) || stored.Name != "修复通知" || stored.Sort != 0 || stored.Enable == nil || *stored.Enable {
		t.Fatalf("metadata changed identity/config or lost patch: %+v", stored)
	}
	for _, body := range []string{`{"id":7,"config":"new-key"}`, `{"id":7,"type":"stripe"}`, `{"id":7,"uuid":"other"}`, `{"id":7,"enable":true}`} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/api/payment/", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		var response struct {
			Success bool `json:"success"`
		}
		_ = json.Unmarshal(recorder.Body.Bytes(), &response)
		if response.Success {
			t.Fatalf("protected edit accepted: %s", body)
		}
	}
}
