package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

type issue041Upstream struct {
	mu       sync.Mutex
	stream   bool
	protocol string
	kind     string
	calls    int
	bodies   [][]byte
	server   *httptest.Server
}

func newIssue041Upstream(t *testing.T, stream bool, kind string) *issue041Upstream {
	t.Helper()
	upstream := &issue041Upstream{stream: stream, protocol: kind, kind: kind}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		upstream.mu.Lock()
		upstream.calls++
		call := upstream.calls
		upstream.bodies = append(upstream.bodies, append([]byte(nil), body...))
		upstream.mu.Unlock()

		if upstream.protocol == "responses" {
			if upstream.stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, issue041ResponsesSSE(call, upstream.kind))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, issue041ResponsesJSON(call, upstream.kind))
			return
		}

		if upstream.stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, issue041ChatSSE(call, upstream.kind))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, issue041ChatJSON(call, upstream.kind))
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func issue041ChatJSON(call int, kind string) string {
	if call > 1 {
		return `{"id":"chat-final","object":"chat.completion","created":2,"model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"finished"},"finish_reason":"stop"}]}`
	}
	if kind == "function" {
		return `{"id":"chat-tool","object":"chat.completion","created":1,"model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_041","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`
	}
	return `{"id":"chat-first","object":"chat.completion","created":1,"model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"first answer"},"finish_reason":"stop"}]}`
}

func issue041ChatSSE(call int, kind string) string {
	if call > 1 {
		return "data: {\"id\":\"chat-final\",\"object\":\"chat.completion.chunk\",\"created\":2,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"finished\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"chat-final\",\"object\":\"chat.completion.chunk\",\"created\":2,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"
	}
	if kind == "function" {
		return "data: {\"id\":\"chat-tool\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"chat-tool\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_041\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"city\\\":\"}}]},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"chat-tool\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"Paris\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n"
	}
	return "data: {\"id\":\"chat-first\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"first answer\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chat-first\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
}

func issue041ResponsesJSON(call int, kind string) string {
	if call > 1 {
		return `{"id":"resp-final","object":"response","status":"completed","model":"gpt-5","output":[{"type":"message","id":"msg-final","status":"completed","role":"assistant","content":[{"type":"output_text","text":"finished"}]}]}`
	}
	if kind == "custom" {
		return `{"id":"resp-tool","object":"response","status":"completed","model":"gpt-5","output":[{"type":"custom_tool_call","id":"ctc-041","status":"completed","call_id":"call_041","name":"shell","input":"echo hi"}]}`
	}
	return `{"id":"resp-tool","object":"response","status":"completed","model":"gpt-5","output":[{"type":"function_call","id":"fc-041","status":"completed","call_id":"call_041","name":"lookup","arguments":"{\"city\":\"Paris\"}"}]}`
}

func issue041ResponsesSSE(call int, kind string) string {
	if call == 1 && kind == "multi-function" {
		return issue041ResponsesMultiToolSSE()
	}
	response := issue041ResponsesJSON(call, kind)
	return "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
}

func issue041ResponsesMultiToolSSE() string {
	return strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp-multi","model":"gpt-5","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc-041-weather","status":"in_progress","call_id":"call_041_weather","name":"lookup_weather","arguments":"{\"city\":\"Paris\"}"}}`,
		`data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc-041-weather","arguments":"{\"city\":\"Paris\"}"}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc-041-time","status":"in_progress","call_id":"call_041_time","name":"lookup_time","arguments":"{\"city\":\"Paris\",\"tz\":\"Europe/Paris\"}"}}`,
		`data: {"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc-041-time","arguments":"{\"city\":\"Paris\",\"tz\":\"Europe/Paris\"}"}`,
		`data: {"type":"response.completed","response":{"id":"resp-multi","object":"response","status":"completed","model":"gpt-5","output":[{"type":"function_call","id":"fc-041-weather","status":"completed","call_id":"call_041_weather","name":"lookup_weather","arguments":"{\"city\":\"Paris\"}"},{"type":"function_call","id":"fc-041-time","status":"completed","call_id":"call_041_time","name":"lookup_time","arguments":"{\"city\":\"Paris\",\"tz\":\"Europe/Paris\"}"}]}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
}

func issue041ConfigureChannelGroup(t *testing.T, channel *model.Channel, modelName string) {
	t.Helper()
	snapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(snapshot) })
	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{channel.Id: {Channel: channel}},
		Rule: map[string]map[string][][]int{
			"default": {modelName: {{channel.Id}}},
		},
		ModelGroup: map[string]map[string]bool{modelName: {"default": true}},
	}
}

func issue041Context(t *testing.T, path, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("token_group", "default")
	return ctx
}

func issue041CrossChannel(endpoint string) *model.Channel {
	proxy := ""
	plugin := datatypes.NewJSONType(model.PluginType{
		"endpoints": {"openai.chat_completions": map[string]any{"enabled": true, "upstream_url": ""}, "openai.responses": map[string]any{"enabled": false, "upstream_url": ""}},
	})
	return &model.Channel{
		Id:                 41041,
		Type:               config.ChannelTypeCustom,
		Key:                "sk-issue-041",
		Status:             config.ChannelStatusEnabled,
		Group:              "default",
		Models:             "gpt-5",
		Weight:             func() *uint { value := uint(1); return &value }(),
		Proxy:              &proxy,
		BaseURL:            &endpoint,
		CompatibleResponse: true,
		Plugin:             &plugin,
	}
}

func issue041ResponsesOutput(t *testing.T, body []byte, stream bool) json.RawMessage {
	t.Helper()
	if !stream {
		var response struct {
			Output json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatalf("decode unary Responses response: %v; body=%s", err, body)
		}
		return append(json.RawMessage(nil), response.Output...)
	}
	for _, event := range bytes.Split(body, []byte("\n\n")) {
		if !bytes.Contains(event, []byte("event: response.completed")) {
			continue
		}
		for _, line := range bytes.Split(event, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			var payload struct {
				Response struct {
					Output json.RawMessage `json:"output"`
				} `json:"response"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &payload); err != nil {
				t.Fatalf("decode Responses completion event: %v; event=%s", err, event)
			}
			return append(json.RawMessage(nil), payload.Response.Output...)
		}
	}
	t.Fatalf("Responses stream omitted response.completed: %s", body)
	return nil
}

func issue041ResponsesHistoryBody(t *testing.T, output json.RawMessage, stream, withToolResult bool) []byte {
	t.Helper()
	var items []json.RawMessage
	if err := json.Unmarshal(output, &items); err != nil || len(items) == 0 {
		t.Fatalf("first Responses output is not a non-empty array: %v; output=%s", err, output)
	}
	var body bytes.Buffer
	body.WriteString(`{"model":"gpt-5","input":[`)
	for index, item := range items {
		if index > 0 {
			body.WriteByte(',')
		}
		body.Write(item)
	}
	if withToolResult {
		body.WriteString(`,{"type":"function_call_output","call_id":"call_041","output":"tool result"}`)
	}
	body.WriteString(`],"store":false`)
	if stream {
		body.WriteString(`,"stream":true`)
	}
	body.WriteByte('}')
	return body.Bytes()
}

func runIssue041ResponsesHistory(t *testing.T, stream, functionOutput bool) {
	t.Helper()
	upstream := newIssue041Upstream(t, stream, "text")
	previousClient := requester.HTTPClient
	requester.HTTPClient = upstream.server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	channel := issue041CrossChannel(upstream.server.URL)
	issue041ConfigureChannelGroup(t, channel, "gpt-5")
	firstBody := `{"model":"gpt-5","input":"hello","store":false}`
	if stream {
		firstBody = `{"model":"gpt-5","input":"hello","store":false,"stream":true}`
	}
	if functionOutput {
		upstream.kind = "function"
	}
	firstRecorder := httptest.NewRecorder()
	firstRelay := NewRelayResponses(issue041ContextWithRecorder(t, "/v1/responses", firstBody, firstRecorder))
	if err := firstRelay.setRequest(); err != nil {
		t.Fatalf("first Responses setRequest: %v", err)
	}
	if err := firstRelay.setProvider(firstRelay.getOriginalModel()); err != nil {
		t.Fatalf("first Responses setProvider: %v", err)
	}
	firstRelay.provider.SetUsage(&types.Usage{})
	if err := finalizeSelectedProviderRequest(firstRelay); err != nil {
		t.Fatalf("first Responses finalize: %v", err)
	}
	if apiErr, _ := firstRelay.send(); apiErr != nil {
		t.Fatalf("first Responses send: %+v; body=%s", apiErr, firstRecorder.Body.Bytes())
	}
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first Responses status=%d body=%s", firstRecorder.Code, firstRecorder.Body.Bytes())
	}
	output := issue041ResponsesOutput(t, firstRecorder.Body.Bytes(), stream)
	if !bytes.Contains(output, []byte(`"id"`)) || !bytes.Contains(output, []byte(`"status"`)) {
		t.Fatalf("first public output lacks standard metadata: %s", output)
	}

	secondBody := issue041ResponsesHistoryBody(t, output, stream, functionOutput)
	secondRecorder := httptest.NewRecorder()
	secondRelay := NewRelayResponses(issue041ContextWithRecorder(t, "/v1/responses", string(secondBody), secondRecorder))
	if err := secondRelay.setRequest(); err != nil {
		t.Fatalf("second Responses setRequest rejected own output history: %v; body=%s", err, secondBody)
	}
	if err := secondRelay.setProvider(secondRelay.getOriginalModel()); err != nil {
		t.Fatalf("second Responses setProvider: %v", err)
	}
	secondRelay.provider.SetUsage(&types.Usage{})
	if err := finalizeSelectedProviderRequest(secondRelay); err != nil {
		t.Fatalf("second Responses finalize rejected own output history: %v", err)
	}
	if apiErr, _ := secondRelay.send(); apiErr != nil {
		t.Fatalf("second Responses send: %+v; body=%s", apiErr, secondRecorder.Body.Bytes())
	}
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second Responses status=%d body=%s", secondRecorder.Code, secondRecorder.Body.Bytes())
	}
	upstream.mu.Lock()
	calls := upstream.calls
	bodies := append([][]byte(nil), upstream.bodies...)
	upstream.mu.Unlock()
	if calls != 2 || len(bodies) != 2 {
		t.Fatalf("expected exactly two upstream calls, calls=%d bodies=%d", calls, len(bodies))
	}
	if functionOutput {
		for _, needle := range []string{"call_041", "lookup", "Paris", "tool result"} {
			if !bytes.Contains(bodies[1], []byte(needle)) {
				t.Fatalf("second converted Chat history lost %q: %s", needle, bodies[1])
			}
		}
	} else if !bytes.Contains(bodies[1], []byte("first answer")) {
		t.Fatalf("second converted Chat history lost first response text: %s", bodies[1])
	}
}

func issue041ContextWithRecorder(t *testing.T, path, body string, recorder *httptest.ResponseRecorder) *gin.Context {
	t.Helper()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("token_group", "default")
	return ctx
}

func TestIssue041ResponsesOutputHistoryIsAcceptedForUnaryAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, functionOutput := range []bool{false, true} {
			name := fmt.Sprintf("%s/%s", map[bool]string{false: "unary", true: "sse"}[stream], map[bool]string{false: "text", true: "function"}[functionOutput])
			t.Run(name, func(t *testing.T) {
				runIssue041ResponsesHistory(t, stream, functionOutput)
			})
		}
	}
}

type issue041ChatFactoryCase struct {
	name        string
	channelType int
	other       string
	key         string
}

func issue041ChatFactoryCases() []issue041ChatFactoryCase {
	return []issue041ChatFactoryCase{
		{name: "OpenAI", channelType: config.ChannelTypeOpenAI, key: "sk-openai"},
		{name: "Azure", channelType: config.ChannelTypeAzure, key: "sk-azure", other: `{"api_version":"2024-10-01-preview"}`},
		{name: "AzureV1", channelType: config.ChannelTypeAzureV1, key: "sk-azure-v1"},
		{name: "Custom", channelType: config.ChannelTypeCustom, key: "sk-custom"},
		{name: "OpenAI-compatible fallback", channelType: 987654, key: "sk-fallback"},
		{name: "Codex", channelType: config.ChannelTypeCodex, key: "access-token"},
	}
}

func issue041ChatChannel(testCase issue041ChatFactoryCase, endpoint string) *model.Channel {
	proxy := ""
	return &model.Channel{
		Id:     42041 + testCase.channelType,
		Plugin: model.NewCustomEndpointPlugin(), Type: testCase.channelType,
		Key:     testCase.key,
		Status:  config.ChannelStatusEnabled,
		Group:   "default",
		Models:  "o3-pro",
		Weight:  func() *uint { value := uint(1); return &value }(),
		Proxy:   &proxy,
		BaseURL: &endpoint,
		Other:   testCase.other,
	}
}

func issue041ChatHistoryBody(t *testing.T, message json.RawMessage, custom bool) []byte {
	t.Helper()
	toolResult := `{"role":"tool","tool_call_id":"call_041","content":"tool result"}`
	if custom {
		toolResult = `{"role":"tool","tool_call_id":"call_041","content":"custom result"}`
	}
	return []byte(fmt.Sprintf(`{"model":"o3-pro","messages":[%s,%s]}`, message, toolResult))
}

func issue041ChatMessage(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var response struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &response); err != nil || len(response.Choices) != 1 || len(response.Choices[0].Message) == 0 {
		t.Fatalf("decode first Chat response: %v; body=%s", err, body)
	}
	return append(json.RawMessage(nil), response.Choices[0].Message...)
}

func issue041ChatStreamMessage(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	calls := make([]*types.ChatCompletionToolCalls, 0, 2)
	for _, event := range bytes.Split(body, []byte("\n\n")) {
		for _, line := range bytes.Split(event, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			var chunk types.ChatCompletionStreamResponse
			if err := json.Unmarshal(payload, &chunk); err != nil {
				t.Fatalf("decode first Chat stream chunk: %v; body=%s", err, body)
			}
			for _, choice := range chunk.Choices {
				for _, delta := range choice.Delta.ToolCalls {
					if delta == nil {
						continue
					}
					var current *types.ChatCompletionToolCalls
					for _, existing := range calls {
						if existing.Index == delta.Index {
							current = existing
							break
						}
					}
					if current == nil {
						current = &types.ChatCompletionToolCalls{Index: delta.Index}
						calls = append(calls, current)
					}
					if delta.Id != "" {
						current.Id = delta.Id
					}
					if delta.Type != "" {
						current.Type = delta.Type
					}
					if delta.Function != nil {
						if current.Function == nil {
							current.Function = &types.ChatCompletionToolCallsFunction{}
						}
						if delta.Function.Name != "" {
							current.Function.Name = delta.Function.Name
						}
						current.Function.Arguments += delta.Function.Arguments
					}
					if delta.Custom != nil {
						if current.Custom == nil {
							current.Custom = &types.ChatCompletionToolCallsCustom{}
						}
						if delta.Custom.Name != "" {
							current.Custom.Name = delta.Custom.Name
						}
						current.Custom.Input += delta.Custom.Input
					}
				}
			}
		}
	}
	message := types.ChatCompletionMessage{Role: types.ChatMessageRoleAssistant, ToolCalls: calls}
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("encode aggregated Chat stream message: %v", err)
	}
	return encoded
}

func runIssue041ChatHistory(t *testing.T, testCase issue041ChatFactoryCase, custom bool) {
	t.Helper()
	kind := "function"
	if custom {
		kind = "custom"
	}
	upstream := newIssue041Upstream(t, testCase.channelType == config.ChannelTypeCodex, "responses")
	upstream.kind = kind
	previousClient := requester.HTTPClient
	requester.HTTPClient = upstream.server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	channel := issue041ChatChannel(testCase, upstream.server.URL)
	issue041ConfigureChannelGroup(t, channel, "o3-pro")
	firstBody := `{"model":"o3-pro","messages":[{"role":"user","content":"call a tool"}]}`
	firstRecorder := httptest.NewRecorder()
	firstRelay := NewRelayChat(issue041ContextWithRecorder(t, "/v1/chat/completions", firstBody, firstRecorder))
	if err := firstRelay.setRequest(); err != nil {
		t.Fatalf("%s first Chat setRequest: %v", testCase.name, err)
	}
	if err := firstRelay.setProvider(firstRelay.getOriginalModel()); err != nil {
		t.Fatalf("%s first Chat setProvider: %v", testCase.name, err)
	}
	firstRelay.provider.SetUsage(&types.Usage{})
	if err := finalizeSelectedProviderRequest(firstRelay); err != nil {
		t.Fatalf("%s first Chat finalize: %v", testCase.name, err)
	}
	if apiErr, _ := firstRelay.send(); apiErr != nil {
		t.Fatalf("%s first Chat send: %+v; body=%s", testCase.name, apiErr, firstRecorder.Body.Bytes())
	}
	message := issue041ChatMessage(t, firstRecorder.Body.Bytes())
	if !bytes.Contains(message, []byte(`"index":0`)) {
		t.Fatalf("%s first public tool message omitted index: %s", testCase.name, message)
	}
	secondBody := issue041ChatHistoryBody(t, message, custom)
	secondRecorder := httptest.NewRecorder()
	secondRelay := NewRelayChat(issue041ContextWithRecorder(t, "/v1/chat/completions", string(secondBody), secondRecorder))
	if err := secondRelay.setRequest(); err != nil {
		t.Fatalf("%s second Chat setRequest rejected own tool history: %v; body=%s", testCase.name, err, secondBody)
	}
	if err := secondRelay.setProvider(secondRelay.getOriginalModel()); err != nil {
		t.Fatalf("%s second Chat setProvider: %v", testCase.name, err)
	}
	secondRelay.provider.SetUsage(&types.Usage{})
	if err := finalizeSelectedProviderRequest(secondRelay); err != nil {
		t.Fatalf("%s second Chat finalize rejected own tool history: %v", testCase.name, err)
	}
	if apiErr, _ := secondRelay.send(); apiErr != nil {
		t.Fatalf("%s second Chat send: %+v; body=%s", testCase.name, apiErr, secondRecorder.Body.Bytes())
	}
	if !bytes.Contains(secondRecorder.Body.Bytes(), []byte("finished")) {
		t.Fatalf("%s second Chat response did not finish: %s", testCase.name, secondRecorder.Body.Bytes())
	}
	upstream.mu.Lock()
	calls := upstream.calls
	bodies := append([][]byte(nil), upstream.bodies...)
	upstream.mu.Unlock()
	if calls != 2 || len(bodies) != 2 {
		t.Fatalf("%s upstream calls=%d bodies=%d, want 2", testCase.name, calls, len(bodies))
	}
	result := "tool result"
	needles := []string{"call_041", result}
	if custom {
		result = "custom result"
		needles = []string{"call_041", result, "custom_tool_call", "shell", "echo hi"}
	} else {
		needles = append(needles, "function_call", "lookup", "Paris")
	}
	for _, needle := range needles {
		if !bytes.Contains(bodies[1], []byte(needle)) {
			t.Fatalf("%s second Responses body lost %q: %s", testCase.name, needle, bodies[1])
		}
	}
}

func TestIssue041ChatStreamHistoryPreservesTwoToolIndexes(t *testing.T) {
	upstream := newIssue041Upstream(t, true, "responses")
	upstream.kind = "multi-function"
	previousClient := requester.HTTPClient
	requester.HTTPClient = upstream.server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	channel := issue041ChatChannel(issue041ChatFactoryCase{name: "OpenAI", channelType: config.ChannelTypeOpenAI, key: "sk-openai"}, upstream.server.URL)
	issue041ConfigureChannelGroup(t, channel, "o3-pro")
	firstBody := `{"model":"o3-pro","messages":[{"role":"user","content":"call two tools"}],"stream":true}`
	firstRecorder := httptest.NewRecorder()
	firstRelay := NewRelayChat(issue041ContextWithRecorder(t, "/v1/chat/completions", firstBody, firstRecorder))
	if err := firstRelay.setRequest(); err != nil {
		t.Fatalf("first Chat setRequest: %v", err)
	}
	if err := firstRelay.setProvider(firstRelay.getOriginalModel()); err != nil {
		t.Fatalf("first Chat setProvider: %v", err)
	}
	firstRelay.provider.SetUsage(&types.Usage{})
	if apiErr, _ := firstRelay.send(); apiErr != nil {
		t.Fatalf("first Chat send: %+v; body=%s", apiErr, firstRecorder.Body.Bytes())
	}
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first Chat status=%d body=%s", firstRecorder.Code, firstRecorder.Body.Bytes())
	}
	message := issue041ChatStreamMessage(t, firstRecorder.Body.Bytes())
	var publicMessage types.ChatCompletionMessage
	if err := json.Unmarshal(message, &publicMessage); err != nil {
		t.Fatalf("decode aggregated public Chat message: %v; message=%s", err, message)
	}
	if len(publicMessage.ToolCalls) != 2 {
		t.Fatalf("first public Chat message has %d tool calls, want 2: %s", len(publicMessage.ToolCalls), message)
	}
	wantCalls := []struct {
		index int
		id    string
		name  string
		args  string
	}{
		{index: 0, id: "call_041_weather", name: "lookup_weather", args: `{"city":"Paris"}`},
		{index: 1, id: "call_041_time", name: "lookup_time", args: `{"city":"Paris","tz":"Europe/Paris"}`},
	}
	for index, want := range wantCalls {
		got := publicMessage.ToolCalls[index]
		if got == nil || got.Index != want.index || got.Id != want.id || got.Function == nil || got.Function.Name != want.name || got.Function.Arguments != want.args {
			t.Fatalf("first public Chat tool call %d changed: got=%+v want=%+v; message=%s", index, got, want, message)
		}
	}

	secondBody := []byte(fmt.Sprintf(`{"model":"o3-pro","messages":[%s,{"role":"tool","tool_call_id":"call_041_weather","content":"weather result"},{"role":"tool","tool_call_id":"call_041_time","content":"time result"}],"stream":true}`, message))
	secondRecorder := httptest.NewRecorder()
	secondRelay := NewRelayChat(issue041ContextWithRecorder(t, "/v1/chat/completions", string(secondBody), secondRecorder))
	if err := secondRelay.setRequest(); err != nil {
		t.Fatalf("second Chat setRequest rejected two-tool history: %v; body=%s", err, secondBody)
	}
	if err := secondRelay.setProvider(secondRelay.getOriginalModel()); err != nil {
		t.Fatalf("second Chat setProvider: %v", err)
	}
	secondRelay.provider.SetUsage(&types.Usage{})
	if apiErr, _ := secondRelay.send(); apiErr != nil {
		t.Fatalf("second Chat send: %+v; body=%s", apiErr, secondRecorder.Body.Bytes())
	}
	if secondRecorder.Code != http.StatusOK || !bytes.Contains(secondRecorder.Body.Bytes(), []byte(`"finish_reason":"stop"`)) {
		t.Fatalf("second Chat did not finish: status=%d body=%s", secondRecorder.Code, secondRecorder.Body.Bytes())
	}
	upstream.mu.Lock()
	calls := upstream.calls
	bodies := append([][]byte(nil), upstream.bodies...)
	upstream.mu.Unlock()
	if calls != 2 || len(bodies) != 2 {
		t.Fatalf("two-tool history upstream calls=%d bodies=%d, want 2", calls, len(bodies))
	}
	for _, needle := range []string{
		"call_041_weather", "lookup_weather", `{\"city\":\"Paris\"}`, "weather result",
		"call_041_time", "lookup_time", `{\"city\":\"Paris\",\"tz\":\"Europe/Paris\"}`, "time result",
	} {
		if !bytes.Contains(bodies[1], []byte(needle)) {
			t.Fatalf("second Responses body lost %q: %s", needle, bodies[1])
		}
	}
}

func TestIssue041ChatToolHistoryKeepsIndexAcrossResponsesFactories(t *testing.T) {
	for _, testCase := range issue041ChatFactoryCases() {
		for _, custom := range []bool{false, true} {
			name := testCase.name + "/" + map[bool]string{false: "function", true: "custom"}[custom]
			t.Run(name, func(t *testing.T) {
				runIssue041ChatHistory(t, testCase, custom)
			})
		}
	}
}

func TestIssue041RejectsUnknownHistoryMetadataAndUnrepresentableSemantics(t *testing.T) {
	storeFalse := false
	unknown := types.OpenAIResponsesRequest{
		Store: &storeFalse,
		Input: []any{map[string]any{
			"type":            types.InputTypeMessage,
			"id":              "msg-known",
			"status":          "completed",
			"role":            "assistant",
			"content":         "ok",
			"future_metadata": true,
		}},
	}
	raw := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","id":"msg-known","status":"completed","role":"assistant","content":"ok","future_metadata":true}]`),
	}
	assertCapabilityGateError(t, validateResponsesToChatRepresentability(&unknown, raw), "input")

	refusal := types.OpenAIResponsesRequest{
		Store: &storeFalse,
		Input: []any{map[string]any{
			"type":   types.InputTypeMessage,
			"id":     "msg-refusal",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":    types.ContentTypeRefusal,
				"refusal": "no",
			}},
		}},
	}
	refusalRaw := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","id":"msg-refusal","status":"completed","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]`),
	}
	assertCapabilityGateError(t, validateResponsesToChatRepresentability(&refusal, refusalRaw), "input")

	index := -1
	chat := &types.ChatCompletionRequest{Messages: []types.ChatCompletionMessage{{
		Role: types.ChatMessageRoleAssistant,
		ToolCalls: []*types.ChatCompletionToolCalls{{
			Id: "call-1", Type: types.ToolChoiceTypeFunction, Index: index,
			Function: &types.ChatCompletionToolCallsFunction{Name: "lookup", Arguments: `{"q":"x"}`},
		}},
	}}}
	chatRaw := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","index":-1,"function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}]`),
	}
	assertCapabilityGateError(t, validateChatToResponsesRepresentability(chat, chatRaw), "messages")
}

func TestIssue041CandidateAndFinalResponsesHistoryUseSameGate(t *testing.T) {
	request := `{"model":"gpt-5","input":[{"type":"message","id":"msg-1","status":"provider_future_status","role":"assistant","content":"hello"}],"store":false}`
	channel := issue041CrossChannel("https://compatible.example")
	issue041ConfigureChannelGroup(t, channel, "gpt-5")
	ctx := issue041Context(t, "/v1/responses", request)
	relay := NewRelayResponses(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("candidate gate rejected own response metadata: %v", err)
	}
	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		t.Fatalf("select candidate provider: %v", err)
	}
	if err := finalizeSelectedProviderRequest(relay); err != nil {
		t.Fatalf("final gate rejected own response metadata: %v", err)
	}
	if relay.preparedChatRequest == nil || len(relay.preparedChatRequest.Messages) != 1 || len(relay.preparedChatRequest.Messages[0].ParseContent()) != 1 || relay.preparedChatRequest.Messages[0].ParseContent()[0].Text != "hello" {
		t.Fatalf("history metadata did not convert to one Chat message: %#v", relay.preparedChatRequest)
	}
}
