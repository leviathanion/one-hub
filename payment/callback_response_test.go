package payment_test

import (
	"net/http"
	"one-api/payment/gateway/stripe"
	"one-api/payment/gateway/wxpay"
	"one-api/payment/types"
	"testing"
)

func TestNotificationHTTPResponsesAreProducedAfterBusinessOutcome(t *testing.T) {
	for _, test := range []struct {
		name     string
		receiver types.CallbackReceiver
		ok       int
	}{{"wxpay", &wxpay.Client{}, http.StatusNoContent}, {"stripe", &stripe.Stripe{}, http.StatusOK}} {
		t.Run(test.name, func(t *testing.T) {
			for _, outcome := range []types.CallbackOutcome{types.CallbackApplied, types.CallbackAlreadyApplied, types.CallbackIgnored} {
				if response := test.receiver.CallbackResponse(outcome); response.StatusCode != test.ok {
					t.Fatalf("outcome=%s status=%d", outcome, response.StatusCode)
				}
			}
			for _, outcome := range []types.CallbackOutcome{types.CallbackRetryableFailure, types.CallbackRejected} {
				if response := test.receiver.CallbackResponse(outcome); response.StatusCode < 400 {
					t.Fatalf("failure outcome=%s acknowledged status=%d", outcome, response.StatusCode)
				}
			}
		})
	}
}
