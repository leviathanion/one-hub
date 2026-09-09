package gemini

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestGeminiUsageInfersOnlyProvableZeroCandidates(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "omitted zero candidate count",
			raw:  `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109}`,
			want: true,
		},
		{
			name: "explicit zero candidate count",
			raw:  `{"promptTokenCount":10,"candidatesTokenCount":0,"thoughtsTokenCount":99,"totalTokenCount":109}`,
			want: true,
		},
		{
			name: "unknown candidate remainder",
			raw:  `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":110}`,
		},
		{
			name: "explicit candidate conflicts with total",
			raw:  `{"promptTokenCount":10,"candidatesTokenCount":1,"thoughtsTokenCount":99,"totalTokenCount":109}`,
		},
		{
			name: "missing prompt count",
			raw:  `{"thoughtsTokenCount":99,"totalTokenCount":99}`,
		},
		{
			name: "missing total count",
			raw:  `{"promptTokenCount":10,"thoughtsTokenCount":99}`,
		},
		{
			name: "negative known count",
			raw:  `{"promptTokenCount":10,"thoughtsTokenCount":-1,"totalTokenCount":9}`,
		},
		{
			name: "cached count exceeds prompt",
			raw:  `{"promptTokenCount":10,"thoughtsTokenCount":99,"cachedContentTokenCount":11,"totalTokenCount":109}`,
		},
		{
			name: "prompt detail exceeds prompt",
			raw:  `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109,"promptTokensDetails":[{"modality":"IMAGE","tokenCount":11}]}`,
		},
		{
			name: "negative candidate detail",
			raw:  `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109,"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":-1}]}`,
		},
		{
			name: "omitted tool total with positive tool detail is uncertain",
			raw:  `{"promptTokenCount":10,"thoughtsTokenCount":103,"totalTokenCount":113,"toolUsePromptTokensDetails":[{"modality":"TEXT","tokenCount":2}]}`,
		},
		{
			name: "explicit zero candidate with unaccounted tool detail is uncertain",
			raw:  `{"promptTokenCount":10,"candidatesTokenCount":0,"thoughtsTokenCount":103,"totalTokenCount":113,"toolUsePromptTokensDetails":[{"modality":"TEXT","tokenCount":2}]}`,
		},
		{
			name: "tool aggregate conflicts with detail",
			raw:  `{"promptTokenCount":10,"candidatesTokenCount":0,"thoughtsTokenCount":103,"toolUsePromptTokenCount":0,"totalTokenCount":113,"toolUsePromptTokensDetails":[{"modality":"TEXT","tokenCount":2}]}`,
		},
		{
			name: "candidate detail contradicts zero",
			raw:  `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109,"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":1}]}`,
		},
		{
			name: "null candidate detail is uncertain",
			raw:  `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109,"candidatesTokensDetails":[null]}`,
		},
		{
			name: "null required count",
			raw:  `{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109,"candidatesTokenCount":null}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var metadata GeminiUsageMetadata
			if err := json.Unmarshal([]byte(test.raw), &metadata); err != nil {
				t.Fatalf("decode Gemini usage: %v", err)
			}
			usage := ConvertOpenAIUsage(&metadata, "gemini-test")
			if usage.HasProviderUsage() != test.want {
				t.Fatalf("Gemini usage evidence=%t, want %t: %+v", usage.HasProviderUsage(), test.want, usage)
			}
			if !test.want && usage.ProviderReported {
				t.Fatalf("invalid/incomplete Gemini usage was marked provider-reported: %+v", usage)
			}
			if test.want && (usage.PromptTokens != 10 || usage.CompletionTokens != 99 || usage.TotalTokens != 109) {
				t.Fatalf("inferred Gemini usage counts changed: %+v", usage)
			}
		})
	}
}

func TestGeminiUsagePreservesToolAndThinkingCountsWhenCandidateIsOmitted(t *testing.T) {
	var metadata GeminiUsageMetadata
	if err := json.Unmarshal([]byte(`{"promptTokenCount":10,"thoughtsTokenCount":99,"toolUsePromptTokenCount":2,"totalTokenCount":111}`), &metadata); err != nil {
		t.Fatalf("decode Gemini usage: %v", err)
	}
	usage := ConvertOpenAIUsage(&metadata, "gemini-test")
	if !usage.HasProviderUsage() || usage.PromptTokens != 12 || usage.CompletionTokens != 99 || usage.TotalTokens != 111 {
		t.Fatalf("tool/thinking usage was not preserved: %+v", usage)
	}
}

const (
	i056GeminiResponse  = `{"candidates":[{"index":0,"content":{"role":"model","parts":[]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109}}`
	i056GeminiCandidate = `{"candidates":[{"index":0,"content":{"role":"model","parts":[]},"finishReason":"MAX_TOKENS"}]}`
	i056GeminiUsage     = `{"usageMetadata":{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109}}`
)

func newI056GeminiChatProvider(t *testing.T, server *httptest.Server) *GeminiProvider {
	t.Helper()
	proxy := ""
	provider := GeminiProviderFactory{}.Create(&model.Channel{
		Type:  config.ChannelTypeGemini,
		Key:   "i056-key",
		Proxy: &proxy,
	}).(*GeminiProvider)
	provider.Config.BaseURL = server.URL
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", nil)
	provider.SetContext(ctx)
	provider.SetUsage(&types.Usage{})
	return provider
}

func withI056HTTPClient(t *testing.T, server *httptest.Server) {
	t.Helper()
	previous := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previous })
}

func newI056ChatRequest(stream bool) *types.ChatCompletionRequest {
	return &types.ChatCompletionRequest{
		Model:  "gemini-2.5-pro",
		Stream: stream,
		Messages: []types.ChatCompletionMessage{{
			Role:    types.ChatMessageRoleUser,
			Content: "hello",
		}},
	}
}

func i056JSONServer(t *testing.T, stream bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+i056GeminiCandidate+"\n\n")
			_, _ = io.WriteString(w, "data: "+i056GeminiUsage+"\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, i056GeminiResponse)
	}))
}

func assertI056Usage(t *testing.T, usage *types.Usage) {
	t.Helper()
	if usage == nil || !usage.HasProviderUsage() {
		t.Fatalf("省略 candidatesTokenCount 后未形成可计费证据: %+v", usage)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 99 || usage.TotalTokens != 109 {
		t.Fatalf("省略 candidatesTokenCount 后用量错误: %+v", usage)
	}
	if amount := int64(usage.PromptTokens) + int64(usage.CompletionTokens); amount != 109 {
		t.Fatalf("固定 input/output 单价 1 下金额=%d，want 109", amount)
	}
}

func collectI056Stream(t *testing.T, stream requester.StreamReaderInterface[string]) error {
	t.Helper()
	dataChan, errChan := stream.Recv()
	defer stream.Close()
	var streamErr error
	for dataChan != nil || errChan != nil {
		select {
		case _, ok := <-dataChan:
			if !ok {
				dataChan = nil
			}
		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err != nil {
				streamErr = err
			}
		}
	}
	return streamErr
}

func TestGeminiI056ChatUnaryHTTPInfersOmittedCandidatesAsZero(t *testing.T) {
	server := i056JSONServer(t, false)
	t.Cleanup(server.Close)
	withI056HTTPClient(t, server)
	provider := newI056GeminiChatProvider(t, server)
	response, apiErr := provider.CreateChatCompletion(newI056ChatRequest(false))
	if apiErr != nil || response == nil {
		t.Fatalf("Gemini Chat unary failed: response=%+v err=%+v", response, apiErr)
	}
	assertI056Usage(t, provider.GetUsage())
}

func TestGeminiI056ChatStreamHTTPInfersOmittedCandidatesAsZero(t *testing.T) {
	server := i056JSONServer(t, true)
	t.Cleanup(server.Close)
	withI056HTTPClient(t, server)
	provider := newI056GeminiChatProvider(t, server)
	stream, apiErr := provider.CreateChatCompletionStream(newI056ChatRequest(true))
	if apiErr != nil || stream == nil {
		t.Fatalf("Gemini Chat stream failed to open: stream=%v err=%+v", stream, apiErr)
	}
	if err := collectI056Stream(t, stream); !errors.Is(err, io.EOF) {
		t.Fatalf("Gemini Chat stream terminal=%v, want EOF", err)
	}
	assertI056Usage(t, provider.GetUsage())
}

func TestGeminiI056NativeUnaryHTTPInfersOmittedCandidatesAsZero(t *testing.T) {
	server := i056JSONServer(t, false)
	t.Cleanup(server.Close)
	withI056HTTPClient(t, server)
	provider, request := newNativeGeminiProviderForTest(t, server, `{"contents":[]}`, false)
	response, apiErr := provider.CreateGeminiChat(request)
	if apiErr != nil || response == nil {
		t.Fatalf("Gemini native unary failed: response=%+v err=%+v", response, apiErr)
	}
	assertI056Usage(t, provider.GetUsage())
}

func TestGeminiI056NativeStreamHTTPInfersOmittedCandidatesAsZero(t *testing.T) {
	wire := "data: " + i056GeminiCandidate + "\n\n" + "data: " + i056GeminiUsage + "\n\n"
	got, streamErr, usage := collectNativeGeminiStream(t, `{"contents":[]}`, wire)
	if got != wire {
		t.Fatalf("Gemini native stream wire changed: got %q, want %q", got, wire)
	}
	if !errors.Is(streamErr, io.EOF) {
		t.Fatalf("Gemini native stream terminal=%v, want EOF", streamErr)
	}
	assertI056Usage(t, usage)
}
