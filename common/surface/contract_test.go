package surface

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"one-api/common/logger"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRecraftContractRendersFlatUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	RecraftContract().RenderJSONError(ctx, &SurfaceError{
		StatusCode: http.StatusBadGateway,
		Code:       "upstream_failed",
		Message:    "recraft upstream failed",
	})

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected upstream status code to be preserved, got %d", recorder.Code)
	}

	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Error != nil {
		t.Fatalf("expected flat Recraft error payload, got %s", recorder.Body.String())
	}
	if payload.Code != "upstream_failed" || payload.Message != "recraft upstream failed" {
		t.Fatalf("unexpected Recraft error payload: %+v", payload)
	}
}

func TestOpenAIContractRendersSafeRawUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	raw := []byte(`{"error":{"message":"invalid field","type":"invalid_request_error","code":"invalid_value","future":{"hint":true}}}`)

	OpenAIContract().RenderJSONError(ctx, &SurfaceError{
		StatusCode:      http.StatusBadRequest,
		Kind:            ErrorKindUpstream,
		RawBody:         raw,
		ResponseHeaders: http.Header{"X-Request-Id": []string{"req-upstream"}},
	})

	if recorder.Code != http.StatusBadRequest || string(recorder.Body.Bytes()) != string(raw) {
		t.Fatalf("expected exact upstream error status/body, got status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Request-Id") != "req-upstream" {
		t.Fatalf("expected upstream request id header, got %#v", recorder.Header())
	}
}

func TestRecraftContractDefaultsTransportCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	RecraftContract().RenderJSONError(ctx, NewTransportError(http.StatusUnsupportedMediaType, `unsupported content encoding "gzip"`))

	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected transport status code to be preserved, got %d", recorder.Code)
	}

	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Code != "invalid_request" {
		t.Fatalf("expected default transport code invalid_request, got %q", payload.Code)
	}
	if payload.Message != `unsupported content encoding "gzip"` {
		t.Fatalf("unexpected transport message: %+v", payload)
	}
}

func TestRecraftContractNormalizesBadResponseStatusCodeAndUsesRequestIDHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set(logger.RequestIdKey, "req-local")

	RecraftContract().RenderJSONError(ctx, &SurfaceError{
		StatusCode: http.StatusBadGateway,
		Code:       "bad_response_status_code",
		Param:      "502",
		Type:       "upstream_error",
	})

	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Code != "bad_response_status_code" {
		t.Fatalf("unexpected code: %+v", payload)
	}
	want := "Provider API error: bad response status code 502"
	if payload.Message != want {
		t.Fatalf("unexpected normalized message: got %q want %q", payload.Message, want)
	}
	if got := recorder.Header().Get(logger.RequestIdKey); got != "req-local" {
		t.Fatalf("expected proxy request id header, got %q", got)
	}
}

func TestRecraftContractPreservesUpstreamRequestIDMessageAndAddsProxyHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set(logger.RequestIdKey, "req-local")

	RecraftContract().RenderJSONError(ctx, &SurfaceError{
		StatusCode: http.StatusBadGateway,
		Code:       "upstream_failed",
		Message:    "recraft upstream failed (request id: upstream-1)",
		Type:       "upstream_error",
	})

	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	want := "recraft upstream failed (request id: upstream-1)"
	if payload.Message != want {
		t.Fatalf("unexpected upstream message mutation: got %q want %q", payload.Message, want)
	}
	if got := recorder.Header().Get(logger.RequestIdKey); got != "req-local" {
		t.Fatalf("expected proxy request id header, got %q", got)
	}
}

func TestRecraftContractPreservesUpstreamTooManyRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	RecraftContract().RenderJSONError(ctx, &SurfaceError{
		StatusCode: http.StatusTooManyRequests,
		Code:       "rate_limit_exceeded",
		Message:    "provider rate limit exceeded",
		Param:      "429",
		Type:       "upstream_error",
	})

	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Message != "provider rate limit exceeded" {
		t.Fatalf("unexpected upstream 429 mutation: %+v", payload)
	}
}

func TestOpenAIContractRewritesOnlyLocalTooManyRequestsAndProtectsProxyRequestIDHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set(logger.RequestIdKey, "req-local")

	OpenAIContract().RenderJSONError(ctx, &SurfaceError{
		StatusCode: http.StatusTooManyRequests,
		Code:       "proxy_rate_limit",
		Message:    "internal limiter detail",
		Type:       "one_hub_error",
		Local:      true,
		Kind:       ErrorKindLocal,
		ResponseHeaders: http.Header{
			logger.RequestIdKey: {"untrusted-upstream-value"},
		},
	})

	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Error.Message != "当前分组上游负载已饱和，请稍后再试" {
		t.Fatalf("expected local 429 normalization, got %+v", payload)
	}
	if got := recorder.Header().Get(logger.RequestIdKey); got != "req-local" {
		t.Fatalf("expected protected proxy request id header, got %q", got)
	}
}

func TestOpenAIContractPreservesFallbackUpstream429Classification(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set(logger.RequestIdKey, "req-local")

	OpenAIContract().RenderJSONError(ctx, &SurfaceError{
		StatusCode: http.StatusTooManyRequests,
		Code:       "rate_limit_exceeded",
		Message:    "rate limit reached for project tier",
		Type:       "rate_limit_error",
		Param:      "requests",
		Kind:       ErrorKindUpstream,
	})

	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Error.Code != "rate_limit_exceeded" || payload.Error.Message != "rate limit reached for project tier" || payload.Error.Type != "rate_limit_error" || payload.Error.Param != "requests" {
		t.Fatalf("expected fallback upstream classification to be preserved, got %+v", payload.Error)
	}
	if strings.Contains(payload.Error.Message, "req-local") {
		t.Fatalf("proxy request id leaked into upstream message: %q", payload.Error.Message)
	}
	if got := recorder.Header().Get(logger.RequestIdKey); got != "req-local" {
		t.Fatalf("expected proxy request id header, got %q", got)
	}
}
