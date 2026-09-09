package epay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"one-api/payment/types"
	"testing"
	"time"
)

func testGateway(t *testing.T, method PayType, partner, key string) *gateway {
	t.Helper()
	encoded, _ := json.Marshal(EpayConfig{PayType: method, ProtocolProfile: Profile, Client: Client{PayDomain: "https://pay.example", PartnerID: partner, Key: key}})
	binding, err := (Factory{}).ValidateConfig(context.Background(), types.GatewayConfigInput{Config: string(encoded), Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := (Factory{}).NewClient(context.Background(), types.GatewaySnapshot{GatewayID: 1, UUID: "test", CredentialRevision: 1, Config: binding.Config, Identity: binding.Identity, TransactionNamespace: binding.TransactionNamespace, DefaultProduct: binding.DefaultProduct})
	if err != nil {
		t.Fatal(err)
	}
	return client.(*gateway)
}

func callback(g *gateway, changes map[string]string) types.CallbackRequest {
	params := map[string]string{"pid": g.snapshot.Identity.MerchantAccount, "type": "alipay", "trade_no": "provider-1", "out_trade_no": "order-1", "trade_status": TradeStatusSuccess, "money": "10.00", "sign_type": FormArgsSignType}
	for key, value := range changes {
		params[key] = value
	}
	params["sign"] = g.config.Sign(params)
	query := make(url.Values)
	for key, value := range params {
		query.Set(key, value)
	}
	return types.CallbackRequest{Method: http.MethodGet, Query: query}
}

func TestAllMethodsPrepareFrozenForm(t *testing.T) {
	for _, method := range []PayType{EpayPay, Alipay, Wechat, QQ, Bank, JD, PayPal, USDT} {
		t.Run(string(method), func(t *testing.T) {
			g := testGateway(t, method, "partner", "secret")
			until := time.Date(2026, 9, 6, 12, 34, 56, 0, time.UTC)
			order := types.FrozenOrder{TradeNo: "order-1", GatewayID: 1, Identity: g.snapshot.Identity, TransactionNamespace: g.snapshot.TransactionNamespace, ProductCode: Checkout, MethodPreference: string(method), Total: types.Money{Minor: 61898, Currency: "CNY", Exponent: 2}, LocalDisplayUntil: until, Input: types.CreateInput{NotifyURL: "https://hub.example/callback", ReturnURL: "https://hub.example/topup", Description: "充值"}}
			result, err := g.PreparePayment(context.Background(), order)
			if err != nil {
				t.Fatal(err)
			}
			form := result.NextAction.Form
			if result.Outcome != types.PreparationReady || result.ProviderResourceRef != "" || result.ProviderPayableUntil != nil || form == nil || form.Method != http.MethodPost || form.ActionURL != "https://pay.example/submit.php" {
				t.Fatalf("unexpected preparation: %+v", result)
			}
			if form.Fields["type"] != string(method) || form.Fields["money"] != "618.98" || form.Fields["out_trade_no"] != "order-1" || form.Fields["notify_url"] != order.Input.NotifyURL || form.Fields["sign"] != g.config.Sign(form.Fields) || !result.NextAction.ValidUntil.Equal(until) {
				t.Fatalf("form=%+v", form)
			}
			if _, supported := any(g).(types.PaymentQuerier); supported {
				t.Fatal("profile invented query capability")
			}
			if _, supported := any(g).(types.PaymentCloser); supported {
				t.Fatal("profile invented close capability")
			}
			order.MethodPreference = "unknown"
			if result, err = g.PreparePayment(context.Background(), order); err == nil || result.Outcome != types.PreparationRejected {
				t.Fatal("unknown method accepted")
			}
		})
	}
}

func TestVerifiedCompleteEvidenceAndACK(t *testing.T) {
	g := testGateway(t, Alipay, "partner", "secret")
	request := callback(g, nil)
	// 固定 MD5 样例独立于签名实现，避免创建和验证同时偏离排序/拼接协议。
	request.Query.Set("sign", "a60c27e1f5fb1d2d8994b18bb647ab47")
	result, err := g.VerifyNotification(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	observation := result.Observation
	if observation == nil || observation.State != types.ObservationSucceeded || observation.OrderTotal.Minor != 1000 || observation.OrderTotal.Currency != "CNY" || observation.TradeNo != "order-1" || observation.ProviderTransactionID != "provider-1" || observation.Identity.MerchantAccount != "partner" || observation.TransactionNamespace != g.snapshot.TransactionNamespace {
		t.Fatalf("result=%+v", result)
	}
	for outcome, expected := range map[types.CallbackOutcome]string{types.CallbackApplied: "success", types.CallbackAlreadyApplied: "success", types.CallbackIgnored: "success", types.CallbackRetryableFailure: "fail", types.CallbackRejected: "fail"} {
		ack := g.CallbackResponse(outcome)
		if ack.StatusCode != http.StatusOK || string(ack.Body) != expected {
			t.Fatalf("ACK for %s: %+v", outcome, ack)
		}
	}
}

func TestCallbackBoundaries(t *testing.T) {
	g := testGateway(t, Alipay, "partner", "secret")
	for name, changes := range map[string]map[string]string{"merchant": {"pid": "other"}, "amount_missing": {"money": ""}, "amount_precision": {"money": "10.001"}, "amount_zero": {"money": "0.00"}, "transaction": {"trade_no": ""}, "order": {"out_trade_no": ""}, "sign_type": {"sign_type": "RSA"}} {
		t.Run(name, func(t *testing.T) {
			if _, err := g.VerifyNotification(context.Background(), callback(g, changes)); err == nil {
				t.Fatal("invalid signed evidence accepted")
			}
		})
	}
	request := callback(g, nil)
	request.Query.Set("money", "1.00")
	if _, err := g.VerifyNotification(context.Background(), request); err == nil {
		t.Fatal("tampered amount accepted")
	}
	request = callback(g, nil)
	request.Query.Add("pid", "partner")
	if _, err := g.VerifyNotification(context.Background(), request); err == nil {
		t.Fatal("duplicate field accepted")
	}
	request = callback(g, nil)
	request.Method = http.MethodPost
	request.Body = []byte(request.Query.Encode())
	request.Query = nil
	if _, err := g.VerifyNotification(context.Background(), request); err == nil {
		t.Fatal("unproven POST dialect accepted")
	}
	result, err := g.VerifyNotification(context.Background(), callback(g, map[string]string{"trade_status": "WAIT_BUYER_PAY", "money": ""}))
	if err != nil || !result.Ignored {
		t.Fatalf("signed non-success misclassified: %+v %v", result, err)
	}
}

func TestBindingIsolationAndProfileValidation(t *testing.T) {
	a := testGateway(t, Alipay, "partner", "secret-a")
	b := testGateway(t, Wechat, "partner", "secret-b")
	if a.snapshot.TransactionNamespace != b.snapshot.TransactionNamespace {
		t.Fatal("payment method split transaction namespace")
	}
	if _, err := b.VerifyNotification(context.Background(), callback(a, nil)); err == nil {
		t.Fatal("gateway clients share credentials")
	}
	for _, change := range []map[string]any{{"protocol_profile": "unknown"}, {"pay_type": "unknown"}, {"partner_id": ""}, {"environment": "sandbox"}} {
		var config map[string]any
		_ = json.Unmarshal([]byte(a.snapshot.Config), &config)
		for key, value := range change {
			config[key] = value
		}
		encoded, _ := json.Marshal(config)
		if _, err := (Factory{}).ValidateConfig(context.Background(), types.GatewayConfigInput{Config: string(encoded), Currency: "CNY"}); err == nil {
			t.Fatalf("bad config accepted: %v", change)
		}
	}
	if _, err := (Factory{}).ValidateConfig(context.Background(), types.GatewayConfigInput{Config: a.snapshot.Config, Currency: "USD"}); err == nil {
		t.Fatal("unproven currency accepted")
	}
}
