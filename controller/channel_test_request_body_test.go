package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/antigravity"
)

func TestAntigravityChannelProbeSendsChatRequest(t *testing.T) {
	for _, upstreamStatus := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(http.StatusText(upstreamStatus), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1internal:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" {
					t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL)
				}
				var body struct {
					Model   string `json:"model"`
					Request struct {
						Contents []struct {
							Role  string `json:"role"`
							Parts []struct {
								Text string `json:"text"`
							} `json:"parts"`
						} `json:"contents"`
					} `json:"request"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode upstream request: %v", err)
				}
				if body.Model != "gemini-3.8-flash-high" || len(body.Request.Contents) != 1 {
					t.Errorf("unexpected probe request: %+v", body)
				} else if message := body.Request.Contents[0]; message.Role != "user" || len(message.Parts) != 1 || message.Parts[0].Text != "You just need to output 'hi' next." {
					t.Errorf("unexpected probe message: %+v", message)
				}
				if upstreamStatus != http.StatusOK {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(upstreamStatus)
					_, _ = io.WriteString(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"probe quota exhausted"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}}\n\n")
			}))
			defer server.Close()
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			credentials := &antigravity.Credentials{
				AccessToken: "probe-access", RefreshToken: "probe-refresh",
				ExpiresAt: time.Now().Add(time.Hour).Unix(), Subject: "probe-subject", ProjectID: "probe-project",
			}
			key, err := credentials.ToJSON()
			if err != nil {
				t.Fatal(err)
			}
			baseURL, proxy, mapping := server.URL, "", `{"probe-alias":"gemini-3.8-flash-high"}`
			apiErr, err := testChannel(&model.Channel{
				Type: config.ChannelTypeAntigravity, Key: key, BaseURL: &baseURL,
				Proxy: &proxy, TestModel: "probe-alias", ModelMapping: &mapping,
			}, "")
			if upstreamStatus == http.StatusOK {
				if apiErr != nil || err != nil {
					t.Fatalf("probe failed: apiErr=%+v err=%v; upstream requests=%d", apiErr, err, requests.Load())
				}
			} else if apiErr == nil || err == nil || apiErr.StatusCode != upstreamStatus || apiErr.Param != "RESOURCE_EXHAUSTED" || apiErr.Message != "Provider API error: probe quota exhausted" || err.Error() != apiErr.Message {
				t.Fatalf("upstream error lost: apiErr=%+v err=%v", apiErr, err)
			}
			if requests.Load() != 1 {
				t.Fatalf("expected exactly one upstream request, got %d", requests.Load())
			}
		})
	}
}

func TestChannelProbeSuppliesMappedRequestBody(t *testing.T) {
	for _, tc := range []struct {
		name        string
		channelType int
		model       string
		path        string
		field       string
		value       any
		custom      string
		response    string
	}{
		{
			name: "chat with custom parameters", channelType: config.ChannelTypeOpenAI,
			model: "chat-alias", path: "/v1/chat/completions", field: "messages",
			value:    []any{map[string]any{"role": "user", "content": "You just need to output 'hi' next."}},
			custom:   `{"temperature":0.25,"future_option":{"value":true}}`,
			response: `{"id":"chat_probe","object":"chat.completion","model":"mapped-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
		},
		{
			name: "embeddings", channelType: config.ChannelTypeOpenAI,
			model: "text-embedding-alias", path: "/v1/embeddings", field: "input", value: "hi",
			response: `{"object":"list","model":"mapped-model","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}]}`,
		},
		{
			name: "image using raw body", channelType: config.ChannelTypeRecraft,
			model: "image-alias", path: "/v1/images/generations", field: "prompt", value: "A cute cat",
			response: `{"created":1,"data":[{"url":"https://example.com/probe.png"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream request: %v", err)
				}
				if r.Method != http.MethodPost || r.URL.Path != tc.path || r.ContentLength != int64(len(raw)) {
					t.Errorf("unexpected upstream request: %s %s length=%d body=%s", r.Method, r.URL, r.ContentLength, raw)
				}
				var body map[string]any
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Errorf("decode upstream request: %v", err)
				}
				if body["model"] != "mapped-model" || !reflect.DeepEqual(body[tc.field], tc.value) {
					t.Errorf("unexpected probe body: %s", raw)
				}
				if tc.custom != "" && (body["temperature"] != 0.25 || !reflect.DeepEqual(body["future_option"], map[string]any{"value": true})) {
					t.Errorf("custom parameters lost: %s", raw)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			baseURL, proxy := server.URL, ""
			mapping := `{"` + tc.model + `":"mapped-model"}`
			apiErr, err := testChannel(&model.Channel{
				Type: tc.channelType, Key: "probe-key", BaseURL: &baseURL, Proxy: &proxy,
				ModelMapping: &mapping, CustomParameter: &tc.custom,
			}, tc.model)
			if apiErr != nil || err != nil || requests.Load() != 1 {
				t.Fatalf("probe failed: apiErr=%+v err=%v requests=%d", apiErr, err, requests.Load())
			}
		})
	}
}
