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
	geminiUsageEvidenceVertexProjectID = "i056-gemini-usage-project"
	geminiUsageEvidenceVertexResponse  = `{"candidates":[{"index":0,"content":{"role":"model","parts":[]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109}}`
	geminiUsageEvidenceVertexCandidate = `{"candidates":[{"index":0,"content":{"role":"model","parts":[]},"finishReason":"MAX_TOKENS"}]}`
	geminiUsageEvidenceVertexUsage     = `{"usageMetadata":{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109}}`
)

func newGeminiUsageEvidenceVertexProvider(t *testing.T, server *httptest.Server) *VertexAIProvider {
	t.Helper()
	cache.InitCacheManager()
	cacheKey := TokenCacheKey + ":" + geminiUsageEvidenceVertexProjectID
	if err := cache.SetCache(cacheKey, "i056-test-token", time.Minute); err != nil {
		t.Fatalf("seed Vertex token cache: %v", err)
	}
	t.Cleanup(func() { _ = cache.DeleteCache(cacheKey) })

	proxy := ""
	provider := (VertexAIProviderFactory{}).Create(&model.Channel{
		Type:  config.ChannelTypeVertexAI,
		Key:   `{}`,
		Proxy: &proxy,
		Other: `{"region":"global","project_id":"` + geminiUsageEvidenceVertexProjectID + `"}`,
	}).(*VertexAIProvider)
	provider.Config.BaseURL = server.URL + "/%s/%s/%s/%s:%s"
	provider.SetUsage(&types.Usage{})
	return provider
}

func withGeminiUsageEvidenceVertexHTTPClient(t *testing.T, server *httptest.Server) {
	t.Helper()
	previous := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previous })
}

func geminiUsageEvidenceVertexServer(t *testing.T, stream bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+geminiUsageEvidenceVertexCandidate+"\n\n")
			_, _ = io.WriteString(w, "data: "+geminiUsageEvidenceVertexUsage+"\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geminiUsageEvidenceVertexResponse)
	}))
}

func assertGeminiUsageEvidenceVertexUsage(t *testing.T, usage *types.Usage) {
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

func collectGeminiUsageEvidenceVertexStream(t *testing.T, stream requester.StreamReaderInterface[string]) error {
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

func newGeminiUsageEvidenceVertexNativeRequest(t *testing.T, provider *VertexAIProvider, raw string, stream bool) *gemini.GeminiChatRequest {
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

func TestVertexChatUnaryHTTPInfersOmittedCandidatesAsZero(t *testing.T) {
	server := geminiUsageEvidenceVertexServer(t, false)
	t.Cleanup(server.Close)
	withGeminiUsageEvidenceVertexHTTPClient(t, server)
	provider := newGeminiUsageEvidenceVertexProvider(t, server)
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
	assertGeminiUsageEvidenceVertexUsage(t, provider.GetUsage())
}

func TestVertexChatStreamHTTPInfersOmittedCandidatesAsZero(t *testing.T) {
	server := geminiUsageEvidenceVertexServer(t, true)
	t.Cleanup(server.Close)
	withGeminiUsageEvidenceVertexHTTPClient(t, server)
	provider := newGeminiUsageEvidenceVertexProvider(t, server)
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
	if err := collectGeminiUsageEvidenceVertexStream(t, stream); !errors.Is(err, io.EOF) {
		t.Fatalf("Vertex Chat stream terminal=%v, want EOF", err)
	}
	assertGeminiUsageEvidenceVertexUsage(t, provider.GetUsage())
}

func TestVertexNativeUnaryHTTPInfersOmittedCandidatesAsZero(t *testing.T) {
	server := geminiUsageEvidenceVertexServer(t, false)
	t.Cleanup(server.Close)
	withGeminiUsageEvidenceVertexHTTPClient(t, server)
	provider := newGeminiUsageEvidenceVertexProvider(t, server)
	request := newGeminiUsageEvidenceVertexNativeRequest(t, provider, `{"contents":[]}`, false)
	response, apiErr := provider.CreateGeminiChat(request)
	if apiErr != nil || response == nil {
		t.Fatalf("Vertex native unary failed: response=%+v err=%+v", response, apiErr)
	}
	assertGeminiUsageEvidenceVertexUsage(t, provider.GetUsage())
}

func TestVertexNativeStreamHTTPInfersOmittedCandidatesAsZero(t *testing.T) {
	server := geminiUsageEvidenceVertexServer(t, true)
	t.Cleanup(server.Close)
	withGeminiUsageEvidenceVertexHTTPClient(t, server)
	provider := newGeminiUsageEvidenceVertexProvider(t, server)
	request := newGeminiUsageEvidenceVertexNativeRequest(t, provider, `{"contents":[]}`, true)
	stream, apiErr := provider.CreateGeminiChatStream(request)
	if apiErr != nil || stream == nil {
		t.Fatalf("Vertex native stream failed to open: stream=%v err=%+v", stream, apiErr)
	}
	if err := collectGeminiUsageEvidenceVertexStream(t, stream); !errors.Is(err, io.EOF) {
		t.Fatalf("Vertex native stream terminal=%v, want EOF", err)
	}
	assertGeminiUsageEvidenceVertexUsage(t, provider.GetUsage())
}
