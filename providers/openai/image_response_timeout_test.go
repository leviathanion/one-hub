package openai

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

func TestUnaryImageResponseWaitsBeyondStreamHeaderTimeout(t *testing.T) {
	const wire = `{"data":[{"b64_json":"image"}],"future":1e3}`
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(31 * time.Second):
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, wire)
	}))
	defer server.Close()
	old := requester.HTTPClient
	requester.HTTPClient = server.Client()
	requester.HTTPClient.Timeout = 40 * time.Second
	defer func() { requester.HTTPClient = old }()
	proxy, baseURL := "", server.URL
	p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy, BaseURL: &baseURL}, baseURL)
	result, apiErr := p.CreateImageGenerationsResponse(&types.ImageRequest{Model: "image", Prompt: "draw"})
	if apiErr != nil || result == nil || result.JSON == nil {
		t.Fatalf("delayed image response: result=%+v err=%+v", result, apiErr)
	}
	if got := string(result.JSON.ReplayProviderRawJSON()); got != wire || calls.Load() != 1 {
		t.Fatalf("wire=%q calls=%d", got, calls.Load())
	}
}

func TestImageResponseTimeoutFollowsWireStreamIntent(t *testing.T) {
	for _, edit := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			name := "generation"
			if edit {
				name = "multipart edit"
			}
			if stream {
				name += " stream"
			}
			t.Run(name, func(t *testing.T) {
				body := []byte(`{"model":"image","stream":false,"future":{"value":1e3}}`)
				contentType := "application/json"
				if stream {
					body = bytes.Replace(body, []byte("false"), []byte("true"), 1)
				}
				path := "/v1/images/generations"
				if edit {
					path = "/v1/images/edits"
					var form bytes.Buffer
					writer := multipart.NewWriter(&form)
					value := "false"
					if stream {
						value = "true"
					}
					for _, field := range [][2]string{{"model", "image"}, {"stream", value}, {"future", "opaque"}} {
						if err := writer.WriteField(field[0], field[1]); err != nil {
							t.Fatal(err)
						}
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					body, contentType = form.Bytes(), writer.FormDataContentType()
				}
				const wire = "event: future\ndata: {\"future\":true}\n\n"
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					got, err := io.ReadAll(r.Body)
					if err != nil || !bytes.Equal(got, body) {
						t.Errorf("request changed: body=%q err=%v", got, err)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return
					case <-time.After(150 * time.Millisecond):
						io.WriteString(w, wire)
					}
				}))
				defer server.Close()
				old := requester.HTTPClient
				requester.HTTPClient = server.Client()
				requester.HTTPClient.Timeout = 50 * time.Millisecond
				defer func() { requester.HTTPClient = old }()
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
				ctx.Request.Header.Set("Content-Type", contentType)
				if _, err := common.CacheRequestBody(ctx); err != nil {
					t.Fatal(err)
				}
				proxy, baseURL := "", server.URL
				p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy, BaseURL: &baseURL}, baseURL)
				p.SetContext(ctx)
				var result *types.ImageResponseWrapper
				var apiErr *types.OpenAIErrorWithStatusCode
				if edit {
					result, apiErr = p.CreateImageEditsResponse(&types.ImageEditRequest{Model: "image"})
				} else {
					result, apiErr = p.CreateImageGenerationsResponse(&types.ImageRequest{Model: "image"})
				}
				if apiErr != nil || result == nil || result.Stream == nil {
					t.Fatalf("response=%+v err=%+v", result, apiErr)
				}
				defer result.Stream.Body.Close()
				got, err := io.ReadAll(result.Stream.Body)
				if stream {
					if err != nil || string(got) != wire {
						t.Fatalf("stream truncated: body=%q err=%v", got, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
					t.Fatalf("unary request lost configured timeout: body=%q err=%v", got, err)
				}
			})
		}
	}
}
