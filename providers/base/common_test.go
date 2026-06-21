package base

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requestbody"
	"strings"
	"testing"
	"time"

	"one-api/common"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func TestGetRawBodyCachesRequestBodyOnDemand(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := `{"prompt":"hello world"}`

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/recraftAI/v1/styles", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	provider := &BaseProvider{Context: ctx}

	gotBody, ok := provider.GetRawBody()
	if !ok {
		t.Fatal("expected GetRawBody to fall back to request body caching")
	}
	if string(gotBody) != body {
		t.Fatalf("unexpected raw body: got %q want %q", gotBody, body)
	}

	gotCanonical, ok := common.GetCanonicalRequestBody(ctx)
	if !ok {
		t.Fatal("expected canonical request body cache to be populated")
	}
	if string(gotCanonical) != body {
		t.Fatalf("unexpected canonical request body: got %q want %q", gotCanonical, body)
	}
}

type countingReadCloser struct {
	reader     *strings.Reader
	readCalls  int
	closeCalls int
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	r.readCalls++
	return r.reader.Read(p)
}

func (r *countingReadCloser) Close() error {
	r.closeCalls++
	return nil
}

func TestGetRawBodyPrefersCanonicalCacheForDecodedRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	decodedBody := []byte(`{"prompt":"decoded body"}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/recraftAI/v1/styles", strings.NewReader(`{"prompt":"wire body"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	common.SetDecodedRequestState(ctx, []byte("compressed"), decodedBody, &requestbody.DecodeMeta{
		ContentEncodings: []string{"zstd"},
		WireBytes:        len("compressed"),
		DecodedBytes:     len(decodedBody),
	})

	tracker := &countingReadCloser{reader: strings.NewReader(`{"prompt":"mutated request body"}`)}
	ctx.Request.Body = tracker

	provider := &BaseProvider{Context: ctx}
	gotBody, ok := provider.GetRawBody()
	if !ok {
		t.Fatal("expected GetRawBody to read from canonical cache")
	}
	if string(gotBody) != string(decodedBody) {
		t.Fatalf("unexpected raw body: got %q want %q", gotBody, decodedBody)
	}
	if tracker.readCalls != 0 || tracker.closeCalls != 0 {
		t.Fatalf("expected canonical cache hit to avoid rereading request body, got reads=%d closes=%d", tracker.readCalls, tracker.closeCalls)
	}
}

func TestProtectedModelHeaderReason(t *testing.T) {
	cases := []struct {
		header string
		reason ModelHeaderProtectionReason
	}{
		{header: "Authorization", reason: ModelHeaderProtectionCredentialRouting},
		{header: "api-key", reason: ModelHeaderProtectionCredentialRouting},
		{header: "X-API-Key", reason: ModelHeaderProtectionCredentialRouting},
		{header: "x-goog-api-key", reason: ModelHeaderProtectionCredentialRouting},
		{header: "Host", reason: ModelHeaderProtectionCredentialRouting},
		{header: "Connection", reason: ModelHeaderProtectionHopByHop},
		{header: "Sec-WebSocket-Protocol", reason: ModelHeaderProtectionWebSocket},
	}

	for _, tc := range cases {
		reason, blocked := ProtectedModelHeaderReason(tc.header)
		if !blocked || reason != tc.reason {
			t.Fatalf("expected %q to be blocked as %q, got blocked=%v reason=%q", tc.header, tc.reason, blocked, reason)
		}
	}
}

func TestCommonRequestHeadersFiltersProtectedModelHeaders(t *testing.T) {
	originalNext := channelConfigLogNext
	channelConfigLogNext = map[string]time.Time{}
	t.Cleanup(func() {
		channelConfigLogNext = originalNext
	})

	modelHeaders := `{"Authorization":"Bearer channel-evil","api-key":"evil-api-key","X-API-Key":"evil-x-api-key","x-goog-api-key":"evil-google","Connection":"keep-alive","Transfer-Encoding":"chunked","Sec-WebSocket-Protocol":"evil","X-From-Channel":"1"}`
	provider := &BaseProvider{Channel: &model.Channel{Id: 424242, ModelHeaders: &modelHeaders}}

	headers := map[string]string{}
	provider.CommonRequestHeaders(headers)

	for _, blocked := range []string{"Authorization", "api-key", "X-API-Key", "x-goog-api-key", "Connection", "Transfer-Encoding", "Sec-WebSocket-Protocol"} {
		if got, ok := headers[blocked]; ok {
			t.Fatalf("expected model_headers %q to be filtered out, got %q", blocked, got)
		}
	}
	if headers["X-From-Channel"] != "1" {
		t.Fatalf("expected normal model header to be merged, got %q", headers["X-From-Channel"])
	}
	afterEntries, _ := logger.GetLatestLogs(500)
	found := false
	for _, entry := range afterEntries {
		if strings.Contains(entry.Message, "channel_id=424242") && strings.Contains(entry.Message, "field=model_headers") && strings.Contains(entry.Message, "ignored_protected_key") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected protected model_headers skip to emit a rate-limited warning")
	}
}

func TestLogChannelConfigParseErrorRedactsAndRateLimits(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	originalNext := channelConfigLogNext
	originalNow := channelConfigLogNow
	baseTime := time.Unix(1000, 0)
	channelConfigLogNext = map[string]time.Time{}
	channelConfigLogNow = func() time.Time { return baseTime }
	t.Cleanup(func() {
		logger.Logger = originalLogger
		channelConfigLogNext = originalNext
		channelConfigLogNow = originalNow
	})

	provider := "unit-provider-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	ctx := context.WithValue(context.Background(), logger.RequestIdKey, "req-provider-config-log")
	err := errors.New("bad Authorization: Bearer secret-token token=query-secret https://provider.example/v1?token=url-secret session session-secret sk-testSECRET123")

	LogChannelConfigParseError(ctx, provider, &model.Channel{Id: 11, Type: config.ChannelTypeGemini}, "api_version", err)
	LogChannelConfigParseError(ctx, provider, &model.Channel{Id: 11, Type: config.ChannelTypeGemini}, "api_version", err)
	LogChannelConfigParseError(ctx, provider, &model.Channel{Id: 12, Type: config.ChannelTypeGemini}, "api_version", err)
	channelConfigLogNow = func() time.Time { return baseTime.Add(channelConfigLogInterval + time.Second) }
	LogChannelConfigParseError(ctx, provider, &model.Channel{Id: 11, Type: config.ChannelTypeGemini}, "api_version", err)

	entries, _ := logger.GetLatestLogs(500)
	matches := make([]string, 0, 3)
	for _, entry := range entries {
		if strings.Contains(entry.Message, provider) {
			matches = append(matches, entry.Message)
		}
	}
	if len(matches) != 3 {
		t.Fatalf("expected same provider/channel/field to log once per interval and different channel separately, got %d entries: %#v", len(matches), matches)
	}
	for _, message := range matches {
		if !strings.Contains(message, "provider="+provider) || !strings.Contains(message, "channel_id=") || !strings.Contains(message, "field=api_version") {
			t.Fatalf("expected provider/channel/field in log message, got %q", message)
		}
		for _, forbidden := range []string{"secret-token", "query-secret", "provider.example", "url-secret", "session-secret", "sk-testSECRET123"} {
			if strings.Contains(message, forbidden) {
				t.Fatalf("expected provider config log to redact %q, got %q", forbidden, message)
			}
		}
	}
}
