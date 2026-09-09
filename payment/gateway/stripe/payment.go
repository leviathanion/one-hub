package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"one-api/payment/types"

	sdk "github.com/stripe/stripe-go/v80"
	"github.com/stripe/stripe-go/v80/balance"
	"github.com/stripe/stripe-go/v80/checkout/session"
	"github.com/stripe/stripe-go/v80/webhook"
	"github.com/stripe/stripe-go/v80/webhookendpoint"
)

const (
	profile        = "stripe.checkout.v1"
	product        = "stripe.checkout"
	requestTimeout = 30 * time.Second
)

var requiredEvents = []string{
	"checkout.session.completed",
	"checkout.session.async_payment_succeeded",
	"checkout.session.async_payment_failed",
	"checkout.session.expired",
}

type Factory struct {
	endpointURL string
	httpClient  *http.Client
}

func (Factory) Descriptor() types.GatewayDescriptor {
	return types.GatewayDescriptor{Kind: "stripe", Products: []string{product}}
}

func (f Factory) getBackend() sdk.Backend {
	client := f.httpClient
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	config := &sdk.BackendConfig{HTTPClient: client, MaxNetworkRetries: sdk.Int64(0)}
	if f.endpointURL != "" {
		config.URL = sdk.String(f.endpointURL)
	}
	return sdk.GetBackendWithConfig(sdk.APIBackend, config)
}

func parseConfig(raw string) (StripeConfig, error) {
	var config StripeConfig
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return config, errors.New("Stripe 配置格式无效")
	}
	if strings.TrimSpace(config.SecretKey) == "" {
		return config, errors.New("Stripe 缺少 API 密钥")
	}
	return config, nil
}

func identityFor(config StripeConfig) types.GatewayIdentity {
	return types.GatewayIdentity{Kind: "stripe", Environment: config.Environment, MerchantAccount: config.AccountID, ProtocolProfile: profile}
}
func namespaceFor(config StripeConfig) string {
	return types.Namespace("stripe", config.AccountID, config.Environment)
}

func (f Factory) ValidateConfig(ctx context.Context, input types.GatewayConfigInput) (types.GatewayBinding, error) {
	if input.Product != "" && input.Product != product {
		return types.GatewayBinding{}, types.ErrUnsupported
	}
	if _, err := types.CurrencyExponent(input.Currency); err != nil {
		return types.GatewayBinding{}, err
	}
	config, err := parseConfig(input.Config)
	if err != nil {
		return types.GatewayBinding{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	backend := f.getBackend()
	// SDK 的 account.Get 不接收 context，直接复用 backend 的认证账户请求。
	account := &sdk.Account{}
	if err := backend.Call(http.MethodGet, "/v1/account", config.SecretKey, &sdk.Params{Context: ctx}, account); err != nil {
		return types.GatewayBinding{}, errors.New("Stripe 账户认证失败")
	}
	b, err := (balance.Client{B: backend, Key: config.SecretKey}).Get(&sdk.BalanceParams{Params: sdk.Params{Context: ctx}})
	if err != nil {
		return types.GatewayBinding{}, errors.New("Stripe 模式认证失败")
	}
	environment := "test"
	if b.Livemode {
		environment = "live"
	}
	if account.ID == "" || (config.AccountID != "" && config.AccountID != account.ID) || (config.Environment != "" && config.Environment != environment) {
		return types.GatewayBinding{}, errors.New("Stripe 密钥与预期账户或模式不一致")
	}
	config.AccountID, config.Environment = account.ID, environment
	raw, _ := json.Marshal(config)
	return types.GatewayBinding{Identity: identityFor(config), TransactionNamespace: namespaceFor(config), DefaultProduct: product, Config: string(raw)}, nil
}

func (f Factory) NewClient(_ context.Context, snapshot types.GatewaySnapshot) (types.GatewayClient, error) {
	config, err := parseConfig(snapshot.Config)
	if err != nil {
		return nil, err
	}
	if config.AccountID == "" || (config.Environment != "test" && config.Environment != "live") || !identityFor(config).Equal(snapshot.Identity) || snapshot.TransactionNamespace != namespaceFor(config) {
		return nil, errors.New("Stripe 配置与冻结身份不一致")
	}
	backend := f.getBackend()
	return &Stripe{config: config, snapshot: snapshot, sessions: session.Client{B: backend, Key: config.SecretKey}, endpoints: webhookendpoint.Client{B: backend, Key: config.SecretKey}}, nil
}

type Stripe struct {
	config    StripeConfig
	snapshot  types.GatewaySnapshot
	sessions  session.Client
	endpoints webhookendpoint.Client
}

func (*Stripe) Capabilities(code types.ProductCode) (types.Capabilities, error) {
	if code != product {
		return types.Capabilities{}, types.ErrUnsupported
	}
	return types.Capabilities{Currencies: []string{"CNY", "USD"}, PreparationMode: types.ServerCreate, CanReceiveCallbacks: true, QueryByResourceRef: true, CanClose: true, CanConfigureCallbacks: true, ExpiryMode: "absolute", DisplayWindow: 3 * time.Hour, ActionKinds: []string{"redirect"}}, nil
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && u.User == nil && (u.Scheme == "https" || u.Scheme == "http")
}

func (s *Stripe) PreparePayment(ctx context.Context, order types.FrozenOrder) (types.PrepareResult, error) {
	rejected := types.PrepareResult{Outcome: types.PreparationRejected, NextAction: types.NoAction()}
	if _, err := s.Capabilities(order.ProductCode); err != nil {
		return rejected, err
	}
	if !order.Identity.Equal(s.snapshot.Identity) || order.TransactionNamespace != s.snapshot.TransactionNamespace || order.GatewayID != s.snapshot.GatewayID {
		return rejected, errors.New("Stripe 订单身份不一致")
	}
	if order.MethodPreference != "" {
		return rejected, types.ErrUnsupported
	}
	if err := order.Total.Validate(); err != nil {
		return rejected, err
	}
	if order.TradeNo == "" || !validURL(order.Input.ReturnURL) {
		return rejected, errors.New("Stripe 订单号或返回地址无效")
	}
	remaining := time.Until(order.LocalDisplayUntil)
	if remaining < 30*time.Minute || remaining > 24*time.Hour {
		return rejected, errors.New("Stripe Checkout 截止时间须在创建后的 30 分钟至 24 小时内")
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	params := &sdk.CheckoutSessionParams{
		Params: sdk.Params{Context: ctx}, Mode: sdk.String(string(sdk.CheckoutSessionModePayment)),
		SuccessURL: sdk.String(order.Input.ReturnURL), ClientReferenceID: sdk.String(order.TradeNo),
		ExpiresAt: sdk.Int64(order.LocalDisplayUntil.Unix()),
		LineItems: []*sdk.CheckoutSessionLineItemParams{{Quantity: sdk.Int64(1), PriceData: &sdk.CheckoutSessionLineItemPriceDataParams{
			Currency: sdk.String(strings.ToLower(order.Total.Currency)), UnitAmount: sdk.Int64(order.Total.Minor),
			ProductData: &sdk.CheckoutSessionLineItemPriceDataProductDataParams{Name: sdk.String(order.Input.Description)},
		}}},
	}
	if order.Input.CustomerEmail != "" {
		params.CustomerEmail = sdk.String(order.Input.CustomerEmail)
	}
	created, err := s.sessions.New(params)
	if err != nil {
		var apiError *sdk.Error
		if errors.As(err, &apiError) {
			switch apiError.HTTPStatusCode {
			case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
				rejected.ErrorCode = "stripe_create_rejected"
				return rejected, errors.New("Stripe 拒绝创建支付订单")
			}
		}
		// 写请求一旦交给 transport，结果不明不允许创建重放。
		return types.PrepareResult{Outcome: types.PreparationUnknown, NextAction: types.NoAction(), ErrorCode: "stripe_create_failed"}, errors.New("Stripe 创建结果不明，请查询原订单")
	}
	result := types.PrepareResult{Outcome: types.PreparationUnknown, NextAction: types.NoAction(), ProviderResourceRef: created.ID}
	if created.ID == "" || created.ClientReferenceID != order.TradeNo || created.Mode != sdk.CheckoutSessionModePayment || created.Livemode != (s.config.Environment == "live") || created.AmountTotal != order.Total.Minor || !strings.EqualFold(string(created.Currency), order.Total.Currency) || !validURL(created.URL) || created.ExpiresAt <= time.Now().Unix() || created.Status != sdk.CheckoutSessionStatusOpen {
		return result, errors.New("Stripe 创建响应缺少有效订单信息")
	}
	until := time.Unix(created.ExpiresAt, 0)
	result.Outcome, result.ProviderPayableUntil, result.ExpirySource = types.PreparationReady, &until, "stripe.expires_at"
	result.NextAction = types.NextAction{Kind: "redirect", Redirect: &types.RedirectAction{URL: created.URL}, ValidUntil: &until}
	return result, nil
}

func (s *Stripe) VerifyNotification(_ context.Context, req types.CallbackRequest) (types.NotificationResult, error) {
	if req.Method != http.MethodPost || s.config.WebhookSecret == "" {
		return types.NotificationResult{}, errors.New("Stripe 通知方法或验签配置无效")
	}
	event, err := webhook.ConstructEvent(req.Body, req.Headers.Get("Stripe-Signature"), s.config.WebhookSecret)
	if err != nil {
		return types.NotificationResult{}, errors.New("Stripe 通知签名或 API 版本无效")
	}
	if event.Livemode != (s.config.Environment == "live") || (event.Account != "" && event.Account != s.config.AccountID) {
		return types.NotificationResult{}, errors.New("Stripe 通知账户或模式不一致")
	}
	if !slices.Contains(requiredEvents, string(event.Type)) {
		return types.NotificationResult{Ignored: true}, nil
	}
	if event.ID == "" || event.Data == nil {
		return types.NotificationResult{}, errors.New("Stripe 通知缺少事件信息")
	}
	var checkout sdk.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &checkout); err != nil {
		return types.NotificationResult{}, errors.New("Stripe Session 格式无效")
	}
	if checkout.ID == "" {
		return types.NotificationResult{}, errors.New("Stripe 通知缺少 Session ID")
	}
	if checkout.Livemode != (s.config.Environment == "live") {
		return types.NotificationResult{}, errors.New("Stripe Session 模式不一致")
	}
	if checkout.Mode != "" && checkout.Mode != sdk.CheckoutSessionModePayment {
		return types.NotificationResult{}, errors.New("Stripe 只支持单次正额支付")
	}
	state := classifySession(&checkout)
	switch event.Type {
	case sdk.EventTypeCheckoutSessionAsyncPaymentFailed:
		if state != types.ObservationSucceeded {
			state = types.ObservationAttemptFailed
		}
	case sdk.EventTypeCheckoutSessionExpired:
		if state != types.ObservationSucceeded {
			state = types.ObservationClosed
		}
	}
	observation, err := s.observation(&checkout, state, types.SourceVerifiedCallback)
	if err != nil {
		// 验签通过但证据不足时，由服务执行认证查单，并把临时失败映射成可重投 ACK。
		return types.NotificationResult{QueryRef: &types.OrderRef{TradeNo: checkout.ClientReferenceID, GatewayID: s.snapshot.GatewayID, ProductCode: product, ProviderResourceRef: checkout.ID}}, nil
	}
	observation.ProviderEventID, observation.VerificationRef = event.ID, "stripe.webhook:"+event.ID
	return types.NotificationResult{Observation: &observation}, nil
}

func classifySession(checkout *sdk.CheckoutSession) string {
	if checkout.PaymentStatus == sdk.CheckoutSessionPaymentStatusPaid {
		return types.ObservationSucceeded
	}
	if checkout.PaymentStatus == sdk.CheckoutSessionPaymentStatusNoPaymentRequired {
		return types.ObservationIgnored
	}
	if checkout.Status == sdk.CheckoutSessionStatusExpired {
		return types.ObservationClosed
	}
	if checkout.Status == sdk.CheckoutSessionStatusComplete {
		return types.ObservationProcessing
	}
	return types.ObservationUnpaid
}

func (s *Stripe) observation(checkout *sdk.CheckoutSession, state, source string) (types.PaymentObservation, error) {
	if checkout.ID == "" || checkout.ClientReferenceID == "" || checkout.Mode != sdk.CheckoutSessionModePayment || checkout.PaymentStatus == "" || checkout.Livemode != (s.config.Environment == "live") {
		return types.PaymentObservation{}, errors.New("Stripe Session 缺少支付证据")
	}
	result := types.PaymentObservation{Source: source, GatewayID: s.snapshot.GatewayID, Identity: s.snapshot.Identity, TransactionNamespace: s.snapshot.TransactionNamespace, TradeNo: checkout.ClientReferenceID, ProviderResourceRef: checkout.ID, State: state, RawStatus: string(checkout.Status) + "/" + string(checkout.PaymentStatus), VerificationRef: "stripe.session:" + checkout.ID}
	if checkout.PaymentIntent != nil {
		result.ProviderTransactionID = checkout.PaymentIntent.ID
	}
	if state == types.ObservationSucceeded {
		if result.ProviderTransactionID == "" {
			return types.PaymentObservation{}, errors.New("Stripe 已付订单缺少 PaymentIntent")
		}
		currency := strings.ToUpper(string(checkout.Currency))
		exponent, err := types.CurrencyExponent(currency)
		if err != nil {
			return types.PaymentObservation{}, err
		}
		total := types.Money{Minor: checkout.AmountTotal, Currency: currency, Exponent: exponent}
		if err := total.Validate(); err != nil {
			return types.PaymentObservation{}, err
		}
		result.OrderTotal = &total
	}
	return result, nil
}

func (s *Stripe) retrieve(ctx context.Context, ref types.OrderRef) (*sdk.CheckoutSession, error) {
	if ref.ProductCode != product || ref.ProviderResourceRef == "" {
		return nil, types.ErrUnsupported
	}
	if ref.GatewayID != s.snapshot.GatewayID {
		return nil, errors.New("Stripe 查询网关不一致")
	}
	checkout, err := s.sessions.Get(ref.ProviderResourceRef, &sdk.CheckoutSessionParams{Params: sdk.Params{Context: ctx}})
	if err != nil {
		return nil, errors.New("Stripe 订单查询失败")
	}
	if checkout.ID != ref.ProviderResourceRef || (ref.TradeNo != "" && checkout.ClientReferenceID != ref.TradeNo) || checkout.Livemode != (s.config.Environment == "live") || checkout.Mode != sdk.CheckoutSessionModePayment {
		return nil, errors.New("Stripe 查询结果与原订单不一致")
	}
	if ref.ProviderTransactionID != "" && (checkout.PaymentIntent == nil || checkout.PaymentIntent.ID != ref.ProviderTransactionID) {
		return nil, errors.New("Stripe PaymentIntent 与原订单不一致")
	}
	return checkout, nil
}

func (s *Stripe) QueryPayment(ctx context.Context, ref types.OrderRef) (types.PaymentObservation, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	checkout, err := s.retrieve(ctx, ref)
	if err != nil {
		return types.PaymentObservation{}, err
	}
	return s.observation(checkout, classifySession(checkout), types.SourceAuthenticatedQuery)
}

func (s *Stripe) ClosePayment(ctx context.Context, ref types.OrderRef) (types.CloseResult, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	checkout, err := s.retrieve(ctx, ref)
	if err != nil {
		return types.CloseResult{State: types.ObservationUnknown}, err
	}
	state := classifySession(checkout)
	if state != types.ObservationUnpaid || checkout.Status != sdk.CheckoutSessionStatusOpen {
		return types.CloseResult{State: state}, nil
	}
	closed, err := s.sessions.Expire(checkout.ID, &sdk.CheckoutSessionExpireParams{Params: sdk.Params{Context: ctx}})
	if err != nil {
		return types.CloseResult{State: types.ObservationUnknown}, errors.New("Stripe 关闭结果不明，请查询原订单")
	}
	if closed.ID != checkout.ID || closed.ClientReferenceID != checkout.ClientReferenceID || closed.Livemode != checkout.Livemode || closed.Mode != sdk.CheckoutSessionModePayment {
		return types.CloseResult{State: types.ObservationUnknown}, errors.New("Stripe 关闭响应与原订单不一致")
	}
	state = classifySession(closed)
	if state != types.ObservationClosed && state != types.ObservationSucceeded && state != types.ObservationProcessing {
		return types.CloseResult{State: types.ObservationUnknown}, errors.New("Stripe 未确认订单关闭")
	}
	return types.CloseResult{State: state}, nil
}

func (*Stripe) CallbackResponse(outcome types.CallbackOutcome) types.CallbackResponse {
	status := http.StatusOK
	switch outcome {
	case types.CallbackApplied, types.CallbackAlreadyApplied, types.CallbackIgnored:
	case types.CallbackRetryableFailure:
		status = http.StatusServiceUnavailable
	default:
		status = http.StatusBadRequest
	}
	return types.CallbackResponse{StatusCode: status, Headers: http.Header{}, Body: nil}
}

func (s *Stripe) ConfigureCallbacks(ctx context.Context, endpoint types.CallbackEndpoint) (types.SetupResult, error) {
	if !validURL(endpoint.URL) {
		return types.SetupResult{}, errors.New("Stripe 通知地址无效")
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	config := s.config
	var existing *sdk.WebhookEndpoint
	if config.WebhookEndpointID != "" {
		var err error
		existing, err = s.endpoints.Get(config.WebhookEndpointID, &sdk.WebhookEndpointParams{Params: sdk.Params{Context: ctx}})
		if err != nil {
			return types.SetupResult{}, errors.New("Stripe 通知端点查询失败")
		}
	} else {
		params := &sdk.WebhookEndpointListParams{ListParams: sdk.ListParams{Context: ctx, Limit: sdk.Int64(100)}}
		iter := s.endpoints.List(params)
		for iter.Next() {
			candidate := iter.WebhookEndpoint()
			if candidate.URL == endpoint.URL {
				existing = candidate
				break
			}
		}
		if iter.Err() != nil {
			return types.SetupResult{}, errors.New("Stripe 通知端点列表查询失败")
		}
	}
	if existing == nil {
		params := &sdk.WebhookEndpointParams{Params: sdk.Params{Context: ctx}, URL: sdk.String(endpoint.URL), APIVersion: sdk.String(sdk.APIVersion), EnabledEvents: sdk.StringSlice(requiredEvents)}
		created, err := s.endpoints.New(params)
		if err != nil {
			return types.SetupResult{}, errors.New("Stripe 通知端点创建结果不明，请核查后重新配置")
		}
		existing = created
		config.WebhookSecret = created.Secret
	} else {
		if existing.URL != endpoint.URL || existing.Livemode != (config.Environment == "live") || existing.APIVersion != sdk.APIVersion {
			return types.SetupResult{}, errors.New("Stripe 通知端点地址、模式或 API 版本不一致")
		}
		if config.WebhookSecret == "" {
			return types.SetupResult{}, errors.New("Stripe 已有端点不会返回签名密钥，请填入该端点的 webhook_secret")
		}
		events := slices.Clone(existing.EnabledEvents)
		if !slices.Contains(events, "*") {
			for _, required := range requiredEvents {
				if !slices.Contains(events, required) {
					events = append(events, required)
				}
			}
		}
		if len(events) != len(existing.EnabledEvents) || existing.Status != "enabled" {
			_, err := s.endpoints.Update(existing.ID, &sdk.WebhookEndpointParams{Params: sdk.Params{Context: ctx}, EnabledEvents: sdk.StringSlice(events), Disabled: sdk.Bool(false)})
			if err != nil {
				return types.SetupResult{}, errors.New("Stripe 通知事件配置失败")
			}
		}
	}
	if existing.ID == "" || config.WebhookSecret == "" || existing.Livemode != (config.Environment == "live") || existing.APIVersion != sdk.APIVersion || existing.URL != endpoint.URL {
		return types.SetupResult{}, errors.New("Stripe 通知端点缺少有效配置")
	}
	config.WebhookEndpointID = existing.ID
	raw, _ := json.Marshal(config)
	return types.SetupResult{Config: string(raw), Ready: true}, nil
}
