package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/logger"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestRelayRerankLogsLocalRequestErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	core, observedLogs := observer.New(zapcore.ErrorLevel)
	originalLogger := logger.Logger
	logger.Logger = zap.New(core)
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(`{}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	RelayRerank(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request, got status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if observedLogs.Len() != 1 {
		t.Fatalf("expected one local error log, got %d", observedLogs.Len())
	}
	if !strings.Contains(observedLogs.All()[0].Message, "field Model is required") {
		t.Fatalf("unexpected log message: %q", observedLogs.All()[0].Message)
	}
	if !strings.Contains(recorder.Body.String(), "field Model is required") {
		t.Fatalf("expected response body to preserve request error, got %s", recorder.Body.String())
	}
}
