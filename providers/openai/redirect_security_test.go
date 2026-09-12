package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

func TestExactEmbeddingRedirectPreservesBodyAndTarget(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "/result/"+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Etag", `"original"`)
		w.Header().Set("Digest", "original")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = io.WriteString(w, `{"debug":"`+r.Header.Get("Authorization")+`"}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	proxy := ""
	p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "provider-secret-123", Proxy: &proxy}, server.URL)
	p.SetProviderRawJSONReplay(true)
	p.Usage = &types.Usage{}
	_, apiErr := p.CreateEmbeddings(&types.EmbeddingRequest{Model: "text-embedding-3-small", Input: "hello"})
	if apiErr == nil || apiErr.StatusCode != http.StatusTemporaryRedirect || !apiErr.ReplayRawResponse || calls.Load() != 1 {
		t.Fatalf("redirect was followed or lost: calls=%d err=%+v", calls.Load(), apiErr)
	}
	if string(apiErr.RawBody) != `{"debug":"Bearer provider-secret-123"}` || apiErr.ResponseHeaders.Get("Location") != "/result/provider-secret-123" || apiErr.ResponseHeaders.Get("Digest") != "original" {
		t.Fatalf("脱敏干扰重定向: body=%q headers=%v", apiErr.RawBody, apiErr.ResponseHeaders)
	}
}
