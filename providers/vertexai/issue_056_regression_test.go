package vertexai

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/gemini"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const (
	i056VertexProjectID = "i056-gemini-usage-project"
	i056VertexResponse  = `{"candidates":[{"index":0,"content":{"role":"model","parts":[]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109}}`
	i056VertexCandidate = `{"candidates":[{"index":0,"content":{"role":"model","parts":[]},"finishReason":"MAX_TOKENS"}]}`
	i056VertexUsage     = `{"usageMetadata":{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109}}`
)

func newI056VertexProvider(t *testing.T, server *httptest.Server) *VertexAIProvider {
	t.Helper()
	cache.InitCacheManager()
	cacheKey := TokenCacheKey + ":" + i056VertexProjectID
	if err := cache.SetCache(cacheKey, "i056-test-token", time.Minute); err != nil {
		t.Fatalf("seed Vertex token cache: %v", err)
	}
	t.Cleanup(func() { _ = cache.DeleteCache(cacheKey) })

	proxy := ""
	provider := (VertexAIProviderFactory{}).Create(&model.Channel{
		Type:  config.ChannelTypeVertexAI,
		Key:   `{}`,
		Proxy: &proxy,
		Other: `{"region":"global","project_id":"` + i056VertexProjectID + `"}`,
	}).(*VertexAIProvider)
	provider.Config.BaseURL = server.URL + "/%s/%s/%s/%s:%s"
	provider.SetUsage(&types.Usage{})
	return provider
}

func withI056VertexHTTPClient(t *testing.T, server *httptest.Server) {
	t.Helper()
	previous := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previous })
}

func i056VertexServer(t *testing.T, stream bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+i056VertexCandidate+"\n\n")
			_, _ = io.WriteString(w, "data: "+i056VertexUsage+"\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, i056VertexResponse)
	}))
}

func assertI056VertexUsage(t *testing.T, usage *types.Usage) {
	t.Helper()
	if usage == nil || !usage.HasProviderUsage() {
		t.Fatalf("Vertex omitted candidatesTokenCount did not become priceable evidence: %+v", usage)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 99 || usage.TotalTokens != 109 {
		t.Fatalf("Vertex omitted candidatesTokenCount usage changed: %+v", usage)
	}
	if amount := int64(usage.PromptTokens) + int64(usage.CompletionTokens); amount != 109 {
		t.Fatalf("fixed input/output unit price 1 produced amount=%d, want 109", amount)
	}
}

func collectI056VertexStream(t *testing.T, stream requester.StreamReaderInterface[string]) error {
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

func newI056VertexNativeRequest(t *testing.T, provider *VertexAIProvider, raw string, stream bool) *gemini.GeminiChatRequest {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/projects/test/locations/global", strings.NewReader(raw))
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatalf("cache Vertex Gemini request: %v", err)
	}
	request := &gemini.GeminiChatRequest{}
	if err := common.UnmarshalBodyReusable(ctx, request); err != nil {
		t.Fatalf("decode Vertex Gemini request: %v", err)
	}
	request.Model = "gemini-2.5-pro"
	request.Stream = stream
	provider.SetContext(ctx)
	provider.SetOriginalModel(request.Model)
	return request
}

func TestVertexI056ChatUnaryHTTPInfersOmittedCandidatesAsZero(t *testing.T) {
	server := i056VertexServer(t, false)
	t.Cleanup(server.Close)
	withI056VertexHTTPClient(t, server)
	provider := newI056VertexProvider(t, server)
	response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{
		Model: "gemini-2.5-pro",
		Messages: []types.ChatCompletionMessage{{
			Role:    types.ChatMessageRoleUser,
			Content: "hello",
		}},
	})
	if apiErr != nil || response == nil {
		t.Fatalf("Vertex Chat unary failed: response=%+v err=%+v", response, apiErr)
	}
	assertI056VertexUsage(t, provider.GetUsage())
}

func TestVertexI056ChatStreamHTTPInfersOmittedCandidatesAsZero(t *testing.T) {
	server := i056VertexServer(t, true)
	t.Cleanup(server.Close)
	withI056VertexHTTPClient(t, server)
	provider := newI056VertexProvider(t, server)
	stream, apiErr := provider.CreateChatCompletionStream(&types.ChatCompletionRequest{
		Model:  "gemini-2.5-pro",
		Stream: true,
		Messages: []types.ChatCompletionMessage{{
			Role:    types.ChatMessageRoleUser,
			Content: "hello",
		}},
	})
	if apiErr != nil || stream == nil {
		t.Fatalf("Vertex Chat stream failed to open: stream=%v err=%+v", stream, apiErr)
	}
	if err := collectI056VertexStream(t, stream); !errors.Is(err, io.EOF) {
		t.Fatalf("Vertex Chat stream terminal=%v, want EOF", err)
	}
	assertI056VertexUsage(t, provider.GetUsage())
}

func TestVertexI056NativeUnaryHTTPInfersOmittedCandidatesAsZero(t *testing.T) {
	server := i056VertexServer(t, false)
	t.Cleanup(server.Close)
	withI056VertexHTTPClient(t, server)
	provider := newI056VertexProvider(t, server)
	request := newI056VertexNativeRequest(t, provider, `{"contents":[]}`, false)
	response, apiErr := provider.CreateGeminiChat(request)
	if apiErr != nil || response == nil {
		t.Fatalf("Vertex native unary failed: response=%+v err=%+v", response, apiErr)
	}
	assertI056VertexUsage(t, provider.GetUsage())
}

func TestVertexI056NativeStreamHTTPInfersOmittedCandidatesAsZero(t *testing.T) {
	server := i056VertexServer(t, true)
	t.Cleanup(server.Close)
	withI056VertexHTTPClient(t, server)
	provider := newI056VertexProvider(t, server)
	request := newI056VertexNativeRequest(t, provider, `{"contents":[]}`, true)
	stream, apiErr := provider.CreateGeminiChatStream(request)
	if apiErr != nil || stream == nil {
		t.Fatalf("Vertex native stream failed to open: stream=%v err=%+v", stream, apiErr)
	}
	if err := collectI056VertexStream(t, stream); !errors.Is(err, io.EOF) {
		t.Fatalf("Vertex native stream terminal=%v, want EOF", err)
	}
	assertI056VertexUsage(t, provider.GetUsage())
}
