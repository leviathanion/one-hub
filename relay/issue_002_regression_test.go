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

const issue002Model = "issue002-stream-model"

type issue002UpstreamState struct {
	mu            sync.Mutex
	calls         int
	lastBody      []byte
	includeUsage  bool
	streamRequest bool
}

func TestIssue002CompatibleStreamingAddsUsageAtFinalWire(t *testing.T) {
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
			server, state := issue002Upstream(t, test.operation)
			provider := issue002NewProvider(t, test.channel, server.URL, nil)
			path := "/v1/chat/completions"
			if test.operation == "completion" {
				path = "/v1/completions"
			}
			var raw string
			if test.operation == "chat" {
				raw = `{"model":"` + issue002Model + `","stream":true,"messages":[{"role":"user","content":"hello"}],"stream_options":{"include_obfuscation":true,"future_option":{"enabled":true}},"future_request_field":{"enabled":true}}`
			} else {
				raw = `{"model":"` + issue002Model + `","prompt":"hello","stream":true,"stream_options":{"include_obfuscation":true,"future_option":{"enabled":true}},"future_request_field":{"enabled":true}}`
			}
			ctx := issue002RequestContext(t, path, test.raw, raw)
			provider.SetContext(ctx)
			provider.SetOriginalModel(issue002Model)
			provider.SetUsage(&types.Usage{})

			var delivered string
			if test.operation == "chat" {
				request := &types.ChatCompletionRequest{Model: issue002Model, Stream: true, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}}
				if test.raw {
					if err := json.Unmarshal([]byte(raw), request); err != nil {
						t.Fatalf("decode raw Chat request: %v", err)
					}
				}
				stream, apiErr := provider.(base.ChatInterface).CreateChatCompletionStream(request)
				if apiErr != nil {
					t.Fatalf("CreateChatCompletionStream failed: %+v", apiErr)
				}
				delivered = issue002DrainStream(t, stream)
			} else {
				request := &types.CompletionRequest{Model: issue002Model, Prompt: "hello", Stream: true}
				if obfuscation := true; !test.raw {
					request.StreamOptions = &types.StreamOptions{IncludeObfuscation: &obfuscation}
				} else if err := json.Unmarshal([]byte(raw), request); err != nil {
					t.Fatalf("decode raw Completion request: %v", err)
				}
				stream, apiErr := provider.(base.CompletionInterface).CreateCompletionStream(request)
				if apiErr != nil {
					t.Fatalf("CreateCompletionStream failed: %+v", apiErr)
				}
				delivered = issue002DrainStream(t, stream)
			}

			if !strings.Contains(delivered, `"ok"`) {
				t.Fatalf("successful stream did not reach downstream: %s", delivered)
			}
			usage := provider.GetUsage()
			if !usage.HasProviderUsage() || !usage.ProviderReported || usage.PromptTokens != 3 || usage.CompletionTokens != 4 || usage.TotalTokens != 7 {
				t.Fatalf("provider usage evidence missing or changed: %+v", usage)
			}
			issue002AssertUpstreamWire(t, state, true, test.raw)
		})
	}
}

func TestIssue002UsageControlsPreserveExactAndProviderOwnedFields(t *testing.T) {
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
			raw:       `{"model":"` + issue002Model + `","stream":true,"messages":[{"role":"user","content":"hello"}],"future_request_field":{"enabled":true}}`,
			support:   false,
			wantUsage: false,
		},
		{
			name:          "existing options",
			raw:           `{"model":"` + issue002Model + `","stream":true,"messages":[{"role":"user","content":"hello"}],"stream_options":{"include_usage":false,"include_obfuscation":false,"future_option":{"enabled":true}},"future_request_field":{"enabled":true}}`,
			support:       true,
			wantUsage:     true,
			wantFutureKey: "future_option",
		},
		{
			name:          "custom before provider usage",
			raw:           `{"model":"` + issue002Model + `","stream":true,"messages":[{"role":"user","content":"hello"}],"future_request_field":{"enabled":true}}`,
			custom:        stringPtr(`{"overwrite":true,"stream_options":{"include_usage":false,"custom_future":{"enabled":true}}}`),
			support:       true,
			wantUsage:     true,
			wantFutureKey: "custom_future",
		},
		{
			name:          "custom typed builder before provider usage",
			raw:           `{"model":"` + issue002Model + `","stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			custom:        stringPtr(`{"overwrite":true,"stream_options":{"include_usage":false,"custom_future":{"enabled":true}}}`),
			support:       true,
			wantUsage:     true,
			wantFutureKey: "custom_future",
			typed:         true,
		},
		{
			name:          "pre_add before provider usage",
			raw:           `{"model":"` + issue002Model + `","stream":true,"messages":[{"role":"user","content":"hello"}],"stream_options":{"include_usage":false,"pre_future":{"enabled":true}},"future_request_field":{"enabled":true}}`,
			custom:        stringPtr(`{"pre_add":true,"overwrite":true,"stream_options":{"include_usage":false,"pre_future":{"enabled":true}}}`),
			support:       true,
			wantUsage:     true,
			wantFutureKey: "pre_future",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, state := issue002Upstream(t, "chat")
			provider := issue002NewProvider(t, config.ChannelTypeOpenAI, server.URL, test.custom)
			p, ok := provider.(*openai.OpenAIProvider)
			if !ok {
				t.Fatal("expected OpenAI provider")
			}
			p.SupportStreamOptions = test.support
			ctx := issue002RequestContext(t, "/v1/chat/completions", !test.typed, test.raw)
			provider.SetContext(ctx)
			provider.SetOriginalModel(issue002Model)
			provider.SetUsage(&types.Usage{})
			request := &types.ChatCompletionRequest{Model: issue002Model, Stream: true, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}}
			if test.typed {
				obfuscation := true
				request.StreamOptions = &types.StreamOptions{IncludeObfuscation: &obfuscation}
			}
			stream, apiErr := p.CreateChatCompletionStream(request)
			if apiErr != nil {
				t.Fatalf("CreateChatCompletionStream failed: %+v", apiErr)
			}
			_ = issue002DrainStream(t, stream)
			issue002AssertUpstreamWire(t, state, test.wantUsage, !test.typed)
			usage := provider.GetUsage()
			if test.wantUsage {
				if !usage.HasProviderUsage() || usage.TotalTokens != 7 {
					t.Fatalf("forced usage did not become provider evidence: %+v", usage)
				}
			} else if usage.HasProviderUsage() || usage.ProviderReported {
				t.Fatalf("unsupported/exact stream unexpectedly became billable: %+v", usage)
			}
			if test.wantFutureKey != "" {
				body := issue002LastBody(state)
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

func TestIssue002OfficialFactoryExactStreamingKeepsRawWire(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation string
		raw       string
	}{
		{
			name:      "Chat",
			operation: "chat",
			raw:       `{"model":"` + issue002Model + `","stream":true,"messages":[{"role":"user","content":"hello"}],"stream_options":{"include_obfuscation":true,"future_option":{"enabled":true}},"future_request_field":{"enabled":true}}`,
		},
		{
			name:      "Completion",
			operation: "completion",
			raw:       `{"model":"` + issue002Model + `","prompt":"hello","stream":true,"stream_options":{"include_obfuscation":true,"future_option":{"enabled":true}},"future_request_field":{"enabled":true}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &issue002ExactCaptureTransport{operation: test.operation}
			previousClient := requester.HTTPClient
			requester.HTTPClient = &http.Client{Transport: transport}
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			proxy := ""
			provider := openai.CreateOpenAIProvider(&model.Channel{
				Type: config.ChannelTypeOpenAI, Key: "issue002-official-key", Proxy: &proxy,
			}, "https://api.openai.com")
			if !provider.ProviderRawJSONReplay || !provider.RequireOpenAIStreamTerminal || !provider.SupportStreamOptions {
				t.Fatalf("official factory did not naturally select exact strategy: replay=%v terminal=%v support_options=%v", provider.ProviderRawJSONReplay, provider.RequireOpenAIStreamTerminal, provider.SupportStreamOptions)
			}
			path := "/v1/chat/completions"
			if test.operation == "completion" {
				path = "/v1/completions"
			}
			ctx := issue002RequestContext(t, path, true, test.raw)
			provider.SetContext(ctx)
			provider.SetOriginalModel(issue002Model)
			provider.SetUsage(&types.Usage{})

			if test.operation == "chat" {
				var request types.ChatCompletionRequest
				if err := json.Unmarshal([]byte(test.raw), &request); err != nil {
					t.Fatalf("decode exact Chat request: %v", err)
				}
				stream, apiErr := provider.CreateChatCompletionStream(&request)
				if apiErr != nil {
					t.Fatalf("exact Chat stream failed: %+v", apiErr)
				}
				_ = issue002DrainStream(t, stream)
			} else {
				var request types.CompletionRequest
				if err := json.Unmarshal([]byte(test.raw), &request); err != nil {
					t.Fatalf("decode exact Completion request: %v", err)
				}
				stream, apiErr := provider.CreateCompletionStream(&request)
				if apiErr != nil {
					t.Fatalf("exact Completion stream failed: %+v", apiErr)
				}
				_ = issue002DrainStream(t, stream)
			}

			calls, body := transport.snapshot()
			if calls != 1 || !bytes.Equal(body, []byte(test.raw)) {
				t.Fatalf("official exact request was rewritten or sent unexpectedly: calls=%d body=%q want=%q", calls, body, test.raw)
			}
			if bytes.Contains(body, []byte(`"include_usage"`)) {
				t.Fatalf("official exact request received proxy include_usage injection: %s", body)
			}
			if usage := provider.GetUsage(); usage.ProviderReported || usage.HasProviderUsage() {
				t.Fatalf("exact stream without provider usage became billable: %+v", usage)
			}
		})
	}
}

type issue002ExactCaptureTransport struct {
	mu        sync.Mutex
	operation string
	calls     int
	body      []byte
}

func (t *issue002ExactCaptureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.calls++
	t.body = append(t.body[:0], body...)
	t.mu.Unlock()
	responseBody := "data: {\"id\":\"issue002-exact\",\"object\":\"chat.completion.chunk\",\"model\":\"" + issue002Model + "\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	if t.operation == "completion" {
		responseBody = "data: {\"id\":\"issue002-exact\",\"object\":\"text_completion\",\"model\":\"" + issue002Model + "\",\"choices\":[{\"index\":0,\"text\":\"ok\"}]}\n\ndata: [DONE]\n\n"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(responseBody)),
		Request:    request,
	}, nil
}

func (t *issue002ExactCaptureTransport) snapshot() (int, []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls, append([]byte(nil), t.body...)
}

func TestIssue002UnaryDoesNotForceProviderUsage(t *testing.T) {
	server, state := issue002Upstream(t, "chat")
	provider := issue002NewProvider(t, config.ChannelTypeOpenAI, server.URL, nil)
	raw := `{"model":"` + issue002Model + `","stream":false,"messages":[{"role":"user","content":"hello"}],"future_request_field":{"enabled":true}}`
	ctx := issue002RequestContext(t, "/v1/chat/completions", true, raw)
	provider.SetContext(ctx)
	provider.SetOriginalModel(issue002Model)
	provider.SetUsage(&types.Usage{})
	response, apiErr := provider.(base.ChatInterface).CreateChatCompletion(&types.ChatCompletionRequest{Model: issue002Model, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}})
	if apiErr != nil || response == nil || len(response.Choices) != 1 {
		t.Fatalf("unary Chat failed: response=%+v err=%+v", response, apiErr)
	}
	issue002AssertUpstreamWire(t, state, false, true)
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

func TestIssue002ProviderUsageReachesAttemptSQLAndRepeatedClose(t *testing.T) {
	issue002PrepareBilling(t)
	server, state := issue002Upstream(t, "chat")
	raw := `{"model":"` + issue002Model + `","stream":true,"messages":[{"role":"user","content":"hello"}],"future_request_field":{"enabled":true}}`
	ctx := issue002RequestContext(t, "/v1/chat/completions", true, raw)
	attempt, err := relay_util.NewAttemptQuota(ctx, issue002Model, 0, relay_util.BillingAttemptSpec{LogProtocol: relay_util.LogProtocolHTTPStream})
	if err != nil {
		t.Fatalf("create I002 Attempt: %v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("reserve I002 quota: %v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("claim I002 submission: %v", err)
	}

	provider := issue002NewProvider(t, config.ChannelTypeOpenAI, server.URL, nil)
	provider.SetContext(ctx)
	provider.SetOriginalModel(issue002Model)
	provider.SetUsage(&types.Usage{})
	stream, apiErr := provider.(base.ChatInterface).CreateChatCompletionStream(&types.ChatCompletionRequest{Model: issue002Model, Stream: true, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}})
	if apiErr != nil {
		t.Fatalf("CreateChatCompletionStream failed: %+v", apiErr)
	}
	_ = issue002DrainStream(t, stream)
	issue002AssertUpstreamWire(t, state, true, true)
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
	if len(logs) != 1 || logs[0].Quota != 7 || logs[0].PromptTokens != 3 || logs[0].CompletionTokens != 4 || !logs[0].IsStream || logs[0].ModelName != issue002Model {
		t.Fatalf("I002 SQL consume log mismatch or repeated charge: logs=%+v", logs)
	}
}

type issue002StreamingProvider interface {
	base.ProviderInterface
	base.ChatInterface
	base.CompletionInterface
}

func issue002NewProvider(t *testing.T, channelType int, baseURL string, custom *string) issue002StreamingProvider {
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
	streamingProvider, ok := provider.(issue002StreamingProvider)
	if !ok {
		t.Fatalf("provider type %T does not implement Chat/Completion streaming", provider)
	}
	return streamingProvider
}

func issue002Upstream(t *testing.T, operation string) (*httptest.Server, *issue002UpstreamState) {
	t.Helper()
	state := &issue002UpstreamState{}
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
				_, _ = io.WriteString(c.Writer, `{"id":"cmpl-i002","object":"text_completion","model":"`+issue002Model+`","choices":[{"text":"ok","index":0,"finish_reason":"stop"}]}`)
			} else {
				_, _ = io.WriteString(c.Writer, `{"id":"chatcmpl-i002","object":"chat.completion","model":"`+issue002Model+`","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}
			return
		}

		c.Header("Content-Type", "text/event-stream")
		if operation == "completion" {
			_, _ = io.WriteString(c.Writer, "data: {\"id\":\"cmpl-i002\",\"object\":\"text_completion\",\"model\":\""+issue002Model+"\",\"choices\":[{\"text\":\"ok\",\"index\":0,\"finish_reason\":null}]}\n\n")
		} else {
			_, _ = io.WriteString(c.Writer, "data: {\"id\":\"chatcmpl-i002\",\"object\":\"chat.completion.chunk\",\"model\":\""+issue002Model+"\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		}
		if includeUsage {
			_, _ = io.WriteString(c.Writer, "data: {\"id\":\""+map[bool]string{true: "cmpl-i002", false: "chatcmpl-i002"}[operation == "completion"]+"\",\"object\":\"chat.completion.chunk\",\"model\":\""+issue002Model+"\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n")
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

func issue002RequestContext(t *testing.T, path string, raw bool, body string) *gin.Context {
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

func issue002DrainStream(t *testing.T, stream requester.StreamReaderInterface[string]) string {
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

func issue002AssertUpstreamWire(t *testing.T, state *issue002UpstreamState, wantUsage, rawRequest bool) {
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

func issue002LastBody(state *issue002UpstreamState) []byte {
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]byte(nil), state.lastBody...)
}

func stringPtr(value string) *string {
	return &value
}

func issue002PrepareBilling(t *testing.T) {
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
		issue002Model: {Model: issue002Model, Type: model.TokensPriceType, Input: 1, Output: 1},
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
