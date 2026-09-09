package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/gemini"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestIssue012NativeRelayProtoJSON(t *testing.T) {
	const raw = `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generation_config":{"candidate_count":2},"future_extension":{"enabled":true}}`
	const wire = "data: {\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"text\":\"first\"}]},\"finishReason\":\"STOP\"}]}\n\n" +
		"data: {\"candidates\":[{\"index\":1,\"content\":{\"parts\":[{\"text\":\"second\"}]},\"finishReason\":\"STOP\"}]}\n\n" +
		"data: {\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"totalTokenCount\":5}}\n\n"
	var observed []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		observed, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取上游请求失败: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}))
	defer upstream.Close()
	old := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	defer func() { requester.HTTPClient = old }()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/gemini/v1beta/models/gemini-test:streamGenerateContent", strings.NewReader(raw))
	c.Params = gin.Params{{Key: "model", Value: "gemini-test:streamGenerateContent"}}
	if _, err := common.CacheRequestBody(c); err != nil {
		t.Fatal(err)
	}
	r := NewRelayGeminiOnly(c)
	if err := r.setRequest(); err != nil {
		t.Fatal(err)
	}
	proxy := ""
	p := gemini.GeminiProviderFactory{}.Create(&model.Channel{Type: config.ChannelTypeGemini, Key: "key", Proxy: &proxy}).(*gemini.GeminiProvider)
	p.Config.BaseURL = upstream.URL
	p.SetContext(c)
	p.SetUsage(&types.Usage{})
	p.SetOriginalModel("gemini-test")
	r.provider = p
	r.modelName = "gemini-test"
	err, _ := r.send()
	if string(observed) != raw {
		t.Fatalf("请求 wire 被改变: %s", observed)
	}
	if err != nil {
		t.Fatalf("完整原生流被判为失败: %+v", err)
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != wire {
		t.Fatalf("客户端流不完整或被追加错误: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if usage := p.GetUsage(); !usage.ProviderReported || usage.PromptTokens != 3 || usage.CompletionTokens != 2 || usage.TotalTokens != 5 {
		t.Fatalf("完整流的供应商用量丢失: %+v", usage)
	}
}
