package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	providersBase "one-api/providers/base"

	"github.com/gin-gonic/gin"
)

func TestRelayRecraftAIRendersFlatProviderLookupError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalGetRecraftRawProviderFunc := getRecraftRawProviderFunc
	getRecraftRawProviderFunc = func(c *gin.Context, model string) (providersBase.RawRelayInterface, string, error) {
		return nil, "", errors.New("provider unavailable")
	}
	t.Cleanup(func() {
		getRecraftRawProviderFunc = originalGetRecraftRawProviderFunc
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/recraftAI/v1/styles", strings.NewReader(`{"prompt":"hello"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	RelayRecraftAI(ctx)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected provider lookup failure to return 503, got status=%d body=%s", recorder.Code, recorder.Body.String())
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
		t.Fatalf("expected Recraft relay errors to stay flat, got %s", recorder.Body.String())
	}
	if payload.Code != "provider_not_found" || payload.Message != "provider unavailable" {
		t.Fatalf("unexpected Recraft relay error payload: %+v", payload)
	}
}
