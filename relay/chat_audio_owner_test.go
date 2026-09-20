package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"one-api/common/config"
	"one-api/model"
	"one-api/providers/openai"
	"one-api/types"
)

func newChatAudioOwnerRelay(t *testing.T, channel *model.Channel, stream bool) (*relayChat, *openai.OpenAIProvider, func()) {
	t.Helper()
	ctx, _ := resourceHTTPContext(http.MethodPost, "/v1/chat/completions", nil)
	provider := openai.CreateOpenAIProvider(channel, channel.GetBaseURL())
	provider.SetContext(ctx)
	provider.SetUsage(&types.Usage{})
	r := NewRelayChat(ctx)
	r.provider = provider
	r.modelName = "test"
	r.chatRequest = types.ChatCompletionRequest{Model: "test", Stream: stream, Modalities: []string{"text", "audio"}}
	cleanup, apiErr := r.prepareChatAudioOwnership()
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	return r, provider, cleanup
}

func TestChatAudioOwnerJSONCommitsBeforeDeliveryWithoutWaitingForExpiry(t *testing.T) {
	const wire = `{"id":"chat1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","audio":{"id":"audio_owned","data":"opaque","future":1e3}}}]}`
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, wire) })
	r, p, cleanup := newChatAudioOwnerRelay(t, channel, false)
	defer cleanup()
	response, apiErr := p.CreateChatCompletion(&r.chatRequest)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if got := string(response.ReplayProviderRawJSON()); got != wire {
		t.Fatalf("raw audio output changed: %s", got)
	}
	owner, err := model.GetResourceOwner(context.Background(), "chat_audio", "audio_owned", 101)
	if err != nil || owner.ChannelID != channel.Id || owner.UpstreamExpiresAt != nil || owner.RetainUntil == nil {
		t.Fatalf("owner barrier/retention: %+v %v", owner, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("requests=%d", calls.Load())
	}
	raw := []byte(`{"messages":[{"role":"assistant","audio":{"id":"audio_owned"}}]}`)
	ctx, _ := resourceHTTPContext(http.MethodPost, "/v1/chat/completions", nil)
	if err := prepareResourceRequest(ctx, raw, "chat"); err != nil {
		t.Fatalf("missing upstream expiry blocked authorized reference: %v", err)
	}
	ctx.Set("id", 102)
	if err := prepareResourceRequest(ctx, raw, "chat"); err == nil {
		t.Fatal("another user referenced audio")
	}
	ctx.Set("id", 101)
	ctx.Set("channel_id", channel.Id+1)
	if err := prepareResourceRequest(ctx, raw, "chat"); err == nil {
		t.Fatal("cross-channel audio reference accepted")
	}
}

func TestChatAudioOwnerSSEDeliversIDBeforeLateExpiredMetadata(t *testing.T) {
	const first = "data: {\"id\":\"chat1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"audio\":{\"id\":\"audio_stream\",\"data\":\"first\"}}}]}\n\n"
	const late = "data: {\"id\":\"chat1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"audio\":{\"expires_at\":1,\"data\":\"last\"}}}]}\n\ndata: [DONE]\n\n"
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	channel, _ := setupResourceHTTPTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, late)
	})
	r, p, cleanup := newChatAudioOwnerRelay(t, channel, true)
	defer cleanup()
	stream, apiErr := p.CreateChatCompletionStream(&r.chatRequest)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	defer stream.Close()
	data, errs := stream.Recv()
	select {
	case got := <-data:
		if got != first {
			t.Fatalf("first frame changed: %q", got)
		}
	case err := <-errs:
		t.Fatalf("before first frame: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("ID frame waited for expires_at")
	}
	owner, err := model.GetResourceOwner(context.Background(), "chat_audio", "audio_stream", 101)
	if err != nil || owner.UpstreamExpiresAt != nil {
		t.Fatalf("minimal owner not committed before ID delivery: %+v %v", owner, err)
	}
	unblock()
	got := first
	for data != nil || errs != nil {
		select {
		case piece, ok := <-data:
			if !ok {
				data = nil
			} else {
				got += piece
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
			} else if err != nil && !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("stream failed to end")
		}
	}
	if got != first+late {
		t.Fatalf("stream changed: %q", got)
	}
	cleanup()
	owner, err = model.GetResourceOwner(context.Background(), "chat_audio", "audio_stream", 101)
	if err != nil || owner.UpstreamExpiresAt == nil || owner.UpstreamExpiresAt.Unix() != 1 {
		t.Fatalf("late expiry observation: %+v %v", owner, err)
	}
	ctx, _ := resourceHTTPContext(http.MethodPost, "/v1/chat/completions", nil)
	if err := prepareResourceRequest(ctx, []byte(`{"messages":[{"role":"assistant","audio":{"id":"audio_stream"}}]}`), "chat"); err != nil {
		t.Fatalf("expired upstream metadata blocked authorized reference: %v", err)
	}
}

func TestChatAudioOwnerBindFailureDoesNotLeakOrRetry(t *testing.T) {
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"audio":{"id":"audio_private","data":"opaque"}}}]}`)
	})
	r, p, cleanup := newChatAudioOwnerRelay(t, channel, false)
	defer cleanup()
	const callback = "test:audio-owner-bind-failure"
	if err := model.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "resource_owners" {
			tx.AddError(errors.New("injected owner bind failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer model.DB.Callback().Update().Remove(callback)
	response, apiErr := p.CreateChatCompletion(&r.chatRequest)
	if response != nil || apiErr == nil || !apiErr.UpstreamAccepted || strings.Contains(apiErr.Error(), "audio_private") {
		t.Fatalf("owner failure leaked output: response=%+v err=%v", response, apiErr)
	}
	if calls.Load() != 1 || shouldRetry(r.c, apiErr, config.ChannelTypeOpenAI) {
		t.Fatalf("owner failure was retryable: calls=%d err=%v", calls.Load(), apiErr)
	}
}

func TestChatAudioOwnerTextDoesNotReserveAndCapacityFailsBeforeSend(t *testing.T) {
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"content":"hello"}}]}`)
	})
	r, p, cleanup := newChatAudioOwnerRelay(t, channel, false)
	defer cleanup()
	r.chatRequest.Modalities = nil
	if _, apiErr := p.CreateChatCompletion(&r.chatRequest); apiErr != nil {
		t.Fatal(apiErr)
	}
	var count int64
	if err := model.DB.Model(&model.ResourceOwner{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("text request created %d resource slots", count)
	}
	r.chatRequest.Modalities = []string{"audio"}
	n := chatAudioOwnerMaxSlots + 1
	r.chatRequest.N = &n
	_, apiErr := p.CreateChatCompletion(&r.chatRequest)
	if apiErr == nil || apiErr.UpstreamAccepted || apiErr.StatusCode != http.StatusTooManyRequests || calls.Load() != 1 {
		t.Fatalf("capacity rejected too late: err=%v calls=%d", apiErr, calls.Load())
	}
}
