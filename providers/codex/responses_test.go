package codex

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/types"
)

type fakeStringStream struct {
	dataChan  chan string
	errChan   chan error
	closed    chan struct{}
	closeOnce sync.Once
}

func (s *fakeStringStream) Recv() (<-chan string, <-chan error) {
	return s.dataChan, s.errChan
}

func (s *fakeStringStream) Close() {
	if s == nil || s.closed == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.closed)
	})
}

func TestCollectResponsesStreamResponseAcceptsDataWithoutSpace(t *testing.T) {
	provider := &CodexProvider{}
	provider.Usage = &types.Usage{}

	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n"
		stream.errChan <- io.EOF
	}()

	resp, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		t.Fatalf("collectResponsesStreamResponse returned error: %v", errWithCode.Message)
	}

	if resp == nil || resp.ID != "resp_1" {
		t.Fatalf("unexpected response: %#v", resp)
	}

	if provider.Usage.TotalTokens != 8 {
		t.Fatalf("expected usage total tokens to be updated, got %d", provider.Usage.TotalTokens)
	}
}

func TestCollectResponsesStreamResponsePreservesEmptyReasoningSummary(t *testing.T) {
	provider := &CodexProvider{}
	provider.Usage = &types.Usage{}

	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"id\":\"rs_1\",\"status\":\"completed\",\"summary\":[]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n"
		stream.errChan <- io.EOF
	}()

	resp, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		t.Fatalf("collectResponsesStreamResponse returned error: %v", errWithCode.Message)
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	if !strings.Contains(string(data), "\"summary\":[]") {
		t.Fatalf("expected marshaled response to preserve empty summary array, got %s", string(data))
	}
}

func TestCollectResponsesStreamResponseAcceptsWrappedEOF(t *testing.T) {
	provider := &CodexProvider{}
	provider.Usage = &types.Usage{}

	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_wrapped_eof\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n"
		stream.errChan <- fmt.Errorf("wrapped: %w", io.EOF)
	}()

	resp, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		t.Fatalf("expected wrapped EOF to terminate stream cleanly, got %v", errWithCode.Message)
	}
	if resp == nil || resp.ID != "resp_wrapped_eof" {
		t.Fatalf("expected wrapped EOF response, got %#v", resp)
	}
}

func TestAdaptCodexCLIAppliesMinimalDefaultInstructions(t *testing.T) {
	provider := &CodexProvider{}
	request := &types.OpenAIResponsesRequest{
		Model:           "gpt-5",
		Instructions:    "",
		MaxOutputTokens: 512,
		Temperature:     ptrFloat64(0.7),
		TopP:            ptrFloat64(0.9),
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	}

	provider.adaptCodexCLI(request)

	if request.Instructions != CodexCLIInstructions {
		t.Fatalf("expected minimal default instructions %q, got %q", CodexCLIInstructions, request.Instructions)
	}
	if request.MaxOutputTokens != 0 {
		t.Fatalf("expected max_output_tokens to be cleared, got %d", request.MaxOutputTokens)
	}
	if request.Temperature != nil {
		t.Fatalf("expected temperature to be cleared")
	}
	if request.TopP != nil {
		t.Fatalf("expected top_p to be cleared")
	}
}

func TestPrepareCodexRequestRemovesUnsupportedContextManagementAndTruncation(t *testing.T) {
	provider := &CodexProvider{}
	request := &types.OpenAIResponsesRequest{
		Model:      "gpt-5",
		Truncation: "disabled",
		ContextManagement: []map[string]any{
			{
				"type":              "compaction",
				"compact_threshold": 12000,
			},
		},
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	}

	provider.prepareCodexRequest(request)

	if request.ContextManagement != nil {
		t.Fatalf("expected context_management to be removed, got %#v", request.ContextManagement)
	}
	if request.Truncation != "" {
		t.Fatalf("expected truncation to be removed, got %q", request.Truncation)
	}
}

func TestPrepareCodexRequestEnsuresReasoningEncryptedContentInclude(t *testing.T) {
	provider := &CodexProvider{}
	request := &types.OpenAIResponsesRequest{
		Model:   "gpt-5",
		Include: []any{"output_text.annotations"},
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	}

	provider.prepareCodexRequest(request)

	includes, ok := request.Include.([]string)
	if !ok {
		t.Fatalf("expected include to normalize to []string, got %T", request.Include)
	}
	if len(includes) != 2 {
		t.Fatalf("expected 2 include values, got %#v", includes)
	}
	if includes[0] != "output_text.annotations" {
		t.Fatalf("expected existing include to be preserved, got %#v", includes)
	}
	if includes[1] != codexReasoningEncryptedContentInclude {
		t.Fatalf("expected encrypted content include to be appended, got %#v", includes)
	}
}

func TestPrepareCodexRequestNormalizesBuiltinToolAliases(t *testing.T) {
	provider := &CodexProvider{}
	request := &types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Tools: []types.ResponsesTools{
			{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "medium"},
		},
		ToolChoice: map[string]any{
			"type": "web_search_preview_2025_03_11",
			"tools": []any{
				map[string]any{"type": types.APIToolTypeWebSearchPreview},
			},
		},
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	}

	provider.prepareCodexRequest(request)

	if request.Tools[0].Type != "web_search" {
		t.Fatalf("expected tools alias to normalize, got %q", request.Tools[0].Type)
	}

	toolChoice, ok := request.ToolChoice.(map[string]any)
	if !ok {
		t.Fatalf("expected tool_choice map, got %T", request.ToolChoice)
	}
	if toolChoice["type"] != "web_search" {
		t.Fatalf("expected tool_choice type to normalize, got %#v", toolChoice["type"])
	}
	tools, ok := toolChoice["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected tool_choice.tools to survive normalization, got %#v", toolChoice["tools"])
	}
	firstTool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("expected nested tool map, got %T", tools[0])
	}
	if firstTool["type"] != "web_search" {
		t.Fatalf("expected nested tool type to normalize, got %#v", firstTool["type"])
	}
}

func TestCodexResponsesSearchTypeRecognizesNormalizedWebSearchAlias(t *testing.T) {
	response := &types.OpenAIResponsesResponses{
		Tools: []types.ResponsesTools{
			{Type: types.APIToolTypeWebSearch, SearchContextSize: "high"},
		},
	}

	if got := codexResponsesSearchType(response); got != "high" {
		t.Fatalf("expected normalized web_search alias to preserve search_context_size, got %q", got)
	}
}

func TestCompactResponsesUsesCompactEndpoint(t *testing.T) {
	var (
		gotPath   string
		gotAccept string
		bodyBytes []byte
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")

		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_cmp_1","object":"response.compaction","output":[{"id":"cmp_1","type":"compaction","encrypted_content":"ciphertext"}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL

	resp, errWithCode := provider.CompactResponses(&types.OpenAIResponsesRequest{
		Model:   "gpt-5",
		Input:   "hello",
		Stream:  true,
		Include: []string{"reasoning.encrypted_content", "custom.include"},
	})
	if errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	if gotPath != "/backend-api/codex/responses/compact" {
		t.Fatalf("expected compact endpoint path, got %q", gotPath)
	}
	if gotAccept != "application/json" {
		t.Fatalf("expected JSON accept header, got %q", gotAccept)
	}

	var raw map[string]any
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		t.Fatalf("failed to decode upstream request body: %v", err)
	}
	if _, exists := raw["stream"]; exists {
		t.Fatalf("expected compact request body to omit stream, got %s", string(bodyBytes))
	}
	if _, exists := raw["context_management"]; exists {
		t.Fatalf("expected compact request body to omit context_management, got %s", string(bodyBytes))
	}
	if _, exists := raw["truncation"]; exists {
		t.Fatalf("expected compact request body to omit truncation, got %s", string(bodyBytes))
	}
	if _, exists := raw["include"]; exists {
		t.Fatalf("expected compact request body to omit include, got %s", string(bodyBytes))
	}

	if resp.Object != "response.compaction" {
		t.Fatalf("expected compaction object, got %q", resp.Object)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "compaction" {
		t.Fatalf("expected compaction output item, got %#v", resp.Output)
	}
	if resp.Output[0].EncryptedContent == nil || *resp.Output[0].EncryptedContent != "ciphertext" {
		t.Fatalf("expected encrypted content to be preserved, got %#v", resp.Output[0].EncryptedContent)
	}
	if provider.Usage.TotalTokens != 18 {
		t.Fatalf("expected provider usage to be updated, got %d", provider.Usage.TotalTokens)
	}
}

func TestCompactResponsesBackfillsPromptCacheKeyFromRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_cmp_2","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL

	resp, errWithCode := provider.CompactResponses(&types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "stable-cache-key",
	})
	if errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	if resp.PromptCacheKey != "stable-cache-key" {
		t.Fatalf("expected response prompt_cache_key to be backfilled, got %q", resp.PromptCacheKey)
	}
}

func TestCompactResponsesDoesNotBackfillUsageFromRetainedOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_cmp_3","object":"response.compaction","output":[{"id":"msg_1","type":"message","role":"user","content":[{"type":"input_text","text":"retained context that should not be billed as completion"}]},{"id":"cmp_1","type":"compaction","encrypted_content":"ciphertext"}]}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{
		PromptTokens:     99,
		CompletionTokens: 77,
		TotalTokens:      176,
	}

	resp, errWithCode := provider.CompactResponses(&types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: "hello",
	})
	if errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	if resp.Usage == nil {
		t.Fatalf("expected usage to be initialized")
	}
	if resp.Usage.InputTokens != 99 || resp.Usage.OutputTokens != 0 || resp.Usage.TotalTokens != 99 {
		t.Fatalf("expected missing compact usage to preserve prompt tokens without output backfill, got %#v", resp.Usage)
	}
	if provider.Usage.PromptTokens != 99 || provider.Usage.CompletionTokens != 0 || provider.Usage.TotalTokens != 99 {
		t.Fatalf("expected provider usage to preserve prompt tokens without output backfill, got %#v", provider.Usage)
	}
}

func TestCompactResponsesPreservesDetailedUsageAndExtraBilling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_cmp_4",
			"object":"response.compaction",
			"tools":[{"type":"web_search_preview","search_context_size":"high"}],
			"output":[{"id":"ws_1","type":"web_search_call","status":"completed"}],
			"usage":{
				"input_tokens":11,
				"output_tokens":7,
				"total_tokens":18,
				"input_tokens_details":{"cached_tokens":4,"text_tokens":2,"image_tokens":3},
				"output_tokens_details":{"reasoning_tokens":5}
			}
		}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{PromptTokens: 11}

	_, errWithCode := provider.CompactResponses(&types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: "hello",
	})
	if errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	if provider.Usage.PromptTokensDetails.CachedTokens != 4 || provider.Usage.PromptTokensDetails.TextTokens != 2 || provider.Usage.PromptTokensDetails.ImageTokens != 3 {
		t.Fatalf("expected compact usage details to be preserved, got %#v", provider.Usage.PromptTokensDetails)
	}
	if provider.Usage.CompletionTokensDetails.ReasoningTokens != 5 {
		t.Fatalf("expected compact reasoning usage to be preserved, got %#v", provider.Usage.CompletionTokensDetails)
	}
	billing, ok := provider.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected compact responses to preserve tool extra billing, got %+v", provider.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search charge, got %+v", billing)
	}
}

func TestCreateResponsesBackfillsUsageAndExtraBillingWithoutTerminalUsage(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.created\",\"response\":{\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}]}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.added\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.output_item.added\",\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_1\",\"status\":\"completed\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.output_text.delta\",\"delta\":\"hello from codex\"}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_4\",\"object\":\"response\",\"status\":\"completed\",\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}],\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello from codex\"}]},{\"id\":\"ws_1\",\"type\":\"web_search_call\",\"status\":\"completed\"}]}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{PromptTokens: 11}

	resp, errWithCode := provider.CreateResponses(&types.OpenAIResponsesRequest{
		Model: "gpt-3.5-turbo",
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("CreateResponses returned error: %v", errWithCode.Message)
	}

	if resp.Usage == nil || resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens <= 0 || resp.Usage.TotalTokens <= 11 {
		t.Fatalf("expected missing terminal usage to be backfilled from response content, got %#v", resp.Usage)
	}
	if provider.Usage.CompletionTokens <= 0 || provider.Usage.TotalTokens <= provider.Usage.PromptTokens {
		t.Fatalf("expected provider usage completion tokens to be backfilled, got %#v", provider.Usage)
	}
	billing, ok := provider.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected create responses to preserve tool extra billing, got %+v", provider.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search charge, got %+v", billing)
	}
}

func TestCodexResponsesStreamHandlerAccumulatesToolBillingAndTextFallbackState(t *testing.T) {
	handler := newCodexResponsesStreamHandler(&types.Usage{})

	dataChan := make(chan string, 8)
	errChan := make(chan error, 1)

	created := []byte(`data: {"type":"response.created","response":{"tools":[{"type":"web_search_preview","search_context_size":"high"}]}}`)
	handler.HandlerResponsesStream(&created, dataChan, errChan)

	added := []byte(`data: {"type":"response.output_item.added","item":{"type":"web_search_call","id":"ws_1","status":"completed"}}`)
	handler.HandlerResponsesStream(&added, dataChan, errChan)

	delta := []byte(`data: {"type":"response.output_text.delta","delta":"hello from codex"}`)
	handler.HandlerResponsesStream(&delta, dataChan, errChan)

	if got := handler.Usage.TextBuilder.String(); got != "hello from codex" {
		t.Fatalf("expected output text delta to accumulate for fallback counting, got %q", got)
	}
	billing, ok := handler.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected stream handler to preserve tool extra billing, got %+v", handler.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search charge, got %+v", billing)
	}
}

func TestCodexResponsesStreamHandlerDoesNotDoubleCountTerminalToolBilling(t *testing.T) {
	handler := newCodexResponsesStreamHandler(&types.Usage{})

	dataChan := make(chan string, 8)
	errChan := make(chan error, 1)

	created := []byte(`data: {"type":"response.created","response":{"tools":[{"type":"web_search_preview","search_context_size":"high"}]}}`)
	handler.HandlerResponsesStream(&created, dataChan, errChan)

	added := []byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"web_search_call","id":"ws_1","status":"completed"}}`)
	handler.HandlerResponsesStream(&added, dataChan, errChan)

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8},"tools":[{"type":"web_search_preview","search_context_size":"high"}],"output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"id":"ws_1","type":"web_search_call","status":"completed"}]}}`)
	handler.HandlerResponsesStream(&completed, dataChan, errChan)

	billing, ok := handler.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected terminal stream handler to preserve tool extra billing, got %+v", handler.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected terminal stream handler to charge web search once, got %+v", billing)
	}
}

func TestCreateResponsesStreamConvertChatDoesNotDoubleCountToolBilling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.created\",\"response\":{\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}]}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.added\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_1\",\"status\":\"completed\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream_chat\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8},\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}],\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello from codex\"}]},{\"id\":\"ws_1\",\"type\":\"web_search_call\",\"status\":\"completed\"}]}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{PromptTokens: 11}

	stream, errWithCode := provider.CreateResponsesStream(&types.OpenAIResponsesRequest{
		Model:       "gpt-5",
		ConvertChat: true,
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("CreateResponsesStream returned error: %v", errWithCode.Message)
	}
	defer stream.Close()

	dataChan, errChan := stream.Recv()
	receivedChunks := 0
	for receivedChunks < 2 {
		select {
		case _, ok := <-dataChan:
			if !ok {
				receivedChunks = 2
				continue
			}
			receivedChunks++
		case err, ok := <-errChan:
			if !ok {
				receivedChunks = 2
				continue
			}
			if err != nil && err != io.EOF {
				t.Fatalf("unexpected stream error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for convert-chat stream output")
		}
	}

	billing, ok := provider.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected convert-chat stream to preserve tool extra billing, got %+v", provider.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected convert-chat stream to charge web search once, got %+v", billing)
	}
}

func TestCreateResponsesBackfillsPromptCacheKeyFromRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_3\",\"object\":\"response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL

	resp, errWithCode := provider.CreateResponses(&types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "stable-cache-key",
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("CreateResponses returned error: %v", errWithCode.Message)
	}

	if resp.PromptCacheKey != "stable-cache-key" {
		t.Fatalf("expected response prompt_cache_key to be backfilled, got %q", resp.PromptCacheKey)
	}
}

func ptrFloat64(v float64) *float64 {
	return &v
}
