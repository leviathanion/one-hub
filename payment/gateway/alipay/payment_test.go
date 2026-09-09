package alipay

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"one-api/payment/types"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type fixture struct {
	gateway  *gateway
	merchant *rsa.PrivateKey
	platform *rsa.PrivateKey
	factory  Factory
	calls    atomic.Int32
}

func privatePEM(key *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
func publicPEM(key *rsa.PrivateKey) string {
	encoded, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded}))
}

func newFixture(t *testing.T, handler func(http.ResponseWriter, *http.Request, *fixture)) *fixture {
	t.Helper()
	merchant, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	platform, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{merchant: merchant, platform: platform}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		if r.PostForm.Get("app_id") != "app-1" || !verifyValues(merchant, r.PostForm, false) {
			t.Error("SDK request was not authenticated by the bound merchant")
			http.Error(w, "bad signature", 400)
			return
		}
		handler(w, r, f)
	}))
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	f.factory = Factory{HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		cloned := request.Clone(request.Context())
		endpoint := *request.URL
		endpoint.Scheme = target.Scheme
		endpoint.Host = target.Host
		cloned.URL = &endpoint
		return transport.RoundTrip(cloned)
	})}}
	encoded, _ := json.Marshal(AlipayConfig{AppID: "app-1", SellerID: "seller-1", PrivateKey: privatePEM(merchant), PublicKey: publicPEM(platform), PayType: FacePay})
	binding, err := f.factory.ValidateConfig(context.Background(), types.GatewayConfigInput{Config: string(encoded), Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := f.factory.NewClient(context.Background(), types.GatewaySnapshot{GatewayID: 1, CredentialRevision: 2, Identity: binding.Identity, TransactionNamespace: binding.TransactionNamespace, Config: binding.Config, DefaultProduct: binding.DefaultProduct})
	if err != nil {
		t.Fatal(err)
	}
	f.gateway = client.(*gateway)
	return f
}

func canonicalValues(values url.Values, callback bool) []byte {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key == "sign" || (callback && (key == "sign_type" || key == "alipay_cert_sn")) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values.Get(key))
	}
	return []byte(strings.Join(parts, "&"))
}

func signBytes(key *rsa.PrivateKey, data []byte) string {
	digest := sha256.Sum256(data)
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(signature)
}

func verifyValues(key *rsa.PrivateKey, values url.Values, callback bool) bool {
	signature, err := base64.StdEncoding.DecodeString(values.Get("sign"))
	if err != nil {
		return false
	}
	digest := sha256.Sum256(canonicalValues(values, callback))
	return rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature) == nil
}

func writeResponse(w http.ResponseWriter, f *fixture, method string, payload map[string]any, signature string) {
	encoded, _ := json.Marshal(payload)
	if signature == "valid" {
		signature = signBytes(f.platform, encoded)
	}
	response := map[string]any{strings.ReplaceAll(method, ".", "_") + "_response": json.RawMessage(encoded)}
	if signature != "" {
		response["sign"] = signature
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (f *fixture) order(product string) types.FrozenOrder {
	return types.FrozenOrder{TradeNo: "order-1", GatewayID: 1, Identity: f.gateway.snapshot.Identity, TransactionNamespace: f.gateway.snapshot.TransactionNamespace, ProductCode: product, Total: types.Money{Minor: 61898, Currency: "CNY", Exponent: 2}, LocalDisplayUntil: time.Date(2026, 9, 6, 12, 34, 56, 0, time.UTC), Input: types.CreateInput{Description: "额度充值", NotifyURL: "https://hub.example/callback", ReturnURL: "https://hub.example/topup"}}
}

func TestThreeProductsUseFrozenAmountDeadlineAndSignedActions(t *testing.T) {
	var remoteBiz map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, f *fixture) {
		if err := json.Unmarshal([]byte(r.PostForm.Get("biz_content")), &remoteBiz); err != nil {
			t.Error(err)
		}
		writeResponse(w, f, r.PostForm.Get("method"), map[string]any{"code": "10000", "out_trade_no": "order-1", "qr_code": "https://qr.alipay.example/1"}, "valid")
	})
	for _, product := range []string{ProductFacePay, ProductPagePay, ProductWapPay} {
		t.Run(product, func(t *testing.T) {
			order := f.order(product)
			result, err := f.gateway.PreparePayment(context.Background(), order)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != types.PreparationReady || result.ProviderResourceRef != "" {
				t.Fatalf("result=%+v", result)
			}
			biz := remoteBiz
			if product == ProductFacePay {
				if result.NextAction.QRCode == nil {
					t.Fatal("missing QR")
				}
			} else {
				if result.NextAction.Form == nil || result.NextAction.Form.Method != http.MethodGet {
					t.Fatal("missing signed form")
				}
				params := make(url.Values)
				for key, value := range result.NextAction.Form.Fields {
					params.Set(key, value)
				}
				if !verifyValues(f.merchant, params, false) {
					t.Fatal("invalid browser action signature")
				}
				if err := json.Unmarshal([]byte(params.Get("biz_content")), &biz); err != nil {
					t.Fatal(err)
				}
			}
			wantExpire := "2026-09-06 20:34:56"
			wantUntil := order.LocalDisplayUntil
			if product == ProductWapPay {
				wantExpire = "2026-09-06 20:34"
				wantUntil = wantUntil.Truncate(time.Minute)
			}
			if biz["total_amount"] != "618.98" || biz["out_trade_no"] != "order-1" || biz["seller_id"] != "seller-1" || biz["time_expire"] != wantExpire || biz["timeout_express"] != nil {
				t.Fatalf("bad wire data: %v", biz)
			}
			if !result.ProviderPayableUntil.Equal(wantUntil) || !result.NextAction.ValidUntil.Equal(wantUntil) {
				t.Fatalf("deadline moved: %+v", result)
			}
		})
	}
	if f.calls.Load() != 1 {
		t.Fatalf("browser products performed remote creation: %d", f.calls.Load())
	}
	for _, product := range []string{ProductPagePay, ProductWapPay} {
		if _, err := f.gateway.PreparePayment(context.Background(), f.order(product)); err != nil {
			t.Fatal(err)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatal("redelivery performed remote creation")
	}
}

func (f *fixture) notification(changes map[string]string, post bool) types.CallbackRequest {
	values := url.Values{"app_id": {"app-1"}, "seller_id": {"seller-1"}, "out_trade_no": {"order-1"}, "trade_no": {"provider-1"}, "trade_status": {"TRADE_SUCCESS"}, "total_amount": {"10.00"}, "buyer_pay_amount": {"9.00"}, "receipt_amount": {"9.50"}, "sign_type": {"RSA2"}, "gmt_payment": {"2026-09-06 20:30:00"}}
	for key, value := range changes {
		values.Set(key, value)
	}
	values.Set("sign", signBytes(f.platform, canonicalValues(values, true)))
	if post {
		return types.CallbackRequest{Method: http.MethodPost, Body: []byte(values.Encode()), Headers: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}}
	}
	return types.CallbackRequest{Method: http.MethodGet, Query: values}
}

func TestNotificationsColdStartStatusIdentityAndMoney(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, f *fixture) { t.Error("callback unexpectedly queried") })
	for status, want := range map[string]string{"TRADE_SUCCESS": types.ObservationSucceeded, "TRADE_FINISHED": types.ObservationSucceeded, "WAIT_BUYER_PAY": types.ObservationUnpaid, "TRADE_CLOSED": types.ObservationClosed} {
		for _, post := range []bool{false, true} {
			result, err := f.gateway.VerifyNotification(context.Background(), f.notification(map[string]string{"trade_status": status}, post))
			if err != nil || result.Observation == nil || result.Observation.State != want {
				t.Fatalf("%s post=%t: %+v %v", status, post, result, err)
			}
			if want == types.ObservationSucceeded && (result.Observation.OrderTotal.Minor != 1000 || result.Observation.OrderTotal.Currency != "CNY" || result.Observation.ProviderPaidAt == nil || result.Observation.SafeDiagnostics == "") {
				t.Fatalf("amount/audit=%+v", result.Observation)
			}
		}
	}
	for name, changes := range map[string]map[string]string{"seller": {"seller_id": "other"}, "app": {"app_id": "other"}, "missing_money": {"total_amount": ""}, "precision": {"total_amount": "10.001"}, "missing_trade": {"trade_no": ""}, "missing_order": {"out_trade_no": ""}} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.gateway.VerifyNotification(context.Background(), f.notification(changes, true)); err == nil {
				t.Fatal("bad signed evidence accepted")
			}
		})
	}
	request := f.notification(nil, true)
	request.Query = url.Values{"seller_id": {"other"}}
	if _, err := f.gateway.VerifyNotification(context.Background(), request); err == nil {
		t.Fatal("conflicting sources accepted")
	}
	request = f.notification(nil, false)
	request.Query.Set("total_amount", "1.00")
	if _, err := f.gateway.VerifyNotification(context.Background(), request); err == nil {
		t.Fatal("tampered notification accepted")
	}
	request = f.notification(nil, false)
	request.Query.Add("seller_id", "seller-1")
	if _, err := f.gateway.VerifyNotification(context.Background(), request); err == nil {
		t.Fatal("duplicate fields accepted")
	}
	if f.calls.Load() != 0 {
		t.Fatal("notification required prior Pay")
	}
	for outcome, want := range map[types.CallbackOutcome]string{types.CallbackApplied: "success", types.CallbackAlreadyApplied: "success", types.CallbackIgnored: "success", types.CallbackRetryableFailure: "failure", types.CallbackRejected: "failure"} {
		if response := f.gateway.CallbackResponse(outcome); response.StatusCode != 200 || string(response.Body) != want {
			t.Fatalf("ACK=%+v", response)
		}
	}
}

func TestAuthenticatedQueryAndCloseRejectUnsignedOrMismatchedEvidence(t *testing.T) {
	signature := "valid"
	payload := map[string]any{"code": "10000", "out_trade_no": "order-1", "trade_no": "provider-1", "trade_status": "TRADE_FINISHED", "total_amount": "10.00", "buyer_pay_amount": "9.00", "receipt_amount": "9.50", "trans_currency": "CNY"}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, f *fixture) {
		writeResponse(w, f, r.PostForm.Get("method"), payload, signature)
	})
	ref := types.OrderRef{GatewayID: 1, ProductCode: ProductFacePay, TradeNo: "order-1", ProviderTransactionID: "provider-1"}
	observation, err := f.gateway.QueryPayment(context.Background(), ref)
	if err != nil || observation.State != types.ObservationSucceeded || observation.OrderTotal.Minor != 1000 || observation.Source != types.SourceAuthenticatedQuery || observation.Identity.MerchantAccount != "seller-1" {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
	closed, err := f.gateway.ClosePayment(context.Background(), ref)
	if err != nil || closed.State != types.ObservationClosed {
		t.Fatalf("close=%+v err=%v", closed, err)
	}
	for _, sig := range []string{"", "invalid"} {
		signature = sig
		if _, err = f.gateway.QueryPayment(context.Background(), ref); err == nil {
			t.Fatal("unauthenticated query accepted")
		}
		if _, err = f.gateway.ClosePayment(context.Background(), ref); err == nil {
			t.Fatal("unauthenticated close accepted")
		}
	}
	signature = "valid"
	for key, value := range map[string]any{"seller_id": "other", "app_id": "other", "trans_currency": "USD", "out_trade_no": "other", "trade_no": "other", "total_amount": "10.001"} {
		old, exists := payload[key]
		payload[key] = value
		if _, err = f.gateway.QueryPayment(context.Background(), ref); err == nil {
			t.Errorf("accepted mismatched %s", key)
		}
		if exists {
			payload[key] = old
		} else {
			delete(payload, key)
		}
	}
	payload = map[string]any{"code": "40004", "sub_code": "ACQ.TRADE_NOT_EXIST"}
	observation, err = f.gateway.QueryPayment(context.Background(), ref)
	if err != nil || observation.State != types.ObservationUnknown {
		t.Fatalf("not-found invented final state: %+v %v", observation, err)
	}
	before := f.calls.Load()
	ref.GatewayID = 2
	if _, err = f.gateway.QueryPayment(context.Background(), ref); err == nil || f.calls.Load() != before {
		t.Fatal("wrong binding performed query")
	}
}

func TestPrecreateAmbiguityDoesNotReplay(t *testing.T) {
	var mode atomic.Value
	mode.Store("unsigned")
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request, f *fixture) {
		switch mode.Load().(string) {
		case "disconnect":
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
		case "redirect":
			w.Header().Set("Location", "https://openapi.alipay.com/again")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "rejected":
			writeResponse(w, f, r.PostForm.Get("method"), map[string]any{"code": "40004", "sub_code": "ACQ.INVALID_PARAMETER"}, "valid")
		case "system":
			writeResponse(w, f, r.PostForm.Get("method"), map[string]any{"code": "20000", "sub_code": "ACQ.SYSTEM_ERROR"}, "valid")
		case "empty_qr":
			writeResponse(w, f, r.PostForm.Get("method"), map[string]any{"code": "10000", "out_trade_no": "order-1"}, "valid")
		default:
			writeResponse(w, f, r.PostForm.Get("method"), map[string]any{"code": "10000", "out_trade_no": "order-1", "qr_code": "https://qr.example"}, "")
		}
	})
	for _, selected := range []string{"unsigned", "disconnect", "redirect", "rejected", "system", "empty_qr"} {
		mode.Store(selected)
		before := f.calls.Load()
		result, err := f.gateway.PreparePayment(context.Background(), f.order(ProductFacePay))
		want := types.PreparationUnknown
		if selected == "rejected" {
			want = types.PreparationRejected
		}
		if err == nil || result.Outcome != want || f.calls.Load() != before+1 || result.NextAction.Kind != "none" {
			t.Fatalf("%s result=%+v err=%v calls=%d", selected, result, err, f.calls.Load()-before)
		}
	}
	order := f.order(ProductFacePay)
	order.MethodPreference = "wxpay"
	before := f.calls.Load()
	if result, err := f.gateway.PreparePayment(context.Background(), order); err == nil || result.Outcome != types.PreparationRejected || f.calls.Load() != before {
		t.Fatal("unrepresentable order submitted")
	}
}

func TestBindingsKeepAppAndCredentialsIsolated(t *testing.T) {
	a := newFixture(t, func(w http.ResponseWriter, r *http.Request, f *fixture) { _, _ = io.WriteString(w, "unexpected") })
	b := newFixture(t, func(w http.ResponseWriter, r *http.Request, f *fixture) { _, _ = io.WriteString(w, "unexpected") })
	if _, err := b.gateway.VerifyNotification(context.Background(), a.notification(nil, true)); err == nil {
		t.Fatal("client used another revision platform key")
	}
	var config AlipayConfig
	_ = json.Unmarshal([]byte(a.gateway.snapshot.Config), &config)
	config.AppID = "app-2"
	config.PayType = WapPay
	encoded, _ := json.Marshal(config)
	binding, err := a.factory.ValidateConfig(context.Background(), types.GatewayConfigInput{Config: string(encoded), Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	if binding.TransactionNamespace != a.gateway.snapshot.TransactionNamespace || binding.Identity.AppBinding == a.gateway.snapshot.Identity.AppBinding {
		t.Fatal("app/product changed merchant namespace")
	}
	for _, currency := range []string{"USD", "USDT"} {
		if _, err := a.factory.ValidateConfig(context.Background(), types.GatewayConfigInput{Config: string(encoded), Currency: currency}); err == nil {
			t.Fatalf("currency %s accepted", currency)
		}
	}
	config.SellerID = ""
	encoded, _ = json.Marshal(config)
	if _, err := a.factory.ValidateConfig(context.Background(), types.GatewayConfigInput{Config: string(encoded), Currency: "CNY"}); err == nil {
		t.Fatal("missing expected seller accepted")
	}
}
