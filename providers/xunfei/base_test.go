package xunfei

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/model"
	"one-api/providers/base"

	"github.com/gin-gonic/gin"
)

func TestXunfeiAPIVersionReadsJSONOther(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/chat", nil)
	provider := &XunfeiProvider{
		BaseProvider: base.BaseProvider{
			Context: ctx,
			Channel: &model.Channel{Other: `{"api_version":"v3.1"}`},
		},
	}

	if got := provider.getAPIVersion("SparkDesk"); got != "v3.1" {
		t.Fatalf("expected JSON Other api_version, got %q", got)
	}
}
