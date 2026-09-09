package providers

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"

	"gorm.io/datatypes"
)

type issue033FactoryCase struct {
	name        string
	channelType int
	model       string
	key         string
	plugin      model.PluginType
}

func issue033FactoryCases() []issue033FactoryCase {
	return []issue033FactoryCase{
		{name: "Ali native", channelType: config.ChannelTypeAli, model: "qwen-vl-plus", key: "sk-ali"},
		{name: "Ali compatible", channelType: config.ChannelTypeAli, model: "qwen-vl-plus", key: "sk-ali", plugin: model.PluginType{"use_openai_api": {"enable": true}}},
		{name: "Baidu compatible", channelType: config.ChannelTypeBaidu, model: "vision-model", key: "sk-baidu", plugin: model.PluginType{"use_openai_api": {"enable": true}}},
		{name: "Cohere", channelType: config.ChannelTypeCohere, model: "command-a-vision", key: "sk-cohere"},
		{name: "Groq", channelType: config.ChannelTypeGroq, model: "meta-llama/llama-4-scout-17b-16e-instruct", key: "sk-groq"},
		{name: "Mistral", channelType: config.ChannelTypeMistral, model: "pixtral-large-latest", key: "sk-mistral"},
		{name: "Siliconflow", channelType: config.ChannelTypeSiliconflow, model: "Qwen/Qwen2.5-VL-72B-Instruct", key: "sk-siliconflow"},
		{name: "xAI", channelType: config.ChannelTypeXAI, model: "grok-4", key: "sk-xai"},
		{name: "Zhipu", channelType: config.ChannelTypeZhipu, model: "glm-4.6v", key: "id.secret"},
		{name: "Codex Chat", channelType: config.ChannelTypeCodex, model: "gpt-5", key: "access-token"},
	}
}

func issue033Channel(testCase issue033FactoryCase, endpoint string) *model.Channel {
	proxy := ""
	channel := &model.Channel{
		Type:    testCase.channelType,
		Key:     testCase.key,
		Proxy:   &proxy,
		BaseURL: &endpoint,
	}
	if testCase.plugin != nil {
		plugin := datatypes.NewJSONType(testCase.plugin)
		channel.Plugin = &plugin
	}
	return channel
}

func issue033ChatRequest(modelName, imageURL string) *types.ChatCompletionRequest {
	return &types.ChatCompletionRequest{
		Model: modelName,
		Messages: []types.ChatCompletionMessage{{
			Role: types.ChatMessageRoleUser,
			Content: []types.ChatMessagePart{
				{Type: types.ContentTypeText, Text: "describe this image"},
				{Type: types.ContentTypeImageURL, ImageURL: &types.ChatMessageImageURL{URL: imageURL}},
			},
		}},
	}
}

func issue033TextRequest(modelName string) *types.ChatCompletionRequest {
	return &types.ChatCompletionRequest{
		Model:    modelName,
		Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hello"}},
	}
}

func issue033JSONContains(value any, needle string) bool {
	switch typed := value.(type) {
	case string:
		return strings.Contains(typed, needle)
	case []any:
		for _, item := range typed {
			if issue033JSONContains(item, needle) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if issue033JSONContains(item, needle) {
				return true
			}
		}
	}
	return false
}

func issue033UpstreamResponse(path string) (contentType, body string) {
	if strings.HasSuffix(path, "/v2/chat") {
		return "application/json", `{"id":"cohere-1","finish_reason":"COMPLETE","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`
	}
	if strings.Contains(path, "/multimodal-generation/") {
		return "application/json", `{"request_id":"ali-1","output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]},"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	}
	if strings.Contains(path, "/backend-api/codex/responses") {
		return "text/event-stream", "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\",\"object\":\"response\",\"status\":\"in_progress\"}}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"id\":\"msg-1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
	}
	return "application/json", `{"id":"chat-1","model":"vision-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
}

func TestIssue033RegisteredVisualFactoriesSendImageAndTextToLocalUpstream(t *testing.T) {
	previousClient := requester.HTTPClient
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read %s request: %v", r.URL.Path, err)
			return
		}
		bodies = append(bodies, append([]byte(nil), body...))
		contentType, responseBody := issue033UpstreamResponse(r.URL.Path)
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, responseBody)
	}))
	t.Cleanup(server.Close)
	requester.HTTPClient = server.Client()

	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("image"))
	for _, testCase := range issue033FactoryCases() {
		t.Run(testCase.name, func(t *testing.T) {
			for _, imageURL := range []string{"https://media.example/image.png", dataURI} {
				bodies = nil
				request := issue033ChatRequest(testCase.model, imageURL)
				channel := issue033Channel(testCase, server.URL)
				mode, summary, err := AssessChatRemoteMedia(channel, request)
				if err != nil || summary.Items != 1 || mode != base.RemoteMediaPassURL {
					t.Fatalf("image %q assessment: mode=%v summary=%+v err=%v", imageURL, mode, summary, err)
				}
				provider := createProvider(channel)
				if provider == nil {
					t.Fatal("factory did not create a provider")
				}
				provider.SetUsage(&types.Usage{})
				if err := PrepareChatRemoteMedia(provider, request, nil); err != nil {
					t.Fatalf("image %q preparation: %v", imageURL, err)
				}
				chat, ok := provider.(base.ChatInterface)
				if !ok {
					t.Fatal("provider does not implement ChatInterface")
				}
				response, apiErr := chat.CreateChatCompletion(request)
				if apiErr != nil || response == nil {
					t.Fatalf("image %q request failed: response=%+v err=%+v", imageURL, response, apiErr)
				}
				if len(bodies) != 1 {
					t.Fatalf("image %q upstream calls=%d, want 1", imageURL, len(bodies))
				}
				var wireBody any
				if err := json.Unmarshal(bodies[0], &wireBody); err != nil {
					t.Fatalf("image %q upstream body is not JSON: %v; body=%s", imageURL, err, bodies[0])
				}
				if !issue033JSONContains(wireBody, imageURL) || !issue033JSONContains(wireBody, "describe this image") {
					t.Fatalf("image %q was not represented in outgoing body: %s", imageURL, bodies[0])
				}
			}

			bodies = nil
			textRequest := issue033TextRequest(testCase.model)
			textChannel := issue033Channel(testCase, server.URL)
			if mode, summary, err := AssessChatRemoteMedia(textChannel, textRequest); err != nil || summary.Items != 0 || mode != base.RemoteMediaReject {
				t.Fatalf("text assessment: mode=%v summary=%+v err=%v", mode, summary, err)
			}
			provider := createProvider(textChannel)
			provider.SetUsage(&types.Usage{})
			chat, ok := provider.(base.ChatInterface)
			if !ok {
				t.Fatal("text provider does not implement ChatInterface")
			}
			if response, apiErr := chat.CreateChatCompletion(textRequest); apiErr != nil || response == nil {
				t.Fatalf("text request failed: response=%+v err=%+v", response, apiErr)
			}
			if len(bodies) != 1 {
				t.Fatalf("text upstream calls=%d, want 1", len(bodies))
			}
			var wireBody any
			if err := json.Unmarshal(bodies[0], &wireBody); err != nil || !issue033JSONContains(wireBody, "hello") {
				t.Fatalf("text was not represented in outgoing body: err=%v body=%s", err, bodies[0])
			}
		})
	}
}

func TestIssue033NativeBaiduAndInvalidMediaFailBeforeProviderWork(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	nativeBaidu := issue033FactoryCase{name: "Baidu native", channelType: config.ChannelTypeBaidu, model: "vision-model", key: "client|secret"}
	for name, testCase := range map[string]issue033FactoryCase{
		"native Baidu":               nativeBaidu,
		"Ali native text-only model": {name: "Ali native text-only", channelType: config.ChannelTypeAli, model: "qwen-plus", key: "sk-test"},
		"invalid scheme":             {name: "invalid", channelType: config.ChannelTypeCohere, model: "command-a", key: "sk-test"},
		"invalid data URI":           {name: "invalid-data", channelType: config.ChannelTypeCohere, model: "command-a", key: "sk-test"},
	} {
		t.Run(name, func(t *testing.T) {
			var imageURL string
			switch name {
			case "native Baidu":
				imageURL = "https://media.example/image.png"
			case "Ali native text-only model":
				imageURL = "https://media.example/image.png"
			case "invalid scheme":
				imageURL = "ftp://media.example/image.png"
			default:
				imageURL = "data:image/png;base64,not-valid"
			}
			request := issue033ChatRequest(testCase.model, imageURL)
			channel := issue033Channel(testCase, server.URL)
			mode, _, err := AssessChatRemoteMedia(channel, request)
			if name == "invalid data URI" {
				if err != nil || mode != base.RemoteMediaPassURL {
					t.Fatalf("invalid data URI should reach selected-provider preparation, mode=%v err=%v", mode, err)
				}
				provider := createProvider(channel)
				if err := PrepareChatRemoteMedia(provider, request, nil); err == nil {
					t.Fatal("invalid data URI was accepted by selected-provider preparation")
				}
			} else if err == nil || mode != base.RemoteMediaReject {
				t.Fatalf("expected pre-work rejection, mode=%v err=%v", mode, err)
			}
			if requests != 0 {
				t.Fatalf("rejected request reached local upstream %d times", requests)
			}
		})
	}
}

func TestIssue033InvalidChatMediaModeIsRejected(t *testing.T) {
	const channelType = 987651
	factory := &issue033InvalidMediaModeFactory{}
	previous, existed := providerFactories[channelType]
	providerFactories[channelType] = factory
	t.Cleanup(func() {
		if existed {
			providerFactories[channelType] = previous
		} else {
			delete(providerFactories, channelType)
		}
	})

	mode, _, err := AssessChatRemoteMedia(remoteMediaTestChannel(channelType), providerMediaRequest("https://media.example/image.png"))
	if err == nil || mode != base.RemoteMediaReject {
		t.Fatalf("invalid provider media mode was accepted: mode=%v err=%v", mode, err)
	}
}

type issue033InvalidMediaModeFactory struct{}

func (issue033InvalidMediaModeFactory) Create(*model.Channel) base.ProviderInterface { return nil }

func (issue033InvalidMediaModeFactory) AssessChatRemoteMedia(*model.Channel, *types.ChatCompletionRequest, base.ChatRemoteMediaSummary) (base.RemoteMediaMode, error) {
	return base.RemoteMediaMode(255), nil
}
