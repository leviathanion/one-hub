package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/providers/openai"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type fakeRelayStream struct {
	dataChan        chan string
	errChan         chan error
	observeAccepted func(string) error
}

type failingRelayResponseWriter struct {
	header http.Header
	err    error
}

func (w *failingRelayResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingRelayResponseWriter) WriteHeader(int) {}

func (w *failingRelayResponseWriter) Write([]byte) (int, error) { return 0, w.err }

func (w *failingRelayResponseWriter) Flush() {}

var _ commonresponses.EventStream = (*fakeRelayStream)(nil)

func (s *fakeRelayStream) Recv() (<-chan string, <-chan error) {
	return s.dataChan, s.errChan
}

func (s *fakeRelayStream) Close() {}

func (s *fakeRelayStream) ObserveResponsesEvent(event string) error {
	if s == nil || s.observeAccepted == nil {
		return nil
	}
	return s.observeAccepted(event)
}

func TestResponseStreamClientReturnsAcceptedMidStreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	stream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- `{"id":"chunk-1"}`
		stream.errChan <- errors.New("upstream stream broken Authorization: Bearer secret-token api_key=query-secret https://provider.example/v1?token=url-secret session session-secret sk-testSECRET123")
	}()

	firstResponseTime, errWithCode := responseStreamClient(ctx, stream, nil)
	if errWithCode == nil || errWithCode.Code != "stream_read_failed" || !errWithCode.UpstreamAccepted {
		t.Fatalf("expected accepted stream failure, got: %+v", errWithCode)
	}

	if firstResponseTime.IsZero() {
		t.Fatalf("expected first response time to be set")
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `data: {"id":"chunk-1"}`) {
		t.Fatalf("expected stream body to include first chunk, got: %q", body)
	}

	if !strings.Contains(body, `"stream_error"`) {
		t.Fatalf("expected stream body to include SSE error payload, got: %q", body)
	}
	if !strings.Contains(body, `"message":"stream interrupted"`) {
		t.Fatalf("expected stream body to include stable stream error message, got: %q", body)
	}
	for _, forbidden := range []string{
		"upstream stream broken",
		"Authorization",
		"secret-token",
		"query-secret",
		"provider.example",
		"url-secret",
		"session-secret",
		"sk-testSECRET123",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("expected stream error body not to leak %q, got %q", forbidden, body)
		}
	}
}

func TestResponseStreamClientPreservesSanitizedProviderAPIError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	tests := []struct {
		name        string
		provider    *types.OpenAIErrorWithStatusCode
		controlCode string
		contains    []string
		forbidden   []string
	}{
		{
			name: "business error",
			provider: &types.OpenAIErrorWithStatusCode{
				OpenAIError: types.OpenAIError{Type: "invalid_request_error", Code: "bad_input", Message: "invalid tool", Param: "tools[0]"},
				StatusCode:  http.StatusBadRequest,
			},
			controlCode: "bad_input",
			contains:    []string{`"type":"invalid_request_error"`, `"code":"bad_input"`, `"message":"invalid tool"`, `"param":"tools[0]"`},
			forbidden:   []string{`"stream_error"`, "[DONE]"},
		},
		{
			name: "provider account error",
			provider: &types.OpenAIErrorWithStatusCode{
				OpenAIError: types.OpenAIError{Type: "authentication_error", Code: "invalid_api_key", Message: "organization org-secret rejected at https://provider.example"},
				StatusCode:  http.StatusUnauthorized,
			},
			controlCode: "invalid_api_key",
			contains:    []string{`"type":"authentication_error"`, `"code":"invalid_api_key"`, `organization [redacted] rejected`},
			forbidden:   []string{"org-secret", "[DONE]"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			stream := &fakeRelayStream{dataChan: make(chan string), errChan: make(chan error, 1)}
			stream.errChan <- test.provider

			_, apiErr := responseStreamClient(ctx, stream, nil)
			if apiErr == nil || apiErr.Code != test.controlCode {
				t.Fatalf("expected provider stream failure to reach control flow: %+v", apiErr)
			}
			body := recorder.Body.String()
			for _, value := range test.contains {
				if !strings.Contains(body, value) {
					t.Fatalf("provider error field %q missing from %q", value, body)
				}
			}
			for _, value := range test.forbidden {
				if strings.Contains(body, value) {
					t.Fatalf("provider error leaked or invented %q in %q", value, body)
				}
			}
		})
	}
}

func TestResponseStreamClientClosedChannelsFinishAsEOF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	stream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}

	go func() {
		stream.dataChan <- `{"id":"chunk-closed"}`
		close(stream.dataChan)
		close(stream.errChan)
	}()

	firstResponseTime, errWithCode := responseStreamClient(ctx, stream, func() string {
		return `{"id":"end"}`
	})
	if errWithCode != nil {
		t.Fatalf("expected nil error, got: %v", errWithCode.Message)
	}
	if firstResponseTime.IsZero() {
		t.Fatal("expected first response time to be set")
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `data: {"id":"chunk-closed"}`) {
		t.Fatalf("expected stream body to include upstream chunk, got: %q", body)
	}
	if !strings.Contains(body, `data: {"id":"end"}`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected closed channels to finish with end payload and [DONE], got: %q", body)
	}
}

func TestResponseStreamClientInBandProviderErrorDoesNotAppendSuccessTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	stream := &fakeRelayStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- `{"error":{"message":"invalid field","type":"invalid_request_error","code":"invalid_value"}}`
	close(stream.dataChan)
	close(stream.errChan)

	if _, apiErr := responseStreamClient(ctx, stream, func() string {
		return `{"id":"synthetic-usage"}`
	}); apiErr == nil || apiErr.Code != "invalid_value" {
		t.Fatalf("expected rendered in-band provider error fact, got %+v", apiErr)
	}
	if !ctx.GetBool(streamErrorAlreadyRenderedContextKey) {
		t.Fatal("in-band provider error did not publish its egress receipt")
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `data: {"error":{"message":"invalid field","type":"invalid_request_error","code":"invalid_value"}}`) {
		t.Fatalf("expected in-band provider error to be delivered, got %q", body)
	}
	for _, unexpected := range []string{"synthetic-usage", "data: [DONE]"} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("in-band provider error was followed by a success terminal %q: %q", unexpected, body)
		}
	}
}

func TestExactChatSSEInBandErrorDoesNotAppendSuccessTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	handler := openai.OpenAIStreamHandler{Usage: &types.Usage{}}
	stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{
		Body: io.NopCloser(strings.NewReader("data: {\"error\":{\"message\":\"invalid field\",\"type\":\"invalid_request_error\",\"code\":\"invalid_value\"}}\n\n")),
	}, handler.HandleExactChatSSE, requester.StreamReadOptions{RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatalf("create exact chat stream: %+v", apiErr)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if _, apiErr := responseStreamClient(ctx, stream, func() string {
		return `{"id":"synthetic-usage"}`
	}); apiErr == nil || apiErr.Code != "invalid_value" {
		t.Fatalf("expected exact in-band provider error fact, got %+v", apiErr)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `"code":"invalid_value"`) {
		t.Fatalf("expected exact in-band error to reach the client, got %q", body)
	}
	for _, unexpected := range []string{"synthetic-usage", "data: [DONE]"} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("exact in-band error was followed by a success terminal %q: %q", unexpected, body)
		}
	}
}

func TestExactChatSSEEgressPreservesCompleteRawEventsAndTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	rawEvent := ": keep  two\r\nid: 007\r\nevent: chunk\r\ndata:  {\"id\":\"chatcmpl_raw\",\"model\":\"gpt-5\",\"message\":\"future success https://example.com\",\"code\":\"future_code\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}],\"future\":12345678901234567890}  \r\n\r\n"
	doneEvent := "data:[DONE]\r\n\r\n"
	handler := openai.OpenAIStreamHandler{Usage: &types.Usage{}}
	stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{
		Body: io.NopCloser(strings.NewReader(rawEvent + doneEvent)),
	}, handler.HandleExactChatSSE, requester.StreamReadOptions{RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatalf("create exact Chat stream: %+v", apiErr)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	first, apiErr := responseStreamClient(ctx, stream, func() string { return `{"id":"synthetic-usage"}` })
	if apiErr != nil {
		t.Fatalf("exact Chat egress failed: %+v", apiErr)
	}
	if first.IsZero() {
		t.Fatal("exact raw event did not establish first response time")
	}
	if got, want := recorder.Body.String(), rawEvent+doneEvent; got != want {
		t.Fatalf("exact Chat egress rewrote or synthesized SSE:\nwant %q\n got %q", want, got)
	}
}

func TestNativeResponseStreamStopsOnDownstreamWriteFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := &failingRelayResponseWriter{err: errors.New("client disconnected")}
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	stream := &fakeRelayStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- "data: {\"type\":\"response.output_text.delta\"}\n\n"

	_, err := responseGeneralStreamClientWithObserverResult(ctx, stream, nil, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "client disconnected") {
		t.Fatalf("downstream write failure was not returned: %v", err)
	}
}

func TestSanitizeProviderSSELinePreservesFramingAndHidesAccountError(t *testing.T) {
	input := "data: {\"type\":\"error\",\"sequence_number\":4,\"code\":\"insufficient_quota\",\"message\":\"organization org-secret exhausted\",\"account_id\":\"acct-secret\"}\r\n"
	got := redactProviderSSEEvent(input)
	if !strings.HasPrefix(got, "data: ") || !strings.HasSuffix(got, "\r\n") || !strings.Contains(got, `"sequence_number":4`) || !strings.Contains(got, `"code":"insufficient_quota"`) {
		t.Fatalf("SSE framing or error envelope changed: %q", got)
	}
	for _, secret := range []string{"org-secret", "acct-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("provider account detail %q leaked from %q", secret, got)
		}
	}
}

func TestSanitizeProviderSSELinePreservesOrdinaryPayloadBytes(t *testing.T) {
	input := "  data:\t{\"type\":\"response.output_text.delta\",\"delta\":\"a  b\\n\\nhttps://example.com\",\"api_key\":\"model-authored-value\"}  \r\n"
	if got := redactProviderSSEEvent(input); got != input {
		t.Fatalf("ordinary SSE payload changed:\nwant: %q\n got: %q", input, got)
	}
}

func TestSanitizeProviderSSEEventHandlesEventPrefixAndSuccessMetadata(t *testing.T) {
	errorEvent := "event: error\ndata: {\"type\":\"error\",\"message\":\"organization org-secret exhausted\",\"account_id\":\"acct-secret\"}\n\n"
	safeError := redactProviderSSEEvent(errorEvent)
	if !strings.HasPrefix(safeError, "event: error\ndata: ") || !strings.HasSuffix(safeError, "\n\n") {
		t.Fatalf("SSE event framing changed: %q", safeError)
	}
	for _, secret := range []string{"org-secret", "acct-secret"} {
		if strings.Contains(safeError, secret) {
			t.Fatalf("provider error metadata %q leaked from %q", secret, safeError)
		}
	}

	successEvent := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"account_id\":\"acct-success\",\"output\":[{\"account_id\":\"model-authored\"}]}}\n\n"
	safeSuccess := redactProviderSSEEvent(successEvent)
	if safeSuccess != successEvent {
		t.Fatalf("success metadata policy was not scoped correctly: %q", safeSuccess)
	}
}

func TestSanitizeProviderSSEEventHandlesMultilineData(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		for _, payload := range [][]string{
			{`{"type":"error",`, `"message":"organization org-secret exhausted","account_id":"acct-secret"}`},
		} {
			prefix := "event: future" + newline + "id: 7" + newline + "retry: 123" + newline
			input := prefix + "data: " + payload[0] + newline + ": comment" + newline + "data: " + payload[1] + newline + newline
			got := redactProviderSSEEvent(input)
			if strings.Contains(got, "acct-secret") || strings.Contains(got, "org-secret") || !strings.HasPrefix(got, prefix) || !strings.Contains(got, ": comment"+newline) || !strings.HasSuffix(got, newline+newline) {
				t.Fatalf("multiline security rewrite lost framing or leaked metadata: %q", got)
			}
			if strings.Contains(input, "model-authored") && !strings.Contains(got, "model-authored") {
				t.Fatal("model-authored output was rewritten")
			}
		}
		ordinary := "event: future" + newline + "data: {\"future\":1e3," + newline + "data: \"content\":{\"account_id\":\"model-authored\"}}" + newline + newline
		if got := redactProviderSSEEvent(ordinary); got != ordinary {
			t.Fatalf("ordinary multiline raw changed: %q", got)
		}
	}
}

func TestChatResponseStreamReturnsDownstreamWriteFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := &failingRelayResponseWriter{err: errors.New("client disconnected")}
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	stream := &fakeRelayStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- `{"id":"chunk-1"}`

	_, apiErr := responseStreamClient(ctx, stream, nil)
	if apiErr == nil || apiErr.Code != "stream_write_failed" || !apiErr.UpstreamAccepted {
		t.Fatalf("downstream write failure was not surfaced with accepted state: %+v", apiErr)
	}
}

func TestChatResponseStreamWriteFailureDrainsBlockedLegacyHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handlerSecondSendFinished := make(chan struct{})
	stream, apiErr := requester.RequestStream[string](nil, &http.Response{
		Body: io.NopCloser(strings.NewReader("{\"chunk\":\"first\"}\n{\"chunk\":\"second\"}\n")),
	}, func(rawLine *[]byte, dataChan chan string, _ chan error) {
		dataChan <- string(*rawLine)
		if string(*rawLine) == `{"chunk":"second"}` {
			close(handlerSecondSendFinished)
		}
	})
	if apiErr != nil {
		t.Fatalf("create stream: %+v", apiErr)
	}

	writer := &failingRelayResponseWriter{err: errors.New("client disconnected")}
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, apiErr = responseStreamClient(ctx, stream, nil)
	if apiErr == nil || apiErr.Code != "stream_write_failed" {
		t.Fatalf("expected downstream write failure, got %+v", apiErr)
	}
	select {
	case <-handlerSecondSendFinished:
	case <-time.After(time.Second):
		t.Fatal("legacy handler remained blocked on its second send")
	}
}

func TestChatResponseStreamRejectsMissingProtocolTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	handler := openai.OpenAIStreamHandler{Usage: &types.Usage{}}
	stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{
		Body: io.NopCloser(strings.NewReader("data: {\"id\":\"chatcmpl_partial\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")),
	}, handler.HandleExactChatSSE, requester.StreamReadOptions{RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatalf("create chat stream: %+v", apiErr)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, apiErr = responseStreamClient(ctx, stream, func() string { return `{"id":"synthetic-usage"}` })
	if apiErr == nil || apiErr.Code != "stream_read_failed" || !apiErr.UpstreamAccepted {
		t.Fatalf("expected accepted missing-terminal failure, got %+v", apiErr)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "partial") || !strings.Contains(body, `"code":"stream_error"`) {
		t.Fatalf("expected partial data followed by a stream error, got %q", body)
	}
	if !ctx.GetBool(streamErrorAlreadyRenderedContextKey) {
		t.Fatal("rendered stream failure did not publish its egress receipt")
	}
	for _, unexpected := range []string{"synthetic-usage", "data: [DONE]"} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("missing terminal was converted to success %q: %q", unexpected, body)
		}
	}
}

func TestFetchChannelByModelWithSelectionRejectsFallbackWhenAffinityIsStrict(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	const (
		sessionID        = "strict-affinity-session"
		defaultChannelID = 11
		staleChannelID   = 424299
	)

	model.ChannelGroup = buildRealtimeTestChannelGroup(defaultChannelID)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil)
	ctx.Request.Header.Set("X-Session-Id", sessionID)
	ctx.Set("token_id", 301)
	ctx.Set("token_group", "default")

	rememberChannelAffinityKey(ctx, channelAffinityKindRealtime, sessionID)
	recordCurrentChannelAffinity(ctx, channelAffinityKindRealtime, staleChannelID)
	setPreferredChannelFromAffinity(ctx, staleChannelID)
	ctx.Set(channelAffinityStrictContextKey, true)

	channel, err := fetchChannelByModelWithSelection(ctx, "gpt-5", currentRealtimeChannelSelection(ctx))
	if err == nil {
		t.Fatal("expected strict affinity selection to reject fallback routing")
	}
	if channel != nil {
		t.Fatalf("expected no channel to be returned, got %#v", channel)
	}
	if channelID, ok := lookupChannelAffinity(ctx, channelAffinityKindRealtime, sessionID); !ok || channelID != staleChannelID {
		t.Fatalf("expected strict affinity binding to survive temporary unavailability, channel=%d ok=%v", channelID, ok)
	}
}

func TestFetchChannelByModelWithSelectionPreservesPreferredBindingWhenSkippedForRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	const (
		sessionID          = "strict-affinity-retry-skip"
		defaultChannelID   = 11
		preferredChannelID = 22
	)

	model.ChannelGroup = buildRealtimeTestChannelGroup(defaultChannelID, preferredChannelID)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil)
	ctx.Request.Header.Set("X-Session-Id", sessionID)
	ctx.Set("token_id", 302)
	ctx.Set("token_group", "default")

	rememberChannelAffinityKey(ctx, channelAffinityKindRealtime, sessionID)
	recordCurrentChannelAffinity(ctx, channelAffinityKindRealtime, preferredChannelID)
	setPreferredChannelFromAffinity(ctx, preferredChannelID)
	ctx.Set(channelAffinityStrictContextKey, true)
	ctx.Set("skip_channel_ids", []int{preferredChannelID})

	channel, err := fetchChannelByModelWithSelection(ctx, "gpt-5", currentRealtimeChannelSelection(ctx))
	if err == nil {
		t.Fatal("expected strict affinity retry skip to reject fallback routing")
	}
	if channel != nil {
		t.Fatalf("expected no channel to be returned, got %#v", channel)
	}
	if got, ok := lookupChannelAffinity(ctx, channelAffinityKindRealtime, sessionID); !ok || got != preferredChannelID {
		t.Fatalf("expected retry-local skip not to clear durable affinity, got channel=%d ok=%v", got, ok)
	}
}

func TestFetchChannelByModelWithSelectionWaitsForPreferredCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	originalWaitBudget := config.PreferredChannelWaitMilliseconds
	originalWaitPoll := config.PreferredChannelWaitPollMilliseconds
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
		config.PreferredChannelWaitMilliseconds = originalWaitBudget
		config.PreferredChannelWaitPollMilliseconds = originalWaitPoll
	})

	const (
		fallbackChannelID  = 11
		preferredChannelID = 22
	)

	config.PreferredChannelWaitMilliseconds = 250
	config.PreferredChannelWaitPollMilliseconds = 10
	model.ChannelGroup = buildRealtimeTestChannelGroup(fallbackChannelID, preferredChannelID)
	model.ChannelGroup.Cooldowns.Store(fmt.Sprintf("%d:%s", preferredChannelID, "gpt-5"), time.Now().Unix()+60)

	go func() {
		time.Sleep(50 * time.Millisecond)
		model.ChannelGroup.Cooldowns.Delete(fmt.Sprintf("%d:%s", preferredChannelID, "gpt-5"))
	}()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil)
	ctx.Set("token_group", "default")

	start := time.Now()
	channel, err := fetchChannelByModelWithSelection(ctx, "gpt-5", realtimeChannelSelection{
		preferredChannelID: preferredChannelID,
	})
	if err != nil {
		t.Fatalf("expected selection to succeed, got %v", err)
	}
	if channel == nil || channel.Id != preferredChannelID {
		t.Fatalf("expected preferred channel %d after cooldown wait, got %#v", preferredChannelID, channel)
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Fatalf("expected cooldown wait before selecting preferred channel, got %v", waited)
	}

	meta := currentChannelAffinityLogMeta(ctx)
	if meta["channel_affinity_wait_triggered"] != true {
		t.Fatalf("expected wait metadata to be recorded, got %#v", meta)
	}
	if meta["channel_affinity_wait_canceled"] != false {
		t.Fatalf("expected wait metadata to report no cancellation, got %#v", meta)
	}
}

func TestFetchChannelByModelWithSelectionFallsBackAfterWaitBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	originalWaitBudget := config.PreferredChannelWaitMilliseconds
	originalWaitPoll := config.PreferredChannelWaitPollMilliseconds
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
		config.PreferredChannelWaitMilliseconds = originalWaitBudget
		config.PreferredChannelWaitPollMilliseconds = originalWaitPoll
	})

	const (
		fallbackChannelID  = 11
		preferredChannelID = 22
	)

	config.PreferredChannelWaitMilliseconds = 50
	config.PreferredChannelWaitPollMilliseconds = 10
	model.ChannelGroup = buildRealtimeTestChannelGroup(fallbackChannelID, preferredChannelID)
	model.ChannelGroup.Cooldowns.Store(fmt.Sprintf("%d:%s", preferredChannelID, "gpt-5"), time.Now().Unix()+60)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil)
	ctx.Set("token_group", "default")

	start := time.Now()
	channel, err := fetchChannelByModelWithSelection(ctx, "gpt-5", realtimeChannelSelection{
		preferredChannelID: preferredChannelID,
	})
	if err != nil {
		t.Fatalf("expected fallback selection to succeed, got %v", err)
	}
	if channel == nil || channel.Id != fallbackChannelID {
		t.Fatalf("expected fallback channel %d after wait exhaustion, got %#v", fallbackChannelID, channel)
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Fatalf("expected bounded wait before fallback, got %v", waited)
	}

	meta := currentChannelAffinityLogMeta(ctx)
	if meta["channel_affinity_wait_triggered"] != true {
		t.Fatalf("expected wait metadata to be recorded, got %#v", meta)
	}
	if meta["channel_affinity_wait_exhausted"] != true {
		t.Fatalf("expected wait exhaustion metadata to be recorded, got %#v", meta)
	}
	if meta["channel_affinity_wait_canceled"] != false {
		t.Fatalf("expected wait metadata to report no cancellation, got %#v", meta)
	}
}

func TestFetchChannelByModelWithSelectionStopsWaitingWhenRequestCanceled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	originalWaitBudget := config.PreferredChannelWaitMilliseconds
	originalWaitPoll := config.PreferredChannelWaitPollMilliseconds
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
		config.PreferredChannelWaitMilliseconds = originalWaitBudget
		config.PreferredChannelWaitPollMilliseconds = originalWaitPoll
	})

	const (
		fallbackChannelID  = 11
		preferredChannelID = 22
	)

	config.PreferredChannelWaitMilliseconds = 250
	config.PreferredChannelWaitPollMilliseconds = 100
	model.ChannelGroup = buildRealtimeTestChannelGroup(fallbackChannelID, preferredChannelID)
	model.ChannelGroup.Cooldowns.Store(fmt.Sprintf("%d:%s", preferredChannelID, "gpt-5"), time.Now().Unix()+60)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil).WithContext(reqCtx)
	ctx.Set("token_group", "default")

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	channel, err := fetchChannelByModelWithSelection(ctx, "gpt-5", realtimeChannelSelection{
		preferredChannelID: preferredChannelID,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation to stop waiting, got channel=%#v err=%v", channel, err)
	}
	if channel != nil {
		t.Fatalf("expected no channel to be selected after cancellation, got %#v", channel)
	}
	if waited := time.Since(start); waited >= 150*time.Millisecond {
		t.Fatalf("expected cancellation to stop wait early, got %v", waited)
	}

	meta := currentChannelAffinityLogMeta(ctx)
	if meta["channel_affinity_wait_triggered"] != true {
		t.Fatalf("expected wait metadata to be recorded, got %#v", meta)
	}
	if meta["channel_affinity_wait_canceled"] != true {
		t.Fatalf("expected wait cancellation metadata to be recorded, got %#v", meta)
	}
}

func TestFetchChannelByModelPreservesAvailabilityWhenCompatibleCandidateIsUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name     string
		disabled bool
		cooldown bool
	}{
		{name: "cooldown", cooldown: true},
		{name: "disabled", disabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			channelGroupSnapshot := snapshotChannelGroup()
			t.Cleanup(func() { restoreChannelGroup(channelGroupSnapshot) })

			weight := uint(1)
			incompatible := &model.Channel{Id: 1, Type: config.ChannelTypeAnthropic, Weight: &weight}
			compatible := &model.Channel{Id: 2, Type: config.ChannelTypeOpenAI, Weight: &weight}
			model.ChannelGroup = model.ChannelsChooser{
				Channels: map[int]*model.ChannelChoice{
					1: {Channel: incompatible},
					2: {Channel: compatible, Disable: test.disabled},
				},
				Rule: map[string]map[string][][]int{"default": {"gpt-5": {{1, 2}}}},
			}
			if test.cooldown {
				model.ChannelGroup.Cooldowns.Store("2:gpt-5", time.Now().Add(time.Minute).Unix())
			}

			ctx := newRelayTestContext(nil)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			ctx.Set("token_group", "default")
			capabilityErr := errors.New("request cannot be represented by channel 1")
			setRequestChannelCapability(ctx, func(channel *model.Channel) error {
				if channel.Id == incompatible.Id {
					return capabilityErr
				}
				return nil
			})

			channel, err := fetchChannelByModelWithSelection(ctx, "gpt-5", realtimeChannelSelection{})
			if channel != nil || err == nil {
				t.Fatalf("expected unavailable compatible candidate to leave selection failed, channel=%+v err=%v", channel, err)
			}
			if errors.Is(err, capabilityErr) {
				t.Fatalf("capability rejection masked %s availability failure: %v", test.name, err)
			}
		})
	}
}

func TestFetchChannelByModelReturnsCapabilityOnlyWhenAllCandidatesReject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(channelGroupSnapshot) })

	weight := uint(1)
	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{
			1: {Channel: &model.Channel{Id: 1, Type: config.ChannelTypeAnthropic, Weight: &weight}},
			2: {Channel: &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Id: 2, Type: config.ChannelTypeCustom, Weight: &weight}},
		},
		Rule: map[string]map[string][][]int{"default": {"gpt-5": {{1, 2}}}},
	}
	ctx := newRelayTestContext(nil)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_group", "default")
	capabilityErr := errors.New("request cannot be represented by any candidate")
	setRequestChannelCapability(ctx, func(*model.Channel) error { return capabilityErr })

	channel, err := fetchChannelByModelWithSelection(ctx, "gpt-5", realtimeChannelSelection{})
	if channel != nil || !errors.Is(err, capabilityErr) {
		t.Fatalf("all-candidate capability exhaustion must return the capability error, channel=%+v err=%v", channel, err)
	}
}

func TestFetchChannelByModelContextCancellationDominatesRecordedCapability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(channelGroupSnapshot) })

	weight := uint(1)
	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{
			1: {Channel: &model.Channel{Id: 1, Type: config.ChannelTypeAnthropic, Weight: &weight}},
		},
		Rule: map[string]map[string][][]int{"default": {"gpt-5": {{1}}}},
	}
	requestCtx, cancel := context.WithCancel(context.Background())
	ctx := newRelayTestContext(nil)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestCtx)
	ctx.Set("token_group", "default")
	capabilityErr := errors.New("recorded capability rejection")
	setRequestChannelCapability(ctx, func(*model.Channel) error {
		cancel()
		return capabilityErr
	})

	channel, err := fetchChannelByModelWithSelection(ctx, "gpt-5", realtimeChannelSelection{})
	if channel != nil || !errors.Is(err, context.Canceled) || errors.Is(err, capabilityErr) {
		t.Fatalf("context cancellation must dominate recorded capability, channel=%+v err=%v", channel, err)
	}
}

func TestChannelAffinityRuleMatchesUserAgentRegex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("User-Agent", "Codex/1.2")
	ctx.Request = req

	rule := config.ChannelAffinityRule{
		Enabled:        true,
		Kind:           "responses",
		PathRegex:      "^/v1/responses$",
		UserAgentRegex: "^Codex/",
	}
	if !channelAffinityRuleMatches(ctx, channelAffinityKindResponses, "gpt-5", rule) {
		t.Fatal("expected user-agent regex rule to match request")
	}

	rule.UserAgentRegex = "^OtherClient/"
	if channelAffinityRuleMatches(ctx, channelAffinityKindResponses, "gpt-5", rule) {
		t.Fatal("expected user-agent regex mismatch to reject rule")
	}
}

func TestProviderRedactionMultilineRedactionRoundTrip(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		for _, tail := range []string{"", newline, newline + newline} {
			for _, value := range []string{`"acct-secret"`, "{\n\"nested\":\"acct-secret\"\n}"} {
				lines := []string{"event: error", "id: 7", "data: {", "data: \"account_id\":" + strings.ReplaceAll(value, "\n", newline+"data: "), ": keep comment", "data: ,\"value\":9007199254740993", "data: }"}
				raw := strings.Join(lines, newline) + tail
				safe := redactProviderSSEEvent(raw)
				payload, hasData := commonresponses.SSEDataPayload(safe)
				if !hasData || !json.Valid([]byte(payload)) || strings.Contains(payload, "acct-secret") || !strings.Contains(payload, "9007199254740993") {
					t.Fatalf("多行 SSE 改写破坏 payload: wire=%q payload=%q", safe, payload)
				}
				if !strings.HasSuffix(safe, "data: "+tail) && !strings.HasSuffix(safe, "data: }"+tail) {
					t.Fatalf("SSE 尾部 framing 改变: %q", safe)
				}
				if !strings.Contains(safe, ": keep comment"+newline) || !strings.HasPrefix(safe, "event: error"+newline+"id: 7"+newline) {
					t.Fatalf("非 data 字段改变: %q", safe)
				}
				if again := redactProviderSSEEvent(safe); again != safe {
					t.Fatalf("SSE 重复改写不稳定: %q", again)
				}
			}
		}
	}
}
