package openai

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestNativeUnaryFutureUnionPreservesRawAndIndependentUsage(t *testing.T) {
	for _, completion := range []bool{false, true} {
		for _, validUsage := range []bool{false, true} {
			t.Run(fmt.Sprintf("completion=%t/usage=%t", completion, validUsage), func(t *testing.T) {
				usage := `{"prompt_tokens":3,"completion_tokens":0,"total_tokens":0}`
				if !validUsage {
					usage = `{"prompt_tokens":3,"completion_tokens":{"future":true}}`
				}
				raw := []byte(`{ "id":"future", "model":"gpt-5", "choices":{"future":true}, "usage":` + usage + `, "future":1e3 }`)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(raw)
				}))
				defer server.Close()
				previousClient := requester.HTTPClient
				requester.HTTPClient = server.Client()
				t.Cleanup(func() { requester.HTTPClient = previousClient })
				proxy := ""
				provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-unary", Proxy: &proxy}, server.URL)
				provider.SetProviderRawJSONReplay(true)
				provider.Usage = &types.Usage{}
				var replay []byte
				var apiErr *types.OpenAIErrorWithStatusCode
				if completion {
					var response *types.CompletionResponse
					response, apiErr = provider.CreateCompletion(&types.CompletionRequest{Model: "gpt-5", Prompt: "hello"})
					if response != nil {
						replay = response.ReplayProviderRawJSON()
					}
				} else {
					var response *types.ChatCompletionResponse
					response, apiErr = provider.CreateChatCompletion(&types.ChatCompletionRequest{Model: "gpt-5"})
					if response != nil {
						replay = response.ReplayProviderRawJSON()
					}
				}
				if apiErr != nil || !bytes.Equal(replay, raw) || provider.Usage.ProviderReported != validUsage {
					t.Fatalf("future union blocked raw or contaminated usage: err=%+v usage=%+v raw=%s", apiErr, provider.Usage, replay)
				}
				if validUsage && (provider.Usage.PromptTokens != 3 || provider.Usage.TotalTokens != 0) {
					t.Fatalf("provider values changed: %+v", provider.Usage)
				}
			})
		}
	}
}

func TestExactOpenAIUnarySurfacesPreserveRequestResponseAndFacts(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		rawRequest    string
		rawResponse   string
		originalModel string
		request       any
		call          func(*OpenAIProvider, any) ([]byte, *types.OpenAIErrorWithStatusCode)
		assertUsage   func(*testing.T, *types.Usage)
	}{
		{
			name: "embeddings", path: "/v1/embeddings",
			rawRequest:    ` {"model":"text-embedding-3-small","input":"hello","future":1e3} `,
			rawResponse:   ` {"object":"list","model":"text-embedding-3-small","data":{"future_union":true},"usage":{"prompt_tokens":3,"total_tokens":3},"future":1e3} `,
			originalModel: "text-embedding-3-small", request: &types.EmbeddingRequest{Model: "text-embedding-3-small", Input: "hello"},
			call: func(p *OpenAIProvider, request any) ([]byte, *types.OpenAIErrorWithStatusCode) {
				response, apiErr := p.CreateEmbeddings(request.(*types.EmbeddingRequest))
				if response == nil {
					return nil, apiErr
				}
				return response.ReplayProviderRawJSON(), apiErr
			},
			assertUsage: func(t *testing.T, usage *types.Usage) {
				if !usage.HasProviderUsage() || usage.PromptTokens != 3 || usage.TotalTokens != 3 || usage.ResponseModel != "text-embedding-3-small" {
					t.Fatalf("embedding facts unavailable: %+v", usage)
				}
			},
		},
		{
			name: "moderations", path: "/v1/moderations",
			rawRequest:    ` {"model":"omni-moderation-latest","input":"hello","future":1e3} `,
			rawResponse:   ` {"id":"modr","model":"omni-moderation-latest","results":{"future_union":true},"future":1e3} `,
			originalModel: "omni-moderation-latest", request: &types.ModerationRequest{Model: "omni-moderation-latest", Input: "hello"},
			call: func(p *OpenAIProvider, request any) ([]byte, *types.OpenAIErrorWithStatusCode) {
				response, apiErr := p.CreateModeration(request.(*types.ModerationRequest))
				if response == nil {
					return nil, apiErr
				}
				return response.ReplayProviderRawJSON(), apiErr
			},
			assertUsage: func(t *testing.T, usage *types.Usage) {
				if usage.TotalTokens != usage.PromptTokens {
					t.Fatalf("moderation local estimate changed: %+v", usage)
				}
			},
		},
		{
			name: "images", path: "/v1/images/generations",
			rawRequest:    ` {"prompt":"draw","future":1e3} `,
			rawResponse:   ` {"created":1,"model":"gpt-image-1","data":[{"future_union":true},"future_variant"],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5,"input_tokens_details":{"text_tokens":2,"image_tokens":1},"output_tokens_details":{"text_tokens":0,"image_tokens":2}},"future":1e3} `,
			originalModel: "dall-e-2", request: &types.ImageRequest{Model: "dall-e-2", Prompt: "draw", N: 1},
			call: func(p *OpenAIProvider, request any) ([]byte, *types.OpenAIErrorWithStatusCode) {
				response, apiErr := p.CreateImageGenerations(request.(*types.ImageRequest))
				if response == nil {
					return nil, apiErr
				}
				return response.ReplayProviderRawJSON(), apiErr
			},
			assertUsage: func(t *testing.T, usage *types.Usage) {
				if !usage.HasProviderUsage() || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 2 || usage.ResponseModel != "gpt-image-1" {
					t.Fatalf("image facts unavailable: %+v", usage)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var providerBody []byte
			var providerHeaders http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerBody, _ = io.ReadAll(r.Body)
				providerHeaders = r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Request-Id", "req-kept")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, test.rawResponse)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.rawRequest))
			ctx.Request.Header.Add("X-Future-Business", "one")
			ctx.Request.Header.Add("X-Future-Business", "two")
			ctx.Request.Header.Set("Authorization", "Bearer client-secret")
			if _, err := common.CacheRequestBody(ctx); err != nil {
				t.Fatal(err)
			}

			proxy := ""
			provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-provider", Proxy: &proxy}, server.URL)
			provider.SetProviderRawJSONReplay(true)
			provider.SetOpenAIErrorEnvelopeReplay(true)
			provider.SetContext(ctx)
			provider.SetOriginalModel(test.originalModel)
			provider.Usage = &types.Usage{PromptTokens: 7}
			replay, apiErr := test.call(provider, test.request)
			if apiErr != nil || !bytes.Equal(replay, []byte(test.rawResponse)) {
				t.Fatalf("exact response changed: err=%+v raw=%q", apiErr, replay)
			}
			if !bytes.Equal(providerBody, []byte(test.rawRequest)) {
				t.Fatalf("exact request changed: got=%q want=%q", providerBody, test.rawRequest)
			}
			if got := providerHeaders.Values("X-Future-Business"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
				t.Fatalf("exact request headers changed: %v", got)
			}
			if providerHeaders.Get("Authorization") != "Bearer sk-provider" {
				t.Fatalf("client auth leaked: %q", providerHeaders.Get("Authorization"))
			}
			if status := ctx.GetInt(requestctx.ProviderResponseStatusContextKey); status != http.StatusCreated {
				t.Fatalf("provider status=%d", status)
			}
			headers, ok := ctx.Get(requestctx.ProviderResponseHeadersContextKey)
			if !ok || headers.(http.Header).Get("X-Request-Id") != "req-kept" {
				t.Fatalf("provider headers missing: %#v", headers)
			}
			test.assertUsage(t, provider.Usage)
		})
	}
}

func TestOpenAIEmbeddingUsageUsesInputOnlyEvidenceContract(t *testing.T) {
	for _, test := range []struct {
		name      string
		usageJSON string
		want      bool
	}{
		{name: "standard prompt and total", usageJSON: `{"prompt_tokens":3,"total_tokens":99}`, want: true},
		{name: "explicit zero completion", usageJSON: `{"prompt_tokens":3,"completion_tokens":0,"total_tokens":3}`, want: true},
		{name: "total omitted", usageJSON: `{"prompt_tokens":3}`, want: true},
		{name: "prompt zero", usageJSON: `{"prompt_tokens":0,"total_tokens":0}`, want: true},
		{name: "missing prompt", usageJSON: `{"total_tokens":3}`},
		{name: "malformed prompt", usageJSON: `{"prompt_tokens":"three","total_tokens":3}`},
		{name: "nonzero completion conflict", usageJSON: `{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := &OpenAIProviderEmbeddingsResponse{}
			payload := []byte(`{"model":"text-embedding-actual","usage":` + test.usageJSON + `}`)
			if err := response.DecodeCapturedProviderJSON(payload); err != nil {
				t.Fatal(err)
			}
			target := &types.Usage{PromptTokens: 17, TotalTokens: 17}
			applyOpenAIEmbeddingUsage(target, response.Usage, response.Model)
			if target.HasProviderUsage() != test.want {
				t.Fatalf("authorized=%t want=%t usage=%+v", target.HasProviderUsage(), test.want, target)
			}
			if test.want {
				if target.ResponseModel != "text-embedding-actual" || target.CompletionTokens != 0 || target.TotalTokens != target.PromptTokens {
					t.Fatalf("embedding projection changed: %+v", target)
				}
			} else if target.ProviderReported {
				t.Fatalf("invalid embedding evidence was published: %+v", target)
			}
		})
	}
}

func TestExactEmbeddingPreservesProviderRedirectWithoutFollowing(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		_, _ = io.WriteString(w, `{"unexpected":true}`)
	}))
	t.Cleanup(target.Close)
	redirectBody := []byte(`{"redirect":"provider"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.Header().Set("X-Request-Id", "redirect-request")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write(redirectBody)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-provider", Proxy: &proxy}, server.URL)
	provider.SetProviderRawJSONReplay(true)
	provider.SetOpenAIErrorEnvelopeReplay(true)
	provider.Usage = &types.Usage{}
	response, apiErr := provider.CreateEmbeddings(&types.EmbeddingRequest{Model: "text-embedding-3-small", Input: "hello"})
	if response != nil || apiErr == nil {
		t.Fatalf("redirect became a success: response=%+v err=%+v", response, apiErr)
	}
	if targetHits != 0 || apiErr.StatusCode != http.StatusTemporaryRedirect || !apiErr.ReplayRawResponse || !bytes.Equal(apiErr.RawBody, redirectBody) {
		t.Fatalf("redirect contract changed: hits=%d err=%+v raw=%q", targetHits, apiErr, apiErr.RawBody)
	}
	if apiErr.ResponseHeaders.Get("Location") != target.URL || apiErr.ResponseHeaders.Get("X-Request-Id") != "redirect-request" {
		t.Fatalf("redirect headers missing: %v", apiErr.ResponseHeaders)
	}
}

func TestRawObservationDoesNotRelaxNonCapturedDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":{"future":true},"usage":{"prompt_tokens":3,"completion_tokens":0}}`)
	}))
	defer server.Close()
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	client := requester.NewHTTPRequester("", nil)
	req, err := http.NewRequest(http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := &OpenAIProviderChatResponse{}
	_, apiErr := client.SendRequest(req, response, false)
	if apiErr == nil || apiErr.Code != "decode_response_failed" || !apiErr.UpstreamAccepted {
		t.Fatalf("non-captured decoder was silently relaxed: %+v", apiErr)
	}
}
