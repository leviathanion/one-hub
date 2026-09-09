package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestCompletionStreamHandlerAcceptsSSEDataWithOptionalSpace(t *testing.T) {
	for _, line := range []string{"data:[DONE]", "data: [DONE]"} {
		t.Run(line, func(t *testing.T) {
			handler := OpenAIStreamHandler{Usage: &types.Usage{}}
			raw := []byte(line)
			dataChan := make(chan string, 1)
			errChan := make(chan error, 1)

			handler.handlerCompletionStream(&raw, dataChan, errChan)

			if string(raw) != string(requester.StreamClosed) {
				t.Fatalf("terminal marker was not consumed: %q", raw)
			}
			if err := <-errChan; !errors.Is(err, io.EOF) {
				t.Fatalf("terminal error = %v, want io.EOF", err)
			}
			select {
			case data := <-dataChan:
				t.Fatalf("terminal marker emitted data %q", data)
			default:
			}
		})
	}
}

func TestExactCompletionPreservesRequestAndResponseWire(t *testing.T) {
	providerResponse := []byte(`{"id":"cmpl_1","object":"text_completion","created":1,"model":"provider-model","choices":[{"text":"ok","index":0,"finish_reason":"stop","logprobs":null}],"system_fingerprint":"fp_1","future":{"kept":true}}`)
	var requestBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(providerResponse)
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	rawRequest := `{"model":"client-model","prompt":"hello","temperature":0,"top_p":0,"seed":42,"future_request":{"kept":true}}`
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewBufferString(rawRequest))
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatalf("cache completion request: %v", err)
	}
	var request types.CompletionRequest
	if err := json.Unmarshal([]byte(rawRequest), &request); err != nil {
		t.Fatalf("decode request projection: %v", err)
	}

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, server.URL)
	provider.SetProviderRawJSONReplay(true)
	provider.SetContext(ctx)
	provider.Usage = &types.Usage{PromptTokens: 1}
	response, apiErr := provider.CreateCompletion(&request)
	if apiErr != nil {
		t.Fatalf("CreateCompletion returned error: %+v", apiErr)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(requestBody, &sent); err != nil {
		t.Fatalf("decode provider request: %v", err)
	}
	for field, want := range map[string]string{"model": `"client-model"`, "temperature": "0", "top_p": "0", "seed": "42", "future_request": `{"kept":true}`} {
		if string(sent[field]) != want {
			t.Fatalf("request field %s=%s want %s (body=%s)", field, sent[field], want, requestBody)
		}
	}
	if got := response.ReplayProviderRawJSON(); !bytes.Equal(got, providerResponse) {
		t.Fatalf("completion response wire changed:\nwant %s\n got %s", providerResponse, got)
	}
	if provider.Usage.ProviderReported {
		t.Fatalf("locally estimated missing usage was marked provider-reported: %+v", provider.Usage)
	}
}

func TestCompletionStreamHandlerAcceptsDataWithoutSpace(t *testing.T) {
	handler := OpenAIStreamHandler{Usage: &types.Usage{PromptTokens: 2}}
	raw := []byte(`data:{"id":"cmpl_1","choices":[{"text":"hello","index":0,"finish_reason":"stop"}]}`)
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)

	handler.handlerCompletionStream(&raw, dataChan, errChan)

	select {
	case data := <-dataChan:
		if data != `{"id":"cmpl_1","choices":[{"text":"hello","index":0,"finish_reason":"stop"}]}` {
			t.Fatalf("completion payload changed: %q", data)
		}
	default:
		t.Fatal("completion payload without a space was dropped")
	}
	select {
	case err := <-errChan:
		t.Fatalf("valid completion payload returned error: %v", err)
	default:
	}
	if handler.Usage.TotalTokens != 2 {
		t.Fatalf("completion usage observation changed: %+v", handler.Usage)
	}
}

func TestCreateCompletionStreamRecognizesDataDoneWithoutSpace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data:[DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, server.URL)
	provider.Usage = &types.Usage{}
	stream, apiErr := provider.CreateCompletionStream(&types.CompletionRequest{Model: "gpt-3.5-turbo-instruct", Prompt: "hello", Stream: true})
	if apiErr != nil {
		t.Fatalf("create completion stream: %v", apiErr)
	}
	t.Cleanup(stream.Close)
	_, errChan := stream.Recv()

	select {
	case err := <-errChan:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream terminal error = %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("completion stream did not recognize data:[DONE]")
	}
}

func TestCreateExactCompletionStreamPreservesRawSSEAndExplicitZeroUsage(t *testing.T) {
	includeObfuscation := true
	rawEvent := "event: completion\r\nid: cmpl-event\r\ndata: {\"id\":\"cmpl_raw\",\"model\":\"gpt-3.5-turbo-instruct\",\"message\":\"future success metadata\",\"code\":\"future_code\",\"choices\":{\"future\":true},\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0},\"future\":true}\r\n\r\n"
	doneEvent := "data: [DONE]\r\n\r\n"
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
	provider.Usage = &types.Usage{PromptTokens: 11, TotalTokens: 11}
	request := &types.CompletionRequest{
		Model: "gpt-3.5-turbo-instruct", Prompt: "hello", Stream: true,
		StreamOptions: &types.StreamOptions{IncludeUsage: true, IncludeObfuscation: &includeObfuscation},
	}
	stream, apiErr := provider.CreateCompletionStream(request)
	if apiErr != nil {
		t.Fatalf("create exact completion stream: %+v", apiErr)
	}
	defer requester.CloseAndDrainStream(stream)
	if !requester.IsRawSSEEventStream(stream) {
		t.Fatal("exact OpenAI Completion stream did not advertise complete raw SSE events")
	}
	data, errs := stream.Recv()
	if got := <-data; got != rawEvent {
		t.Fatalf("raw Completion SSE event changed:\nwant %q\n got %q", rawEvent, got)
	}
	if got := <-data; got != doneEvent {
		t.Fatalf("raw Completion terminal changed: want %q got %q", doneEvent, got)
	}
	if err := <-errs; !errors.Is(err, io.EOF) {
		t.Fatalf("exact Completion terminal error=%v, want EOF", err)
	}
	if provider.Usage.PromptTokens != 0 || provider.Usage.CompletionTokens != 0 || provider.Usage.TotalTokens != 0 || !provider.Usage.ProviderReported {
		t.Fatalf("explicit provider zero usage was overwritten: %+v", provider.Usage)
	}
	var sent struct {
		StreamOptions *types.StreamOptions `json:"stream_options"`
	}
	if err := json.Unmarshal(requestBody, &sent); err != nil || sent.StreamOptions == nil || !sent.StreamOptions.IncludeUsage || sent.StreamOptions.IncludeObfuscation == nil || !*sent.StreamOptions.IncludeObfuscation {
		t.Fatalf("exact Completion stream_options changed: body=%s err=%v", requestBody, err)
	}
}

func TestExactCompletionProviderErrorStopsBeforeLateUsage(t *testing.T) {
	handler := OpenAIStreamHandler{Usage: &types.Usage{}}
	body := "data: {\"error\":{\"message\":\"invalid prompt\",\"type\":\"invalid_request_error\",\"code\":\"invalid_value\"}}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":5,\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"
	stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{
		Body: io.NopCloser(strings.NewReader(body)),
	}, handler.handleExactCompletionSSE, requester.StreamReadOptions{RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatalf("create exact Completion stream: %+v", apiErr)
	}
	defer requester.CloseAndDrainStream(stream)
	data, errs := stream.Recv()
	if got := <-data; !strings.Contains(got, `"code":"invalid_value"`) {
		t.Fatalf("provider error event changed: %q", got)
	}
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "invalid prompt") {
		t.Fatalf("provider error did not terminate Completion producer: %v", err)
	}
	if _, ok := <-data; ok {
		t.Fatal("Completion producer emitted data after provider error")
	}
	if handler.Usage.ProviderReported || handler.Usage.TotalTokens != 0 {
		t.Fatalf("late Completion usage mutated accounting: %+v", handler.Usage)
	}
}

func TestCreateCompletionStreamTerminalRequirementFollowsProviderCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"cmpl_eof","choices":[{"text":"ok","index":0,"finish_reason":"stop"}]}`+"\n\n")
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
			stream, apiErr := provider.CreateCompletionStream(&types.CompletionRequest{Model: "gpt-3.5-turbo-instruct", Prompt: "hello", Stream: true})
			if apiErr != nil {
				t.Fatalf("create completion stream: %+v", apiErr)
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
