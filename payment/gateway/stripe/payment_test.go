package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/stripe/stripe-go/v80"
	"github.com/stripe/stripe-go/v80/webhook"
	"one-api/payment/types"
)

func testFactory(t *testing.T, handler http.HandlerFunc) Factory {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return Factory{endpointURL: server.URL, httpClient: server.Client()}
}

func testClient(t *testing.T, factory Factory, account, key string, id int) *Stripe {
	t.Helper()
	config := StripeConfig{AccountID: account, Environment: "test", SecretKey: key, WebhookSecret: "whsec_" + account}
	raw, _ := json.Marshal(config)
	client, err := factory.NewClient(context.Background(), types.GatewaySnapshot{GatewayID: id, Identity: identityFor(config), TransactionNamespace: namespaceFor(config), DefaultProduct: product, Config: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return client.(*Stripe)
}

func frozenOrder(client *Stripe, trade string) types.FrozenOrder {
	return types.FrozenOrder{TradeNo: trade, GatewayID: client.snapshot.GatewayID, Identity: client.snapshot.Identity, TransactionNamespace: client.snapshot.TransactionNamespace, ProductCode: product, Total: types.Money{Minor: 61898, Currency: "USD", Exponent: 2}, LocalDisplayUntil: time.Now().Add(3 * time.Hour), Input: types.CreateInput{ReturnURL: "https://shop.example/return", Description: "额度充值", CustomerEmail: "payer@example.com"}}
}

func checkoutFixture(trade string) sdk.CheckoutSession {
	return sdk.CheckoutSession{ID: "cs_" + trade, ClientReferenceID: trade, Mode: sdk.CheckoutSessionModePayment, Status: sdk.CheckoutSessionStatusComplete, PaymentStatus: sdk.CheckoutSessionPaymentStatusPaid, AmountTotal: 61898, Currency: sdk.CurrencyUSD, PaymentIntent: &sdk.PaymentIntent{ID: "pi_" + trade}}
}

func signedNotification(t *testing.T, client *Stripe, eventType string, checkout sdk.CheckoutSession, mutate func(map[string]any)) types.CallbackRequest {
	t.Helper()
	envelope := map[string]any{"id": "evt_" + eventType, "object": "event", "type": eventType, "api_version": sdk.APIVersion, "livemode": false, "created": time.Now().Unix(), "data": map[string]any{"object": checkout}}
	if mutate != nil {
		mutate(envelope)
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: raw, Secret: client.config.WebhookSecret})
	return types.CallbackRequest{Method: http.MethodPost, Body: signed.Payload, Headers: http.Header{"Stripe-Signature": []string{signed.Header}}}
}

func TestValidateBindsAuthenticatedAccountAndMode(t *testing.T) {
	factory := testFactory(t, func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key != "sk_live_fixture" {
			t.Errorf("wrong key %q", key)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/account":
			fmt.Fprint(w, `{"id":"acct_authenticated"}`)
		case "/v1/balance":
			fmt.Fprint(w, `{"object":"balance","livemode":true}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})
	binding, err := factory.ValidateConfig(context.Background(), types.GatewayConfigInput{Config: `{"secret_key":"sk_live_fixture"}`, Currency: "USD", Product: product})
	if err != nil {
		t.Fatal(err)
	}
	if binding.Identity.MerchantAccount != "acct_authenticated" || binding.Identity.Environment != "live" || binding.TransactionNamespace != types.Namespace("stripe", "acct_authenticated", "live") {
		t.Fatalf("bad binding %+v", binding)
	}
	if _, err := factory.ValidateConfig(context.Background(), types.GatewayConfigInput{Config: `{"secret_key":"sk_live_fixture","account_id":"acct_other"}`, Currency: "USD"}); err == nil {
		t.Fatal("accepted different account")
	}
	if _, err := factory.ValidateConfig(context.Background(), types.GatewayConfigInput{Config: `{"secret_key":"sk_live_fixture","environment":"test"}`, Currency: "USD"}); err == nil {
		t.Fatal("accepted different mode")
	}
}

func TestPrepareUsesFrozenMinorAndExpiryWithoutRetries(t *testing.T) {
	var calls atomic.Int32
	var frozen types.FrozenOrder
	factory := testFactory(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/checkout/sessions" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		for key, expected := range map[string]string{"mode": "payment", "client_reference_id": frozen.TradeNo, "expires_at": strconv.FormatInt(frozen.LocalDisplayUntil.Unix(), 10), "line_items[0][price_data][unit_amount]": "61898", "line_items[0][price_data][currency]": "cny", "line_items[0][quantity]": "1", "customer_email": "payer@example.com"} {
			if r.Form.Get(key) != expected {
				t.Errorf("%s=%q want %q", key, r.Form.Get(key), expected)
			}
		}
		if r.Form.Has("allow_promotion_codes") || r.Form.Has("automatic_tax") {
			t.Error("mutable price options enabled")
		}
		checkout := checkoutFixture(frozen.TradeNo)
		checkout.Currency = sdk.CurrencyCNY
		checkout.Status, checkout.PaymentStatus, checkout.URL, checkout.ExpiresAt = sdk.CheckoutSessionStatusOpen, sdk.CheckoutSessionPaymentStatusUnpaid, "https://checkout.stripe.com/c/pay/fixture", frozen.LocalDisplayUntil.Unix()
		json.NewEncoder(w).Encode(checkout)
	})
	client := testClient(t, factory, "acct_A", "sk_test_A", 1)
	frozen = frozenOrder(client, "trade_create")
	frozen.Total.Currency = "CNY"
	prepared, err := client.PreparePayment(context.Background(), frozen)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Outcome != types.PreparationReady || prepared.ProviderResourceRef != "cs_trade_create" || prepared.ProviderPayableUntil.Unix() != frozen.LocalDisplayUntil.Unix() || prepared.NextAction.Redirect == nil {
		t.Fatalf("bad prepare %+v", prepared)
	}
	invalid := frozen
	invalid.Total.Minor = 0
	if result, err := client.PreparePayment(context.Background(), invalid); err == nil || result.Outcome != types.PreparationRejected {
		t.Fatalf("invalid amount %+v %v", result, err)
	}
	invalid = frozen
	invalid.LocalDisplayUntil = time.Now().Add(15 * time.Minute)
	if _, err := client.PreparePayment(context.Background(), invalid); err == nil {
		t.Fatal("accepted unsupported expiry")
	}
	if calls.Load() != 1 {
		t.Fatalf("provider called %d times", calls.Load())
	}

	var failures atomic.Int32
	failingFactory := testFactory(t, func(w http.ResponseWriter, r *http.Request) {
		failures.Add(1)
		w.Header().Set("Stripe-Should-Retry", "true")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"type":"api_error","message":"ambiguous"}}`)
	})
	failing := testClient(t, failingFactory, "acct_A", "sk_test_A", 1)
	result, err := failing.PreparePayment(context.Background(), frozenOrder(failing, "unknown"))
	if err == nil || result.Outcome != types.PreparationUnknown || failures.Load() != 1 {
		t.Fatalf("create replay result=%+v err=%v calls=%d", result, err, failures.Load())
	}
}

func TestNotificationStateAndStableReferences(t *testing.T) {
	client := testClient(t, Factory{}, "acct_A", "sk_test_A", 1)
	tests := []struct{ name, event, payment, status, state string }{
		{"completed unpaid", requiredEvents[0], "unpaid", "complete", types.ObservationProcessing},
		{"completed paid", requiredEvents[0], "paid", "complete", types.ObservationSucceeded},
		{"async succeeded", requiredEvents[1], "paid", "complete", types.ObservationSucceeded},
		{"async failed", requiredEvents[2], "unpaid", "complete", types.ObservationAttemptFailed},
		{"expired", requiredEvents[3], "unpaid", "expired", types.ObservationClosed},
		{"free session", requiredEvents[0], "no_payment_required", "complete", types.ObservationIgnored},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checkout := checkoutFixture("trade_states")
			checkout.PaymentStatus = sdk.CheckoutSessionPaymentStatus(test.payment)
			checkout.Status = sdk.CheckoutSessionStatus(test.status)
			result, err := client.VerifyNotification(context.Background(), signedNotification(t, client, test.event, checkout, nil))
			if err != nil || result.Observation == nil || result.Observation.State != test.state {
				t.Fatalf("observation=%+v err=%v", result, err)
			}
			obs := result.Observation
			if obs.ProviderResourceRef != "cs_trade_states" || obs.ProviderTransactionID != "pi_trade_states" || obs.ProviderEventID != "evt_"+test.event || obs.Source != types.SourceVerifiedCallback {
				t.Fatalf("wrong references %+v", obs)
			}
			if test.state == types.ObservationSucceeded && (obs.OrderTotal == nil || obs.OrderTotal.Minor != 61898 || obs.OrderTotal.Currency != "USD") {
				t.Fatalf("wrong total %+v", obs)
			}
		})
	}
	result, err := client.VerifyNotification(context.Background(), signedNotification(t, client, "customer.created", checkoutFixture("ignored"), nil))
	if err != nil || !result.Ignored {
		t.Fatalf("ignored=%+v %v", result, err)
	}
}

func TestNotificationRejectsWrongIdentityAndRequestsMissingEvidence(t *testing.T) {
	client := testClient(t, Factory{}, "acct_A", "sk_test_A", 1)
	other := testClient(t, Factory{}, "acct_B", "sk_test_B", 2)
	valid := signedNotification(t, client, requiredEvents[0], checkoutFixture("trade"), nil)
	if _, err := other.VerifyNotification(context.Background(), valid); err == nil {
		t.Fatal("accepted another secret")
	}
	for _, mutate := range []func(map[string]any){func(e map[string]any) { e["account"] = "acct_B" }, func(e map[string]any) { e["livemode"] = true }, func(e map[string]any) { e["api_version"] = "2020-08-27" }} {
		if _, err := client.VerifyNotification(context.Background(), signedNotification(t, client, requiredEvents[0], checkoutFixture("trade"), mutate)); err == nil {
			t.Fatal("accepted wrong account/mode/version")
		}
	}
	for _, field := range []string{"intent", "total", "trade"} {
		checkout := checkoutFixture("trade")
		switch field {
		case "intent":
			checkout.PaymentIntent = nil
		case "total":
			checkout.AmountTotal = 0
		case "trade":
			checkout.ClientReferenceID = ""
		}
		result, err := client.VerifyNotification(context.Background(), signedNotification(t, client, requiredEvents[0], checkout, nil))
		if err != nil || result.Observation != nil || result.QueryRef == nil || result.QueryRef.ProviderResourceRef != "cs_trade" {
			t.Fatalf("missing %s result=%+v err=%v", field, result, err)
		}
	}
	for outcome, status := range map[types.CallbackOutcome]int{types.CallbackApplied: 200, types.CallbackAlreadyApplied: 200, types.CallbackIgnored: 200, types.CallbackRejected: 400, types.CallbackRetryableFailure: 503} {
		if response := client.CallbackResponse(outcome); response.StatusCode != status {
			t.Fatalf("ack %s=%d", outcome, response.StatusCode)
		}
	}
}

func TestQueryCloseAndSameTypeClientsStayIsolated(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	factory := testFactory(t, func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		account := strings.TrimPrefix(key, "sk_test_")
		if account != "A" && account != "B" {
			t.Errorf("wrong key %q", key)
		}
		if r.URL.Path != "/v1/checkout/sessions" && !strings.Contains(r.URL.Path, "trade_"+account) {
			t.Errorf("key %s used resource %s", key, r.URL.Path)
		}
		checkout := checkoutFixture("trade_" + account)
		checkout.Status, checkout.PaymentStatus = sdk.CheckoutSessionStatusOpen, sdk.CheckoutSessionPaymentStatusUnpaid
		if r.Method == http.MethodPost {
			if r.URL.Path == "/v1/checkout/sessions" {
				r.ParseForm()
				if r.Form.Get("client_reference_id") != "trade_"+account {
					t.Errorf("created wrong order %v", r.Form)
				}
				checkout.URL = "https://checkout.stripe.com/pay/" + account
				checkout.ExpiresAt, _ = strconv.ParseInt(r.Form.Get("expires_at"), 10, 64)
			} else {
				checkout.Status = sdk.CheckoutSessionStatusExpired
			}
		}
		mu.Lock()
		seen[account+" "+r.Method]++
		mu.Unlock()
		json.NewEncoder(w).Encode(checkout)
	})
	for _, account := range []string{"A", "B", "A", "B"} {
		client := testClient(t, factory, "acct_"+account, "sk_test_"+account, int(account[0]))
		prepared, err := client.PreparePayment(context.Background(), frozenOrder(client, "trade_"+account))
		if err != nil || prepared.ProviderResourceRef != "cs_trade_"+account {
			t.Fatalf("create isolation %+v %v", prepared, err)
		}
		ref := types.OrderRef{TradeNo: "trade_" + account, GatewayID: client.snapshot.GatewayID, ProductCode: product, ProviderResourceRef: "cs_trade_" + account}
		observed, err := client.QueryPayment(context.Background(), ref)
		if err != nil || observed.Identity.MerchantAccount != "acct_"+account || observed.Source != types.SourceAuthenticatedQuery {
			t.Fatalf("query %+v %v", observed, err)
		}
		closed, err := client.ClosePayment(context.Background(), ref)
		if err != nil || closed.State != types.ObservationClosed {
			t.Fatalf("close %+v %v", closed, err)
		}
		notification, err := client.VerifyNotification(context.Background(), signedNotification(t, client, requiredEvents[0], checkoutFixture(ref.TradeNo), nil))
		if err != nil || notification.Observation.Identity != client.snapshot.Identity {
			t.Fatalf("callback isolation %+v %v", notification, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, account := range []string{"A", "B"} {
		if seen[account+" GET"] != 4 || seen[account+" POST"] != 4 {
			t.Fatalf("requests=%v", seen)
		}
	}
}

func TestCloseDoesNotExpirePaidOrProcessingAndQueryNeedsResource(t *testing.T) {
	var calls atomic.Int32
	for _, payment := range []sdk.CheckoutSessionPaymentStatus{sdk.CheckoutSessionPaymentStatusPaid, sdk.CheckoutSessionPaymentStatusUnpaid} {
		factory := testFactory(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Method != http.MethodGet {
				t.Errorf("unexpected expire %s", r.Method)
			}
			checkout := checkoutFixture("trade")
			checkout.PaymentStatus = payment
			json.NewEncoder(w).Encode(checkout)
		})
		client := testClient(t, factory, "acct_A", "sk_test_A", 1)
		ref := types.OrderRef{TradeNo: "trade", GatewayID: 1, ProductCode: product, ProviderResourceRef: "cs_trade"}
		closed, err := client.ClosePayment(context.Background(), ref)
		expected := types.ObservationSucceeded
		if payment == sdk.CheckoutSessionPaymentStatusUnpaid {
			expected = types.ObservationProcessing
		}
		if err != nil || closed.State != expected {
			t.Fatalf("close %+v %v", closed, err)
		}
		ref.ProviderResourceRef = ""
		if _, err := client.QueryPayment(context.Background(), ref); err == nil {
			t.Fatal("queried without session")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected %d requests", calls.Load())
	}
}

func TestConfigureCallbacksRequiredEventsAndPreserveSecret(t *testing.T) {
	var updates atomic.Int32
	factory := testFactory(t, func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		account := strings.TrimPrefix(key, "sk_test_")
		endpoint := sdk.WebhookEndpoint{ID: "we_" + account, URL: "https://shop.example/notify/" + account, EnabledEvents: []string{requiredEvents[0]}, APIVersion: sdk.APIVersion, Status: "enabled"}
		if r.Method == http.MethodGet {
			if r.URL.Path == "/v1/webhook_endpoints" {
				json.NewEncoder(w).Encode(sdk.WebhookEndpointList{Data: []*sdk.WebhookEndpoint{&endpoint}})
			} else {
				json.NewEncoder(w).Encode(endpoint)
			}
			return
		}
		updates.Add(1)
		if r.URL.Path != "/v1/webhook_endpoints/we_"+account {
			t.Errorf("wrong endpoint %s", r.URL.Path)
		}
		r.ParseForm()
		for _, event := range requiredEvents {
			found := false
			for _, values := range r.Form {
				if slices.Contains(values, event) {
					found = true
				}
			}
			if !found {
				t.Errorf("missing %s: %v", event, r.Form)
			}
		}

		json.NewEncoder(w).Encode(endpoint)
	})
	for _, account := range []string{"A", "B", "A", "B"} {
		client := testClient(t, factory, "acct_"+account, "sk_test_"+account, int(account[0]))
		if account == "B" {
			client.config.WebhookEndpointID = "we_B"
		}
		setup, err := client.ConfigureCallbacks(context.Background(), types.CallbackEndpoint{URL: "https://shop.example/notify/" + account})
		if err != nil || !setup.Ready {
			t.Fatalf("setup %+v %v", setup, err)
		}
		var config StripeConfig
		json.Unmarshal([]byte(setup.Config), &config)
		if config.WebhookSecret != "whsec_acct_"+account || config.WebhookEndpointID != "we_"+account {
			t.Fatalf("lost secret/isolation %+v", config)
		}
	}
	if updates.Load() != 4 {
		t.Fatalf("updates=%d", updates.Load())
	}
}

func TestConfigureCreatesVersionedEndpointAndMissingSecretFails(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(strconv.FormatBool(exists), func(t *testing.T) {
			var created atomic.Int32
			factory := testFactory(t, func(w http.ResponseWriter, r *http.Request) {
				endpoint := sdk.WebhookEndpoint{ID: "we_fixture", URL: "https://shop.example/notify", APIVersion: sdk.APIVersion, Status: "enabled", EnabledEvents: requiredEvents}
				if r.Method == http.MethodGet {
					data := []*sdk.WebhookEndpoint{}
					if exists {
						data = append(data, &endpoint)
					}
					json.NewEncoder(w).Encode(sdk.WebhookEndpointList{Data: data})
					return
				}
				created.Add(1)
				r.ParseForm()
				if r.Form.Get("api_version") != sdk.APIVersion {
					t.Errorf("version %v", r.Form)
				}
				for _, event := range requiredEvents {
					found := false
					for _, values := range r.Form {
						if slices.Contains(values, event) {
							found = true
						}
					}
					if !found {
						t.Errorf("event %s missing", event)
					}
				}
				endpoint.Secret = "whsec_new"
				json.NewEncoder(w).Encode(endpoint)
			})
			client := testClient(t, factory, "acct_A", "sk_test_A", 1)
			client.config.WebhookSecret = ""
			result, err := client.ConfigureCallbacks(context.Background(), types.CallbackEndpoint{URL: "https://shop.example/notify"})
			if exists {
				if err == nil || created.Load() != 0 {
					t.Fatalf("existing secret accepted or recreated %+v %v", result, err)
				}
				return
			}
			if err != nil || !result.Ready || created.Load() != 1 || !strings.Contains(result.Config, "whsec_new") {
				t.Fatalf("new setup %+v %v", result, err)
			}
		})
	}
}
