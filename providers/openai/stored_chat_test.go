package openai

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestStoredChatJSONCommitsOwnerBeforeReturningOriginalBody(t *testing.T) {
	wire := "{ \"id\":\"chatcmpl-owned\", \"object\":\"chat.completion\", \"choices\":[], \"future\":1e3 }"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, wire) }))
	t.Cleanup(server.Close)
	oldClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = oldClient })
	proxy := ""
	p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, server.URL)
	p.Usage = &types.Usage{}
	store := true
	committed := ""
	p.SetStoredChatOwnerCommit(func(id string) error { committed = id; return nil })
	response, apiErr := p.CreateChatCompletion(&types.ChatCompletionRequest{Model: "test", Store: &store})
	if apiErr != nil || committed != "chatcmpl-owned" {
		t.Fatalf("owner=%q err=%v", committed, apiErr)
	}
	if string(response.ReplayProviderRawJSON()) != wire {
		t.Fatalf("response replay changed: %s", response.ReplayProviderRawJSON())
	}
}

func TestStoredChatSSECommitBarrierPreservesEventsAndRejectsOnlyOwnerFailure(t *testing.T) {
	const extension = "event: extension\ndata: {\"id\":\"business-id\",\"object\":\"future.event\"}\n\n"
	const chunk = "id: 007\ndata: {\"id\":\"chatcmpl-owned\",\"object\":\"chat.completion.chunk\",\"choices\":[]}\n\n"
	const tail = "data: [DONE]\n\nevent: extension\ndata: opaque\n\n"
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "owner unavailable"}[fail], func(t *testing.T) {
			proxy := ""
			p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, "https://example.test")
			var ids []string
			p.SetStoredChatOwnerCommit(func(id string) error {
				ids = append(ids, id)
				if fail {
					return errors.New("database unavailable")
				}
				return nil
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"store":true,"stream":true}`))
			commit, apiErr := p.chatResourceRequestCommit(req)
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			handler := &OpenAIStreamHandler{Usage: &types.Usage{}, beforeChatDelivery: commit}
			got, err := collectNativeSSE(t, newExactChatTestStream(t, handler, extension+chunk+tail))
			if len(ids) != 1 || ids[0] != "chatcmpl-owned" {
				t.Fatalf("incorrect identity extraction: %v", ids)
			}
			if fail {
				var typed *types.OpenAIErrorWithStatusCode
				if got != extension || !errors.As(err, &typed) || !typed.UpstreamAccepted {
					t.Fatalf("owner failure leaked id: wire=%q error=%v", got, err)
				}
			} else if got != extension+chunk+tail || !errors.Is(err, io.EOF) {
				t.Fatalf("wire=%q error=%v", got, err)
			}
		})
	}
}

func TestStoredChatOverlayCannotCreateWithoutReservation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unreserved storage reached upstream") }))
	t.Cleanup(server.Close)
	oldClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = oldClient })
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[],"store":false}`))
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatal(err)
	}
	custom, proxy := `{"overwrite":true,"store":true}`, ""
	p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy, CustomParameter: &custom}, server.URL)
	p.SetContext(ctx)
	p.Usage = &types.Usage{}
	store := false
	response, apiErr := p.CreateChatCompletion(&types.ChatCompletionRequest{Model: "test", Store: &store})
	if response != nil || apiErr == nil || !apiErr.LocalError || apiErr.UpstreamAccepted {
		t.Fatalf("unreserved store accepted: %v %v", response, apiErr)
	}
}

func TestStoredChatNativeLifecycleURLInheritsChatEndpoint(t *testing.T) {
	proxy := ""
	channel := &model.Channel{Type: config.ChannelTypeCustom, Proxy: &proxy, Plugin: model.NewCustomEndpointPlugin()}
	channel.Plugin.Data()["endpoints"]["openai.chat_completions"] = map[string]any{"enabled": true, "upstream_url": "https://managed.example/chat?tenant=one"}
	p := CreateOpenAIProvider(channel, "https://unused.example")
	got, err := p.BuildResourceRelayURL("/v1/chat/completions/chatcmpl-1/messages", "limit=5&after=a%2Fb")
	if err != nil || got != "https://managed.example/chat/chatcmpl-1/messages?tenant=one&limit=5&after=a%2Fb" {
		t.Fatalf("URL=%q err=%v", got, err)
	}
	channel.Plugin.Data()["endpoints"]["openai.chat_completions"] = map[string]any{"enabled": false}
	if _, err := p.BuildResourceRelayURL("/v1/chat/completions/chatcmpl-1", ""); err == nil {
		t.Fatal("disabled Chat endpoint accepted")
	}
}
