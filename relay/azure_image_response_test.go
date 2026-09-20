package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/azure"
	"one-api/types"
)

func TestAzureImageResponseKeepsAsyncAdapter(t *testing.T) {
	for _, pollFails := range []bool{false, true} {
		name := "success"
		if pollFails {
			name = "poll_failure"
		}
		t.Run(name, func(t *testing.T) {
			var methods []string
			var body string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				methods = append(methods, req.Method)
				w.Header().Set("Content-Type", "application/json")
				if req.Method == http.MethodPost {
					raw, _ := io.ReadAll(req.Body)
					body = string(raw)
					w.Header().Set("operation-location", "http://"+req.Host+"/operations/image-1")
					w.WriteHeader(http.StatusAccepted)
					io.WriteString(w, `{"id":"image-1","status":"notRunning"}`)
					return
				}
				if pollFails {
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":{"code":"poll_failed","message":"cannot retrieve operation"}}`)
					return
				}
				io.WriteString(w, `{"id":"image-1","status":"succeeded","result":{"created":123,"data":[{"url":"https://example.com/image.png"}]}}`)
			}))
			defer server.Close()

			const input = `{"model":"dall-e-2","prompt":"draw","future":{"unknown":[1,true]}}`
			r, recorder := azureImageResponseRelay(t, server, "dall-e-2", input)
			apiErr, done := r.send()
			if !done || strings.Join(methods, ",") != "POST,GET" {
				t.Fatalf("done=%t methods=%v err=%+v", done, methods, apiErr)
			}
			if body != input {
				t.Fatalf("request changed: %s", body)
			}
			if pollFails {
				if apiErr == nil || !apiErr.UpstreamAccepted || apiErr.StatusCode != http.StatusBadRequest {
					t.Fatalf("poll failure can resubmit generation: %+v", apiErr)
				}
				return
			}
			if apiErr != nil || recorder.Code != http.StatusOK {
				t.Fatalf("status=%d err=%+v body=%s", recorder.Code, apiErr, recorder.Body.String())
			}
			var result types.ImageResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Data) != 1 || result.Data[0].URL != "https://example.com/image.png" {
				t.Fatalf("async result not delivered: %s", recorder.Body.String())
			}
		})
	}
}

func TestAzureImageResponseKeepsNativeDelivery(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		t.Run(contentType, func(t *testing.T) {
			wire := `{"created":123,"data":[{"url":"https://example.com/image.png","future":true}],"extension":{"value":1}}`
			if contentType == "text/event-stream" {
				wire = "event: future\ndata: {\"extension\":true}\n\n"
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				w.Header().Set("Content-Type", contentType)
				io.WriteString(w, wire)
			}))
			defer server.Close()
			r, recorder := azureImageResponseRelay(t, server, "dall-e-3", `{"model":"dall-e-3","prompt":"draw"}`)
			apiErr, done := r.send()
			if apiErr != nil || !done || calls != 1 || recorder.Body.String() != wire {
				t.Fatalf("err=%+v done=%t calls=%d body=%s", apiErr, done, calls, recorder.Body.String())
			}
		})
	}
}

func azureImageResponseRelay(t *testing.T, server *httptest.Server, modelName, input string) (*relayImageGenerations, *httptest.ResponseRecorder) {
	t.Helper()
	old := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = old })
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(input))
	ctx.Request.Header.Set("Content-Type", "application/json")
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatal(err)
	}
	proxy, baseURL := "", server.URL
	provider := azure.AzureProviderFactory{}.Create(&model.Channel{
		Type: config.ChannelTypeAzure, Key: "test", Proxy: &proxy, BaseURL: &baseURL,
		Other: `{"api_version":"2024-10-01-preview"}`,
	})
	provider.SetContext(ctx)
	provider.SetUsage(&types.Usage{})
	provider.SetOriginalModel(modelName)
	r := NewRelayImageGenerations(ctx)
	if err := r.setRequest(); err != nil {
		t.Fatal(err)
	}
	r.provider, r.modelName = provider, modelName
	return r, recorder
}
