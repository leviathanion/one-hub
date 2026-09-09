package gemini

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/datatypes"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func TestGeminiRemoteMediaPolicyFollowsWireDialect(t *testing.T) {
	for _, test := range []struct {
		name         string
		useOpenAIAPI bool
		want         base.RemoteMediaMode
	}{
		{name: "OpenAI dialect passes URLs", useOpenAIAPI: true, want: base.RemoteMediaPassURL},
		{name: "native Gemini materializes", want: base.RemoteMediaMaterialize},
	} {
		t.Run(test.name, func(t *testing.T) {
			channel := &model.Channel{}
			if test.useOpenAIAPI {
				plugin := datatypes.NewJSONType(model.PluginType{"use_openai_api": {"enable": true}})
				channel.Plugin = &plugin
			}
			mode, err := (GeminiProviderFactory{}).AssessChatRemoteMedia(channel, &types.ChatCompletionRequest{}, base.ChatRemoteMediaSummary{Items: 1, RemoteURLs: 1})
			if err != nil || mode != test.want {
				t.Fatalf("Gemini remote media policy = %v, %v; want %v", mode, err, test.want)
			}
		})
	}
}

func TestConvertFromChatOpenaiDoesNotInterpretImageSymbolInText(t *testing.T) {
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gets.Add(1)
		_, _ = w.Write([]byte("unexpected"))
	}))
	t.Cleanup(server.Close)

	want := "prefix " + GeminiImageSymbol + "(" + server.URL + "/image.png) suffix"
	converted, apiErr := ConvertFromChatOpenai(&types.ChatCompletionRequest{
		Model: "gemini-2.5-pro",
		Messages: []types.ChatCompletionMessage{{
			Role:    types.ChatMessageRoleUser,
			Content: want,
		}},
	})
	if apiErr != nil {
		t.Fatalf("convert request: %v", apiErr)
	}
	if got := gets.Load(); got != 0 {
		t.Fatalf("converter issued %d network requests, want 0", got)
	}
	if len(converted.Contents) != 1 || len(converted.Contents[0].Parts) != 1 || converted.Contents[0].Parts[0].Text != want {
		t.Fatalf("ordinary text was reinterpreted as remote media: %+v", converted.Contents)
	}
}

func TestConvertFromChatOpenaiAcceptsOnlyPreparedImageData(t *testing.T) {
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gets.Add(1)
		_, _ = w.Write([]byte("unexpected"))
	}))
	t.Cleanup(server.Close)

	remote := &types.ChatCompletionRequest{
		Model: "gemini-2.5-pro",
		Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: []types.ChatMessagePart{{
			Type:     types.ContentTypeImageURL,
			ImageURL: &types.ChatMessageImageURL{URL: server.URL + "/image.png"},
		}}}},
	}
	if _, apiErr := ConvertFromChatOpenai(remote); apiErr == nil || apiErr.Code != "image_url_invalid" {
		t.Fatalf("unprepared remote image was accepted: %v", apiErr)
	}
	if got := gets.Load(); got != 0 {
		t.Fatalf("converter issued %d network requests, want 0", got)
	}

	prepared := remote
	prepared.Messages = []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: []types.ChatMessagePart{{
		Type:     types.ContentTypeImageURL,
		ImageURL: &types.ChatMessageImageURL{URL: "data:image/png;base64,aW1hZ2U="},
	}}}}
	converted, apiErr := ConvertFromChatOpenai(prepared)
	if apiErr != nil {
		t.Fatalf("prepared image rejected: %v", apiErr)
	}
	part := converted.Contents[0].Parts[0]
	if part.InlineData == nil || part.InlineData.MimeType != "image/png" || part.InlineData.Data != "aW1hZ2U=" {
		t.Fatalf("prepared image was not converted to inline data: %+v", part)
	}
}

func TestConvertFromChatOpenaiDoesNotSilentlyDropImagesPastLegacyLimit(t *testing.T) {
	parts := make([]types.ChatMessagePart, GeminiVisionMaxImageNum+1)
	for i := range parts {
		parts[i] = types.ChatMessagePart{Type: types.ContentTypeImageURL, ImageURL: &types.ChatMessageImageURL{URL: "data:image/png;base64,aW1hZ2U="}}
	}
	converted, apiErr := ConvertFromChatOpenai(&types.ChatCompletionRequest{
		Model:    "gemini-2.5-pro",
		Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: parts}},
	})
	if apiErr != nil {
		t.Fatalf("convert prepared images: %v", apiErr)
	}
	if got := len(converted.Contents[0].Parts); got != len(parts) {
		t.Fatalf("converter silently dropped images: got %d parts, want %d", got, len(parts))
	}
}

func TestGeminiAPIVersionReadsJSONOther(t *testing.T) {
	channel := &model.Channel{Other: `{"api_version":"v1"}`}

	if got := geminiAPIVersion(channel); got != "v1" {
		t.Fatalf("expected JSON Other api_version, got %q", got)
	}
}

func TestChatAdapterPreservesSamplingParameters(t *testing.T) {
	topK := 17.0
	request, apiErr := ConvertFromChatOpenai(&types.ChatCompletionRequest{
		Model: "gemini-3",
		TopK:  &topK,
		Stop:  []string{"END", "STOP"},
	})
	if apiErr != nil {
		t.Fatalf("convert Gemini request: %v", apiErr)
	}
	if request.GenerationConfig.TopK == nil || *request.GenerationConfig.TopK != topK {
		t.Fatalf("top_k was not preserved: %+v", request.GenerationConfig.TopK)
	}
	if got := request.GenerationConfig.StopSequences; len(got) != 2 || got[0] != "END" || got[1] != "STOP" {
		t.Fatalf("stop sequences were not preserved: %#v", got)
	}

	request, apiErr = ConvertFromChatOpenai(&types.ChatCompletionRequest{Model: "gemini-3", Stop: "DONE"})
	if apiErr != nil || len(request.GenerationConfig.StopSequences) != 1 || request.GenerationConfig.StopSequences[0] != "DONE" {
		t.Fatalf("single stop string was not preserved: request=%+v err=%v", request, apiErr)
	}
}

func TestCleaningErrorRedactsRepeatedAPIKey(t *testing.T) {
	const key = "gemini-secret-key"
	errorInfo := &GeminiError{Message: "request gemini-secret-key failed for gemini-secret-key"}

	cleaningError(errorInfo, key)

	if strings.Contains(errorInfo.Message, key) {
		t.Fatalf("expected repeated Gemini key to be redacted, got %q", errorInfo.Message)
	}
	if got := strings.Count(errorInfo.Message, "xxxxx"); got != 2 {
		t.Fatalf("expected both key occurrences to be redacted, got %d in %q", got, errorInfo.Message)
	}
}

func TestConvertFromChatOpenAIUsesModelSpecificOfficialReasoningEffort(t *testing.T) {
	effort := "high"
	for _, test := range []struct {
		name       string
		model      string
		wantBudget *int
		wantLevel  string
	}{
		{name: "Gemini 2.5 uses budget", model: "gemini-2.5-pro", wantBudget: intPointer(24576)},
		{name: "Gemini 3 uses level", model: "gemini-3.1-pro", wantLevel: "HIGH"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := &types.ChatCompletionRequest{
				Model:               test.model,
				MaxCompletionTokens: 1024,
				ReasoningEffort:     &effort,
				Messages: []types.ChatCompletionMessage{
					{Role: types.ChatMessageRoleUser, Content: "hello"},
				},
			}

			converted, apiErr := ConvertFromChatOpenai(request)
			if apiErr != nil {
				t.Fatalf("convert request: %v", apiErr)
			}
			thinking := converted.GenerationConfig.ThinkingConfig
			if thinking == nil || thinking.ThinkingLevel != test.wantLevel {
				t.Fatalf("Gemini thinking config = %+v, want level %q", thinking, test.wantLevel)
			}
			if test.wantBudget == nil {
				if thinking.ThinkingBudget != nil {
					t.Fatalf("Gemini thinking config unexpectedly combined budget and level: %+v", thinking)
				}
			} else if thinking.ThinkingBudget == nil || *thinking.ThinkingBudget != *test.wantBudget {
				t.Fatalf("Gemini thinking budget = %+v, want %d", thinking.ThinkingBudget, *test.wantBudget)
			}
		})
	}
}

func TestGeminiExplicitThinkingBudgetDoesNotAlsoSendLevel(t *testing.T) {
	thinking := geminiThinkingConfig("gemini-3.1-pro", &types.ChatReasoning{MaxTokens: 512, Effort: "high"})
	if thinking == nil || thinking.ThinkingBudget == nil || *thinking.ThinkingBudget != 512 || thinking.ThinkingLevel != "" {
		t.Fatalf("explicit thinking budget must be the only control: %+v", thinking)
	}
}

func TestGeminiThinkingBudgetPreservesExplicitZeroAndNegativeValues(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want int
	}{
		{name: "explicit zero disables thinking", raw: `{"max_tokens":0}`, want: 0},
		{name: "negative requests dynamic thinking", raw: `{"max_tokens":-1}`, want: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var reasoning types.ChatReasoning
			if err := json.Unmarshal([]byte(test.raw), &reasoning); err != nil {
				t.Fatalf("decode reasoning: %v", err)
			}
			thinking := geminiThinkingConfig("gemini-2.5-flash", &reasoning)
			if thinking == nil || thinking.ThinkingBudget == nil || *thinking.ThinkingBudget != test.want || thinking.ThinkingLevel != "" {
				t.Fatalf("Gemini thinking config = %+v, want budget %d", thinking, test.want)
			}
		})
	}
}

func TestGeminiEffortOnlyReasoningDoesNotBecomeZeroBudget(t *testing.T) {
	var reasoning types.ChatReasoning
	if err := json.Unmarshal([]byte(`{"effort":"high"}`), &reasoning); err != nil {
		t.Fatalf("decode reasoning: %v", err)
	}
	thinking := geminiThinkingConfig("gemini-2.5-pro", &reasoning)
	if thinking == nil || thinking.ThinkingBudget == nil || *thinking.ThinkingBudget != 24576 {
		t.Fatalf("effort-only Gemini thinking config = %+v", thinking)
	}
}

func TestGeminiUsagePreservesAtomicTokenDimensionsAndAttribution(t *testing.T) {
	var metadata GeminiUsageMetadata
	if err := json.Unmarshal([]byte(`{
		"promptTokenCount":10,
		"candidatesTokenCount":3,
		"thoughtsTokenCount":1,
		"toolUsePromptTokenCount":2,
		"totalTokenCount":16,
		"cachedContentTokenCount":4,
		"serviceTier":"flex",
		"promptTokensDetails":[{"modality":"TEXT","tokenCount":7},{"modality":"IMAGE","tokenCount":3}],
		"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":3}]
	}`), &metadata); err != nil {
		t.Fatalf("decode Gemini usage: %v", err)
	}
	usage := ConvertOpenAIUsage(&metadata, "gemini-actual")
	if !usage.HasProviderUsage() || usage.PromptTokens != 12 || usage.CompletionTokens != 4 || usage.TotalTokens != 16 {
		t.Fatalf("Gemini token evidence is incomplete: %+v", usage)
	}
	if usage.PromptTokensDetails.CachedTokens != 4 || usage.PromptTokensDetails.ImageTokens != 3 || usage.GetExtraTokens()["tool_use_prompt_tokens"] != 2 {
		t.Fatalf("Gemini price dimensions were lost: %+v", usage)
	}
	if usage.ResponseModel != "gemini-actual" || usage.ServiceTier != "flex" {
		t.Fatalf("Gemini attribution was lost: %+v", usage)
	}
}

func TestGeminiUsageRejectsPartialOrConflictingTotals(t *testing.T) {
	for _, raw := range []string{
		`{"totalTokenCount":3}`,
		`{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":9}`,
	} {
		var metadata GeminiUsageMetadata
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
			t.Fatalf("decode Gemini usage: %v", err)
		}
		usage := ConvertOpenAIUsage(&metadata, "gemini-actual")
		if usage.HasProviderUsage() {
			t.Fatalf("partial/conflicting Gemini usage became priceable: raw=%s usage=%+v", raw, usage)
		}
	}
}

func TestGeminiUsageRequiresCacheAndImagePartitionsWhenRequestUsesThem(t *testing.T) {
	var metadata GeminiUsageMetadata
	if err := json.Unmarshal([]byte(`{"promptTokenCount":5,"candidatesTokenCount":0,"totalTokenCount":5}`), &metadata); err != nil {
		t.Fatalf("decode Gemini usage: %v", err)
	}
	usage := ConvertOpenAIUsage(&metadata, "gemini-actual")
	applyGeminiUsageRequirements(&usage, true, true)
	if usage.HasProviderUsage() {
		t.Fatalf("missing requested cache/image partitions became priceable: %+v", usage)
	}
}

func TestGeminiChatRejectsUnrepresentablePricedFeaturesBeforeWork(t *testing.T) {
	if _, apiErr := ConvertFromChatOpenai(&types.ChatCompletionRequest{Model: "gemini-3", ServiceTier: "flex"}); apiErr == nil || apiErr.Code != "gemini_service_tier_unsupported" {
		t.Fatalf("expected service tier rejection, got %+v", apiErr)
	}
	if _, apiErr := ConvertFromChatOpenai(&types.ChatCompletionRequest{Model: "gemini-tts", Modalities: []string{"audio"}}); apiErr == nil || apiErr.Code != "gemini_audio_output_unsupported" {
		t.Fatalf("expected audio output rejection, got %+v", apiErr)
	}
	searchRequest := &types.ChatCompletionRequest{Model: "gemini-3", Tools: []*types.ChatCompletionTool{{
		Type:     "function",
		Function: types.ChatCompletionFunction{Name: "googleSearch"},
	}}}
	if _, apiErr := ConvertFromChatOpenai(searchRequest); apiErr == nil || apiErr.Code != "gemini_grounding_billing_unsupported" {
		t.Fatalf("expected grounding billing rejection, got %+v", apiErr)
	}
}

func intPointer(value int) *int {
	return &value
}
