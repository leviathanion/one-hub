package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGeminiUnsupportedActionNeverEntersGeneration(t *testing.T) {
	for _, action := range []string{"countTokens", "embedContent", "batchGenerateContent", "futureAction", ""} {
		t.Run(action, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/gemini/v1beta/models/gemini-test:"+action, strings.NewReader(`{"contents":[]}`))
			ctx.Params = gin.Params{{Key: "model", Value: "gemini-test:" + action}}
			// 完整入口必须在选路、用量准入和 provider work 之前返回原生错误。
			Relay(ctx)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var body struct {
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
					Status  string `json:"status"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Error.Code != http.StatusBadRequest || body.Error.Status == "" || !strings.Contains(body.Error.Message, "not supported") {
				t.Fatalf("missing native unsupported-action error: %s (%v)", recorder.Body.String(), err)
			}
		})
	}
}

func TestGeminiGenerationActionsRetainProjection(t *testing.T) {
	for _, action := range []string{"generateContent", "streamGenerateContent"} {
		t.Run(action, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/gemini/v1beta/models/gemini-test:"+action, strings.NewReader(`{"contents":[],"future":{"shape":true}}`))
			ctx.Params = gin.Params{{Key: "model", Value: "gemini-test:" + action}}
			relay := NewRelayGeminiOnly(ctx)
			if err := relay.setRequest(); err != nil {
				t.Fatal(err)
			}
			if relay.geminiRequest.Model != "gemini-test" || relay.IsStream() != (action == "streamGenerateContent") {
				t.Fatalf("unexpected projection: %+v", relay.geminiRequest)
			}
		})
	}
}
