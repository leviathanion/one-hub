package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
)

func TestStoredLifecycleStreamDropsStaleBodyHeadersBeforeDelivery(t *testing.T) {
	const credential = "provider-credential-for-stored-response"
	const futureEvent = "event: future_extension\r\ndata: {\"opaque\":[null,1e0,9007199254740993]}\r\n\r\n"
	const errorEvent = "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"invalid_request\",\"param\":\"input\",\"message\":\"rejected: " + credential + "\"}}\n\n"
	for _, test := range []struct {
		name string
		wire string
	}{
		{name: "first event error", wire: errorEvent},
		{name: "error after delivered event", wire: futureEvent + errorEvent},
		{name: "unmodified extension", wire: futureEvent},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{
				"Content-Type":     {"text/event-stream; charset=utf-8"},
				"Cache-Control":    {"private, no-store"},
				"X-Request-Id":     {"req_stored_stream"},
				"Retry-After":      {"3"},
				"Content-Length":   {strconv.Itoa(len(test.wire))},
				"Content-Encoding": {"identity"},
				"Digest":           {"sha-256=:b3JpZ2luYWw=:"},
				"Etag":             {`"original-body"`},
			}
			engine := gin.New()
			engine.GET("/stream", func(c *gin.Context) {
				requestctx.SetProviderCredentials(c, []string{credential})
				response := &http.Response{
					StatusCode: http.StatusAccepted,
					Header:     headers.Clone(),
					Body:       io.NopCloser(strings.NewReader(test.wire)),
				}
				if apiErr := responseStoredLifecycleClient(c, response, nil, providerresponse.Policy{
					Operation:      providerresponse.OperationResponsesRetrieve,
					DataPath:       providerresponse.DataPathExactWire,
					BodyUnmodified: true,
				}); apiErr != nil {
					t.Errorf("恢复流失败：%+v", apiErr)
				}
			})
			server := httptest.NewServer(engine)
			defer server.Close()
			client := server.Client()
			client.Timeout = 3 * time.Second
			response, err := client.Get(server.URL + "/stream")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("恢复流交付不完整：body=%q err=%v", body, err)
			}
			if want := strings.ReplaceAll(test.wire, credential, "[redacted]"); string(body) != want {
				t.Fatalf("脱敏改变了事件协议或未知字段：want=%q got=%q", want, body)
			}
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("上游状态改变：%d", response.StatusCode)
			}
			for _, name := range []string{"Content-Length", "Content-Encoding", "Digest", "Etag"} {
				if value := response.Header.Get(name); value != "" {
					t.Errorf("恢复流保留了可能失效的 %s=%q", name, value)
				}
			}
			for _, name := range []string{"Content-Type", "Cache-Control", "X-Request-Id", "Retry-After"} {
				if value := response.Header.Get(name); value != headers.Get(name) {
					t.Errorf("必要响应头 %s 改变：want=%q got=%q", name, headers.Get(name), value)
				}
			}
		})
	}
}
