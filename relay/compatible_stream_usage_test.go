package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/azure"
	azurev1 "one-api/providers/azure_v1"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/relay/relay_util"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const compatibleStreamUsageModel = "issue002-stream-model"

type compatibleStreamUsageUpstreamState struct {
	mu            sync.Mutex
	calls         int
	lastBody      []byte
	includeUsage  bool
	streamRequest bool
}

func TestCompatibleStreamingAddsUsageAtFinalWire(t *testing.T) {
	for _, test := range []struct {
		name      string
		channel   int
		operation string
		raw       bool
	}{
		{name: "OpenAI Chat raw", channel: config.ChannelTypeOpenAI, operation: "chat", raw: true},
		{name: "OpenAI Chat typed", channel: config.ChannelTypeOpenAI, operation: "chat"},
		{name: "OpenAI Completion raw", channel: config.ChannelTypeOpenAI, operation: "completion", raw: true},
		{name: "OpenAI Completion typed", channel: config.ChannelTypeOpenAI, operation: "completion"},
		{name: "Azure Chat raw", channel: config.ChannelTypeAzure, operation: "chat", raw: true},
		{name: "Azure Chat typed", channel: config.ChannelTypeAzure, operation: "chat"},
		{name: "Azure Completion raw", channel: config.ChannelTypeAzure, operation: "completion", raw: true},
		{name: "Azure Completion typed", channel: config.ChannelTypeAzure, operation: "completion"},
		{name: "AzureV1 Chat raw", channel: config.ChannelTypeAzureV1, operation: "chat", raw: true},
		{name: "AzureV1 Chat typed", channel: config.ChannelTypeAzureV1, operation: "chat"},
		{name: "AzureV1 Completion raw", channel: config.ChannelTypeAzureV1, operation: "completion", raw: true},
		{name: "AzureV1 Completion typed", channel: config.ChannelTypeAzureV1, operation: "completion"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, state := compatibleStreamUsageUpstream(t, test.operation)
			provider := compatibleStreamUsageNewProvider(t, test.channel, server.URL, nil)
			path := "/v1/chat/completions"
			if test.operation == "completion" {
				path = "/v1/completions"
			}
			var raw string
			if test.operation == "chat" {
				raw = `{"model":"` + compatibleStreamUsageModel + `","stream":true,"messages":[{"role":"user","content":"hello"}],"stream_options":{"include_obfuscation":true,"future_option":{"enabled":true}},"future_request_field":{"enabled":true}}`
			} else {
				raw = `{"model":"` + compatibleStreamUsageModel + `","prompt":"hello","stream":true,"stream_options":{"include_obfuscation":true,"future_option":{"enabled":true}},"future_request_field":{"enabled":true}}`
			}
			ctx := compatibleStreamUsageRequestContext(t, path, test.raw, raw)
			provider.SetContext(ctx)
			provider.SetOriginalModel(compatibleStreamUsageModel)
			provider.SetUsage(&types.Usage{})

			var delivered string
			if test.operation == "chat" {
				request := &types.ChatCompletionRequest{Model: compatibleStreamUsageModel, Stream: true, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}}
				if test.raw {
					if err := json.Unmarshal([]byte(raw), request); err != nil {
						t.Fatalf("decode raw Chat request: %v", err)
					}
				}
				stream, apiErr := provider.(base.ChatInterface).CreateChatCompletionStream(request)
				if apiErr != nil {
					t.Fatalf("CreateChatCompletionStream failed: %+v", apiErr)
				}
				delivered = compatibleStreamUsageDrainStream(t, stream)
			} else {
				request := &types.CompletionRequest{Model: compatibleStreamUsageModel, Prompt: "hello", Stream: true}
				if obfuscation := true; !test.raw {
					request.StreamOptions = &types.StreamOptions{IncludeObfuscation: &obfuscation}
				} else if err := json.Unmarshal([]byte(raw), request); err != nil {
					t.Fatalf("decode raw Completion request: %v", err)
				}
				stream, apiErr := provider.(base.CompletionInterface).CreateCompletionStream(request)
				if apiErr != nil {
					t.Fatalf("CreateCompletionStream failed: %+v", apiErr)
				}
				delivered = compatibleStreamUsageDrainStream(t, stream)
			}

			if !strings.Contains(delivered, `"ok"`) {
				t.Fatalf("successful stream did not reach downstream: %s", delivered)
			}
			usage := provider.GetUsage()
			if !usage.HasProviderUsage() || !usage.ProviderReported || usage.PromptTokens != 3 || usage.CompletionTokens != 4 || usage.TotalTokens != 7 {
				t.Fatalf("provider usage evidence missing or changed: %+v", usage)
			}
			compatibleStreamUsageAssertUpstreamWire(t, state, true, test.raw)
		})
	}
}

func TestUsageControlsPreserveExactAndProviderOwnedFields(t *testing.T) {
	for _, test := range []struct {
		name          string
		raw           string
		custom        *string
		support       bool
		wantUsage     bool
		wantFutureKey string
		typed         bool
	}{
		{
			name:      "SupportStreamOptions false",
			raw:       `{"model":"` + compatibleStreamUsageModel + `","stream":true,"messages":[{"role":"user","content":"hello"}],"future_request_field":{"enabled":true}}`,
			support:   false,
			wantUsage: false,
		},
		{
			name:          "existing options",
			raw:           `{"model":"` + compatibleStreamUsageModel + `","stream":true,"messages":[{"role":"user","content":"hello"}],"stream_options":{"include_usage":false,"include_obfuscation":false,"future_option":{"enabled":true}},"future_request_field":{"enabled":true}}`,
			support:       true,
			wantUsage:     true,
			wantFutureKey: "future_option",
		},
		{
			name:          "custom before provider usage",
			raw:           `{"model":"` + compatibleStreamUsageModel + `","stream":true,"messages":[{"role":"user","content":"hello"}],"future_request_field":{"enabled":true}}`,
			custom:        stringPtr(`{"overwrite":true,"stream_options":{"include_usage":false,"custom_future":{"enabled":true}}}`),
			support:       true,
			wantUsage:     true,
			wantFutureKey: "custom_future",
		},
		{
			name:          "custom typed builder before provider usage",
			raw:           `{"model":"` + compatibleStreamUsageModel + `","stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			custom:        stringPtr(`{"overwrite":true,"stream_options":{"include_usage":false,"custom_future":{"enabled":true}}}`),
			support:       true,
			wantUsage:     true,
			wantFutureKey: "custom_future",
			typed:         true,
		},
		{
			name:          "pre_add before provider usage",
			raw:           `{"model":"` + compatibleStreamUsageModel + `","stream":true,"messages":[{"role":"user","content":"hello"}],"stream_options":{"include_usage":false,"pre_future":{"enabled":true}},"future_request_field":{"enabled":true}}`,
			custom:        stringPtr(`{"pre_add":true,"overwrite":true,"stream_options":{"include_usage":false,"pre_future":{"enabled":true}}}`),
			support:       true,
			wantUsage:     true,
			wantFutureKey: "pre_future",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, state := compatibleStreamUsageUpstream(t, "chat")
			provider := compatibleStreamUsageNewProvider(t, config.ChannelTypeOpenAI, server.URL, test.custom)
			p, ok := provider.(*openai.OpenAIProvider)
			if !ok {
				t.Fatal("expected OpenAI provider")
			}
			p.SupportStreamOptions = test.support
			ctx := compatibleStreamUsageRequestContext(t, "/v1/chat/completions", !test.typed, test.raw)
			provider.SetContext(ctx)
			provider.SetOriginalModel(compatibleStreamUsageModel)
			provider.SetUsage(&types.Usage{})
			request := &types.ChatCompletionRequest{Model: compatibleStreamUsageModel, Stream: true, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}}
			if test.typed {
				obfuscation := true
				request.StreamOptions = &types.StreamOptions{IncludeObfuscation: &obfuscation}
			}
			stream, apiErr := p.CreateChatCompletionStream(request)
			if apiErr != nil {
				t.Fatalf("CreateChatCompletionStream failed: %+v", apiErr)
			}
			_ = compatibleStreamUsageDrainStream(t, stream)
			compatibleStreamUsageAssertUpstreamWire(t, state, test.wantUsage, !test.typed)
			usage := provider.GetUsage()
			if test.wantUsage {
				if !usage.HasProviderUsage() || usage.TotalTokens != 7 {
					t.Fatalf("forced usage did not become provider evidence: %+v", usage)
				}
			} else if usage.HasProviderUsage() || usage.ProviderReported {
				t.Fatalf("unsupported/exact stream unexpectedly became billable: %+v", usage)
			}
			if test.wantFutureKey != "" {
				body := compatibleStreamUsageLastBody(state)
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatalf("decode final provider body: %v", err)
				}
				var options map[string]json.RawMessage
				if err := json.Unmarshal(fields["stream_options"], &options); err != nil || options[test.wantFutureKey] == nil {
					t.Fatalf("provider-owned sibling field %q was lost: %s", test.wantFutureKey, body)
				}
			}
		})
	}
}

func TestOfficialStreamingCollectsUsageIndependentlyOfClientDisplay(t *testing.T) {
	for _, operation := range []string{"chat", "completion"} {
		for _, test := range []struct {
			name, options          string
			clientUsage, wantUsage bool
		}{
			{name: "omitted", wantUsage: true},
			{name: "false", options: `,"stream_options":{"include_usage":false,"include_obfuscation":true,"future_option":{"n":9007199254740993,"text":"\u0061"}}`, wantUsage: true},
			{name: "true", options: `,"stream_options":{"include_usage":true,"future_option":{"n":9007199254740993}}`, clientUsage: true, wantUsage: true},
			{name: "null", options: `,"stream_options":null`, wantUsage: true},
			{name: "future array", options: `,"stream_options":["future"]`},
			{name: "future string", options: `,"stream_options":"future"`},
			{name: "future number", options: `,"stream_options":42`},
		} {
			t.Run(operation+"/"+test.name, func(t *testing.T) {
				transport := &compatibleStreamUsageExactCaptureTransport{operation: operation}
				previousClient := requester.HTTPClient
				requester.HTTPClient = &http.Client{Transport: transport}
				t.Cleanup(func() { requester.HTTPClient = previousClient })
				proxy := ""
				provider := openai.CreateOpenAIProvider(&model.Channel{
					Type: config.ChannelTypeOpenAI, Key: "official-key", Proxy: &proxy,
				}, "https://api.openai.com")
				path, input := "/v1/chat/completions", `"messages":[{"role":"user","content":"hello"}]`
				if operation == "completion" {
					path, input = "/v1/completions", `"prompt":"hello"`
				}
				raw := `{"model":"` + compatibleStreamUsageModel + `","stream":true,` + input + test.options + `,"future_request_field":{"n":9007199254740993,"text":"\u0061"}}`
				ctx := compatibleStreamUsageRequestContext(t, path, true, raw)
				provider.SetContext(ctx)
				provider.SetOriginalModel(compatibleStreamUsageModel)
				provider.SetUsage(&types.Usage{})
				var options *types.StreamOptions
				if test.name == "false" || test.name == "true" {
					options = &types.StreamOptions{IncludeUsage: test.clientUsage}
				}
				var delivered string
				if operation == "chat" {
					request := &types.ChatCompletionRequest{Model: compatibleStreamUsageModel, Stream: true, StreamOptions: options}
					stream, apiErr := provider.CreateChatCompletionStream(request)
					if apiErr != nil {
						t.Fatalf("chat stream: %+v", apiErr)
					}
					delivered = compatibleStreamUsageDrainStream(t, stream)
					if request.StreamOptions != options {
						t.Fatal("client options mutated")
					}
				} else {
					request := &types.CompletionRequest{Model: compatibleStreamUsageModel, Stream: true, StreamOptions: options}
					stream, apiErr := provider.CreateCompletionStream(request)
					if apiErr != nil {
						t.Fatalf("completion stream: %+v", apiErr)
					}
					delivered = compatibleStreamUsageDrainStream(t, stream)
					if request.StreamOptions != options {
						t.Fatal("client options mutated")
					}
				}
				calls, body := transport.snapshot()
				if calls != 1 {
					t.Fatalf("provider calls=%d", calls)
				}
				var before, after map[string]json.RawMessage
				if json.Unmarshal([]byte(raw), &before) != nil || json.Unmarshal(body, &after) != nil {
					t.Fatalf("invalid request body: %s", body)
				}
				for key, value := range before {
					if key != "stream_options" && !bytes.Equal(value, after[key]) {
						t.Fatalf("request field %s changed: %s", key, body)
					}
				}
				if test.wantUsage {
					var oldOptions, sentOptions map[string]json.RawMessage
					_ = json.Unmarshal(before["stream_options"], &oldOptions)
					if json.Unmarshal(after["stream_options"], &sentOptions) != nil || string(sentOptions["include_usage"]) != "true" {
						t.Fatalf("usage not requested: %s", body)
					}
					for key, value := range oldOptions {
						if key != "include_usage" && !bytes.Equal(value, sentOptions[key]) {
							t.Fatalf("option %s changed: %s", key, body)
						}
					}
				} else if !bytes.Equal(body, []byte(raw)) {
					t.Fatalf("provider-owned union rewritten: %s", body)
				}
				if !strings.Contains(delivered, `"ok"`) || strings.Contains(delivered, `"usage":`) != test.clientUsage ||
					strings.Contains(delivered, `"choices":[]`) != test.clientUsage {
					t.Fatalf("client display contract changed: %s", delivered)
				}
				usage := provider.GetUsage()
				if usage.HasProviderUsage() != test.wantUsage || (test.wantUsage && usage.TotalTokens != 7) {
					t.Fatalf("usage evidence: %+v", usage)
				}
			})
		}
	}
}

type compatibleStreamUsageExactCaptureTransport struct {
	mu        sync.Mutex
	operation string
	omitUsage bool
	reject    bool
	calls     int
	body      []byte
}

func (t *compatibleStreamUsageExactCaptureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.calls++
	t.body = append(t.body[:0], body...)
	t.mu.Unlock()
	if t.reject {
		return &http.Response{StatusCode: http.StatusUnprocessableEntity, Status: "422 Unprocessable Entity", Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"future options rejected","type":"invalid_request_error","code":"future_options","param":"stream_options"},"future_error":{"kept":true}}`)), Request: request}, nil
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	if string(fields["stream"]) != "true" {
		reply := `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`
		if t.operation == "completion" {
			reply = `{"choices":[{"index":0,"text":"ok"}]}`
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(reply)), Request: request,
		}, nil
	}
	responseBody := "data: {\"id\":\"issue002-exact\",\"object\":\"chat.completion.chunk\",\"model\":\"" + compatibleStreamUsageModel + "\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}],\"obfuscation\":\"content-padding\"}\n\ndata: [DONE]\n\n"
	if t.operation == "completion" {
		responseBody = "data: {\"id\":\"issue002-exact\",\"object\":\"text_completion\",\"model\":\"" + compatibleStreamUsageModel + "\",\"choices\":[{\"index\":0,\"text\":\"ok\"}],\"obfuscation\":\"content-padding\"}\n\ndata: [DONE]\n\n"
	}
	var options struct {
		IncludeUsage bool `json:"include_usage"`
	}
	_ = json.Unmarshal(fields["stream_options"], &options)
	if options.IncludeUsage && !t.omitUsage {
		responseBody = strings.Replace(responseBody, `"choices":`, `"usage":null,"choices":`, 1)
		object := "chat.completion.chunk"
		if t.operation == "completion" {
			object = "text_completion"
		}
		usage := "data: {\"id\":\"usage\",\"object\":\"" + object + "\",\"model\":\"" + compatibleStreamUsageModel + "\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7},\"obfuscation\":\"usage-padding\"}\n\n"
		responseBody = strings.Replace(responseBody, "data: [DONE]", usage+"data: [DONE]", 1)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(responseBody)),
		Request:    request,
	}, nil
}

func (t *compatibleStreamUsageExactCaptureTransport) snapshot() (int, []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls, append([]byte(nil), t.body...)
}

func TestUnaryDoesNotForceProviderUsage(t *testing.T) {
	server, state := compatibleStreamUsageUpstream(t, "chat")
	provider := compatibleStreamUsageNewProvider(t, config.ChannelTypeOpenAI, server.URL, nil)
	raw := `{"model":"` + compatibleStreamUsageModel + `","stream":false,"messages":[{"role":"user","content":"hello"}],"future_request_field":{"enabled":true}}`
	ctx := compatibleStreamUsageRequestContext(t, "/v1/chat/completions", true, raw)
	provider.SetContext(ctx)
	provider.SetOriginalModel(compatibleStreamUsageModel)
	provider.SetUsage(&types.Usage{})
	response, apiErr := provider.(base.ChatInterface).CreateChatCompletion(&types.ChatCompletionRequest{Model: compatibleStreamUsageModel, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}})
	if apiErr != nil || response == nil || len(response.Choices) != 1 {
		t.Fatalf("unary Chat failed: response=%+v err=%+v", response, apiErr)
	}
	compatibleStreamUsageAssertUpstreamWire(t, state, false, true)
	state.mu.Lock()
	streamRequest := state.streamRequest
	state.mu.Unlock()
	if streamRequest {
		t.Fatal("unary request reached upstream with stream=true")
	}
	if usage := provider.GetUsage(); usage.HasProviderUsage() || usage.ProviderReported {
		t.Fatalf("unary request unexpectedly published provider usage: %+v", usage)
	}
}

func TestProviderUsageReachesAttemptSQLAndRepeatedClose(t *testing.T) {
	compatibleStreamUsagePrepareBilling(t)
	server, state := compatibleStreamUsageUpstream(t, "chat")
	raw := `{"model":"` + compatibleStreamUsageModel + `","stream":true,"messages":[{"role":"user","content":"hello"}],"future_request_field":{"enabled":true}}`
	ctx := compatibleStreamUsageRequestContext(t, "/v1/chat/completions", true, raw)
	attempt, err := relay_util.NewAttemptQuota(ctx, compatibleStreamUsageModel, 0, relay_util.BillingAttemptSpec{LogProtocol: relay_util.LogProtocolHTTPStream})
	if err != nil {
		t.Fatalf("create I002 Attempt: %v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("reserve I002 quota: %v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("claim I002 submission: %v", err)
	}

	provider := compatibleStreamUsageNewProvider(t, config.ChannelTypeOpenAI, server.URL, nil)
	provider.SetContext(ctx)
	provider.SetOriginalModel(compatibleStreamUsageModel)
	provider.SetUsage(&types.Usage{})
	stream, apiErr := provider.(base.ChatInterface).CreateChatCompletionStream(&types.ChatCompletionRequest{Model: compatibleStreamUsageModel, Stream: true, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}})
	if apiErr != nil {
		t.Fatalf("CreateChatCompletionStream failed: %+v", apiErr)
	}
	_ = compatibleStreamUsageDrainStream(t, stream)
	compatibleStreamUsageAssertUpstreamWire(t, state, true, true)
	usage := provider.GetUsage()
	if !usage.HasProviderUsage() || !usage.ProviderReported || usage.PromptTokens != 3 || usage.CompletionTokens != 4 || usage.TotalTokens != 7 {
		t.Fatalf("provider Usage did not carry real prompt/completion/total evidence: %+v", usage)
	}

	first, err := attempt.CloseFromProviderResult(ctx.Request.Context(), usage, true)
	if err != nil || !first.Confirmed || first.ChargedQuota != 7 {
		t.Fatalf("I002 provider usage did not settle as SQL quota 7: result=%+v err=%v", first, err)
	}
	second, err := attempt.CloseFromProviderResult(ctx.Request.Context(), usage, true)
	if err != nil || !reflect.DeepEqual(second, first) {
		t.Fatalf("repeated Close changed I002 settlement: first=%+v second=%+v err=%v", first, second, err)
	}
	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("read I002 user: %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("read I002 token: %v", err)
	}
	if user.Quota != 99993 || user.UsedQuota != 7 || token.RemainQuota != 99993 || token.UsedQuota != 7 {
		t.Fatalf("I002 SQL balance changed by wrong amount: user=%+v token=%+v", user, token)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatalf("read I002 consume logs: %v", err)
	}
	if len(logs) != 1 || logs[0].Quota != 7 || logs[0].PromptTokens != 3 || logs[0].CompletionTokens != 4 || !logs[0].IsStream || logs[0].ModelName != compatibleStreamUsageModel {
		t.Fatalf("I002 SQL consume log mismatch or repeated charge: logs=%+v", logs)
	}
}

type compatibleStreamUsageStreamingProvider interface {
	base.ProviderInterface
	base.ChatInterface
	base.CompletionInterface
}

func compatibleStreamUsageNewProvider(t *testing.T, channelType int, baseURL string, custom *string) compatibleStreamUsageStreamingProvider {
	t.Helper()
	proxy := ""
	channel := &model.Channel{
		Type: channelType, Key: "issue002-provider-key", Proxy: &proxy, BaseURL: &baseURL,
		Other: `{"api_version":"2024-10-01-preview"}`, CustomParameter: custom,
	}
	var provider base.ProviderInterface
	switch channelType {
	case config.ChannelTypeOpenAI:
		provider = openai.CreateOpenAIProvider(channel, baseURL)
	case config.ChannelTypeAzure:
		provider = (azure.AzureProviderFactory{}).Create(channel)
	case config.ChannelTypeAzureV1:
		provider = (azurev1.AzureV1ProviderFactory{}).Create(channel)
	default:
		t.Fatalf("unsupported I002 test channel type %d", channelType)
	}
	streamingProvider, ok := provider.(compatibleStreamUsageStreamingProvider)
	if !ok {
		t.Fatalf("provider type %T does not implement Chat/Completion streaming", provider)
	}
	return streamingProvider
}

func compatibleStreamUsageUpstream(t *testing.T, operation string) (*httptest.Server, *compatibleStreamUsageUpstreamState) {
	t.Helper()
	state := &compatibleStreamUsageUpstreamState{}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Any("/*path", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		includeUsage := false
		var streamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		}
		if rawOptions := fields["stream_options"]; len(rawOptions) > 0 {
			_ = json.Unmarshal(rawOptions, &streamOptions)
			includeUsage = streamOptions.IncludeUsage
		}
		streamRequest := false
		_ = json.Unmarshal(fields["stream"], &streamRequest)
		state.mu.Lock()
		state.calls++
		state.lastBody = append(state.lastBody[:0], body...)
		state.includeUsage = includeUsage
		state.streamRequest = streamRequest
		state.mu.Unlock()

		if !streamRequest {
			c.Header("Content-Type", "application/json")
			if operation == "completion" {
				_, _ = io.WriteString(c.Writer, `{"id":"cmpl-i002","object":"text_completion","model":"`+compatibleStreamUsageModel+`","choices":[{"text":"ok","index":0,"finish_reason":"stop"}]}`)
			} else {
				_, _ = io.WriteString(c.Writer, `{"id":"chatcmpl-i002","object":"chat.completion","model":"`+compatibleStreamUsageModel+`","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}
			return
		}

		c.Header("Content-Type", "text/event-stream")
		if operation == "completion" {
			_, _ = io.WriteString(c.Writer, "data: {\"id\":\"cmpl-i002\",\"object\":\"text_completion\",\"model\":\""+compatibleStreamUsageModel+"\",\"choices\":[{\"text\":\"ok\",\"index\":0,\"finish_reason\":null}]}\n\n")
		} else {
			_, _ = io.WriteString(c.Writer, "data: {\"id\":\"chatcmpl-i002\",\"object\":\"chat.completion.chunk\",\"model\":\""+compatibleStreamUsageModel+"\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		}
		if includeUsage {
			_, _ = io.WriteString(c.Writer, "data: {\"id\":\""+map[bool]string{true: "cmpl-i002", false: "chatcmpl-i002"}[operation == "completion"]+"\",\"object\":\"chat.completion.chunk\",\"model\":\""+compatibleStreamUsageModel+"\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n")
		}
		_, _ = io.WriteString(c.Writer, "data: [DONE]\n\n")
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
	})
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	return server, state
}

func compatibleStreamUsageRequestContext(t *testing.T, path string, raw bool, body string) *gin.Context {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	var reader io.Reader
	if raw {
		reader = strings.NewReader(body)
	}
	ctx.Request = httptest.NewRequest(http.MethodPost, path, reader).WithContext(context.Background())
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("group_ratio", 1.0)
	groupctx.SetRoutingGroup(ctx, "issue002", groupctx.RoutingGroupSourceUserGroup)
	if raw {
		if _, err := common.CacheRequestBody(ctx); err != nil {
			t.Fatalf("cache I002 raw body: %v", err)
		}
	}
	return ctx
}

func compatibleStreamUsageDrainStream(t *testing.T, stream requester.StreamReaderInterface[string]) string {
	t.Helper()
	if stream == nil {
		t.Fatal("I002 stream is nil")
	}
	defer requester.CloseAndDrainStream(stream)
	dataChan, errChan := stream.Recv()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var delivered strings.Builder
	for dataChan != nil || errChan != nil {
		select {
		case data, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}
			delivered.WriteString(data)
		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("I002 stream failed: %v", err)
			}
		case <-timer.C:
			t.Fatal("waiting for I002 stream timed out")
		}
	}
	return delivered.String()
}

func compatibleStreamUsageAssertUpstreamWire(t *testing.T, state *compatibleStreamUsageUpstreamState, wantUsage, rawRequest bool) {
	t.Helper()
	state.mu.Lock()
	calls, includeUsage, body := state.calls, state.includeUsage, append([]byte(nil), state.lastBody...)
	state.mu.Unlock()
	if calls != 1 || includeUsage != wantUsage {
		t.Fatalf("I002 final provider switch mismatch: calls=%d include_usage=%v want=%v body=%s", calls, includeUsage, wantUsage, body)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode I002 final provider body: %v body=%s", err, body)
	}
	if rawRequest && fields["future_request_field"] == nil {
		t.Fatalf("unknown top-level field was lost from final provider body: %s", body)
	}
	if wantUsage {
		var options map[string]json.RawMessage
		if err := json.Unmarshal(fields["stream_options"], &options); err != nil || string(options["include_usage"]) != "true" {
			t.Fatalf("include_usage was not written at final provider boundary: %s", body)
		}
	}
}

func compatibleStreamUsageLastBody(state *compatibleStreamUsageUpstreamState) []byte {
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]byte(nil), state.lastBody...)
}

func stringPtr(value string) *string {
	return &value
}

func compatibleStreamUsagePrepareBilling(t *testing.T) {
	t.Helper()
	setupRelayTestDB(t, &model.User{}, &model.Token{}, &model.Log{})
	model.GlobalUserGroupRatio.Lock()
	oldGroups := model.GlobalUserGroupRatio.UserGroup
	groups := make(map[string]*model.UserGroup, len(oldGroups)+1)
	for key, value := range oldGroups {
		groups[key] = value
	}
	groups["issue002"] = &model.UserGroup{Symbol: "issue002", Ratio: 1}
	model.GlobalUserGroupRatio.UserGroup = groups
	model.GlobalUserGroupRatio.Unlock()
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.UserGroup = oldGroups
		model.GlobalUserGroupRatio.Unlock()
	})
	if err := model.DB.Create(&model.User{
		Id: 1, Username: "issue002-user", Password: "password123", AccessToken: "issue002-access",
		Quota: 100000, Group: "issue002", Status: config.UserStatusEnabled, Role: config.RoleCommonUser,
	}).Error; err != nil {
		t.Fatalf("create I002 user: %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id: 1, UserId: 1, Key: "issue002-token", Name: "issue002-token", Status: config.TokenStatusEnabled,
		ExpiredTime: -1, RemainQuota: 100000, Group: "issue002",
	}).Error; err != nil {
		t.Fatalf("create I002 token: %v", err)
	}

	oldPricing := model.PricingInstance
	oldBatch, oldRedis, oldReserve, oldLog := config.BatchUpdateEnabled, config.RedisEnabled, config.PreConsumedQuota, config.LogConsumeEnabled
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		compatibleStreamUsageModel: {Model: compatibleStreamUsageModel, Type: model.TokensPriceType, Input: 1, Output: 1},
	}}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.PreConsumedQuota = 50
	config.LogConsumeEnabled = true
	t.Cleanup(func() {
		model.PricingInstance = oldPricing
		config.BatchUpdateEnabled, config.RedisEnabled, config.PreConsumedQuota, config.LogConsumeEnabled = oldBatch, oldRedis, oldReserve, oldLog
	})
}

func TestOfficialStreamUsageThroughRelayEntryAndSQL(t *testing.T) {
	oldDisableTokenEncoders := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() { config.DisableTokenEncoders = oldDisableTokenEncoders })
	for _, operation := range []string{"chat", "completion"} {
		for _, test := range []struct {
			name, options, custom      string
			display, omitUsage, reject bool
		}{
			{name: "omitted"},
			{name: "false", options: `,"stream_options":{"include_usage":false}`},
			{name: "true", options: `,"stream_options":{"include_usage":true}`, display: true},
			{name: "null", options: `,"stream_options":null`},
			{name: "pre_add enables display", options: `,"stream_options":{"include_usage":false}`, custom: `{"pre_add":true,"overwrite":true,"stream_options":{"include_usage":true}}`},
			{name: "pre_add disables display", options: `,"stream_options":{"include_usage":true}`, custom: `{"pre_add":true,"overwrite":true,"stream_options":{"include_usage":false}}`, display: true},
			{name: "future array", options: `,"stream_options":["future"]`, omitUsage: true},
			{name: "future string", options: `,"stream_options":"future"`, omitUsage: true},
			{name: "future number", options: `,"stream_options":42`, omitUsage: true},
			{name: "future obfuscation union", options: `,"stream_options":{"include_obfuscation":{"future":true}}`},
			{name: "upstream rejects include_usage union", options: `,"stream_options":{"include_usage":{"future":true}}`, reject: true},
			{name: "post custom false keeps stream", custom: `{"overwrite":true,"stream":false}`},
			{name: "post custom removal keeps stream", custom: `{"remove_params":["stream"]}`},
			{name: "upstream rejects future value", options: `,"stream_options":["future"]`, reject: true},
			{name: "upstream omits usage", options: `,"stream_options":{"include_usage":true}`, omitUsage: true},
		} {
			t.Run(operation+"/"+test.name, func(t *testing.T) {
				compatibleStreamUsagePrepareBilling(t)
				transport := &compatibleStreamUsageExactCaptureTransport{operation: operation, omitUsage: test.omitUsage, reject: test.reject}
				previousClient := requester.HTTPClient
				requester.HTTPClient = &http.Client{Transport: transport}
				t.Cleanup(func() { requester.HTTPClient = previousClient })
				channel := capabilitySelectionChannel(1, config.ChannelTypeOpenAI, compatibleStreamUsageModel)
				channel.Key = "official-key"
				if test.custom != "" {
					channel.CustomParameter = &test.custom
				}
				path, input := "/v1/chat/completions", `"messages":[{"role":"user","content":"hello"}]`
				if operation == "completion" {
					path, input = "/v1/completions", `"prompt":"hello"`
				}
				raw := `{"model":"` + compatibleStreamUsageModel + `","stream":true,` + input + test.options + `,"future_request_field":{"n":9007199254740993,"text":"\u0061"}}`
				ctx := capabilitySelectionContext(t, path, raw, []*model.Channel{channel})
				recorder := httptest.NewRecorder()
				recordingContext, _ := gin.CreateTestContext(recorder)
				ctx.Writer = recordingContext.Writer
				ctx.Set("id", 1)
				ctx.Set("token_id", 1)
				ctx.Set("group_ratio", 1.0)
				groupctx.SetRoutingGroup(ctx, "issue002", groupctx.RoutingGroupSourceUserGroup)
				// 选路与结算使用相同的已授权分组。
				model.ChannelGroup.Rule["issue002"] = model.ChannelGroup.Rule["default"]
				model.ChannelGroup.ModelGroup[compatibleStreamUsageModel]["issue002"] = true
				channel.Group = "issue002"
				relayer := Path2Relay(ctx, path)
				if err := relayer.setRequest(); err != nil {
					t.Fatalf("relay decode: %v", err)
				}
				ctx.Set("is_stream", relayer.IsStream())
				if err := relayer.setProvider(relayer.getOriginalModel()); err != nil {
					t.Fatalf("select provider: %v", err)
				}
				if err := finalizeSelectedProviderRequest(relayer); err != nil {
					t.Fatalf("finalize provider: %v", err)
				}
				apiErr, done := RelayHandler(relayer)
				if !done {
					t.Fatal("submitted stream may be retried")
				}
				if test.reject {
					if apiErr == nil || apiErr.StatusCode != http.StatusUnprocessableEntity || apiErr.Code != "future_options" || apiErr.Param != "stream_options" {
						t.Fatalf("upstream error changed: %+v", apiErr)
					}
					relayer.HandleJsonError(apiErr)
				} else if apiErr != nil {
					t.Fatalf("relay failed: %+v", apiErr)
				}
				calls, wire := transport.snapshot()
				if calls != 1 {
					t.Fatalf("provider calls=%d", calls)
				}
				var before, after map[string]json.RawMessage
				_ = json.Unmarshal([]byte(raw), &before)
				_ = json.Unmarshal(wire, &after)
				if !bytes.Equal(before["future_request_field"], after["future_request_field"]) {
					t.Fatalf("future request field changed: %s", wire)
				}
				if strings.HasPrefix(test.name, "future ") && test.name != "future obfuscation union" && !bytes.Equal(before["stream_options"], after["stream_options"]) {
					t.Fatalf("future union changed: %s", wire)
				}
				if test.name == "future obfuscation union" {
					var options map[string]json.RawMessage
					_ = json.Unmarshal(after["stream_options"], &options)
					if string(options["include_obfuscation"]) != `{"future":true}` {
						t.Fatalf("future obfuscation changed: %s", wire)
					}
				}
				delivered := recorder.Body.String()
				if test.reject {
					if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(delivered, `"future_error":{"kept":true}`) {
						t.Fatalf("upstream error wire changed: status=%d body=%s", recorder.Code, delivered)
					}
				} else {
					displayUsage := test.display && !test.omitUsage
					if strings.Contains(delivered, `"usage":`) != displayUsage || strings.Contains(delivered, `"choices":[]`) != displayUsage ||
						!strings.Contains(delivered, `"ok"`) || !strings.Contains(delivered, `"obfuscation":"content-padding"`) {
						t.Fatalf("client display changed: %s", delivered)
					}
				}
				wantCharge := 7
				if test.omitUsage || test.reject {
					wantCharge = 0
				}
				var user model.User
				var token model.Token
				if err := model.DB.First(&user, 1).Error; err != nil {
					t.Fatal(err)
				}
				if err := model.DB.First(&token, 1).Error; err != nil {
					t.Fatal(err)
				}
				if user.Quota != 100000-wantCharge || user.UsedQuota != wantCharge || token.RemainQuota != 100000-wantCharge || token.UsedQuota != wantCharge {
					t.Fatalf("incorrect settlement: user=%+v token=%+v", user, token)
				}
				var logs []model.Log
				if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
					t.Fatal(err)
				}
				wantCount := 1
				if wantCharge == 0 {
					wantCount = 0
				}
				if len(logs) != wantCount || user.RequestCount != wantCount {
					t.Fatalf("usage projection mismatch: logs=%+v requests=%d", logs, user.RequestCount)
				}
			})
		}
	}
}

func TestChatResponsesRejectsUnrepresentableStreamOptionsBeforeProviderWork(t *testing.T) {
	for _, options := range []string{`["future"]`, `"future"`, `42`, `{"include_usage":"future"}`, `{"include_obfuscation":{"future":true}}`} {
		t.Run(options, func(t *testing.T) {
			channel := capabilitySelectionChannel(1, config.ChannelTypeOpenAI, "o3-pro")
			ctx := capabilitySelectionContext(t, "/v1/chat/completions", `{"model":"o3-pro","stream":true,"messages":[{"role":"user","content":"hello"}],"stream_options":`+options+`}`, []*model.Channel{channel})
			relayer := NewRelayChat(ctx)
			if err := relayer.setRequest(); err != nil {
				t.Fatalf("native envelope decoding rejected upstream-owned options: %v", err)
			}
			if err := relayer.setProvider(relayer.getOriginalModel()); err == nil {
				t.Fatal("unrepresentable stream options passed channel capability assessment")
			}
		})
	}
}

func TestStreamOptionsCandidateMatchesUpstreamRequest(t *testing.T) {
	oldDisableTokenEncoders := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() { config.DisableTokenEncoders = oldDisableTokenEncoders })
	for _, test := range []struct {
		name, options, siblings, custom, wantOptions            string
		channelType                                             int
		allowExtra, accepted, unary, completion, upstreamReject bool
	}{
		{name: "typed future string", channelType: config.ChannelTypeDeepseek, options: `"future"`},
		{name: "typed future include_usage", channelType: config.ChannelTypeDeepseek, options: `{"include_usage":{"future":true}}`},
		{name: "typed future include_obfuscation", channelType: config.ChannelTypeDeepseek, options: `{"include_obfuscation":{"future":true}}`},
		{name: "typed true", channelType: config.ChannelTypeDeepseek, options: `{"include_usage":true,"include_obfuscation":true}`, accepted: true, wantOptions: `{"include_usage":true,"include_obfuscation":true}`},
		{name: "typed false", channelType: config.ChannelTypeDeepseek, options: `{"include_usage":false,"include_obfuscation":false}`, accepted: true, wantOptions: `{"include_usage":true,"include_obfuscation":false}`},
		{name: "Groq extra option", channelType: config.ChannelTypeGroq, options: `{"vendor_option":9007199254740993}`, allowExtra: true, accepted: true, wantOptions: `{"vendor_option":9007199254740993}`},
		{name: "Groq typed projection", channelType: config.ChannelTypeGroq, options: `{"vendor_option":true}`},
		{name: "Baichuan extra option", channelType: config.ChannelTypeBaichuan, options: `{"vendor_option":9007199254740993}`, allowExtra: true, accepted: true, wantOptions: `{"vendor_option":9007199254740993}`},
		{name: "Baichuan typed projection", channelType: config.ChannelTypeBaichuan, options: `{"vendor_option":true}`},
		{name: "Moonshot unary extra option", channelType: config.ChannelTypeMoonshot, options: `{"vendor_option":9007199254740993}`, unary: true, allowExtra: true, accepted: true, wantOptions: `{"vendor_option":9007199254740993}`},
		{name: "Moonshot streaming replaces options", channelType: config.ChannelTypeMoonshot, options: `{"vendor_option":true}`, allowExtra: true},
		{name: "OpenRouter unary extra option", channelType: config.ChannelTypeOpenRouter, options: `{"vendor_option":9007199254740993}`, unary: true, allowExtra: true, accepted: true, wantOptions: `{"vendor_option":9007199254740993}`},
		{name: "OpenRouter streaming replaces options", channelType: config.ChannelTypeOpenRouter, options: `{"vendor_option":true}`, allowExtra: true},
		{name: "OpenAI unary Completion leaves validation upstream", channelType: config.ChannelTypeOpenAI, options: `{"include_usage":{"future":true}}`, unary: true, completion: true, upstreamReject: true},
		{name: "typed unary Completion keeps DTO options", channelType: config.ChannelType360, options: `{"include_usage":true,"include_obfuscation":false}`, unary: true, accepted: true, wantOptions: `{"include_usage":true,"include_obfuscation":false}`},
		{name: "typed unary Completion keeps extra options", channelType: config.ChannelType360, options: `{"include_usage":"future","extension":9007199254740993}`, unary: true, allowExtra: true, accepted: true, wantOptions: `{"include_usage":"future","extension":9007199254740993}`},
		{name: "typed unary Completion defers union", channelType: config.ChannelType360, options: `{"include_usage":{"future":9007199254740993}}`, unary: true, accepted: true, wantOptions: `{"include_usage":{"future":9007199254740993}}`, siblings: `,"unregistered_extra":true`},
		{name: "typed unary Completion preserves false and null", channelType: config.ChannelType360, options: `{"include_usage":false,"include_obfuscation":null}`, unary: true, accepted: true, wantOptions: `{"include_usage":false,"include_obfuscation":null}`},
		{name: "typed unary Completion channel overlay", channelType: config.ChannelType360, options: `{"include_usage":false,"nested":9007199254740993}`, custom: `{"stream_options":{"from_channel":true}}`, unary: true, accepted: true, wantOptions: `{"include_usage":false,"nested":9007199254740993,"from_channel":true}`},
		{name: "Mistral extra option", channelType: config.ChannelTypeMistral, options: `{"vendor_option":9007199254740993}`, allowExtra: true, accepted: true, wantOptions: `{"vendor_option":9007199254740993}`},
		{name: "typed extra plus usage", channelType: config.ChannelTypeDeepseek, options: `{"include_usage":false,"vendor_option":9007199254740993}`, allowExtra: true, accepted: true, wantOptions: `{"include_usage":true,"vendor_option":9007199254740993}`},
		{name: "typed exact field name", channelType: config.ChannelTypeDeepseek, options: `{"include_obfuscation":true}`, siblings: `,"STREAM_OPTIONS":{"include_obfuscation":false}`, accepted: true, wantOptions: `{"include_usage":true,"include_obfuscation":true}`},
		{name: "Completion exact field name", channelType: config.ChannelType360, options: `{"include_usage":"future"}`, siblings: `,"STREAM_OPTIONS":{}`},
		{name: "Completion projection", channelType: config.ChannelType360, options: `{"include_usage":"future"}`},
		{name: "Completion extra option", channelType: config.ChannelType360, options: `{"include_usage":"future"}`, allowExtra: true, accepted: true, wantOptions: `{"include_usage":"future"}`},
		{name: "Completion pre_add introduces union", channelType: config.ChannelType360, options: `{}`, custom: `{"pre_add":true,"overwrite":true,"stream_options":{"vendor_option":1}}`},
		{name: "Completion pre_add removes union", channelType: config.ChannelType360, options: `{"include_usage":"future"}`, custom: `{"pre_add":true,"remove_params":["stream_options"]}`, accepted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			compatibleStreamUsagePrepareBilling(t)
			operation, path, input := "chat", "/v1/chat/completions", `"messages":[{"role":"user","content":"hello"}]`
			if test.channelType == config.ChannelType360 || test.completion {
				operation, path, input = "completion", "/v1/completions", `"prompt":"hello"`
			}
			transport := &compatibleStreamUsageExactCaptureTransport{operation: operation, reject: test.upstreamReject}
			previousClient := requester.HTTPClient
			requester.HTTPClient = &http.Client{Transport: transport}
			t.Cleanup(func() { requester.HTTPClient = previousClient })
			channel := capabilitySelectionChannel(1, test.channelType, compatibleStreamUsageModel)
			channel.Key = "typed-provider-key"
			if test.channelType != config.ChannelTypeOpenAI {
				channel.BaseURL = stringPtr("https://upstream.example")
			}
			channel.AllowExtraBody = test.allowExtra
			if test.custom != "" {
				channel.CustomParameter = &test.custom
			}
			stream := "true"
			if test.unary {
				stream = "false"
			}
			raw := `{"model":"` + compatibleStreamUsageModel + `","stream":` + stream + `,` + input + `,"stream_options":` + test.options + test.siblings + `}`
			ctx := capabilitySelectionContext(t, path, raw, []*model.Channel{channel})
			recorder := httptest.NewRecorder()
			recordingContext, _ := gin.CreateTestContext(recorder)
			ctx.Writer = recordingContext.Writer
			ctx.Set("id", 1)
			ctx.Set("token_id", 1)
			ctx.Set("group_ratio", 1.0)
			groupctx.SetRoutingGroup(ctx, "issue002", groupctx.RoutingGroupSourceUserGroup)
			model.ChannelGroup.Rule["issue002"] = model.ChannelGroup.Rule["default"]
			model.ChannelGroup.ModelGroup[compatibleStreamUsageModel]["issue002"] = true
			channel.Group = "issue002"

			Relay(ctx)

			calls, wire := transport.snapshot()
			if test.upstreamReject {
				if calls != 1 || recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), `"future_error":{"kept":true}`) || !bytes.Equal(wire, []byte(raw)) {
					t.Fatalf("非流式原生请求或上游错误被改写：calls=%d status=%d wire=%s body=%s", calls, recorder.Code, wire, recorder.Body.String())
				}
				return
			}
			if test.accepted {
				if calls != 1 || recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ok"`) {
					t.Fatalf("expected upstream request failed or retried: calls=%d status=%d body=%s", calls, recorder.Code, recorder.Body.String())
				}
				var sent map[string]json.RawMessage
				if err := json.Unmarshal(wire, &sent); err != nil {
					t.Fatal(err)
				}
				if !test.allowExtra && sent["unregistered_extra"] != nil {
					t.Fatalf("透传已知 stream_options 时带入了额外的顶层字段：%s", wire)
				}
				var got, want map[string]json.RawMessage
				if options := sent["stream_options"]; len(options) != 0 {
					if err := json.Unmarshal(options, &got); err != nil {
						t.Fatal(err)
					}
				}
				if test.wantOptions != "" {
					if err := json.Unmarshal([]byte(test.wantOptions), &want); err != nil {
						t.Fatal(err)
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("上游 stream_options = %s，预期 %s", sent["stream_options"], test.wantOptions)
				}
				return
			}
			if calls != 0 || recorder.Code == http.StatusOK || !strings.Contains(recorder.Body.String(), unsupportedCapabilityCode) {
				t.Fatalf("unrepresentable typed options executed or lost capability error: calls=%d status=%d body=%s", calls, recorder.Code, recorder.Body.String())
			}
			var user model.User
			var token model.Token
			if err := model.DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			var logs []model.Log
			if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != 100000 || user.UsedQuota != 0 || user.RequestCount != 0 || token.RemainQuota != 100000 || token.UsedQuota != 0 || len(logs) != 0 {
				t.Fatalf("rejected typed options changed billing: user=%+v token=%+v logs=%+v", user, token, logs)
			}
		})
	}
}
