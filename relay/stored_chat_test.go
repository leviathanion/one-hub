package relay

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"one-api/model"
	"one-api/providers/openai"
	"one-api/types"
)

func TestStoredChatOwnershipUsesOneResourceReservation(t *testing.T) {
	channel, _ := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chatcmpl-created","object":"chat.completion","choices":[]}`))
	})
	ctx, _ := resourceHTTPContext(http.MethodPost, "/v1/chat/completions", nil)
	provider := openai.CreateOpenAIProvider(channel, channel.GetBaseURL())
	provider.SetContext(ctx)
	provider.SetUsage(&types.Usage{})
	store := true
	relay := NewRelayChat(ctx)
	relay.provider, relay.modelName = provider, "test"
	relay.chatRequest = types.ChatCompletionRequest{Model: "test", Store: &store}
	cleanup, apiErr := relay.prepareStoredChatOwnership()
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	var count int64
	model.DB.Model(&model.ResourceOwner{}).Where("kind = ?", "stored_chat").Count(&count)
	if count != 1 {
		t.Fatalf("reserved owners=%d", count)
	}
	if _, apiErr := provider.CreateChatCompletion(&relay.chatRequest); apiErr != nil {
		t.Fatal(apiErr)
	}
	cleanup()
	owner, err := model.GetResourceOwner(context.Background(), "stored_chat", "chatcmpl-created", 101)
	if err != nil || owner.ChannelID != channel.Id {
		t.Fatalf("owner=%+v err=%v", owner, err)
	}
	if _, err := model.GetResourceOwner(context.Background(), "stored_chat", "chatcmpl-created", 102); !errors.Is(err, model.ErrResourceOwnerNotFound) {
		t.Fatalf("cross-user owner access: %v", err)
	}
	store = false
	cleanup, apiErr = relay.prepareStoredChatOwnership()
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	cleanup()
	model.DB.Model(&model.ResourceOwner{}).Where("kind = ?", "stored_chat").Count(&count)
	if count != 1 {
		t.Fatalf("store:false allocated an owner: %d", count)
	}
}

func TestStoredChatLifecycleUsesOwnerEvenAfterDelete(t *testing.T) {
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions/chatcmpl-owned" && r.URL.Path != "/v1/chat/completions/chatcmpl-owned/messages" {
			t.Errorf("unexpected URL: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"future":true}`))
	})
	seedResourceHTTPOwner(t, channel, "stored_chat", "chatcmpl-owned", 101)
	for _, op := range []struct{ method, suffix string }{{"GET", ""}, {"POST", ""}, {"GET", "/messages"}, {"DELETE", ""}, {"GET", ""}, {"DELETE", ""}} {
		ctx, recorder := resourceHTTPContext(op.method, "/v1/chat/completions/chatcmpl-owned"+op.suffix, strings.NewReader(`{"metadata":{"future":1e3}}`))
		ResourceRelay(ctx)
		if recorder.Code != http.StatusOK || recorder.Body.String() != `{"future":true}` {
			t.Fatalf("%s %s: %d %s", op.method, op.suffix, recorder.Code, recorder.Body.String())
		}
	}
	if calls.Load() != 6 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/chat/completions/chatcmpl-unknown"} {
		ctx, recorder := resourceHTTPContext(http.MethodGet, path, nil)
		ResourceRelay(ctx)
		if recorder.Code != http.StatusForbidden && recorder.Code != http.StatusNotFound {
			t.Fatalf("unowned/list request accepted: %d %s", recorder.Code, recorder.Body.String())
		}
	}
	if calls.Load() != 6 {
		t.Fatal("unowned/list request reached upstream")
	}
}

func TestStoredChatSendKeepsOriginalJSONAndReleasesUnboundCapacity(t *testing.T) {
	const wire = "{ \"id\":\"chatcmpl-send\", \"object\":\"chat.completion\", \"choices\":[], \"future\":1e3 }"
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		var count int64
		if err := model.DB.Model(&model.ResourceOwner{}).Where("kind = ? AND phase = ?", "stored_chat", model.ResourceOwnerReserved).Count(&count).Error; err != nil || count != 1 {
			t.Errorf("no prior ownership capacity reservation: count=%d err=%v", count, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(wire))
	})
	ctx, recorder := resourceHTTPContext(http.MethodPost, "/v1/chat/completions", nil)
	provider := openai.CreateOpenAIProvider(channel, channel.GetBaseURL())
	provider.SetContext(ctx)
	provider.SetUsage(&types.Usage{})
	store := true
	r := NewRelayChat(ctx)
	r.provider, r.modelName = provider, "test"
	r.chatRequest = types.ChatCompletionRequest{Model: "test", Store: &store}
	if apiErr, _ := r.sendCurrentProvider(); apiErr != nil {
		t.Fatal(apiErr)
	}
	if calls.Load() != 1 || recorder.Body.String() != wire {
		t.Fatalf("calls=%d wire=%q", calls.Load(), recorder.Body.String())
	}
	if _, err := model.GetResourceOwner(context.Background(), "stored_chat", "chatcmpl-send", 101); err != nil {
		t.Fatal(err)
	}
	var unbound int64
	model.DB.Model(&model.ResourceOwner{}).Where("kind = ? AND phase = ?", "stored_chat", model.ResourceOwnerReserved).Count(&unbound)
	if unbound != 0 {
		t.Fatalf("unbound capacity remained: %d", unbound)
	}
}

func TestStoredChatLifecycleRejectsPathAliasesAndUnknownOperations(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions/", "/v1/chat/completions/..", "/v1/chat/completions/%2e%2e", "/v1/chat/completions/a%2fb", "/v1/chat/completions/a%5cb", "/v1/chat/completions/a%00b", "/v1/chat/completions/a/resume"} {
		if _, ok := parseStoredChatHTTPOperation(http.MethodGet, path); ok {
			t.Fatalf("invalid resource envelope accepted: %s", path)
		}
	}
}
