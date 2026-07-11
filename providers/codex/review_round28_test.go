package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common/requester"
)

func TestConsumeResetCreditClassifiesOnlyUnusable2xxAsCommitted(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantCommitted bool
	}{
		{name: "malformed success", status: http.StatusOK, body: `{"windows_reset":`, wantCommitted: true},
		{name: "oversized success", status: http.StatusOK, body: strings.Repeat("x", usageResponseBodyMaxBytes+1), wantCommitted: true},
		{name: "oversized failure", status: http.StatusBadGateway, body: strings.Repeat("x", usageResponseBodyMaxBytes+1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			originalClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = originalClient })

			provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
			provider.Channel.BaseURL = stringPtr(server.URL)
			result, err := provider.ConsumeResetCredit(context.Background())
			if err == nil {
				t.Fatal("expected unusable/error response to return an error classification")
			}
			if got := IsResetCreditCommittedResponseUnusable(err); got != test.wantCommitted {
				t.Fatalf("committed classification=%v, want %v: %v", got, test.wantCommitted, err)
			}
			if result == nil {
				t.Fatal("expected minimal result preserving status")
			}
			wantCode := "200"
			if test.status == http.StatusBadGateway {
				wantCode = "502"
			}
			if result.Code != wantCode {
				t.Fatalf("status was not preserved: %+v", result)
			}
			encoded, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if strings.Contains(string(encoded), "raw") || len(encoded) > 1024 {
				t.Fatalf("reset result must stay modeled and small: %d bytes %s", len(encoded), encoded)
			}
			if requests.Load() != 1 {
				t.Fatalf("irreversible POST must not be retried, requests=%d", requests.Load())
			}
			if test.wantCommitted && !errors.Is(err, ErrResetCreditCommittedResponseUnusable) {
				t.Fatalf("committed error must support errors.Is: %v", err)
			}
		})
	}
}

func TestFullwidthUnknownCredentialFieldStillRedactsKnownSecretValue(t *testing.T) {
	credentials := &OAuth2Credentials{AccessToken: "known-secret"}
	redacted := redactUsageCredentialValues(map[string]any{"ａｃｃｅｓｓ＿ｔｏｋｅｎ": "known-secret"}, credentials)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), credentials.AccessToken) {
		t.Fatalf("known credential leaked through non-standard field: %s", encoded)
	}
}
