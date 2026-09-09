package openai_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/azure"
	"one-api/providers/azure_v1"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const issue047Model = "whisper-1"

type issue047TranscriptionFactory struct {
	name    string
	channel int
	new     func(*model.Channel) base.ProviderInterface
}

func issue047TranscriptionFactories() []issue047TranscriptionFactory {
	return []issue047TranscriptionFactory{
		{
			name:    "openai",
			channel: config.ChannelTypeOpenAI,
			new: func(channel *model.Channel) base.ProviderInterface {
				return openai.OpenAIProviderFactory{}.Create(channel)
			},
		},
		{
			name:    "azure",
			channel: config.ChannelTypeAzure,
			new: func(channel *model.Channel) base.ProviderInterface {
				return azure.AzureProviderFactory{}.Create(channel)
			},
		},
		{
			name:    "azure-v1",
			channel: config.ChannelTypeAzureV1,
			new: func(channel *model.Channel) base.ProviderInterface {
				return azure_v1.AzureV1ProviderFactory{}.Create(channel)
			},
		},
		{
			name:    "custom-openai-compatible",
			channel: config.ChannelTypeCustom,
			new: func(channel *model.Channel) base.ProviderInterface {
				return openai.OpenAIProviderFactory{}.Create(channel)
			},
		},
	}
}

func TestIssue047WhisperJSONUsesActualResponseForBothClientStreamIntents(t *testing.T) {
	rawResponse := `{"text":"hello","model":"whisper-actual","duration":9,"usage":{"type":"duration","seconds":9},"future":{"keep":true}}`
	for _, factory := range issue047TranscriptionFactories() {
		for _, stream := range []bool{false, true} {
			t.Run(factory.name+"/stream="+map[bool]string{false: "false", true: "true"}[stream], func(t *testing.T) {
				var upstreamCalls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					upstreamCalls.Add(1)
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.Header().Set("X-Request-Id", "i047-json-request")
					w.Header().Set("Cache-Control", "private, max-age=1")
					w.Header().Set("Content-Language", "en")
					w.Header().Set("Digest", "sha-256=i047")
					_, _ = io.WriteString(w, rawResponse)
				}))
				t.Cleanup(server.Close)
				previousHTTPClient := requester.HTTPClient
				requester.HTTPClient = server.Client()
				t.Cleanup(func() { requester.HTTPClient = previousHTTPClient })

				proxy, baseURL := "", server.URL
				channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: factory.channel, Key: "i047-key", Proxy: &proxy, BaseURL: &baseURL}
				if factory.channel == config.ChannelTypeAzure {
					channel.Other = `{"api_version":"2024-10-01-preview"}`
				}
				provider := factory.new(channel)
				provider.SetContext(issue047TranscriptionContext(t, stream))
				provider.SetOriginalModel(issue047Model)
				provider.SetUsage(&types.Usage{})
				transcriber, ok := provider.(base.TranscriptionsInterface)
				if !ok {
					t.Fatalf("factory lost transcription interface: %T", provider)
				}

				response, apiErr := transcriber.CreateTranscriptions(&types.AudioRequest{
					Model: issue047Model, ResponseFormat: "json", Stream: stream,
				})
				if apiErr != nil || response == nil {
					t.Fatalf("JSON transcription failed: response=%+v err=%+v", response, apiErr)
				}
				if response.Stream != nil {
					t.Fatalf("JSON provider response was misclassified as SSE: %+v", response)
				}
				if !bytes.Equal(response.Body, []byte(rawResponse)) {
					t.Fatalf("JSON provider body changed: got=%q want=%q", response.Body, rawResponse)
				}
				var decoded map[string]any
				if err := json.Unmarshal(response.Body, &decoded); err != nil || decoded["future"] == nil || decoded["text"] != "hello" {
					t.Fatalf("returned JSON is not parseable or lost unknown fields: err=%v body=%s", err, response.Body)
				}
				for name, want := range map[string]string{
					"Content-Type":     "application/json; charset=utf-8",
					"X-Request-Id":     "i047-json-request",
					"Cache-Control":    "private, max-age=1",
					"Content-Language": "en",
					"Digest":           "sha-256=i047",
				} {
					if got := response.Headers[name]; got != want {
						t.Fatalf("response header %s=%q want %q (all=%v)", name, got, want, response.Headers)
					}
				}
				if got := upstreamCalls.Load(); got != 1 {
					t.Fatalf("provider request was replayed: calls=%d", got)
				}
				usage := provider.GetUsage()
				if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 ||
					!usage.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription] ||
					usage.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != 9 {
					t.Fatalf("duration evidence did not survive JSON fallback: %+v", usage)
				}
				if usage.ResponseModel != "whisper-actual" {
					t.Fatalf("actual response model attribution was lost: %+v", usage)
				}
			})
		}
	}
}

func TestIssue047WhisperTokenSSEStillUsesSSEAndPublishesUsage(t *testing.T) {
	const rawSSE = "event: transcript.text.delta\ndata: {\"type\":\"transcript.text.delta\",\"delta\":\"hello\"}\n\n" +
		"event: transcript.text.done\ndata: {\"type\":\"transcript.text.done\",\"model\":\"whisper-sse\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":14,\"output_tokens\":45,\"total_tokens\":59,\"input_token_details\":{\"text_tokens\":0,\"audio_tokens\":14}}}\n\n"
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "i047-sse-request")
		_, _ = io.WriteString(w, rawSSE)
	}))
	t.Cleanup(server.Close)
	previousHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousHTTPClient })

	proxy, baseURL := "", server.URL
	provider := openai.OpenAIProviderFactory{}.Create(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "i047-sse-key", Proxy: &proxy, BaseURL: &baseURL})
	provider.SetContext(issue047TranscriptionContext(t, true))
	provider.SetOriginalModel(issue047Model)
	provider.SetUsage(&types.Usage{})
	transcriber := provider.(base.TranscriptionsInterface)
	response, apiErr := transcriber.CreateTranscriptions(&types.AudioRequest{Model: issue047Model, ResponseFormat: "json", Stream: true})
	if apiErr != nil || response == nil || response.Stream == nil || response.ObserveProviderEvent == nil {
		t.Fatalf("token SSE was not retained as a stream: response=%+v err=%+v", response, apiErr)
	}
	body, err := io.ReadAll(response.Stream.Body)
	_ = response.Stream.Body.Close()
	if err != nil || string(body) != rawSSE {
		t.Fatalf("SSE wire changed: err=%v got=%q want=%q", err, body, rawSSE)
	}
	for _, event := range strings.Split(string(body), "\n\n") {
		for _, line := range strings.Split(event, "\n") {
			if strings.HasPrefix(line, "data:") {
				response.ObserveProviderEvent([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
			}
		}
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("SSE provider request was replayed: calls=%d", upstreamCalls.Load())
	}
	usage := provider.GetUsage()
	if usage == nil || !usage.HasProviderUsage() || usage.PromptTokens != 14 || usage.CompletionTokens != 45 || usage.TotalTokens != 59 {
		t.Fatalf("token SSE usage was not published: %+v", usage)
	}
	if usage.ResponseModel != "whisper-sse" || usage.GetExtraTokens()[config.UsageExtraInputAudio] != 14 {
		t.Fatalf("token SSE attribution/details changed: %+v", usage)
	}
}

func TestIssue047InvalidJSONAfterAcceptedProviderResponseIsAnError(t *testing.T) {
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":`)
	}))
	t.Cleanup(server.Close)
	previousHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousHTTPClient })

	proxy, baseURL := "", server.URL
	provider := openai.OpenAIProviderFactory{}.Create(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "i047-invalid-key", Proxy: &proxy, BaseURL: &baseURL})
	provider.SetContext(issue047TranscriptionContext(t, true))
	provider.SetOriginalModel(issue047Model)
	provider.SetUsage(&types.Usage{})
	response, apiErr := provider.(base.TranscriptionsInterface).CreateTranscriptions(&types.AudioRequest{Model: issue047Model, ResponseFormat: "json", Stream: true})
	if response != nil || apiErr == nil || !apiErr.UpstreamAccepted || apiErr.Code != "decode_response_failed" {
		t.Fatalf("invalid JSON was treated as a successful stream: response=%+v err=%+v", response, apiErr)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("invalid JSON request was replayed: calls=%d", upstreamCalls.Load())
	}
}

func issue047TranscriptionContext(t *testing.T, stream bool) *gin.Context {
	t.Helper()
	var raw bytes.Buffer
	writer := multipart.NewWriter(&raw)
	boundary := writer.Boundary()
	if err := writer.WriteField("model", issue047Model); err != nil {
		t.Fatal(err)
	}
	if stream {
		if err := writer.WriteField("stream", "true"); err != nil {
			t.Fatal(err)
		}
	}
	file, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("wave-bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(raw.Bytes()))
	ctx.Request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	ctx.Request.ContentLength = int64(raw.Len())
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx
}
