package openai_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	providerregistry "one-api/providers"
	"one-api/providers/azure"
	"one-api/providers/azure_v1"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/providers/siliconflow"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const issue046Model = "gpt-4o-transcribe-diarize"

type issue046TranscriptionFactory struct {
	name    string
	channel int
	new     func(*model.Channel) base.ProviderInterface
}

func issue046TranscriptionFactories() []issue046TranscriptionFactory {
	return []issue046TranscriptionFactory{
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
		{
			name:    "siliconflow",
			channel: config.ChannelTypeSiliconflow,
			new: func(channel *model.Channel) base.ProviderInterface {
				return siliconflow.SiliconflowProviderFactory{}.Create(channel)
			},
		},
	}
}

func TestIssue046DiarizedJSONGateCoversSharedFactories(t *testing.T) {
	for _, factory := range issue046TranscriptionFactories() {
		t.Run(factory.name, func(t *testing.T) {
			proxy, baseURL := "", "http://127.0.0.1:1"
			channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: factory.channel, Key: "i046-key", Proxy: &proxy, BaseURL: &baseURL}
			if factory.channel == config.ChannelTypeAzure {
				channel.Other = `{"api_version":"2024-10-01-preview"}`
			}
			err := providerregistry.AssessTranscriptionRequest(channel, &types.AudioRequest{
				Model: issue046Model, ResponseFormat: "diarized_json",
			})
			if err != nil {
				t.Fatalf("candidate transcription gate rejected diarized_json: %v", err)
			}
		})
	}
}

func TestIssue046DiarizedJSONDirectProvidersPreserveSpeakerWireAndUsage(t *testing.T) {
	rawResponse := `{"task":"transcribe","text":"hello","segments":[{"id":0,"speaker":"A","text":"hello"}],"speaker_labels":[{"speaker":"A","start":0,"end":1}],"model":"diarized-actual","usage":{"type":"duration","seconds":9},"future":{"keep":true}}`
	for _, factory := range issue046TranscriptionFactories() {
		t.Run(factory.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("X-Request-Id", "i046-diarized")
				_, _ = io.WriteString(w, rawResponse)
			}))
			t.Cleanup(server.Close)
			previousHTTPClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousHTTPClient })

			proxy, baseURL := "", server.URL
			channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: factory.channel, Key: "i046-key", Proxy: &proxy, BaseURL: &baseURL}
			if factory.channel == config.ChannelTypeAzure {
				channel.Other = `{"api_version":"2024-10-01-preview"}`
			}
			provider := factory.new(channel)
			provider.SetContext(issue046TranscriptionContext(t))
			provider.SetOriginalModel(issue046Model)
			provider.SetUsage(&types.Usage{})
			transcriber, ok := provider.(base.TranscriptionsInterface)
			if !ok {
				t.Fatalf("factory lost transcription interface: %T", provider)
			}
			response, apiErr := transcriber.CreateTranscriptions(&types.AudioRequest{
				Model: issue046Model, ResponseFormat: "diarized_json",
			})
			if apiErr != nil || response == nil || response.Stream != nil {
				t.Fatalf("diarized JSON provider call failed or became stream: response=%+v err=%+v", response, apiErr)
			}
			if !bytes.Equal(response.Body, []byte(rawResponse)) {
				t.Fatalf("diarized JSON body was re-encoded: got=%q want=%q", response.Body, rawResponse)
			}
			var decoded map[string]any
			if err := json.Unmarshal(response.Body, &decoded); err != nil || decoded["segments"] == nil || decoded["speaker_labels"] == nil || decoded["future"] == nil {
				t.Fatalf("speaker/unknown fields were not parseable in returned JSON: err=%v body=%s", err, response.Body)
			}
			if response.Headers["X-Request-Id"] != "i046-diarized" {
				t.Fatalf("provider response header was lost: %+v", response.Headers)
			}
			if upstreamCalls.Load() != 1 {
				t.Fatalf("diarized JSON provider request was replayed: calls=%d", upstreamCalls.Load())
			}
			usage := provider.GetUsage()
			if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 ||
				!usage.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription] ||
				usage.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != 9 || usage.ResponseModel != "diarized-actual" {
				t.Fatalf("diarized JSON usage evidence was not published: %+v", usage)
			}
		})
	}
}

func TestIssue046InvalidFormatsAndSiliconflowStreamRemainRejected(t *testing.T) {
	for _, test := range []struct {
		name    string
		format  string
		stream  bool
		channel int
		wantErr bool
	}{
		{name: "normal json", format: "json", channel: config.ChannelTypeOpenAI},
		{name: "verbose json", format: "verbose_json", channel: config.ChannelTypeOpenAI},
		{name: "invalid format", format: "xml", channel: config.ChannelTypeOpenAI, wantErr: true},
		{name: "siliconflow stream", format: "diarized_json", stream: true, channel: config.ChannelTypeSiliconflow, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			proxy, baseURL := "", "http://127.0.0.1:1"
			channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: test.channel, Key: "i046-key", Proxy: &proxy, BaseURL: &baseURL}
			err := providerregistry.AssessTranscriptionRequest(channel, &types.AudioRequest{
				Model: issue046Model, ResponseFormat: test.format, Stream: test.stream,
			})
			if (err != nil) != test.wantErr {
				t.Fatalf("gate result for format=%q stream=%t: err=%v wantErr=%t", test.format, test.stream, err, test.wantErr)
			}
			if test.wantErr {
				var capabilityErr *base.RequestCapabilityError
				if !errors.As(err, &capabilityErr) {
					t.Fatalf("gate returned an untyped error: %v", err)
				}
			}
		})
	}
}

func TestIssue046DirectTranscriptionGateRejectsInvalidFormatBeforeProviderWork(t *testing.T) {
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls.Add(1)
	}))
	t.Cleanup(server.Close)
	previousHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousHTTPClient })

	proxy, baseURL := "", server.URL
	provider := openai.OpenAIProviderFactory{}.Create(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "i046-key", Proxy: &proxy, BaseURL: &baseURL})
	provider.SetContext(issue046TranscriptionContext(t))
	provider.SetOriginalModel(issue046Model)
	provider.SetUsage(&types.Usage{})
	response, apiErr := provider.(base.TranscriptionsInterface).CreateTranscriptions(&types.AudioRequest{
		Model: issue046Model, ResponseFormat: "xml",
	})
	if response != nil || apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || !apiErr.LocalError || apiErr.Code != "transcription_billing_evidence_unavailable" {
		t.Fatalf("direct provider accepted invalid transcription format: response=%+v err=%+v", response, apiErr)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("direct transcription gate sent provider work: calls=%d", upstreamCalls.Load())
	}
}

func issue046TranscriptionContext(t *testing.T) *gin.Context {
	t.Helper()
	var raw bytes.Buffer
	writer := multipart.NewWriter(&raw)
	boundary := writer.Boundary()
	if err := writer.WriteField("model", issue046Model); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("response_format", "diarized_json"); err != nil {
		t.Fatal(err)
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
