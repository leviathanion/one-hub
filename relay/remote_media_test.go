package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	providersBase "one-api/providers/base"
	claudeProvider "one-api/providers/claude"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

type relayRemoteMediaFetcher struct {
	calls atomic.Int32
	err   error
}

func (f *relayRemoteMediaFetcher) Fetch(string) (string, []byte, error) {
	f.calls.Add(1)
	if f.err != nil {
		return "", nil, f.err
	}
	return "image/png", []byte("prepared-image"), nil
}

type relayRemoteMediaProvider struct {
	providersBase.BaseProvider
	sendCalls atomic.Int32
	seenURL   string
}

type relayNativeClaudeMediaProvider struct {
	providersBase.BaseProvider
	sendCalls atomic.Int32
	seenType  string
}

func (p *relayNativeClaudeMediaProvider) GetRequestHeaders() map[string]string { return nil }

func (p *relayNativeClaudeMediaProvider) MaterializeNativeClaudeRemoteMedia(request *claudeProvider.ClaudeRequest, fetcher providersBase.RemoteMediaFetcher) (*claudeProvider.ClaudeRequest, error) {
	return claudeProvider.MaterializeNativeRemoteMedia(request, fetcher)
}

func (p *relayNativeClaudeMediaProvider) CreateClaudeChat(request *claudeProvider.ClaudeRequest) (*claudeProvider.ClaudeResponse, *types.OpenAIErrorWithStatusCode) {
	p.sendCalls.Add(1)
	blocks, _ := request.Messages[0].Content.([]any)
	if len(blocks) > 0 {
		block, _ := blocks[0].(map[string]any)
		source, _ := block["source"].(map[string]any)
		p.seenType, _ = source["type"].(string)
	}
	return &claudeProvider.ClaudeResponse{Id: "msg_media", Type: "message", Role: "assistant", Model: request.Model}, nil
}

func (p *relayNativeClaudeMediaProvider) CreateClaudeChatStream(*claudeProvider.ClaudeRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	panic("unexpected streaming send")
}

func (p *relayRemoteMediaProvider) GetRequestHeaders() map[string]string { return nil }

func (p *relayRemoteMediaProvider) MaterializeChatRemoteMedia(request *types.ChatCompletionRequest, fetcher providersBase.RemoteMediaFetcher) error {
	return providersBase.MaterializeChatRemoteMedia(request, fetcher)
}

func (p *relayRemoteMediaProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	p.sendCalls.Add(1)
	parts := request.Messages[0].ParseContent()
	if len(parts) > 0 && parts[0].ImageURL != nil {
		p.seenURL = parts[0].ImageURL.URL
	}
	return &types.ChatCompletionResponse{
		ID:    "chatcmpl-remote-media-test",
		Model: request.Model,
		Usage: &types.Usage{},
		Choices: []types.ChatCompletionChoice{{
			Index:   0,
			Message: types.ChatCompletionMessage{Role: types.ChatMessageRoleAssistant, Content: "ok"},
		}},
	}, nil
}

func (p *relayRemoteMediaProvider) CreateChatCompletionStream(*types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	panic("unexpected streaming send")
}

func remoteMediaChatRequest(rawURL string) types.ChatCompletionRequest {
	return types.ChatCompletionRequest{
		Model: "test-model",
		Messages: []types.ChatCompletionMessage{{
			Role: types.ChatMessageRoleUser,
			Content: []types.ChatMessagePart{{
				Type:     types.ContentTypeImageURL,
				ImageURL: &types.ChatMessageImageURL{URL: rawURL},
			}},
		}},
	}
}

func TestFinalizeMaterializesSelectedChatMediaOnceBeforeSend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	provider := &relayRemoteMediaProvider{
		BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 1, Type: config.ChannelTypeGemini}},
	}
	fetcher := &relayRemoteMediaFetcher{}
	r := &relayChat{
		relayBase:   relayBase{c: ctx, provider: provider, modelName: "test-model", originalModel: "test-model", remoteMedia: fetcher},
		chatRequest: remoteMediaChatRequest("https://example.com/a.png"),
	}

	if err := finalizeSelectedProviderRequest(r); err != nil {
		t.Fatal(err)
	}
	if fetcher.calls.Load() != 1 {
		t.Fatalf("selected remote media fetches = %d, want 1", fetcher.calls.Load())
	}
	if _, apiErr := provider.CreateChatCompletion(&r.chatRequest); apiErr != nil {
		t.Fatal(apiErr)
	}
	if fetcher.calls.Load() != 1 || provider.sendCalls.Load() != 1 {
		t.Fatalf("send repeated preparation: fetches=%d sends=%d", fetcher.calls.Load(), provider.sendCalls.Load())
	}
	if !strings.HasPrefix(provider.seenURL, "data:image/png;base64,") {
		t.Fatalf("provider did not receive prepared data URI: %q", provider.seenURL)
	}
}

func TestFinalizeRemoteMediaFailureStopsBeforeProviderWork(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	provider := &relayRemoteMediaProvider{
		BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 1, Type: config.ChannelTypeGemini}},
	}
	fetcher := &relayRemoteMediaFetcher{err: errors.New("safe fetch rejected target")}
	r := &relayChat{
		relayBase:   relayBase{c: ctx, provider: provider, modelName: "test-model", originalModel: "test-model", remoteMedia: fetcher},
		chatRequest: remoteMediaChatRequest("https://example.com/a.png"),
	}

	err := finalizeSelectedProviderRequest(r)
	if err == nil || !strings.Contains(err.Error(), "safe fetch rejected target") {
		t.Fatalf("expected pre-work materialization error, got %v", err)
	}
	if provider.sendCalls.Load() != 0 {
		t.Fatalf("provider work started after preparation failure: %d", provider.sendCalls.Load())
	}
}

func TestResponsesCompatibilityFinalizesAndReusesPreparedChatRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	provider := &relayRemoteMediaProvider{
		BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 7, Type: config.ChannelTypeGemini, CompatibleResponse: true}},
	}
	fetcher := &relayRemoteMediaFetcher{}
	store := false
	r := &relayResponses{
		relayBase: relayBase{c: ctx, provider: provider, modelName: "test-model", originalModel: "test-model", remoteMedia: fetcher},
		responsesRequest: types.OpenAIResponsesRequest{
			Model: "test-model",
			Store: &store,
			Input: []any{map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{map[string]any{
					"type":      types.ContentTypeInputImage,
					"image_url": "https://example.com/a.png",
				}},
			}},
		},
	}

	if err := finalizeSelectedProviderRequest(r); err != nil {
		t.Fatal(err)
	}
	prepared := r.preparedChatRequest
	if prepared == nil || fetcher.calls.Load() != 1 {
		t.Fatalf("responses request was not prepared once: request=%p fetches=%d", prepared, fetcher.calls.Load())
	}
	if apiErr, _ := r.compatibleSend(provider); apiErr != nil {
		t.Fatalf("compatible send failed: %+v", apiErr)
	}
	if r.preparedChatRequest != prepared || fetcher.calls.Load() != 1 || provider.sendCalls.Load() != 1 {
		t.Fatalf("compatible send rebuilt or refetched request: same=%t fetches=%d sends=%d", r.preparedChatRequest == prepared, fetcher.calls.Load(), provider.sendCalls.Load())
	}
}

func TestRemoteMediaFetchAdmissionCancellationReleasesSlots(t *testing.T) {
	if cap(remoteMediaFetchAdmission) != remoteMediaFetchConcurrency || remoteMediaFetchConcurrency != 8 {
		t.Fatalf("unexpected global admission capacity: %d", cap(remoteMediaFetchAdmission))
	}
	admission := make(chan struct{}, 1)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	first := &admittedRemoteMediaFetcher{
		ctx:       context.Background(),
		admission: admission,
		fetch: func(context.Context, string) (string, []byte, error) {
			close(firstEntered)
			<-releaseFirst
			return "image/png", []byte("ok"), nil
		},
	}
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := first.Fetch("https://example.com/first.png")
		firstDone <- err
	}()
	<-firstEntered

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	second := &admittedRemoteMediaFetcher{
		ctx:       secondCtx,
		admission: admission,
		fetch: func(context.Context, string) (string, []byte, error) {
			t.Fatal("canceled waiter entered fetch")
			return "", nil, nil
		},
	}
	secondDone := make(chan error, 1)
	go func() {
		_, _, err := second.Fetch("https://example.com/second.png")
		secondDone <- err
	}()
	cancelSecond()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting fetch error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled admission waiter did not return")
	}
	if len(admission) != 1 {
		t.Fatalf("canceled waiter changed held slots: %d", len(admission))
	}

	close(releaseFirst)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("active fetch did not finish")
	}
	if len(admission) != 0 {
		t.Fatalf("completed fetch leaked admission slot: %d", len(admission))
	}
}

func nativeClaudeRemoteMediaRequest(kind, rawURL string) *claudeProvider.ClaudeRequest {
	return &claudeProvider.ClaudeRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 16,
		Messages: []claudeProvider.Message{{Role: "user", Content: []any{
			map[string]any{"type": kind, "source": map[string]any{"type": "url", "url": rawURL}},
		}}},
	}
}

func TestFinalizeMaterializesSelectedNativeClaudeMediaOnceBeforeSend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/claude/v1/messages", nil)
	provider := &relayNativeClaudeMediaProvider{BaseProvider: providersBase.BaseProvider{
		Channel: &model.Channel{Id: 9, Type: config.ChannelTypeBedrock},
		Usage:   &types.Usage{},
	}}
	fetcher := &relayRemoteMediaFetcher{}
	request := nativeClaudeRemoteMediaRequest("image", "https://example.com/a.png")
	r := &relayClaudeOnly{
		relayBase:     relayBase{c: ctx, provider: provider, modelName: request.Model, originalModel: request.Model, remoteMedia: fetcher},
		claudeRequest: request,
	}

	if err := finalizeSelectedProviderRequest(r); err != nil {
		t.Fatal(err)
	}
	if fetcher.calls.Load() != 1 {
		t.Fatalf("native Claude fetches=%d, want 1", fetcher.calls.Load())
	}
	originalSource := request.Messages[0].Content.([]any)[0].(map[string]any)["source"].(map[string]any)
	if originalSource["type"] != "url" {
		t.Fatalf("direct/raw request projection was mutated: %#v", originalSource)
	}
	if apiErr, _ := r.send(); apiErr != nil {
		t.Fatalf("native Claude send failed: %v", apiErr)
	}
	if fetcher.calls.Load() != 1 || provider.sendCalls.Load() != 1 || provider.seenType != "base64" {
		t.Fatalf("send repeated or missed preparation: fetches=%d sends=%d source=%q", fetcher.calls.Load(), provider.sendCalls.Load(), provider.seenType)
	}
}

func TestFinalizeNativeClaudeMaterializationFailureStopsBeforeProviderWork(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/claude/v1/messages", nil)
	provider := &relayNativeClaudeMediaProvider{BaseProvider: providersBase.BaseProvider{
		Channel: &model.Channel{Id: 9, Type: config.ChannelTypeBedrock},
		Usage:   &types.Usage{},
	}}
	fetcher := &relayRemoteMediaFetcher{err: errors.New("safe fetch rejected target")}
	request := nativeClaudeRemoteMediaRequest("image", "https://example.com/a.png")
	r := &relayClaudeOnly{
		relayBase:     relayBase{c: ctx, provider: provider, modelName: request.Model, originalModel: request.Model, remoteMedia: fetcher},
		claudeRequest: request,
	}
	if err := finalizeSelectedProviderRequest(r); err == nil || !strings.Contains(err.Error(), "safe fetch rejected target") {
		t.Fatalf("expected pre-work materialization failure, got %v", err)
	}
	if provider.sendCalls.Load() != 0 {
		t.Fatalf("provider work started after materialization failure: %d", provider.sendCalls.Load())
	}
}

func TestFinalizeNativeClaudeDocumentURLRejectsWithoutFetch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/claude/v1/messages", nil)
	provider := &relayNativeClaudeMediaProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 9, Type: config.ChannelTypeBedrock}}}
	fetcher := &relayRemoteMediaFetcher{}
	request := nativeClaudeRemoteMediaRequest("document", "https://example.com/a.pdf")
	r := &relayClaudeOnly{
		relayBase:     relayBase{c: ctx, provider: provider, modelName: request.Model, originalModel: request.Model, remoteMedia: fetcher},
		claudeRequest: request,
	}
	if err := finalizeSelectedProviderRequest(r); err == nil || !strings.Contains(err.Error(), "document URL") {
		t.Fatalf("expected document URL capability failure, got %v", err)
	}
	if fetcher.calls.Load() != 0 || provider.sendCalls.Load() != 0 {
		t.Fatalf("document rejection performed work: fetches=%d sends=%d", fetcher.calls.Load(), provider.sendCalls.Load())
	}
}

func TestCountNativeClaudeTokensDoesNotPanicOnUnknownContentShapes(t *testing.T) {
	request := &claudeProvider.ClaudeRequest{Model: "claude-test", Messages: []claudeProvider.Message{{Content: []any{
		"future-block",
		map[string]any{"type": "text", "text": map[string]any{"future": true}},
		map[string]any{"type": "future", "content": []any{"opaque"}},
	}}}}
	if _, err := CountTokenMessages(request, config.PreCostDefault); err != nil {
		t.Fatalf("unknown Claude content shape returned an error: %v", err)
	}
}
