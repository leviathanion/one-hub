package gemini_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/providers/gemini"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const i042ImageData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jL1sAAAAASUVORK5CYII="

type i042NoFetch struct{ calls atomic.Int32 }

func (f *i042NoFetch) Fetch(string) (string, []byte, error) {
	f.calls.Add(1)
	return "", nil, errors.New("unexpected remote media fetch")
}

type i042RecordingFetcher struct {
	calls atomic.Int32
	body  []byte
	mime  string
}

func (f *i042RecordingFetcher) Fetch(string) (string, []byte, error) {
	f.calls.Add(1)
	return f.mime, append([]byte(nil), f.body...), nil
}

func TestGeminiI042GeneratedAssistantImageRoundTripsWithoutStorage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "unary", true: "stream"}[stream], func(t *testing.T) {
			runI042GeminiHistoryRoundTrip(t, stream)
		})
	}
}

func runI042GeminiHistoryRoundTrip(t *testing.T, stream bool) {
	t.Helper()
	const modelName = "gemini-2.5-flash-image"
	firstFrame := `{"responseId":"i042-first","modelVersion":"gemini-2.5-flash-image","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"generated"},{"inlineData":{"mimeType":"image/png","data":"` + i042ImageData + `"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
	secondFrame := `{"responseId":"i042-second","modelVersion":"gemini-2.5-flash-image","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"updated"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`
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
				t.Errorf("decode second Gemini request: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 && stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+firstFrame+"\n\n")
			return
		}
		_, _ = io.WriteString(w, map[bool]string{true: secondFrame, false: firstFrame}[calls == 2])
	}))
	t.Cleanup(server.Close)

	proxy := ""
	provider := gemini.GeminiProviderFactory{}.Create(&model.Channel{
		Type:    config.ChannelTypeGemini,
		Key:     "i042-key",
		Proxy:   &proxy,
		BaseURL: &server.URL,
	}).(*gemini.GeminiProvider)
	provider.Config.BaseURL = server.URL
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
	assistant := runI042FirstGeminiResponse(t, provider, firstRequest, stream)
	if len(assistant.Image) != 1 || !strings.Contains(assistant.StringContent(), gemini.GeminiImageSymbol) {
		t.Fatalf("public assistant image representation missing: %+v", assistant)
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
		t.Fatalf("marshal public history: %v", err)
	}
	var decodedNext types.ChatCompletionRequest
	if err := json.Unmarshal(wire, &decodedNext); err != nil {
		t.Fatalf("unmarshal public history: %v", err)
	}
	fetcher := &i042NoFetch{}
	if err := providers.PrepareChatRemoteMedia(provider, &decodedNext, fetcher); err != nil {
		t.Fatalf("prepare generated assistant image: %v", err)
	}
	if fetcher.calls.Load() != 0 {
		t.Fatalf("data URI history unexpectedly fetched remotely: %d", fetcher.calls.Load())
	}

	if response, apiErr := provider.CreateChatCompletion(&decodedNext); apiErr != nil || response == nil {
		t.Fatalf("second Gemini request failed: response=%+v err=%+v", response, apiErr)
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d, want 2", calls)
	}
	imageParts := 0
	textMarkers := 0
	for _, content := range secondRequest.Contents {
		for _, part := range content.Parts {
			if part.InlineData != nil && part.InlineData.MimeType == "image/png" && part.InlineData.Data == i042ImageData {
				imageParts++
			}
			if strings.Contains(part.Text, gemini.GeminiImageSymbol) {
				textMarkers++
			}
		}
	}
	if imageParts != 1 || textMarkers != 0 {
		t.Fatalf("second request did not carry an image part: image_parts=%d marker_text=%d request=%+v", imageParts, textMarkers, secondRequest)
	}
}

func runI042FirstGeminiResponse(t *testing.T, provider *gemini.GeminiProvider, request *types.ChatCompletionRequest, stream bool) types.ChatCompletionMessage {
	t.Helper()
	if !stream {
		response, apiErr := provider.CreateChatCompletion(request)
		if apiErr != nil || response == nil || len(response.Choices) != 1 {
			t.Fatalf("first Gemini unary failed: response=%+v err=%+v", response, apiErr)
		}
		wire, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("marshal first public response: %v", err)
		}
		var public types.ChatCompletionResponse
		if err := json.Unmarshal(wire, &public); err != nil {
			t.Fatalf("unmarshal first public response: %v", err)
		}
		return public.Choices[0].Message
	}

	streamReader, apiErr := provider.CreateChatCompletionStream(request)
	if apiErr != nil || streamReader == nil {
		t.Fatalf("first Gemini stream failed: stream=%v err=%+v", streamReader, apiErr)
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
				t.Fatalf("decode first Gemini stream chunk: %v", err)
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
				t.Fatalf("first Gemini stream error: %v", err)
			}
		case <-timer.C:
			t.Fatal("first Gemini stream timed out")
		}
	}
	return assistant
}

func TestGeminiI042AssistantImageMediaUsesSafePreparation(t *testing.T) {
	proxy := ""
	provider := gemini.GeminiProviderFactory{}.Create(&model.Channel{Type: config.ChannelTypeGemini, Key: "i042-key", Proxy: &proxy}).(*gemini.GeminiProvider)

	dataURI := "data:image/png;base64," + i042ImageData
	for _, test := range []struct {
		name      string
		markerURL string
		imageData string
		fetcher   *i042RecordingFetcher
		wantErr   bool
		wantCalls int32
	}{
		{name: "prepared_data_uri", markerURL: dataURI, imageData: i042ImageData, fetcher: &i042RecordingFetcher{mime: "image/png", body: []byte("unused")}},
		{name: "upload_failed_base64", markerURL: "image upload err", imageData: i042ImageData, fetcher: &i042RecordingFetcher{}},
		{name: "upload_failed_data_uri", markerURL: "image upload err", imageData: dataURI, fetcher: &i042RecordingFetcher{}},
		{name: "upload_failed_non_image", markerURL: "image upload err", imageData: base64.StdEncoding.EncodeToString([]byte("not an image")), fetcher: &i042RecordingFetcher{}, wantErr: true},
		{name: "safe_remote_url", markerURL: "https://cdn.example/image.png", imageData: i042ImageData, fetcher: &i042RecordingFetcher{mime: "image/jpeg", body: []byte("remote")}, wantCalls: 1},
		{name: "unsafe_scheme", markerURL: "file:///tmp/image.png", imageData: i042ImageData, fetcher: &i042RecordingFetcher{mime: "image/png", body: []byte("unused")}, wantErr: true},
		{name: "unsupported_remote_mime", markerURL: "https://cdn.example/file.pdf", imageData: i042ImageData, fetcher: &i042RecordingFetcher{mime: "application/pdf", body: []byte("pdf")}, wantErr: true, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := &types.ChatCompletionRequest{Model: "gemini-2.5-pro", Messages: []types.ChatCompletionMessage{{
				Role:    types.ChatMessageRoleAssistant,
				Content: gemini.GeminiImageSymbol + "(" + test.markerURL + ")",
				Image:   []types.MultimediaData{{Data: test.imageData}},
			}}}
			err := providers.PrepareChatRemoteMedia(provider, request, test.fetcher)
			if (err != nil) != test.wantErr {
				t.Fatalf("safe media preparation error=%v, wantErr=%t", err, test.wantErr)
			}
			if got := test.fetcher.calls.Load(); got != test.wantCalls {
				t.Fatalf("fetch calls=%d, want %d", got, test.wantCalls)
			}
			if test.wantErr {
				return
			}
			converted, apiErr := gemini.ConvertFromChatOpenai(request)
			if apiErr != nil || converted == nil {
				t.Fatalf("prepared assistant image conversion failed: %v", apiErr)
			}
			found := false
			for _, content := range converted.Contents {
				for _, part := range content.Parts {
					if part.InlineData != nil && strings.HasPrefix(part.InlineData.MimeType, "image/") {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("prepared assistant image was not converted to image part: %+v", converted)
			}
		})
	}
}

func TestGeminiI042UserMarkerTextAndInvalidAssistantHistoryAreBounded(t *testing.T) {
	proxy := ""
	provider := gemini.GeminiProviderFactory{}.Create(&model.Channel{Type: config.ChannelTypeGemini, Key: "i042-key", Proxy: &proxy}).(*gemini.GeminiProvider)
	url := "https://public.example/image.png"
	user := &types.ChatCompletionRequest{Model: "gemini-2.5-pro", Messages: []types.ChatCompletionMessage{{
		Role:    types.ChatMessageRoleUser,
		Content: "ordinary text " + gemini.GeminiImageSymbol + "(" + url + ")",
	}}}
	noFetch := &i042NoFetch{}
	if err := providers.PrepareChatRemoteMedia(provider, user, noFetch); err != nil {
		t.Fatalf("ordinary user marker text was rejected: %v", err)
	}
	if noFetch.calls.Load() != 0 {
		t.Fatalf("ordinary user marker text triggered fetch: %d", noFetch.calls.Load())
	}
	converted, apiErr := gemini.ConvertFromChatOpenai(user)
	if apiErr != nil || len(converted.Contents) != 1 || len(converted.Contents[0].Parts) != 1 || converted.Contents[0].Parts[0].Text != user.Messages[0].Content {
		t.Fatalf("ordinary user marker text was reinterpreted: converted=%+v err=%v", converted, apiErr)
	}

	oversized := base64.StdEncoding.EncodeToString(make([]byte, providersBase.MaxChatRemoteMediaItemBytes+1))
	invalid := []struct {
		name    string
		content string
		data    string
	}{
		{name: "empty_image_data", content: gemini.GeminiImageSymbol + "(" + url + ")"},
		{name: "upload_failed_empty_data", content: gemini.GeminiImageSymbol + "(image upload err)"},
		{name: "upload_failed_oversized_data", content: gemini.GeminiImageSymbol + "(image upload err)", data: oversized},
		{name: "malformed_marker", content: gemini.GeminiImageSymbol + "(" + url, data: i042ImageData},
		{name: "oversized_image_data", content: gemini.GeminiImageSymbol + "(" + url + ")", data: oversized},
		{name: "invalid_raw_padding", content: gemini.GeminiImageSymbol + "(" + url + ")", data: "YQ="},
		{name: "invalid_data_uri_padding", content: gemini.GeminiImageSymbol + "(data:image/png;base64,YQ=)", data: "YQ=="},
		{name: "mismatched_data_uri", content: gemini.GeminiImageSymbol + "(data:image/png;base64,AAAA)", data: i042ImageData},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			request := &types.ChatCompletionRequest{Model: "gemini-2.5-pro", Messages: []types.ChatCompletionMessage{{
				Role:    types.ChatMessageRoleAssistant,
				Content: test.content,
				Image:   []types.MultimediaData{{Data: test.data}},
			}}}
			fetcher := &i042NoFetch{}
			if err := providers.PrepareChatRemoteMedia(provider, request, fetcher); err == nil {
				t.Fatal("invalid assistant image history was accepted")
			}
			if fetcher.calls.Load() != 0 {
				t.Fatalf("invalid assistant history fetched media: %d", fetcher.calls.Load())
			}
		})
	}
}
