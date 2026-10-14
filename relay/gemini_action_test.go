package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/gemini"
	"one-api/types"
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

func TestGeminiIngressPreservesFutureUnionWithoutTypedAdmission(t *testing.T) {
	raw := `{ "contents":{"future_union":true}, "generationConfig":{"future":1e3}, "large":9007199254740993 }`
	router := gin.New()
	router.POST("/gemini/:version/models/:model", func(c *gin.Context) {
		r := NewRelayGeminiOnly(c)
		if err := r.setRequest(); err != nil {
			t.Errorf("raw ingress rejected future union: %v", err)
			c.Status(400)
			return
		}
		if _, err := r.geminiRequest.Projection(); err == nil {
			t.Error("fixture must exceed typed projection")
		}
		if err := currentRequestChannelCapability(c)(&model.Channel{Type: config.ChannelTypeGemini}); err != nil {
			t.Errorf("same-dialect candidate rejected raw request: %v", err)
		}
		c.Data(200, "application/json", r.geminiRequest.Raw)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/gemini/v1beta/models/gemini-test:generateContent", strings.NewReader(raw)))
	if recorder.Code != 200 || recorder.Body.String() != raw {
		t.Fatalf("ingress changed raw: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGeminiHTTPIngressForwardsFutureUnionToNativeProvider(t *testing.T) {
	raw := `{ "contents":{"future_union":true}, "generationConfig":{"future":1e3}, "large":9007199254740993 }`
	response := `{ "candidates":{"future_union":true}, "future":1e3 }`
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		upstreamBody, _ = io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(upstream.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	proxy, baseURL := "", upstream.URL
	channel := &model.Channel{Type: config.ChannelTypeGemini, Key: "test-key", Proxy: &proxy, BaseURL: &baseURL}
	router := gin.New()
	router.POST("/gemini/:version/models/:model", func(c *gin.Context) {
		r := NewRelayGeminiOnly(c)
		if err := r.setRequest(); err != nil {
			t.Error(err)
			c.Status(400)
			return
		}
		if err := currentRequestChannelCapability(c)(channel); err != nil {
			t.Error(err)
			c.Status(400)
			return
		}
		provider := (gemini.GeminiProviderFactory{}).Create(channel)
		provider.SetContext(c)
		provider.SetUsage(&types.Usage{})
		r.provider, r.modelName = provider, "gemini-test"
		if err := r.prepareSelectedProviderRemoteMedia(); err != nil {
			t.Error(err)
			c.Status(500)
			return
		}
		if err, _ := r.send(); err != nil {
			t.Errorf("native send: %+v", err)
			c.Status(500)
		}
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/gemini/v1beta/models/gemini-test:generateContent", strings.NewReader(raw)))
	if recorder.Code != 200 || string(upstreamBody) != raw || recorder.Body.String() != response {
		t.Fatalf("native HTTP wire changed: status=%d request=%s response=%s", recorder.Code, upstreamBody, recorder.Body.String())
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
