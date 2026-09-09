package wxpay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"one-api/common/logger"
	"one-api/payment/types"

	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/signers"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/validators"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/verifiers"
	"github.com/wechatpay-apiv3/wechatpay-go/core/cipher/ciphers"
	"github.com/wechatpay-apiv3/wechatpay-go/core/cipher/decryptors"
	"github.com/wechatpay-apiv3/wechatpay-go/core/cipher/encryptors"
	"github.com/wechatpay-apiv3/wechatpay-go/core/downloader"
	"github.com/wechatpay-apiv3/wechatpay-go/core/notify"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments"
	"github.com/wechatpay-apiv3/wechatpay-go/utils"
)

const ProductNative = "wxpay.native"
const protocolProfile = "wxpay.native-v3.v1"

type Factory struct{}

func (Factory) Descriptor() types.GatewayDescriptor {
	return types.GatewayDescriptor{Kind: "wxpay", Products: []string{ProductNative}}
}

func (Factory) ValidateConfig(_ context.Context, input types.GatewayConfigInput) (types.GatewayBinding, error) {
	config, err := parseConfig(input.Config)
	if err != nil {
		return types.GatewayBinding{}, err
	}
	if input.Currency != "" && input.Currency != "CNY" {
		return types.GatewayBinding{}, errors.New("微信 Native 仅支持 CNY")
	}
	if input.Product != "" && input.Product != ProductNative && !strings.EqualFold(input.Product, "native") {
		return types.GatewayBinding{}, types.ErrUnsupported
	}
	identity := types.GatewayIdentity{Kind: "wxpay", Environment: "live", MerchantAccount: config.MchID, AppBinding: config.AppID, ProtocolProfile: protocolProfile}
	normalized, err := json.Marshal(config)
	if err != nil {
		return types.GatewayBinding{}, err
	}
	return types.GatewayBinding{Identity: identity, TransactionNamespace: types.Namespace("wxpay", "live", config.MchID), DefaultProduct: ProductNative, Config: string(normalized)}, nil
}

func parseConfig(raw string) (WeChatConfig, error) {
	var config WeChatConfig
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return config, errors.New("微信支付配置格式错误")
	}
	config.MchID = strings.TrimSpace(config.MchID)
	config.AppID = strings.TrimSpace(config.AppID)
	config.MchCertificateSerialNumber = strings.TrimSpace(config.MchCertificateSerialNumber)
	if config.MchID == "" || config.AppID == "" || config.MchCertificateSerialNumber == "" || len(config.MchAPIv3Key) != 32 {
		return config, errors.New("微信支付商户、应用、证书序列号或 APIv3 密钥配置无效")
	}
	if config.PayType != "" && !strings.EqualFold(config.PayType, "native") && config.PayType != ProductNative {
		return config, types.ErrUnsupported
	}
	config.PayType = ProductNative
	if _, err := utils.LoadPrivateKey(config.MchPrivateKey); err != nil {
		return config, errors.New("微信支付商户私钥无效")
	}
	return config, nil
}

// Client 的证书下载、API 签名和通知解密始终属于同一凭证 revision。
// registry 等待业务引用归零后调用 CloseResources；ctx 必须是服务生命周期。
type Client struct {
	snapshot     types.GatewaySnapshot
	config       WeChatConfig
	api          *core.Client
	certificates *downloader.CertificateDownloader
	refresh      *certificateRefresh
	handler      *notify.Handler
}

func (Factory) NewClient(ctx context.Context, snapshot types.GatewaySnapshot) (types.GatewayClient, error) {
	return newClient(ctx, snapshot, &http.Client{Timeout: 15 * time.Second})
}

func newClient(ctx context.Context, snapshot types.GatewaySnapshot, httpClient *http.Client) (*Client, error) {
	binding, err := (Factory{}).ValidateConfig(ctx, types.GatewayConfigInput{Config: snapshot.Config, Product: snapshot.DefaultProduct})
	if err != nil {
		return nil, err
	}
	if !binding.Identity.Equal(snapshot.Identity) || binding.TransactionNamespace != snapshot.TransactionNamespace {
		return nil, errors.New("微信支付配置与绑定身份不一致")
	}
	config, err := parseConfig(binding.Config)
	if err != nil {
		return nil, err
	}
	privateKey, err := utils.LoadPrivateKey(config.MchPrivateKey)
	if err != nil {
		return nil, err
	}
	client := &Client{snapshot: snapshot, config: config}
	signer := &signers.SHA256WithRSASigner{MchID: config.MchID, CertificateSerialNo: config.MchCertificateSerialNumber, PrivateKey: privateKey}
	// 首次证书下载采用 SDK 的信任引导方式；后续下载由 SDK 使用已取得的平台证书验签。
	downloadClient, err := core.NewClientWithDialSettings(ctx, &core.DialSettings{
		Signer:    signer,
		Validator: &validators.NullValidator{}, HTTPClient: httpClient,
	})
	if err != nil {
		return nil, err
	}
	downloadCtx, cancel := context.WithTimeout(ctx, certificateDownloadTimeout)
	client.certificates, err = downloader.NewCertificateDownloaderWithClient(downloadCtx, downloadClient, config.MchAPIv3Key)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("微信平台证书初始化失败: %w", err)
	}
	verifier := verifiers.NewSHA256WithRSAVerifier(client.certificates)
	client.api, err = core.NewClientWithDialSettings(ctx, &core.DialSettings{
		Signer:     signer,
		Validator:  validators.NewWechatPayResponseValidator(verifier),
		Cipher:     ciphers.NewWechatPayCipher(encryptors.NewWechatPayEncryptor(client.certificates), decryptors.NewWechatPayDecryptor(privateKey)),
		HTTPClient: httpClient,
	})
	if err != nil {
		return nil, err
	}
	client.handler, err = notify.NewRSANotifyHandler(config.MchAPIv3Key, verifier)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 完成全部初始化后才启动后台任务，失败候选不会留下 worker。
	client.refresh = startCertificateRefresh(ctx, client.certificates.DownloadCertificates, func(error) {
		// SDK 错误可能包含完整上游报文，只记录可定位的本地网关和版本。
		logger.SysError(fmt.Sprintf("微信平台证书刷新失败，保留上次证书并等待下次刷新：gateway_id=%d revision=%d", snapshot.GatewayID, snapshot.CredentialRevision))
	})
	return client, nil
}

func (c *Client) CloseResources() error {
	c.refresh.close()
	return nil
}

func (*Client) Capabilities(product types.ProductCode) (types.Capabilities, error) {
	if product != ProductNative {
		return types.Capabilities{}, types.ErrUnsupported
	}
	return types.Capabilities{Currencies: []string{"CNY"}, PreparationMode: types.ServerCreate, CanReceiveCallbacks: true, QueryByMerchantRef: true, CanClose: true, ExpiryMode: "absolute", ExpiryStart: "order_created", DisplayWindow: 3 * time.Hour, ActionKinds: []string{"qr_code"}}, nil
}

func (c *Client) VerifyNotification(ctx context.Context, input types.CallbackRequest) (types.NotificationResult, error) {
	if input.Method != http.MethodPost {
		return types.NotificationResult{}, errors.New("微信通知必须使用 POST")
	}
	// SDK 假定通知 envelope 及 GCM nonce 已有效；这里校验代理负责的传输 envelope。
	var envelope notify.Request
	if err := json.Unmarshal(input.Body, &envelope); err != nil || envelope.Resource == nil || len(envelope.Resource.Nonce) != 12 {
		return types.NotificationResult{}, errors.New("微信通知 envelope 无效")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://callback.invalid/", bytes.NewReader(input.Body))
	if err != nil {
		return types.NotificationResult{}, err
	}
	req.Header = input.Headers.Clone()
	var plaintext json.RawMessage
	notification, err := c.handler.ParseNotifyRequest(ctx, req, &plaintext)
	if err != nil {
		return types.NotificationResult{}, errors.New("微信通知验签或解密失败")
	}
	if notification.EventType != "TRANSACTION.SUCCESS" {
		return types.NotificationResult{Ignored: true}, nil
	}
	var transaction payments.Transaction
	if err := json.Unmarshal(plaintext, &transaction); err != nil {
		return types.NotificationResult{}, errors.New("微信支付通知交易内容无效")
	}
	if transaction.TradeState == nil {
		return types.NotificationResult{}, errors.New("微信支付通知缺少交易状态")
	}
	if *transaction.TradeState != "SUCCESS" {
		return types.NotificationResult{Ignored: true}, nil
	}
	observation, err := c.observe(&transaction, types.SourceVerifiedCallback, input.Headers.Get("Wechatpay-Serial"))
	if err != nil {
		return types.NotificationResult{}, err
	}
	observation.ProviderEventID = notification.ID
	return types.NotificationResult{Observation: &observation}, nil
}

func (c *Client) observe(transaction *payments.Transaction, source, verificationRef string) (types.PaymentObservation, error) {
	result := types.PaymentObservation{Source: source, GatewayID: c.snapshot.GatewayID, Identity: c.snapshot.Identity, TransactionNamespace: c.snapshot.TransactionNamespace, VerificationRef: verificationRef, MethodCode: "wxpay"}
	if transaction == nil || transaction.Mchid == nil || *transaction.Mchid != c.config.MchID || transaction.Appid == nil || *transaction.Appid != c.config.AppID {
		return result, errors.New("微信支付结果商户或应用不匹配")
	}
	if transaction.OutTradeNo == nil || *transaction.OutTradeNo == "" || transaction.TradeState == nil {
		return result, errors.New("微信支付结果缺少商户单号或状态")
	}
	result.TradeNo, result.RawStatus = *transaction.OutTradeNo, *transaction.TradeState
	switch result.RawStatus {
	case "SUCCESS":
		result.State = types.ObservationSucceeded
	case "NOTPAY":
		result.State = types.ObservationUnpaid
	case "USERPAYING":
		result.State = types.ObservationProcessing
	case "CLOSED", "REVOKED":
		result.State = types.ObservationClosed
	case "PAYERROR":
		result.State = types.ObservationAttemptFailed
	default:
		result.State = types.ObservationUnknown
	}
	if result.State != types.ObservationSucceeded {
		return result, nil
	}
	if transaction.TransactionId == nil || *transaction.TransactionId == "" || transaction.Amount == nil || transaction.Amount.Total == nil || *transaction.Amount.Total <= 0 || transaction.Amount.Currency == nil || *transaction.Amount.Currency != "CNY" {
		return result, errors.New("微信成功交易缺少有效交易号或应付总额/币种")
	}
	result.ProviderTransactionID = *transaction.TransactionId
	result.OrderTotal = &types.Money{Minor: *transaction.Amount.Total, Currency: *transaction.Amount.Currency, Exponent: 2}
	if transaction.SuccessTime != nil {
		paidAt, err := time.Parse(time.RFC3339, *transaction.SuccessTime)
		if err != nil {
			return result, errors.New("微信支付成功时间无效")
		}
		result.ProviderPaidAt = &paidAt
	}
	if transaction.Amount.PayerTotal != nil {
		result.SafeDiagnostics = fmt.Sprintf("payer_total=%d", *transaction.Amount.PayerTotal)
	}
	return result, nil
}

func (*Client) CallbackResponse(outcome types.CallbackOutcome) types.CallbackResponse {
	switch outcome {
	case types.CallbackApplied, types.CallbackAlreadyApplied, types.CallbackIgnored:
		return types.CallbackResponse{StatusCode: http.StatusNoContent}
	case types.CallbackRetryableFailure:
		return types.CallbackResponse{StatusCode: http.StatusInternalServerError, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"code":"FAIL","message":"processing failed"}`)}
	default:
		return types.CallbackResponse{StatusCode: http.StatusBadRequest, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"code":"FAIL","message":"callback rejected"}`)}
	}
}
