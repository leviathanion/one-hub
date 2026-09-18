package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func (p *OpenAIProvider) CompactResponsesForTest(request *types.OpenAIResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "marshal_request_failed", http.StatusInternalServerError)
	}
	if p != nil && p.Context != nil {
		if cached, cacheErr := common.CacheRequestBody(p.Context); cacheErr == nil && len(cached) > 0 {
			raw = cached
		}
	}
	envelope, err := commonresponses.ParseRawEnvelope(raw)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_request_error", http.StatusBadRequest)
	}
	return p.CompactResponses(context.Background(), &commonresponses.Request{
		Operation: commonresponses.ResponsesCompact,
		Body:      envelope,
		Control: commonresponses.Control{
			DownstreamDialect: commonresponses.DownstreamResponses,
			Stream:            request.Stream,
		},
		Model: request.Model,
	})
}

func (h *OpenAIResponsesStreamHandler) HandlerResponsesStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	rawEvent := string(*rawLine)
	if !strings.HasSuffix(rawEvent, "\n\n") && !strings.HasSuffix(rawEvent, "\r\n\r\n") {
		rawEvent += "\n\n"
	}
	if err := h.ObserveResponsesEvent(rawEvent); err != nil {
		*rawLine = requester.StreamClosed
		errChan <- responsesUsageTrackingError(err)
		return
	}
	dataChan <- string(*rawLine)
}

func openAIResponsesRawRequestForTest(t *testing.T, raw, model string, stream bool, promptCacheKey string) *commonresponses.Request {
	t.Helper()
	envelope, err := commonresponses.ParseRawEnvelope([]byte(raw))
	if err != nil {
		t.Fatalf("parse raw responses envelope: %v", err)
	}
	req := &commonresponses.Request{
		Operation: commonresponses.ResponsesCreate,
		Body:      envelope,
		Control: commonresponses.Control{
			DownstreamDialect: commonresponses.DownstreamResponses,
			Stream:            stream,
		},
		Model: model,
	}
	if promptCacheKey != "" {
		req.Policy.PromptCache = &commonresponses.PromptCacheDecision{
			Key:    promptCacheKey,
			Source: commonresponses.PromptCacheRouteHint,
		}
	}
	return req
}

func TestOpenAIResponsesCreateUsesRawEnvelopeBody(t *testing.T) {
	var bodyBytes []byte
	var requestHeaders http.Header
	providerResponseBody := []byte(`{"id":"resp_1","object":"response","model":"mapped-model","status":"completed","output":[{"type":"future_output","quality":{"future":true}}],"usage":{"input_tokens":18,"output_tokens":1,"total_tokens":19,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":5,"future_cache_tokens":11}},"future_response_field":{"enabled":true}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHeaders = r.Header.Clone()
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerResponseBody)
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Type:  config.ChannelTypeOpenAI,
		Key:   "sk-test",
		Proxy: &proxy,
	}, server.URL)
	// The test server stands in for the statically trusted official endpoint.
	provider.SetProviderRawJSONReplay(true)
	provider.Usage = &types.Usage{}

	rawReq := openAIResponsesRawRequestForTest(t, `{
		"model":"client-model",
		"input":"hello",
		"prompt_cache_key":"client-cache",
		"future_field":{"enabled":true},
		"unknown_number":12345678901234567890
	}`, "mapped-model", false, "route-cache")
	rawReq.Headers = requestctx.NewHeaderSnapshot(http.Header{
		"Idempotency-Key":   {"idem-create"},
		"X-Future-Business": {"one", "two"},
		"Authorization":     {"Bearer client-secret"},
	})
	response, errWithCode := provider.CreateResponses(context.Background(), rawReq)
	if errWithCode != nil {
		t.Fatalf("CreateResponses returned error: %v", errWithCode.Message)
	}
	if got := response.ProviderRawJSON(); !bytes.Equal(got, providerResponseBody) {
		t.Fatalf("expected exact provider response body to be retained, got %s", got)
	}
	if got := response.ReplayProviderRawJSON(); !bytes.Equal(got, providerResponseBody) {
		t.Fatalf("expected native Responses adapter to opt in to exact provider response replay, got %s", got)
	}
	extraTokens := provider.Usage.GetExtraTokens()
	if extraTokens[config.UsageExtraCache] != 2 || extraTokens[config.UsageExtraCacheWrite] != 5 {
		t.Fatalf("exact-wire observation lost official provider cache evidence: %+v", extraTokens)
	}

	body := make(map[string]json.RawMessage)
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("decode request body: %v body=%s", err, bodyBytes)
	}
	if string(body["model"]) != `"mapped-model"` {
		t.Fatalf("expected mapped model to overwrite client model, got %s", body["model"])
	}
	if string(body["prompt_cache_key"]) != `"client-cache"` {
		t.Fatalf("expected client prompt_cache_key to win over route hint, got %s", body["prompt_cache_key"])
	}
	if string(body["future_field"]) != `{"enabled":true}` {
		t.Fatalf("expected unknown raw field to be preserved, got %s", body["future_field"])
	}
	if string(body["unknown_number"]) != `12345678901234567890` {
		t.Fatalf("expected unknown numeric field to be preserved, got %s", body["unknown_number"])
	}
	if _, exists := body["stream"]; exists {
		t.Fatalf("expected non-streaming create to preserve stream omission, got %s", body["stream"])
	}
	if requestHeaders.Get("Idempotency-Key") != "idem-create" || len(requestHeaders.Values("X-Future-Business")) != 2 {
		t.Fatalf("expected full create send path to forward business headers, got %v", requestHeaders)
	}
	if got := requestHeaders.Get("Authorization"); got != "Bearer sk-test" {
		t.Fatalf("client credential replaced provider credential: %q", got)
	}
}

func TestOpenAIResponsesCreatePreservesPresentZeroUsage(t *testing.T) {
	providerResponseBody := []byte(`{"id":"resp_zero","object":"response","model":"provider-model","service_tier":"default","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"nonempty"}]}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerResponseBody)
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Type:  config.ChannelTypeOpenAI,
		Key:   "sk-test",
		Proxy: &proxy,
	}, server.URL)
	provider.Usage = &types.Usage{PromptTokens: 99, CompletionTokens: 88, TotalTokens: 187}

	response, apiErr := provider.CreateResponses(context.Background(), openAIResponsesRawRequestForTest(
		t,
		`{"model":"client-model","input":"hello"}`,
		"mapped-model",
		false,
		"",
	))
	if apiErr != nil {
		t.Fatalf("CreateResponses returned error: %v", apiErr.Message)
	}
	if response.Usage == nil || response.Usage.InputTokens != 0 || response.Usage.OutputTokens != 0 || response.Usage.TotalTokens != 0 {
		t.Fatalf("present zero usage must not be replaced by fallback estimation: %+v", response.Usage)
	}
	if provider.Usage.PromptTokens != 0 || provider.Usage.CompletionTokens != 0 || provider.Usage.TotalTokens != 0 {
		t.Fatalf("settlement usage must preserve provider zero evidence: %+v", provider.Usage)
	}
	if provider.Usage.ResponseModel != "provider-model" || provider.Usage.ServiceTier != "default" {
		t.Fatalf("zero token evidence must retain provider attribution: %+v", provider.Usage)
	}
}

func TestOpenAIResponsesJSONAndSSETerminalAreSemanticallyEquivalent(t *testing.T) {
	providerResponseBody := []byte(`{
		"id":"resp_contract",
		"object":"response",
		"model":"future-model",
		"status":"completed",
		"output":[
			{"id":"program_1","type":"program","status":"completed","fingerprint":"fp_1","agent":{"id":"agent_a"},"phase":"analysis"},
			{"id":"call_1","type":"function_call","status":"completed","call_id":"call_1","name":"run","arguments":"{}","caller":{"type":"program","id":"program_1"},"author":"agent_a","recipient":"tool"},
			{"id":"program_output_1","type":"program_output","status":"completed","program_id":"program_1","output":"ok","caller":{"id":"call_1"}},
			{"id":"reasoning_1","type":"reasoning","status":"completed","summary":[],"encrypted_content":"opaque","context":"all_turns","effective_context":{"mode":"all_turns"}}
		],
		"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7},
		"multi_agent":{"root_agent":"agent_a"},
		"future_top_level":{"large_integer":9007199254740993}
	}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerResponseBody)
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Type:  config.ChannelTypeOpenAI,
		Key:   "sk-test",
		Proxy: &proxy,
	}, server.URL)
	provider.SetProviderRawJSONReplay(true)
	provider.Usage = &types.Usage{}
	response, apiErr := provider.CreateResponses(context.Background(), openAIResponsesRawRequestForTest(
		t,
		`{"model":"future-model","input":"hello"}`,
		"future-model",
		false,
		"",
	))
	if apiErr != nil {
		t.Fatalf("CreateResponses returned error: %v", apiErr.Message)
	}

	handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "future-model"}
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)
	var compactResponse bytes.Buffer
	if err := json.Compact(&compactResponse, providerResponseBody); err != nil {
		t.Fatalf("compact provider response: %v", err)
	}
	eventBody := append([]byte(`data: {"type":"response.completed","sequence_number":9,"response":`), compactResponse.Bytes()...)
	eventBody = append(eventBody, '}')
	handler.HandlerResponsesStream(&eventBody, dataChan, errChan)
	emitted := <-dataChan

	var terminal struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(emitted, "data:"))), &terminal); err != nil {
		t.Fatalf("decode emitted SSE terminal: %v; event=%s", err, emitted)
	}
	assertJSONSemanticallyEqual(t, response.ReplayProviderRawJSON(), terminal.Response)
	if provider.Usage.TotalTokens != 7 || handler.Usage.TotalTokens != 7 {
		t.Fatalf("raw delivery must retain equivalent typed usage observation: json=%+v sse=%+v", provider.Usage, handler.Usage)
	}
}

func assertJSONSemanticallyEqual(t *testing.T, left, right []byte) {
	t.Helper()
	decode := func(raw []byte) any {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("decode JSON fixture: %v; body=%s", err, raw)
		}
		return value
	}
	if leftValue, rightValue := decode(left), decode(right); !reflect.DeepEqual(leftValue, rightValue) {
		t.Fatalf("JSON and SSE terminal responses differ semantically:\nJSON=%s\nSSE=%s", left, right)
	}
}

func TestSameDialectResponsesCreatePreservesFutureNestedResponseFields(t *testing.T) {
	providerResponseBody := []byte(`{"id":"resp_same","object":"response","model":"mapped-model","status":"completed","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","logprobs":{"future_number":12345678901234567890}}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"future_usage":{"keep":true}}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerResponseBody)
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "sk-test", Proxy: &proxy}, server.URL)
	if provider.ProviderRawJSONReplay {
		t.Fatal("same-dialect preservation must not depend on exact-wire replay")
	}
	provider.Usage = &types.Usage{}
	response, apiErr := provider.CreateResponses(context.Background(), openAIResponsesRawRequestForTest(t, `{"model":"client-model","input":"hello"}`, "mapped-model", false, ""))
	if apiErr != nil {
		t.Fatalf("CreateResponses returned error: %v", apiErr.Message)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal same-dialect response: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"future_number":12345678901234567890`)) || !bytes.Contains(encoded, []byte(`"future_usage":{"keep":true}`)) {
		t.Fatalf("same-dialect response lost future nested fields: %s", encoded)
	}
}

func TestSameDialectResponsesDoesNotInventMissingUsage(t *testing.T) {
	providerResponseBody := []byte(`{"id":"resp_no_usage","object":"response","model":"mapped-model","status":"completed","output":[],"future":{"kept":true}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerResponseBody)
	}))
	t.Cleanup(server.Close)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "sk-test", Proxy: &proxy}, server.URL)
	provider.Usage = &types.Usage{PromptTokens: 99}
	response, apiErr := provider.CreateResponses(context.Background(), openAIResponsesRawRequestForTest(t, `{"model":"client-model","input":"hello"}`, "mapped-model", false, ""))
	if apiErr != nil {
		t.Fatalf("CreateResponses returned error: %+v", apiErr)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, exists := fields["usage"]; exists {
		t.Fatalf("local estimate escaped as provider usage: %s", encoded)
	}
	if provider.Usage.ProviderReported {
		t.Fatalf("missing provider usage became evidence: %+v", provider.Usage)
	}
}

func TestUnaryExactResponseCaptureKeepsCacheSafetyHeaders(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, "https://api.openai.com")
	provider.SetProviderRawJSONReplay(true)
	provider.SetContext(ctx)
	provider.captureProviderResponseHeaders(&http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"Content-Type":  {"application/json"},
		"Cache-Control": {"private, no-store"},
		"Vary":          {"Authorization"},
		"Etag":          {`"wire-v1"`},
	}}, true)
	value, ok := ctx.Get(requestctx.ProviderResponseHeadersContextKey)
	if !ok {
		t.Fatal("provider response headers were not captured")
	}
	headers := value.(http.Header)
	if headers.Get("Cache-Control") != "private, no-store" || headers.Get("Vary") != "Authorization" {
		t.Fatalf("exact unary cache headers lost: %v", headers)
	}
	if headers.Get("Etag") != "" {
		t.Fatalf("create operation exposed read-validator metadata: %v", headers)
	}
}

func TestSameDialectCompactPreservesMissingModelAndStatus(t *testing.T) {
	providerResponseBody := []byte(`{"id":"resp_compact","object":"response.compaction","created_at":1750000000,"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"future_compact_field":{"kept":true}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerResponseBody)
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "sk-test", Proxy: &proxy}, server.URL)
	provider.Usage = &types.Usage{}
	rawReq := openAIResponsesRawRequestForTest(t, `{"model":"gpt-5","input":"hello"}`, "gpt-5", false, "")
	rawReq.Operation = commonresponses.ResponsesCompact
	response, apiErr := provider.CompactResponses(context.Background(), rawReq)
	if apiErr != nil {
		t.Fatalf("CompactResponses returned error: %v", apiErr.Message)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal same-dialect compact response: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode same-dialect compact response: %v", err)
	}
	for _, field := range []string{"model", "status"} {
		if value, ok := fields[field]; ok {
			t.Fatalf("same-dialect compact injected %s=%s: %s", field, value, encoded)
		}
	}
	if string(fields["future_compact_field"]) != `{"kept":true}` || string(fields["object"]) != `"response.compaction"` {
		t.Fatalf("same-dialect compact fields changed: %s", encoded)
	}
}

func TestStructuredResponsesRedirectPassthroughRequiresExactWireDialect(t *testing.T) {
	redirectHit := make(chan struct{}, 1)
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectHit <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(redirectTarget.Close)

	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Location", redirectTarget.URL)
		w.Header().Set("X-Request-Id", "req-redirect")
		w.Header().Set("Set-Cookie", "provider_session=secret")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write([]byte("redirect body\n"))
	}))
	t.Cleanup(providerServer.Close)

	originalHTTPClient := requester.HTTPClient
	httpClient := providerServer.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	requester.HTTPClient = httpClient
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	tests := []struct {
		name       string
		exactWire  bool
		dialect    commonresponses.DownstreamDialect
		operation  commonresponses.Operation
		wantReplay bool
	}{
		{name: "create exact wire", exactWire: true, dialect: commonresponses.DownstreamResponses, operation: commonresponses.ResponsesCreate, wantReplay: true},
		{name: "compact exact wire", exactWire: true, dialect: commonresponses.DownstreamResponses, operation: commonresponses.ResponsesCompact, wantReplay: true},
		{name: "input tokens exact wire", exactWire: true, dialect: commonresponses.DownstreamResponses, operation: commonresponses.ResponsesInputTokens, wantReplay: true},
		{name: "create same dialect", dialect: commonresponses.DownstreamResponses, operation: commonresponses.ResponsesCreate},
		{name: "compact same dialect", dialect: commonresponses.DownstreamResponses, operation: commonresponses.ResponsesCompact},
		{name: "create cross protocol", exactWire: true, dialect: commonresponses.DownstreamChatCompletions, operation: commonresponses.ResponsesCreate},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy := ""
			provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, providerServer.URL)
			provider.SetProviderRawJSONReplay(test.exactWire)
			provider.Usage = &types.Usage{}
			rawReq := openAIResponsesRawRequestForTest(t, `{"model":"gpt-5","input":"hello"}`, "gpt-5", false, "")
			rawReq.Operation = test.operation
			rawReq.Control.DownstreamDialect = test.dialect

			var apiErr *types.OpenAIErrorWithStatusCode
			switch test.operation {
			case commonresponses.ResponsesCompact:
				_, apiErr = provider.CompactResponses(context.Background(), rawReq)
			case commonresponses.ResponsesInputTokens:
				_, apiErr = provider.CountResponsesInputTokens(context.Background(), rawReq)
			default:
				_, apiErr = provider.CreateResponses(context.Background(), rawReq)
			}
			if apiErr == nil || apiErr.StatusCode != http.StatusTemporaryRedirect {
				t.Fatalf("expected provider redirect, got %+v", apiErr)
			}
			if apiErr.ReplayRawResponse != test.wantReplay {
				t.Fatalf("ReplayRawResponse=%v, want %v: %+v", apiErr.ReplayRawResponse, test.wantReplay, apiErr)
			}
			if test.wantReplay {
				if string(apiErr.RawBody) != "redirect body\n" || apiErr.ResponseHeaders.Get("Location") != redirectTarget.URL {
					t.Fatalf("exact redirect response changed: body=%q headers=%#v", apiErr.RawBody, apiErr.ResponseHeaders)
				}
				if apiErr.ResponseHeaders.Get("Set-Cookie") != "" {
					t.Fatalf("unsafe redirect header leaked: %#v", apiErr.ResponseHeaders)
				}
			} else if len(apiErr.RawBody) != 0 || apiErr.ResponseHeaders.Get("Location") != "" {
				t.Fatalf("non-exact path exposed redirect response: %+v", apiErr)
			}
		})
	}

	select {
	case <-redirectHit:
		t.Fatal("provider client followed redirect target")
	default:
	}
}

func TestOpenAIStoredResponsesRelayPreservesOperationQueryAndSanitizesErrors(t *testing.T) {
	tests := []struct {
		name      string
		operation providersBase.Operation
		method    string
		path      string
	}{
		{name: "retrieve", operation: providersBase.OperationResponsesRetrieve, method: http.MethodGet, path: "/v1/responses/resp_123"},
		{name: "delete", operation: providersBase.OperationResponsesDelete, method: http.MethodDelete, path: "/v1/responses/resp_123"},
		{name: "input items", operation: providersBase.OperationResponsesInputItems, method: http.MethodGet, path: "/v1/responses/resp_123/input_items"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != test.method || r.URL.Path != test.path || r.URL.RawQuery != "limit=7&after=item_1" {
					t.Errorf("unexpected lifecycle request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
				}
				if got := r.Header.Get("OpenAI-Beta"); got != "responses=v1" {
					t.Errorf("expected business header, got %q", got)
				}
				if got := r.Header.Values("X-Future-Business"); len(got) != 0 {
					t.Errorf("compatible endpoint received an unregistered exact-wire header: %v", got)
				}
				wantConditional := ""
				if test.method == http.MethodGet {
					wantConditional = `"etag-1"`
				}
				if got := r.Header.Get("If-None-Match"); got != wantConditional {
					t.Errorf("conditional header=%q, want %q", got, wantConditional)
				}
				if got := r.Header.Get("X-One-Hub-Channel-Id"); got != "" {
					t.Errorf("proxy channel selector leaked upstream: %q", got)
				}
				w.Header().Set("X-Request-Id", "req-lifecycle")
				w.WriteHeader(http.StatusTeapot)
				_, _ = w.Write([]byte(`{"error":{"message":"tenant diagnostics","type":"invalid_request_error","future":{"account_id":"acct-secret"}}}`))
			}))
			t.Cleanup(server.Close)

			originalHTTPClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

			proxy := ""
			provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, server.URL)
			response, apiErr := provider.RelayStoredResponse(context.Background(), providersBase.StoredResponsesRequest{
				Operation:  test.operation,
				ResponseID: "resp_123",
				RawQuery:   "limit=7&after=item_1",
				Headers: requestctx.NewHeaderSnapshot(http.Header{
					"OpenAI-Beta":          {"responses=v1"},
					"If-None-Match":        {`"etag-1"`},
					"X-Future-Business":    {"one", "two"},
					"X-One-Hub-Channel-Id": {"17"},
				}),
			})
			if apiErr == nil {
				t.Fatal("expected lifecycle provider error to use the shared sanitizer")
			}
			if response != nil || apiErr.StatusCode != http.StatusTeapot {
				t.Fatalf("expected sanitized status 418 without a raw response, response=%v error=%+v", response, apiErr)
			}
			if strings.Contains(apiErr.Message, "acct-secret") || len(apiErr.RawBody) != 0 {
				t.Fatalf("expected provider account diagnostics to be redacted, got %q", apiErr.Message)
			}
			if apiErr.ResponseHeaders.Get("X-Request-Id") != "req-lifecycle" {
				t.Fatalf("expected safe lifecycle response headers, got %#v", apiErr.ResponseHeaders)
			}
		})
	}
}

func TestRegisteredExactWireHeadersCoverResponsesCreateCompactAndInputTokens(t *testing.T) {
	proxy := ""
	modelHeaders := `{"X-Admin-Fixed":"admin"}`
	provider := CreateOpenAIProvider(&model.Channel{
		Type:         config.ChannelTypeOpenAI,
		Key:          "provider-key",
		Proxy:        &proxy,
		ModelHeaders: &modelHeaders,
	}, "https://api.openai.com")
	rawReq := openAIResponsesRawRequestForTest(t, `{"model":"gpt-5","input":"hello"}`, "gpt-5", false, "")
	rawReq.Headers = requestctx.NewHeaderSnapshot(http.Header{
		"Idempotency-Key":   {"idem-1"},
		"If-Modified-Since": {"Mon, 17 Aug 2026 00:00:00 GMT"},
		"X-Future-Business": {"one", "two"},
		"X-Admin-Fixed":     {"client"},
		"Authorization":     {"Bearer client-secret"},
		"Connection":        {"keep-alive"},
	})

	create, createErr := provider.buildResponsesCreateRequest(rawReq, responsesRequestProjection(rawReq), false)
	if createErr != nil {
		t.Fatalf("build create request: %v", createErr)
	}
	compact, compactErr := provider.buildCompactResponsesRequest(rawReq, responsesRequestProjection(rawReq))
	if compactErr != nil {
		t.Fatalf("build compact request: %v", compactErr)
	}
	for _, req := range []*http.Request{create, compact} {
		assertRegisteredExactWireRequestHeaders(t, req)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assertRegisteredExactWireRequestHeaders(t, req)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })
	provider.Config.BaseURL = server.URL
	response, inputErr := provider.CountResponsesInputTokens(context.Background(), rawReq)
	if inputErr != nil {
		t.Fatalf("count input tokens: %v", inputErr)
	}
	response.Body.Close()
}

func assertRegisteredExactWireRequestHeaders(t *testing.T, req *http.Request) {
	t.Helper()
	if got := req.Header.Get("Idempotency-Key"); got != "idem-1" {
		t.Fatalf("Idempotency-Key=%q", got)
	}
	if got := req.Header.Get("If-Modified-Since"); got == "" {
		t.Fatal("If-Modified-Since was not forwarded")
	}
	if got := req.Header.Values("X-Future-Business"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("unknown business header values=%v", got)
	}
	if got := req.Header.Get("X-Admin-Fixed"); got != "admin" {
		t.Fatalf("administrator header priority lost: %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer provider-key" {
		t.Fatalf("provider authorization=%q", got)
	}
	if got := req.Header.Get("Connection"); got != "" {
		t.Fatalf("hop-by-hop header leaked: %q", got)
	}
}

func TestAzureV1StoredResponseRelayKeepsEscapedResponseIDSegment(t *testing.T) {
	var requestURI string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI = r.RequestURI
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Type:  config.ChannelTypeAzureV1,
		Key:   "azure-key",
		Proxy: &proxy,
	}, server.URL)
	provider.IsAzure = true
	provider.Config.Responses = "/v1/responses?api-version=2026-01-01"

	response, apiErr := provider.RelayStoredResponse(context.Background(), providersBase.StoredResponsesRequest{
		Operation:  providersBase.OperationResponsesRetrieve,
		ResponseID: "resp/tenant",
		RawQuery:   "limit=1",
	})
	if apiErr != nil {
		t.Fatalf("relay Azure V1 stored response: %v", apiErr)
	}
	defer response.Body.Close()
	if requestURI != "/openai/v1/responses/resp%2Ftenant?api-version=2026-01-01&limit=1" {
		t.Fatalf("expected escaped response ID in upstream RequestURI, got %q", requestURI)
	}
}

func TestResponsesURLPathAndQueryComposition(t *testing.T) {
	for _, test := range []struct {
		name    string
		base    string
		segment string
		want    string
	}{
		{name: "relative without query", base: "/v1/responses", segment: "compact", want: "/v1/responses/compact"},
		{name: "relative with query", base: "/v1/responses?api-version=2026-01-01", segment: "input_tokens", want: "/v1/responses/input_tokens?api-version=2026-01-01"},
		{name: "absolute with query", base: "https://upstream.example/v1/responses?api-version=2026-01-01", segment: "compact", want: "https://upstream.example/v1/responses/compact?api-version=2026-01-01"},
		{name: "response id remains one segment", base: "/v1/responses?api-version=2026-01-01", segment: "resp/tenant", want: "/v1/responses/resp%2Ftenant?api-version=2026-01-01"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := appendURLPathSegment(test.base, test.segment)
			if err != nil {
				t.Fatalf("append path segment: %v", err)
			}
			if got != test.want {
				t.Fatalf("URL=%q want=%q", got, test.want)
			}
		})
	}

	got, err := mergeURLRawQuery("/v1/responses/resp_1/input_items?api-version=2026-01-01", "limit=7&after=item_1")
	if err != nil {
		t.Fatalf("merge raw query: %v", err)
	}
	if got != "/v1/responses/resp_1/input_items?api-version=2026-01-01&limit=7&after=item_1" {
		t.Fatalf("merged URL=%q", got)
	}
}

func TestStructuredResponsesPathsPreserveDownstreamQuery(t *testing.T) {
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://resource.openai.azure.com")
	provider.IsAzure = true
	rawReq := &commonresponses.Request{RawQuery: "api-version=preview&feature=one&feature=two"}

	for _, test := range []struct {
		name     string
		segments []string
		wantPath string
	}{
		{name: "create", wantPath: "/v1/responses"},
		{name: "compact", segments: []string{"compact"}, wantPath: "/v1/responses/compact"},
		{name: "input tokens", segments: []string{"input_tokens"}, wantPath: "/v1/responses/input_tokens"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requestPath, err := provider.responsesRequestPath(rawReq, "/v1/responses", test.segments...)
			if err != nil {
				t.Fatalf("build request path: %v", err)
			}
			parsed, err := url.Parse(requestPath)
			if err != nil {
				t.Fatalf("parse request path: %v", err)
			}
			if parsed.Path != test.wantPath || parsed.Query().Get("api-version") != "preview" {
				t.Fatalf("structured request query changed: %q", requestPath)
			}
			if features := parsed.Query()["feature"]; len(features) != 2 {
				t.Fatalf("repeated query values changed: %q", requestPath)
			}
		})
	}

	for _, channelType := range []int{config.ChannelTypeOpenAI, config.ChannelTypeCustom, config.ChannelTypeAzure} {
		provider := CreateOpenAIProvider(&model.Channel{Type: channelType, Proxy: &proxy}, "https://compatible.example")
		path, err := provider.responsesRequestPath(rawReq, "/v1/responses")
		if err != nil || path != "/v1/responses?api-version=preview&feature=one&feature=two" {
			t.Fatalf("channel %d lost downstream query: path=%q err=%v", channelType, path, err)
		}
	}
}

func TestCustomAbsoluteResponsesURIAndAzureQueryRemainIntact(t *testing.T) {
	proxy := ""
	custom := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "sk-test", Proxy: &proxy}, "https://default.example")
	custom.Config.Responses = "https://responses.example/custom/responses?tenant=one"
	path, err := appendURLPathSegment(custom.Config.Responses, "compact")
	if err != nil {
		t.Fatalf("append custom compact path: %v", err)
	}
	if got := custom.GetFullRequestURL(path, "gpt-5"); got != "https://responses.example/custom/responses/compact?tenant=one" {
		t.Fatalf("absolute custom Responses URL=%q", got)
	}

	azure := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeAzure, Key: "azure-key", Proxy: &proxy, Other: `{"api_version":"2026-01-01"}`}, "https://resource.openai.azure.com")
	azure.IsAzure = true
	azure.Config.Responses = "/v1/responses?api-version=2026-01-01"
	path, err = appendURLPathSegment(azure.Config.Responses, "input_tokens")
	if err != nil {
		t.Fatalf("append Azure input_tokens path: %v", err)
	}
	if got := azure.GetFullRequestURL(path, "gpt-5"); !strings.Contains(got, "/openai/responses/input_tokens?") || !strings.Contains(got, "api-version=2026-01-01") {
		t.Fatalf("Azure Responses URL lost path/query: %q", got)
	}
}

func TestOpenAIResponsesCreateBodyPlannerPatchesPolicyStreamAndCustom(t *testing.T) {
	proxy := ""
	customParameter := `{"extra_metadata":{"operator":"custom","nested":{"operator":true}}}`
	provider := CreateOpenAIProvider(&model.Channel{
		Type:            config.ChannelTypeOpenAI,
		Key:             "sk-test",
		Proxy:           &proxy,
		CustomParameter: &customParameter,
	}, "https://api.openai.com")

	rawReq := openAIResponsesRawRequestForTest(t, `{
		"model":"client-model",
		"input":"hello",
		"stream":false,
		"extra_metadata":{"client":"kept","nested":{"client":true}}
	}`, "mapped-model", true, "route-cache")
	request := responsesRequestProjection(rawReq)
	body, errWithCode := provider.buildResponsesCreateBody(rawReq, request, true)
	if errWithCode != nil {
		t.Fatalf("buildResponsesCreateBody returned error: %v", errWithCode.Message)
	}
	if body["model"] != "mapped-model" {
		t.Fatalf("expected mapped model patch, got %#v", body["model"])
	}
	if body["stream"] != true {
		t.Fatalf("expected streaming create to force stream=true, got %#v", body["stream"])
	}
	if body["prompt_cache_key"] != "route-cache" {
		t.Fatalf("expected route hint prompt_cache_key to be added, got %#v", body["prompt_cache_key"])
	}
	metadata, ok := body["extra_metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected extra_metadata object, got %#v", body["extra_metadata"])
	}
	if metadata["client"] != "kept" || metadata["operator"] != "custom" {
		t.Fatalf("expected custom extra_metadata to merge with raw field, got %#v", metadata)
	}

	rawReq = openAIResponsesRawRequestForTest(t, `{
		"model":"client-model",
		"input":"hello",
		"stream":false,
		"prompt_cache_key":"client-cache"
	}`, "mapped-model", false, "route-cache")
	request = responsesRequestProjection(rawReq)
	body, errWithCode = provider.buildResponsesCreateBody(rawReq, request, false)
	if errWithCode != nil {
		t.Fatalf("buildResponsesCreateBody returned error: %v", errWithCode.Message)
	}
	if body["stream"] != false {
		t.Fatalf("expected non-streaming create to preserve explicit stream=false, got %#v", body["stream"])
	}
	if body["prompt_cache_key"] != "client-cache" {
		t.Fatalf("expected client prompt_cache_key to remain, got %#v", body["prompt_cache_key"])
	}
}

func TestOpenAIResponsesCreateBodyRejectsLifecycleCustomParameterChanges(t *testing.T) {
	proxy := ""
	for _, test := range []struct {
		name            string
		rawBody         string
		customParameter string
		wantParam       string
	}{
		{
			name:            "adds store false after omitted store admission",
			rawBody:         `{"model":"gpt-5","input":"hello"}`,
			customParameter: `{"store":false}`,
			wantParam:       "store",
		},
		{
			name:            "overwrites previous response owner route",
			rawBody:         `{"model":"gpt-5","input":"hello","previous_response_id":"resp_client"}`,
			customParameter: `{"overwrite":true,"previous_response_id":"resp_channel"}`,
			wantParam:       "previous_response_id",
		},
		{
			name:            "removes store after admission",
			rawBody:         `{"model":"gpt-5","input":"hello","store":false}`,
			customParameter: `{"remove_params":["store"]}`,
			wantParam:       "store",
		},
		{
			name:            "adds unsupported background lifecycle",
			rawBody:         `{"model":"gpt-5","input":"hello"}`,
			customParameter: `{"background":true}`,
			wantParam:       "background",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := CreateOpenAIProvider(&model.Channel{
				Type:            config.ChannelTypeOpenAI,
				Key:             "sk-test",
				Proxy:           &proxy,
				CustomParameter: &test.customParameter,
			}, "https://api.openai.com")
			rawReq := openAIResponsesRawRequestForTest(t, test.rawBody, "gpt-5", false, "")
			_, apiErr := provider.buildResponsesCreateBody(rawReq, responsesRequestProjection(rawReq), false)
			if apiErr == nil || apiErr.Code != "responses_lifecycle_custom_parameter_conflict" || apiErr.Param != test.wantParam || !apiErr.LocalError || apiErr.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("expected lifecycle custom parameter conflict for %s, got %+v", test.wantParam, apiErr)
			}
		})
	}
}

func TestOpenAIResponsesCreateBodyAllowsUnchangedLifecycleCustomParameter(t *testing.T) {
	proxy := ""
	customParameter := `{"overwrite":true,"store":false,"temperature":0.4}`
	provider := CreateOpenAIProvider(&model.Channel{
		Type:            config.ChannelTypeOpenAI,
		Key:             "sk-test",
		Proxy:           &proxy,
		CustomParameter: &customParameter,
	}, "https://api.openai.com")
	rawReq := openAIResponsesRawRequestForTest(t, `{"model":"gpt-5","input":"hello","store":false}`, "gpt-5", false, "")
	body, apiErr := provider.buildResponsesCreateBody(rawReq, responsesRequestProjection(rawReq), false)
	if apiErr != nil {
		t.Fatalf("unchanged lifecycle custom parameter was rejected: %+v", apiErr)
	}
	if body["store"] != false || body["temperature"] != 0.4 {
		t.Fatalf("ordinary custom parameter merge changed: %#v", body)
	}
}

func TestOpenAIResponsesCreateBodyRejectsResourceInjectedByCustomParameter(t *testing.T) {
	proxy := ""
	customParameter := `{"tools":[{"type":"file_search","vector_store_ids":["vs_shared"]}]}`
	provider := CreateOpenAIProvider(&model.Channel{
		Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy, CustomParameter: &customParameter,
	}, "https://api.openai.com")
	rawReq := openAIResponsesRawRequestForTest(t, `{"model":"gpt-5","input":"hello"}`, "gpt-5", false, "")
	if _, apiErr := provider.buildResponsesCreateBody(rawReq, responsesRequestProjection(rawReq), false); apiErr == nil || apiErr.Code != "unsupported_resource_reference" || apiErr.StatusCode != http.StatusBadRequest || !apiErr.LocalError {
		t.Fatalf("expected post-overlay resource gate before provider work, got %+v", apiErr)
	}
}

func TestHandlerChatStreamToolCallsFinishReasonFromToolEvent(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 6)
	errChan := make(chan error, 1)

	added := []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","status":"in_progress","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"}}`)
	handler.HandlerChatStream(&added, dataChan, errChan)

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
	handler.HandlerChatStream(&completed, dataChan, errChan)

	_ = mustReadChunk(t, dataChan) // tool_call delta chunk
	finalChunk := mustReadChunk(t, dataChan)
	finishReason := mustGetFinishReason(t, finalChunk)

	if finishReason != types.FinishReasonToolCalls {
		t.Fatalf("expected finish_reason=%q, got %q", types.FinishReasonToolCalls, finishReason)
	}

	mustReadProtocolEOF(t, errChan)
}

func TestHandlerChatStreamTurnsFailedTerminalIntoError(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-5"}
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)

	failed := []byte(`data: {"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"type":"server_error","code":"provider_failed","message":"provider failed"}}}`)
	handler.HandlerChatStream(&failed, dataChan, errChan)
	select {
	case chunk := <-dataChan:
		t.Fatalf("failed Responses terminal became a successful Chat chunk: %s", chunk)
	default:
	}
	if err := <-errChan; err == nil || errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "provider failed") {
		t.Fatalf("failed terminal signal=%v", err)
	}
	if !bytes.Equal(failed, requester.StreamClosed) {
		t.Fatalf("terminal did not close stream: %q", failed)
	}
}

func TestHandlerChatStreamMapsOnlyLengthIncompleteToSuccess(t *testing.T) {
	for _, test := range []struct {
		name       string
		reason     string
		wantLength bool
	}{
		{name: "max output tokens", reason: "max_output_tokens", wantLength: true},
		{name: "content filter", reason: "content_filter"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-5"}
			dataChan := make(chan string, 1)
			errChan := make(chan error, 1)
			raw := []byte(`data: {"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"` + test.reason + `"}}}`)
			handler.HandlerChatStream(&raw, dataChan, errChan)
			if test.wantLength {
				chunk := mustReadChunk(t, dataChan)
				if got := mustGetFinishReason(t, chunk); got != types.FinishReasonLength {
					t.Fatalf("finish reason=%q", got)
				}
				mustReadProtocolEOF(t, errChan)
				return
			}
			select {
			case chunk := <-dataChan:
				t.Fatalf("non-length incomplete became success: %s", chunk)
			default:
			}
			if err := <-errChan; err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("non-length incomplete error=%v", err)
			}
		})
	}
}

func TestHandlerChatStreamPreservesRefusalDelta(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-5"}
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)
	raw := []byte(`data: {"type":"response.refusal.delta","delta":"cannot comply"}`)
	handler.HandlerChatStream(&raw, dataChan, errChan)
	chunk := mustReadChunk(t, dataChan)
	if len(chunk.Choices) != 1 || chunk.Choices[0].Delta.Refusal != "cannot comply" {
		t.Fatalf("Responses refusal delta was lost: %+v", chunk)
	}
}

func TestHandlerChatStreamTerminatesOnTopLevelErrorEvent(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-5"}
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)

	raw := []byte(`data: {"type":"error","sequence_number":3,"code":"bad_input","message":"invalid tool","param":"tools[0]"}`)
	handler.HandlerChatStream(&raw, dataChan, errChan)

	select {
	case data := <-dataChan:
		t.Fatalf("top-level Responses error was emitted as a successful Chat chunk: %s", data)
	default:
	}
	if !bytes.Equal(raw, requester.StreamClosed) {
		t.Fatalf("top-level Responses error did not terminate the provider stream: %q", raw)
	}
	select {
	case err := <-errChan:
		apiErr, ok := err.(*types.OpenAIErrorWithStatusCode)
		if !ok || apiErr.Code != "bad_input" || apiErr.Message != "invalid tool" || apiErr.Param != "tools[0]" || apiErr.LocalError {
			t.Fatalf("provider error semantics changed during Chat conversion: %#v", err)
		}
	default:
		t.Fatal("top-level Responses error was silently dropped")
	}
}

func TestHandlerChatStreamCustomToolCallEvents(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}
	dataChan := make(chan string, 6)
	errChan := make(chan error, 1)

	added := []byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","status":"in_progress","call_id":"call_1","name":"shell","input":""}}`)
	handler.HandlerChatStream(&added, dataChan, errChan)
	addedChunk := mustReadChunk(t, dataChan)
	if len(addedChunk.Choices) != 1 || len(addedChunk.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("expected one custom tool call header, got %#v", addedChunk.Choices)
	}
	addedCall := addedChunk.Choices[0].Delta.ToolCalls[0]
	if addedCall.Index != 0 || addedCall.Id != "call_1" || addedCall.Type != types.ToolChoiceTypeCustom || addedCall.Custom == nil || addedCall.Custom.Name != "shell" || addedCall.Function != nil {
		t.Fatalf("unexpected custom tool call header: %#v", addedCall)
	}

	delta := []byte(`data: {"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"ctc_1","delta":"echo ok"}`)
	handler.HandlerChatStream(&delta, dataChan, errChan)
	deltaChunk := mustReadChunk(t, dataChan)
	deltaCall := deltaChunk.Choices[0].Delta.ToolCalls[0]
	if deltaCall.Index != 0 || deltaCall.Custom == nil || deltaCall.Custom.Input != "echo ok" || deltaCall.Function != nil {
		t.Fatalf("unexpected custom tool input delta: %#v", deltaCall)
	}

	done := []byte(`data: {"type":"response.custom_tool_call_input.done","output_index":0,"item_id":"ctc_1","input":"echo ok"}`)
	handler.HandlerChatStream(&done, dataChan, errChan)
	select {
	case chunk := <-dataChan:
		t.Fatalf("aggregate custom input done event must not duplicate Chat deltas: %s", chunk)
	default:
	}

	secondAdded := []byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","id":"ctc_2","status":"in_progress","call_id":"call_2","name":"shell","input":"ls"}}`)
	handler.HandlerChatStream(&secondAdded, dataChan, errChan)
	secondChunk := mustReadChunk(t, dataChan)
	secondCall := secondChunk.Choices[0].Delta.ToolCalls[0]
	if secondCall.Index != 1 || secondCall.Custom == nil || secondCall.Custom.Input != "ls" {
		t.Fatalf("custom input done did not advance the Chat tool index: %#v", secondCall)
	}
	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
	handler.HandlerChatStream(&completed, dataChan, errChan)
	if finishReason := mustGetFinishReason(t, mustReadChunk(t, dataChan)); finishReason != types.FinishReasonToolCalls {
		t.Fatalf("expected finish_reason=%q, got %q", types.FinishReasonToolCalls, finishReason)
	}
	mustReadProtocolEOF(t, errChan)
}

func TestHandlerChatStreamUsesActualResponseModelAndTier(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "requested-alias",
	}
	dataChan := make(chan string, 2)
	errChan := make(chan error, 1)

	created := []byte(`data: {"type":"response.created","response":{"id":"resp_actual","model":"gpt-5.6-terra","service_tier":"flex","status":"in_progress"}}`)
	handler.HandlerChatStream(&created, dataChan, errChan)
	createdChunk := mustReadChunk(t, dataChan)
	if createdChunk.Model != "gpt-5.6-terra" || createdChunk.ServiceTier != "flex" {
		t.Fatalf("expected actual response model/tier, got model=%q tier=%q", createdChunk.Model, createdChunk.ServiceTier)
	}

	delta := []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`)
	handler.HandlerChatStream(&delta, dataChan, errChan)
	deltaChunk := mustReadChunk(t, dataChan)
	if deltaChunk.Model != "gpt-5.6-terra" || deltaChunk.ServiceTier != "flex" {
		t.Fatalf("expected later chunks to retain actual model/tier, got model=%q tier=%q", deltaChunk.Model, deltaChunk.ServiceTier)
	}
	if handler.Usage.ResponseModel != "gpt-5.6-terra" || handler.Usage.ServiceTier != "flex" {
		t.Fatalf("expected usage attribution to retain actual model/tier, got %+v", handler.Usage)
	}
}

func TestHandlerResponsesStreamUsesSharedUsageTracker(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}
	dataChan := make(chan string, 4)
	errChan := make(chan error, 1)

	image := []byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"img_1","type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}}`)
	handler.HandlerResponsesStream(&image, dataChan, errChan)
	key := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "high-1024x1024")
	if handler.Usage.ExtraBilling[key].CallCount != 0 {
		t.Fatalf("expected image generation billing to wait for a successful terminal, got %+v", handler.Usage.ExtraBilling)
	}

	realtimeDone := []byte(`data: {"type":"response.done","response":{"id":"resp_realtime","status":"completed","usage":{"input_tokens":9,"output_tokens":9,"total_tokens":18}}}`)
	handler.HandlerResponsesStream(&realtimeDone, dataChan, errChan)
	if handler.Usage.TotalTokens != 0 || handler.Usage.ExtraBilling[key].CallCount != 0 {
		t.Fatalf("expected Realtime terminal dialect not to settle Responses usage, got %+v", handler.Usage)
	}

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_done","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
	handler.HandlerResponsesStream(&completed, dataChan, errChan)
	if handler.Usage.TotalTokens != 3 {
		t.Fatalf("expected response.completed terminal usage to be applied, got %+v", handler.Usage)
	}
	if handler.Usage.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("expected successful terminal to commit image generation billing, got %+v", handler.Usage.ExtraBilling)
	}
}

func TestAcceptedResponsesEventStopsAtToolIdentityLimit(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-5"}
	for index := 0; index < 1024; index++ {
		event := fmt.Sprintf("data: {\"type\":\"response.output_item.done\",\"item_id\":\"ws_%d\",\"item\":{\"id\":\"ws_%d\",\"type\":\"web_search_call\",\"status\":\"completed\",\"action\":{\"type\":\"search\"}}}\n\n", index, index)
		if err := handler.ObserveResponsesEvent(event); err != nil {
			t.Fatalf("accepted event %d failed: %v", index, err)
		}
	}
	overflow := "data: {\"type\":\"response.output_item.done\",\"item_id\":\"ws_overflow\",\"item\":{\"id\":\"ws_overflow\",\"type\":\"web_search_call\",\"status\":\"completed\",\"action\":{\"type\":\"search\"}}}\n\n"
	if err := handler.ObserveResponsesEvent(overflow); err == nil || commonresponses.ResponsesStreamTrackingFailureCode(err) != "provider_usage_state_limit" {
		t.Fatalf("unexpected tool identity overflow error: %#v", err)
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if handler.Usage.ExtraBilling[key].CallCount != 1024 {
		t.Fatalf("overflow changed accepted-prefix billing: %+v", handler.Usage.ExtraBilling)
	}
}

func TestHandlerChatStreamStopsAtToolIdentityLimit(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-5"}
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)
	accepted := 0
	for ; accepted <= 1024; accepted++ {
		raw := []byte(fmt.Sprintf(`data: {"type":"response.output_item.done","item_id":"ws_%d","item":{"id":"ws_%d","type":"web_search_call","status":"completed","action":{"type":"search"}}}`, accepted, accepted))
		handler.HandlerChatStream(&raw, dataChan, errChan)
		if bytes.Equal(raw, requester.StreamClosed) {
			break
		}
	}
	if accepted != 1024 {
		t.Fatalf("chat stream stopped after %d accepted identities, want 1024", accepted)
	}
	select {
	case err := <-errChan:
		var providerErr *types.OpenAIErrorWithStatusCode
		if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusBadGateway || providerErr.Code != "provider_usage_state_limit" {
			t.Fatalf("unexpected chat tool identity overflow error: %#v", err)
		}
	default:
		t.Fatal("chat tool identity overflow did not surface a stream error")
	}
}

func TestAcceptedResponsesEventRejectsOversizedImageIdentityAtomically(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-5"}
	itemID := strings.Repeat("i", 257)
	event := fmt.Sprintf("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":%q,\"type\":\"image_generation_call\",\"status\":\"completed\",\"quality\":\"high\",\"size\":\"1024x1024\"}}\n\n", itemID)
	if err := handler.ObserveResponsesEvent(event); err == nil || commonresponses.ResponsesStreamTrackingFailureCode(err) != "provider_usage_state_limit" {
		t.Fatalf("unexpected image identity overflow error: %#v", err)
	}
	if len(handler.Usage.ExtraBilling) != 0 {
		t.Fatalf("rejected image event changed billing: %+v", handler.Usage.ExtraBilling)
	}
}

func TestHandlerChatStreamRejectsOversizedImageIdentityBeforeConversion(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-5"}
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)
	itemID := strings.Repeat("i", 257)
	raw := []byte(fmt.Sprintf(`data: {"type":"response.output_item.done","output_index":0,"item":{"id":%q,"type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}}`, itemID))
	handler.HandlerChatStream(&raw, dataChan, errChan)

	if !bytes.Equal(raw, requester.StreamClosed) {
		t.Fatalf("oversized image identity did not close converted stream: %q", raw)
	}
	select {
	case data := <-dataChan:
		t.Fatalf("offending image event produced a converted chunk: %q", data)
	default:
	}
	select {
	case err := <-errChan:
		var providerErr *types.OpenAIErrorWithStatusCode
		if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusBadGateway || providerErr.Code != "provider_usage_state_limit" {
			t.Fatalf("unexpected converted image identity overflow error: %#v", err)
		}
	default:
		t.Fatal("converted image identity overflow did not surface a stream error")
	}
}

func TestHandlerChatStreamIsolatesConflictingImageEvidence(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-5"}
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)
	raw := []byte(`data: {"type":"response.output_item.done","item_id":"img_top","output_index":0,"item":{"id":"img_item","type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}}`)
	handler.HandlerChatStream(&raw, dataChan, errChan)

	if bytes.Equal(raw, requester.StreamClosed) || !handler.Usage.HasExtraBillingConflict(types.APIToolTypeImageGeneration) {
		t.Fatalf("image conflict affected stream: %q %+v", raw, handler.Usage)
	}
	select {
	case err := <-errChan:
		t.Fatalf("component conflict became transport error: %v", err)
	default:
	}
}

func TestHandlerResponsesStreamPreservesNonBillableImageTerminalWire(t *testing.T) {
	for _, test := range []struct {
		eventType string
		status    string
	}{
		{eventType: "response.failed", status: "failed"},
		{eventType: "response.incomplete", status: "incomplete"},
		{eventType: "response.completed", status: "failed"},
	} {
		t.Run(test.eventType+"/"+test.status, func(t *testing.T) {
			handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-5"}
			dataChan := make(chan string, 1)
			errChan := make(chan error, 1)
			oversized := strings.Repeat("q", 257)
			rawText := fmt.Sprintf(`data: {"type":%q,"response":{"id":"resp_failed_image","status":%q,"tools":[{"type":"image_generation","model":%q}],"output":[{"id":"img_duplicate","type":"image_generation_call","status":"completed","quality":%q},{"id":"img_duplicate","type":"image_generation_call","status":"completed","quality":%q}]}}`, test.eventType, test.status, strings.Repeat("m", 1025), oversized, oversized)
			raw := []byte(rawText)
			handler.HandlerResponsesStream(&raw, dataChan, errChan)
			select {
			case got := <-dataChan:
				if got != rawText {
					t.Fatalf("non-billable terminal wire changed: got %q want %q", got, rawText)
				}
			default:
				t.Fatal("non-billable provider terminal was suppressed")
			}
			select {
			case err := <-errChan:
				t.Fatalf("non-billable provider terminal became proxy error: %v", err)
			default:
			}
			if len(handler.Usage.ExtraBilling) != 0 {
				t.Fatalf("non-billable provider terminal produced image billing: %+v", handler.Usage.ExtraBilling)
			}
		})
	}
}

func TestHandlerResponsesStreamAcceptsDataWithoutSpace(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-5"}
	dataChan := make(chan string, 2)
	errChan := make(chan error, 1)

	createdRaw := `data:{"type":"response.created","response":{"id":"resp_no_space","model":"gpt-5","status":"in_progress"}}`
	created := []byte(createdRaw)
	handler.HandlerResponsesStream(&created, dataChan, errChan)
	if got := <-dataChan; got != createdRaw {
		t.Fatalf("exact Responses stream changed no-space SSE line: %q", got)
	}

	completedRaw := `data:{"type":"response.completed","response":{"id":"resp_no_space","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`
	completed := []byte(completedRaw)
	handler.HandlerResponsesStream(&completed, dataChan, errChan)
	if got := <-dataChan; got != completedRaw {
		t.Fatalf("exact Responses stream changed no-space terminal line: %q", got)
	}
	if handler.Usage.TotalTokens != 5 || handler.Usage.ResponseModel != "gpt-5" {
		t.Fatalf("no-space SSE usage was not observed: %+v", handler.Usage)
	}
}

func TestHandlerChatStreamAcceptsDataWithoutSpace(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-5"}
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)
	raw := []byte(`data:{"type":"response.output_text.delta","delta":"hello"}`)

	handler.HandlerChatStream(&raw, dataChan, errChan)
	chunk := mustReadChunk(t, dataChan)
	if len(chunk.Choices) != 1 || chunk.Choices[0].Delta.Content != "hello" {
		t.Fatalf("no-space Responses event was not converted to Chat: chunk=%+v usage=%+v", chunk, handler.Usage)
	}
	select {
	case err := <-errChan:
		t.Fatalf("unexpected no-space stream error: %v", err)
	default:
	}
}

func TestHandlerChatStreamToolCallArgumentsAcceptJSONObject(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 2)
	errChan := make(chan error, 1)

	added := []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","status":"in_progress","call_id":"call_1","name":"lookup","arguments":{"city":"Paris","days":0}}}`)
	handler.HandlerChatStream(&added, dataChan, errChan)

	chunk := mustReadChunk(t, dataChan)
	if len(chunk.Choices) != 1 || len(chunk.Choices[0].Delta.ToolCalls) != 1 || chunk.Choices[0].Delta.ToolCalls[0].Function == nil {
		t.Fatalf("expected one tool call chunk, got %#v", chunk.Choices)
	}
	if got := chunk.Choices[0].Delta.ToolCalls[0].Function.Arguments; got != `{"city":"Paris","days":0}` {
		t.Fatalf("expected normalized object arguments, got %q", got)
	}

	select {
	case err := <-errChan:
		t.Fatalf("unexpected stream error: %v", err)
	default:
	}
}

func TestHandlerChatStreamToolCallsFinishReasonFromResponseOutput(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 2)
	errChan := make(chan error, 1)

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_2","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3},"output":[{"type":"function_call","id":"fc_2","status":"completed","call_id":"call_2","name":"lookup","arguments":"{}"}]}}`)
	handler.HandlerChatStream(&completed, dataChan, errChan)

	finalChunk := mustReadChunk(t, dataChan)
	finishReason := mustGetFinishReason(t, finalChunk)

	if finishReason != types.FinishReasonToolCalls {
		t.Fatalf("expected finish_reason=%q, got %q", types.FinishReasonToolCalls, finishReason)
	}

	mustReadProtocolEOF(t, errChan)
}

func TestHandlerChatStreamCustomToolCallsFinishReasonFromResponseOutput(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_custom","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3},"output":[{"type":"custom_tool_call","id":"ctc_1","status":"completed","call_id":"call_1","name":"shell","input":"echo ok"}]}}`)
	handler.HandlerChatStream(&completed, dataChan, errChan)

	if finishReason := mustGetFinishReason(t, mustReadChunk(t, dataChan)); finishReason != types.FinishReasonToolCalls {
		t.Fatalf("expected finish_reason=%q, got %q", types.FinishReasonToolCalls, finishReason)
	}
	mustReadProtocolEOF(t, errChan)
}

func TestHandlerChatStreamStopFinishReasonWithoutToolCall(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 2)
	errChan := make(chan error, 1)

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_3","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
	handler.HandlerChatStream(&completed, dataChan, errChan)

	finalChunk := mustReadChunk(t, dataChan)
	finishReason := mustGetFinishReason(t, finalChunk)

	if finishReason != types.FinishReasonStop {
		t.Fatalf("expected finish_reason=%q, got %q", types.FinishReasonStop, finishReason)
	}

	mustReadProtocolEOF(t, errChan)
}

func TestHandlerResponsesStreamIgnoreNonTrackedEventWithKeyword(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)

	raw := `data: {"type":"response.reasoning.delta","delta":{"text":"contains response.completed text"}}`
	line := []byte(raw)
	handler.HandlerResponsesStream(&line, dataChan, errChan)

	select {
	case out := <-dataChan:
		if out != raw {
			t.Fatalf("expected passthrough %q, got %q", raw, out)
		}
	default:
		t.Fatal("expected passthrough data, got none")
	}

	select {
	case err := <-errChan:
		t.Fatalf("unexpected stream error: %v", err)
	default:
	}
}

func TestHandlerResponsesStreamTracksResponsesToolBilling(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 8)
	errChan := make(chan error, 1)

	created := []byte(`data: {"type":"response.created","response":{"tools":[{"type":"web_search_preview","search_context_size":"high"},{"type":"image_generation","model":"gpt-image-2","quality":"auto","size":"auto","partial_images":2}]}}`)
	handler.HandlerResponsesStream(&created, dataChan, errChan)

	webSearchAdded := []byte(`data: {"type":"response.output_item.added","item_id":"ws_1","output_index":0,"item":{"type":"web_search_call","status":"in_progress"}}`)
	handler.HandlerResponsesStream(&webSearchAdded, dataChan, errChan)
	webSearchDone := []byte(`data: {"type":"response.output_item.done","item_id":"ws_1","output_index":0,"item":{"type":"web_search_call","status":"completed","action":{"type":"search"}}}`)
	handler.HandlerResponsesStream(&webSearchDone, dataChan, errChan)

	codeInterpreter := []byte(`data: {"type":"response.output_item.added","item":{"type":"code_interpreter_call"}}`)
	handler.HandlerResponsesStream(&codeInterpreter, dataChan, errChan)

	fileSearch := []byte(`data: {"type":"response.output_item.added","item":{"type":"file_search_call"}}`)
	handler.HandlerResponsesStream(&fileSearch, dataChan, errChan)

	partialImage := []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"img_1","partial_image_index":0,"partial_image_b64":"preview"}`)
	handler.HandlerResponsesStream(&partialImage, dataChan, errChan)
	imageKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-2|high|1024x1024|1")
	if got := handler.Usage.ExtraBilling[imageKey].CallCount; got != 0 {
		t.Fatalf("expected partial image event not to bill before successful completion, got %+v", handler.Usage.ExtraBilling)
	}

	imageGeneration := []byte(`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"img_1","type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}}`)
	handler.HandlerResponsesStream(&imageGeneration, dataChan, errChan)
	completed := []byte(`data: {"type":"response.completed","response":{"status":"completed"}}`)
	handler.HandlerResponsesStream(&completed, dataChan, errChan)

	if got := handler.Usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount; got != 1 {
		t.Fatalf("expected responses stream handler to track web search billing, got %+v", handler.Usage.ExtraBilling)
	}
	for _, unsupported := range []string{types.APIToolTypeCodeInterpreter, types.APIToolTypeFileSearch} {
		if _, exists := handler.Usage.ExtraBilling[types.BuildExtraBillingKey(unsupported, "")]; exists {
			t.Fatalf("unsupported hosted tool %q produced billing evidence: %+v", unsupported, handler.Usage.ExtraBilling)
		}
	}
	if got := handler.Usage.ExtraBilling[imageKey].CallCount; got != 1 {
		t.Fatalf("expected responses stream handler to track completed image generation billing, got %+v", handler.Usage.ExtraBilling)
	}

	select {
	case err := <-errChan:
		t.Fatalf("unexpected stream error: %v", err)
	default:
	}
}

func TestResponsesCustomPreAddAppliesOnceAtItsSourceDialect(t *testing.T) {
	preAddTemperature := `{"pre_add":true,"temperature":0.37}`
	provider := &OpenAIProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{CustomParameter: &preAddTemperature}}}
	envelope, err := commonresponses.ParseRawEnvelope([]byte(`{"model":"o3-pro","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	request := &types.OpenAIResponsesRequest{Model: "o3-pro", Input: "hello"}
	for _, test := range []struct {
		name       string
		dialect    commonresponses.DownstreamDialect
		wantPreAdd bool
	}{
		{name: "native Responses", dialect: commonresponses.DownstreamResponses, wantPreAdd: true},
		{name: "converted Chat", dialect: commonresponses.DownstreamChatCompletions},
	} {
		t.Run(test.name, func(t *testing.T) {
			rawReq := &commonresponses.Request{Operation: commonresponses.ResponsesCreate, Body: envelope, Control: commonresponses.Control{DownstreamDialect: test.dialect}}
			body, apiErr := provider.buildResponsesCreateBody(rawReq, request, false)
			if apiErr != nil {
				t.Fatalf("build Responses body: %v", apiErr)
			}
			_, present := body["temperature"]
			if present != test.wantPreAdd {
				t.Fatalf("temperature presence=%t, want %t: %#v", present, test.wantPreAdd, body)
			}
		})
	}

	preAddChatTool := `{"pre_add":true,"overwrite":true,"tools":[{"type":"function","function":{"name":"wrong-shape"}}]}`
	provider.Channel.CustomParameter = &preAddChatTool
	envelope, err = commonresponses.ParseRawEnvelope([]byte(`{"model":"o3-pro","input":"hello","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	rawReq := &commonresponses.Request{Operation: commonresponses.ResponsesCreate, Body: envelope, Control: commonresponses.Control{DownstreamDialect: commonresponses.DownstreamChatCompletions}}
	body, apiErr := provider.buildResponsesCreateBody(rawReq, request, false)
	if apiErr != nil {
		t.Fatalf("build Chat-converted Responses body: %v", apiErr)
	}
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("converted Responses tools changed shape: %#v", body["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || tool["name"] != "lookup" || tool["function"] != nil {
		t.Fatalf("Chat pre_add was applied a second time after conversion: %#v", tool)
	}
}

func TestHandlerResponsesStreamTracksGAWebSearchBilling(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{Usage: &types.Usage{}, Prefix: "data: ", Model: "gpt-4o"}
	dataChan := make(chan string, 2)
	errChan := make(chan error, 1)

	created := []byte(`data: {"type":"response.created","response":{"tools":[{"type":"web_search","search_context_size":"high"}]}}`)
	handler.HandlerResponsesStream(&created, dataChan, errChan)
	done := []byte(`data: {"type":"response.output_item.done","item_id":"ws_ga","item":{"type":"web_search_call","status":"completed","action":{"type":"search"}}}`)
	handler.HandlerResponsesStream(&done, dataChan, errChan)

	gaKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "high")
	if got := handler.Usage.ExtraBilling[gaKey].CallCount; got != 1 {
		t.Fatalf("expected GA web search service evidence, got %+v", handler.Usage.ExtraBilling)
	}
	if got := handler.Usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount; got != 0 {
		t.Fatalf("GA web search was mislabeled as preview: %+v", handler.Usage.ExtraBilling)
	}
}

func TestHandlerChatStreamDoesNotTrackUnsupportedHostedToolBilling(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 2)
	errChan := make(chan error, 1)

	codeInterpreter := []byte(`data: {"type":"response.output_item.added","item":{"type":"code_interpreter_call"}}`)
	handler.HandlerChatStream(&codeInterpreter, dataChan, errChan)

	fileSearch := []byte(`data: {"type":"response.output_item.added","item":{"type":"file_search_call"}}`)
	handler.HandlerChatStream(&fileSearch, dataChan, errChan)

	if len(handler.Usage.ExtraBilling) != 0 {
		t.Fatalf("unsupported hosted tools produced chat stream billing evidence: %+v", handler.Usage.ExtraBilling)
	}

	select {
	case err := <-errChan:
		t.Fatalf("unexpected stream error: %v", err)
	default:
	}
}

func mustReadChunk(t *testing.T, dataChan <-chan string) types.ChatCompletionStreamResponse {
	t.Helper()

	select {
	case data := <-dataChan:
		var chunk types.ChatCompletionStreamResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("failed to parse stream chunk %q: %v", data, err)
		}
		return chunk
	default:
		t.Fatal("expected stream chunk, got none")
	}

	return types.ChatCompletionStreamResponse{}
}

func mustReadProtocolEOF(t *testing.T, errChan <-chan error) {
	t.Helper()
	select {
	case err := <-errChan:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("terminal signal=%v, want io.EOF", err)
		}
	default:
		t.Fatal("expected protocol terminal signal")
	}
}

func mustGetFinishReason(t *testing.T, chunk types.ChatCompletionStreamResponse) string {
	t.Helper()

	if len(chunk.Choices) == 0 {
		t.Fatal("chunk has no choices")
	}

	finishReason, ok := chunk.Choices[0].FinishReason.(string)
	if !ok {
		t.Fatalf("finish_reason should be string, got %#v", chunk.Choices[0].FinishReason)
	}

	return finishReason
}

func TestCompactResponsesPreservesStructuredInclude(t *testing.T) {
	body, _ := captureCompactRequestBody(t, nil, &types.OpenAIResponsesRequest{
		Model:   "gpt-5",
		Input:   "hello",
		Include: []string{"reasoning.encrypted_content"},
	})

	if _, exists := body["include"]; !exists {
		t.Fatalf("expected compact request body to preserve structured include, got %#v", body)
	}
}

func TestCompactResponsesPreservesExplicitStoreAndCompactFields(t *testing.T) {
	store := false
	body, _ := captureCompactRequestBody(t, nil, &types.OpenAIResponsesRequest{
		Model:                "gpt-5",
		Input:                "hello",
		Instructions:         "summarize",
		PreviousResponseID:   "resp_prev",
		PromptCacheKey:       "cache-key",
		PromptCacheRetention: "7d",
		Store:                &store,
	})

	if stored, exists := body["store"]; !exists || stored != false {
		t.Fatalf("expected compact request body to preserve store=false, got %#v", body["store"])
	}
	if body["model"] != "gpt-5" || body["input"] != "hello" {
		t.Fatalf("expected compact request body to keep core compact fields, got %#v", body)
	}
	if body["instructions"] != "summarize" {
		t.Fatalf("expected compact request body to preserve instructions, got %#v", body["instructions"])
	}
	if body["previous_response_id"] != "resp_prev" {
		t.Fatalf("expected compact request body to preserve previous_response_id, got %#v", body["previous_response_id"])
	}
	if body["prompt_cache_key"] != "cache-key" {
		t.Fatalf("expected compact request body to preserve prompt_cache_key, got %#v", body["prompt_cache_key"])
	}
	if body["prompt_cache_retention"] != "7d" {
		t.Fatalf("expected compact request body to preserve prompt_cache_retention, got %#v", body["prompt_cache_retention"])
	}
}

func TestCompactResponsesPreservesRawKnownAndUnknownFields(t *testing.T) {
	rawBody := []byte(`{"model":"gpt-5","input":"hello","include":["raw-include"],"experimental_feature":"enabled"}`)
	rawMap := make(map[string]interface{})
	if err := json.Unmarshal(rawBody, &rawMap); err != nil {
		t.Fatalf("failed to decode raw body: %v", err)
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(rawBody))
	ctx.Request.Header.Set("Content-Type", "application/json")
	common.SetReusableRequestBodyMap(ctx, rawBody, rawMap)

	body, _ := captureCompactRequestBody(t, func(provider *OpenAIProvider) {
		provider.Channel.AllowExtraBody = true
		provider.SetContext(ctx)
	}, &types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: "hello",
	})

	if _, exists := body["include"]; !exists {
		t.Fatalf("expected compact request body to preserve raw include, got %#v", body)
	}
	if got := body["experimental_feature"]; got != "enabled" {
		t.Fatalf("expected compact request body to preserve unknown extra field, got %#v", got)
	}
}

func TestCompactResponsesCustomParameterCanRestoreIncludeWithPreAdd(t *testing.T) {
	customParameter := `{"pre_add":true,"overwrite":true,"include":["from_custom"]}`
	body, _ := captureCompactRequestBody(t, func(provider *OpenAIProvider) {
		provider.Channel.CustomParameter = &customParameter
	}, &types.OpenAIResponsesRequest{
		Model:   "gpt-5",
		Input:   "hello",
		Include: []string{"from_client"},
	})

	includeValues, ok := body["include"].([]interface{})
	if !ok || len(includeValues) != 1 || includeValues[0] != "from_custom" {
		t.Fatalf("expected custom_parameter to restore include after compact cleanup, got %#v", body["include"])
	}
}

func captureCompactRequestBody(t *testing.T, configure func(*OpenAIProvider), request *types.OpenAIResponsesRequest) (map[string]interface{}, *OpenAIProvider) {
	t.Helper()

	var bodyBytes []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Type:  config.ChannelTypeOpenAI,
		Key:   "sk-test",
		Proxy: &proxy,
	}, server.URL)
	provider.Usage = &types.Usage{}

	if configure != nil {
		configure(provider)
	}

	if _, errWithCode := provider.CompactResponsesForTest(request); errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	requestBody := make(map[string]interface{})
	if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
		t.Fatalf("failed to decode compact request body: %v", err)
	}

	return requestBody, provider
}

func (h *OpenAIResponsesStreamHandler) HandlerChatStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	event := string(*rawLine) + "\n\n"
	h.handleChatStream(rawLine, dataChan, errChan, func() (bool, error) {
		return true, h.ObserveResponsesEvent(event)
	})
}

// ChatSSEHandler converts complete Responses events. Accounting belongs to
// the upstream provider, which supplies the request-local observer.
