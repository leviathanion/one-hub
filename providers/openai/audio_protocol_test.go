package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestOpenAISpeechPreservesCurrentFieldsAndStructuredVoice(t *testing.T) {
	var providerBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("audio"))
	}))
	t.Cleanup(server.Close)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	raw := `{"model":"client-model","input":"hello","voice":{"id":"voice_1"},"instructions":"speak softly","stream_format":"sse","future":{"kept":true}}`
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(raw))
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatalf("cache speech request: %v", err)
	}
	var request types.SpeechAudioRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatalf("structured voice projection failed: %v", err)
	}
	if _, ok := request.VoiceString(); ok {
		t.Fatal("structured voice was incorrectly projected as a string")
	}

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "sk-test", Proxy: &proxy}, server.URL)
	provider.SetContext(ctx)
	provider.Usage = &types.Usage{}
	response, apiErr := provider.CreateSpeech(&request)
	if apiErr != nil {
		t.Fatalf("CreateSpeech returned error: %+v", apiErr)
	}
	response.Body.Close()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(providerBody, &fields); err != nil {
		t.Fatalf("decode provider speech body: %v", err)
	}
	for field, want := range map[string]string{
		"voice":         `{"id":"voice_1"}`,
		"instructions":  `"speak softly"`,
		"stream_format": `"sse"`,
		"future":        `{"kept":true}`,
	} {
		if string(fields[field]) != want {
			t.Fatalf("speech field %s=%s want %s (body=%s)", field, fields[field], want, providerBody)
		}
	}
}

func TestTranscriptionModelRewritePreservesMultipartSurfaceAndStream(t *testing.T) {
	var raw bytes.Buffer
	writer := multipart.NewWriter(&raw)
	boundary := writer.Boundary()
	_ = writer.WriteField("model", "client-model")
	_ = writer.WriteField("stream", "true")
	_ = writer.WriteField("chunking_strategy", "auto")
	_ = writer.WriteField("include[]", "logprobs")
	_ = writer.WriteField("include[]", "speaker_labels")
	_ = writer.WriteField("known_speaker_names[]", "Alice")
	file, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatalf("create test file part: %v", err)
	}
	_, _ = file.Write([]byte("wave-bytes"))
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	contentType := "multipart/form-data; boundary=" + boundary

	var providerBody []byte
	var providerContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerContentType = r.Header.Get("Content-Type")
		providerBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: transcript.text.done\ndata: {\"type\":\"transcript.text.done\",\"text\":\"ok\"}\n\n")
	}))
	t.Cleanup(server.Close)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(raw.Bytes()))
	ctx.Request.Header.Set("Content-Type", contentType)
	ctx.Request.ContentLength = int64(raw.Len())
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatalf("cache transcription multipart: %v", err)
	}

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "sk-test", Proxy: &proxy}, server.URL)
	provider.SetContext(ctx)
	provider.SetOriginalModel("client-model")
	provider.Usage = &types.Usage{}
	response, apiErr := provider.CreateTranscriptions(&types.AudioRequest{Model: "mapped-model", Stream: true})
	if apiErr != nil {
		t.Fatalf("CreateTranscriptions returned error: %+v", apiErr)
	}
	if response == nil || response.Stream == nil {
		t.Fatal("streaming transcription was buffered instead of returning a stream")
	}
	response.Stream.Body.Close()

	mediaType, params, err := mime.ParseMediaType(providerContentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] != boundary {
		t.Fatalf("multipart boundary changed: type=%q params=%v err=%v", providerContentType, params, err)
	}
	parsed := multipart.NewReader(bytes.NewReader(providerBody), boundary)
	values := make(map[string][]string)
	var fileBody string
	for {
		part, nextErr := parsed.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatalf("parse rewritten multipart: %v", nextErr)
		}
		body, _ := io.ReadAll(part)
		if part.FormName() == "file" {
			fileBody = string(body)
		} else {
			values[part.FormName()] = append(values[part.FormName()], string(body))
		}
		part.Close()
	}
	if got := values["model"]; len(got) != 1 || got[0] != "mapped-model" {
		t.Fatalf("model patch=%v", got)
	}
	if got := values["include[]"]; len(got) != 2 || got[0] != "logprobs" || got[1] != "speaker_labels" {
		t.Fatalf("repeated include fields changed: %v", got)
	}
	for field, want := range map[string]string{"stream": "true", "chunking_strategy": "auto", "known_speaker_names[]": "Alice"} {
		if got := values[field]; len(got) != 1 || got[0] != want {
			t.Fatalf("multipart field %s=%v want %q", field, got, want)
		}
	}
	if fileBody != "wave-bytes" {
		t.Fatalf("file part changed: %q", fileBody)
	}
}

func TestImageModelRewritePreservesRawMultipartPartsAndAddsMissingModel(t *testing.T) {
	var raw bytes.Buffer
	writer := multipart.NewWriter(&raw)
	boundary := writer.Boundary()
	_ = writer.WriteField("prompt", "draw")
	imageHeader := make(textproto.MIMEHeader)
	imageHeader.Set("Content-Disposition", `form-data; name="image[]"; filename="one.png"`)
	imageHeader.Set("Content-Type", "image/png")
	imagePart, err := writer.CreatePart(imageHeader)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = imagePart.Write([]byte("png-bytes"))
	futureHeader := make(textproto.MIMEHeader)
	futureHeader.Set("Content-Disposition", `form-data; name="future_part"`)
	futureHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	futurePart, err := writer.CreatePart(futureHeader)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = futurePart.Write([]byte("raw=3Dvalue"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	contentType := "multipart/form-data; boundary=" + boundary

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(raw.Bytes()))
	ctx.Request.Header.Set("Content-Type", contentType)
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatal(err)
	}
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, "https://api.openai.com")
	provider.SetContext(ctx)
	provider.SetOriginalModel("dall-e-2")
	req, apiErr := provider.getRequestImageBody(config.RelayModeImagesEdits, "mapped-image", &types.ImageEditRequest{Model: "mapped-image", Prompt: "draw"})
	if apiErr != nil {
		t.Fatalf("build image request: %+v", apiErr)
	}
	providerBody, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] != boundary {
		t.Fatalf("boundary changed: type=%q params=%v err=%v", mediaType, params, err)
	}

	reader := multipart.NewReader(bytes.NewReader(providerBody), boundary)
	values := make(map[string][]string)
	transferEncoding := make(map[string]string)
	for {
		part, nextErr := reader.NextRawPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		partBody, _ := io.ReadAll(part)
		values[part.FormName()] = append(values[part.FormName()], string(partBody))
		transferEncoding[part.FormName()] = part.Header.Get("Content-Transfer-Encoding")
		_ = part.Close()
	}
	if got := values["model"]; len(got) != 1 || got[0] != "mapped-image" {
		t.Fatalf("missing model was not added after mapping: %v", got)
	}
	if got := values["image[]"]; len(got) != 1 || got[0] != "png-bytes" {
		t.Fatalf("image part changed: %v", got)
	}
	if got := values["future_part"]; len(got) != 1 || got[0] != "raw=3Dvalue" || transferEncoding["future_part"] != "quoted-printable" {
		t.Fatalf("unknown encoded part changed: body=%v transfer-encoding=%q", got, transferEncoding["future_part"])
	}
}

func TestOpenAITranscriptionUsageKeepsTokenAndDurationEvidenceSeparate(t *testing.T) {
	inputTokens, outputTokens, totalTokens := 7, 2, 9
	textTokens, audioTokens := 1, 6
	tokenUsage := &types.AudioUsage{
		Type: "tokens", InputTokens: &inputTokens, OutputTokens: &outputTokens, TotalTokens: &totalTokens,
		InputDetails: &types.AudioUsageInputDetails{TextTokens: &textTokens, AudioTokens: &audioTokens},
	}
	target := &types.Usage{}
	applyOpenAITranscriptionUsage(target, tokenUsage, "gpt-transcribe-actual")
	if !target.HasProviderUsage() || target.ResponseModel != "gpt-transcribe-actual" || target.GetExtraTokens()[config.UsageExtraInputAudio] != 6 {
		t.Fatalf("transcription token evidence was lost: %+v", target)
	}

	seconds := 2.5
	durationTarget := &types.Usage{}
	applyOpenAITranscriptionUsage(durationTarget, &types.AudioUsage{Type: "duration", Seconds: &seconds}, "gpt-transcribe-actual")
	if durationTarget.ProviderReported || durationTarget.ProviderOperationUnits == nil || *durationTarget.ProviderOperationUnits != 1 || !durationTarget.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription] || durationTarget.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != 2.5 {
		t.Fatalf("duration usage was mixed into token evidence: %+v", durationTarget)
	}
}

func TestOpenAITranscriptionUsageRejectsMissingTokenPartition(t *testing.T) {
	inputTokens, outputTokens, totalTokens := 7, 2, 9
	target := &types.Usage{}
	applyOpenAITranscriptionUsage(target, &types.AudioUsage{Type: "tokens", InputTokens: &inputTokens, OutputTokens: &outputTokens, TotalTokens: &totalTokens}, "")
	if target.HasProviderUsage() {
		t.Fatalf("missing transcription input details became priceable: %+v", target)
	}
}
