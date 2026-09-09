package ali

import (
	"encoding/json"
	"testing"

	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

func TestI009AliEmbeddingMapsReportedTotalToInputAndPublishesIt(t *testing.T) {
	var response AliEmbeddingResponse
	if err := json.Unmarshal([]byte(`{
		"output":{"embeddings":[{"embedding":[0.1],"text_index":0}]},
		"usage":{"total_tokens":8}
	}`), &response); err != nil {
		t.Fatal(err)
	}

	provider := &AliProvider{OpenAIProvider: openai.OpenAIProvider{
		BaseProvider: base.BaseProvider{Usage: &types.Usage{PromptTokens: 99}},
	}}
	request := &types.EmbeddingRequest{Model: "text-embedding-v1", Input: "hello"}
	public, apiErr := provider.convertToEmbeddingOpenai(&response, request)
	if apiErr != nil || public == nil || public.Usage == nil {
		t.Fatalf("embedding usage was not converted: response=%+v err=%+v", public, apiErr)
	}
	want := &types.Usage{
		PromptTokens:     8,
		CompletionTokens: 0,
		TotalTokens:      8,
		ProviderReported: true,
		ProviderTokenFields: map[string]bool{
			"prompt_tokens":     true,
			"completion_tokens": true,
			"total_tokens":      true,
		},
	}
	if public.Usage.PromptTokens != want.PromptTokens || public.Usage.CompletionTokens != want.CompletionTokens || public.Usage.TotalTokens != want.TotalTokens || !public.Usage.ProviderReported || !public.Usage.ProviderTokenFields["prompt_tokens"] || !public.Usage.ProviderTokenFields["completion_tokens"] || !public.Usage.ProviderTokenFields["total_tokens"] {
		t.Fatalf("public embedding usage mismatch: got=%+v", public.Usage)
	}
	providerUsage := provider.GetUsage()
	if providerUsage.PromptTokens != public.Usage.PromptTokens || providerUsage.CompletionTokens != public.Usage.CompletionTokens || providerUsage.TotalTokens != public.Usage.TotalTokens || providerUsage.ProviderReported != public.Usage.ProviderReported {
		t.Fatalf("provider and public embedding usage diverged: provider=%+v public=%+v", providerUsage, public.Usage)
	}
	if !public.Usage.HasProviderUsage() {
		t.Fatalf("reported embedding usage did not authorize its valid input evidence: %+v", public.Usage)
	}

	var wire struct {
		Usage *types.Usage `json:"usage"`
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Usage == nil || wire.Usage.PromptTokens != 8 || wire.Usage.CompletionTokens != 0 || wire.Usage.TotalTokens != 8 {
		t.Fatalf("public embedding wire usage mismatch: %s", encoded)
	}
}

func TestI009AliEmbeddingUsageRequiresNonNegativePresentTotal(t *testing.T) {
	for _, test := range []struct {
		name       string
		usage      string
		wantValid  bool
		wantTokens int
	}{
		{name: "缺少usage", wantValid: false},
		{name: "null_usage", usage: `null`, wantValid: false},
		{name: "缺少total", usage: `{"input_tokens":8}`, wantValid: false},
		{name: "null_total", usage: `{"total_tokens":null}`, wantValid: false},
		{name: "negative_total", usage: `{"total_tokens":-1}`, wantValid: false},
		{name: "explicit_zero", usage: `{"total_tokens":0}`, wantValid: true, wantTokens: 0},
		{name: "positive_total", usage: `{"total_tokens":8}`, wantValid: true, wantTokens: 8},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := `{"output":{"embeddings":[]}`
			if test.usage != "" {
				raw += `,"usage":` + test.usage
			}
			raw += `}`

			var response AliEmbeddingResponse
			if err := json.Unmarshal([]byte(raw), &response); err != nil {
				t.Fatal(err)
			}
			provider := &AliProvider{OpenAIProvider: openai.OpenAIProvider{
				BaseProvider: base.BaseProvider{Usage: &types.Usage{PromptTokens: 99}},
			}}
			public, apiErr := provider.convertToEmbeddingOpenai(&response, &types.EmbeddingRequest{Model: "text-embedding-v1"})
			if apiErr != nil || public == nil {
				t.Fatalf("unexpected conversion error: response=%+v err=%+v", public, apiErr)
			}
			if (public.Usage != nil) != test.wantValid {
				t.Fatalf("embedding usage presence=%t want=%t: %+v", public.Usage != nil, test.wantValid, public.Usage)
			}
			if !test.wantValid {
				if provider.GetUsage().ProviderReported || provider.GetUsage().HasProviderUsage() {
					t.Fatalf("invalid embedding usage authorized billing: %+v", provider.GetUsage())
				}
				return
			}
			if public.Usage.PromptTokens != test.wantTokens || public.Usage.CompletionTokens != 0 || public.Usage.TotalTokens != test.wantTokens || !public.Usage.HasProviderUsage() {
				t.Fatalf("valid embedding usage mismatch: %+v", public.Usage)
			}
		})
	}
}

func TestI009AliEmbeddingErrorDoesNotCopyUsage(t *testing.T) {
	var response AliEmbeddingResponse
	if err := json.Unmarshal([]byte(`{
		"code":"InvalidParameter","message":"upstream rejected",
		"output":{"embeddings":[]},"usage":{"total_tokens":8}
	}`), &response); err != nil {
		t.Fatal(err)
	}
	provider := &AliProvider{OpenAIProvider: openai.OpenAIProvider{
		BaseProvider: base.BaseProvider{Usage: &types.Usage{PromptTokens: 99}},
	}}
	public, apiErr := provider.convertToEmbeddingOpenai(&response, &types.EmbeddingRequest{Model: "text-embedding-v1"})
	if public != nil || apiErr == nil {
		t.Fatalf("upstream embedding error was not preserved: response=%+v err=%+v", public, apiErr)
	}
	if provider.GetUsage().ProviderReported || provider.GetUsage().HasProviderUsage() {
		t.Fatalf("upstream embedding error authorized billing: %+v", provider.GetUsage())
	}
}

func TestI009AliChatUsageStillRequiresCompleteNonNullSnapshot(t *testing.T) {
	for _, test := range []struct {
		name      string
		raw       string
		wantValid bool
	}{
		{name: "完整", raw: `{"input_tokens":4,"output_tokens":2,"total_tokens":6}`, wantValid: true},
		{name: "缺input", raw: `{"output_tokens":2,"total_tokens":2}`},
		{name: "null_input", raw: `{"input_tokens":null,"output_tokens":2,"total_tokens":2}`},
		{name: "冲突total", raw: `{"input_tokens":4,"output_tokens":2,"total_tokens":5}`},
		{name: "完整显式零", raw: `{"input_tokens":0,"output_tokens":0,"total_tokens":0}`, wantValid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var usage AliUsage
			if err := json.Unmarshal([]byte(test.raw), &usage); err != nil {
				t.Fatal(err)
			}
			converted := aliUsageToOpenAI(&usage)
			if converted == nil {
				t.Fatal("chat usage converter returned nil")
			}
			if converted.HasProviderUsage() != test.wantValid {
				t.Fatalf("chat usage authorization=%t want=%t: %+v", converted.HasProviderUsage(), test.wantValid, converted)
			}
		})
	}
}
