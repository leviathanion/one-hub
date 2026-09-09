package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func newNativeClaudeProviderForTest(t *testing.T, server *httptest.Server, raw string, headers http.Header) (*ClaudeProvider, *ClaudeRequest) {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/claude/v1/messages", strings.NewReader(raw))
	ctx.Request.Header.Set("Content-Type", "application/json")
	for name, values := range headers {
		ctx.Request.Header[name] = append([]string(nil), values...)
	}
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatalf("cache Claude request: %v", err)
	}
	request := &ClaudeRequest{}
	if err := common.UnmarshalBodyReusable(ctx, request); err != nil {
		t.Fatalf("decode Claude projection: %v", err)
	}
	proxy := ""
	provider := CreateClaudeProvider(&model.Channel{Type: config.ChannelTypeAnthropic, Key: "provider-key", Proxy: &proxy}, server.URL)
	provider.SetContext(ctx)
	provider.SetOriginalModel(request.Model)
	provider.SetUsage(&types.Usage{})
	return provider, request
}

func TestNativeClaudePreservesBetaRequestAndUnaryResponseWire(t *testing.T) {
	responseWire := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":"refusal","stop_details":{"reason":"policy"},"usage":{"input_tokens":1,"output_tokens":1},"future":{"kept":true}}`)
	var gotBody []byte
	var gotBeta []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotBeta = append([]string(nil), r.Header.Values("anthropic-beta")...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseWire)
	}))
	t.Cleanup(server.Close)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	raw := `{"model":"claude-test","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"mcp_toolset","mcp_server_name":"server_1","default_config":{"enabled":true},"configs":{"tool":{"enabled":true}},"future":{"kept":true}}],"output_config":{"effort":"high","format":{"type":"json_schema"}},"future_request":{"kept":true}}`
	provider, request := newNativeClaudeProviderForTest(t, server, raw, http.Header{"Anthropic-Beta": {"mcp-client-2025-04-04", "structured-outputs-2025-11-13"}})
	response, apiErr := provider.CreateClaudeChat(request)
	if apiErr != nil {
		t.Fatalf("native Claude call failed: %+v", apiErr)
	}
	if !bytes.Equal(gotBody, []byte(raw)) {
		t.Fatalf("native Claude request was reconstructed:\nwant %s\n got %s", raw, gotBody)
	}
	if len(gotBeta) != 2 || gotBeta[0] != "mcp-client-2025-04-04" || gotBeta[1] != "structured-outputs-2025-11-13" {
		t.Fatalf("anthropic-beta values changed: %v", gotBeta)
	}
	if got := response.ReplayProviderRawJSON(); !bytes.Equal(got, responseWire) {
		t.Fatalf("native Claude response was reconstructed:\nwant %s\n got %s", responseWire, got)
	}
}

func TestNativeClaudeFutureResponseUnionKeepsRawAndUsage(t *testing.T) {
	raw := []byte(`{ "id":"msg_future", "type":"message", "model":"claude-test", "content":{"future":true}, "usage":{"input_tokens":3,"output_tokens":2}, "future":1e3 }`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	provider, request := newNativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	response, apiErr := provider.CreateClaudeChat(request)
	if apiErr != nil || response == nil || !bytes.Equal(response.ReplayProviderRawJSON(), raw) {
		t.Fatalf("future union blocked native raw delivery: response=%+v err=%+v", response, apiErr)
	}
	if usage := provider.GetUsage(); usage.PromptTokens != 3 || usage.CompletionTokens != 2 || !usage.ProviderReported {
		t.Fatalf("independent provider usage was lost: %+v", usage)
	}
}

func TestNativeClaudeMalformedUsageKeepsContentAsNonBillingObservation(t *testing.T) {
	previousDisabled := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() { config.DisableTokenEncoders = previousDisabled })
	raw := []byte(`{"id":"msg_partial","type":"message","model":"claude-test","content":[{"type":"text","text":"This content remains available for diagnostic token counts."}],"usage":{"input_tokens":{"future":true},"output_tokens":9}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	provider, request := newNativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":64,"messages":[]}`, nil)
	response, apiErr := provider.CreateClaudeChat(request)
	if apiErr != nil || response == nil || !bytes.Equal(response.ReplayProviderRawJSON(), raw) || len(response.Content) != 1 {
		t.Fatalf("malformed optional usage lost native content: response=%+v err=%+v", response, apiErr)
	}
	if usage := provider.GetUsage(); usage.ProviderReported || usage.CompletionTokens == 0 {
		t.Fatalf("diagnostic content must not authorize billing: %+v", usage)
	}
}

func TestNativeClaudePassesRemoteMediaRawWithoutFetching(t *testing.T) {
	var mediaGets atomic.Int32
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mediaGets.Add(1)
		_, _ = w.Write([]byte("must not fetch"))
	}))
	t.Cleanup(media.Close)

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_media","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	raw := `{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"` + media.URL + `/image.png"}},{"type":"tool_result","tool_use_id":"tool_1","content":[{"type":"document","source":{"type":"url","url":"` + media.URL + `/document.pdf"}}]}]}],"future_request":{"kept":true}}`
	provider, request := newNativeClaudeProviderForTest(t, upstream, raw, nil)
	if _, apiErr := provider.CreateClaudeChat(request); apiErr != nil {
		t.Fatalf("native Claude call failed: %+v", apiErr)
	}
	if mediaGets.Load() != 0 {
		t.Fatalf("direct Claude path fetched remote media %d times", mediaGets.Load())
	}
	if !bytes.Equal(gotBody, []byte(raw)) {
		t.Fatalf("direct Claude media request was reconstructed:\nwant %s\n got %s", raw, gotBody)
	}
}

func TestNativeClaudeModelMappingOnlyPatchesModelInRawMediaRequest(t *testing.T) {
	var mediaGets atomic.Int32
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mediaGets.Add(1)
		_, _ = w.Write([]byte("must not fetch"))
	}))
	t.Cleanup(media.Close)

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_mapped","type":"message","role":"assistant","model":"claude-mapped","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	raw := `{"model":"public-claude","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"` + media.URL + `/image.png","future_source":true}},{"type":"tool_result","tool_use_id":"tool_1","content":[{"type":"document","source":{"type":"url","url":"` + media.URL + `/document.pdf"}}]}]}],"future_integer":900719925474099312345,"future_request":{"kept":true}}`
	provider, request := newNativeClaudeProviderForTest(t, upstream, raw, nil)
	request.Model = "claude-mapped"
	if provider.GetOriginalModel() != "public-claude" || request.Model != "claude-mapped" {
		t.Fatalf("invalid mapping test setup: original=%q request=%q", provider.GetOriginalModel(), request.Model)
	}
	if _, apiErr := provider.CreateClaudeChat(request); apiErr != nil {
		t.Fatalf("mapped native Claude call failed: %+v", apiErr)
	}
	if mediaGets.Load() != 0 {
		t.Fatalf("mapped direct Claude path fetched remote media %d times", mediaGets.Load())
	}
	decoder := json.NewDecoder(bytes.NewReader(gotBody))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode mapped request: %v", err)
	}
	if body["model"] != "claude-mapped" || body["future_integer"].(json.Number).String() != "900719925474099312345" {
		t.Fatalf("mapped request changed owned/unknown fields: %#v", body)
	}
	messages := body["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	imageSource := content[0].(map[string]any)["source"].(map[string]any)
	toolContent := content[1].(map[string]any)["content"].([]any)
	documentSource := toolContent[0].(map[string]any)["source"].(map[string]any)
	if imageSource["type"] != "url" || imageSource["future_source"] != true || documentSource["type"] != "url" {
		t.Fatalf("mapped request changed remote media wire: image=%#v document=%#v", imageSource, documentSource)
	}
}

func TestNativeClaudeHTTPErrorPreservesWireAndRetryHeader(t *testing.T) {
	errorWire := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down","future":{"kept":true}},"request_id":"req_1"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("X-Request-Id", "req_1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(errorWire)
	}))
	t.Cleanup(server.Close)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })
	provider, request := newNativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":1,"messages":[]}`, nil)
	_, apiErr := provider.CreateClaudeChat(request)
	if apiErr == nil || !apiErr.ReplayRawResponse || apiErr.StatusCode != http.StatusTooManyRequests || !bytes.Equal(apiErr.RawBody, errorWire) {
		t.Fatalf("native Claude error wire changed: %+v", apiErr)
	}
	if apiErr.ResponseHeaders.Get("Retry-After") != "7" || apiErr.ResponseHeaders.Get("X-Request-Id") != "req_1" {
		t.Fatalf("native Claude retry/diagnostic headers lost: %v", apiErr.ResponseHeaders)
	}
}

func TestNativeClaudeStreamRequiresMessageStopAndPreservesErrorEvent(t *testing.T) {
	for _, test := range []struct {
		name      string
		wire      string
		wantError func(error) bool
	}{
		{name: "missing message_stop", wire: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n", wantError: func(err error) bool { return errors.Is(err, requester.ErrStreamProtocolTerminalMissing) }},
		{name: "provider error", wire: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"failed\"}}\n\n", wantError: func(err error) bool { return err != nil && !errors.Is(err, io.EOF) }},
		{name: "message stop", wire: "event: message_stop\ndata: {\"type\":\"message_stop\",\"future\":{\"kept\":true}}\n\n", wantError: func(err error) bool { return errors.Is(err, io.EOF) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.wire)
			}))
			t.Cleanup(server.Close)
			originalHTTPClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })
			provider, request := newNativeClaudeProviderForTest(t, server, `{"model":"claude-test","max_tokens":1,"messages":[],"stream":true}`, nil)
			stream, apiErr := provider.CreateClaudeChatStream(request)
			if apiErr != nil {
				t.Fatalf("open Claude stream: %+v", apiErr)
			}
			data, streamErrors := stream.Recv()
			defer stream.Close()
			select {
			case got := <-data:
				if got != test.wire {
					t.Fatalf("Claude SSE wire changed:\nwant %q\n got %q", test.wire, got)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for Claude event")
			}
			select {
			case err := <-streamErrors:
				if !test.wantError(err) {
					t.Fatalf("unexpected Claude terminal error: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for Claude terminal")
			}
		})
	}
}
