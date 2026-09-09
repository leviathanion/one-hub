package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/providers/openai"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

type i015HTTPResult struct {
	status        int
	headers       http.Header
	body          []byte
	contentLength int64
}

type i015ProviderOperation func(*gin.Context, *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode

// runI015HTTP uses two real localhost HTTP servers: the provider is the
// upstream server and the Gin handler is the proxy. This keeps response body
// framing in net/http instead of relying on ResponseRecorder behavior.
func runI015HTTP(t *testing.T, path, upstreamBody string, channelType int, exactRaw, nativeRaw bool, operation i015ProviderOperation) i015HTTPResult {
	t.Helper()
	preserveRawHeaders := exactRaw || nativeRaw
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(upstreamBody)))
		if !preserveRawHeaders {
			w.Header().Set("Content-Encoding", "zstd")
			w.Header().Set("Content-Range", "bytes 0-9/10")
			w.Header().Set("Digest", "sha-256=:YWJj:")
			w.Header().Set("Etag", `"provider-v1"`)
		}
		w.Header().Set("Content-Language", "en-US")
		if exactRaw {
			w.Header().Set("Etag", `"provider-v1"`)
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Digest", "sha-256=:YWJj:")
		w.Header().Set("X-Request-Id", "request-i015")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(upstream.Close)

	previousClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	engine := gin.New()
	engine.POST(path, func(c *gin.Context) {
		proxy := ""
		channel := &model.Channel{
			Plugin: model.NewCustomEndpointPlugin(), Type: channelType,
			Key:     "provider-secret-i015",
			BaseURL: i015StringPointer(upstream.URL),
			Proxy:   &proxy,
		}
		provider := openai.CreateOpenAIProvider(channel, upstream.URL)
		provider.SetContext(c)
		provider.SetUsage(&types.Usage{})
		if exactRaw {
			provider.SetProviderRawJSONReplay(true)
			provider.SetOpenAIErrorEnvelopeReplay(true)
		}
		if apiErr := operation(c, provider); apiErr != nil {
			c.JSON(apiErr.StatusCode, gin.H{"error": apiErr.OpenAIError})
		}
	})
	proxy := httptest.NewServer(engine)
	t.Cleanup(proxy.Close)

	request, err := http.NewRequest(http.MethodPost, proxy.URL+path, strings.NewReader(`{"model":"gpt-5","input":"hello"}`))
	if err != nil {
		t.Fatalf("build proxy request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("proxy HTTP request: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read proxy HTTP response: read=%v close=%v", readErr, closeErr)
	}
	return i015HTTPResult{
		status:        response.StatusCode,
		headers:       response.Header.Clone(),
		body:          body,
		contentLength: response.ContentLength,
	}
}

func TestFixI015TypedJSONResponsesDropStaleRepresentationHeaders(t *testing.T) {
	chatBody := `{
  "id":"chatcmpl_i015",
  "object":"chat.completion",
  "created":1,
  "model":"gpt-5",
  "choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
  "future_business":{"long":"` + strings.Repeat("x", 80) + `"}
}`
	responsesBody := `{
  "id":"resp_i015",
  "object":"response",
  "model":"gpt-5",
  "status":"completed",
  "output":[],
  "future_business":{"long":"` + strings.Repeat("x", 80) + `"}
}`
	completionBody := `{
  "id":"cmpl_i015",
  "object":"text_completion",
  "created":1,
  "model":"gpt-5",
  "choices":[{"text":"hello","index":0,"finish_reason":"stop"}],
  "future_business":{"long":"` + strings.Repeat("x", 80) + `"}
}`

	testCases := []struct {
		name       string
		path       string
		body       string
		operation  i015ProviderOperation
		assertBody func(t *testing.T, body []byte)
	}{
		{
			name: "Chat same dialect",
			path: "/v1/chat/completions",
			body: chatBody,
			operation: func(c *gin.Context, provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
				response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{Model: "gpt-5", Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}})
				if apiErr != nil {
					return apiErr
				}
				responseJsonClient(c, response)
				return nil
			},
			assertBody: func(t *testing.T, body []byte) {
				var response types.ChatCompletionResponse
				if err := json.Unmarshal(body, &response); err != nil || len(response.Choices) != 1 || response.Choices[0].Message.Content != "hello" {
					t.Fatalf("Chat body was not valid or lost business content: err=%v body=%s", err, body)
				}
			},
		},
		{
			name:      "Responses create",
			path:      "/v1/responses",
			body:      responsesBody,
			operation: i015ResponsesCreateOperation,
			assertBody: func(t *testing.T, body []byte) {
				var response types.OpenAIResponsesResponses
				if err := json.Unmarshal(body, &response); err != nil || response.Status != types.ResponseStatusCompleted {
					t.Fatalf("Responses body was not valid: err=%v body=%s", err, body)
				}
				if !bytes.Contains(body, []byte(`"future_business"`)) {
					t.Fatalf("unknown Responses business field was dropped: %s", body)
				}
			},
		},
		{
			name:      "Responses compact",
			path:      "/v1/responses/compact",
			body:      responsesBody,
			operation: i015ResponsesCompactOperation,
			assertBody: func(t *testing.T, body []byte) {
				var response types.OpenAIResponsesResponses
				if err := json.Unmarshal(body, &response); err != nil || response.Status != types.ResponseStatusCompleted {
					t.Fatalf("Compact body was not valid: err=%v body=%s", err, body)
				}
				if !bytes.Contains(body, []byte(`"future_business"`)) {
					t.Fatalf("unknown Compact business field was dropped: %s", body)
				}
			},
		},
		{
			name:      "Chat to Responses",
			path:      "/v1/responses",
			body:      chatBody,
			operation: i015ChatToResponsesOperation,
			assertBody: func(t *testing.T, body []byte) {
				var response types.OpenAIResponsesResponses
				if err := json.Unmarshal(body, &response); err != nil || len(response.Output) != 1 || response.Output[0].StringContent() != "hello" {
					t.Fatalf("Chat→Responses body was not valid or lost text: err=%v body=%s", err, body)
				}
			},
		},
		{
			name:      "Responses to Chat",
			path:      "/v1/chat/completions",
			body:      responsesBody,
			operation: i015ResponsesToChatOperation,
			assertBody: func(t *testing.T, body []byte) {
				var response types.ChatCompletionResponse
				if err := json.Unmarshal(body, &response); err != nil || len(response.Choices) != 1 {
					t.Fatalf("Responses→Chat body was not valid: err=%v body=%s", err, body)
				}
			},
		},
		{
			name: "Completion",
			path: "/v1/completions",
			body: completionBody,
			operation: func(c *gin.Context, provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
				response, apiErr := provider.CreateCompletion(&types.CompletionRequest{Model: "gpt-5", Prompt: "hello"})
				if apiErr != nil {
					return apiErr
				}
				responseJsonClient(c, response)
				return nil
			},
			assertBody: func(t *testing.T, body []byte) {
				var response types.CompletionResponse
				if err := json.Unmarshal(body, &response); err != nil || len(response.Choices) != 1 || response.Choices[0].Text != "hello" {
					t.Fatalf("Completion body was not valid or lost text: err=%v body=%s", err, body)
				}
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			result := runI015HTTP(t, testCase.path, testCase.body, config.ChannelTypeAnthropic, false, false, testCase.operation)
			if result.status != http.StatusOK {
				t.Fatalf("proxy returned %d: %s", result.status, result.body)
			}
			if result.contentLength >= 0 && result.contentLength != int64(len(result.body)) {
				t.Fatalf("proxy Content-Length=%d does not match body=%d", result.contentLength, len(result.body))
			}
			if result.headers.Get("X-Request-Id") != "request-i015" {
				t.Fatalf("safe request id header was dropped: %v", result.headers)
			}
			for _, name := range []string{"Content-Encoding", "Content-Range", "Content-Language", "Digest", "Etag"} {
				if value := result.headers.Get(name); value != "" {
					t.Fatalf("rewritten response retained %s=%q: %v", name, value, result.headers)
				}
			}
			testCase.assertBody(t, result.body)
		})
	}
}

func TestFixI015TypedJSONResponseExpansionDropsStaleContentLength(t *testing.T) {
	result := runI015HTTP(t, "/v1/chat/completions", `{"choices":[]}`, config.ChannelTypeAnthropic, false, false, func(c *gin.Context, provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
		response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{Model: "gpt-5"})
		if apiErr != nil {
			return apiErr
		}
		responseJsonClient(c, response)
		return nil
	})
	if result.status != http.StatusOK || !json.Valid(result.body) {
		t.Fatalf("expanded Chat response was not valid HTTP JSON: status=%d body=%s", result.status, result.body)
	}
	if len(result.body) <= len(`{"choices":[]}`) {
		t.Fatalf("fixture did not exercise a larger rewritten response: upstream=%d output=%d", len(`{"choices":[]}`), len(result.body))
	}
	if result.contentLength >= 0 && result.contentLength != int64(len(result.body)) {
		t.Fatalf("expanded Chat Content-Length=%d, body=%d", result.contentLength, len(result.body))
	}
	for _, name := range []string{"Content-Encoding", "Content-Range", "Content-Language", "Digest", "Etag"} {
		if value := result.headers.Get(name); value != "" {
			t.Fatalf("expanded response retained %s=%q: %v", name, value, result.headers)
		}
	}
}

func TestFixI015ExactRawReplayKeepsRepresentationHeadersAndUnknownFields(t *testing.T) {
	body := `{"id":"chatcmpl_i015_raw","object":"chat.completion","created":1,"model":"gpt-5","choices":[],"future_business":{"keep":true}}`
	result := runI015HTTP(t, "/v1/chat/completions", body, config.ChannelTypeOpenAI, true, false, func(c *gin.Context, provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
		response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{Model: "gpt-5"})
		if apiErr != nil {
			return apiErr
		}
		responseJsonClient(c, response)
		return nil
	})
	if result.status != http.StatusOK || !bytes.Equal(result.body, []byte(body)) {
		t.Fatalf("exact raw body changed: status=%d body=%q", result.status, result.body)
	}
	if result.contentLength != int64(len(body)) {
		t.Fatalf("exact raw Content-Length=%d, want %d", result.contentLength, len(body))
	}
	if result.headers.Get("Cache-Control") != "private, no-store" || result.headers.Get("Content-Language") != "en-US" || result.headers.Get("Digest") != "sha-256=:YWJj:" || result.headers.Get("X-Request-Id") != "request-i015" {
		t.Fatalf("exact raw representation/safe headers changed: %v", result.headers)
	}
}

func TestFixI015NativeChatReplayKeepsRepresentationHeaders(t *testing.T) {
	body := `{"id":"chatcmpl_i015_native","object":"chat.completion","created":1,"model":"gpt-5","choices":[],"future_business":{"keep":true}}`
	for _, testCase := range []struct {
		name        string
		channelType int
	}{
		{name: "Azure", channelType: config.ChannelTypeAzure},
		{name: "Custom", channelType: config.ChannelTypeCustom},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := runI015HTTP(t, "/v1/chat/completions", body, testCase.channelType, false, true, func(c *gin.Context, provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
				response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{Model: "gpt-5"})
				if apiErr != nil {
					return apiErr
				}
				responseJsonClient(c, response)
				return nil
			})
			if result.status != http.StatusOK || !bytes.Equal(result.body, []byte(body)) {
				t.Fatalf("native Chat body changed: status=%d body=%q", result.status, result.body)
			}
			if result.contentLength != int64(len(body)) {
				t.Fatalf("native Chat Content-Length=%d, want %d", result.contentLength, len(body))
			}
			if result.headers.Get("Cache-Control") != "private, no-store" || result.headers.Get("Content-Language") != "en-US" || result.headers.Get("Digest") != "sha-256=:YWJj:" || result.headers.Get("X-Request-Id") != "request-i015" {
				t.Fatalf("native Chat representation/safe headers changed: %v", result.headers)
			}
		})
	}
}

func TestFixI015NativeCompletionReplayKeepsRepresentationHeaders(t *testing.T) {
	body := `{"id":"cmpl_i015_native","object":"text_completion","created":1,"model":"gpt-5","choices":[],"future_business":{"keep":true}}`
	for _, testCase := range []struct {
		name        string
		channelType int
	}{
		{name: "Azure", channelType: config.ChannelTypeAzure},
		{name: "Custom", channelType: config.ChannelTypeCustom},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := runI015HTTP(t, "/v1/completions", body, testCase.channelType, false, true, func(c *gin.Context, provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
				response, apiErr := provider.CreateCompletion(&types.CompletionRequest{Model: "gpt-5", Prompt: "hello"})
				if apiErr != nil {
					return apiErr
				}
				responseJsonClient(c, response)
				return nil
			})
			if result.status != http.StatusOK || !bytes.Equal(result.body, []byte(body)) {
				t.Fatalf("native Completion body changed: status=%d body=%q", result.status, result.body)
			}
			if result.contentLength != int64(len(body)) {
				t.Fatalf("native Completion Content-Length=%d, want %d", result.contentLength, len(body))
			}
			if result.headers.Get("Cache-Control") != "private, no-store" || result.headers.Get("Content-Language") != "en-US" || result.headers.Get("Digest") != "sha-256=:YWJj:" || result.headers.Get("X-Request-Id") != "request-i015" {
				t.Fatalf("native Completion representation/safe headers changed: %v", result.headers)
			}
		})
	}
}

func TestFixI015ExactCompletionRedactionInvalidatesRepresentationHeaders(t *testing.T) {
	body := `{"id":"cmpl_i015_secret","object":"text_completion","created":1,"model":"gpt-5","choices":[],"account_id":"acct-i015-secret","future_business":{"keep":true}}`
	result := runI015HTTP(t, "/v1/completions", body, config.ChannelTypeOpenAI, true, false, func(c *gin.Context, provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
		response, apiErr := provider.CreateCompletion(&types.CompletionRequest{Model: "gpt-5", Prompt: "hello"})
		if apiErr != nil {
			return apiErr
		}
		responseJsonClient(c, response)
		return nil
	})
	if result.status != http.StatusOK || !json.Valid(result.body) {
		t.Fatalf("redacted Completion response was not valid HTTP JSON: status=%d body=%s", result.status, result.body)
	}
	if bytes.Contains(result.body, []byte("acct-i015-secret")) || !bytes.Contains(result.body, []byte(`"account_id":"[redacted]"`)) {
		t.Fatalf("provider account metadata was not redacted: %s", result.body)
	}
	if result.headers.Get("Etag") != "" || result.headers.Get("Digest") != "" || result.headers.Get("Content-Encoding") != "" {
		t.Fatalf("redaction retained validators/encoding for a new body: %v", result.headers)
	}
	if result.contentLength >= 0 && result.contentLength != int64(len(result.body)) {
		t.Fatalf("redacted Completion Content-Length=%d, body=%d", result.contentLength, len(result.body))
	}
}

func i015ResponsesRequest(operation commonresponses.Operation) *commonresponses.Request {
	envelope, err := commonresponses.ParseRawEnvelope([]byte(`{"model":"gpt-5","input":"hello"}`))
	if err != nil {
		panic(err)
	}
	return &commonresponses.Request{
		Operation: operation,
		Body:      envelope,
		Control:   commonresponses.Control{DownstreamDialect: commonresponses.DownstreamResponses},
		Model:     "gpt-5",
	}
}

func i015ResponsesCreateOperation(c *gin.Context, provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
	response, apiErr := provider.CreateResponses(context.Background(), i015ResponsesRequest(commonresponses.ResponsesCreate))
	if apiErr != nil {
		return apiErr
	}
	responseJsonClient(c, response)
	return nil
}

func i015ResponsesCompactOperation(c *gin.Context, provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
	response, apiErr := provider.CompactResponses(context.Background(), i015ResponsesRequest(commonresponses.ResponsesCompact))
	if apiErr != nil {
		return apiErr
	}
	responseJsonClient(c, response)
	return nil
}

func i015ChatToResponsesOperation(c *gin.Context, provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
	response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{Model: "gpt-5", Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}})
	if apiErr != nil {
		return apiErr
	}
	responseJsonClient(c, response.ToResponses(&types.OpenAIResponsesRequest{Model: "gpt-5"}))
	return nil
}

func i015ResponsesToChatOperation(c *gin.Context, provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
	response, apiErr := provider.CreateResponses(context.Background(), i015ResponsesRequest(commonresponses.ResponsesCreate))
	if apiErr != nil {
		return apiErr
	}
	responseJsonClient(c, response.ToChat())
	return nil
}

func i015StringPointer(value string) *string {
	return &value
}
