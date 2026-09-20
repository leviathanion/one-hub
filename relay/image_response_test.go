package relay

import (
	"fmt"
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
	"one-api/providers/openai"
	"one-api/types"
)

func TestNativeImageSSEPreservesWireAndSettlesOnce(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(fmt.Sprintf("edit=%t", edit), func(t *testing.T) {
			event := "image_generation"
			path := "/v1/images/generations"
			if edit {
				event = "image_edit"
				path = "/v1/images/edits"
			}
			done := fmt.Sprintf("event: %s.completed\ndata: {\"type\":\"%s.completed\",\"b64_json\":\"final\"}\n\n", event, event)
			wire := fmt.Sprintf(": comment\r\nid: 1\r\nretry: 20\r\nevent: %s.partial_image\r\ndata: {\"type\":\"%s.partial_image\",\"b64_json\":\"preview\"}\r\n\r\n", event, event) + done + done + "event: extension\ndata: {\"future\":true}\n\n"
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, wire)
			}))
			defer server.Close()
			old := requester.HTTPClient
			requester.HTTPClient = server.Client()
			defer func() { requester.HTTPClient = old }()
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"`+issue053ImageModel+`","prompt":"draw","stream":true}`))
			ctx.Request.Header.Set("Content-Type", "application/json")
			if _, err := common.CacheRequestBody(ctx); err != nil {
				t.Fatal(err)
			}
			proxy := ""
			baseURL := server.URL
			p := openai.CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "test", Proxy: &proxy, BaseURL: &baseURL}, server.URL)
			p.SetContext(ctx)
			p.SetUsage(&types.Usage{})
			var apiErr *types.OpenAIErrorWithStatusCode
			var doneWork bool
			if edit {
				r := NewRelayImageEdits(ctx)
				if err := r.setRequest(); err != nil {
					t.Fatal(err)
				}
				r.provider = p
				r.modelName = issue053ImageModel
				apiErr, doneWork = r.send()
			} else {
				r := NewRelayImageGenerations(ctx)
				if err := r.setRequest(); err != nil {
					t.Fatal(err)
				}
				r.provider = p
				r.modelName = issue053ImageModel
				apiErr, doneWork = r.send()
			}
			if apiErr != nil || !doneWork || calls != 1 {
				t.Fatalf("err=%+v done=%t calls=%d", apiErr, doneWork, calls)
			}
			if recorder.Body.String() != wire {
				t.Fatalf("SSE changed: %q", recorder.Body.String())
			}
			if p.GetUsage().ProviderOperationUnits == nil || *p.GetUsage().ProviderOperationUnits != 1 {
				t.Fatalf("partial or duplicate usage: %+v", p.GetUsage())
			}
			issue053SettleImageUsage(t, p.GetUsage(), 1)
		})
	}
}

func TestNativeImageServerFailureIsNotReplayable(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"message":"work may already have run","type":"server_error"}}`)
	}))
	defer server.Close()
	old := requester.HTTPClient
	requester.HTTPClient = server.Client()
	defer func() { requester.HTTPClient = old }()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"image","stream":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	proxy := ""
	baseURL := server.URL
	p := openai.CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "test", Proxy: &proxy, BaseURL: &baseURL}, server.URL)
	p.SetContext(ctx)
	p.SetUsage(&types.Usage{})
	r := NewRelayImageGenerations(ctx)
	if err := r.setRequest(); err != nil {
		t.Fatal(err)
	}
	r.provider = p
	r.modelName = "image"
	apiErr, done := r.send()
	if apiErr == nil || !apiErr.UpstreamAmbiguous || !done || calls != 1 {
		t.Fatalf("ambiguous generation can retry: err=%+v done=%t calls=%d", apiErr, done, calls)
	}
}
