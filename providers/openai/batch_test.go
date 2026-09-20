package openai

import (
	"bytes"
	"testing"

	"one-api/common/config"
	"one-api/common/providerendpoint"
	"one-api/model"
	"one-api/types"
)

func TestBatchRelayURLNativeCapabilityAndPath(t *testing.T) {
	for _, channelType := range []int{config.ChannelTypeOpenAI, config.ChannelTypeAzureV1, config.ChannelTypeAzure, config.ChannelTypeGemini} {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{Type: channelType, Proxy: &proxy}, "https://provider.example")
		got, err := provider.BuildBatchRelayURL("/v1/batches/batch%3Aid/cancel", "future=a%2Bb&future=c")
		supported := channelType == config.ChannelTypeOpenAI || channelType == config.ChannelTypeAzureV1
		if supported && (err != nil || got != "https://provider.example/v1/batches/batch%3Aid/cancel?future=a%2Bb&future=c") {
			t.Fatalf("type=%d url=%q err=%v", channelType, got, err)
		}
		if !supported && err == nil {
			t.Fatalf("unsupported type=%d accepted", channelType)
		}
		for _, path := range []string{"/v1/batches_other", "//evil.example/v1/batches", "/v1/batches/", "/v1/batches/../files", "/v1/batches/%2e%2e", "/v1/batches/id%2Fcancel", "/v1/batches/bad%", "/v1/batches/id/unknown", "/v1/batches/id/cancel/extra"} {
			if _, err := provider.BuildBatchRelayURL(path, ""); err == nil {
				t.Fatalf("invalid path %q accepted", path)
			}
		}
	}
}

func TestBatchRelayURLCustomEndpoint(t *testing.T) {
	proxy := ""
	channel := &model.Channel{Type: config.ChannelTypeCustom, Proxy: &proxy, Plugin: model.NewCustomEndpointPlugin()}
	provider := CreateOpenAIProvider(channel, "https://unused.example")
	if _, err := provider.BuildBatchRelayURL("/v1/batches", ""); err == nil {
		t.Fatal("disabled custom endpoint accepted")
	}
	channel.Plugin.Data()["endpoints"][providerendpoint.Batches] = (providerendpoint.Setting{Enabled: true, UpstreamURL: "https://managed.example/jobs?fixed=a%2Fb"}).Data()
	got, err := provider.BuildBatchRelayURL("/v1/batches/batch%3Aid", "after=x%2Fy&after=z")
	if err != nil || got != "https://managed.example/jobs/batch%3Aid?fixed=a%2Fb&after=x%2Fy&after=z" {
		t.Fatalf("url=%q err=%v", got, err)
	}
}

func TestBatchUsageExtractsProviderEvidenceWithoutChangingWire(t *testing.T) {
	provider := &OpenAIProvider{}
	for _, tc := range []struct {
		name, endpoint, body, model, tier string
		prompt, completion                int
	}{
		{"chat", "/v1/chat/completions", `{"model":"actual-chat","service_tier":"flex","choices":{"future":true},"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`, "actual-chat", "flex", 3, 4},
		{"completion", "/v1/completions", `{"model":"actual-completion","usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, "actual-completion", "", 2, 1},
		{"embedding", "/v1/embeddings", `{"model":"actual-embedding","data":{"future":true},"usage":{"prompt_tokens":5,"total_tokens":5}}`, "actual-embedding", "", 5, 0},
		{"responses", "/v1/responses", `{"id":"resp_x","model":"actual-response","service_tier":"default","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9},"future":{"union":[true]}}`, "actual-response", "default", 7, 2},
		{"moderation explicit usage", "/v1/moderations", `{"model":"actual-moderation","usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`, "actual-moderation", "", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.body)
			original := bytes.Clone(raw)
			u := provider.ExtractBatchUsage(tc.endpoint, raw)
			if !u.ProviderReported || u.PromptTokens != tc.prompt || u.CompletionTokens != tc.completion || u.ResponseModel != tc.model || u.ServiceTier != tc.tier {
				t.Fatalf("unexpected evidence: %+v", u)
			}
			if !bytes.Equal(original, raw) {
				t.Fatal("observer modified result body")
			}
		})
	}
}

func TestBatchUsageDoesNotInventEvidence(t *testing.T) {
	provider := &OpenAIProvider{}
	for _, tc := range []struct{ endpoint, body string }{
		{"/v1/chat/completions", `{"model":"actual","choices":[{}]}`},
		{"/v1/chat/completions", `{"usage":"future"}`},
		{"/v1/chat/completions", `{"usage":`},
		{"/v1/embeddings", `{"usage":{"total_tokens":5}}`},
		{"/v1/embeddings", `{"usage":{"prompt_tokens":5,"completion_tokens":1}}`},
		{"/v1/images/generations", `{"model":"image","n":9}`},
		{"/v1/moderations", `{"results":[{"flagged":false}]}`},
		{"/v1/unknown", `{"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`},
	} {
		u := provider.ExtractBatchUsage(tc.endpoint, []byte(tc.body))
		if u.HasProviderBaseUsage() || u.ProviderOperationUnits != nil {
			t.Fatalf("invented evidence for %s %s: %+v", tc.endpoint, tc.body, u)
		}
	}
}

func TestBatchImageUsageCountsOnlyObservedOutput(t *testing.T) {
	for _, endpoint := range []string{"/v1/images/generations", "/v1/images/edits"} {
		u := (&OpenAIProvider{}).ExtractBatchUsage(endpoint, []byte(`{"model":"actual-image","n":99,"data":[{"future_image":"opaque"},{"b64_json":"abcd"}]}`))
		if u.ProviderOperationUnits == nil || *u.ProviderOperationUnits != 2 || u.ResponseModel != "actual-image" || u.HasProviderBaseUsage() {
			t.Fatalf("unexpected image evidence: %+v", u)
		}
	}
}

func TestBatchResponsesIndependentToolEvidence(t *testing.T) {
	u := (&OpenAIProvider{}).ExtractBatchUsage("/v1/responses", []byte(`{"id":"resp_tool","model":"actual","output":[{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search"}}]}`))
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if u.HasProviderBaseUsage() || u.ExtraBilling[key].CallCount != 1 || !u.HasProviderExtraBilling(key) {
		t.Fatalf("independent tool evidence lost or tokens invented: %+v", u)
	}
}

func TestBatchImageUsageKeepsTokenPartitions(t *testing.T) {
	u := (&OpenAIProvider{}).ExtractBatchUsage("/v1/images/generations", []byte(`{"model":"actual-image","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"text_tokens":6,"image_tokens":4},"output_tokens_details":{"text_tokens":1,"image_tokens":4}}}`))
	if !u.HasProviderUsage() || len(u.TokenExtraEvidenceGroups) != 2 || u.ProviderOperationUnits != nil {
		t.Fatalf("image partitions lost or operation count invented: %+v", u)
	}
	if u.GetExtraTokens()[config.UsageExtraOutputImageTokens] != 4 {
		t.Fatalf("image token detail lost: %+v", u)
	}
}
