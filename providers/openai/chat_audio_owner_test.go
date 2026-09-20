package openai

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func TestChatAudioDeliveryBarrierPreservesSSEAndBlocksUnownedID(t *testing.T) {
	const extension = "event: future\ndata: {\"object\":\"future.event\",\"choices\":[{\"delta\":{\"audio\":{\"id\":\"business\"}}}]}\n\n"
	const audio = "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"audio\":{\"id\":\"audio_private\",\"future\":[1,true]}}}]}\n\n"
	const tail = "data: [DONE]\n\n"
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{true: "owner failure", false: "owner committed"}[fail], func(t *testing.T) {
			proxy := ""
			p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, "https://example.test")
			calls := 0
			p.SetChatAudioOwnerPolicy(func(n int) error {
				if n != 2 {
					t.Fatalf("final n=%d", n)
				}
				return nil
			}, func(facts []base.ChatAudioResourceFact) error {
				calls++
				if len(facts) != 1 || facts[0].ID != "audio_private" {
					t.Fatalf("incorrect audio projection: %+v", facts)
				}
				if fail {
					return errors.New("owner database unavailable")
				}
				return nil
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true,"modalities":["audio"],"n":2}`))
			commit, apiErr := p.chatResourceRequestCommit(req)
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			handler := &OpenAIStreamHandler{Usage: &types.Usage{}, beforeChatDelivery: commit}
			got, err := collectNativeSSE(t, newExactChatTestStream(t, handler, extension+audio+tail))
			if calls != 1 {
				t.Fatalf("audio commits=%d", calls)
			}
			if fail {
				var typed *types.OpenAIErrorWithStatusCode
				if got != extension || !errors.As(err, &typed) || !typed.UpstreamAccepted {
					t.Fatalf("ownership failure leaked ID: %q %v", got, err)
				}
			} else if got != extension+audio+tail || !errors.Is(err, io.EOF) {
				t.Fatalf("raw stream changed: %q %v", got, err)
			}
		})
	}
}

func TestChatAudioNativeAzureDoesNotRequireStoredChatLifecycle(t *testing.T) {
	proxy := ""
	p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeAzure, Proxy: &proxy}, "https://example.test")
	calls := 0
	p.SetChatAudioOwnerPolicy(func(n int) error { calls++; return nil }, func([]base.ChatAudioResourceFact) error { return nil })
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"modalities":["audio"]}`))
	commit, apiErr := p.chatResourceRequestCommit(req)
	if apiErr != nil || commit == nil || calls != 1 {
		t.Fatalf("audio inherited Stored Chat restriction: %v", apiErr)
	}
}
