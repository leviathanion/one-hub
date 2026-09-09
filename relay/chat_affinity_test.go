package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

type chatAffinityProvider struct {
	providersBase.BaseProvider
	err    *types.OpenAIErrorWithStatusCode
	stream requester.StreamReaderInterface[string]
}

func (p *chatAffinityProvider) GetRequestHeaders() map[string]string {
	return map[string]string{}
}

func (p *chatAffinityProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	if p.err != nil {
		return nil, p.err
	}
	return &types.ChatCompletionResponse{
		ID:      "chatcmpl-affinity",
		Object:  "chat.completion",
		Model:   request.Model,
		Choices: []types.ChatCompletionChoice{},
		Usage:   &types.Usage{},
	}, nil
}

func (p *chatAffinityProvider) CreateChatCompletionStream(*types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	if p.stream != nil {
		return p.stream, nil
	}
	return nil, common.StringErrorWrapperLocal("unexpected stream call", "test_error", http.StatusInternalServerError)
}

func newChatAffinityRelay(t *testing.T, modelName, promptCacheKey, group string) (*relayChat, *httptest.ResponseRecorder) {
	t.Helper()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	body := `{"model":"` + modelName + `","messages":[{"role":"user","content":"hello"}],"prompt_cache_key":"` + promptCacheKey + `"}`
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
	ctx.Set("token_id", 101)
	ctx.Set("token_group", group)

	relay := NewRelayChat(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("set chat request: %v", err)
	}
	return relay, recorder
}

func TestChatPromptCacheAffinityRecordsAndScopesGenericModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withChannelAffinitySettings(t, config.DefaultChannelAffinitySettings())

	const (
		modelName      = "future-openai-model"
		promptCacheKey = "chat-cache-key"
	)
	seed, _ := newChatAffinityRelay(t, modelName, promptCacheKey, "team-a")
	state := currentChannelAffinityState(seed.c)
	if state == nil || state.Kind != channelAffinityKindChat || state.Lookup == nil || state.Lookup.Value != promptCacheKey {
		t.Fatalf("expected raw Chat prompt_cache_key to produce a generic affinity binding, got %#v", state)
	}
	if state.Hit || currentPreferredChannelID(seed.c) != 0 {
		t.Fatalf("expected first request to miss affinity, got state=%#v preferred=%d", state, currentPreferredChannelID(seed.c))
	}

	seed.modelName = modelName
	seed.provider = &chatAffinityProvider{BaseProvider: providersBase.BaseProvider{
		Channel: &model.Channel{Id: 77},
		Usage:   &types.Usage{},
	}}
	if apiErr, done := seed.send(); apiErr != nil || done {
		t.Fatalf("send Chat completion: err=%#v done=%v", apiErr, done)
	}

	same, _ := newChatAffinityRelay(t, modelName, promptCacheKey, "team-a")
	if got := currentPreferredChannelID(same.c); got != 77 {
		t.Fatalf("expected same scope/model/key to prefer channel 77, got %d", got)
	}
	if state := currentChannelAffinityState(same.c); state == nil || !state.Hit || state.ResumeFingerprint != "model:"+modelName {
		t.Fatalf("expected model-independent Chat affinity hit, got %#v", state)
	}

	otherGroup, _ := newChatAffinityRelay(t, modelName, promptCacheKey, "team-b")
	if got := currentPreferredChannelID(otherGroup.c); got != 0 {
		t.Fatalf("expected routing group isolation, got preferred channel %d", got)
	}
	otherModel, _ := newChatAffinityRelay(t, "another-model", promptCacheKey, "team-a")
	if got := currentPreferredChannelID(otherModel.c); got != 0 {
		t.Fatalf("expected model isolation, got preferred channel %d", got)
	}
	otherKey, _ := newChatAffinityRelay(t, modelName, "another-cache-key", "team-a")
	if got := currentPreferredChannelID(otherKey.c); got != 0 {
		t.Fatalf("expected prompt cache key isolation, got preferred channel %d", got)
	}
}

func TestChatPromptCacheAffinityRecordsOnlySuccessfulProviderAndDiagnosesFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withChannelAffinitySettings(t, config.DefaultChannelAffinitySettings())

	seed, _ := newChatAffinityRelay(t, "model-a", "cache-a", "team-a")
	recordCurrentChannelAffinity(seed.c, channelAffinityKindChat, 88)

	failed, _ := newChatAffinityRelay(t, "model-a", "cache-a", "team-a")
	failed.modelName = "model-a"
	failed.provider = &chatAffinityProvider{
		BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 99}},
		err:          common.StringErrorWrapperLocal("provider failed", "test_error", http.StatusBadGateway),
	}
	if apiErr, _ := failed.send(); apiErr == nil {
		t.Fatal("expected failed provider call to return an error")
	}
	if channelID, ok := lookupChannelAffinity(failed.c, channelAffinityKindChat, "cache-a"); !ok || channelID != 88 {
		t.Fatalf("expected failed call not to overwrite affinity, got channel=%d ok=%v", channelID, ok)
	}

	succeeded, _ := newChatAffinityRelay(t, "model-a", "cache-a", "team-a")
	if got := currentPreferredChannelID(succeeded.c); got != 88 {
		t.Fatalf("expected seeded affinity hit on channel 88, got %d", got)
	}
	succeeded.modelName = "model-a"
	succeeded.provider = &chatAffinityProvider{BaseProvider: providersBase.BaseProvider{
		Channel: &model.Channel{Id: 99},
		Usage:   &types.Usage{},
	}}
	if apiErr, done := succeeded.send(); apiErr != nil || done {
		t.Fatalf("send fallback Chat completion: err=%#v done=%v", apiErr, done)
	}
	if channelID, ok := lookupChannelAffinity(succeeded.c, channelAffinityKindChat, "cache-a"); !ok || channelID != 99 {
		t.Fatalf("expected successful fallback to refresh affinity, got channel=%d ok=%v", channelID, ok)
	}
	meta := currentChannelAffinityLogMeta(succeeded.c)
	if meta["channel_affinity_fallback"] != true || meta["channel_affinity_preferred_id"] != 88 || meta["channel_affinity_selected_id"] != 99 {
		t.Fatalf("expected affinity fallback diagnostic, got %#v", meta)
	}
}

func TestChatInBandProviderErrorDoesNotOverwriteAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withChannelAffinitySettings(t, config.DefaultChannelAffinitySettings())

	seed, _ := newChatAffinityRelay(t, "model-a", "cache-in-band", "team-a")
	recordCurrentChannelAffinity(seed.c, channelAffinityKindChat, 88)

	relay, _ := newChatAffinityRelay(t, "model-a", "cache-in-band", "team-a")
	relay.modelName = "model-a"
	stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
	stream.dataChan <- `{"error":{"message":"provider stream failed","type":"server_error","code":"stream_failed"}}`
	close(stream.dataChan)
	close(stream.errChan)
	relay.provider = &chatAffinityProvider{BaseProvider: providersBase.BaseProvider{
		Channel: &model.Channel{Id: 99, Type: config.ChannelTypeOpenAI},
		Usage:   &types.Usage{},
	}, stream: stream}
	relay.chatRequest.Stream = true
	if apiErr, _ := relay.send(); apiErr == nil || apiErr.Code != "stream_failed" {
		t.Fatalf("expected provider stream error fact, got %v", apiErr)
	}
	if channelID, ok := lookupChannelAffinity(relay.c, channelAffinityKindChat, "cache-in-band"); !ok || channelID != 88 {
		t.Fatalf("in-band provider error overwrote affinity: channel=%d ok=%v", channelID, ok)
	}
}

func TestChatPromptCacheAffinityIgnoresNonStringKeyWithoutOwningUpstreamValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withChannelAffinitySettings(t, config.DefaultChannelAffinitySettings())

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-a","messages":[{"role":"user","content":"hello"}],"prompt_cache_key":{"future":"shape"}}`))
	relay := NewRelayChat(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("proxy must leave provider-owned prompt_cache_key validation upstream, got %v", err)
	}
	if state := currentChannelAffinityState(ctx); state == nil || state.Lookup != nil || len(state.RequestBindings) != 0 || state.Hit {
		t.Fatalf("expected an unrepresentable cache key to create no local affinity binding, got %#v", state)
	}
	recordCurrentChannelAffinity(ctx, channelAffinityKindChat, 77)
	if currentChannelAffinityKey(ctx) != "" {
		t.Fatalf("expected an unrepresentable cache key not to become recordable, got %q", currentChannelAffinityKey(ctx))
	}
}
