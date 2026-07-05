package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	providers_base "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestResponsesChannelProbeCarriesPurpose(t *testing.T) {
	originalGetProvider := getProviderFunc
	t.Cleanup(func() {
		getProviderFunc = originalGetProvider
	})

	fake := &recordingResponsesProbeProvider{}
	getProviderFunc = func(channel *model.Channel, c *gin.Context) providers_base.ProviderInterface {
		fake.channel = channel
		fake.context = c
		return fake
	}

	proxy := ""
	openaiErr, err := testChannel(&model.Channel{
		Id:        77,
		Type:      config.ChannelTypeOpenAI,
		Key:       "sk-test",
		Proxy:     &proxy,
		TestModel: "o3",
	}, "o3")

	if openaiErr != nil || err != nil {
		t.Fatalf("expected probe to succeed, openaiErr=%+v err=%v", openaiErr, err)
	}
	if fake.req == nil {
		t.Fatal("expected fake provider to receive Responses request")
	}
	if fake.req.Control.Purpose != commonresponses.RequestPurposeChannelProbe {
		t.Fatalf("expected channel probe purpose, got %+v", fake.req.Control)
	}
	if fake.contextPurpose != commonresponses.RequestPurposeChannelProbe {
		t.Fatalf("expected context channel probe purpose, got %q", fake.contextPurpose)
	}
	body := requestBodyMapFromEnvelope(t, fake.req)
	if _, exists := body["client_metadata"]; exists {
		t.Fatalf("expected controller probe body not to contain Codex metadata, got %#v", body["client_metadata"])
	}
	for _, header := range []string{"originator", "session-id", "thread-id", "x-codex-window-id", "x-codex-installation-id", "x-codex-turn-metadata"} {
		if fake.req.Headers.HasNonEmpty(header) {
			t.Fatalf("expected controller not to set %s in control request headers", header)
		}
	}
}

func TestResponsesChannelProbeRequestUsesOrdinaryResponsesShape(t *testing.T) {
	originalHTTPClient := requester.HTTPClient
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	requestBody := make(chan map[string]any, 1)
	requestHeaders := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Errorf("decode request body %s: %v", string(bodyBytes), err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requestBody <- body
		requestHeaders <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_test",
			"object":"response",
			"created_at":0,
			"status":"completed",
			"model":"o3",
			"output":[{"type":"message","id":"msg_test","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],
			"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"input_tokens_details":{"cached_tokens":0}}
		}`))
	}))
	defer server.Close()

	requester.HTTPClient = server.Client()

	baseURL := server.URL
	proxy := ""
	openaiErr, err := testChannel(&model.Channel{
		Id:        78,
		Type:      config.ChannelTypeOpenAI,
		Key:       "sk-test",
		BaseURL:   &baseURL,
		Proxy:     &proxy,
		TestModel: "o3",
	}, "o3")
	if openaiErr != nil || err != nil {
		t.Fatalf("expected probe to succeed, openaiErr=%+v err=%v", openaiErr, err)
	}

	var body map[string]any
	select {
	case body = <-requestBody:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected fake upstream request body")
	}
	headers := <-requestHeaders

	if _, exists := body["client_metadata"]; exists {
		t.Fatalf("expected controller probe body not to contain Codex metadata, got %#v", body["client_metadata"])
	}
	inputItems, ok := body["input"].([]any)
	if !ok || len(inputItems) != 1 {
		t.Fatalf("expected Responses input message array, got %#v", body["input"])
	}
	message := requireControllerProbeMap(t, inputItems[0])
	if message["type"] != "message" || message["role"] != "user" {
		t.Fatalf("expected user message item, got %#v", message)
	}
	contentItems, ok := message["content"].([]any)
	if !ok || len(contentItems) != 1 {
		t.Fatalf("expected message content array, got %#v", message["content"])
	}
	content := requireControllerProbeMap(t, contentItems[0])
	if content["type"] != "input_text" || content["text"] != "You just need to output 'hi' next." {
		t.Fatalf("expected input_text content, got %#v", content)
	}
	for _, header := range []string{"originator", "session-id", "thread-id", "x-codex-window-id", "x-codex-installation-id", "x-codex-turn-metadata"} {
		if got := headers.Get(header); got != "" {
			t.Fatalf("expected controller not to set %s, got %q", header, got)
		}
	}
}

func requireControllerProbeMap(t *testing.T, value any) map[string]any {
	t.Helper()
	out, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %#v", value)
	}
	return out
}

func requestBodyMapFromEnvelope(t *testing.T, req *commonresponses.Request) map[string]any {
	t.Helper()
	if req == nil || req.Body == nil || req.Body.Object == nil {
		t.Fatal("expected Responses request body object")
	}
	raw, err := req.Body.Object.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request body %s: %v", string(raw), err)
	}
	return body
}

type recordingResponsesProbeProvider struct {
	channel        *model.Channel
	context        *gin.Context
	usage          *types.Usage
	req            *commonresponses.Request
	contextPurpose commonresponses.RequestPurpose
}

func (p *recordingResponsesProbeProvider) GetRequestHeaders() map[string]string { return nil }
func (p *recordingResponsesProbeProvider) GetUsage() *types.Usage               { return p.usage }
func (p *recordingResponsesProbeProvider) SetUsage(usage *types.Usage)          { p.usage = usage }
func (p *recordingResponsesProbeProvider) SetContext(c *gin.Context)            { p.context = c }
func (p *recordingResponsesProbeProvider) SetOriginalModel(string)              {}
func (p *recordingResponsesProbeProvider) GetOriginalModel() string             { return "" }
func (p *recordingResponsesProbeProvider) GetChannel() *model.Channel           { return p.channel }
func (p *recordingResponsesProbeProvider) ModelMappingHandler(modelName string) (string, error) {
	return modelName, nil
}
func (p *recordingResponsesProbeProvider) GetRequester() *requester.HTTPRequester { return nil }
func (p *recordingResponsesProbeProvider) CustomParameterHandler() (map[string]interface{}, error) {
	return nil, nil
}
func (p *recordingResponsesProbeProvider) GetSupportedResponse() bool { return true }
func (p *recordingResponsesProbeProvider) CreateResponses(ctx context.Context, req *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	p.req = req
	p.contextPurpose = commonresponses.RequestPurposeFromContext(ctx)
	return &types.OpenAIResponsesResponses{
		ID:     "resp_test",
		Object: "response",
		Status: "completed",
		Model:  "o3",
		Output: []types.ResponsesOutput{
			{
				Type:   "message",
				Role:   "assistant",
				Status: "completed",
				Content: []types.ContentResponses{
					{Type: "output_text", Text: "hi"},
				},
			},
		},
		Usage: &types.ResponsesUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	}, nil
}
func (p *recordingResponsesProbeProvider) CreateResponsesStream(context.Context, *commonresponses.Request) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}
func (p *recordingResponsesProbeProvider) CompactResponses(context.Context, *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}
