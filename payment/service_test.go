package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/model"
	"one-api/payment/types"

	stripeSDK "github.com/stripe/stripe-go/v80"
	"github.com/stripe/stripe-go/v80/webhook"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 第五个网关只实现公共协议；同一资金入口覆盖仅通知和仅查单两种能力。
const fixtureGatewayKind = "contract-fixture"
const fixtureProduct = "contract-fixture.checkout"

type serviceFakeState struct {
	mode            string
	preparationMode string
	prepares        atomic.Int32
	queries         atomic.Int32
	closes          atomic.Int32
	mu              sync.Mutex
	frozen          []types.FrozenOrder
	prepare         func(context.Context, types.FrozenOrder) (types.PrepareResult, error)
	query           func(types.OrderRef, types.GatewaySnapshot) (types.PaymentObservation, error)
	close           func(types.OrderRef) (types.CloseResult, error)
}
type serviceFakeFactory struct{ state *serviceFakeState }

func (*serviceFakeFactory) Descriptor() types.GatewayDescriptor {
	return types.GatewayDescriptor{Kind: fixtureGatewayKind, Products: []string{fixtureProduct}}
}
func (*serviceFakeFactory) ValidateConfig(context.Context, types.GatewayConfigInput) (types.GatewayBinding, error) {
	return types.GatewayBinding{}, nil
}
func (f *serviceFakeFactory) NewClient(_ context.Context, snapshot types.GatewaySnapshot) (GatewayClient, error) {
	client := &serviceFakeClient{state: f.state, snapshot: snapshot}
	switch f.state.mode {
	case "callback":
		return &serviceCallbackFake{client}, nil
	case "query":
		return &serviceQueryFake{client}, nil
	case "close":
		return &serviceCloseFake{&serviceQueryFake{client}}, nil
	case "both":
		return &serviceBothFake{&serviceCallbackFake{client}}, nil
	default:
		return client, nil
	}
}

type serviceFakeClient struct {
	state    *serviceFakeState
	snapshot types.GatewaySnapshot
}

func (c *serviceFakeClient) Capabilities(product string) (types.Capabilities, error) {
	if product != fixtureProduct {
		return types.Capabilities{}, types.ErrUnsupported
	}
	return types.Capabilities{Currencies: []string{"CNY", "USD"}, PreparationMode: c.state.preparationMode, CanReceiveCallbacks: c.state.mode == "callback" || c.state.mode == "both", QueryByMerchantRef: c.state.mode == "query" || c.state.mode == "both" || c.state.mode == "close", CanClose: c.state.mode == "close", DisplayWindow: 3 * time.Hour, ExpiryMode: "unknown", ActionKinds: []string{"redirect", "form", "qr_code"}}, nil
}
func (c *serviceFakeClient) PreparePayment(ctx context.Context, order types.FrozenOrder) (types.PrepareResult, error) {
	c.state.prepares.Add(1)
	c.state.mu.Lock()
	c.state.frozen = append(c.state.frozen, order)
	c.state.mu.Unlock()
	persisted, err := model.FindPaymentOrder(ctx, order.TradeNo)
	if err != nil || persisted.PreparationState != types.PreparationClaimed {
		return types.PrepareResult{}, fmt.Errorf("准备付款前没有持久化执行权: %v", err)
	}
	if c.state.prepare != nil {
		return c.state.prepare(ctx, order)
	}
	return types.PrepareResult{Outcome: types.PreparationReady, ProviderResourceRef: "resource_" + order.TradeNo, NextAction: types.NextAction{Kind: "redirect", Redirect: &types.RedirectAction{URL: "https://fixture.example/pay/" + order.TradeNo}}}, nil
}
func (c *serviceFakeClient) queryPayment(_ context.Context, ref types.OrderRef) (types.PaymentObservation, error) {
	c.state.queries.Add(1)
	if c.state.query != nil {
		return c.state.query(ref, c.snapshot)
	}
	return types.PaymentObservation{Source: types.SourceAuthenticatedQuery, TradeNo: ref.TradeNo, GatewayID: c.snapshot.GatewayID, Identity: c.snapshot.Identity, TransactionNamespace: c.snapshot.TransactionNamespace, ProviderResourceRef: ref.ProviderResourceRef, State: types.ObservationUnpaid, VerificationRef: "fixture-auth-query"}, nil
}

type serviceCallbackFake struct{ *serviceFakeClient }

func (*serviceCallbackFake) VerifyNotification(_ context.Context, request types.CallbackRequest) (types.NotificationResult, error) {
	if request.Headers.Get("Fixture-Signature") != "verified" {
		return types.NotificationResult{}, errors.New("invalid fixture signature")
	}
	var notification types.NotificationResult
	err := json.Unmarshal(request.Body, &notification)
	return notification, err
}
func (*serviceCallbackFake) CallbackResponse(outcome types.CallbackOutcome) types.CallbackResponse {
	status := 200
	switch outcome {
	case types.CallbackRejected:
		status = 400
	case types.CallbackRetryableFailure:
		status = 503
	}
	return types.CallbackResponse{StatusCode: status, Body: []byte(outcome)}
}

type serviceQueryFake struct{ *serviceFakeClient }

func (c *serviceQueryFake) QueryPayment(ctx context.Context, ref types.OrderRef) (types.PaymentObservation, error) {
	return c.queryPayment(ctx, ref)
}

type serviceBothFake struct{ *serviceCallbackFake }

type serviceCloseFake struct{ *serviceQueryFake }

func (c *serviceCloseFake) ClosePayment(_ context.Context, ref types.OrderRef) (types.CloseResult, error) {
	c.state.closes.Add(1)
	if c.state.close != nil {
		return c.state.close(ref)
	}
	return types.CloseResult{State: types.ObservationClosed}, nil
}

func (c *serviceBothFake) QueryPayment(ctx context.Context, ref types.OrderRef) (types.PaymentObservation, error) {
	return c.queryPayment(ctx, ref)
}

type serviceFixture struct {
	payment *model.Payment
	state   *serviceFakeState
	options *config.OptionManager
	clock   atomic.Int64
}

func newServiceFixture(t *testing.T, mode string) *serviceFixture {
	t.Helper()
	oldDB, oldResources, oldOptions, oldNow := model.DB, Resources, config.GlobalOption, Now
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "payment.db")+"?_busy_timeout=10000&_txlock=immediate"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.User{}, &model.UserGroup{}, &model.Payment{}, &model.Order{}, &model.Log{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(8)
	model.DB = db
	Resources = NewResourcePool(context.Background())
	fixture := &serviceFixture{state: &serviceFakeState{mode: mode, preparationMode: types.ServerCreate}}
	fixture.clock.Store(time.Now().UTC().UnixNano())
	Now = func() time.Time { return time.Unix(0, fixture.clock.Load()).UTC() }
	manager := config.NewOptionManager()
	minimum := 1
	rate, quota := float64(7), float64(100)
	discount, server, name := `{"101":0.85,"10":1}`, "https://initial.example/", "初始名称"
	manager.RegisterInt("PaymentMinAmount", &minimum)
	manager.RegisterFloat("PaymentUSDRate", &rate)
	manager.RegisterFloat("QuotaPerUnit", &quota)
	manager.RegisterString("RechargeDiscount", &discount)
	manager.RegisterString("ServerAddress", &server)
	manager.RegisterString("SystemName", &name)
	if _, err = manager.PublishRuntimeOverrides(1, map[string]string{
		"PaymentMinAmount": "1", "PaymentUSDRate": "7", "QuotaPerUnit": "100", "RechargeDiscount": discount,
		"ServerAddress": server, "SystemName": name,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.options = manager
	config.GlobalOption = manager
	registry.RLock()
	oldFactory, hadFactory := registry.factories[fixtureGatewayKind]
	registry.RUnlock()
	RegisterFactory(&serviceFakeFactory{state: fixture.state})
	t.Cleanup(func() {
		Resources.Close()
		Resources = oldResources
		model.DB = oldDB
		config.GlobalOption = oldOptions
		Now = oldNow
		registry.Lock()
		if hadFactory {
			registry.factories[fixtureGatewayKind] = oldFactory
		} else {
			delete(registry.factories, fixtureGatewayKind)
		}
		registry.Unlock()
		sqlDB.Close()
	})
	user := model.User{Id: 1, Username: "fixture-user", Password: "unused", AccessToken: "fixture-access", Status: config.UserStatusEnabled, Quota: 10, Group: "default"}
	if err = db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	enabled := true
	identity := types.GatewayIdentity{Kind: fixtureGatewayKind, Environment: "test", MerchantAccount: "fixture-merchant", ProtocolProfile: "fixture.v1"}
	fixture.payment = &model.Payment{ID: 1, UUID: "fixture-payment", Type: fixtureGatewayKind, Name: "第五家", Currency: model.CurrencyTypeCNY, PercentFee: 0.03, Enable: &enabled, Identity: identity, TransactionNamespace: types.Namespace(fixtureGatewayKind, "fixture-merchant", "test"), DefaultProduct: fixtureProduct, CredentialRevision: 1, SetupStatus: "ready", Config: `{}`}
	if err = db.Create(fixture.payment).Error; err != nil {
		t.Fatal(err)
	}
	return fixture
}
func (f *serviceFixture) request(key string) CreateOrderRequest {
	return CreateOrderRequest{UUID: f.payment.UUID, Amount: 101, RequestKey: key}
}
func (f *serviceFixture) advance(delta time.Duration) { f.clock.Add(int64(delta)) }
func loadServiceOrder(t *testing.T, trade string) *model.Order {
	t.Helper()
	order, err := model.FindPaymentOrder(context.Background(), trade)
	if err != nil {
		t.Fatal(err)
	}
	return order
}
func successFixture(order *model.Order, source string) types.PaymentObservation {
	total := order.Money()
	return types.PaymentObservation{Source: source, TradeNo: order.TradeNo, GatewayID: order.GatewayId, Identity: order.Identity, TransactionNamespace: order.TransactionNamespace, ProviderResourceRef: order.ProviderResourceRef, ProviderTransactionID: "transaction_" + order.TradeNo, OrderTotal: &total, State: types.ObservationSucceeded, VerificationRef: "fixture-verified-payment"}
}
func callbackFixture(t *testing.T, service *PaymentService, result types.NotificationResult) (types.CallbackResponse, error) {
	t.Helper()
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return service.Callback(context.Background(), types.CallbackRequest{Method: http.MethodPost, Headers: http.Header{"Fixture-Signature": []string{"verified"}}, Body: body})
}
func assertServiceCredit(t *testing.T, quota int) {
	t.Helper()
	var user model.User
	if err := model.DB.Unscoped().First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != quota {
		t.Fatalf("余额=%d，期望=%d", user.Quota, quota)
	}
}

func TestFifthGatewayConfirmationCapabilities(t *testing.T) {
	for _, mode := range []string{"callback", "query", "none"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newServiceFixture(t, mode)
			view, err := CreateOrder(context.Background(), 1, fixture.request("capabilities"))
			if mode == "none" {
				if err == nil || fixture.state.prepares.Load() != 0 {
					t.Fatalf("无确认能力网关被启用: %+v %v", view, err)
				}
				var count int64
				model.DB.Model(&model.Order{}).Count(&count)
				if count != 0 {
					t.Fatal("无确认能力仍落单")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			order := loadServiceOrder(t, view.TradeNo)
			if mode == "callback" {
				if _, err := QueryOrder(context.Background(), 1, view.TradeNo); !errors.Is(err, types.ErrUnsupported) {
					t.Fatalf("仅通知产品伪装支持查询: %v", err)
				}
				if _, err := CloseOrder(context.Background(), 1, view.TradeNo); !errors.Is(err, types.ErrUnsupported) {
					t.Fatalf("仅通知产品伪装支持关单: %v", err)
				}
				evidence := successFixture(order, types.SourceVerifiedCallback)
				processing := evidence
				processing.State, processing.OrderTotal, processing.ProviderTransactionID = types.ObservationProcessing, nil, ""
				response, err := callbackFixture(t, &PaymentService{Payment: fixture.payment}, types.NotificationResult{Observation: &processing})
				if err != nil || response.StatusCode != 200 {
					t.Fatalf("合法待支付事件未接受: %+v %v", response, err)
				}
				assertServiceCredit(t, 10)
				response, err = callbackFixture(t, &PaymentService{Payment: fixture.payment}, types.NotificationResult{Observation: &evidence})
				if err != nil || response.StatusCode != 200 {
					t.Fatalf("callback %+v %v", response, err)
				}
				if _, err := QueryOrder(context.Background(), 1, view.TradeNo); err != nil {
					t.Fatalf("已付只读查询: %v", err)
				}
			} else {
				fixture.state.query = func(ref types.OrderRef, _ types.GatewaySnapshot) (types.PaymentObservation, error) {
					return successFixture(loadServiceOrder(t, ref.TradeNo), types.SourceAuthenticatedQuery), nil
				}
				queried, err := QueryOrder(context.Background(), 1, view.TradeNo)
				if err != nil || queried.PaymentState != types.PaymentPaid {
					t.Fatalf("query %+v %v", queried, err)
				}
			}
			assertServiceCredit(t, 10110)
		})
	}
}

func TestCreateOrderPersistsBeforeProviderAndRecoversNew(t *testing.T) {
	for _, failure := range []string{"insert", "claim"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newServiceFixture(t, "callback")
			trigger := "CREATE TRIGGER fail_order BEFORE INSERT ON orders BEGIN SELECT RAISE(FAIL, 'forced insert failure'); END"
			if failure == "claim" {
				trigger = "CREATE TRIGGER fail_order BEFORE UPDATE ON orders WHEN NEW.preparation_state = 'claimed' BEGIN SELECT RAISE(FAIL, 'forced claim failure'); END"
			}
			if err := model.DB.Exec(trigger).Error; err != nil {
				t.Fatal(err)
			}
			req := fixture.request("persist-first")
			if _, err := CreateOrder(context.Background(), 1, req); err == nil {
				t.Fatal("数据库失败没有返回错误")
			}
			if fixture.state.prepares.Load() != 0 {
				t.Fatal("未取得持久执行权就调用了网关")
			}
			var before model.Order
			if failure == "claim" {
				if err := model.DB.Where("request_key = ?", req.RequestKey).First(&before).Error; err != nil || before.PreparationState != types.PreparationNew {
					t.Fatalf("保存的 new 丢失: %+v %v", before, err)
				}
			}
			if err := model.DB.Exec("DROP TRIGGER fail_order").Error; err != nil {
				t.Fatal(err)
			}
			view, err := CreateOrder(context.Background(), 1, req)
			if err != nil || view.PreparationState != types.PreparationReady || fixture.state.prepares.Load() != 1 {
				t.Fatalf("恢复失败 %+v %v", view, err)
			}
			if before.TradeNo != "" && before.TradeNo != view.TradeNo {
				t.Fatal("new 恢复创建了第二单")
			}
		})
	}
}

func TestStableRequestFreezesQuoteAndRuntimeInputs(t *testing.T) {
	fixture := newServiceFixture(t, "callback")
	req := fixture.request("stable")
	view, err := CreateOrder(context.Background(), 1, req)
	if err != nil {
		t.Fatal(err)
	}
	order := loadServiceOrder(t, view.TradeNo)
	if view.OrderTotal.Minor != 61898 || view.OrderTotal.Currency != "CNY" || view.Quota != 10100 || order.ExpectedAmountMinor != 61898 {
		t.Fatalf("最终一次舍入失败: %+v", view)
	}
	if _, err = fixture.options.PublishRuntimeOverrides(2, map[string]string{"ServerAddress": "https://updated.example/", "SystemName": "更新名称", "PaymentUSDRate": "8", "QuotaPerUnit": "200", "RechargeDiscount": `{"101":0.9}`}); err != nil {
		t.Fatal(err)
	}
	if err = model.DB.Model(fixture.payment).Update("percent_fee", 0.1).Error; err != nil {
		t.Fatal(err)
	}
	duplicate, err := CreateOrder(context.Background(), 1, req)
	if err != nil || duplicate.TradeNo != view.TradeNo || duplicate.OrderTotal != view.OrderTotal || duplicate.Quota != view.Quota || !duplicate.LocalDisplayUntil.Equal(view.LocalDisplayUntil) {
		t.Fatalf("同键没有保留冻结报价 %+v %v", duplicate, err)
	}
	changed := req
	changed.Amount++
	if _, err = CreateOrder(context.Background(), 1, changed); !errors.Is(err, model.ErrPaymentOrderConflict) {
		t.Fatalf("同键异输入未冲突 %v", err)
	}
	next, err := CreateOrder(context.Background(), 1, fixture.request("new-request"))
	if err != nil || next.TradeNo == view.TradeNo || next.OrderTotal == view.OrderTotal {
		t.Fatalf("新请求未采用新报价 %+v %v", next, err)
	}
	fixture.state.mu.Lock()
	defer fixture.state.mu.Unlock()
	if len(fixture.state.frozen) != 2 {
		t.Fatalf("prepare次数=%d", len(fixture.state.frozen))
	}
	first, last := fixture.state.frozen[0], fixture.state.frozen[1]
	if first.Total.Minor != 61898 || first.Input.NotifyURL != "https://initial.example/api/payment/notify/fixture-payment" || first.Input.Description != "初始名称" || last.Input.NotifyURL != "https://updated.example/api/payment/notify/fixture-payment" || last.Input.Description != "更新名称" {
		t.Fatalf("运行时配置没有在落单时冻结: %+v / %+v", first, last)
	}
}

func TestConcurrentSameRequestRunsPreparationOnce(t *testing.T) {
	fixture := newServiceFixture(t, "callback")
	var group sync.WaitGroup
	results := make(chan *OrderView, 8)
	failures := make(chan error, 8)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			view, err := CreateOrder(context.Background(), 1, fixture.request("concurrent"))
			results <- view
			failures <- err
		}()
	}
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	trade := ""
	for view := range results {
		if view == nil {
			t.Fatal("empty view")
		}
		if trade == "" {
			trade = view.TradeNo
		}
		if trade != view.TradeNo {
			t.Fatal("同请求键得到多单")
		}
	}
	var count int64
	model.DB.Model(&model.Order{}).Count(&count)
	if count != 1 || fixture.state.prepares.Load() != 1 {
		t.Fatalf("orders=%d prepares=%d", count, fixture.state.prepares.Load())
	}
}

func TestClaimDeadlineDoesNotReplayAndLateResultCannotUndoPaid(t *testing.T) {
	fixture := newServiceFixture(t, "callback")
	entered := make(chan types.FrozenOrder, 1)
	release := make(chan struct{})
	releasePreparation := sync.OnceFunc(func() { close(release) })
	defer releasePreparation()
	fixture.state.prepare = func(_ context.Context, order types.FrozenOrder) (types.PrepareResult, error) {
		entered <- order
		<-release
		return types.PrepareResult{Outcome: types.PreparationReady, ProviderResourceRef: "resource_" + order.TradeNo, NextAction: types.NextAction{Kind: "redirect", Redirect: &types.RedirectAction{URL: "https://fixture.example/pay"}}}, nil
	}
	result := make(chan error, 1)
	go func() { _, err := CreateOrder(context.Background(), 1, fixture.request("claimed")); result <- err }()
	var orderInput types.FrozenOrder
	select {
	case orderInput = <-entered:
	case err := <-result:
		t.Fatalf("准备执行没有开始: %v", err)
	}
	active, err := Status(context.Background(), 1, orderInput.TradeNo)
	if err != nil || active.PreparationState != types.PreparationClaimed {
		releasePreparation()
		t.Fatalf("活执行者被提前恢复 %+v %v", active, err)
	}
	fixture.advance(OperationTimeout + time.Second)
	recovered, err := Status(context.Background(), 1, orderInput.TradeNo)
	if err != nil || recovered.PreparationState != types.PreparationUnknown {
		releasePreparation()
		t.Fatalf("claimed 未收敛 unknown %+v %v", recovered, err)
	}
	retried, err := CreateOrder(context.Background(), 1, fixture.request("claimed"))
	if err != nil || retried.PreparationState != types.PreparationUnknown || fixture.state.prepares.Load() != 1 {
		releasePreparation()
		t.Fatalf("unknown 被重放 %+v %v", retried, err)
	}
	evidence := successFixture(loadServiceOrder(t, orderInput.TradeNo), types.SourceVerifiedCallback)
	evidence.ProviderResourceRef = "resource_" + orderInput.TradeNo
	response, err := callbackFixture(t, &PaymentService{Payment: fixture.payment}, types.NotificationResult{Observation: &evidence})
	releasePreparation()
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("迟到付款失败 %+v %v", response, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	final, err := Status(context.Background(), 1, orderInput.TradeNo)
	if err != nil || final.PaymentState != types.PaymentPaid || final.NextAction.Kind != "none" || fixture.state.prepares.Load() != 1 {
		t.Fatalf("迟到准备结果倒退 paid %+v %v", final, err)
	}
	assertServiceCredit(t, 10110)
}

func TestUnknownCreationAndHistoricalCallbackRemainPayable(t *testing.T) {
	fixture := newServiceFixture(t, "callback")
	fixture.state.prepare = func(context.Context, types.FrozenOrder) (types.PrepareResult, error) {
		return types.PrepareResult{Outcome: types.PreparationUnknown}, context.DeadlineExceeded
	}
	view, err := CreateOrder(context.Background(), 1, fixture.request("unknown"))
	if err != nil || view.PreparationState != types.PreparationUnknown {
		t.Fatalf("unknown丢失 %+v %v", view, err)
	}
	fixture.advance(4 * time.Hour)
	if err = model.DB.Delete(fixture.payment).Error; err != nil {
		t.Fatal(err)
	}
	Resources.Close()
	Resources = NewResourcePool(context.Background())
	history, err := NewHistoricalPaymentService(fixture.payment.UUID)
	if err != nil {
		t.Fatal(err)
	}
	evidence := successFixture(loadServiceOrder(t, view.TradeNo), types.SourceVerifiedCallback)
	for range 2 {
		response, err := callbackFixture(t, history, types.NotificationResult{Observation: &evidence})
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("历史迟到通知 %+v %v", response, err)
		}
	}
	if _, err = CreateOrder(context.Background(), 1, fixture.request("disabled-new")); err == nil {
		t.Fatal("软删网关接受新单")
	}
	assertServiceCredit(t, 10110)
	final, err := Status(context.Background(), 1, view.TradeNo)
	if err != nil || final.PaymentState != types.PaymentPaid || final.NextAction.Kind != "none" || fixture.state.prepares.Load() != 1 {
		t.Fatalf("迟到付款 %+v %v", final, err)
	}
}

func TestOrderQueryUsesSharedBudgetAndDoesNotRecreate(t *testing.T) {
	fixture := newServiceFixture(t, "query")
	view, err := CreateOrder(context.Background(), 1, fixture.request("query-budget"))
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := QueryOrder(context.Background(), 1, view.TradeNo)
			failures <- err
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if fixture.state.queries.Load() != 1 {
		t.Fatalf("并发查询扩大频率: %d", fixture.state.queries.Load())
	}
	fixture.advance(31 * time.Second)
	if _, err = QueryOrder(context.Background(), 1, view.TradeNo); err != nil {
		t.Fatal(err)
	}
	fixture.advance(11 * time.Minute)
	if _, err = QueryOrder(context.Background(), 1, view.TradeNo); err != nil {
		t.Fatal(err)
	}
	order := loadServiceOrder(t, view.TradeNo)
	if order.NextQueryAt == nil || order.NextQueryAt.Sub(Now()) != 5*time.Minute {
		t.Fatalf("10分钟后预算=%v", order.NextQueryAt)
	}
	fixture.advance(time.Minute)
	if _, err = QueryOrder(context.Background(), 1, view.TradeNo); err != nil {
		t.Fatal(err)
	}
	if fixture.state.queries.Load() != 3 || fixture.state.prepares.Load() != 1 {
		t.Fatalf("queries=%d prepares=%d", fixture.state.queries.Load(), fixture.state.prepares.Load())
	}
	fixture.state.query = func(types.OrderRef, types.GatewaySnapshot) (types.PaymentObservation, error) {
		return types.PaymentObservation{}, errors.New("provider query missing")
	}
	fixture.advance(5 * time.Minute)
	if _, err = QueryOrder(context.Background(), 1, view.TradeNo); err == nil {
		t.Fatal("查无结果被伪装成功")
	}
	if fixture.state.prepares.Load() != 1 || loadServiceOrder(t, view.TradeNo).PaymentState == types.PaymentPaid {
		t.Fatal("查询失败重建或制造成功")
	}
}

func TestStatusRemovesExpiredAndProcessingActionsWithoutProviderWork(t *testing.T) {
	for _, boundary := range []string{"local", "action", "processing", "wrong-user"} {
		t.Run(boundary, func(t *testing.T) {
			fixture := newServiceFixture(t, "callback")
			fixture.state.preparationMode = types.BrowserHandoff
			fixture.state.prepare = func(_ context.Context, order types.FrozenOrder) (types.PrepareResult, error) {
				action := types.NextAction{Kind: "form", Form: &types.FormAction{Method: "POST", ActionURL: "https://fixture.example/submit", Fields: map[string]string{"trade_no": order.TradeNo}}}
				if boundary == "action" {
					deadline := Now().Add(time.Minute)
					action.ValidUntil = &deadline
				}
				return types.PrepareResult{Outcome: types.PreparationReady, NextAction: action}, nil
			}
			view, err := CreateOrder(context.Background(), 1, fixture.request("window"))
			if err != nil || view.NextAction.Kind != "form" || view.NextAction.ValidUntil == nil {
				t.Fatalf("有效表单不可交付 %+v %v", view, err)
			}
			order := loadServiceOrder(t, view.TradeNo)
			if order.ProviderResourceRef != "" || order.ProviderPayableUntil != nil {
				t.Fatal("本地表单伪造远端引用/截止")
			}
			userID := 1
			switch boundary {
			case "local":
				fixture.advance(4 * time.Hour)
			case "action":
				fixture.advance(2 * time.Minute)
			case "processing":
				evidence := successFixture(order, types.SourceVerifiedCallback)
				evidence.State = types.ObservationProcessing
				if _, _, err = ApplyPaymentObservation(context.Background(), evidence); err != nil {
					t.Fatal(err)
				}
			case "wrong-user":
				userID = 2
			}
			current, err := Status(context.Background(), userID, view.TradeNo)
			if boundary == "wrong-user" {
				if !errors.Is(err, model.ErrPaymentOrderUnavailable) {
					t.Fatalf("越权读取 %v", err)
				}
				return
			}
			if err != nil || current.NextAction.Kind != "none" || fixture.state.prepares.Load() != 1 || fixture.state.queries.Load() != 0 {
				t.Fatalf("状态读取交付旧动作或产生provider操作 %+v %v", current, err)
			}
			if boundary != "processing" && current.WindowState != types.WindowLocalExpired {
				t.Fatalf("未派生本地到期 %+v", current)
			}
		})
	}
}

func TestCallbackDatabaseFailureGetsRetryableAckAndCanRetryOnce(t *testing.T) {
	fixture := newServiceFixture(t, "callback")
	view, err := CreateOrder(context.Background(), 1, fixture.request("callback-rollback"))
	if err != nil {
		t.Fatal(err)
	}
	evidence := successFixture(loadServiceOrder(t, view.TradeNo), types.SourceVerifiedCallback)
	if err = model.DB.Exec("CREATE TRIGGER fail_credit BEFORE UPDATE ON users BEGIN SELECT RAISE(FAIL, 'credit failed'); END").Error; err != nil {
		t.Fatal(err)
	}
	response, err := callbackFixture(t, &PaymentService{Payment: fixture.payment}, types.NotificationResult{Observation: &evidence})
	if err == nil || response.StatusCode != 503 {
		t.Fatalf("数据库失败ACK %+v %v", response, err)
	}
	assertServiceCredit(t, 10)
	if loadServiceOrder(t, view.TradeNo).PaymentState == types.PaymentPaid {
		t.Fatal("付款状态没有回滚")
	}
	if err = model.DB.Exec("DROP TRIGGER fail_credit").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response, err = callbackFixture(t, &PaymentService{Payment: fixture.payment}, types.NotificationResult{Observation: &evidence})
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("重投 %+v %v", response, err)
		}
	}
	for _, state := range []string{types.ObservationProcessing, types.ObservationAttemptFailed, types.ObservationClosed} {
		late := evidence
		late.State = state
		response, err = callbackFixture(t, &PaymentService{Payment: fixture.payment}, types.NotificationResult{Observation: &late})
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("合法迟到事件ACK %+v %v", response, err)
		}
		if loadServiceOrder(t, view.TradeNo).PaymentState != types.PaymentPaid {
			t.Fatalf("%s倒退已入账订单", state)
		}
	}
	assertServiceCredit(t, 10110)
}

func TestVerifiedSparseCallbackQueriesOriginalSessionBeforeCredit(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			fixture := newServiceFixture(t, "both")
			if unknown {
				fixture.state.prepare = func(context.Context, types.FrozenOrder) (types.PrepareResult, error) {
					return types.PrepareResult{Outcome: types.PreparationUnknown}, context.DeadlineExceeded
				}
			}
			view, err := CreateOrder(context.Background(), 1, fixture.request("sparse-callback"))
			if err != nil {
				t.Fatal(err)
			}
			order := loadServiceOrder(t, view.TradeNo)
			resource := order.ProviderResourceRef
			if resource == "" {
				resource = "resource_discovered_after_timeout"
			}
			fixture.state.query = func(ref types.OrderRef, snapshot types.GatewaySnapshot) (types.PaymentObservation, error) {
				if ref.ProviderResourceRef != resource || snapshot.GatewayID != order.GatewayId {
					return types.PaymentObservation{}, errors.New("wrong authenticated query binding")
				}
				e := successFixture(order, types.SourceAuthenticatedQuery)
				e.ProviderResourceRef = resource
				return e, nil
			}
			response, err := callbackFixture(t, &PaymentService{Payment: fixture.payment}, types.NotificationResult{QueryRef: &types.OrderRef{ProductCode: fixtureProduct, ProviderResourceRef: resource}})
			if err != nil || response.StatusCode != 200 || fixture.state.queries.Load() != 1 {
				t.Fatalf("缺TradeNo回调无法认证补证: %+v %v", response, err)
			}
			assertServiceCredit(t, 10110)
			if actual := loadServiceOrder(t, view.TradeNo); actual.ProviderResourceRef != resource {
				t.Fatalf("未绑定认证发现的资源 %+v", actual)
			}
		})
	}
}

func TestAmbiguousClaimCommitDoesNotAuthorizeProvider(t *testing.T) {
	fixture := newServiceFixture(t, "callback")
	callbackName := "fixture:claim_commit_unknown"
	if err := model.DB.Callback().Update().After("gorm:commit_or_rollback_transaction").Register(callbackName, func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]any)
		if ok && tx.Statement.Table == "orders" && values["preparation_state"] == types.PreparationClaimed {
			tx.AddError(errors.New("commit result unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { model.DB.Callback().Update().Remove(callbackName) })
	request := fixture.request("claim-commit-unknown")
	if _, err := CreateOrder(context.Background(), 1, request); err == nil {
		t.Fatal("提交结果不明未返回错误")
	}
	if fixture.state.prepares.Load() != 0 {
		t.Fatal("提交结果不明后执行了SDK")
	}
	order, err := model.FindOrderRequest(context.Background(), 1, request.RequestKey)
	if err != nil || order.PreparationState != types.PreparationClaimed {
		t.Fatalf("提交已发生fixture失败: %+v %v", order, err)
	}
	if err = model.DB.Callback().Update().Remove(callbackName); err != nil {
		t.Fatal(err)
	}
	same, err := CreateOrder(context.Background(), 1, request)
	if err != nil || same.PreparationState != types.PreparationClaimed || fixture.state.prepares.Load() != 0 {
		t.Fatalf("重读自己的claim被当作执行许可 %+v %v", same, err)
	}
	fixture.advance(OperationTimeout + time.Second)
	expired, err := CreateOrder(context.Background(), 1, request)
	if err != nil || expired.PreparationState != types.PreparationUnknown || fixture.state.prepares.Load() != 0 {
		t.Fatalf("不明提交到期被重放 %+v %v", expired, err)
	}
}

func TestReconcileSkipsCallbackOnlyOrdersBeforeBatchLimit(t *testing.T) {
	fixture := newServiceFixture(t, "callback")
	fixture.state.preparationMode = types.BrowserHandoff
	fixture.state.prepare = func(_ context.Context, order types.FrozenOrder) (types.PrepareResult, error) {
		return types.PrepareResult{Outcome: types.PreparationReady, NextAction: types.NextAction{Kind: "form", Form: &types.FormAction{Method: "POST", ActionURL: "https://fixture.example/form", Fields: map[string]string{"trade_no": order.TradeNo}}}}, nil
	}
	first, err := CreateOrder(context.Background(), 1, fixture.request("callback-only-0"))
	if err != nil {
		t.Fatal(err)
	}
	original := loadServiceOrder(t, first.TradeNo)
	rows := make([]model.Order, 99)
	for i := range rows {
		rows[i] = *original
		rows[i].ID = 0
		rows[i].TradeNo = fmt.Sprintf("callback-only-trade-%d", i+1)
		rows[i].RequestKey = fmt.Sprintf("callback-only-%d", i+1)
	}
	if err = model.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	fixture.state.mode = "query"
	fixture.state.preparationMode = types.ServerCreate
	fixture.state.prepare = nil
	Resources.Close()
	Resources = NewResourcePool(context.Background())
	queryable, err := CreateOrder(context.Background(), 1, fixture.request("queryable-after-one-hundred"))
	if err != nil {
		t.Fatal(err)
	}
	if err = Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	queried := loadServiceOrder(t, queryable.TradeNo)
	if queried.ID != 101 || queried.QueryCount != 1 || fixture.state.queries.Load() != 1 {
		t.Fatalf("可查订单被不可查前缀饿死: id=%d query_count=%d calls=%d", queried.ID, queried.QueryCount, fixture.state.queries.Load())
	}
	var polluted int64
	if err = model.DB.Model(&model.Order{}).Where("id < ? AND (query_count <> 0 OR next_query_at IS NOT NULL)", queried.ID).Count(&polluted).Error; err != nil {
		t.Fatal(err)
	}
	if polluted != 0 {
		t.Fatal("不可查订单消耗了查询预算")
	}
}

func TestCloseOrderRequiresFreshQueryPermission(t *testing.T) {
	for _, delay := range []time.Duration{0, time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			fixture := newServiceFixture(t, "close")
			view, err := CreateOrder(context.Background(), 1, fixture.request("close-throttled"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = QueryOrder(context.Background(), 1, view.TradeNo); err != nil {
				t.Fatal(err)
			}
			fixture.advance(delay)
			_, err = CloseOrder(context.Background(), 1, view.TradeNo)
			if err == nil || fixture.state.closes.Load() != 0 || fixture.state.queries.Load() != 1 {
				t.Fatalf("查询预算未允许仍关单: err=%v queries=%d closes=%d", err, fixture.state.queries.Load(), fixture.state.closes.Load())
			}
		})
	}
}

func TestClosePaidResponseAuthenticatesSameOrderAndCreditsOnce(t *testing.T) {
	fixture := newServiceFixture(t, "close")
	view, err := CreateOrder(context.Background(), 1, fixture.request("close-already-paid"))
	if err != nil {
		t.Fatal(err)
	}
	order := loadServiceOrder(t, view.TradeNo)
	fixture.state.query = func(ref types.OrderRef, _ types.GatewaySnapshot) (types.PaymentObservation, error) {
		if ref.TradeNo != order.TradeNo || ref.ProviderResourceRef != order.ProviderResourceRef {
			return types.PaymentObservation{}, errors.New("query changed payment owner")
		}
		e := successFixture(order, types.SourceAuthenticatedQuery)
		if fixture.state.queries.Load() == 1 {
			e.State = types.ObservationUnpaid
			e.OrderTotal = nil
			e.ProviderTransactionID = ""
		}
		return e, nil
	}
	fixture.state.close = func(ref types.OrderRef) (types.CloseResult, error) {
		if ref.TradeNo != order.TradeNo {
			return types.CloseResult{}, errors.New("close changed payment owner")
		}
		return types.CloseResult{State: types.ObservationSucceeded}, nil
	}
	closed, err := CloseOrder(context.Background(), 1, view.TradeNo)
	if err != nil || closed.PaymentState != types.PaymentPaid || closed.WindowState == types.WindowProviderClosed || fixture.state.queries.Load() != 2 || fixture.state.closes.Load() != 1 {
		t.Fatalf("关单已付没有认证补查 %+v err=%v queries=%d closes=%d", closed, err, fixture.state.queries.Load(), fixture.state.closes.Load())
	}
	assertServiceCredit(t, 10110)
	if _, err = CloseOrder(context.Background(), 1, view.TradeNo); err != nil {
		t.Fatal(err)
	}
	if fixture.state.queries.Load() != 2 || fixture.state.closes.Load() != 1 {
		t.Fatal("已付款后继续触发供应商操作")
	}
	assertServiceCredit(t, 10110)
}

func TestUnknownCloseIsObservedWithoutRepeatingWrite(t *testing.T) {
	for _, transportError := range []bool{false, true} {
		t.Run(fmt.Sprint(transportError), func(t *testing.T) {
			fixture := newServiceFixture(t, "close")
			fixture.state.close = func(types.OrderRef) (types.CloseResult, error) {
				var err error
				if transportError {
					err = context.DeadlineExceeded
				}
				return types.CloseResult{State: types.ObservationUnknown}, err
			}
			view, err := CreateOrder(context.Background(), 1, fixture.request("close-unknown"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = CloseOrder(context.Background(), 1, view.TradeNo)
			if transportError && err == nil {
				t.Fatal("关单超时被伪装成功")
			}
			order := loadServiceOrder(t, view.TradeNo)
			if order.CloseState != "unknown" || order.WindowState == types.WindowProviderClosed || fixture.state.closes.Load() != 1 {
				t.Fatalf("关单不明状态丢失: %+v closes=%d", order, fixture.state.closes.Load())
			}
			fixture.advance(31 * time.Second)
			if _, err = CloseOrder(context.Background(), 1, view.TradeNo); err == nil {
				t.Fatal("已提交关单未说明等待查询")
			}
			if fixture.state.closes.Load() != 1 || fixture.state.prepares.Load() != 1 || fixture.state.queries.Load() != 2 {
				t.Fatalf("unknown触发重放 prepares=%d closes=%d queries=%d", fixture.state.prepares.Load(), fixture.state.closes.Load(), fixture.state.queries.Load())
			}
		})
	}
}

func TestStripeSignedAsyncEventsUseCommonCreditTransaction(t *testing.T) {
	fixture := newServiceFixture(t, "callback")
	enabled := true
	identity := types.GatewayIdentity{Kind: "stripe", Environment: "test", MerchantAccount: "acct_fixture", ProtocolProfile: "stripe.checkout.v1"}
	payment := &model.Payment{ID: 2, UUID: "stripe-real-adapter", Type: "stripe", Currency: model.CurrencyTypeCNY, Enable: &enabled, Identity: identity, TransactionNamespace: types.Namespace("stripe", "acct_fixture", "test"), CredentialRevision: 1, DefaultProduct: "stripe.checkout", SetupStatus: "ready", Config: `{"secret_key":"sk_test_fixture","account_id":"acct_fixture","environment":"test","webhook_secret":"whsec_fixture"}`}
	if err := model.DB.Create(payment).Error; err != nil {
		t.Fatal(err)
	}
	order := &model.Order{UserId: 1, GatewayId: payment.ID, TradeNo: "stripe-async-trade", RequestKey: "stripe-request", RequestFingerprint: "fixture", TransactionNamespace: payment.TransactionNamespace, Identity: identity, ProductCode: payment.DefaultProduct, ExpectedAmountMinor: 61898, OrderCurrency: model.CurrencyTypeCNY, Quota: 10100, PreparationMode: types.ServerCreate, PreparationState: types.PreparationReady, PaymentState: types.PaymentUnconfirmed, WindowState: types.WindowOpen, ProviderResourceRef: "cs_fixture", LocalDisplayUntil: Now().Add(3 * time.Hour)}
	if err := model.DB.Create(order).Error; err != nil {
		t.Fatal(err)
	}
	callback := func(eventType, paymentStatus, sessionStatus string, total int64) (types.CallbackResponse, error) {
		envelope := map[string]any{"id": "evt_" + eventType, "object": "event", "api_version": stripeSDK.APIVersion, "type": eventType, "livemode": false, "data": map[string]any{"object": map[string]any{"id": "cs_fixture", "object": "checkout.session", "client_reference_id": order.TradeNo, "mode": "payment", "status": sessionStatus, "payment_status": paymentStatus, "payment_intent": "pi_fixture", "currency": "cny", "amount_total": total, "livemode": false}}}
		body, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: "whsec_fixture"})
		return (&PaymentService{Payment: payment}).Callback(context.Background(), types.CallbackRequest{Method: http.MethodPost, Headers: http.Header{"Stripe-Signature": []string{signed.Header}}, Body: signed.Payload})
	}
	response, err := callback("checkout.session.completed", "unpaid", "complete", 61898)
	if err != nil || response.StatusCode != 200 || loadServiceOrder(t, order.TradeNo).PaymentState != types.PaymentProcessing {
		t.Fatalf("completed unpaid不应入账: %+v %v", response, err)
	}
	assertServiceCredit(t, 10)
	response, err = callback("checkout.session.async_payment_succeeded", "paid", "complete", 61897)
	if err == nil || response.StatusCode != 400 {
		t.Fatalf("错额Stripe付款被接受 %+v %v", response, err)
	}
	assertServiceCredit(t, 10)
	fixture.advance(4 * time.Hour)
	for range 2 {
		response, err = callback("checkout.session.async_payment_succeeded", "paid", "complete", 61898)
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("迟到Stripe成功失败 %+v %v", response, err)
		}
	}
	confirmed := loadServiceOrder(t, order.TradeNo)
	if confirmed.PaymentState != types.PaymentPaid || confirmed.ProviderTransactionID == nil || *confirmed.ProviderTransactionID != "pi_fixture" || confirmed.ProviderResourceRef != "cs_fixture" {
		t.Fatalf("Stripe交易引用或资金状态错误 %+v", confirmed)
	}
	for _, event := range []string{"checkout.session.async_payment_failed", "checkout.session.expired"} {
		response, err = callback(event, "unpaid", "expired", 61898)
		if err != nil || response.StatusCode != 200 || loadServiceOrder(t, order.TradeNo).PaymentState != types.PaymentPaid {
			t.Fatalf("Stripe迟到失败倒退paid %+v %v", response, err)
		}
	}
	assertServiceCredit(t, 10110)
}
