package codex

import (
	"context"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/http/httptest"
	"one-api/common/cache"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/types"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderRedactionCodexReloadRetainsCredentialSnapshot(t *testing.T) {
	cache.InitCacheManager()
	const secret = "reloaded-provider-secret-12345"
	oldDB := model.DB
	model.DB = nil
	defer func() { model.DB = oldDB }()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("auth=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\""+secret+"\"}\n\n")
	}))
	defer upstream.Close()
	oldClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	defer func() { requester.HTTPClient = oldClient }()
	expiry := time.Now().Add(time.Hour)
	oldKey, _ := (&OAuth2Credentials{AccessToken: "old-provider-token", RefreshToken: "refresh-token", ExpiresAt: expiry}).ToJSON()
	nextKey, _ := (&OAuth2Credentials{AccessToken: secret, RefreshToken: "refresh-token", ExpiresAt: expiry}).ToJSON()
	baseURL := upstream.URL
	proxy := ""
	channel := &model.Channel{Id: 982344, Key: oldKey, BaseURL: &baseURL, Proxy: &proxy}
	p := CodexProviderFactory{}.Create(channel).(*CodexProvider)
	oldLoad := loadLatestChannelByID
	loadLatestChannelByID = func(context.Context, int) (*model.Channel, error) {
		next := *channel
		next.Key = nextKey
		return &next, nil
	}
	defer func() { loadLatestChannelByID = oldLoad }()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set("self_hosted", true)
	p.SetContext(c)
	p.SetUsage(&types.Usage{})
	if p.Requester.ObserveRequest == nil {
		t.Fatal("fixture did not bind callback")
	}
	rawReq, err := p.rawResponsesRequestForTest(&types.OpenAIResponsesRequest{Model: "gpt-5", Stream: true, Input: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	stream, apiErr := p.CreateResponsesStream(context.Background(), rawReq)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	defer requester.CloseAndDrainStream(stream)
	data, errs := stream.Recv()
	var raw strings.Builder
	for data != nil || errs != nil {
		select {
		case chunk, ok := <-data:
			if !ok {
				data = nil
			} else {
				raw.WriteString(chunk)
			}
		case _, ok := <-errs:
			if !ok {
				errs = nil
			}
		}
	}
	payload, hasData := commonresponses.SSEDataPayload(raw.String())
	if !hasData || !strings.Contains(payload, secret) || calls.Load() != 1 || p.Requester.Context != c.Request.Context() {
		t.Fatalf("真实上游事件、单次执行或上下文继承不成立: raw=%q calls=%d", raw.String(), calls.Load())
	}

	safe, changed := providerresponse.SanitizeErrorPayload([]byte(payload), requestctx.ProviderCredentials(c)...)
	if changed || string(safe) != payload {
		t.Fatalf("普通事件被改写: %s", safe)
	}
	diagnostic := []byte(`{"error":{"message":"` + secret + `"}}`)
	safe, changed = providerresponse.SanitizeErrorPayload(diagnostic, requestctx.ProviderCredentials(c)...)
	if !changed || strings.Contains(string(safe), secret) {
		t.Fatalf("错误未使用实际连接凭据: %s", safe)
	}
}
