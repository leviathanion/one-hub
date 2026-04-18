package surface

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"one-api/common/logger"
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

func TestRecraftContractNormalizesBadResponseStatusCodeAndRequestID(t *testing.T) {
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
	want := "Provider API error: bad response status code 502 (request id: req-local)"
	if payload.Message != want {
		t.Fatalf("unexpected normalized message: got %q want %q", payload.Message, want)
	}
}

func TestRecraftContractReplacesUpstreamRequestID(t *testing.T) {
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
	want := "recraft upstream failed (request id: req-local)"
	if payload.Message != want {
		t.Fatalf("unexpected request id normalization: got %q want %q", payload.Message, want)
	}
}

func TestRecraftContractRewritesTooManyRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	RecraftContract().RenderJSONError(ctx, &SurfaceError{
		StatusCode: http.StatusTooManyRequests,
		Code:       "bad_response_status_code",
		Param:      "429",
		Type:       "upstream_error",
	})

	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Message != "当前分组上游负载已饱和，请稍后再试" {
		t.Fatalf("unexpected 429 rewrite: %+v", payload)
	}
}
