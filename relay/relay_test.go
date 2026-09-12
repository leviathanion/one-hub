package relay

import (
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	commonRequester "one-api/common/requester"
	"one-api/model"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRawRelayConsumeLogOmitsQueryValues(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/files?cursor=user-data&api_key=sk-secret", nil)
	content := rawRelayConsumeLogContent(ctx)
	if content != "中继:/v1/files" || strings.Contains(content, "user-data") || strings.Contains(content, "sk-secret") {
		t.Fatalf("raw relay log content leaked query: %q", content)
	}
}

func TestRelayOnlySanitizesFailureBodyAndPreservesSuccessfulRawResponses(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		headers    http.Header
		assertBody func(*testing.T, string)
	}{
		{
			name:   "失败响应脱敏",
			status: http.StatusUnauthorized,
			body:   `{"error":{"message":"account acct-secret rejected","type":"authentication_error","code":"invalid_api_key"},"account_id":"acct-secret","api_key":"sk-upstream-secret"}`,
			headers: http.Header{
				"Content-Type": {"application/json"},
				"Set-Cookie":   {"upstream-session=secret"},
			},
			assertBody: func(t *testing.T, body string) {
				t.Helper()
				if !strings.Contains(body, `"message":"account [redacted] rejected"`) || !strings.Contains(body, `"code":"invalid_api_key"`) {
					t.Fatalf("错误消息或协议不正确: %s", body)
				}
			},
		},
		{
			name:   "成功响应保持原始 body",
			status: http.StatusOK,
			body:   "{\n  \"future\": {\"shape\": true}\n}\n",
			headers: http.Header{
				"Content-Type": {"application/json"},
			},
			assertBody: func(t *testing.T, body string) {
				t.Helper()
				if body != "{\n  \"future\": {\"shape\": true}\n}\n" {
					t.Fatalf("successful raw relay body changed: %q", body)
				}
			},
		},
		{
			name:   "普通失败保留错误语义",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"invalid field","type":"invalid_request_error","code":"invalid_value","future":{"kept":true}}}`,
			headers: http.Header{
				"Content-Type": {"application/json"},
			},
			assertBody: func(t *testing.T, body string) {
				t.Helper()
				want := `{"error":{"message":"invalid field","type":"invalid_request_error","code":"invalid_value","future":{"kept":true}}}`
				if body != want {
					t.Fatalf("ordinary provider error wire changed:\nwant %s\n got %s", want, body)
				}
			},
		},
		{
			name:   "重定向保持原始 body",
			status: http.StatusTemporaryRedirect,
			body:   "redirect body\n",
			headers: http.Header{
				"Content-Type": {"text/plain"},
				"Location":     {"https://files.example/download"},
			},
			assertBody: func(t *testing.T, body string) {
				t.Helper()
				if body != "redirect body\n" {
					t.Fatalf("redirect raw relay body changed: %q", body)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setupRelayTestDB(t, &model.Channel{})
			originalLogConsumeEnabled := config.LogConsumeEnabled
			config.LogConsumeEnabled = false
			t.Cleanup(func() {
				config.LogConsumeEnabled = originalLogConsumeEnabled
			})

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for name, values := range test.headers {
					w.Header()[name] = append([]string(nil), values...)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer upstream.Close()
			originalHTTPClient := commonRequester.HTTPClient
			commonRequester.HTTPClient = upstream.Client()
			t.Cleanup(func() {
				commonRequester.HTTPClient = originalHTTPClient
			})

			baseURL := upstream.URL
			proxy := ""
			channel := &model.Channel{
				Id:      61,
				Type:    config.ChannelTypeOpenAI,
				Name:    "raw-relay-test",
				Key:     "sk-proxy-key",
				BaseURL: &baseURL,
				Proxy:   &proxy,
				Status:  config.ChannelStatusEnabled,
				Group:   "default",
			}
			if err := model.DB.Create(channel).Error; err != nil {
				t.Fatalf("persist raw relay channel: %v", err)
			}

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/files", nil)
			ctx.Set("specific_channel_id", channel.Id)

			RelayOnly(ctx)

			if recorder.Code != test.status {
				t.Fatalf("expected status %d, got %d body=%s", test.status, recorder.Code, recorder.Body.String())
			}
			test.assertBody(t, recorder.Body.String())
			if test.status == http.StatusUnauthorized && recorder.Header().Get("Set-Cookie") != "" {
				t.Fatalf("raw relay failure exposed upstream cookie: %#v", recorder.Header())
			}
			if test.status == http.StatusTemporaryRedirect && recorder.Header().Get("Location") != "https://files.example/download" {
				t.Fatalf("raw relay redirect location changed: %#v", recorder.Header())
			}
		})
	}
}

func TestRelayOnlyPreservesKnownUploadContentLength(t *testing.T) {
	setupRelayTestDB(t, &model.Channel{})
	originalLogConsumeEnabled := config.LogConsumeEnabled
	config.LogConsumeEnabled = false
	t.Cleanup(func() { config.LogConsumeEnabled = originalLogConsumeEnabled })

	gotLength := int64(-2)
	var gotTransferEncoding []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLength = r.ContentLength
		gotTransferEncoding = append([]string(nil), r.TransferEncoding...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"file_1"}`))
	}))
	t.Cleanup(upstream.Close)
	originalHTTPClient := commonRequester.HTTPClient
	commonRequester.HTTPClient = upstream.Client()
	t.Cleanup(func() { commonRequester.HTTPClient = originalHTTPClient })

	baseURL := upstream.URL
	proxy := ""
	channel := &model.Channel{Id: 63, Type: config.ChannelTypeOpenAI, Name: "raw-upload-length", Key: "sk-proxy-key", BaseURL: &baseURL, Proxy: &proxy, Status: config.ChannelStatusEnabled, Group: "default"}
	if err := model.DB.Create(channel).Error; err != nil {
		t.Fatalf("persist raw relay channel: %v", err)
	}

	body := "multipart-wire-body"
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/files", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "multipart/form-data; boundary=frozen")
	ctx.Set("specific_channel_id", channel.Id)

	RelayOnly(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("raw upload failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if gotLength != int64(len(body)) || len(gotTransferEncoding) != 0 {
		t.Fatalf("known length changed upstream: length=%d transfer=%v", gotLength, gotTransferEncoding)
	}
}

func TestRelayOnlyRejectsProviderWithoutRawRelayCapability(t *testing.T) {
	setupRelayTestDB(t, &model.Channel{})
	proxy := ""
	baseURL := "https://api.anthropic.com"
	channel := &model.Channel{
		Id:      62,
		Type:    config.ChannelTypeAnthropic,
		Name:    "unsupported-raw-relay",
		Key:     "provider-key",
		BaseURL: &baseURL,
		Proxy:   &proxy,
		Status:  config.ChannelStatusEnabled,
		Group:   "default",
	}
	if err := model.DB.Create(channel).Error; err != nil {
		t.Fatalf("persist unsupported raw relay channel: %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/files", nil)
	ctx.Set("specific_channel_id", channel.Id)

	RelayOnly(ctx)

	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "does not support raw resource relay") {
		t.Fatalf("expected an explicit raw relay capability error, got status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
