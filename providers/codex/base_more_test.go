package codex

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"one-api/common/logger"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"

	"go.uber.org/zap"
)

type codexErrReadCloser struct{}

func (codexErrReadCloser) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func (codexErrReadCloser) Close() error {
	return nil
}

func TestCodexBaseHelperFunctionsAndHeaderFallbacks(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	if DefaultUserAgent() != defaultUserAgent {
		t.Fatalf("expected DefaultUserAgent to expose codex default, got %q", DefaultUserAgent())
	}
	if got := normalizeCodexModelName(" gpt-5-turbo "); got != "gpt-5" {
		t.Fatalf("expected gpt-5-* models to normalize to gpt-5, got %q", got)
	}
	if got := normalizeCodexModelName("gpt-5-codex"); got != "gpt-5-codex" {
		t.Fatalf("expected gpt-5-codex to remain stable, got %q", got)
	}

	if prepared := prepareChannelForProvider(nil); prepared != nil {
		t.Fatalf("expected nil channel preparation to stay nil, got %#v", prepared)
	}
	proxyValue := "http://proxy.example"
	sourceChannel := &model.Channel{Id: 42, Proxy: &proxyValue}
	preparedChannel := prepareChannelForProvider(sourceChannel)
	if preparedChannel == nil || preparedChannel == sourceChannel || preparedChannel.Proxy == sourceChannel.Proxy {
		t.Fatalf("expected prepareChannelForProvider to clone channel and proxy pointer, got prepared=%#v source=%#v", preparedChannel, sourceChannel)
	}
	if got := channelProxyValue(preparedChannel); got != "http://proxy.example" {
		t.Fatalf("expected prepared channel proxy to be preserved, got %q", got)
	}
	if got := channelProxyValue(nil); got != "" {
		t.Fatalf("expected nil channel proxy lookup to be empty, got %q", got)
	}

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, `{"user_agent":"legacy-codex-ua"}`, map[string]string{
		"Version":             "2026-03-28",
		"OpenAI-Beta":         "responses=v1",
		"X-Session-Id":        "session-123",
		"X-Codex-Turn-State":  "turn-state",
		"X-Client-Request-Id": "request-123",
		"Accept":              "application/x-custom",
		"Content-Type":        "application/x-json",
	})
	provider.Channel.ModelHeaders = stringPtr(`{"X-From-Channel":"1","User-Agent":"channel-ua"}`)

	headers := newCodexHeaderBag()
	provider.filterAndPassthroughClientHeaders(headers)
	if got := headers.Get("Version"); got != "2026-03-28" {
		t.Fatalf("expected allowed client headers to pass through, got %q", got)
	}
	provider.applyCommonRequestHeaders(headers)
	if got := headers.Get("Content-Type"); got != "application/x-json" {
		t.Fatalf("expected request content type to be preserved, got %q", got)
	}
	if got := headers.Get("X-From-Channel"); got != "1" {
		t.Fatalf("expected channel model headers to merge, got %q", got)
	}

	options := provider.getChannelOptions()
	if options == nil || options.UserAgent != "legacy-codex-ua" {
		t.Fatalf("expected codex channel options to decode once, got %+v", options)
	}
	if provider.getChannelOptions() != options {
		t.Fatal("expected codex channel options to be cached")
	}
	if got := provider.getLegacyUserAgentOverride(); got != "legacy-codex-ua" {
		t.Fatalf("expected legacy user agent override, got %q", got)
	}

	provider.channelOptions = &codexChannelOptions{UserAgent: "cached"}
	provider.channelOptionsLoaded = true
	provider.syncRuntimeChannel(&model.Channel{Id: 43, Key: key})
	if provider.Channel == nil || provider.Channel.Id != 43 || provider.channelOptions != nil || provider.channelOptionsLoaded {
		t.Fatalf("expected syncRuntimeChannel to reset cached options and swap channel, got provider=%+v", provider)
	}
	provider.syncRuntimeKey("updated-key")
	if provider.Channel.Key != "updated-key" {
		t.Fatalf("expected syncRuntimeKey to update runtime channel key, got %q", provider.Channel.Key)
	}

	fallbackProvider := newTestCodexProviderWithContext(t, key, "", map[string]string{"Accept": "application/json"})
	fallbackProvider.Credentials = nil
	fallbackHeaders := fallbackProvider.GetRequestHeaders()
	if _, ok := fallbackHeaders["Authorization"]; ok {
		t.Fatalf("expected GetRequestHeaders fallback not to include authorization on token failure, got %+v", fallbackHeaders)
	}
	if got := getHeaderValue(fallbackHeaders, "Accept"); got != "application/json" {
		t.Fatalf("expected fallback headers to preserve common request headers, got %q", got)
	}
	replaceHeader(fallbackHeaders, "X-Test", "1")
	if !hasHeader(fallbackHeaders, "X-Test") {
		t.Fatalf("expected replaceHeader to normalize header map entries, got %+v", fallbackHeaders)
	}

	if errWithCode := provider.handleTokenError(errors.New("token expired")); errWithCode == nil || errWithCode.StatusCode != http.StatusUnauthorized || errWithCode.Code != "codex_token_error" {
		t.Fatalf("expected handleTokenError to wrap unauthorized token failures, got %+v", errWithCode)
	}
}

func TestCodexRequestErrorHandleParsesAndScrubsResponses(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	handler := RequestErrorHandle("secret-token")

	rateLimitResp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body: io.NopCloser(strings.NewReader(`{
			"error": {
				"message": "secret-token exhausted",
				"type": "rate_limit_error",
				"code": "rate_limit",
				"resets_in_seconds": 1
			}
		}`)),
	}
	if err := handler(rateLimitResp); err == nil || err.Code != "rate_limit" || strings.Contains(err.Message, "secret-token") || !strings.Contains(err.Message, "xxxxx") {
		t.Fatalf("expected codex rate limit errors to parse and scrub access tokens, got %+v", err)
	}

	standardResp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"message":"secret-token bad request","type":"invalid_request_error","code":"bad_request"}`)),
	}
	if err := handler(standardResp); err == nil || err.Type != "invalid_request_error" || strings.Contains(err.Message, "secret-token") {
		t.Fatalf("expected standard openai errors to parse and scrub access tokens, got %+v", err)
	}

	if err := handler(&http.Response{StatusCode: http.StatusInternalServerError, Body: codexErrReadCloser{}}); err != nil {
		t.Fatalf("expected body read failures to return nil, got %+v", err)
	}
	if err := handler(&http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"message":""}`))}); err != nil {
		t.Fatalf("expected empty fallback error payloads to be ignored, got %+v", err)
	}

	parseCodexConfig(nil)
	parseCodexConfig(&CodexProvider{})
	plain := &CodexProvider{OpenAIProvider: openai.OpenAIProvider{BaseProvider: base.BaseProvider{Channel: &model.Channel{Key: `{"access_token":"plain-token"}`}}}}
	parseCodexConfig(plain)
	if plain.Credentials == nil || plain.Credentials.AccessToken != "plain-token" {
		t.Fatalf("expected parseCodexConfig to load credentials from channel key, got %+v", plain.Credentials)
	}
}

func TestCodexBaseAdditionalOptionAndHeaderBranches(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	if got := (&CodexProvider{}).getChannelOptions(); got != nil {
		t.Fatalf("expected nil-ish provider channel options lookup to stay nil, got %+v", got)
	}
	if ctx := (&CodexProvider{}).channelLogContext(); ctx == nil {
		t.Fatal("expected channelLogContext fallback to return a background context")
	}

	emptyOtherProvider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	if got := emptyOtherProvider.getChannelOptions(); got != nil {
		t.Fatalf("expected empty other config not to produce channel options, got %+v", got)
	}

	invalidOtherProvider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"user_agent":123}`, nil)
	if got := invalidOtherProvider.getChannelOptions(); got != nil {
		t.Fatalf("expected invalid channel options payload not to decode, got %+v", got)
	}
	if got := invalidOtherProvider.getLegacyUserAgentOverride(); got != "" {
		t.Fatalf("expected invalid decoded options not to expose a user agent override, got %q", got)
	}

	applyCommonRequestHeadersProvider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{"Accept": "application/json"})
	applyCommonRequestHeadersProvider.applyCommonRequestHeaders(nil)

	headerBagProvider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	headers, err := headerBagProvider.getRequestHeadersInternal()
	if err != nil {
		t.Fatalf("expected internal request header builder to succeed, got %v", err)
	}
	if got := getHeaderValue(headers, "Authorization"); got != "Bearer access-token" {
		t.Fatalf("expected internal request headers to include authorization, got %q", got)
	}

	errorProvider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	errorProvider.Credentials = nil
	if headers, err := errorProvider.getRequestHeadersInternal(); err == nil || headers != nil {
		t.Fatalf("expected internal request header builder to surface token failures, headers=%+v err=%v", headers, err)
	}
}
