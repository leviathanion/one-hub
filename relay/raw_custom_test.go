package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/providerendpoint"
	"one-api/common/requester"
	"one-api/middleware"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

func TestCustomRawRelayKeepsCredentialsAndResponseWire(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			setupRelayTestDB(t, &model.Channel{})
			oldLog := config.LogConsumeEnabled
			config.LogConsumeEnabled = false
			t.Cleanup(func() { config.LogConsumeEnabled = oldLog })
			wire := "\x00opaque-download\xff\n"
			if status == http.StatusBadRequest {
				wire = `{"error":{"message":"upstream rejected value","type":"invalid_request_error","future":{"kept":true}}}`
			}
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.EscapedPath() != "/managed/files/file%2Ftenant/content" || r.URL.RawQuery != "tenant=one&after=a%2Fb&after=c" {
					t.Errorf("unexpected URL: %s", r.URL.String())
				}
				if r.Header.Get("Authorization") != "Bearer channel-secret" || r.Header.Get("Cookie") != "" {
					t.Errorf("client credentials leaked or channel credentials missing: %#v", r.Header)
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != "{ \"future\": 1e3 }" {
					t.Errorf("request body changed: %q", body)
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				if status == http.StatusBadRequest {
					w.Header().Set("Content-Type", "application/json")
				}
				w.Header().Set("Set-Cookie", "upstream-secret=hidden")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(wire))
			}))
			t.Cleanup(upstream.Close)
			oldClient := requester.HTTPClient
			requester.HTTPClient = upstream.Client()
			t.Cleanup(func() { requester.HTTPClient = oldClient })
			proxy, baseURL := "", upstream.URL
			channel := &model.Channel{Id: 95, Type: config.ChannelTypeCustom, Key: "channel-secret", BaseURL: &baseURL, Proxy: &proxy, Plugin: model.NewCustomEndpointPlugin(), Status: config.ChannelStatusEnabled, Group: "default"}
			channel.Plugin.Data()["endpoints"][providerendpoint.Files] = (providerendpoint.Setting{Enabled: true, UpstreamURL: "/managed/files?tenant=one"}).Data()
			if err := model.DB.Create(channel).Error; err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/files/file%2Ftenant/content?after=a%2Fb&after=c", strings.NewReader("{ \"future\": 1e3 }"))
			ctx.Request.Header.Set("Authorization", "Bearer client-secret")
			ctx.Request.Header.Set("Cookie", "client-session=hidden")
			ctx.Set("specific_channel_id", channel.Id)
			RelayOnly(ctx)
			if calls != 1 || recorder.Code != status || recorder.Body.String() != wire || recorder.Header().Get("Set-Cookie") != "" {
				t.Fatalf("calls=%d status=%d wire=%q headers=%#v", calls, recorder.Code, recorder.Body.String(), recorder.Header())
			}
		})
	}
}

func TestCustomRawRelayStillRequiresAdministratorChannelSelection(t *testing.T) {
	engine := gin.New()
	called := false
	engine.GET("/v1/files", middleware.SpecifiedChannel(), func(c *gin.Context) { called = true })
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/files", nil))
	if called || recorder.Code != http.StatusForbidden {
		t.Fatalf("unselected ordinary request reached raw relay: called=%v status=%d", called, recorder.Code)
	}
}
