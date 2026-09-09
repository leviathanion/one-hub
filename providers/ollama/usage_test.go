package ollama

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"one-api/providers/base"
	"one-api/types"
)

func TestConvertFromChatOpenaiOnlyAcceptsPreparedImageData(t *testing.T) {
	mode, policyErr := (OllamaProviderFactory{}).AssessChatRemoteMedia(nil, &types.ChatCompletionRequest{}, base.ChatRemoteMediaSummary{Items: 1, RemoteURLs: 1})
	if policyErr != nil || mode != base.RemoteMediaMaterialize {
		t.Fatalf("Ollama remote media policy = %v, %v; want Materialize", mode, policyErr)
	}
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gets.Add(1)
		_, _ = w.Write([]byte("unexpected"))
	}))
	t.Cleanup(server.Close)

	remote := &types.ChatCompletionRequest{
		Model: "llama3",
		Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: []types.ChatMessagePart{{
			Type:     types.ContentTypeImageURL,
			ImageURL: &types.ChatMessageImageURL{URL: server.URL + "/image.png"},
		}}}},
	}
	if _, apiErr := convertFromChatOpenai(remote); apiErr == nil || apiErr.Code != "image_url_invalid" {
		t.Fatalf("unprepared remote image was accepted: %v", apiErr)
	}
	if got := gets.Load(); got != 0 {
		t.Fatalf("converter issued %d network requests, want 0", got)
	}

	remote.Messages[0].Content = []types.ChatMessagePart{{
		Type:     types.ContentTypeImageURL,
		ImageURL: &types.ChatMessageImageURL{URL: "data:image/png;base64,aW1hZ2U="},
	}}
	converted, apiErr := convertFromChatOpenai(remote)
	if apiErr != nil {
		t.Fatalf("prepared image rejected: %v", apiErr)
	}
	if len(converted.Messages) != 1 || len(converted.Messages[0].Images) != 1 || converted.Messages[0].Images[0] != "aW1hZ2U=" {
		t.Fatalf("prepared image was not converted: %+v", converted.Messages)
	}
}

func TestOllamaUsageUsesTerminalPromptAndEvalCounts(t *testing.T) {
	provider := &OllamaProvider{BaseProvider: base.BaseProvider{Usage: &types.Usage{}}}
	var providerResponse ChatResponse
	if err := json.Unmarshal([]byte(`{"model":"llama3","done":true,"prompt_eval_count":5,"eval_count":0}`), &providerResponse); err != nil {
		t.Fatal(err)
	}
	response, apiErr := provider.convertToChatOpenai(&providerResponse, &types.ChatCompletionRequest{Model: "llama3"})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if response.Usage == nil || !response.Usage.HasProviderUsage() || response.Usage.PromptTokens != 5 || response.Usage.CompletionTokens != 0 || response.Usage.TotalTokens != 5 {
		t.Fatalf("Ollama terminal counts did not produce provider usage: %+v", response.Usage)
	}

	if err := json.Unmarshal([]byte(`{"model":"llama3","done":true,"prompt_eval_count":5}`), &providerResponse); err != nil {
		t.Fatal(err)
	}
	response, apiErr = provider.convertToChatOpenai(&providerResponse, &types.ChatCompletionRequest{Model: "llama3"})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if response.Usage.HasProviderUsage() {
		t.Fatalf("missing Ollama eval_count became provider evidence: %+v", response.Usage)
	}
}
