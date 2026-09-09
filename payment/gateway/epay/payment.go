package epay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"one-api/payment/types"
	"strings"
	"time"
)

// Profile 是本仓库 GET 通知、MD5 表单、人民币元金额的固定方言。
// 运营商须按此契约验收；本 profile 不声明 POST 通知、查单或关单能力。
const Profile = "epay.form-md5.v1"
const Checkout = "epay.checkout"

type Factory struct{}

type EpayConfig struct {
	PayType         PayType `json:"pay_type"`
	ProtocolProfile string  `json:"protocol_profile"`
	Environment     string  `json:"environment,omitempty"`
	Client
}

type gateway struct {
	config   EpayConfig
	snapshot types.GatewaySnapshot
}

func (Factory) Descriptor() types.GatewayDescriptor {
	return types.GatewayDescriptor{Kind: "epay", Products: []string{Checkout}}
}

func (Factory) ValidateConfig(_ context.Context, input types.GatewayConfigInput) (types.GatewayBinding, error) {
	var config EpayConfig
	if err := json.Unmarshal([]byte(input.Config), &config); err != nil {
		return types.GatewayBinding{}, errors.New("易支付配置格式错误")
	}
	if config.ProtocolProfile != Profile {
		return types.GatewayBinding{}, errors.New("未支持的易支付协议 profile")
	}
	if input.Currency != "CNY" {
		return types.GatewayBinding{}, errors.New("当前易支付 profile 仅支持 CNY")
	}
	if input.Product != "" && input.Product != Checkout {
		return types.GatewayBinding{}, types.ErrUnsupported
	}
	if !validMethod(string(config.PayType)) {
		return types.GatewayBinding{}, errors.New("未支持的易支付付款方式")
	}
	if config.PartnerID == "" || config.Key == "" {
		return types.GatewayBinding{}, errors.New("缺少易支付商户或密钥")
	}
	endpoint, err := url.Parse(config.PayDomain)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return types.GatewayBinding{}, errors.New("无效的易支付端点")
	}
	endpoint.Host = strings.ToLower(endpoint.Host)
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	config.PayDomain = endpoint.String()
	if config.Environment == "" {
		config.Environment = "production"
	}
	if config.Environment != "production" {
		return types.GatewayBinding{}, errors.New("当前易支付 profile 未证明独立沙箱环境")
	}
	identity := types.GatewayIdentity{Kind: "epay", Environment: config.Environment, MerchantAccount: config.PartnerID, EndpointScope: config.PayDomain, ProtocolProfile: Profile}
	encoded, _ := json.Marshal(config)
	return types.GatewayBinding{Identity: identity, TransactionNamespace: types.Namespace("epay", config.Environment, config.PayDomain, Profile, config.PartnerID), DefaultProduct: Checkout, DefaultMethod: string(config.PayType), Config: string(encoded)}, nil
}

func (f Factory) NewClient(ctx context.Context, snapshot types.GatewaySnapshot) (types.GatewayClient, error) {
	binding, err := f.ValidateConfig(ctx, types.GatewayConfigInput{Config: snapshot.Config, Currency: "CNY", Product: snapshot.DefaultProduct})
	if err != nil {
		return nil, err
	}
	if !binding.Identity.Equal(snapshot.Identity) || binding.TransactionNamespace != snapshot.TransactionNamespace {
		return nil, errors.New("易支付配置与冻结身份不符")
	}
	var config EpayConfig
	if err = json.Unmarshal([]byte(binding.Config), &config); err != nil {
		return nil, err
	}
	return &gateway{config: config, snapshot: snapshot}, nil
}

func (*gateway) Capabilities(product string) (types.Capabilities, error) {
	if product != Checkout {
		return types.Capabilities{}, types.ErrUnsupported
	}
	return types.Capabilities{Currencies: []string{"CNY"}, PreparationMode: types.BrowserHandoff, CanReceiveCallbacks: true, ExpiryMode: "none", DisplayWindow: 3 * time.Hour, ActionKinds: []string{"form"}}, nil
}

func validMethod(method string) bool {
	switch PayType(method) {
	case EpayPay, Alipay, Wechat, QQ, Bank, JD, PayPal, USDT:
		return true
	}
	return false
}

func (g *gateway) PreparePayment(_ context.Context, order types.FrozenOrder) (types.PrepareResult, error) {
	if _, err := g.Capabilities(order.ProductCode); err != nil {
		return rejected(err)
	}
	if order.GatewayID != g.snapshot.GatewayID || !order.Identity.Equal(g.snapshot.Identity) || order.TransactionNamespace != g.snapshot.TransactionNamespace {
		return rejected(errors.New("易支付订单身份不符"))
	}
	if err := order.Total.Validate(); err != nil {
		return rejected(err)
	}
	if order.Total.Currency != "CNY" || order.TradeNo == "" || order.LocalDisplayUntil.IsZero() {
		return rejected(errors.New("无效的易支付订单"))
	}
	method := order.MethodPreference
	if !validMethod(method) {
		return rejected(errors.New("未支持的易支付付款方式"))
	}
	actionURL, fields, err := g.config.FormPay(&PayArgs{Type: PayType(method), OutTradeNo: order.TradeNo, NotifyUrl: order.Input.NotifyURL, ReturnUrl: order.Input.ReturnURL, Name: order.Input.Description, Money: order.Total.DecimalString()})
	if err != nil {
		return rejected(err)
	}
	until := order.LocalDisplayUntil
	return types.PrepareResult{Outcome: types.PreparationReady, NextAction: types.NextAction{Kind: "form", Form: &types.FormAction{Method: http.MethodPost, ActionURL: actionURL, Fields: fields}, ValidUntil: &until}}, nil
}

func rejected(err error) (types.PrepareResult, error) {
	return types.PrepareResult{Outcome: types.PreparationRejected, NextAction: types.NoAction(), ErrorCode: "invalid_order"}, err
}

func (g *gateway) VerifyNotification(_ context.Context, request types.CallbackRequest) (types.NotificationResult, error) {
	if request.Method != http.MethodGet || len(request.Body) != 0 {
		return types.NotificationResult{}, errors.New("当前易支付 profile 仅接受 GET query 通知")
	}
	params := make(map[string]string, len(request.Query))
	for key, values := range request.Query {
		if len(values) != 1 {
			return types.NotificationResult{}, errors.New("易支付通知包含重复参数")
		}
		params[key] = values[0]
	}
	result, ok := g.config.Verify(params)
	if !ok {
		return types.NotificationResult{}, errors.New("易支付通知签名无效")
	}
	if result.PartnerID != g.snapshot.Identity.MerchantAccount {
		return types.NotificationResult{}, errors.New("易支付通知商户不符")
	}
	if result.TradeStatus != TradeStatusSuccess {
		return types.NotificationResult{Ignored: true}, nil
	}
	if result.OutTradeNo == "" || result.TradeNo == "" {
		return types.NotificationResult{}, errors.New("易支付成功通知缺少交易引用")
	}
	money, err := types.ParseMoney(result.Money, "CNY")
	if err != nil {
		return types.NotificationResult{}, err
	}
	if err = money.Validate(); err != nil {
		return types.NotificationResult{}, err
	}
	observation := &types.PaymentObservation{Source: types.SourceVerifiedCallback, GatewayID: g.snapshot.GatewayID, TransactionNamespace: g.snapshot.TransactionNamespace, Identity: g.snapshot.Identity, TradeNo: result.OutTradeNo, ProviderTransactionID: result.TradeNo, State: types.ObservationSucceeded, OrderTotal: &money, MethodCode: string(result.Type), RawStatus: result.TradeStatus, VerificationRef: fmt.Sprintf("%s/revision:%d", Profile, g.snapshot.CredentialRevision)}
	return types.NotificationResult{Observation: observation}, nil
}

func (*gateway) CallbackResponse(outcome types.CallbackOutcome) types.CallbackResponse {
	body := "fail"
	if outcome == types.CallbackApplied || outcome == types.CallbackAlreadyApplied || outcome == types.CallbackIgnored {
		body = "success"
	}
	return types.CallbackResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}}, Body: []byte(body)}
}
