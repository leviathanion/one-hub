package requester

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"testing"

	"one-api/common/providerresponse"
	"one-api/common/utils"
	"one-api/types"
)

func TestReadProviderBodyBounded(t *testing.T) {
	body, err := readProviderBodyBounded(strings.NewReader("12345678"), 8)
	if err != nil || string(body) != "12345678" {
		t.Fatalf("body at limit failed: body=%q err=%v", body, err)
	}
	if _, err := readProviderBodyBounded(strings.NewReader("123456789"), 8); !errors.Is(err, errProviderRawJSONBodyTooLarge) {
		t.Fatalf("oversized body error=%v", err)
	}
}

func TestNewRequestWithContextPreservesProxyContext(t *testing.T) {
	requester := NewHTTPRequester("http://proxy.example:8080", nil)
	type contextKey string
	ctx := context.WithValue(context.Background(), contextKey("request-id"), "req-1")

	req, err := requester.NewRequest(http.MethodGet, "https://example.com", requester.WithContext(ctx))
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}
	if got := req.Context().Value(contextKey("request-id")); got != "req-1" {
		t.Fatalf("expected caller context value to be preserved, got %v", got)
	}
	if got := req.Context().Value(utils.ProxyHTTPAddrKey); got != "http://proxy.example:8080" {
		t.Fatalf("expected proxy context value to be preserved, got %v", got)
	}
}

func TestHandleErrorRespPreservesOnlySafeOpenAIClientErrors(t *testing.T) {
	parseOpenAIError := func(resp *http.Response) *types.OpenAIError {
		var envelope types.OpenAIErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			return nil
		}
		return &envelope.Error
	}

	safeBody := `{"error":{"message":"invalid field","type":"invalid_request_error","code":"invalid_value","future":{"hint":true}}}`
	safe := HandleErrorResp(&http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(safeBody)),
		Header: http.Header{
			"X-Request-Id": []string{"req-upstream"},
			"Retry-After":  []string{"3"},
			"Set-Cookie":   []string{"secret=1"},
		},
	}, parseOpenAIError, true, true)
	if string(safe.RawBody) != safeBody {
		t.Fatalf("expected safe client error body to be preserved, got %q", safe.RawBody)
	}
	if safe.ResponseHeaders.Get("X-Request-Id") != "req-upstream" || safe.ResponseHeaders.Get("Retry-After") != "3" || safe.ResponseHeaders.Get("Set-Cookie") != "" {
		t.Fatalf("expected only safe provider response headers, got %#v", safe.ResponseHeaders)
	}

	safeRateLimitBody := `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded","retry_after":1}}`
	safeRateLimit := HandleErrorResp(&http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(safeRateLimitBody)),
	}, parseOpenAIError, true, true)
	if string(safeRateLimit.RawBody) != safeRateLimitBody {
		t.Fatalf("ordinary same-dialect rate limit should preserve its envelope, got %q", safeRateLimit.RawBody)
	}

	safeServerBody := `{"error":{"message":"temporary upstream failure","type":"server_error","code":"server_error"},"future_request_id":"req_future"}`
	safeServer := HandleErrorResp(&http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       io.NopCloser(strings.NewReader(safeServerBody)),
	}, parseOpenAIError, true, true)
	if string(safeServer.RawBody) != safeServerBody {
		t.Fatalf("same-dialect server errors should preserve future envelope fields, got %q", safeServer.RawBody)
	}

	credentialRequest, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	credentialRequest.Header.Set("Authorization", "Bearer sk-proxy-owned-secret")
	leakingBody := `{"error":{"message":"invalid field","type":"invalid_request_error"},"future":{"debug":"sk-proxy-owned-secret"}}`
	leaking := HandleErrorResp(&http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(leakingBody)),
		Request:    credentialRequest,
	}, parseOpenAIError, true, true)
	if string(leaking.RawBody) != leakingBody {
		t.Fatalf("proxy-owned credential in an unknown field must not be replayed: %+v", leaking)
	}

	untrusted := HandleErrorResp(&http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(safeBody)),
	}, parseOpenAIError, true, false)
	if len(untrusted.RawBody) != 0 {
		t.Fatalf("same shape without adapter authorization must not be replayed, got %s", untrusted.RawBody)
	}

	crossDialect := HandleErrorResp(&http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"message":"bad input"}`)),
	}, func(*http.Response) *types.OpenAIError {
		return &types.OpenAIError{Message: "bad input", Type: "provider_error"}
	}, true, true)
	if len(crossDialect.RawBody) != 0 {
		t.Fatalf("non-OpenAI envelope must not be replayed, got %s", crossDialect.RawBody)
	}

	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "credential", status: http.StatusUnauthorized, body: `{"error":{"message":"invalid api key","type":"authentication_error","code":"invalid_api_key"}}`},
		{name: "account quota", status: http.StatusTooManyRequests, body: `{"error":{"message":"account quota exhausted","type":"insufficient_quota","code":"insufficient_quota"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := HandleErrorResp(&http.Response{
				StatusCode: test.status,
				Body:       io.NopCloser(strings.NewReader(test.body)),
			}, parseOpenAIError, true, true)
			if string(got.RawBody) != test.body {
				t.Fatalf("原始错误丢失: %s", got.RawBody)
			}
		})
	}

	for _, body := range []string{
		`{"error":{"message":"invalid field","type":"invalid_request_error","future":{"account_id":"acct-secret"}}}`,
		`{"error":{"message":"invalid field","type":"invalid_request_error","details":[{"subscriptionId":"sub-secret","TenantId":"tenant-secret"}]}}`,
		`{"error":{"message":"first","message":"account acct_secret","type":"invalid_request_error"}}`,
	} {
		got := HandleErrorResp(&http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(body)),
		}, parseOpenAIError, true, true)
		if string(got.RawBody) != body {
			t.Fatalf("原始错误被修改: %s", got.RawBody)
		}
	}
}

func TestHandleErrorRespDoesNotReclassifyClientFieldDiagnosticsAsProviderAccountFailure(t *testing.T) {
	parseOpenAIError := func(resp *http.Response) *types.OpenAIError {
		var envelope types.OpenAIErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			return nil
		}
		return &envelope.Error
	}
	body := `{"error":{"message":"api_key is not an accepted request field","type":"invalid_request_error","code":"invalid_value","param":"api_key","details":{"project_id":"client-supplied"}}}`

	got := HandleErrorResp(&http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(body)),
	}, parseOpenAIError, false, true)

	if got.Code != "invalid_value" || got.Type != "invalid_request_error" || got.ProviderAuthRejected || got.ProviderQuotaExhausted {
		t.Fatalf("client field diagnostic was reclassified as a provider account failure: %+v", got)
	}
	if string(got.RawBody) != body {
		t.Fatalf("错误字段未按角色脱敏: %s", got.RawBody)
	}
}

func TestHandleErrorRespBoundsProviderErrorBody(t *testing.T) {
	source := strings.NewReader(strings.Repeat("x", int(maxProviderErrorBodyBytes*2)))
	body := &trackingReadCloser{reader: source}
	mapperCalled := false
	got := HandleErrorResp(&http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       body,
	}, func(*http.Response) *types.OpenAIError {
		mapperCalled = true
		return &types.OpenAIError{Message: "unexpected"}
	}, true, true)

	if mapperCalled {
		t.Fatal("oversized provider error body must not be buffered and passed to an adapter")
	}
	if !body.closed || source.Len() == 0 {
		t.Fatalf("expected the response to close after a bounded read, closed=%v remaining=%d", body.closed, source.Len())
	}
	if len(got.RawBody) != 0 || !strings.Contains(got.Message, "bad response status code") {
		t.Fatalf("expected a generic non-replayable provider error, got %+v", got)
	}
}

func TestHandleErrorRespPreservesExactWirePaymentRequiredStatus(t *testing.T) {
	body := `{"error":{"message":"credit exhausted","type":"insufficient_quota","code":"insufficient_quota"}}`
	mapper := func(resp *http.Response) *types.OpenAIError {
		var envelope types.OpenAIErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode provider error: %v", err)
		}
		return &envelope.Error
	}
	exact := HandleErrorResp(&http.Response{
		StatusCode: http.StatusPaymentRequired,
		Body:       io.NopCloser(strings.NewReader(body)),
	}, mapper, true, true)
	if exact.StatusCode != http.StatusPaymentRequired || !exact.ProviderQuotaExhausted {
		t.Fatalf("expected exact-wire 402 semantics, got %+v", exact)
	}
	if exact.Code != "insufficient_quota" || string(exact.RawBody) != body {
		t.Fatalf("exact-wire status must not expose the shared account body: %+v", exact)
	}

	mapped := HandleErrorResp(&http.Response{
		StatusCode: http.StatusPaymentRequired,
		Body:       io.NopCloser(strings.NewReader(body)),
	}, mapper, true, false)
	if mapped.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected cross-protocol quota mapping to 429, got %+v", mapped)
	}
}

func TestHandleErrorRespSanitizesStatusOnlyAccountFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusProxyAuthRequired, http.StatusPaymentRequired} {
		got := HandleErrorResp(&http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader("provider account detail")),
			Header:     make(http.Header),
		}, nil, false, true)
		if got.StatusCode != status || len(got.RawBody) != 0 {
			t.Fatalf("status-only account failure was not safely normalized: %+v", got)
		}
	}
}

func TestHandleErrorRespClassifiesOversizedBodyByStatus(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		wantQuota bool
		wantRate  bool
	}{
		{name: "payment required", status: http.StatusPaymentRequired, wantQuota: true},
		{name: "rate limited", status: http.StatusTooManyRequests, wantRate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := HandleErrorResp(&http.Response{
				StatusCode: test.status,
				Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", int(maxProviderErrorBodyBytes+1)))),
			}, func(*http.Response) *types.OpenAIError {
				t.Fatal("oversized provider error body must not reach the adapter")
				return nil
			}, true, true)

			if got.ProviderQuotaExhausted != test.wantQuota || got.ProviderRateLimited != test.wantRate {
				t.Fatalf("unexpected status classification: %+v", got)
			}
		})
	}
}

func TestProviderErrorSensitiveAccountKeysUseSeparatorInsensitiveExactMatch(t *testing.T) {
	for _, key := range []string{
		"subscription_id", "subscription-id", "subscriptionId", "SubscriptionId",
		"tenant_id", "tenant-id", "tenantId", "TenantId",
		"account_id", "account-id", "accountId", "AccountId",
		"organizationId", "projectId", "billingAccountId", "clientSecret", "accessToken", "apiKey",
	} {
		t.Run(key, func(t *testing.T) {
			body := []byte(`{"error":{"message":"safe","type":"invalid_request_error","details":[{"` + key + `":"provider-secret"}]}}`)
			_, changed := providerresponse.SanitizeErrorPayload(body)
			if !changed {
				t.Fatalf("sensitive key %q was not classified", key)
			}
		})
	}

	for _, key := range []string{"subscriptionIdentity", "tenantIdentifier", "accountingId", "projectIdea", "clientSecretsEnabled"} {
		t.Run("near miss "+key, func(t *testing.T) {
			body := []byte(`{"error":{"message":"safe","type":"invalid_request_error","details":{"` + key + `":"ordinary"}}}`)
			_, changed := providerresponse.SanitizeErrorPayload(body)
			if changed {
				t.Fatalf("ordinary near-match key %q was rejected", key)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackingReadCloser struct {
	reader io.Reader
	closed bool
}

func (b *trackingReadCloser) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *trackingReadCloser) Close() error {
	b.closed = true
	return nil
}

func TestSendRequestOutputRespClosesOriginalBodyOnDecodeFailure(t *testing.T) {
	originalHTTPClient := HTTPClient
	body := &trackingReadCloser{reader: strings.NewReader(`{"broken"`)}
	HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       body,
			Header: http.Header{
				"X-Request-Id": []string{"req-malformed"},
				"Set-Cookie":   []string{"provider_session=secret"},
			},
			Request: req,
		}, nil
	})}
	t.Cleanup(func() {
		HTTPClient = originalHTTPClient
	})

	requester := NewHTTPRequester("", nil)
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var decoded struct {
		OK bool `json:"ok"`
	}
	resp, apiErr := requester.SendRequest(req, &decoded, true)
	if apiErr == nil || apiErr.Code != "decode_response_failed" {
		t.Fatalf("expected decode failure, resp=%v err=%+v", resp, apiErr)
	}
	if !apiErr.UpstreamAccepted {
		t.Fatalf("expected a malformed 2xx response to retain provider acceptance evidence, got %+v", apiErr)
	}
	if apiErr.ResponseHeaders.Get("X-Request-Id") != "req-malformed" || apiErr.ResponseHeaders.Get("Set-Cookie") != "" {
		t.Fatalf("expected malformed 2xx error headers to use the safe provider policy, got %#v", apiErr.ResponseHeaders)
	}
	if !body.closed {
		t.Fatal("expected original response body to be closed on decode failure")
	}
}

func TestSendRequestMarksAmbiguousProviderExecution(t *testing.T) {
	originalHTTPClient := HTTPClient
	t.Cleanup(func() { HTTPClient = originalHTTPClient })
	requester := NewHTTPRequester("", nil)

	t.Run("transport failure before write", func(t *testing.T) {
		HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, io.ErrUnexpectedEOF
		})}
		req, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", strings.NewReader(`{}`))
		_, apiErr := requester.SendRequest(req, &map[string]any{}, false)
		if apiErr == nil || !apiErr.UpstreamNotAttempted || apiErr.UpstreamAmbiguous || apiErr.UpstreamAccepted {
			t.Fatalf("expected not-attempted transport evidence, got %+v", apiErr)
		}
	})

	t.Run("transport disconnect after write", func(t *testing.T) {
		HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			trace := httptrace.ContextClientTrace(req.Context())
			trace.WroteRequest(httptrace.WroteRequestInfo{})
			return nil, io.ErrUnexpectedEOF
		})}
		req, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", strings.NewReader(`{}`))
		_, apiErr := requester.SendRequest(req, &map[string]any{}, false)
		if apiErr == nil || !apiErr.UpstreamAmbiguous || apiErr.UpstreamNotAttempted || apiErr.UpstreamAccepted {
			t.Fatalf("expected ambiguous transport evidence, got %+v", apiErr)
		}
	})

	t.Run("provider 5xx", func(t *testing.T) {
		HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"upstream failed"}}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})}
		req, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", strings.NewReader(`{}`))
		_, apiErr := requester.SendRequest(req, &map[string]any{}, false)
		if apiErr == nil || !apiErr.UpstreamAmbiguous {
			t.Fatalf("expected 5xx execution ambiguity, got %+v", apiErr)
		}
	})
}

func TestHTTPTransportErrorNeverExposesUpstreamURL(t *testing.T) {
	for _, test := range []struct {
		name         string
		op           string
		wroteRequest bool
	}{
		{name: "get before write", op: http.MethodGet},
		{name: "delete after write", op: http.MethodDelete, wroteRequest: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			apiErr := httpTransportError(&url.Error{
				Op:  test.op,
				URL: "https://provider.internal/v1/responses/resp_secret?cursor=private",
				Err: context.DeadlineExceeded,
			}, test.wroteRequest)
			if apiErr.Message != "请求上游地址失败" || strings.Contains(apiErr.Message, "provider.internal") || strings.Contains(apiErr.Message, "resp_secret") {
				t.Fatalf("transport error exposed provider URL: %+v", apiErr)
			}
			if test.wroteRequest != apiErr.UpstreamAmbiguous || test.wroteRequest == apiErr.UpstreamNotAttempted {
				t.Fatalf("transport disposition changed: wrote=%t err=%+v", test.wroteRequest, apiErr)
			}
		})
	}
}

func TestRawRequestTransportDispositionUsesWriteEvidence(t *testing.T) {
	originalHTTPClient := HTTPClient
	t.Cleanup(func() { HTTPClient = originalHTTPClient })
	requester := NewHTTPRequester("", nil)
	senders := map[string]func(*http.Request) (*http.Response, *types.OpenAIErrorWithStatusCode){
		"checked":     requester.SendRequestRaw,
		"no redirect": requester.SendRequestRawNoRedirect,
	}
	for name, send := range senders {
		t.Run(name+" before write", func(t *testing.T) {
			HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, io.ErrUnexpectedEOF
			})}
			req, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", strings.NewReader(`{}`))
			_, apiErr := send(req)
			if apiErr == nil || !apiErr.UpstreamNotAttempted || apiErr.UpstreamAmbiguous {
				t.Fatalf("expected not-attempted transport evidence, got %+v", apiErr)
			}
		})
		t.Run(name+" after write", func(t *testing.T) {
			HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				trace := httptrace.ContextClientTrace(req.Context())
				trace.WroteRequest(httptrace.WroteRequestInfo{})
				return nil, io.ErrUnexpectedEOF
			})}
			req, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", strings.NewReader(`{}`))
			_, apiErr := send(req)
			if apiErr == nil || !apiErr.UpstreamAmbiguous || apiErr.UpstreamNotAttempted {
				t.Fatalf("expected ambiguous transport evidence, got %+v", apiErr)
			}
		})
	}
}

func TestSendRequestDoesNotCaptureRawJSONWithoutExplicitOptIn(t *testing.T) {
	originalHTTPClient := HTTPClient
	HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl_1","choices":[]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	t.Cleanup(func() { HTTPClient = originalHTTPClient })

	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	response := &types.ChatCompletionResponse{}
	if _, apiErr := NewHTTPRequester("", nil).SendRequest(req, response, false); apiErr != nil {
		t.Fatalf("decode response: %v", apiErr)
	}
	if response.ProviderRawJSON() != nil {
		t.Fatal("raw JSON was captured without replay opt-in")
	}
}

func TestSendRequestRawCapturePreservesWireAndReportsCredential(t *testing.T) {
	originalHTTPClient := HTTPClient
	HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl_1","debug":"credential provider-secret-123","choices":[]}`)),
			Header: http.Header{
				"Content-Type":   {"application/json"},
				"Content-Length": {"79"},
				"Etag":           {`"provider-wire"`},
			},
			Request: req,
		}, nil
	})}
	t.Cleanup(func() { HTTPClient = originalHTTPClient })

	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	req.Header.Set("Authorization", "Bearer provider-secret-123")
	response := &types.ChatCompletionResponse{}
	response.EnableProviderRawJSONCapture()
	requester := NewHTTPRequester("", nil)
	var credentials []string
	requester.ObserveRequest = func(req *http.Request) { credentials = providerresponse.RequestCredentials(req) }
	httpResponse, apiErr := requester.SendRequest(req, response, false)
	if apiErr != nil {
		t.Fatalf("decode response: %v", apiErr)
	}
	raw := string(response.ProviderRawJSON())
	if !strings.Contains(raw, "provider-secret-123") || len(credentials) != 2 || credentials[1] != "provider-secret-123" {
		t.Fatalf("transport changed raw body or did not report credentials: %s", raw)
	}
	if httpResponse.Header.Get("Content-Length") != "79" || httpResponse.Header.Get("Etag") != `"provider-wire"` {
		t.Fatalf("transport changed representation headers: %v", httpResponse.Header)
	}
}

func TestSendRequestPreservingRedirectRequiresExplicitOptIn(t *testing.T) {
	originalHTTPClient := HTTPClient
	var responseBodies []*trackingReadCloser
	redirectTargetCalls := 0
	HTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/v1/responses/redirected" {
				redirectTargetCalls++
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
					Header:     http.Header{"Content-Type": {"application/json"}},
					Request:    req,
				}, nil
			}
			body := &trackingReadCloser{reader: strings.NewReader("redirect body\n")}
			responseBodies = append(responseBodies, body)
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Body:       body,
				Header: http.Header{
					"Content-Type":        {"text/plain; charset=utf-8"},
					"Location":            {"https://api.openai.com/v1/responses/redirected"},
					"X-Request-Id":        {"req-redirect"},
					"Set-Cookie":          {"provider_session=secret"},
					"Openai-Organization": {"org-secret"},
				},
				Request: req,
			}, nil
		}),
		CheckRedirect: providerRedirectPolicy,
	}
	t.Cleanup(func() { HTTPClient = originalHTTPClient })

	httpRequester := NewHTTPRequester("", nil)
	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", strings.NewReader(`{}`))
	resp, apiErr := httpRequester.SendRequestPreservingRedirect(req, &map[string]any{}, false)
	if resp != nil || apiErr == nil || !apiErr.ReplayRawResponse || apiErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("expected authorized raw redirect response, resp=%v err=%+v", resp, apiErr)
	}
	if string(apiErr.RawBody) != "redirect body\n" {
		t.Fatalf("redirect body changed: %q", apiErr.RawBody)
	}
	if len(responseBodies) != 1 || !responseBodies[0].closed {
		t.Fatal("preserved redirect response body was not closed")
	}
	for name, want := range map[string]string{
		"Content-Type": "text/plain; charset=utf-8",
		"Location":     "https://api.openai.com/v1/responses/redirected",
		"X-Request-Id": "req-redirect",
	} {
		if got := apiErr.ResponseHeaders.Get(name); got != want {
			t.Fatalf("%s=%q, want %q in %#v", name, got, want, apiErr.ResponseHeaders)
		}
	}
	for _, name := range []string{"Set-Cookie", "Openai-Organization"} {
		if got := apiErr.ResponseHeaders.Get(name); got != "" {
			t.Fatalf("unsafe redirect header %s=%q", name, got)
		}
	}

	req, _ = http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", strings.NewReader(`{}`))
	var ordinaryResponse map[string]any
	_, ordinaryErr := httpRequester.SendRequest(req, &ordinaryResponse, false)
	if ordinaryErr == nil || ordinaryErr.StatusCode != http.StatusTemporaryRedirect || redirectTargetCalls != 0 {
		t.Fatalf("ordinary structured requester replayed a redirect: response=%#v calls=%d err=%+v", ordinaryResponse, redirectTargetCalls, ordinaryErr)
	}
	if len(responseBodies) != 2 || !responseBodies[1].closed {
		t.Fatal("ordinary redirect response body was not closed")
	}

	req, _ = http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", strings.NewReader(`{}`))
	rawResponse, rawErr := httpRequester.SendRequestRaw(req)
	if rawErr == nil || rawErr.StatusCode != http.StatusTemporaryRedirect || rawResponse != nil || redirectTargetCalls != 0 {
		t.Fatalf("ordinary raw requester replayed a redirect: response=%#v calls=%d err=%+v", rawResponse, redirectTargetCalls, rawErr)
	}
	if len(responseBodies) != 3 || !responseBodies[2].closed {
		t.Fatal("raw redirect response body was not closed")
	}

	empty := preservedRedirectResponse(&http.Response{
		StatusCode: http.StatusFound,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{"Location": {"/empty"}},
	}, providerresponse.OperationUnknown)
	if empty == nil || !empty.ReplayRawResponse || empty.StatusCode != http.StatusFound || len(empty.RawBody) != 0 || empty.ResponseHeaders.Get("Location") != "/empty" {
		t.Fatalf("empty redirect response was not preserved: %+v", empty)
	}
}

func TestDecodeFailureFiltersActualCredentialFromErrorHeader(t *testing.T) {
	old := HTTPClient
	t.Cleanup(func() { HTTPClient = old })
	HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Request: req, Body: io.NopCloser(strings.NewReader(`{"broken"`)), Header: http.Header{"X-Request-Id": {"temporary-provider-secret"}, "Retry-After": {"3"}}}, nil
	})}
	req, _ := http.NewRequest(http.MethodGet, "https://provider.example", nil)
	req.Header.Set("Authorization", "Bearer temporary-provider-secret")
	var result map[string]any
	_, apiErr := NewHTTPRequester("", nil).SendRequest(req, &result, false)
	if apiErr == nil || !apiErr.UpstreamAccepted || apiErr.ResponseHeaders.Get("X-Request-Id") != "" || apiErr.ResponseHeaders.Get("Retry-After") != "3" {
		t.Fatalf("错误响应头或执行事实错误: %+v", apiErr)
	}
}
