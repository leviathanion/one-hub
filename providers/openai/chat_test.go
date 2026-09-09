package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestCreateChatCompletionStreamTerminalRequirementFollowsProviderCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl_terminal","model":"gpt-5","choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\n")
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	for _, test := range []struct {
		name            string
		requireTerminal bool
		wantMissing     bool
	}{
		{name: "compatible EOF"},
		{name: "required terminal missing", requireTerminal: true, wantMissing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			proxy := ""
			provider := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "sk-test", Proxy: &proxy}, server.URL)
			provider.RequireOpenAIStreamTerminal = test.requireTerminal
			provider.Usage = &types.Usage{}
			stream, apiErr := provider.CreateChatCompletionStream(&types.ChatCompletionRequest{
				Model: "gpt-5", Stream: true, Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hello"}},
			})
			if apiErr != nil {
				t.Fatalf("create stream: %+v", apiErr)
			}
			if requester.IsRawSSEEventStream(stream) {
				t.Fatal("OpenAI-compatible stream was mislabeled as exact raw SSE")
			}
			data, streamErrs := stream.Recv()
			defer stream.Close()
			select {
			case <-data:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for provider data")
			}
			select {
			case streamErr := <-streamErrs:
				if got := errors.Is(streamErr, requester.ErrStreamProtocolTerminalMissing); got != test.wantMissing {
					t.Fatalf("missing-terminal error=%v, want missing=%t", streamErr, test.wantMissing)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for stream terminal")
			}
		})
	}
}

type cancellationObservingRoundTripper struct {
	requestStarted  chan struct{}
	requestCanceled chan struct{}
}

func (r *cancellationObservingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	close(r.requestStarted)
	<-request.Context().Done()
	close(r.requestCanceled)
	return nil, request.Context().Err()
}

func TestChatRequestsCancelWhileWaitingForProviderHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, stream := range []bool{false, true} {
		name := "non_stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			transport := &cancellationObservingRoundTripper{
				requestStarted:  make(chan struct{}),
				requestCanceled: make(chan struct{}),
			}

			originalHTTPClient := requester.HTTPClient
			requester.HTTPClient = &http.Client{Transport: transport}
			t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

			requestContext, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestContext)

			proxy := ""
			provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, "https://provider.example")
			provider.SetContext(ctx)
			provider.Usage = &types.Usage{}
			request := &types.ChatCompletionRequest{
				Model:    "gpt-5",
				Stream:   stream,
				Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hello"}},
			}

			result := make(chan *types.OpenAIErrorWithStatusCode, 1)
			go func() {
				if stream {
					_, apiErr := provider.CreateChatCompletionStream(request)
					result <- apiErr
					return
				}
				_, apiErr := provider.CreateChatCompletion(request)
				result <- apiErr
			}()

			select {
			case <-transport.requestStarted:
			case <-time.After(time.Second):
				t.Fatal("provider request did not start")
			}
			cancel()
			select {
			case <-transport.requestCanceled:
			case <-time.After(time.Second):
				t.Fatal("provider request did not observe downstream cancellation")
			}
			select {
			case apiErr := <-result:
				if apiErr == nil {
					t.Fatal("canceled provider request unexpectedly succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("provider call remained blocked after downstream cancellation")
			}
		})
	}
}

func TestExactChatStreamPreservesProviderUsageChunk(t *testing.T) {
	handler := OpenAIStreamHandler{Usage: &types.Usage{}}
	stream := newExactChatTestStream(t, &handler, `data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8},"future_usage_field":{"kept":true}}`+"\n\n")
	defer requester.CloseAndDrainStream(stream)
	dataChan, _ := stream.Recv()
	got := <-dataChan
	if !strings.Contains(got, `"future_usage_field":{"kept":true}`) || !strings.Contains(got, `"choices":[]`) {
		t.Fatalf("exact usage chunk changed: %s", got)
	}
	if handler.Usage.TotalTokens != 8 {
		t.Fatalf("usage evidence was not extracted: %+v", handler.Usage)
	}
}

func TestSameDialectChatStreamPreservesRequestedProviderUsageChunk(t *testing.T) {
	handler := OpenAIStreamHandler{Usage: &types.Usage{}, ExposeProviderUsage: true}
	raw := []byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8},"future_usage_field":{"kept":true}}`)
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)
	handler.HandlerChatStream(&raw, dataChan, errChan)

	select {
	case got := <-dataChan:
		if !strings.Contains(got, `"future_usage_field":{"kept":true}`) || !strings.Contains(got, `"choices":[]`) {
			t.Fatalf("same-dialect usage chunk changed: %s", got)
		}
	default:
		t.Fatal("client-requested provider usage chunk was swallowed")
	}
	if handler.Usage.TotalTokens != 8 {
		t.Fatalf("usage evidence was not extracted: %+v", handler.Usage)
	}
}

func TestSameDialectChatStreamHidesInternallyRequestedProviderUsageChunk(t *testing.T) {
	handler := OpenAIStreamHandler{Usage: &types.Usage{}}
	raw := []byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)
	handler.HandlerChatStream(&raw, dataChan, errChan)

	select {
	case got := <-dataChan:
		t.Fatalf("internally requested usage escaped downstream: %s", got)
	default:
	}
	if handler.Usage.TotalTokens != 8 {
		t.Fatalf("internal usage evidence was not extracted: %+v", handler.Usage)
	}
}

func TestExactChatStreamPreservesSanitizedProviderErrorEnvelope(t *testing.T) {
	handler := OpenAIStreamHandler{Usage: &types.Usage{}, ProviderCredential: "provider-secret"}
	stream := newExactChatTestStream(t, &handler, `data: {"error":{"message":"authorization=Bearer provider-secret","api_key":"sk-provider-secret-12345","code":"upstream_failed"}}`+"\n\n")
	defer requester.CloseAndDrainStream(stream)
	dataChan, _ := stream.Recv()
	got := <-dataChan
	if strings.Contains(got, "provider-secret") || !strings.Contains(got, `"code":"upstream_failed"`) {
		t.Fatalf("exact provider error was not safely preserved: %s", got)
	}
}

func TestExactChatStreamRedactsConcreteProviderCredentialInSuccessText(t *testing.T) {
	handler := OpenAIStreamHandler{Usage: &types.Usage{}, ProviderCredential: "provider-secret-123"}
	stream := newExactChatTestStream(t, &handler, `data: {"id":"chatcmpl_1","choices":[{"index":0,"delta":{"content":"provider-secret-123"}}]}`+"\n\n")
	defer requester.CloseAndDrainStream(stream)
	dataChan, _ := stream.Recv()
	got := <-dataChan
	if strings.Contains(got, "provider-secret-123") || !strings.Contains(got, "[redacted]") {
		t.Fatalf("concrete provider credential leaked from success chunk: %s", got)
	}
}

func TestCreateExactChatStreamPreservesRawSSEAndClientStreamOptions(t *testing.T) {
	includeObfuscation := false
	rawEvent := ": keep  two\r\nid: 007\r\nevent: chunk\r\ndata:  {\"id\":\"chatcmpl_raw\",\"model\":\"gpt-5\",\"service_tier\":\"priority\",\"message\":\"future success metadata\",\"code\":\"future_code\",\"choices\":{\"future\":true},\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8},\"future\":12345678901234567890}  \r\n\r\n"
	doneEvent := "data:[DONE]\r\n\r\n"
	var requestBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, rawEvent+doneEvent)
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, server.URL)
	provider.SetProviderRawJSONReplay(true)
	provider.RequireOpenAIStreamTerminal = true
	provider.Usage = &types.Usage{}
	request := &types.ChatCompletionRequest{
		Model: "gpt-5", Stream: true,
		Messages:      []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hello"}},
		StreamOptions: &types.StreamOptions{IncludeUsage: true, IncludeObfuscation: &includeObfuscation},
	}
	stream, apiErr := provider.CreateChatCompletionStream(request)
	if apiErr != nil {
		t.Fatalf("create exact chat stream: %+v", apiErr)
	}
	defer requester.CloseAndDrainStream(stream)
	if !requester.IsRawSSEEventStream(stream) {
		t.Fatal("exact OpenAI Chat stream did not advertise complete raw SSE events")
	}
	data, errs := stream.Recv()
	if got := <-data; got != rawEvent {
		t.Fatalf("raw Chat SSE event changed:\nwant %q\n got %q", rawEvent, got)
	}
	if got := <-data; got != doneEvent {
		t.Fatalf("raw Chat terminal changed: want %q got %q", doneEvent, got)
	}
	if err := <-errs; !errors.Is(err, io.EOF) {
		t.Fatalf("exact Chat terminal error=%v, want EOF", err)
	}
	if provider.Usage.TotalTokens != 8 || !provider.Usage.ProviderReported || provider.Usage.ResponseModel != "gpt-5" || provider.Usage.ServiceTier != "priority" {
		t.Fatalf("stable usage facts were lost behind a future choices union: %+v", provider.Usage)
	}
	var sent struct {
		StreamOptions *types.StreamOptions `json:"stream_options"`
	}
	if err := json.Unmarshal(requestBody, &sent); err != nil || sent.StreamOptions == nil || !sent.StreamOptions.IncludeUsage || sent.StreamOptions.IncludeObfuscation == nil || *sent.StreamOptions.IncludeObfuscation {
		t.Fatalf("exact request stream_options changed: body=%s err=%v", requestBody, err)
	}
	if request.StreamOptions == nil || !request.StreamOptions.IncludeUsage || request.StreamOptions.IncludeObfuscation == nil {
		t.Fatalf("downstream stream_options were not restored: %+v", request.StreamOptions)
	}
}

func TestExactChatProviderErrorStopsBeforeLateUsage(t *testing.T) {
	handler := OpenAIStreamHandler{Usage: &types.Usage{}}
	body := "data: {\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_value\"}}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":9,\"total_tokens\":18}}\n\n" +
		"data: [DONE]\n\n"
	stream := newExactChatTestStream(t, &handler, body)
	defer requester.CloseAndDrainStream(stream)
	data, errs := stream.Recv()
	if got := <-data; !strings.Contains(got, `"code":"invalid_value"`) {
		t.Fatalf("provider error event changed: %q", got)
	}
	streamErr := <-errs
	var providerErr *types.OpenAIErrorWithStatusCode
	if !errors.As(streamErr, &providerErr) || providerErr == nil || providerErr.Code != "invalid_value" {
		t.Fatalf("code-only provider error did not terminate the raw producer: %v", streamErr)
	}
	if _, ok := <-data; ok {
		t.Fatal("raw producer observed or emitted data after the provider error")
	}
	if handler.Usage.ProviderReported || handler.Usage.TotalTokens != 0 {
		t.Fatalf("late usage mutated accounting after provider error: %+v", handler.Usage)
	}
}

func TestExactChatSSEEventLimitRejectsBeforeRawDelivery(t *testing.T) {
	event := "event: chunk\ndata: {\"id\":\"too-large\"}\n\n"
	handler := OpenAIStreamHandler{Usage: &types.Usage{}, sseFramer: requester.NewSSEEventFramer(len(event) - 1)}
	stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{
		Body: io.NopCloser(strings.NewReader(event)),
	}, handler.HandleExactChatSSE, requester.StreamReadOptions{})
	if apiErr != nil {
		t.Fatalf("create exact Chat stream: %+v", apiErr)
	}
	defer requester.CloseAndDrainStream(stream)
	data, errs := stream.Recv()
	if err := <-errs; !errors.Is(err, requester.ErrSSEEventTooLarge) {
		t.Fatalf("oversized exact event error=%v", err)
	}
	if _, ok := <-data; ok {
		t.Fatal("oversized exact event was partially delivered")
	}
}

func newExactChatTestStream(t *testing.T, handler *OpenAIStreamHandler, body string) requester.StreamReaderInterface[string] {
	t.Helper()
	stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{
		Body: io.NopCloser(strings.NewReader(body)),
	}, handler.HandleExactChatSSE, requester.StreamReadOptions{})
	if apiErr != nil {
		t.Fatalf("create exact chat stream: %+v", apiErr)
	}
	return stream
}

func TestCreateChatCompletionOptsInToExactProviderResponseReplay(t *testing.T) {
	providerBody := []byte(`{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2},"future_response_field":{"kept":true}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerBody)
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Type:  config.ChannelTypeOpenAI,
		Key:   "sk-test",
		Proxy: &proxy,
	}, server.URL)
	// The test server stands in for the statically trusted official endpoint.
	provider.SetProviderRawJSONReplay(true)
	provider.Usage = &types.Usage{}

	response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{
		Model:    "gpt-5",
		Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hello"}},
	})
	if apiErr != nil {
		t.Fatalf("CreateChatCompletion returned error: %v", apiErr)
	}
	if got := response.ProviderRawJSON(); !bytes.Equal(got, providerBody) {
		t.Fatalf("expected provider raw response to be retained, got %s", got)
	}
	if got := response.ReplayProviderRawJSON(); !bytes.Equal(got, providerBody) {
		t.Fatalf("expected direct OpenAI adapter to opt in to exact response replay, got %s", got)
	}
}

func TestExactChatPreservesProviderRedirectWithoutFollowingIt(t *testing.T) {
	redirectCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			redirectCalls++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Location", "/target")
		w.Header().Set("X-Request-Id", "req-chat-redirect")
		w.Header().Set("Set-Cookie", "provider-secret")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write([]byte("redirect body\n"))
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	for _, stream := range []bool{false, true} {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, server.URL)
		provider.SetProviderRawJSONReplay(true)
		provider.Usage = &types.Usage{}
		request := &types.ChatCompletionRequest{Model: "gpt-5", Stream: stream}
		var apiErr *types.OpenAIErrorWithStatusCode
		if stream {
			_, apiErr = provider.CreateChatCompletionStream(request)
		} else {
			_, apiErr = provider.CreateChatCompletion(request)
		}
		if apiErr == nil || !apiErr.ReplayRawResponse || apiErr.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("stream=%v: exact redirect was not preserved: %+v", stream, apiErr)
		}
		if string(apiErr.RawBody) != "redirect body\n" || apiErr.ResponseHeaders.Get("Location") != "/target" || apiErr.ResponseHeaders.Get("Set-Cookie") != "" {
			t.Fatalf("stream=%v: exact redirect wire/header policy changed: %+v", stream, apiErr)
		}
	}
	if redirectCalls != 0 {
		t.Fatalf("work action followed provider redirect %d times", redirectCalls)
	}
}

func TestCreateSearchChatStreamRejectsBeforeProviderWorkWithoutExecutionEvidence(t *testing.T) {
	providerCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Type:  config.ChannelTypeOpenAI,
		Key:   "sk-test",
		Proxy: &proxy,
	}, server.URL)
	provider.Usage = &types.Usage{PromptTokens: 3}

	stream, apiErr := provider.CreateChatCompletionStream(&types.ChatCompletionRequest{
		Model:    "gpt-5-search-api",
		Stream:   true,
		Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hello"}},
	})
	if apiErr == nil || apiErr.Code != "chat_search_billing_evidence_unavailable" {
		t.Fatalf("expected pre-work billing evidence rejection, got stream=%v err=%+v", stream, apiErr)
	}
	if providerCalls != 0 {
		t.Fatalf("search request reached provider %d times", providerCalls)
	}
}

func TestOpenAIStreamUsageKeepsEarlierAttributionWhenTerminalFieldsAreEmpty(t *testing.T) {
	usage := &types.Usage{ResponseModel: "actual-model", ServiceTier: "flex"}
	handler := OpenAIStreamHandler{Usage: usage}
	line := []byte(`data: {"id":"chat-1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":0,"total_tokens":5}}`)
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)
	handler.HandlerChatStream(&line, dataChan, errChan)

	if !usage.HasProviderUsage() || usage.ResponseModel != "actual-model" || usage.ServiceTier != "flex" {
		t.Fatalf("usage-only terminal cleared provider attribution: %+v", usage)
	}
}

func TestCustomChatResponsePreservesSafeUnknownFieldsWithoutInventingUsage(t *testing.T) {
	originalApproximate := config.ApproximateTokenEnabled
	config.ApproximateTokenEnabled = true
	t.Cleanup(func() { config.ApproximateTokenEnabled = originalApproximate })
	providerBody := []byte(`{"id":"chatcmpl_custom","object":"chat.completion","model":"gpt-5","account_id":"acct-secret","future_response_field":{"large_integer":9007199254740993},"choices":[{"index":0,"message":{"role":"assistant","content":"hello\n  session token","future_message_field":{"kept":true}},"logprobs":{"content":[{"token":" hello","logprob":-0.1}]},"finish_reason":"stop","future_choice_field":"kept"}]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerBody)
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "sk-test", Proxy: &proxy}, server.URL)
	provider.Usage = &types.Usage{PromptTokens: 2}
	response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{
		Model: "gpt-5", Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hi"}},
	})
	if apiErr != nil {
		t.Fatalf("CreateChatCompletion returned error: %v", apiErr)
	}
	replay := response.ReplayProviderRawJSON()
	for _, expected := range []string{`"future_response_field"`, `9007199254740993`, `"future_message_field"`, `"future_choice_field"`} {
		if !bytes.Contains(replay, []byte(expected)) {
			t.Fatalf("compatible response lost %s: %s", expected, replay)
		}
	}
	if bytes.Contains(replay, []byte("acct-secret")) || !bytes.Contains(replay, []byte(`"account_id":"[redacted]"`)) {
		t.Fatalf("compatible response replay was not sanitized: %s", replay)
	}
	if bytes.Contains(replay, []byte(`"usage"`)) {
		t.Fatalf("compatible response invented public usage from a local estimate: %s", replay)
	}
	var replayObject map[string]any
	if err := json.Unmarshal(replay, &replayObject); err != nil {
		t.Fatalf("decode compatible replay: %v", err)
	}
	choice := replayObject["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "hello\n  session token" {
		t.Fatalf("compatible response content changed: %#v", message["content"])
	}
	logprobs := choice["logprobs"].(map[string]any)
	token := logprobs["content"].([]any)[0].(map[string]any)["token"]
	if token != " hello" {
		t.Fatalf("compatible response logprobs token changed: %#v", token)
	}
}

func TestSafeCompatibleChatResponseReplayMergesOwnedUsageFields(t *testing.T) {
	raw := []byte(`{"id":"chatcmpl_usage","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"future_usage_field":{"kept":true}},"choices":[]}`)
	response := &types.ChatCompletionResponse{
		ID: "chatcmpl_usage", Usage: &types.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
	}
	replay, err := safeCompatibleChatResponseReplay(raw, response, true)
	if err != nil {
		t.Fatalf("safe compatible replay: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(replay, &object); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(object["usage"], &usage); err != nil {
		t.Fatalf("decode replay usage: %v", err)
	}
	if string(usage["prompt_tokens"]) != "3" || string(usage["completion_tokens"]) != "5" || string(usage["total_tokens"]) != "8" || string(usage["future_usage_field"]) != `{"kept":true}` {
		t.Fatalf("owned usage merge lost provider fields: %s", object["usage"])
	}
}
