package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/model"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func assertSanitizedDebugRaw(t *testing.T, raw any, secrets ...string) {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal raw: %v", err)
	}
	text := string(encoded)
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Fatalf("raw leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "safe-debug-value") {
		t.Fatalf("non-sensitive debug raw was lost: %s", text)
	}
}

func TestUsageAndResetRawSanitizedOnEveryPayloadOutcome(t *testing.T) {
	credentials := &OAuth2Credentials{AccessToken: "access-secret", RefreshToken: "refresh-secret"}
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{
			name: "2xx", status: http.StatusOK,
			body: `{"safe":"safe-debug-value","access-secret":"secret-key-value","nested":{"access":"access-secret"},"items":["refresh-secret","safe-debug-value"]}`,
		},
		{
			name: "non-2xx", status: http.StatusTooManyRequests,
			body: `{"safe":"safe-debug-value","error":{"message":"access-secret refresh-secret"}}`,
		},
		{
			name: "normalize failure", status: http.StatusOK,
			body: `{"safe":"safe-debug-value","rate_limit":"access-secret refresh-secret"}`,
		},
	}

	for _, test := range cases {
		t.Run("usage "+test.name, func(t *testing.T) {
			snapshot, _ := normalizeUsageSnapshot(1, credentials, test.status, []byte(test.body))
			if snapshot == nil {
				t.Fatal("expected diagnostic snapshot")
			}
			assertSanitizedDebugRaw(t, snapshot.Raw, credentials.AccessToken, credentials.RefreshToken)
		})

		t.Run("reset "+test.name, func(t *testing.T) {
			body := test.body
			if test.name == "normalize failure" {
				body = `{"safe":"safe-debug-value","windows_reset":"access-secret refresh-secret"}`
			}
			result, _ := normalizeResetCreditResultWithCredentials(1, credentials, test.status, []byte(body))
			if result == nil {
				t.Fatal("expected minimal reset result")
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "raw") || strings.Contains(string(encoded), credentials.AccessToken) || strings.Contains(string(encoded), credentials.RefreshToken) {
				t.Fatalf("reset result exposed upstream raw: %s", encoded)
			}
		})
	}

	t.Run("whitespace-only body", func(t *testing.T) {
		body := []byte(" \n\t")
		snapshot, snapshotErr := normalizeUsageSnapshot(1, credentials, http.StatusBadGateway, body)
		if snapshotErr == nil || snapshot == nil || snapshot.Raw != invalidCodexRawPlaceholder {
			t.Fatalf("usage malformed raw was not safely omitted: snapshot=%+v err=%v", snapshot, snapshotErr)
		}
		result, resetErr := normalizeResetCreditResultWithCredentials(1, credentials, http.StatusBadGateway, body)
		if resetErr == nil || result == nil {
			t.Fatalf("expected minimal reset failure result: result=%+v err=%v", result, resetErr)
		}
	})

	t.Run("invalid JSON string fallback", func(t *testing.T) {
		body := []byte(`safe-debug-value access-secret refresh-secret {`)
		snapshot, snapshotErr := normalizeUsageSnapshot(1, credentials, http.StatusBadGateway, body)
		if snapshotErr == nil || snapshot == nil {
			t.Fatalf("expected usage decode failure, snapshot=%+v err=%v", snapshot, snapshotErr)
		}
		if snapshot.Raw != invalidCodexRawPlaceholder {
			t.Fatalf("usage malformed raw was not safely omitted: %#v", snapshot.Raw)
		}

		result, resetErr := normalizeResetCreditResultWithCredentials(1, credentials, http.StatusBadGateway, body)
		if resetErr == nil || result == nil {
			t.Fatalf("expected reset decode failure, result=%+v err=%v", result, resetErr)
		}
	})
}

func TestOAuthErrorFieldsRedactCurrentCredentials(t *testing.T) {
	const (
		accessToken  = "oauth-access-secret"
		refreshToken = "oauth-refresh-secret"
		clientID     = "oauth-client-secret"
	)
	withTokenEndpointTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant ` + accessToken + ` ` + clientID + `","error_description":"safe-debug-value ` + refreshToken + `"}`))
	}))

	credentials := &OAuth2Credentials{AccessToken: accessToken, RefreshToken: refreshToken, ClientID: clientID}
	err := credentials.Refresh(context.Background(), "")
	if err == nil {
		t.Fatal("expected OAuth error")
	}
	message := err.Error()
	for _, secret := range []string{accessToken, refreshToken, clientID} {
		if strings.Contains(message, secret) {
			t.Fatalf("OAuth error leaked %q: %q", secret, message)
		}
	}
	if !strings.Contains(message, "safe-debug-value") || !strings.Contains(message, "[redacted]") {
		t.Fatalf("sanitization discarded safe OAuth detail: %q", message)
	}
}

func TestOAuthBackgroundErrorsAndLogsAreSanitized(t *testing.T) {
	const (
		accessToken  = "background-access-secret"
		refreshToken = "background-refresh-secret"
		clientID     = "background-client-secret"
	)
	credentials := &OAuth2Credentials{
		AccessToken: accessToken, RefreshToken: refreshToken, ClientID: clientID,
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	key, err := credentials.ToJSON()
	if err != nil {
		t.Fatalf("encode credentials: %v", err)
	}
	channel := &model.Channel{Id: 22001, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled, Key: key}

	originalLoadChannels := loadAutoRefreshChannels
	originalLoadLatest := loadLatestChannelByID
	originalRefresh := refreshOAuthCredentials
	originalLogger := logger.Logger
	core, observedLogs := observer.New(zapcore.ErrorLevel)
	loadAutoRefreshChannels = func(context.Context) ([]*model.Channel, error) { return []*model.Channel{channel}, nil }
	loadLatestChannelByID = func(context.Context, int) (*model.Channel, error) {
		copy := *channel
		return &copy, nil
	}
	refreshOAuthCredentials = func(*OAuth2Credentials, context.Context, string) error {
		return errors.New("safe-debug-value " + accessToken + " " + refreshToken + " " + clientID + " Authorization: Bearer\r\n folded-header-secret " + `{"access\u005ftoken":"unicode-key-secret"} accessToken='unknown-secret tail`)
	}
	logger.Logger = zap.New(core)
	t.Cleanup(func() {
		loadAutoRefreshChannels = originalLoadChannels
		loadLatestChannelByID = originalLoadLatest
		refreshOAuthCredentials = originalRefresh
		logger.Logger = originalLogger
	})
	cache.InitCacheManager()

	first := refreshAutoRefreshChannel(context.Background(), channel).FirstErr
	if first == "" {
		t.Fatal("expected background FirstErr")
	}
	RefreshChannelsInBackground(context.Background())
	last := GetAutoRefreshStatus().LastError
	for label, text := range map[string]string{"FirstErr": first, "LastError": last} {
		for _, secret := range []string{accessToken, refreshToken, clientID, "folded-header-secret", "unicode-key-secret", "unknown-secret", "tail"} {
			if strings.Contains(text, secret) {
				t.Fatalf("%s leaked %q: %q", label, secret, text)
			}
		}
		if !strings.Contains(text, "safe-debug-value") || !strings.Contains(text, "[redacted]") {
			t.Fatalf("%s lost safe detail or marker: %q", label, text)
		}
	}
	for _, entry := range observedLogs.All() {
		for _, secret := range []string{accessToken, refreshToken, clientID, "folded-header-secret", "unicode-key-secret"} {
			if strings.Contains(entry.Message, secret) {
				t.Fatalf("background log leaked %q: %q", secret, entry.Message)
			}
		}
	}
}

func TestTokenRefreshRedactionHandlesOverlappingSecrets(t *testing.T) {
	credentials := &OAuth2Credentials{
		AccessToken:  "secret",
		RefreshToken: "secret-refresh",
		ClientID:     "secret-refresh-client",
	}
	input := "safe-debug-value secret-refresh-client secret-refresh secret"
	redacted := redactTokenRefreshSecrets(input, credentials, credentials.ClientID)
	for _, secret := range []string{credentials.AccessToken, credentials.RefreshToken, credentials.ClientID} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("overlapping credential leaked %q: %q", secret, redacted)
		}
	}
	if !strings.Contains(redacted, "safe-debug-value") {
		t.Fatalf("non-sensitive error detail was lost: %q", redacted)
	}
}
