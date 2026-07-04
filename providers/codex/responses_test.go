package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
	"one-api/types"
)

func (p *CodexProvider) CreateResponsesForTest(request *types.OpenAIResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	rawReq, errWithCode := p.rawResponsesRequestForTest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	return p.CreateResponses(context.Background(), rawReq)
}

func (p *CodexProvider) CreateResponsesStreamForTest(request *types.OpenAIResponsesRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	rawReq, errWithCode := p.rawResponsesRequestForTest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	return p.CreateResponsesStream(context.Background(), rawReq)
}

func (p *CodexProvider) CompactResponsesForTest(request *types.OpenAIResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	rawReq, errWithCode := p.rawResponsesRequestForTest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	rawReq.Operation = "responses.compact.http"
	return p.CompactResponses(context.Background(), rawReq)
}

func (p *CodexProvider) rawResponsesRequestForTest(request *types.OpenAIResponsesRequest) (*commonresponses.Request, *types.OpenAIErrorWithStatusCode) {
	if request == nil {
		return nil, common.StringErrorWrapperLocal("request is required", "invalid_request_error", http.StatusBadRequest)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "marshal_request_failed", http.StatusInternalServerError)
	}
	envelope, err := commonresponses.ParseRawEnvelope(raw)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_request_error", http.StatusBadRequest)
	}
	downstreamDialect := commonresponses.DownstreamResponses
	if request.ConvertChat {
		downstreamDialect = commonresponses.DownstreamChatCompletions
	}
	headers := requestctx.HeaderSnapshot{}
	principal := requestctx.Principal{}
	channelID := 0
	if p != nil {
		if p.Context != nil && p.Context.Request != nil {
			headers = requestctx.NewHeaderSnapshot(p.Context.Request.Header)
			principal = requestctx.PrincipalFromGin(p.Context)
		}
		if p.Channel != nil {
			channelID = p.Channel.Id
		}
	}
	return &commonresponses.Request{
		Operation: commonresponses.ResponsesCreate,
		Headers:   headers,
		Body:      envelope,
		Control: commonresponses.Control{
			DownstreamDialect: downstreamDialect,
			Stream:            request.Stream,
		},
		Principal: principal,
		ChannelID: channelID,
		Model:     request.Model,
	}, nil
}

func TestChatResponsesRequestUsesControlPlaneForChatDialect(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	rawReq, errWithCode := provider.chatResponsesRequestFromTyped(&types.OpenAIResponsesRequest{
		Model:       "gpt-5",
		Stream:      true,
		ConvertChat: true,
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("chatResponsesRequestFromTyped returned error: %v", errWithCode.Message)
	}
	if rawReq.Control.DownstreamDialect != commonresponses.DownstreamChatCompletions || !rawReq.Control.Stream {
		t.Fatalf("expected chat-completions stream control, got %+v", rawReq.Control)
	}
	if rawReq.Body.Projection.ConvertChat {
		t.Fatal("expected raw JSON projection not to carry ConvertChat")
	}
	rawBody := string(rawReq.Body.Object.Raw)
	if strings.Contains(rawBody, "ConvertChat") || strings.Contains(rawBody, "convert_chat") {
		t.Fatalf("expected raw upstream body not to contain ConvertChat control, got %s", rawBody)
	}
}

type fakeStringStream struct {
	dataChan  chan string
	errChan   chan error
	closed    chan struct{}
	closeOnce sync.Once
}

func (s *fakeStringStream) Recv() (<-chan string, <-chan error) {
	return s.dataChan, s.errChan
}

func (s *fakeStringStream) Close() {
	if s == nil || s.closed == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.closed)
	})
}

func TestCodexResponsesWSAdapterTreatsBinaryProviderFrameAsMalformed(t *testing.T) {
	adapter := &codexResponsesWSAdapter{}
	if _, ok := any(adapter).(responsesws.BinaryProviderFrameCapable); ok {
		t.Fatal("expected codex responses websocket adapter not to opt into binary provider frames")
	}
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewBinaryFrame([]byte("binary")))
	if result.Origin != responsesws.RecvDetailOriginProviderMalformed || result.Err == nil || !result.CloseTransport || result.EmitFrame != nil {
		t.Fatalf("expected binary provider frame to become provider_malformed, got %+v", result)
	}
}

func TestCodexResponsesWSAdapterTreatsMalformedTextProviderFrameAsMalformed(t *testing.T) {
	adapter := &codexResponsesWSAdapter{}
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":`)))
	if result.Origin != responsesws.RecvDetailOriginProviderMalformed || result.Err == nil || !result.CloseTransport || result.EmitFrame != nil {
		t.Fatalf("expected malformed text provider frame to become provider_malformed, got %+v", result)
	}
	if payload := responsesws.ClientPayloadFromError(result.Err); len(payload) != 0 {
		t.Fatalf("expected provider malformed error not to carry client payload, got %q", payload)
	}
}

func TestCodexResponsesWSAdapterMissingProviderFailsClosed(t *testing.T) {
	adapter := &codexResponsesWSAdapter{}
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.created"}`)))
	if result.Origin != responsesws.RecvDetailOriginProviderMalformed || result.Err == nil || !result.CloseTransport {
		t.Fatalf("expected missing provider to fail closed, got %+v", result)
	}
}

func TestCodexResponsesWSAdapterFutureProviderEventShapePassesThrough(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5"}
	payload := []byte(`{"type":"response.future","event_id":"evt_future","response":"opaque","future":{"enabled":true}}`)
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(payload))
	if result.Err != nil || result.CloseTransport || result.Filtered || result.EmitFrame == nil {
		t.Fatalf("expected future provider event to pass through, got %+v", result)
	}
	if string(result.EmitFrame.Payload()) != string(payload) || result.Usage != nil || result.Origin != responsesws.RecvDetailOriginProviderFrame {
		t.Fatalf("expected future provider event to pass through byte-identically without usage, got %+v payload=%s", result, result.EmitFrame.Payload())
	}
}

func TestCodexResponsesWSAdapterKnownTerminalBadResponseShapeClosesAsProviderMalformed(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5"}
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.completed","response":"opaque"}`)))
	if result.Origin != responsesws.RecvDetailOriginProviderMalformed || !errors.Is(result.Err, responsesws.ErrInvalidProviderEventPayload) || !result.CloseTransport || result.EmitFrame != nil {
		t.Fatalf("expected bad known terminal shape to become provider_malformed, got %+v", result)
	}
}

func TestCodexResponsesWSAdapterRejectsDuplicateKeyClientCancel(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5"}
	_, err := adapter.PrepareClientFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","type":"response.cancel"}`)))
	if err == nil || !strings.Contains(err.Error(), "invalid_event") {
		t.Fatalf("expected duplicate-key client event to become invalid_event, got %v", err)
	}
}

func TestCodexResponsesWSAdapterAppliesOpenPreviousResponseIDDefault(t *testing.T) {
	tests := []struct {
		name                   string
		defaultPrevious        string
		payload                string
		wantPrevious           string
		wantPreviousResponseID bool
	}{
		{
			name:                   "injects open default when client omits previous response",
			defaultPrevious:        " resp_open_default ",
			payload:                `{"type":"response.create","model":"gpt-5","input":"hi"}`,
			wantPrevious:           "resp_open_default",
			wantPreviousResponseID: true,
		},
		{
			name:                   "preserves explicit client previous response",
			defaultPrevious:        "resp_open_default",
			payload:                `{"type":"response.create","model":"gpt-5","previous_response_id":"resp_client","input":"hi"}`,
			wantPrevious:           "resp_client",
			wantPreviousResponseID: true,
		},
		{
			name:                   "does not inject blank open default",
			defaultPrevious:        "   ",
			payload:                `{"type":"response.create","model":"gpt-5","input":"hi"}`,
			wantPreviousResponseID: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &codexResponsesWSAdapter{
				provider:                  &CodexProvider{},
				model:                     "gpt-5",
				defaultPreviousResponseID: strings.TrimSpace(tc.defaultPrevious),
			}

			frame, err := adapter.PrepareClientFrame(context.Background(), responsesws.NewTextFrame([]byte(tc.payload)))
			if err != nil {
				t.Fatalf("prepare response.create: %v", err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(frame.Payload(), &object); err != nil {
				t.Fatalf("decode prepared frame: %v", err)
			}
			rawPrevious, exists := object["previous_response_id"]
			if exists != tc.wantPreviousResponseID {
				t.Fatalf("previous_response_id presence mismatch: got %v want %v payload=%s", exists, tc.wantPreviousResponseID, frame.Payload())
			}
			if !tc.wantPreviousResponseID {
				return
			}
			var gotPrevious string
			if err := json.Unmarshal(rawPrevious, &gotPrevious); err != nil {
				t.Fatalf("decode previous_response_id: %v", err)
			}
			if gotPrevious != tc.wantPrevious {
				t.Fatalf("previous_response_id mismatch: got %q want %q payload=%s", gotPrevious, tc.wantPrevious, frame.Payload())
			}
		})
	}
}

func TestCodexResponsesWSAdapterFiltersOpaqueSessionCreatedBootstrap(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5"}
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"session.created","session":"opaque"}`)))
	if !result.Filtered || result.Err != nil || result.CloseTransport || result.EmitFrame != nil || result.Usage != nil || result.Origin != responsesws.RecvDetailOriginProviderFrame {
		t.Fatalf("expected opaque session.created bootstrap to be filtered, got %+v", result)
	}
}

func TestCodexResponsesWSAdapterTerminalTextProviderFrameExtractsUsage(t *testing.T) {
	adapter := &codexResponsesWSAdapter{
		provider:    &CodexProvider{},
		model:       "gpt-5",
		accumulator: newCodexTurnUsageAccumulator(),
	}
	payload := []byte(`{"type":"response.completed","response":{"id":"resp_done","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`)
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(payload))
	if result.Err != nil || result.CloseTransport || result.Filtered || result.EmitFrame == nil {
		t.Fatalf("expected terminal provider frame to pass through, got %+v", result)
	}
	if string(result.EmitFrame.Payload()) != string(payload) || result.Origin != responsesws.RecvDetailOriginProviderFrame {
		t.Fatalf("expected terminal provider frame to pass through byte-identically, got %+v payload=%s", result, result.EmitFrame.Payload())
	}
	if result.Usage == nil || result.Usage.InputTokens != 3 || result.Usage.OutputTokens != 5 || result.Usage.TotalTokens != 8 {
		t.Fatalf("expected terminal usage to be extracted, got %+v", result.Usage)
	}
	if adapter.lastResponse != "resp_done" || adapter.accumulator != nil {
		t.Fatalf("expected terminal frame to update response state and clear accumulator, last=%q accumulator=%+v", adapter.lastResponse, adapter.accumulator)
	}
}

func TestCollectResponsesStreamResponseAcceptsDataWithoutSpace(t *testing.T) {
	provider := &CodexProvider{}
	provider.Usage = &types.Usage{}

	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n"
		stream.errChan <- io.EOF
	}()

	resp, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		t.Fatalf("collectResponsesStreamResponse returned error: %v", errWithCode.Message)
	}

	if resp == nil || resp.ID != "resp_1" {
		t.Fatalf("unexpected response: %#v", resp)
	}

	if provider.Usage.TotalTokens != 0 {
		t.Fatalf("expected stream collector not to finalize provider usage directly, got %d", provider.Usage.TotalTokens)
	}
}

func TestCollectResponsesStreamResponseClosedErrChannelFinishes(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_closed_err\",\"status\":\"completed\"}}\n"
	close(stream.dataChan)
	close(stream.errChan)

	done := make(chan *types.OpenAIErrorWithStatusCode, 1)
	go func() {
		_, errWithCode := provider.collectResponsesStreamResponse(stream)
		done <- errWithCode
	}()

	timeout := time.NewTimer(500 * time.Millisecond)
	defer timeout.Stop()
	select {
	case errWithCode := <-done:
		if errWithCode != nil {
			t.Fatalf("collectResponsesStreamResponse returned error: %v", errWithCode.Message)
		}
	case <-timeout.C:
		t.Fatal("collectResponsesStreamResponse did not finish after err channel closed")
	}
}

func TestCodexResponsesStreamHandlerFlushesUnterminatedEvent(t *testing.T) {
	handler := newCodexResponsesStreamHandler(&types.Usage{})
	var emitted []string
	send := func(data string) bool {
		emitted = append(emitted, data)
		return true
	}

	firstEvent := []byte("event: response.created")
	firstData := []byte(`data: {"type":"response.created"}`)
	secondEvent := []byte("event: response.completed")
	handler.handleResponsesStream(&firstEvent, send)
	handler.handleResponsesStream(&firstData, send)
	handler.handleResponsesStream(&secondEvent, send)

	if len(emitted) != 1 || !strings.Contains(emitted[0], "response.created") {
		t.Fatalf("expected unterminated first event to be flushed before next event, got %#v", emitted)
	}
}

func TestExtractJSONFromSSEUsesLineCursorAndCombinesDataLines(t *testing.T) {
	got := extractJSONFromSSE("event: response.completed\r\ndata: {\r\ndata: \"type\":\"response.completed\"}\r\ndata: [DONE]\r\n")
	want := "{\n\"type\":\"response.completed\"}"
	if got != want {
		t.Fatalf("unexpected SSE payload: got %q want %q", got, want)
	}
}

func TestCodexResponsesStreamHandlerRejectsOversizedEvent(t *testing.T) {
	originalLimit := codexResponsesStreamMaxEventBytes
	codexResponsesStreamMaxEventBytes = 12
	t.Cleanup(func() {
		codexResponsesStreamMaxEventBytes = originalLimit
	})

	handler := newCodexResponsesStreamHandler(&types.Usage{})
	var emitted []string
	var gotErr error
	sendData := func(data string) bool {
		emitted = append(emitted, data)
		return true
	}
	sendError := func(err error) bool {
		gotErr = err
		return true
	}

	line := []byte("event: response.created")
	handler.handleResponsesStreamWithError(&line, sendData, sendError)

	if !errors.Is(gotErr, errCodexResponsesSSEEventTooLarge) {
		t.Fatalf("expected oversized event error, got %v", gotErr)
	}
	if len(emitted) != 0 {
		t.Fatalf("expected oversized event not to be emitted, got %#v", emitted)
	}
	if handler.eventBuffer.Len() != 0 || handler.eventType != "" {
		t.Fatalf("expected oversized event to reset buffer, len=%d type=%q", handler.eventBuffer.Len(), handler.eventType)
	}
}

func TestCollectResponsesStreamResponsePreservesEmptyReasoningSummary(t *testing.T) {
	provider := &CodexProvider{}
	provider.Usage = &types.Usage{}

	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"id\":\"rs_1\",\"status\":\"completed\",\"summary\":[]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n"
		stream.errChan <- io.EOF
	}()

	resp, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		t.Fatalf("collectResponsesStreamResponse returned error: %v", errWithCode.Message)
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	if !strings.Contains(string(data), "\"summary\":[]") {
		t.Fatalf("expected marshaled response to preserve empty summary array, got %s", string(data))
	}
}

func TestCollectResponsesStreamResponseAcceptsWrappedEOF(t *testing.T) {
	provider := &CodexProvider{}
	provider.Usage = &types.Usage{}

	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_wrapped_eof\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n"
		stream.errChan <- fmt.Errorf("wrapped: %w", io.EOF)
	}()

	resp, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		t.Fatalf("expected wrapped EOF to terminate stream cleanly, got %v", errWithCode.Message)
	}
	if resp == nil || resp.ID != "resp_wrapped_eof" {
		t.Fatalf("expected wrapped EOF response, got %#v", resp)
	}
}

func TestCodexResponsesSearchTypeRecognizesNormalizedWebSearchAlias(t *testing.T) {
	response := &types.OpenAIResponsesResponses{
		Tools: []types.ResponsesTools{
			{Type: types.APIToolTypeWebSearch, SearchContextSize: "high"},
		},
	}

	if got := codexResponsesSearchType(response); got != "high" {
		t.Fatalf("expected normalized web_search alias to preserve search_context_size, got %q", got)
	}
}

func TestCompactResponsesUsesCompactEndpoint(t *testing.T) {
	var (
		gotPath   string
		gotAccept string
		bodyBytes []byte
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")

		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_cmp_1","object":"response.compaction","output":[{"id":"cmp_1","type":"compaction","encrypted_content":"ciphertext"}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL

	resp, errWithCode := provider.CompactResponsesForTest(&types.OpenAIResponsesRequest{
		Model:   "gpt-5",
		Input:   "hello",
		Stream:  true,
		Include: []string{"reasoning.encrypted_content", "custom.include"},
	})
	if errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	if gotPath != "/backend-api/codex/responses/compact" {
		t.Fatalf("expected compact endpoint path, got %q", gotPath)
	}
	if gotAccept != "application/json" {
		t.Fatalf("expected JSON accept header, got %q", gotAccept)
	}

	var raw map[string]any
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		t.Fatalf("failed to decode upstream request body: %v", err)
	}
	if _, exists := raw["stream"]; exists {
		t.Fatalf("expected compact request body to omit stream, got %s", string(bodyBytes))
	}
	if _, exists := raw["context_management"]; exists {
		t.Fatalf("expected compact request body to omit context_management, got %s", string(bodyBytes))
	}
	if _, exists := raw["truncation"]; exists {
		t.Fatalf("expected compact request body to omit truncation, got %s", string(bodyBytes))
	}
	if _, exists := raw["include"]; exists {
		t.Fatalf("expected compact request body to omit include, got %s", string(bodyBytes))
	}
	if _, exists := raw["store"]; exists {
		t.Fatalf("expected compact request body to omit store, got %s", string(bodyBytes))
	}

	if resp.Object != "response.compaction" {
		t.Fatalf("expected compaction object, got %q", resp.Object)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "compaction" {
		t.Fatalf("expected compaction output item, got %#v", resp.Output)
	}
	if resp.Output[0].EncryptedContent == nil || *resp.Output[0].EncryptedContent != "ciphertext" {
		t.Fatalf("expected encrypted content to be preserved, got %#v", resp.Output[0].EncryptedContent)
	}
	if provider.Usage.TotalTokens != 18 {
		t.Fatalf("expected provider usage to be updated, got %d", provider.Usage.TotalTokens)
	}
}

func TestCompactResponsesBackfillsPromptCacheKeyFromRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_cmp_2","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL

	resp, errWithCode := provider.CompactResponsesForTest(&types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "stable-cache-key",
	})
	if errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	if resp.PromptCacheKey != "stable-cache-key" {
		t.Fatalf("expected response prompt_cache_key to be backfilled, got %q", resp.PromptCacheKey)
	}
}

func TestCompactResponsesDoesNotBackfillUsageFromRetainedOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_cmp_3","object":"response.compaction","output":[{"id":"msg_1","type":"message","role":"user","content":[{"type":"input_text","text":"retained context that should not be billed as completion"}]},{"id":"cmp_1","type":"compaction","encrypted_content":"ciphertext"}]}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{
		PromptTokens:     99,
		CompletionTokens: 77,
		TotalTokens:      176,
	}

	resp, errWithCode := provider.CompactResponsesForTest(&types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: "hello",
	})
	if errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	if resp.Usage == nil {
		t.Fatalf("expected usage to be initialized")
	}
	if resp.Usage.InputTokens != 99 || resp.Usage.OutputTokens != 0 || resp.Usage.TotalTokens != 99 {
		t.Fatalf("expected missing compact usage to preserve prompt tokens without output backfill, got %#v", resp.Usage)
	}
	if provider.Usage.PromptTokens != 99 || provider.Usage.CompletionTokens != 0 || provider.Usage.TotalTokens != 99 {
		t.Fatalf("expected provider usage to preserve prompt tokens without output backfill, got %#v", provider.Usage)
	}
}

func TestCompactResponsesPreservesDetailedUsageAndExtraBilling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_cmp_4",
			"object":"response.compaction",
			"tools":[{"type":"web_search_preview","search_context_size":"high"}],
			"output":[{"id":"ws_1","type":"web_search_call","status":"completed"}],
			"usage":{
				"input_tokens":11,
				"output_tokens":7,
				"total_tokens":18,
				"input_tokens_details":{"cached_tokens":4,"text_tokens":2,"image_tokens":3},
				"output_tokens_details":{"reasoning_tokens":5}
			}
		}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{PromptTokens: 11}

	_, errWithCode := provider.CompactResponsesForTest(&types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: "hello",
	})
	if errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	if provider.Usage.PromptTokensDetails.CachedTokens != 4 || provider.Usage.PromptTokensDetails.TextTokens != 2 || provider.Usage.PromptTokensDetails.ImageTokens != 3 {
		t.Fatalf("expected compact usage details to be preserved, got %#v", provider.Usage.PromptTokensDetails)
	}
	if provider.Usage.CompletionTokensDetails.ReasoningTokens != 5 {
		t.Fatalf("expected compact reasoning usage to be preserved, got %#v", provider.Usage.CompletionTokensDetails)
	}
	billing, ok := provider.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected compact responses to preserve tool extra billing, got %+v", provider.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search charge, got %+v", billing)
	}
}

func TestCreateResponsesBackfillsUsageAndExtraBillingWithoutTerminalUsage(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.created\",\"response\":{\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}]}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.added\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.output_item.added\",\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_1\",\"status\":\"completed\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.output_text.delta\",\"delta\":\"hello from codex\"}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_4\",\"object\":\"response\",\"status\":\"completed\",\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}],\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello from codex\"}]},{\"id\":\"ws_1\",\"type\":\"web_search_call\",\"status\":\"completed\"}]}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{PromptTokens: 11}

	resp, errWithCode := provider.CreateResponsesForTest(&types.OpenAIResponsesRequest{
		Model: "gpt-3.5-turbo",
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("CreateResponses returned error: %v", errWithCode.Message)
	}

	if resp.Usage == nil || resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens <= 0 || resp.Usage.TotalTokens <= 11 {
		t.Fatalf("expected missing terminal usage to be backfilled from response content, got %#v", resp.Usage)
	}
	if provider.Usage.CompletionTokens <= 0 || provider.Usage.TotalTokens <= provider.Usage.PromptTokens {
		t.Fatalf("expected provider usage completion tokens to be backfilled, got %#v", provider.Usage)
	}
	billing, ok := provider.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected create responses to preserve tool extra billing, got %+v", provider.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search charge, got %+v", billing)
	}
}

func TestCodexResponsesStreamHandlerAccumulatesToolBillingAndTextFallbackState(t *testing.T) {
	handler := newCodexResponsesStreamHandler(&types.Usage{})

	dataChan := make(chan string, 8)
	errChan := make(chan error, 1)

	created := []byte(`data: {"type":"response.created","response":{"tools":[{"type":"web_search_preview","search_context_size":"high"}]}}`)
	handler.HandlerResponsesStream(&created, dataChan, errChan)

	added := []byte(`data: {"type":"response.output_item.added","item":{"type":"web_search_call","id":"ws_1","status":"completed"}}`)
	handler.HandlerResponsesStream(&added, dataChan, errChan)

	delta := []byte(`data: {"type":"response.output_text.delta","delta":"hello from codex"}`)
	handler.HandlerResponsesStream(&delta, dataChan, errChan)

	if got := handler.Usage.TextBuilder.String(); got != "hello from codex" {
		t.Fatalf("expected output text delta to accumulate for fallback counting, got %q", got)
	}
	billing, ok := handler.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected stream handler to preserve tool extra billing, got %+v", handler.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search charge, got %+v", billing)
	}
}

func TestCodexResponsesStreamHandlerDoesNotDoubleCountTerminalToolBilling(t *testing.T) {
	handler := newCodexResponsesStreamHandler(&types.Usage{})

	dataChan := make(chan string, 8)
	errChan := make(chan error, 1)

	created := []byte(`data: {"type":"response.created","response":{"tools":[{"type":"web_search_preview","search_context_size":"high"}]}}`)
	handler.HandlerResponsesStream(&created, dataChan, errChan)

	added := []byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"web_search_call","id":"ws_1","status":"completed"}}`)
	handler.HandlerResponsesStream(&added, dataChan, errChan)

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8},"tools":[{"type":"web_search_preview","search_context_size":"high"}],"output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"id":"ws_1","type":"web_search_call","status":"completed"}]}}`)
	handler.HandlerResponsesStream(&completed, dataChan, errChan)

	billing, ok := handler.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected terminal stream handler to preserve tool extra billing, got %+v", handler.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected terminal stream handler to charge web search once, got %+v", billing)
	}
}

func TestCreateResponsesStreamConvertChatDoesNotDoubleCountToolBilling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.created\",\"response\":{\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}]}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.added\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_1\",\"status\":\"completed\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream_chat\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8},\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}],\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello from codex\"}]},{\"id\":\"ws_1\",\"type\":\"web_search_call\",\"status\":\"completed\"}]}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{PromptTokens: 11}

	stream, errWithCode := provider.CreateResponsesStreamForTest(&types.OpenAIResponsesRequest{
		Model:       "gpt-5",
		ConvertChat: true,
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("CreateResponsesStream returned error: %v", errWithCode.Message)
	}
	defer stream.Close()

	dataChan, errChan := stream.Recv()
	receivedChunks := 0
	timeout := time.NewTimer(500 * time.Millisecond)
	defer timeout.Stop()
	for receivedChunks < 2 {
		select {
		case _, ok := <-dataChan:
			if !ok {
				receivedChunks = 2
				continue
			}
			receivedChunks++
		case err, ok := <-errChan:
			if !ok {
				receivedChunks = 2
				continue
			}
			if err != nil && err != io.EOF {
				t.Fatalf("unexpected stream error: %v", err)
			}
		case <-timeout.C:
			t.Fatal("timed out waiting for convert-chat stream output")
		}
	}

	billing, ok := provider.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected convert-chat stream to preserve tool extra billing, got %+v", provider.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected convert-chat stream to charge web search once, got %+v", billing)
	}
}

func TestCreateResponsesBackfillsPromptCacheKeyFromRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_3\",\"object\":\"response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL

	resp, errWithCode := provider.CreateResponsesForTest(&types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "stable-cache-key",
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("CreateResponses returned error: %v", errWithCode.Message)
	}

	if resp.PromptCacheKey != "stable-cache-key" {
		t.Fatalf("expected response prompt_cache_key to be backfilled, got %q", resp.PromptCacheKey)
	}
}

func TestCreateResponsesBodyIncludesPromptCachePolicyDecision(t *testing.T) {
	var upstreamBody map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if err := json.Unmarshal(body, &upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v body=%s", err, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_prompt_policy\",\"object\":\"response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"prompt_cache_key_strategy":"user_id"}`, nil)
	provider.Context.Set("id", int64(7))
	provider.Channel.BaseURL = &server.URL

	request := &types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	}
	rawReq, errWithCode := provider.rawResponsesRequestForTest(request)
	if errWithCode != nil {
		t.Fatalf("rawResponsesRequestForTest returned error: %v", errWithCode.Message)
	}
	expectedKey := promptCacheKeyForRequestStrategy(&types.OpenAIResponsesRequest{Model: "gpt-5"}, provider.Context, codexPromptCacheStrategyUserID)
	rawReq.Policy.PromptCache = &commonresponses.PromptCacheDecision{
		Key:    expectedKey,
		Source: commonresponses.PromptCacheRouteHint,
	}
	resp, errWithCode := provider.CreateResponses(context.Background(), rawReq)
	if errWithCode != nil {
		t.Fatalf("CreateResponses returned error: %v", errWithCode.Message)
	}

	if string(upstreamBody["prompt_cache_key"]) != `"`+expectedKey+`"` {
		t.Fatalf("expected upstream prompt_cache_key %q, got %s body=%#v", expectedKey, upstreamBody["prompt_cache_key"], upstreamBody)
	}
	if resp.PromptCacheKey != expectedKey {
		t.Fatalf("expected response prompt_cache_key backfill from policy, got %q want %q", resp.PromptCacheKey, expectedKey)
	}
}
