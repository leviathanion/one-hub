package alipay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"one-api/payment/types"
	"time"

	sdk "github.com/smartwalle/alipay/v3"
)

const Profile = "alipay.direct.v1"

// Factory 为每个绑定创建独立 SDK；HTTPClient 供受控 transport 和集成测试注入。
type Factory struct{ HTTPClient *http.Client }

type AlipayConfig struct {
	AppID       string  `json:"app_id"`
	SellerID    string  `json:"seller_id"`
	PrivateKey  string  `json:"private_key"`
	PublicKey   string  `json:"public_key"`
	PayType     PayType `json:"pay_type"`
	Environment string  `json:"environment,omitempty"`
}

type gateway struct {
	client   *sdk.Client
	snapshot types.GatewaySnapshot
}

var chinaTime = time.FixedZone("CST", 8*60*60)

func (Factory) Descriptor() types.GatewayDescriptor {
	return types.GatewayDescriptor{Kind: "alipay", Products: []string{ProductFacePay, ProductPagePay, ProductWapPay}}
}

func parseConfig(input types.GatewayConfigInput) (AlipayConfig, types.GatewayBinding, error) {
	var config AlipayConfig
	if err := json.Unmarshal([]byte(input.Config), &config); err != nil {
		return config, types.GatewayBinding{}, errors.New("支付宝配置格式错误")
	}
	if input.Currency != "CNY" {
		return config, types.GatewayBinding{}, errors.New("当前支付宝产品仅支持 CNY")
	}
	if config.AppID == "" || config.SellerID == "" || config.PrivateKey == "" || config.PublicKey == "" {
		return config, types.GatewayBinding{}, errors.New("缺少支付宝应用、收款主体或验签凭证")
	}
	if config.Environment == "" {
		config.Environment = "production"
	}
	if config.Environment != "production" && config.Environment != "sandbox" {
		return config, types.GatewayBinding{}, errors.New("无效的支付宝环境")
	}
	if config.PayType == "" {
		config.PayType = FacePay
	}
	if config.PayType != FacePay && config.PayType != PagePay && config.PayType != WapPay {
		return config, types.GatewayBinding{}, types.ErrUnsupported
	}
	product := "alipay." + string(config.PayType)
	if input.Product != "" {
		product = input.Product
	}
	switch product {
	case ProductFacePay:
		config.PayType = FacePay
	case ProductPagePay:
		config.PayType = PagePay
	case ProductWapPay:
		config.PayType = WapPay
	default:
		return config, types.GatewayBinding{}, types.ErrUnsupported
	}
	identity := types.GatewayIdentity{Kind: "alipay", Environment: config.Environment, MerchantAccount: config.SellerID, AppBinding: config.AppID, ProtocolProfile: Profile}
	encoded, _ := json.Marshal(config)
	return config, types.GatewayBinding{Identity: identity, TransactionNamespace: types.Namespace("alipay", config.Environment, config.SellerID), DefaultProduct: product, DefaultMethod: "alipay", Config: string(encoded)}, nil
}

func (f Factory) ValidateConfig(_ context.Context, input types.GatewayConfigInput) (types.GatewayBinding, error) {
	config, binding, err := parseConfig(input)
	if err != nil {
		return types.GatewayBinding{}, err
	}
	if _, err = f.newSDK(config); err != nil {
		return types.GatewayBinding{}, err
	}
	return binding, nil
}

func (f Factory) newSDK(config AlipayConfig) (*sdk.Client, error) {
	httpClient := http.Client{Timeout: 30 * time.Second}
	if f.HTTPClient != nil {
		httpClient = *f.HTTPClient
		if httpClient.Timeout == 0 {
			httpClient.Timeout = 30 * time.Second
		}
	}
	// 307/308 也不自动重发预下单；是否再次创建由订单 owner 决定。
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client, err := sdk.New(config.AppID, config.PrivateKey, config.Environment == "production", sdk.WithHTTPClient(&httpClient), sdk.WithTimeLocation(chinaTime))
	if err != nil {
		return nil, errors.New("支付宝私钥无效")
	}
	if err = client.LoadAliPayPublicKey(config.PublicKey); err != nil {
		return nil, errors.New("支付宝平台公钥无效")
	}
	return client, nil
}

func (f Factory) NewClient(_ context.Context, snapshot types.GatewaySnapshot) (types.GatewayClient, error) {
	config, binding, err := parseConfig(types.GatewayConfigInput{Config: snapshot.Config, Currency: "CNY", Product: snapshot.DefaultProduct})
	if err != nil {
		return nil, err
	}
	if !binding.Identity.Equal(snapshot.Identity) || binding.TransactionNamespace != snapshot.TransactionNamespace {
		return nil, errors.New("支付宝配置与冻结身份不符")
	}
	client, err := f.newSDK(config)
	if err != nil {
		return nil, err
	}
	return &gateway{client: client, snapshot: snapshot}, nil
}

func (*gateway) Capabilities(product string) (types.Capabilities, error) {
	capability := types.Capabilities{Currencies: []string{"CNY"}, CanReceiveCallbacks: true, QueryByMerchantRef: true, CanClose: true, ExpiryMode: "absolute", DisplayWindow: 15 * time.Minute}
	switch product {
	case ProductFacePay:
		capability.PreparationMode = types.ServerCreate
		capability.ActionKinds = []string{"qr_code"}
	case ProductPagePay, ProductWapPay:
		capability.PreparationMode = types.BrowserHandoff
		capability.ActionKinds = []string{"form"}
	default:
		return types.Capabilities{}, types.ErrUnsupported
	}
	return capability, nil
}

// 支付宝通知以 POST form 为主；保留原 GET 通知入口，重复值和跨来源冲突明确拒绝。
func notificationParams(request types.CallbackRequest) (url.Values, error) {
	if request.Method != http.MethodPost && request.Method != http.MethodGet {
		return nil, errors.New("不支持的支付宝通知方法")
	}
	params := make(url.Values, len(request.Query))
	for key, values := range request.Query {
		if len(values) != 1 {
			return nil, errors.New("支付宝通知包含重复参数")
		}
		params.Set(key, values[0])
	}
	if request.Method == http.MethodGet && len(request.Body) != 0 {
		return nil, errors.New("GET 通知不可带表单体")
	}
	if request.Method == http.MethodPost {
		body, err := url.ParseQuery(string(request.Body))
		if err != nil {
			return nil, errors.New("支付宝通知表单无效")
		}
		for key, values := range body {
			if len(values) != 1 {
				return nil, errors.New("支付宝通知包含重复参数")
			}
			if old, ok := params[key]; ok && old[0] != values[0] {
				return nil, errors.New("支付宝通知 query 与 form 冲突")
			}
			params.Set(key, values[0])
		}
	}
	return params, nil
}

func (g *gateway) VerifyNotification(_ context.Context, request types.CallbackRequest) (types.NotificationResult, error) {
	params, err := notificationParams(request)
	if err != nil {
		return types.NotificationResult{}, err
	}
	if err = g.client.VerifySign(params); err != nil {
		return types.NotificationResult{}, errors.New("支付宝通知签名无效")
	}
	if params.Get("app_id") != g.snapshot.Identity.AppBinding || params.Get("seller_id") != g.snapshot.Identity.MerchantAccount {
		return types.NotificationResult{}, errors.New("支付宝通知应用或收款主体不符")
	}
	observation, err := g.observation(types.SourceVerifiedCallback, params.Get("out_trade_no"), params.Get("trade_no"), params.Get("trade_status"), params.Get("total_amount"), params.Get("buyer_pay_amount"), params.Get("receipt_amount"))
	if err != nil {
		return types.NotificationResult{}, err
	}
	observation.ProviderEventID = params.Get("notify_id")
	if paid, err := time.ParseInLocation("2006-01-02 15:04:05", params.Get("gmt_payment"), chinaTime); err == nil {
		observation.ProviderPaidAt = &paid
	}
	if observation.State == types.ObservationIgnored {
		return types.NotificationResult{Ignored: true}, nil
	}
	return types.NotificationResult{Observation: &observation}, nil
}

func (g *gateway) observation(source, tradeNo, transaction, status, amount, buyerAmount, receiptAmount string) (types.PaymentObservation, error) {
	state := types.ObservationIgnored
	switch sdk.TradeStatus(status) {
	case sdk.TradeStatusSuccess, sdk.TradeStatusFinished:
		state = types.ObservationSucceeded
	case sdk.TradeStatusWaitBuyerPay:
		state = types.ObservationUnpaid
	case sdk.TradeStatusClosed:
		state = types.ObservationClosed
	}
	observation := types.PaymentObservation{Source: source, GatewayID: g.snapshot.GatewayID, TransactionNamespace: g.snapshot.TransactionNamespace, Identity: g.snapshot.Identity, TradeNo: tradeNo, ProviderTransactionID: transaction, State: state, MethodCode: "alipay", RawStatus: status, VerificationRef: fmt.Sprintf("%s/revision:%d", Profile, g.snapshot.CredentialRevision)}
	if state != types.ObservationIgnored && tradeNo == "" {
		return types.PaymentObservation{}, errors.New("支付宝通知缺少原订单号")
	}
	if state == types.ObservationSucceeded {
		if transaction == "" {
			return types.PaymentObservation{}, errors.New("支付宝成功结果缺少交易号")
		}
		money, err := types.ParseMoney(amount, "CNY")
		if err != nil {
			return types.PaymentObservation{}, err
		}
		if err = money.Validate(); err != nil {
			return types.PaymentObservation{}, err
		}
		observation.OrderTotal = &money
	}
	// 实收与买家实付只作审计，不参与订单总额核对。
	audit := make(map[string]types.Money)
	for name, value := range map[string]string{"buyer_pay_amount": buyerAmount, "receipt_amount": receiptAmount} {
		if money, err := types.ParseMoney(value, "CNY"); err == nil {
			audit[name] = money
		}
	}
	if len(audit) != 0 {
		encoded, _ := json.Marshal(audit)
		observation.SafeDiagnostics = string(encoded)
	}
	return observation, nil
}

func (*gateway) CallbackResponse(outcome types.CallbackOutcome) types.CallbackResponse {
	body := "failure"
	if outcome == types.CallbackApplied || outcome == types.CallbackAlreadyApplied || outcome == types.CallbackIgnored {
		body = "success"
	}
	return types.CallbackResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}}, Body: []byte(body)}
}
