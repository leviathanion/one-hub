package wxpay

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"one-api/payment/types"

	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments"
)

type merchantFixture struct {
	config  WeChatConfig
	key     *rsa.PrivateKey
	allowed bool
}
type platformFixture struct {
	serial, pem string
	key         *rsa.PrivateKey
}
type requestRecord struct {
	method, path, mchid, serial, query string
	body                               []byte
}
type wechatFixture struct {
	t         *testing.T
	mu        sync.Mutex
	merchants map[string]*merchantFixture
	platforms []platformFixture
	records   []requestRecord
	server    *httptest.Server
	client    *http.Client
}
type rewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.target.Scheme, r.target.Host
	return r.base.RoundTrip(req)
}
func newFixture(t *testing.T) *wechatFixture {
	t.Helper()
	f := &wechatFixture{t: t, merchants: make(map[string]*merchantFixture)}
	f.platforms = []platformFixture{newPlatform(t, 101)}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	target, _ := url.Parse(f.server.URL)
	f.client = &http.Client{Timeout: time.Second, Transport: rewriteTransport{target: target, base: f.server.Client().Transport}}
	return f
}
func newPlatform(t *testing.T, serial int64) platformFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "wechat-fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return platformFixture{serial: fmt.Sprintf("%X", serial), key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}
func (f *wechatFixture) merchant(mchid, serial, apiKey string) WeChatConfig {
	f.t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		f.t.Fatal(err)
	}
	config := WeChatConfig{AppID: "app-" + mchid, MchID: mchid, MchCertificateSerialNumber: serial, MchAPIv3Key: apiKey, MchPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), PayType: "Native"}
	f.mu.Lock()
	f.merchants[serial] = &merchantFixture{config: config, key: key, allowed: true}
	f.mu.Unlock()
	return config
}
func snapshotFor(t *testing.T, config WeChatConfig, id int, revision int64) types.GatewaySnapshot {
	t.Helper()
	raw, _ := json.Marshal(config)
	binding, err := (Factory{}).ValidateConfig(context.Background(), types.GatewayConfigInput{Config: string(raw), Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	return types.GatewaySnapshot{GatewayID: id, CredentialRevision: revision, Identity: binding.Identity, TransactionNamespace: binding.TransactionNamespace, DefaultProduct: binding.DefaultProduct, Config: binding.Config}
}
func (f *wechatFixture) bind(config WeChatConfig, id int, revision int64) *Client {
	f.t.Helper()
	c, err := newClient(context.Background(), snapshotFor(f.t, config, id, revision), f.client)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = c.CloseResources() })
	return c
}
func encryptFixture(key, plaintext string) map[string]string {
	block, _ := aes.NewCipher([]byte(key))
	gcm, _ := cipher.NewGCM(block)
	nonce, associated := "fixture12345", "transaction"
	return map[string]string{"algorithm": "AEAD_AES_256_GCM", "nonce": nonce, "associated_data": associated, "ciphertext": base64.StdEncoding.EncodeToString(gcm.Seal(nil, []byte(nonce), []byte(plaintext), []byte(associated)))}
}
func signedHeaders(t *testing.T, platform platformFixture, body []byte) http.Header {
	t.Helper()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "signed-nonce"
	sum := sha256.Sum256([]byte(timestamp + "\n" + nonce + "\n" + string(body) + "\n"))
	sig, err := rsa.SignPKCS1v15(rand.Reader, platform.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return http.Header{"Wechatpay-Timestamp": []string{timestamp}, "Wechatpay-Nonce": []string{nonce}, "Wechatpay-Serial": []string{platform.serial}, "Wechatpay-Signature": []string{base64.StdEncoding.EncodeToString(sig)}, "Content-Type": []string{"application/json"}}
}
func transactionFor(mchid string) *payments.Transaction {
	return &payments.Transaction{Appid: core.String("app-" + mchid), Mchid: core.String(mchid), OutTradeNo: core.String("order-29"), TransactionId: core.String("transaction-29"), TradeState: core.String("SUCCESS"), TradeType: core.String("NATIVE"), SuccessTime: core.String(time.Now().Format(time.RFC3339)), Amount: &payments.TransactionAmount{Total: core.Int64(29), Currency: core.String("CNY"), PayerTotal: core.Int64(19)}}
}

var authorizationFields = regexp.MustCompile(`(\w+)="([^"]*)"`)

func (f *wechatFixture) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	fields := make(map[string]string)
	for _, m := range authorizationFields.FindAllStringSubmatch(r.Header.Get("Authorization"), -1) {
		fields[m[1]] = m[2]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	record := requestRecord{method: r.Method, path: r.URL.Path, mchid: fields["mchid"], serial: fields["serial_no"], query: r.URL.RawQuery, body: body}
	f.records = append(f.records, record)
	merchant := f.merchants[record.serial]
	if merchant == nil || !merchant.allowed || merchant.config.MchID != record.mchid {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"code":"INVALID_CREDENTIAL"}`))
		return
	}
	sum := sha256.Sum256([]byte(r.Method + "\n" + r.URL.RequestURI() + "\n" + fields["timestamp"] + "\n" + fields["nonce_str"] + "\n" + string(body) + "\n"))
	signature, err := base64.StdEncoding.DecodeString(fields["signature"])
	if err != nil || rsa.VerifyPKCS1v15(&merchant.key.PublicKey, crypto.SHA256, sum[:], signature) != nil {
		f.t.Error("商户请求签名无效")
		w.WriteHeader(401)
		return
	}
	status := http.StatusOK
	var response []byte
	switch {
	case r.URL.Path == "/v3/certificates":
		data := []any{}
		for _, platform := range f.platforms {
			data = append(data, map[string]any{"serial_no": platform.serial, "effective_time": time.Now().Add(-time.Hour).Format(time.RFC3339), "expire_time": time.Now().Add(48 * time.Hour).Format(time.RFC3339), "encrypt_certificate": encryptFixture(merchant.config.MchAPIv3Key, platform.pem)})
		}
		response, _ = json.Marshal(map[string]any{"data": data})
	case r.URL.Path == "/v3/pay/transactions/native":
		response = []byte(`{"code_url":"weixin://wxpay/fixture-code"}`)
	case strings.HasSuffix(r.URL.Path, "/close"):
		status = http.StatusNoContent
	default:
		response, _ = json.Marshal(transactionFor(merchant.config.MchID))
	}
	headers := signedHeaders(f.t, f.platforms[0], response)
	for k, v := range headers {
		w.Header()[k] = v
	}
	w.WriteHeader(status)
	_, _ = w.Write(response)
}
func callbackFixture(t *testing.T, platform platformFixture, apiKey, event string, transaction any) types.CallbackRequest {
	t.Helper()
	plaintext, _ := json.Marshal(transaction)
	body, _ := json.Marshal(map[string]any{"id": "event-29", "event_type": event, "resource_type": "encrypt-resource", "resource": encryptFixture(apiKey, string(plaintext))})
	return types.CallbackRequest{Method: http.MethodPost, Body: body, Headers: signedHeaders(t, platform, body)}
}
func TestNativePrepareQueryAndCloseUseFrozenMoneyAndMerchant(t *testing.T) {
	f := newFixture(t)
	config := f.merchant("merchant-A", "serial-A", strings.Repeat("a", 32))
	c := f.bind(config, 1, 1)
	money, err := types.ParseMoney("0.29", "CNY")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Hour).Truncate(time.Second)
	order := types.FrozenOrder{TradeNo: "order-29", GatewayID: 1, Identity: c.snapshot.Identity, TransactionNamespace: c.snapshot.TransactionNamespace, ProductCode: ProductNative, Total: money, LocalDisplayUntil: deadline, Input: types.CreateInput{NotifyURL: "https://onehub.example/callback", Description: "充值"}}
	f.mu.Lock()
	beforeInvalid := len(f.records)
	f.mu.Unlock()
	unsupported := order
	unsupported.MethodPreference = "alipay"
	if rejected, err := c.PreparePayment(context.Background(), unsupported); err == nil || rejected.Outcome != types.PreparationRejected {
		t.Fatal("不支持的付款方式未在上游调用前拒绝")
	}
	f.mu.Lock()
	afterInvalid := len(f.records)
	f.mu.Unlock()
	if afterInvalid != beforeInvalid {
		t.Fatal("不支持的付款方式产生了上游调用")
	}
	result, err := c.PreparePayment(context.Background(), order)
	if err != nil || result.Outcome != types.PreparationReady || result.NextAction.QRCode == nil || result.NextAction.QRCode.Content != "weixin://wxpay/fixture-code" || result.ProviderResourceRef != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	f.mu.Lock()
	prepay := f.records[len(f.records)-1]
	f.mu.Unlock()
	var sent struct {
		Amount struct {
			Total    int64
			Currency string
		}
		Mchid, Appid, OutTradeNo string
		TimeExpire               string `json:"time_expire"`
	}
	if err := json.Unmarshal(prepay.body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Amount.Total != 29 || sent.Amount.Currency != "CNY" || sent.Mchid != config.MchID || sent.Appid != config.AppID || sent.TimeExpire != deadline.Format(time.RFC3339) {
		t.Fatalf("prepay=%s", prepay.body)
	}
	ref := types.OrderRef{TradeNo: order.TradeNo, GatewayID: 1, ProductCode: ProductNative}
	for _, transactionID := range []string{"", "transaction-29"} {
		ref.ProviderTransactionID = transactionID
		observed, err := c.QueryPayment(context.Background(), ref)
		if err != nil || observed.State != types.ObservationSucceeded || observed.OrderTotal.Minor != 29 {
			t.Fatalf("observation=%+v err=%v", observed, err)
		}
	}
	closed, err := c.ClosePayment(context.Background(), ref)
	if err != nil || closed.State != types.ObservationClosed {
		t.Fatalf("closed=%+v err=%v", closed, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.records[2:4] {
		if !strings.Contains(request.query, "mchid=merchant-A") {
			t.Fatalf("query=%+v", request)
		}
	}
	last := f.records[len(f.records)-1]
	if last.path != "/v3/pay/transactions/out-trade-no/order-29/close" || !bytes.Contains(last.body, []byte(`"mchid":"merchant-A"`)) {
		t.Fatalf("close=%+v", last)
	}
}
func TestColdNotificationVerifiesDecryptsIdentityAndOrderTotal(t *testing.T) {
	f := newFixture(t)
	config := f.merchant("merchant-A", "serial-A", strings.Repeat("a", 32))
	c := f.bind(config, 1, 1)
	callback := callbackFixture(t, f.platforms[0], config.MchAPIv3Key, "TRANSACTION.SUCCESS", transactionFor(config.MchID))
	result, err := c.VerifyNotification(context.Background(), callback)
	if err != nil || result.Observation == nil || result.Observation.OrderTotal.Minor != 29 || result.Observation.ProviderTransactionID != "transaction-29" || result.Observation.SafeDiagnostics != "payer_total=19" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, mutate := range []func(*payments.Transaction){func(v *payments.Transaction) { v.Mchid = core.String("other") }, func(v *payments.Transaction) { v.Appid = core.String("other") }, func(v *payments.Transaction) { v.Amount.Total = nil }, func(v *payments.Transaction) { v.Amount.Currency = core.String("USD") }, func(v *payments.Transaction) { v.TransactionId = nil }} {
		transaction := transactionFor(config.MchID)
		mutate(transaction)
		request := callbackFixture(t, f.platforms[0], config.MchAPIv3Key, "TRANSACTION.SUCCESS", transaction)
		if _, err := c.VerifyNotification(context.Background(), request); err == nil {
			t.Fatal("接受了缺少支付证据或主体不匹配的通知")
		}
	}
	callback.Headers.Set("Wechatpay-Signature", "invalid")
	if _, err := c.VerifyNotification(context.Background(), callback); err == nil {
		t.Fatal("接受了无效签名")
	}
	badKey := callbackFixture(t, f.platforms[0], strings.Repeat("z", 32), "TRANSACTION.SUCCESS", transactionFor(config.MchID))
	if _, err := c.VerifyNotification(context.Background(), badKey); err == nil {
		t.Fatal("接受了错误 APIv3 解密密钥")
	}
	ignored := callbackFixture(t, f.platforms[0], config.MchAPIv3Key, "REFUND.SUCCESS", map[string]string{"refund_id": "refund"})
	result, err = c.VerifyNotification(context.Background(), ignored)
	if err != nil || !result.Ignored {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, outcome := range []types.CallbackOutcome{types.CallbackApplied, types.CallbackAlreadyApplied, types.CallbackIgnored} {
		if c.CallbackResponse(outcome).StatusCode != 204 {
			t.Fatal("成功 ACK 不是 204")
		}
	}
	if c.CallbackResponse(types.CallbackRetryableFailure).StatusCode != 500 || c.CallbackResponse(types.CallbackRejected).StatusCode != 400 {
		t.Fatal("失败 ACK 状态错误")
	}
}
func TestCredentialRotationDownloadsNewCertificateWithNewCredentials(t *testing.T) {
	f := newFixture(t)
	configA := f.merchant("same-merchant", "serial-A", strings.Repeat("a", 32))
	a := f.bind(configA, 1, 1)
	configB := f.merchant("same-merchant", "serial-B", strings.Repeat("b", 32))
	b := f.bind(configB, 1, 2)
	if a.certificates == b.certificates || a.api == b.api {
		t.Fatal("revision 共享了资源")
	}
	newPlatform := newPlatform(t, 202)
	f.mu.Lock()
	f.merchants["serial-A"].allowed = false
	f.platforms = append(f.platforms, newPlatform)
	before := len(f.records)
	f.mu.Unlock()
	if err := b.refresh.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	visitor := b.certificates
	if _, ok := visitor.Get(context.Background(), newPlatform.serial); !ok {
		t.Fatal("新 revision 没有实际取得新平台证书")
	}
	if _, ok := a.certificates.Get(context.Background(), newPlatform.serial); ok {
		t.Fatal("旧 revision 证书缓存被新 revision 改写")
	}
	f.mu.Lock()
	records := append([]requestRecord(nil), f.records[before:]...)
	f.mu.Unlock()
	if len(records) != 1 || records[0].serial != "serial-B" || records[0].path != "/v3/certificates" {
		t.Fatalf("refresh=%+v", records)
	}
	f.mu.Lock()
	f.platforms[0], f.platforms[1] = f.platforms[1], f.platforms[0]
	f.mu.Unlock()
	if _, err := b.QueryPayment(context.Background(), types.OrderRef{GatewayID: b.snapshot.GatewayID, TradeNo: "order-29", ProductCode: ProductNative}); err != nil {
		t.Fatalf("业务响应未使用更新后的证书验签: %v", err)
	}
	request := callbackFixture(t, newPlatform, configB.MchAPIv3Key, "TRANSACTION.SUCCESS", transactionFor(configB.MchID))
	result, err := b.VerifyNotification(context.Background(), request)
	if err != nil || result.Observation == nil || result.Observation.OrderTotal.Minor != 29 {
		t.Fatalf("新平台证书通知验签失败 result=%+v err=%v", result, err)
	}
	assertNotificationCreditsOnce(t, *result.Observation)
	if _, err := a.VerifyNotification(context.Background(), request); err == nil {
		t.Fatal("旧 revision 仍能解密新 APIv3 密钥通知")
	}
	if err := a.CloseResources(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.VerifyNotification(context.Background(), request); err != nil {
		t.Fatalf("旧 revision 退役使新 visitor 失效: %v", err)
	}
}
func TestInstancesKeepBusinessAndDownloaderCredentialsIsolated(t *testing.T) {
	f := newFixture(t)
	a := f.bind(f.merchant("merchant-A", "A1", strings.Repeat("a", 32)), 1, 1)
	same := f.bind(f.merchant("merchant-A", "A2", strings.Repeat("b", 32)), 2, 1)
	other := f.bind(f.merchant("merchant-B", "B1", strings.Repeat("c", 32)), 3, 1)
	for _, client := range []*Client{a, same, other, a, same, other} {
		if err := client.refresh.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		observed, err := client.QueryPayment(context.Background(), types.OrderRef{GatewayID: client.snapshot.GatewayID, TradeNo: "order-29", ProductCode: ProductNative})
		if err != nil || observed.Identity.MerchantAccount != client.config.MchID {
			t.Fatalf("query=%+v err=%v", observed, err)
		}
		request := callbackFixture(t, f.platforms[0], client.config.MchAPIv3Key, "TRANSACTION.SUCCESS", transactionFor(client.config.MchID))
		if _, err := client.VerifyNotification(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if a.certificates == same.certificates || a.certificates == other.certificates || same.certificates == other.certificates {
		t.Fatal("实例共享证书下载器")
	}
}
func TestInvalidCandidateAndConcurrentResourceClose(t *testing.T) {
	f := newFixture(t)
	config := f.merchant("merchant-A", "serial-A", strings.Repeat("a", 32))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newClient(ctx, snapshotFor(t, config, 1, 1), f.client); err == nil {
		t.Fatal("已取消生命周期仍成功初始化")
	}
	f.mu.Lock()
	f.merchants["serial-A"].allowed = false
	f.mu.Unlock()
	if _, err := newClient(context.Background(), snapshotFor(t, config, 1, 1), f.client); err == nil {
		t.Fatal("失效商户凭证仍成功初始化")
	}
	f.mu.Lock()
	f.merchants["serial-A"].allowed = true
	f.mu.Unlock()
	c := f.bind(config, 1, 1)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.CloseResources(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

type errorTransport struct{}

func (errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("fixture initialization failure")
}

func TestFailedInitializationStopsCandidateWorker(t *testing.T) {
	f := newFixture(t)
	snapshot := snapshotFor(t, f.merchant("merchant-A", "serial-A", strings.Repeat("a", 32)), 1, 1)
	synctest.Test(t, func(t *testing.T) {
		_, err := newClient(context.Background(), snapshot, &http.Client{Transport: errorTransport{}, Timeout: time.Second})
		if err == nil {
			t.Fatal("证书下载失败后发布了候选")
		}
		// bubble 必须没有存活的后台任务才能退出；只返回初始化错误不足以通过此检查。
	})
}

func TestFailedCertificateRefreshPreservesVerifiedCache(t *testing.T) {
	f := newFixture(t)
	config := f.merchant("merchant-A", "serial-A", strings.Repeat("a", 32))
	c := f.bind(config, 1, 1)
	f.mu.Lock()
	f.merchants[config.MchCertificateSerialNumber].allowed = false
	f.mu.Unlock()
	if err := c.refresh.refresh(context.Background()); err == nil {
		t.Fatal("失效商户凭据刷新仍成功")
	}
	request := callbackFixture(t, f.platforms[0], config.MchAPIv3Key, "TRANSACTION.SUCCESS", transactionFor(config.MchID))
	if _, err := c.VerifyNotification(context.Background(), request); err != nil {
		t.Fatalf("刷新失败丢失了上次成功取得的证书: %v", err)
	}
	unknown := callbackFixture(t, newPlatform(t, 303), config.MchAPIv3Key, "TRANSACTION.SUCCESS", transactionFor(config.MchID))
	if _, err := c.VerifyNotification(context.Background(), unknown); err == nil {
		t.Fatal("刷新失败后放行了未知证书签名")
	}
}

type contextBlockingTransport struct{}

func (contextBlockingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestInitialCertificateDownloadHasBoundedContext(t *testing.T) {
	f := newFixture(t)
	snapshot := snapshotFor(t, f.merchant("merchant-A", "serial-A", strings.Repeat("a", 32)), 1, 1)
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		_, err := newClient(context.Background(), snapshot, &http.Client{Transport: contextBlockingTransport{}})
		if err == nil || time.Since(start) != certificateDownloadTimeout {
			t.Fatalf("初始化没有按受控下载超时退出: elapsed=%v err=%v", time.Since(start), err)
		}
	})
}
