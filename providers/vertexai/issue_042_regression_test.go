package vertexai_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers"
	"one-api/providers/gemini"
	"one-api/providers/vertexai"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const (
	i042VertexImageData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jL1sAAAAASUVORK5CYII="
	i042VertexProject   = "i042-vertex-history"
)

type i042VertexNoFetch struct{ calls atomic.Int32 }

func (f *i042VertexNoFetch) Fetch(string) (string, []byte, error) {
	f.calls.Add(1)
	return "", nil, errors.New("unexpected remote media fetch")
}

func TestVertexI042GeneratedAssistantImageRoundTripsWithoutStorage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "unary", true: "stream"}[stream], func(t *testing.T) {
			runI042VertexHistoryRoundTrip(t, stream)
		})
	}
}

func runI042VertexHistoryRoundTrip(t *testing.T, stream bool) {
	t.Helper()
	const modelName = "gemini-2.5-flash-image"
	firstFrame := `{"responseId":"i042-vertex-first","modelVersion":"gemini-2.5-flash-image","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"generated"},{"inlineData":{"mimeType":"image/png","data":"` + i042VertexImageData + `"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
	secondFrame := `{"responseId":"i042-vertex-second","modelVersion":"gemini-2.5-flash-image","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"updated"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`
	var calls int
	var secondRequest gemini.GeminiChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		if calls == 2 {
			if err := json.Unmarshal(body, &secondRequest); err != nil {
				t.Errorf("decode second Vertex Gemini request: %v", err)
			}
		}
		if calls == 1 && stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+firstFrame+"\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if calls == 2 {
			_, _ = io.WriteString(w, secondFrame)
			return
		}
		_, _ = io.WriteString(w, firstFrame)
	}))
	t.Cleanup(server.Close)

	cache.InitCacheManager()
	cacheKey := vertexai.TokenCacheKey + ":" + i042VertexProject
	if err := cache.SetCache(cacheKey, "i042-token", time.Minute); err != nil {
		t.Fatalf("seed Vertex token cache: %v", err)
	}
	t.Cleanup(func() { _ = cache.DeleteCache(cacheKey) })
	proxy := ""
	provider := (vertexai.VertexAIProviderFactory{}).Create(&model.Channel{
		Type:  config.ChannelTypeVertexAI,
		Key:   `{}`,
		Proxy: &proxy,
		Other: `{"region":"global","project_id":"` + i042VertexProject + `"}`,
	}).(*vertexai.VertexAIProvider)
	provider.Config.BaseURL = server.URL + "/%s/%s/%s/%s:%s"
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	provider.SetContext(ctx)
	provider.SetUsage(&types.Usage{})
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	firstRequest := &types.ChatCompletionRequest{
		Model:      modelName,
		Stream:     stream,
		Modalities: []string{"TEXT", "IMAGE"},
		Messages:   []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "draw a cat"}},
	}
	assistant := runI042FirstVertexResponse(t, provider, firstRequest, stream)
	if len(assistant.Image) != 1 || !strings.Contains(assistant.StringContent(), gemini.GeminiImageSymbol) {
		t.Fatalf("public Vertex assistant image representation missing: %+v", assistant)
	}

	next := &types.ChatCompletionRequest{
		Model: modelName,
		Messages: []types.ChatCompletionMessage{
			firstRequest.Messages[0],
			assistant,
			{Role: types.ChatMessageRoleUser, Content: "make it red"},
		},
	}
	wire, err := json.Marshal(next)
	if err != nil {
		t.Fatalf("marshal public Vertex history: %v", err)
	}
	var decodedNext types.ChatCompletionRequest
	if err := json.Unmarshal(wire, &decodedNext); err != nil {
		t.Fatalf("unmarshal public Vertex history: %v", err)
	}
	fetcher := &i042VertexNoFetch{}
	if err := providers.PrepareChatRemoteMedia(provider, &decodedNext, fetcher); err != nil {
		t.Fatalf("prepare generated Vertex assistant image: %v", err)
	}
	if fetcher.calls.Load() != 0 {
		t.Fatalf("Vertex data URI history unexpectedly fetched remotely: %d", fetcher.calls.Load())
	}
	if response, apiErr := provider.CreateChatCompletion(&decodedNext); apiErr != nil || response == nil {
		t.Fatalf("second Vertex Gemini request failed: response=%+v err=%+v", response, apiErr)
	}
	if calls != 2 {
		t.Fatalf("Vertex upstream calls=%d, want 2", calls)
	}
	imageParts := 0
	textMarkers := 0
	for _, content := range secondRequest.Contents {
		for _, part := range content.Parts {
			if part.InlineData != nil && part.InlineData.MimeType == "image/png" && part.InlineData.Data == i042VertexImageData {
				imageParts++
			}
			if strings.Contains(part.Text, gemini.GeminiImageSymbol) {
				textMarkers++
			}
		}
	}
	if imageParts != 1 || textMarkers != 0 {
		t.Fatalf("second Vertex request did not carry an image part: image_parts=%d marker_text=%d request=%+v", imageParts, textMarkers, secondRequest)
	}
}

func runI042FirstVertexResponse(t *testing.T, provider *vertexai.VertexAIProvider, request *types.ChatCompletionRequest, stream bool) types.ChatCompletionMessage {
	t.Helper()
	if !stream {
		response, apiErr := provider.CreateChatCompletion(request)
		if apiErr != nil || response == nil || len(response.Choices) != 1 {
			t.Fatalf("first Vertex unary failed: response=%+v err=%+v", response, apiErr)
		}
		wire, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("marshal first Vertex public response: %v", err)
		}
		var public types.ChatCompletionResponse
		if err := json.Unmarshal(wire, &public); err != nil {
			t.Fatalf("unmarshal first Vertex public response: %v", err)
		}
		return public.Choices[0].Message
	}

	streamReader, apiErr := provider.CreateChatCompletionStream(request)
	if apiErr != nil || streamReader == nil {
		t.Fatalf("first Vertex stream failed: stream=%v err=%+v", streamReader, apiErr)
	}
	data, streamErrors := streamReader.Recv()
	defer requester.CloseAndDrainStream(streamReader)
	var assistant types.ChatCompletionMessage
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for data != nil || streamErrors != nil {
		select {
		case raw, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			var chunk types.ChatCompletionStreamResponse
			if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
				t.Fatalf("decode first Vertex stream chunk: %v", err)
			}
			for _, choice := range chunk.Choices {
				assistant.Role = types.ChatMessageRoleAssistant
				assistant.Content = assistant.StringContent() + choice.Delta.Content
				assistant.Image = append(assistant.Image, choice.Delta.Image...)
			}
		case err, ok := <-streamErrors:
			if !ok {
				streamErrors = nil
				continue
			}
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("first Vertex stream error: %v", err)
			}
		case <-timer.C:
			t.Fatal("first Vertex stream timed out")
		}
	}
	return assistant
}

func TestVertexI042DoesNotExpandGeminiHistoryForOtherCategories(t *testing.T) {
	proxy := ""
	originalContent := gemini.GeminiImageSymbol + "(https://public.example/image.png)"
	provider := (vertexai.VertexAIProviderFactory{}).Create(&model.Channel{
		Type:  config.ChannelTypeVertexAI,
		Key:   `{}`,
		Proxy: &proxy,
		Other: `{"region":"global","project_id":"` + i042VertexProject + `"}`,
	}).(*vertexai.VertexAIProvider)
	request := &types.ChatCompletionRequest{
		Model: "claude-sonnet-4",
		Messages: []types.ChatCompletionMessage{{
			Role:    types.ChatMessageRoleAssistant,
			Content: originalContent,
			Image:   []types.MultimediaData{{Data: i042VertexImageData}},
		}},
	}
	fetcher := &i042VertexNoFetch{}
	if err := providers.PrepareChatRemoteMedia(provider, request, fetcher); err != nil {
		t.Fatalf("non-Gemini Vertex category was rejected: %v", err)
	}
	if fetcher.calls.Load() != 0 {
		t.Fatalf("non-Gemini Vertex category unexpectedly touched Gemini history: calls=%d content=%#v", fetcher.calls.Load(), request.Messages[0].Content)
	}
	if got, ok := request.Messages[0].Content.(string); !ok || got != originalContent {
		t.Fatalf("non-Gemini Vertex category content was rewritten: %#v", request.Messages[0].Content)
	}
}
