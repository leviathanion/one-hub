package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestIssue005ImageProducerKeepsOptionalPartitionPresenceSeparateFromBase(t *testing.T) {
	var response OpenAIProviderImageResponse
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-image-actual",
		"data":[{"b64_json":"image"}],
		"usage":{"input_tokens":50,"output_tokens":50,"total_tokens":100,
			"input_tokens_details":{"text_tokens":50,"image_tokens":0}}
	}`), &response); err != nil {
		t.Fatal(err)
	}
	target := &types.Usage{}
	ApplyImageEvidence(target, &response.ImageResponse)
	if !target.HasProviderBaseUsage() {
		t.Fatalf("valid base image usage was rejected: %+v", target)
	}
	if target.HasProviderUsage() {
		t.Fatalf("strict partition check unexpectedly erased the missing output fact: %+v", target)
	}
	if got := target.GetExtraTokens()[config.UsageExtraInputTextTokens]; got != 50 {
		t.Fatalf("input text detail was lost: %+v", target)
	}
	if !target.ProviderTokenFields[config.UsageExtraInputImageTokens] || target.GetExtraTokens()[config.UsageExtraInputImageTokens] != 0 {
		t.Fatalf("explicit zero input image detail was lost: fields=%v extras=%v", target.ProviderTokenFields, target.GetExtraTokens())
	}
	if len(target.RequiredTokenExtraKeys) != 4 || len(target.TokenExtraEvidenceGroups) != 2 {
		t.Fatalf("image partition contract was not retained: %+v", target)
	}
}

func TestIssue005ImageProducerKeepsInputOutputWhenTotalIsOmitted(t *testing.T) {
	var response OpenAIProviderImageResponse
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-image-actual",
		"data":[{"b64_json":"image"}],
		"usage":{"input_tokens":50,"output_tokens":50}
	}`), &response); err != nil {
		t.Fatal(err)
	}
	target := &types.Usage{}
	ApplyImageEvidence(target, &response.ImageResponse)
	if !target.HasProviderBaseUsage() {
		t.Fatalf("input/output image usage without redundant total was rejected: %+v", target)
	}
	if target.ProviderTokenFields["total_tokens"] {
		t.Fatalf("omitted total_tokens was fabricated as provider evidence: %+v", target)
	}
}

func TestIssue005ImageProducerRejectsConflictingReportedTotal(t *testing.T) {
	var response OpenAIProviderImageResponse
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-image-actual",
		"data":[{"b64_json":"image"}],
		"usage":{"input_tokens":50,"output_tokens":50,"total_tokens":101}
	}`), &response); err != nil {
		t.Fatal(err)
	}
	target := &types.Usage{}
	ApplyImageEvidence(target, &response.ImageResponse)
	if target.HasProviderBaseUsage() || !target.ProviderTokenConflict {
		t.Fatalf("conflicting reported total was accepted: %+v", target)
	}
}

func issue005OpenAITranscriptionContext(t *testing.T, stream bool) *gin.Context {
	t.Helper()
	var raw bytes.Buffer
	writer := multipart.NewWriter(&raw)
	boundary := writer.Boundary()
	_ = writer.WriteField("model", "whisper-1")
	if stream {
		_ = writer.WriteField("stream", "true")
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

func issue005OpenAIProvider(t *testing.T, serverURL string, ctx *gin.Context) *OpenAIProvider {
	t.Helper()
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "sk-test", Proxy: &proxy}, serverURL)
	provider.SetContext(ctx)
	provider.SetOriginalModel("whisper-1")
	provider.SetUsage(&types.Usage{})
	return provider
}

func TestIssue005OpenAITranscriptionJSONProducerRetainsOptionalInputEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"ok","model":"whisper-actual","usage":{"type":"tokens","input_tokens":14,"output_tokens":45,"total_tokens":59,"input_token_details":{"text_tokens":0}}}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	provider := issue005OpenAIProvider(t, server.URL, issue005OpenAITranscriptionContext(t, false))
	response, apiErr := provider.CreateTranscriptions(&types.AudioRequest{Model: "whisper-1", ResponseFormat: "json"})
	if apiErr != nil || response == nil {
		t.Fatalf("JSON transcription failed: response=%+v err=%+v", response, apiErr)
	}
	if !provider.Usage.HasProviderBaseUsage() || provider.Usage.HasProviderUsage() {
		t.Fatalf("JSON transcription base/strict evidence contracts are wrong: %+v", provider.Usage)
	}
	if provider.Usage.PromptTokens != 14 || provider.Usage.CompletionTokens != 45 || provider.Usage.TotalTokens != 59 {
		t.Fatalf("JSON transcription counters changed: %+v", provider.Usage)
	}
	if len(provider.Usage.TokenExtraEvidenceGroups) != 1 {
		t.Fatalf("JSON transcription partition contract missing: %+v", provider.Usage)
	}
	if !provider.Usage.ProviderTokenFields[config.UsageExtraInputTextTokens] || provider.Usage.GetExtraTokens()[config.UsageExtraInputTextTokens] != 0 {
		t.Fatalf("explicit zero transcription detail lost: fields=%v extras=%v", provider.Usage.ProviderTokenFields, provider.Usage.GetExtraTokens())
	}
}

func TestIssue005OpenAITranscriptionTokenSSEProducerPublishesUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: transcript.text.done\ndata: {\"type\":\"transcript.text.done\",\"model\":\"whisper-sse-actual\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":14,\"output_tokens\":45,\"total_tokens\":59,\"input_token_details\":{\"audio_tokens\":14}}}\n\n")
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	provider := issue005OpenAIProvider(t, server.URL, issue005OpenAITranscriptionContext(t, true))
	response, apiErr := provider.CreateTranscriptions(&types.AudioRequest{Model: "whisper-1", Stream: true})
	if apiErr != nil || response == nil || response.Stream == nil || response.ObserveProviderEvent == nil {
		t.Fatalf("SSE transcription did not return an observable stream: response=%+v err=%+v", response, apiErr)
	}
	body, err := io.ReadAll(response.Stream.Body)
	response.Stream.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range strings.Split(string(body), "\n\n") {
		for _, line := range strings.Split(event, "\n") {
			if strings.HasPrefix(line, "data:") {
				response.ObserveProviderEvent([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
			}
		}
	}
	if !provider.Usage.HasProviderBaseUsage() || provider.Usage.PromptTokens != 14 || provider.Usage.CompletionTokens != 45 || provider.Usage.TotalTokens != 59 {
		t.Fatalf("SSE transcription usage was not published: %+v", provider.Usage)
	}
	if provider.Usage.ResponseModel != "whisper-sse-actual" || provider.Usage.GetExtraTokens()[config.UsageExtraInputAudio] != 14 {
		t.Fatalf("SSE transcription attribution/details changed: %+v", provider.Usage)
	}
}
