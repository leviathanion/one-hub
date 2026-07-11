package relay

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"one-api/common"
	"one-api/common/config"
	ratelimit "one-api/common/limit"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/middleware"
	"one-api/relay/relay_util"
	runtimeaffinity "one-api/runtime/channelaffinity"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/model"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type responsesWSReadResult struct {
	messageType int
	payload     []byte
	err         error
}

type responsesWSFakeUserConn struct {
	reads           chan responsesWSReadResult
	writeErr        error
	closeWriteErr   error
	writeCount      int32
	closeCount      int32
	controlCount    int32
	lastMessageType int32
	lastWrite       atomic.Value
	lastControl     atomic.Value
}

func NewResponsesWSIOBridge(conn *responsesWSFakeUserConn, actor *ResponsesWSSessionActor) *ResponsesWSIOBridge {
	if conn == nil {
		return newResponsesWSBridgeForTest(nil, actor)
	}
	return newResponsesWSBridgeForTest(conn, actor)
}

func (c *responsesWSFakeUserConn) ReadMessage() (int, []byte, error) {
	result := <-c.reads
	return result.messageType, result.payload, result.err
}

func (c *responsesWSFakeUserConn) WriteFrame(messageType int, payload []byte, _ ResponsesWSWriteMode) error {
	if messageType == responsesWSCloseMessageType {
		if c.closeWriteErr != nil {
			return c.closeWriteErr
		}
		code, reason := parseResponsesWSClosePayload(payload)
		c.CloseWithCode(code, reason)
		return nil
	}
	atomic.AddInt32(&c.writeCount, 1)
	atomic.StoreInt32(&c.lastMessageType, int32(messageType))
	if c.writeErr != nil {
		return c.writeErr
	}
	c.lastWrite.Store(string(payload))
	return nil
}

func (c *responsesWSFakeUserConn) CloseWithCode(code int, reason string) {
	atomic.AddInt32(&c.controlCount, 1)
	c.lastControl.Store(string(wsconn.SafeCloseMessage(wsconn.SanitizeWireCloseCode(code), reason)))
}

func (c *responsesWSFakeUserConn) Abort(string) {
	atomic.AddInt32(&c.closeCount, 1)
}

func responsesWSTestClientTextFrame(payload []byte) ResponsesWSEventClientFrame {
	return ResponsesWSEventClientFrame{Frame: responsesws.NewTextFrame(payload)}
}

func responsesWSTestProviderTextFrame(payload []byte) *responsesws.Frame {
	frame := responsesws.NewTextFrame(payload)
	return &frame
}

func responsesWSTestCurrentAttemptID(actor *ResponsesWSSessionActor) string {
	if actor == nil {
		return "attempt-test"
	}
	if actor.turns.pending.attempt != nil && actor.turns.pending.attempt.AttemptID != "" {
		return actor.turns.pending.attempt.AttemptID
	}
	if actor.turns.active.attempt != nil && actor.turns.active.attempt.AttemptID != "" {
		return actor.turns.active.attempt.AttemptID
	}
	return "attempt-test"
}

func responsesWSTestProviderBinaryFrame(payload []byte) *responsesws.Frame {
	frame := responsesws.NewBinaryFrame(payload)
	return &frame
}

func responsesWSTestProviderEventPayload(event ResponsesWSEventProviderDownstream) []byte {
	if event.Frame == nil {
		return nil
	}
	return event.Frame.Payload()
}

func responsesWSTestProviderProjection(events ...responsesws.UpstreamEvent) responsesws.ProviderSettlementLogProjection {
	var projection responsesws.ProviderSettlementLogProjection
	for _, event := range events {
		projection.Observe(responsesws.NewProviderObservation(event))
	}
	return projection
}

func responsesWSTestProviderFrameProjection() responsesws.ProviderSettlementLogProjection {
	return responsesWSTestProviderProjection(responsesws.UpstreamEvent{
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	})
}

func responsesWSTestProviderUsageProjection() responsesws.ProviderSettlementLogProjection {
	return responsesWSTestProviderProjection(responsesws.UpstreamEvent{
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		Usage:        &types.UsageEvent{TotalTokens: 1},
	})
}

func responsesWSTestProviderJournal(events ...responsesws.UpstreamEvent) responsesWSProviderJournal {
	var journal responsesWSProviderJournal
	for _, event := range events {
		journal.AppendLifecycle(event)
	}
	return journal
}

func responsesWSTestProviderFrameJournal() responsesWSProviderJournal {
	return responsesWSTestProviderJournal(responsesws.UpstreamEvent{
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	})
}

func responsesWSTraceHasDetailOrigin(trace ResponsesWSSettlementTrace, origin responsesws.RecvDetailOrigin) bool {
	for _, got := range trace.Input.Diagnostics.DetailOrigins {
		if got == string(origin) {
			return true
		}
	}
	return false
}

func readResponsesWSEvent(t *testing.T, actor *ResponsesWSSessionActor) ResponsesWSEvent {
	t.Helper()
	select {
	case event := <-actor.events:
		return event
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for responses websocket actor event")
		return nil
	}
}

func waitResponsesWSTestCondition(t *testing.T, timeout time.Duration, interval time.Duration, condition func() bool, failureMessage func() string) {
	t.Helper()
	if condition() {
		return
	}
	if interval <= 0 {
		interval = time.Millisecond
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			t.Fatal(failureMessage())
			return
		case <-ticker.C:
			if condition() {
				return
			}
		}
	}
}

func assertResponsesWSErrorPayload(t *testing.T, payload string, status int, code string, messageContains string) {
	t.Helper()
	var event struct {
		Status int `json:"status"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		t.Fatalf("expected websocket error payload to decode, got err=%v payload=%q", err, payload)
	}
	if event.Status != status {
		t.Fatalf("expected status %d, got %d payload=%q", status, event.Status, payload)
	}
	if event.Error.Code != code {
		t.Fatalf("expected code %q, got %q payload=%q", code, event.Error.Code, payload)
	}
	if messageContains != "" && !strings.Contains(event.Error.Message, messageContains) {
		t.Fatalf("expected message to contain %q, got %q payload=%q", messageContains, event.Error.Message, payload)
	}
}

func TestResponsesWSErrorPayloadMarshalFallbackReturnsSystemErrorJSON(t *testing.T) {
	payload := responsesWSMarshalErrorPayload(map[string]any{
		"type": "error",
		"bad":  func() {},
	})
	if len(payload) == 0 {
		t.Fatal("expected fallback payload to be non-empty")
	}
	assertResponsesWSErrorPayload(t, string(payload), http.StatusInternalServerError, "system_error", "system error")
}

func TestResponsesWSOpenParamsUseConnectionScopedUpstreamSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	options := responsesWSOpenParams(c)

	if got := options.upstreamSessionID; !strings.HasPrefix(got, "responses-ws:") {
		t.Fatalf("expected synthetic responses websocket client session id, got %q", got)
	}
	if err := runtimesession.ValidateClientSessionID(options.upstreamSessionID); err != nil {
		t.Fatalf("expected synthetic responses websocket client session id to be valid, got %v", err)
	}
	if options.upstreamSessionID != c.GetString(responsesWSConnectionSessionIDKey) {
		t.Fatal("expected open options to reuse the context connection session id")
	}
	if options.transport != runtimesession.TransportModeResponsesWS {
		t.Fatalf("expected default responses websocket transport to be native, got %q", options.transport)
	}
}

func TestResponsesWSOpenParamsUseExplicitPreviousResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	options := responsesWSOpenParamsWithPreviousResponseID(c, " resp_explicit ", runtimesession.TransportModeResponsesHTTPBridge)

	if options.previousResponseID != "resp_explicit" {
		t.Fatalf("expected explicit previous response id, got %q", options.previousResponseID)
	}
	if options.transport != runtimesession.TransportModeResponsesHTTPBridge {
		t.Fatalf("expected explicit bridge transport, got %q", options.transport)
	}
}

func TestParseResponsesWSTransportMode(t *testing.T) {
	cases := []struct {
		name       string
		other      string
		want       runtimesession.TransportMode
		wantStatus int
		wantCode   string
	}{
		{
			name: "empty defaults native",
			want: runtimesession.TransportModeResponsesWS,
		},
		{
			name:  "native",
			other: `{"responses_ws_transport":"native"}`,
			want:  runtimesession.TransportModeResponsesWS,
		},
		{
			name:  "native normalized",
			other: `{"responses_ws_transport":" Native "}`,
			want:  runtimesession.TransportModeResponsesWS,
		},
		{
			name:  "http bridge",
			other: `{"responses_ws_transport":"http_bridge"}`,
			want:  runtimesession.TransportModeResponsesHTTPBridge,
		},
		{
			name:  "azure http bridge with api version",
			other: `{"api_version":"2024-10-01-preview","responses_ws_transport":"http_bridge"}`,
			want:  runtimesession.TransportModeResponsesHTTPBridge,
		},
		{
			name:  "http bridge normalized",
			other: `{"responses_ws_transport":" HTTP_BRIDGE "}`,
			want:  runtimesession.TransportModeResponsesHTTPBridge,
		},
		{
			name:       "invalid value",
			other:      `{"responses_ws_transport":"auto"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_responses_ws_transport",
		},
		{
			name:       "invalid json",
			other:      `{"responses_ws_transport":`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_responses_ws_transport",
		},
		{
			name:       "non string",
			other:      `{"responses_ws_transport":123}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_responses_ws_transport",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, apiErr := parseResponsesWSTransportMode(&model.Channel{Other: tc.other})
			if tc.wantCode != "" {
				if apiErr == nil || apiErr.StatusCode != tc.wantStatus || openAIErrorCodeString(apiErr.Code, "") != tc.wantCode {
					t.Fatalf("expected %d/%s, got mode=%q err=%+v", tc.wantStatus, tc.wantCode, got, apiErr)
				}
				return
			}
			if apiErr != nil {
				t.Fatalf("expected transport mode parse to succeed, got %v", apiErr)
			}
			if got != tc.want {
				t.Fatalf("expected transport %q, got %q", tc.want, got)
			}
		})
	}
}

func TestOpenResponsesWSUpstreamHTTPBridgeRequiresProviderSupport(t *testing.T) {
	_, apiErr := openResponsesWSUpstreamWithFrame(context.Background(), nil, &relayTestBaseProvider{}, "gpt-5", responsesWSUpstreamOpenParams{
		transport: runtimesession.TransportModeResponsesHTTPBridge,
	}, responsesWSTestOpenFrame(t))
	if apiErr == nil || apiErr.StatusCode != http.StatusUpgradeRequired || openAIErrorCodeString(apiErr.Code, "") != "responses_ws_unsupported_for_channel" {
		t.Fatalf("expected http_bridge without ResponsesWS provider support to return 426 unsupported, got %+v", apiErr)
	}
}

type relayTestResponsesWSProvider struct {
	relayTestBaseProvider
	gotTransport  runtimesession.TransportMode
	gotFirstFrame bool
}

func (p *relayTestResponsesWSProvider) OpenResponsesWS(_ context.Context, req *responsesws.OpenRequest) (responsesws.Upstream, *types.OpenAIErrorWithStatusCode) {
	if req != nil {
		p.gotTransport = req.Transport
		p.gotFirstFrame = req.FirstFrame != nil
	}
	return &responsesWSTestSession{}, nil
}

func TestOpenResponsesWSUpstreamHTTPBridgePassesNormalizedTransportToProvider(t *testing.T) {
	provider := &relayTestResponsesWSProvider{}
	session, apiErr := openResponsesWSUpstreamWithFrame(context.Background(), nil, provider, "gpt-5", responsesWSUpstreamOpenParams{
		transport: runtimesession.TransportModeResponsesHTTPBridge,
	}, responsesWSTestOpenFrame(t))
	if apiErr != nil {
		t.Fatalf("expected provider to receive http_bridge transport, got %v", apiErr)
	}
	if session == nil || provider.gotTransport != runtimesession.TransportModeResponsesHTTPBridge || !provider.gotFirstFrame {
		t.Fatalf("expected http_bridge transport and first frame to reach provider, session=%T transport=%q first_frame=%v", session, provider.gotTransport, provider.gotFirstFrame)
	}
}

func TestOpenResponsesWSUpstreamDoesNotUseLegacyRealtimeOptions(t *testing.T) {
	var legacyCalls int
	provider := &relayTestRealtimeProvider{
		openFn: func(modelName string, options runtimerealtime.RealtimeOpenOptions) (runtimerealtime.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
			legacyCalls++
			t.Fatalf("ResponsesWS open path must not call legacy realtime opener, model=%q options=%+v", modelName, options)
			return nil, nil
		},
	}

	for _, transport := range []runtimesession.TransportMode{
		runtimesession.TransportModeResponsesWS,
		runtimesession.TransportModeResponsesHTTPBridge,
	} {
		t.Run(string(transport), func(t *testing.T) {
			session, apiErr := openResponsesWSUpstreamWithFrame(context.Background(), nil, provider, "gpt-5", responsesWSUpstreamOpenParams{transport: transport}, responsesWSTestOpenFrame(t))
			if session != nil {
				t.Fatalf("expected no session from provider without OpenResponsesWS support, got %T", session)
			}
			if apiErr == nil || apiErr.StatusCode != http.StatusUpgradeRequired || openAIErrorCodeString(apiErr.Code, "") != "responses_ws_unsupported_for_channel" {
				t.Fatalf("expected unsupported ResponsesWS provider error, got %+v", apiErr)
			}
		})
	}
	if legacyCalls != 0 {
		t.Fatalf("expected legacy realtime opener not to be called, got %d calls", legacyCalls)
	}
}

func responsesWSTestOpenFrame(t *testing.T) *responsesws.RawResponsesCreateFrame {
	t.Helper()
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse test response.create frame: %v", err)
	}
	return frame
}

func TestResponsesWSOpenParamsIgnoreRequestSessionIDForProviderSessionReuse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reqA := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	reqA.Header.Set("x-session-id", "shared-client-session")
	wA := httptest.NewRecorder()
	cA, _ := gin.CreateTestContext(wA)
	cA.Request = reqA

	reqB := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	reqB.Header.Set("x-session-id", "shared-client-session")
	wB := httptest.NewRecorder()
	cB, _ := gin.CreateTestContext(wB)
	cB.Request = reqB

	optionsA := responsesWSOpenParams(cA)
	optionsB := responsesWSOpenParams(cB)

	if optionsA.upstreamSessionID == "" || optionsB.upstreamSessionID == "" {
		t.Fatalf("expected both responses websocket opens to use synthetic session ids, got %q and %q", optionsA.upstreamSessionID, optionsB.upstreamSessionID)
	}
	if optionsA.upstreamSessionID == "shared-client-session" || optionsB.upstreamSessionID == "shared-client-session" {
		t.Fatalf("expected request x-session-id not to be used for provider session reuse, got %q and %q", optionsA.upstreamSessionID, optionsB.upstreamSessionID)
	}
	if optionsA.upstreamSessionID == optionsB.upstreamSessionID {
		t.Fatalf("expected separate downstream websocket connections to get different provider session ids, got %q", optionsA.upstreamSessionID)
	}
}

func intSliceContains(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func dialResponsesWSTestManagedConn(t *testing.T, wsURL string) *wsconn.ManagedConn {
	t.Helper()
	conn, err := dialResponsesWSTestManagedConnErr(wsURL)
	if err != nil {
		t.Fatalf("expected websocket connection to upgrade, got %v", err)
	}
	return conn
}

func dialResponsesWSTestManagedConnErr(wsURL string) (*wsconn.ManagedConn, error) {
	return wsconn.DialManaged(context.Background(), wsURL, nil, wsconn.Config{
		Label:        "responses ws test client",
		WriteTimeout: responsesWSTestWriteTimeout(),
	}, wsconn.WithDialSecurityPolicy(wsconn.DialSecurityPolicy{
		AllowInsecureWS: true,
		AllowPrivateIP:  true,
	}))
}

func responsesWSTestWriteTimeout() func() time.Duration {
	timeout := config.RealtimeWebsocketWriteTimeout()
	return func() time.Duration { return timeout }
}

func waitResponsesWSTestManagedClose(t *testing.T, conn *wsconn.ManagedConn) wsconn.CloseInfo {
	t.Helper()
	done := make(chan wsconn.CloseInfo, 1)
	go wsconn.Pump{
		Conn: conn,
		OnClose: func(info wsconn.CloseInfo) {
			done <- info
		},
	}.Run(context.Background())
	select {
	case info := <-done:
		return info
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket close")
		return wsconn.CloseInfo{}
	}
}

type responsesWSTestSession struct {
	abortReason      string
	abortCh          chan string
	abortCount       int32
	preflightErr     error
	preflightCalls   int32
	preflightEventID string
	preflightRequest *types.OpenAIResponsesRequest
}

func (s *responsesWSTestSession) SendClientWithResult(context.Context, responsesws.SendRequest) responsesws.ResponsesWSTransportSendResult {
	return responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAttempted}
}

func (s *responsesWSTestSession) PreflightResponsesWSSend(_ context.Context, eventID string, request *types.OpenAIResponsesRequest) error {
	atomic.AddInt32(&s.preflightCalls, 1)
	s.preflightEventID = eventID
	if request != nil {
		cloned := *request
		s.preflightRequest = &cloned
	} else {
		s.preflightRequest = nil
	}
	return s.preflightErr
}

func (s *responsesWSTestSession) Recv(context.Context) (responsesws.UpstreamEvent, error) {
	return responsesws.UpstreamEvent{}, responsesws.ErrUpstreamClosed
}

func (s *responsesWSTestSession) Detach(string) {}

func (s *responsesWSTestSession) Abort(reason string) {
	atomic.AddInt32(&s.abortCount, 1)
	s.abortReason = reason
	if s.abortCh != nil {
		select {
		case s.abortCh <- reason:
		default:
		}
	}
}

type responsesWSBridgeContinuationTestSession struct {
	responsesWSTestSession
	supports bool
}

func (s *responsesWSBridgeContinuationTestSession) SupportsBridgeContinuationDefault() bool {
	return s != nil && s.supports
}

type responsesWSSendResultTestSession struct {
	result      responsesws.ResponsesWSTransportSendResult
	resultCalls int32
}

func (s *responsesWSSendResultTestSession) SendClientWithResult(context.Context, responsesws.SendRequest) responsesws.ResponsesWSTransportSendResult {
	atomic.AddInt32(&s.resultCalls, 1)
	return s.result
}

func (s *responsesWSSendResultTestSession) Recv(context.Context) (responsesws.UpstreamEvent, error) {
	return responsesws.UpstreamEvent{}, responsesws.ErrUpstreamClosed
}

func (s *responsesWSSendResultTestSession) Abort(string) {}

type responsesWSCaptureSendSession struct {
	result   responsesws.ResponsesWSTransportSendResult
	requests chan responsesws.SendRequest
	calls    int32
}

func (s *responsesWSCaptureSendSession) SendClientWithResult(_ context.Context, req responsesws.SendRequest) responsesws.ResponsesWSTransportSendResult {
	atomic.AddInt32(&s.calls, 1)
	if s.requests != nil {
		s.requests <- req
	}
	if s.result.Status != "" || s.result.Err != nil || s.result.Reason != "" {
		return s.result
	}
	return responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAttempted}
}

func (s *responsesWSCaptureSendSession) Recv(context.Context) (responsesws.UpstreamEvent, error) {
	return responsesws.UpstreamEvent{}, responsesws.ErrUpstreamClosed
}

func (s *responsesWSCaptureSendSession) Abort(string) {}

type responsesWSControlLaneTestSession struct {
	createStarted   chan struct{}
	releaseCreate   chan struct{}
	controlCalled   chan struct{}
	createOnce      sync.Once
	controlOnce     sync.Once
	controlCalls    int32
	controlContexts chan context.Context
	controlRequests chan responsesws.SendRequest
	controlFrames   chan responsesws.Frame
	controlResult   responsesws.ResponsesWSTransportSendResult
}

func (s *responsesWSControlLaneTestSession) SendClientWithResult(context.Context, responsesws.SendRequest) responsesws.ResponsesWSTransportSendResult {
	s.createOnce.Do(func() {
		close(s.createStarted)
	})
	<-s.releaseCreate
	return responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAmbiguous, Err: responsesws.ErrUpstreamClosed}
}

func (s *responsesWSControlLaneTestSession) SendControl(ctx context.Context, req responsesws.SendRequest) responsesws.ResponsesWSTransportSendResult {
	atomic.AddInt32(&s.controlCalls, 1)
	if s.controlContexts != nil {
		s.controlContexts <- ctx
	}
	if s.controlRequests != nil {
		s.controlRequests <- req
	}
	if s.controlFrames != nil {
		s.controlFrames <- req.Frame
	}
	s.controlOnce.Do(func() {
		close(s.controlCalled)
	})
	if s.controlResult.Status != "" || s.controlResult.Err != nil || s.controlResult.Reason != "" {
		return s.controlResult
	}
	return responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAttempted}
}

func (s *responsesWSControlLaneTestSession) Recv(context.Context) (responsesws.UpstreamEvent, error) {
	return responsesws.UpstreamEvent{}, responsesws.ErrUpstreamClosed
}

func (s *responsesWSControlLaneTestSession) Abort(string) {}

type responsesWSRecvResult struct {
	messageType   int
	payload       []byte
	providerClose *responsesws.ProviderClose
	usage         *types.UsageEvent
	attemptID     string
	detailOrigin  responsesws.RecvDetailOrigin
	detailPhase   responsesws.RecvDetailPhase
	err           error
	topErr        error
}

type responsesWSRecvSequenceSession struct {
	responses chan responsesWSRecvResult
	recvCalls int32
}

func (s *responsesWSRecvSequenceSession) SendClientWithResult(context.Context, responsesws.SendRequest) responsesws.ResponsesWSTransportSendResult {
	return responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAttempted}
}

func (s *responsesWSRecvSequenceSession) Recv(ctx context.Context) (responsesws.UpstreamEvent, error) {
	atomic.AddInt32(&s.recvCalls, 1)
	select {
	case result := <-s.responses:
		if result.topErr != nil {
			return responsesws.UpstreamEvent{}, result.topErr
		}
		event := responsesws.UpstreamEvent{
			ProviderClose: result.providerClose,
			Usage:         result.usage,
			AttemptID:     result.attemptID,
			DetailOrigin:  result.detailOrigin,
			DetailPhase:   result.detailPhase,
			Err:           result.err,
		}
		if len(result.payload) > 0 {
			frame := responsesWSFrameFromWireMessage(result.messageType, result.payload)
			event.Frame = &frame
		}
		return event, nil
	case <-ctx.Done():
		return responsesws.UpstreamEvent{}, ctx.Err()
	}
}

func (s *responsesWSRecvSequenceSession) Detach(string) {}
func (s *responsesWSRecvSequenceSession) Abort(string)  {}

type responsesWSTestLease struct {
	releases int32
	lost     chan struct{}
}

func (l *responsesWSTestLease) Release() {
	atomic.AddInt32(&l.releases, 1)
}

func (l *responsesWSTestLease) Lost() <-chan struct{} {
	if l == nil {
		return nil
	}
	return l.lost
}

var (
	responsesWSConnectionAttemptTokenSeq int64 = 91000
	responsesWSTestViperMu               sync.Mutex
)

func nextResponsesWSConnectionAttemptTokenID() int {
	return int(atomic.AddInt64(&responsesWSConnectionAttemptTokenSeq, 1))
}

func setResponsesWSTestViperInt(t *testing.T, key string, value int) {
	t.Helper()
	responsesWSTestViperMu.Lock()
	previous := viper.Get(key)
	viper.Set(key, value)
	responsesWSTestViperMu.Unlock()
	t.Cleanup(func() {
		responsesWSTestViperMu.Lock()
		defer responsesWSTestViperMu.Unlock()
		viper.Set(key, previous)
	})
}

func startResponsesWSTestActor(t *testing.T, actor *ResponsesWSSessionActor) {
	t.Helper()
	actor.Start()
	t.Cleanup(func() {
		actor.close("test_cleanup")
		select {
		case <-actor.Done():
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for responses websocket actor cleanup")
		}
		actor.waitStartedGoroutines()
	})
}

func setResponsesWSTestRedisEnabled(t *testing.T, value bool) {
	t.Helper()
	previous := config.RedisEnabled
	config.RedisEnabled = value
	t.Cleanup(func() {
		config.RedisEnabled = previous
	})
}

func installResponsesWSTestAPILimiter(t *testing.T, rpm int) {
	t.Helper()
	originalAPILimiter := model.GlobalUserGroupRatio.APILimiter
	model.GlobalUserGroupRatio.Lock()
	model.GlobalUserGroupRatio.APILimiter = map[string]ratelimit.RateLimiter{
		"default": ratelimit.NewMemoryLimiter(rpm, rpm, time.Minute, false),
	}
	model.GlobalUserGroupRatio.Unlock()
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.APILimiter = originalAPILimiter
		model.GlobalUserGroupRatio.Unlock()
	})
}

func setupResponsesWSQuotaFixture(t *testing.T, quota int) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}, &model.Channel{}); err != nil {
		t.Fatalf("expected quota settlement schema migration to succeed, got %v", err)
	}
	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
		if sqlDB, dbErr := testDB.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"gpt-5": {
				Model: "gpt-5",
				Type:  model.TimesPriceType,
				Input: 0.1,
			},
		},
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	originalBatchUpdate := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	t.Cleanup(func() {
		config.BatchUpdateEnabled = originalBatchUpdate
		config.RedisEnabled = originalRedisEnabled
	})

	if err := model.DB.Create(&model.User{
		Id:          1,
		Username:    "alice",
		Password:    "password123",
		AccessToken: "access-token-1",
		Quota:       quota,
		Group:       "default",
		Status:      config.UserStatusEnabled,
		Role:        config.RoleCommonUser,
		DisplayName: "Alice",
		CreatedTime: 1,
	}).Error; err != nil {
		t.Fatalf("expected user fixture to persist, got %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id:          1,
		UserId:      1,
		Key:         "token-key-1",
		Name:        "token-alpha",
		RemainQuota: quota,
		Group:       "default",
	}).Error; err != nil {
		t.Fatalf("expected token fixture to persist, got %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Channel{
		Id:          17,
		Type:        config.ChannelTypeOpenAI,
		Name:        "openai-quota",
		Key:         "sk-test",
		Status:      config.ChannelStatusEnabled,
		Models:      "gpt-5",
		Group:       "default",
		CreatedTime: 1,
	}).Error; err != nil {
		t.Fatalf("expected channel fixture to persist, got %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group", "default")
	ctx.Set("group_ratio", 1.0)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("token_group", "default")
	ctx.Set("group", "default")
	ctx.Set("group_ratio", 1.0)
	ctx.Set("channel_id", 17)
	return ctx
}

func preparePreconsumedResponsesWSTestAttempt(t *testing.T, ctx *gin.Context) *ResponsesWSTurnAttempt {
	t.Helper()
	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:           ctx,
		SelectedChannelID: 17,
		BillingModel:      "gpt-5",
		PromptModel:       "gpt-5",
		Request:           &types.OpenAIResponsesRequest{Model: "gpt-5", Input: []types.ChatCompletionMessage{}},
	})
	if apiErr != nil {
		t.Fatalf("expected attempt preparation to succeed, got %v", apiErr)
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		t.Fatalf("expected quota preconsume to succeed, got %v", apiErr)
	}
	attempt.AttemptID = "attempt-" + strings.ReplaceAll(t.Name(), "/", "_")
	return attempt
}

func setupPreconsumedResponsesWSActorAttempt(t *testing.T, quota int, attemptID string) (*gin.Context, *ResponsesWSTurnAttempt) {
	t.Helper()
	ctx := setupResponsesWSQuotaFixture(t, quota)
	configureResponsesWSTokenPricingFloor(t, 100)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	attempt.AttemptID = attemptID
	return ctx, attempt
}

func readResponsesWSQuotaFixture(t *testing.T) (model.User, model.Token) {
	t.Helper()
	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup to succeed, got %v", err)
	}
	return user, token
}

func readResponsesWSConsumeLog(t *testing.T) model.Log {
	t.Helper()
	var log model.Log
	if err := model.DB.Order("id desc").First(&log).Error; err != nil {
		t.Fatalf("expected consume log lookup to succeed, got %v", err)
	}
	return log
}

func readResponsesWSChannelFixture(t *testing.T) model.Channel {
	t.Helper()
	var channel model.Channel
	if err := model.DB.First(&channel, 17).Error; err != nil {
		t.Fatalf("expected channel lookup to succeed, got %v", err)
	}
	return channel
}

func TestResponsesWSCurrentModelNamesSeparatesProviderAndBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("original_model", "gpt-5")
	ctx.Set("new_model", "gpt-5-upstream")
	ctx.Set("billing_original_model", true)

	providerModel, billingModel := responsesWSCurrentModelNames(ctx)
	if providerModel != "gpt-5-upstream" {
		t.Fatalf("expected provider model to use mapped upstream model, got %q", providerModel)
	}
	if billingModel != "gpt-5" {
		t.Fatalf("expected billing model to keep original model, got %q", billingModel)
	}

	ctx.Set("billing_original_model", false)
	_, billingModel = responsesWSCurrentModelNames(ctx)
	if billingModel != "gpt-5-upstream" {
		t.Fatalf("expected billing model to use mapped model when billing_original_model=false, got %q", billingModel)
	}
}

func TestResponsesWSSelectedChannelSnapshotAttachAndClear(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("original_model", "gpt-5")
	ctx.Set("billing_original_model", true)
	snapshot := NewResponsesWSRequestSnapshot(ctx)

	attachResponsesWSSelectedChannelSnapshot(snapshot, &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, PreCost: 42}, "gpt-5-upstream", "gpt-5")
	attached := snapshot.Context()
	if attached.GetInt("channel_id") != 17 || attached.GetInt("channel_type") != config.ChannelTypeOpenAI {
		t.Fatalf("expected selected channel ids in snapshot, got channel_id=%d type=%d", attached.GetInt("channel_id"), attached.GetInt("channel_type"))
	}
	if attached.GetString("new_model") != "gpt-5-upstream" || !attached.GetBool("billing_original_model") {
		t.Fatalf("expected selected model billing state in snapshot, new_model=%q billing_original=%v", attached.GetString("new_model"), attached.GetBool("billing_original_model"))
	}
	selected, ok := attached.Get("responses_ws_selected_channel_snapshot")
	if !ok || selected.(*SelectedChannelSnapshot).PreCost != 42 {
		t.Fatalf("expected selected channel snapshot with pre-cost, got %#v", selected)
	}

	clearResponsesWSSelectedChannelSnapshot(snapshot)
	cleared := snapshot.Context()
	for _, key := range []string{"responses_ws_selected_channel_snapshot", "responses_ws_selected_channel", "channel_id", "channel_type", "new_model", "billing_original_model"} {
		if _, ok := cleared.Get(key); ok {
			t.Fatalf("expected retry cleanup to remove %q", key)
		}
	}
	if cleared.GetString("original_model") != "gpt-5" {
		t.Fatalf("expected retry cleanup to preserve original model, got %q", cleared.GetString("original_model"))
	}
}

func TestResponsesWebSocketConnectionLimitRejectsBeforeUpgradeAndPending(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	setResponsesWSTestRedisEnabled(t, false)
	setResponsesWSTestViperInt(t, "responses_ws.connect_per_credential_per_minute", 1)
	setResponsesWSTestViperInt(t, "responses_ws.pending_per_credential", 1)
	setResponsesWSTestViperInt(t, "responses_ws.first_frame_timeout_ms", 30000)
	installResponsesWSTestAPILimiter(t, 60)
	tokenID := nextResponsesWSConnectionAttemptTokenID()
	firstHandlerDone := make(chan struct{})
	var firstHandlerDoneOnce sync.Once
	var handlerCount int32

	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		c.Set("id", 7)
		c.Set("token_id", tokenID)
		c.Set("group", "default")
		if atomic.AddInt32(&handlerCount, 1) == 1 {
			defer firstHandlerDoneOnce.Do(func() { close(firstHandlerDone) })
		}
		ResponsesWebSocket(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"

	firstConn := dialResponsesWSTestManagedConn(t, wsURL)

	_, err := dialResponsesWSTestManagedConnErr(wsURL)
	if err == nil {
		t.Fatal("expected second websocket connection to be rejected by connection limiter")
	}
	var dialErr *wsconn.DialError
	if !errors.As(err, &dialErr) || dialErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 from connection limiter, got err=%v", err)
	}
	if !strings.Contains(string(dialErr.BodySnippet), "too many responses websocket connection attempts") {
		t.Fatalf("expected connection limiter response before pending acquisition, body=%q", string(dialErr.BodySnippet))
	}
	firstConn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
	select {
	case <-firstHandlerDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first websocket handler to exit")
	}
}

func TestResponsesWebSocketRejectsNonUpgradeBeforeConnectionLimiter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	setResponsesWSTestRedisEnabled(t, false)
	setResponsesWSTestViperInt(t, "responses_ws.connect_per_credential_per_minute", 1)
	installResponsesWSTestAPILimiter(t, 60)

	tokenID := nextResponsesWSConnectionAttemptTokenID()
	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		c.Set("id", 7)
		c.Set("token_id", tokenID)
		c.Set("group", "default")
		ResponsesWebSocket(c)
	})

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("expected non-websocket request to be rejected with 426, got %d body=%q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "websocket_upgrade_required") {
		t.Fatalf("expected websocket upgrade error, got %q", recorder.Body.String())
	}

	ctx := setupResponsesWSQuotaFixture(t, 10000)
	ctx.Set("token_id", tokenID)
	if apiErr := middleware.AllowResponsesWSConnectionAttempt(ctx); apiErr != nil {
		t.Fatalf("expected non-upgrade rejection not to consume connection limiter, got %v", apiErr)
	}
}

func TestResponsesWSFrameDiagnosticsSanitizesClientMetadata(t *testing.T) {
	parentThreadID := "thread-secret-sk-proj-abcdefghijklmnopqrstuvwxyz"
	raw, err := json.Marshal(map[string]any{
		"type":  "response.create",
		"model": "gpt-5\nmini",
		"client_metadata": map[string]string{
			"x-openai-subagent":        "Bearer abcdefghij.klmnopqrst.uvwxyzabcd",
			"x-codex-parent-thread-id": parentThreadID,
			"x-codex-turn-metadata":    `{"request_kind":"kind\nsk-proj-abcdefghijklmnopqrstuvwxyz"}`,
		},
	})
	if err != nil {
		t.Fatalf("marshal diagnostic payload: %v", err)
	}

	diag := responsesWSFrameDiagnosticsFromRaw(raw)
	if strings.ContainsAny(diag.Model+diag.TurnRequestKind, "\r\n\t") {
		t.Fatalf("expected diagnostic values to escape control characters, got %+v", diag)
	}
	if !diag.SubagentPresent || diag.SubagentBytes == 0 || diag.SubagentHash == "" {
		t.Fatalf("expected subagent presence, length, and hash only, got %+v", diag)
	}
	if !diag.ParentThreadPresent || diag.ParentThreadBytes != len(parentThreadID) || diag.ParentThreadHash == "" {
		t.Fatalf("expected parent thread presence, length, and hash only, got %+v", diag)
	}
	rendered := fmt.Sprintf("%+v", diag)
	if strings.Contains(rendered, "Bearer") || strings.Contains(rendered, parentThreadID) || strings.Contains(rendered, "sk-proj-abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("expected client metadata secrets to be redacted from diagnostics, got %+v", diag)
	}
	if !strings.Contains(diag.TurnRequestKind, "[redacted]") {
		t.Fatalf("expected request kind secret to be redacted, got %+v", diag)
	}
}

func TestResponsesWSDiagnosticHookLogsCorrelationIDs(t *testing.T) {
	core, observedLogs := observer.New(zapcore.ErrorLevel)
	originalLogger := logger.Logger
	logger.Logger = zap.New(core)
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req = req.WithContext(context.WithValue(req.Context(), logger.RequestIdKey, "req-diag-1"))
	ctx.Request = req
	ctx.Set(logger.RequestIdKey, "req-diag-1")
	ctx.Set(responsesWSConnectionSessionIDKey, "responses-ws-session-1")
	ctx.Set("id", 42)
	ctx.Set("token_id", 77)

	hook := responsesWSDiagnosticHook(ctx)
	hook(responsesws.Diagnostic{
		Code:        "adapter_panic",
		Provider:    "codex",
		ChannelID:   13,
		Transport:   "responses_ws",
		Phase:       responsesws.RecvDetailPhaseHandleProviderFrame,
		PanicClass:  "string",
		StackHash:   "stackhash",
		DetailError: "adapter panic",
	})

	logs := observedLogs.All()
	if len(logs) != 1 {
		t.Fatalf("expected one diagnostic log, got %d", len(logs))
	}
	message := logs[0].Message
	for _, want := range []string{
		"request_id=req-diag-1",
		"connection_session_id=responses-ws-session-1",
		"user_id=42",
		"token_id=77",
		"provider=codex",
		"channel_id=13",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("expected diagnostic log to contain %q, got %q", want, message)
		}
	}
}

type responsesWSPanicAfterHookContext struct {
	context.Context
	panicOnValue bool
}

func (c *responsesWSPanicAfterHookContext) Value(key any) any {
	if c.panicOnValue {
		panic("request context used after diagnostic hook creation")
	}
	return c.Context.Value(key)
}

func TestResponsesWSDiagnosticHookDetachesRequestContext(t *testing.T) {
	core, observedLogs := observer.New(zapcore.ErrorLevel)
	originalLogger := logger.Logger
	logger.Logger = zap.New(core)
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestCtx := &responsesWSPanicAfterHookContext{
		Context: context.WithValue(context.Background(), logger.RequestIdKey, "req-detached-1"),
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Request = req.WithContext(requestCtx)

	hook := responsesWSDiagnosticHook(ctx)
	requestCtx.panicOnValue = true

	hook(responsesws.Diagnostic{
		Code:      "adapter_panic",
		Provider:  "codex",
		ChannelID: 13,
		Transport: "responses_ws",
		Phase:     responsesws.RecvDetailPhaseHandleProviderFrame,
	})

	logs := observedLogs.All()
	if len(logs) != 1 {
		t.Fatalf("expected one diagnostic log, got %d", len(logs))
	}
	if !strings.Contains(logs[0].Message, "req-detached-1") {
		t.Fatalf("expected detached diagnostic log to keep request id, got %q", logs[0].Message)
	}
}

func TestResponsesWebSocketOversizedFirstFrameClosesOrReturnsInvalidEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	setResponsesWSTestRedisEnabled(t, false)
	setResponsesWSTestViperInt(t, "realtime.websocket_read_limit", 64)
	setResponsesWSTestViperInt(t, "responses_ws.connect_per_credential_per_minute", -1)
	setResponsesWSTestViperInt(t, "responses_ws.first_frame_timeout_ms", 30000)
	installResponsesWSTestAPILimiter(t, 60)

	router := gin.New()
	handlerDone := make(chan struct{})
	router.GET("/v1/responses", func(c *gin.Context) {
		defer close(handlerDone)
		c.Set("id", 7)
		c.Set("token_id", nextResponsesWSConnectionAttemptTokenID())
		c.Set("group", "default")
		ResponsesWebSocket(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"

	conn := dialResponsesWSTestManagedConn(t, wsURL)
	defer func() {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
		select {
		case <-handlerDone:
		case <-time.After(time.Second):
			t.Errorf("timed out waiting for oversized websocket handler to exit")
		}
	}()

	if err := conn.WriteMessage(wsconn.TextMessage, []byte(strings.Repeat("x", 1024))); err != nil {
		t.Fatalf("expected oversized first frame write to reach server, got %v", err)
	}
	_, payload, err := conn.ReadInitial(context.Background())
	if err == nil {
		if !strings.Contains(string(payload), "invalid_event") || !strings.Contains(string(payload), "frame is too large or invalid") {
			t.Fatalf("expected oversized frame guidance payload, got %q", payload)
		}
		return
	}
	var closeErr *wsconn.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != wsconn.CloseMessageTooBig {
		t.Fatalf("expected oversized frame close or invalid_event payload, err=%v payload=%q", err, payload)
	}
}

func TestResponsesWebSocketFirstFrameTimeoutReturnsSafeInvalidEventMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	setResponsesWSTestRedisEnabled(t, false)
	setResponsesWSTestViperInt(t, "responses_ws.connect_per_credential_per_minute", -1)
	setResponsesWSTestViperInt(t, "responses_ws.first_frame_timeout_ms", 10)
	installResponsesWSTestAPILimiter(t, 60)

	router := gin.New()
	handlerDone := make(chan struct{})
	router.GET("/v1/responses", func(c *gin.Context) {
		defer close(handlerDone)
		c.Set("id", 7)
		c.Set("token_id", nextResponsesWSConnectionAttemptTokenID())
		c.Set("group", "default")
		ResponsesWebSocket(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"

	conn := dialResponsesWSTestManagedConn(t, wsURL)
	defer conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})

	_, payload, err := conn.ReadInitial(context.Background())
	if err != nil {
		t.Fatalf("expected first-frame timeout payload before close, got err=%v", err)
	}
	got := string(payload)
	assertResponsesWSErrorPayload(t, got, http.StatusBadRequest, "invalid_event", "timeout waiting for first websocket frame")
	if strings.Contains(got, "tcp") || strings.Contains(got, "127.0.0.1") || strings.Contains(got, "172.") {
		t.Fatalf("expected safe timeout message without socket details, got %q", got)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first-frame timeout handler to exit")
	}
}

func TestResponsesWebSocketBinaryFirstFrameClosesUnsupportedData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	setResponsesWSTestRedisEnabled(t, false)
	setResponsesWSTestViperInt(t, "responses_ws.connect_per_credential_per_minute", -1)
	setResponsesWSTestViperInt(t, "responses_ws.first_frame_timeout_ms", 30000)
	installResponsesWSTestAPILimiter(t, 60)

	router := gin.New()
	handlerDone := make(chan struct{})
	router.GET("/v1/responses", func(c *gin.Context) {
		defer close(handlerDone)
		c.Set("id", 7)
		c.Set("token_id", nextResponsesWSConnectionAttemptTokenID())
		c.Set("group", "default")
		ResponsesWebSocket(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"

	conn := dialResponsesWSTestManagedConn(t, wsURL)
	defer conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})

	if err := conn.WriteMessage(wsconn.BinaryMessage, []byte{1, 2, 3}); err != nil {
		t.Fatalf("expected binary first frame write to reach server, got %v", err)
	}
	closeInfo := waitResponsesWSTestManagedClose(t, conn)
	if closeInfo.Code != wsconn.CloseUnsupportedData || closeInfo.Reason != "text_only" {
		t.Fatalf("expected unsupported-data close with text_only reason, got %+v", closeInfo)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for binary first-frame handler to exit")
	}
}

func TestResponsesWebSocketInvalidJSONFirstFrameWritesErrorThenPolicyClose(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	setResponsesWSTestRedisEnabled(t, false)
	setResponsesWSTestViperInt(t, "responses_ws.connect_per_credential_per_minute", -1)
	setResponsesWSTestViperInt(t, "responses_ws.first_frame_timeout_ms", 30000)
	installResponsesWSTestAPILimiter(t, 60)

	router := gin.New()
	handlerDone := make(chan struct{})
	router.GET("/v1/responses", func(c *gin.Context) {
		defer close(handlerDone)
		c.Set("id", 7)
		c.Set("token_id", nextResponsesWSConnectionAttemptTokenID())
		c.Set("group", "default")
		ResponsesWebSocket(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"

	conn := dialResponsesWSTestManagedConn(t, wsURL)
	defer conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})

	if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":`)); err != nil {
		t.Fatalf("expected invalid JSON first frame write to reach server, got %v", err)
	}
	_, payload, err := conn.ReadInitial(context.Background())
	if err != nil {
		t.Fatalf("expected invalid_response_create payload before close, got err=%v", err)
	}
	assertResponsesWSErrorPayload(t, string(payload), http.StatusBadRequest, responsesWSErrorCodeInvalidResponseCreate, responsesWSMessageInvalidResponseCreate)
	closeInfo := waitResponsesWSTestManagedClose(t, conn)
	if closeInfo.Code != wsconn.ClosePolicyViolation || closeInfo.Reason != responsesWSErrorCodeInvalidResponseCreate {
		t.Fatalf("expected policy-violation close with invalid_response_create reason, got %+v", closeInfo)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for invalid JSON first-frame handler to exit")
	}
}

func TestResponsesWSConnectionAttemptConsumedOncePerSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	setResponsesWSTestRedisEnabled(t, false)
	setResponsesWSTestViperInt(t, "responses_ws.connect_per_credential_per_minute", 2)

	ctx := setupResponsesWSQuotaFixture(t, 10000)
	tokenID := nextResponsesWSConnectionAttemptTokenID()
	ctx.Set("token_id", tokenID)
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id:          tokenID,
		UserId:      1,
		Key:         fmt.Sprintf("token-key-%d", tokenID),
		Name:        "token-connection-attempt",
		RemainQuota: 10000,
		Group:       "default",
	}).Error; err != nil {
		t.Fatalf("expected unique token fixture to persist, got %v", err)
	}
	for i := 0; i < 30; i++ {
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		attempt.RollbackBeforeLocalWriteOK("test_cleanup")
	}

	if apiErr := middleware.AllowResponsesWSConnectionAttempt(ctx); apiErr != nil {
		t.Fatalf("expected first session connection attempt to pass, got %v", apiErr)
	}
	if apiErr := middleware.AllowResponsesWSConnectionAttempt(ctx); apiErr != nil {
		t.Fatalf("expected second session connection attempt to pass, got %v", apiErr)
	}
	if apiErr := middleware.AllowResponsesWSConnectionAttempt(ctx); apiErr == nil {
		t.Fatal("expected third session connection attempt to be limited")
	}
}

func TestResponsesWSFirstTurnSetupSkipsOpenAfterClientClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group", "default")
	ctx.Set("group_ratio", 1.0)
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"gpt-5": {
			Model:  "gpt-5",
			Type:   model.TokensPriceType,
			Input:  1,
			Output: 1,
		},
	}}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse first frame: %v", err)
	}

	originalOpen := openAndPrimeResponsesWSSessionForActor
	var openCalls int32
	openAndPrimeResponsesWSSessionForActor = func(context.Context, *gin.Context, *responsesws.RawResponsesCreateFrame, *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
		atomic.AddInt32(&openCalls, 1)
		return nil, common.StringErrorWrapperLocal("unexpected open", "unexpected_open", http.StatusInternalServerError)
	}
	t.Cleanup(func() {
		openAndPrimeResponsesWSSessionForActor = originalOpen
	})

	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(nil, actor))
	lease := &responsesWSTestLease{}
	actor.markClientClosed(errors.New("client closed"))
	actor.handleFirstTurnSetup(ResponsesWSEventFirstTurnSetup{Frame: frame, PendingLease: lease})

	if got := atomic.LoadInt32(&openCalls); got != 0 {
		t.Fatalf("expected client close before setup to skip upstream open, got %d calls", got)
	}
	if got := atomic.LoadInt32(&lease.releases); got != 1 {
		t.Fatalf("expected pending lease to be released once, got %d", got)
	}
	if !actor.closing.closed.Load() {
		t.Fatalf("expected actor to close after client close")
	}
}

func TestResponsesWSProviderPayloadKeepsCodexCreateModelTopLevel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("channel_type", config.ChannelTypeCodex)

	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","event_id":"evt_codex","model":"gpt-5","input":"hi","generate":true,"unknown_number":12345678901234567890}`))
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	request := frame.Projection

	payload, err := responsesWSProviderPayload(ctx, frame, &request, "gpt-5-mini")
	if err != nil {
		t.Fatalf("build provider payload: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decode provider payload: %v", err)
	}
	if string(got["model"]) != `"gpt-5-mini"` {
		t.Fatalf("expected top-level mapped model, got %s", got["model"])
	}
	if _, exists := got["response"]; exists {
		t.Fatalf("did not expect Codex WS payload to nest response fields: %s", payload)
	}
	if string(got["event_id"]) != `"evt_codex"` || string(got["generate"]) != `true` {
		t.Fatalf("expected event_id and unknown fields to stay top-level, got %s", payload)
	}
	if string(got["unknown_number"]) != `12345678901234567890` {
		t.Fatalf("expected raw numeric field to be preserved, got %s", got["unknown_number"])
	}
}

func TestResponsesWSBridgeDefaultPreviousResponseIDRequiresCapability(t *testing.T) {
	actor := NewResponsesWSSessionActor(nil)
	actor.turns.history.lastFinal = &types.OpenAIResponsesResponses{ID: "resp_success"}

	bridgeSession := responsesws.NewBridgeSession(nil, responsesws.BridgeSessionOptions{})
	if got := actor.bridgeDefaultPreviousResponseID(bridgeSession); got != "resp_success" {
		t.Fatalf("expected bridge session to use latest successful response id, got %q", got)
	}
	capableSession := &responsesWSBridgeContinuationTestSession{supports: true}
	if got := actor.bridgeDefaultPreviousResponseID(capableSession); got != "resp_success" {
		t.Fatalf("expected capable bridge session to use latest successful response id, got %q", got)
	}
	if got := actor.bridgeDefaultPreviousResponseID(&responsesWSTestSession{}); got != "" {
		t.Fatalf("expected non-capable session not to use latest response id, got %q", got)
	}
	disabledSession := &responsesWSBridgeContinuationTestSession{supports: false}
	if got := actor.bridgeDefaultPreviousResponseID(disabledSession); got != "" {
		t.Fatalf("expected disabled capable session not to use latest response id, got %q", got)
	}
	actor.turns.history.lastFinal = nil
	if got := actor.bridgeDefaultPreviousResponseID(bridgeSession); got != "" {
		t.Fatalf("expected missing latest response to skip bridge default, got %q", got)
	}
}

func TestResponsesWSFirstTurnOpenResultAfterClientCloseIsAbortedNotAdopted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse first frame: %v", err)
	}

	session := &responsesWSTestSession{abortCh: make(chan string, 1)}
	openStarted := make(chan struct{})
	releaseOpen := make(chan struct{})
	originalOpen := openAndPrimeResponsesWSSessionForActor
	openAndPrimeResponsesWSSessionForActor = func(context.Context, *gin.Context, *responsesws.RawResponsesCreateFrame, *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
		close(openStarted)
		<-releaseOpen
		return &responsesWSOpenResult{
			Session:       session,
			ProviderModel: "gpt-5",
			BillingModel:  "gpt-5",
			Channel:       &model.Channel{Id: 17},
		}, nil
	}
	t.Cleanup(func() {
		openAndPrimeResponsesWSSessionForActor = originalOpen
	})

	actor := NewResponsesWSSessionActor(ctx)
	actor.ReserveFirstTurnOpening(frame)
	startResponsesWSTestActor(t, actor)
	actor.startFirstTurnOpenWorker(actor.turns.opening.openingID, frame)

	select {
	case <-openStarted:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for first-turn open worker")
	}

	actor.markClientClosed(errors.New("client closed"))
	actor.Post(ResponsesWSEventClientClosed{Err: errors.New("client closed")})
	close(releaseOpen)

	select {
	case reason := <-session.abortCh:
		if reason == "" {
			t.Fatalf("expected opened session to be aborted")
		}
	case <-time.After(time.Second):
		t.Fatalf("expected opened session to be aborted after client close")
	}
	select {
	case <-actor.Done():
	case <-time.After(time.Second):
		t.Fatalf("expected actor to close after client close")
	}
	if actor.upstream.session != nil || actor.upstream.sessionGeneration != "" {
		t.Fatalf("expected closed actor not to adopt opened session, upstreamSessionGeneration=%q session=%#v", actor.upstream.sessionGeneration, actor.upstream.session)
	}
}

func TestResponsesWSFirstTurnOpenResultSuccessAdoptsSnapshotAndStartsSend(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := setupResponsesWSQuotaFixture(t, 10000)
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":[]}`))
	if err != nil {
		t.Fatalf("parse first frame: %v", err)
	}

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSSendResultTestSession{
		result: responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAttempted},
	}
	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(conn, actor)
	actor.SetBridge(bridge)
	t.Cleanup(func() {
		actor.finish()
		bridge.Close()
	})
	actor.ReserveFirstTurnOpening(frame)

	eventSnapshot := NewResponsesWSRequestSnapshot(ctx)
	eventSnapshot.Set("snapshot_marker", []string{"event"})
	adoptedCh := make(chan bool, 1)
	actor.handleFirstTurnOpenResult(ResponsesWSEventFirstTurnOpenResult{
		OpeningID: actor.turns.opening.openingID,
		Snapshot:  eventSnapshot,
		OpenResult: &responsesWSOpenResult{
			Session:       session,
			ProviderModel: "gpt-5",
			BillingModel:  "gpt-5",
			Channel:       &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Name: "openai", Models: "gpt-5", Group: "default"},
		},
		Adopted: adoptedCh,
	})

	if adopted := <-adoptedCh; !adopted {
		t.Fatal("expected first-turn open result to be adopted")
	}
	if actor.snapshot.snapshot == eventSnapshot {
		t.Fatal("expected actor to clone event snapshot instead of retaining caller-owned pointer")
	}
	if actor.upstream.session != session || actor.upstream.channelID != 17 || actor.upstream.sessionGeneration == "" || !actor.upstream.recvArmed {
		t.Fatalf("expected upstream session to be attached and recv pump armed, session=%T channel=%d generation=%q armed=%v", actor.upstream.session, actor.upstream.channelID, actor.upstream.sessionGeneration, actor.upstream.recvArmed)
	}
	if actor.turns.pending.attempt == nil || actor.turns.pending.attempt.Session != session || actor.turns.pending.attempt.SelectedChannelID != 17 {
		t.Fatalf("expected first turn attempt to be prepared, pending=%+v", actor.turns.pending.attempt)
	}
	if actor.turns.pending.phase != responsesWSPendingTurnSend || actor.state != responsesWSStatePendingSend {
		t.Fatalf("expected first turn to enter send phase, phase=%v state=%v", actor.turns.pending.phase, actor.state)
	}
	attached := actor.snapshotClone()
	rawSelected, ok := attached.Get("responses_ws_selected_channel_snapshot")
	selected, okSelected := rawSelected.(*SelectedChannelSnapshot)
	if !ok || !okSelected || selected.ChannelID != 17 || selected.ProviderModel != "gpt-5" || selected.BillingModel != "gpt-5" {
		t.Fatalf("expected selected channel snapshot to be attached, got %#v", rawSelected)
	}
	waitResponsesWSTestCondition(t, time.Second, time.Millisecond, func() bool {
		return atomic.LoadInt32(&session.resultCalls) == 1
	}, func() string {
		return fmt.Sprintf("expected first-turn send worker to call provider once, got %d", atomic.LoadInt32(&session.resultCalls))
	})
}

func TestResponsesWSFirstTurnOpenResultUnsupportedWritesFallbackAndCloseControl(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse first frame: %v", err)
	}

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.ReserveFirstTurnOpening(frame)
	adoptedCh := make(chan bool, 1)

	actor.handleFirstTurnOpenResult(ResponsesWSEventFirstTurnOpenResult{
		OpeningID: actor.turns.opening.openingID,
		Err:       common.StringErrorWrapperLocal("unsupported", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired),
		Adopted:   adoptedCh,
	})

	if adopted := <-adoptedCh; !adopted {
		t.Fatal("expected unsupported open error to be adopted by actor")
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected unsupported open error to close actor")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_unsupported_for_channel") {
		t.Fatalf("expected fallback payload, got %q", got)
	}
	if atomic.LoadInt32(&conn.controlCount) != 1 {
		t.Fatalf("expected exactly one fallback close control, got %d", atomic.LoadInt32(&conn.controlCount))
	}
	if got, _ := conn.lastControl.Load().(string); !strings.Contains(got, "responses_ws_unsupported_for_channel") {
		t.Fatalf("expected fallback close reason, got %q", got)
	}
}

func TestResponsesWSFirstTurnOpenResultOpenAIErrorWritesErrorWithoutFallbackClose(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse first frame: %v", err)
	}

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.ReserveFirstTurnOpening(frame)
	adoptedCh := make(chan bool, 1)

	actor.handleFirstTurnOpenResult(ResponsesWSEventFirstTurnOpenResult{
		OpeningID: actor.turns.opening.openingID,
		Err: &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{
				Type:    "rate_limit_error",
				Code:    "rate_limit_exceeded",
				Message: "provider busy",
			},
			StatusCode: http.StatusTooManyRequests,
		},
		Adopted: adoptedCh,
	})

	if adopted := <-adoptedCh; !adopted {
		t.Fatal("expected open error to be adopted by actor")
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected open error to close actor")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "rate_limit_exceeded") || strings.Contains(got, "responses_ws_unsupported_for_channel") {
		t.Fatalf("expected OpenAI error payload without fallback code, got %q", got)
	}
	if got, _ := conn.lastControl.Load().(string); strings.Contains(got, "responses_ws_unsupported_for_channel") {
		t.Fatalf("expected ordinary close control not fallback close, got %q", got)
	}
}

func TestResponsesWSFirstTurnOpenResultInvalidOpenResultClosesWithChannelError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name       string
		openResult *responsesWSOpenResult
	}{
		{name: "nil open result"},
		{name: "nil session", openResult: &responsesWSOpenResult{Channel: &model.Channel{Id: 17}}},
		{name: "nil channel", openResult: &responsesWSOpenResult{Session: &responsesWSTestSession{}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
			if err != nil {
				t.Fatalf("parse first frame: %v", err)
			}

			conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
			actor := NewResponsesWSSessionActor(ctx)
			actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
			actor.ReserveFirstTurnOpening(frame)
			adoptedCh := make(chan bool, 1)

			actor.handleFirstTurnOpenResult(ResponsesWSEventFirstTurnOpenResult{
				OpeningID:  actor.turns.opening.openingID,
				OpenResult: tc.openResult,
				Adopted:    adoptedCh,
			})

			if adopted := <-adoptedCh; !adopted {
				t.Fatal("expected invalid open result to be adopted and cleaned up by actor")
			}
			if !actor.closing.closed.Load() {
				t.Fatal("expected invalid open result to close actor")
			}
			if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "channel_error") {
				t.Fatalf("expected safe channel_error payload, got %q", got)
			}
		})
	}
}

func TestResponsesWSActiveLeaseLossPostsTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	lease := &responsesWSTestLease{lost: make(chan struct{})}
	actor.setActiveLease(lease)
	close(lease.lost)

	event := readResponsesWSEvent(t, actor)
	timeout, ok := event.(ResponsesWSEventTimeout)
	if !ok {
		t.Fatalf("expected active lease loss to post a timeout event, got %T", event)
	}
	if timeout.Reason != "responses_ws_active_lease_lost" {
		t.Fatalf("expected active lease timeout reason, got %+v", timeout)
	}
}

func TestResponsesWSFirstTurnAdmissionRejectsBeforeUpstreamOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()

	originalAPILimiter := model.GlobalUserGroupRatio.APILimiter
	model.GlobalUserGroupRatio.Lock()
	model.GlobalUserGroupRatio.APILimiter = map[string]ratelimit.RateLimiter{}
	model.GlobalUserGroupRatio.Unlock()
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.APILimiter = originalAPILimiter
		model.GlobalUserGroupRatio.Unlock()
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("id", 7)
	ctx.Set("token_id", 101)
	ctx.Set("group_ratio", 1.0)

	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse first frame: %v", err)
	}

	originalOpen := openAndPrimeResponsesWSSessionForActor
	openCalled := make(chan struct{}, 1)
	openAndPrimeResponsesWSSessionForActor = func(context.Context, *gin.Context, *responsesws.RawResponsesCreateFrame, *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
		select {
		case openCalled <- struct{}{}:
		default:
		}
		return nil, nil
	}
	t.Cleanup(func() {
		openAndPrimeResponsesWSSessionForActor = originalOpen
	})

	actor := NewResponsesWSSessionActor(ctx)
	actor.handleFirstTurnSetup(ResponsesWSEventFirstTurnSetup{
		Frame:        frame,
		PendingLease: &responsesWSTestLease{},
	})

	select {
	case <-openCalled:
		t.Fatal("expected RPM rejection before upstream open")
	case <-time.After(100 * time.Millisecond):
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected actor to close on RPM rejection")
	}
}

func TestResponsesWSFirstTurnActiveLeaseRejectsBeforeRPM(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	setResponsesWSTestRedisEnabled(t, false)
	setResponsesWSTestViperInt(t, "responses_ws.active_per_credential", 1)
	setResponsesWSTestViperInt(t, "responses_ws.active_per_group", -1)
	setResponsesWSTestViperInt(t, "responses_ws.active_global", -1)
	installResponsesWSTestAPILimiter(t, 1)

	tokenID := nextResponsesWSConnectionAttemptTokenID()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("id", 7)
	ctx.Set("token_id", tokenID)
	ctx.Set("group", "default")
	ctx.Set("group_ratio", 1.0)

	heldLease, apiErr := middleware.AcquireResponsesWSActiveLease(ctx)
	if apiErr != nil {
		t.Fatalf("expected fixture active lease to be acquired, got %v", apiErr)
	}
	defer heldLease.Release()

	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse first frame: %v", err)
	}

	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(&responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}, actor))
	actor.handleFirstTurnSetup(ResponsesWSEventFirstTurnSetup{
		Frame:        frame,
		PendingLease: &responsesWSTestLease{},
	})
	if !actor.closing.closed.Load() {
		t.Fatal("expected actor to close on active lease rejection")
	}

	heldLease.Release()
	if apiErr := middleware.AllowCurrentUserRequest(ctx); apiErr != nil {
		t.Fatalf("expected RPM budget to remain available after active lease rejection, got %v", apiErr)
	}
}

func TestResponsesWSActorCloseReleasesActiveLeaseOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	lease := &responsesWSTestLease{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.lease.activeLease = lease
	actor.close("first_close")
	actor.close("second_close")

	if got := atomic.LoadInt32(&lease.releases); got != 1 {
		t.Fatalf("expected active lease to be released once, got %d", got)
	}
}

func TestResponsesWSClientCloseBeforeOpenReleasesActiveLease(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	lease := &responsesWSTestLease{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.lease.activeLease = lease
	actor.handleClientClosed(errors.New("client closed"))

	if got := atomic.LoadInt32(&lease.releases); got != 1 {
		t.Fatalf("expected client close cleanup to release active lease, got %d", got)
	}
}

func TestResponsesWSExpectedClientDisconnectErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "codex exit abnormal close eof",
			err:  &wsconn.CloseError{Code: wsconn.CloseAbnormalClosure, Reason: "unexpected EOF"},
			want: true,
		},
		{
			name: "normal close",
			err:  &wsconn.CloseError{Code: wsconn.CloseNormalClosure, Reason: "bye"},
			want: true,
		},
		{
			name: "managed normal close",
			err:  &wsconn.CloseError{Code: wsconn.CloseNormalClosure, Reason: "bye"},
			want: true,
		},
		{
			name: "raw unexpected eof",
			err:  io.ErrUnexpectedEOF,
			want: true,
		},
		{
			name: "message too big remains visible",
			err:  &wsconn.CloseError{Code: wsconn.CloseMessageTooBig, Reason: "too large"},
			want: false,
		},
		{
			name: "application read error remains visible",
			err:  errors.New("frame decode failed"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isResponsesWSExpectedClientDisconnectError(tt.err); got != tt.want {
				t.Fatalf("expected classification %v, got %v for %v", tt.want, got, tt.err)
			}
		})
	}
}

func TestResponsesWSFirstTurnFailureAfterAttachAbortsSessionOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	installResponsesWSTestAPILimiter(t, 60)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group", "default")
	ctx.Set("group_ratio", 1.0)
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"gpt-5": {
			Model:  "gpt-5",
			Type:   model.TokensPriceType,
			Input:  1,
			Output: 1,
		},
	}}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse first frame: %v", err)
	}

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.ReserveFirstTurnOpening(frame)

	actor.prepareAndSendFirstTurn(&responsesWSOpenResult{
		Session:      session,
		BillingModel: "gpt-5",
		Channel:      &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, PreCost: config.PreContNotAll},
		Candidate:    &ResponsesTurnAffinity{},
	})

	if got := atomic.LoadInt32(&session.abortCount); got != 1 {
		t.Fatalf("expected attached first-turn failure to abort session once, got %d", got)
	}
	if session.abortReason != "rewrite_failed" {
		t.Fatalf("expected actor close to own abort reason, got %q", session.abortReason)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected actor to close after first-turn rewrite failure")
	}
	got, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, got, http.StatusInternalServerError, "responses_ws_payload_rewrite_failed", "internal payload rewrite failed")
}

func TestResponsesWSFirstTurnRetryOpenDoesNotBlockActorLoop(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse first frame: %v", err)
	}

	openStarted := make(chan struct{})
	originalOpen := openAndPrimeResponsesWSSessionForActor
	openAndPrimeResponsesWSSessionForActor = func(ctx context.Context, _ *gin.Context, _ *responsesws.RawResponsesCreateFrame, _ *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
		select {
		case <-openStarted:
		default:
			close(openStarted)
		}
		<-ctx.Done()
		return nil, common.StringErrorWrapperLocal(ctx.Err().Error(), "ws_request_failed", http.StatusInternalServerError)
	}
	t.Cleanup(func() {
		openAndPrimeResponsesWSSessionForActor = originalOpen
	})

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	actor.SetBridge(bridge)
	t.Cleanup(bridge.Close)

	openingID := actor.ReserveFirstTurnOpening(frame)
	admission := NewResponsesWSTurnAdmission()
	actor.turns.opening.admission = admission
	actor.upstream.session = &responsesWSTestSession{}
	actor.upstream.sessionGeneration = "old-session"
	actor.upstream.channelID = 17
	actor.upstream.recvArmed = true
	actor.turns.pending.attempt = &ResponsesWSTurnAttempt{
		OpeningID:         openingID,
		AttemptID:         "attempt-old",
		Admission:         admission,
		Candidate:         &ResponsesTurnAffinity{},
		SelectedChannelID: 17,
		Session:           actor.upstream.session,
		snapshot:          NewResponsesWSRequestSnapshot(ctx),
	}
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend
	startResponsesWSTestActor(t, actor)

	if !actor.Post(ResponsesWSEventSendResult{
		AttemptID:         "attempt-old",
		SelectedChannelID: 17,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    responsesws.ErrUpstreamClosed,
		},
	}) {
		t.Fatal("expected send result to queue")
	}
	select {
	case <-openStarted:
	case <-time.After(time.Second):
		t.Fatal("expected retry open worker to start")
	}

	if !actor.Post(ResponsesWSEventClientClosed{Err: errors.New("client closed")}) {
		t.Fatal("expected client close event to queue while retry open is blocked")
	}
	select {
	case <-actor.Done():
	case <-time.After(time.Second):
		t.Fatal("expected actor to process client close while retry open worker is blocked")
	}
}

func setupResponsesWSReplayableFirstTurnAttempt(t *testing.T) (*ResponsesWSSessionActor, *ResponsesWSTurnAttempt, *responsesWSFakeUserConn, string) {
	t.Helper()
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, PreCost: config.PreContNotAll})
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":[]}`))
	if err != nil {
		t.Fatalf("parse first frame: %v", err)
	}
	actor := NewResponsesWSSessionActor(ctx)
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	openingID := actor.ReserveFirstTurnOpening(frame)
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:           ctx,
		Snapshot:          actor.snapshotClone(),
		OpeningID:         openingID,
		Admission:         actor.turns.opening.admission,
		Candidate:         &ResponsesTurnAffinity{},
		SelectedChannelID: 17,
		Session:           actor.upstream.session,
		BillingModel:      "gpt-5",
		PromptModel:       "gpt-5",
		Request:           &frame.Projection,
	})
	if apiErr != nil {
		t.Fatalf("prepare attempt: %v", apiErr)
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		t.Fatalf("preconsume attempt: %v", apiErr)
	}
	attempt.AttemptID = "attempt-replay-" + strings.ReplaceAll(t.Name(), "/", "_")
	t.Cleanup(func() {
		actor.close("test_cleanup")
	})
	return actor, attempt, conn, generation
}

func setupResponsesWSTestResponseIDAffinity(t *testing.T, ctx *gin.Context, responseID string, ownerChannelID int) (*ResponsesTurnAffinity, string) {
	t.Helper()
	settings := config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 60,
		Rules: []config.ChannelAffinityRule{
			{
				Name:            "responses-response-id",
				Enabled:         true,
				Kind:            "responses",
				IncludeModel:    true,
				IncludeRuleName: true,
				RecordOnSuccess: true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "request_field", Key: "previous_response_id", Alias: config.ChannelAffinityAliasResponseID},
				},
			},
		},
	}
	settings.Normalize()
	manager := withChannelAffinitySettings(t, settings)
	request := &types.OpenAIResponsesRequest{Model: "gpt-5", PreviousResponseID: responseID}
	candidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{Context: ctx, Request: request})
	if err != nil {
		t.Fatalf("prepare affinity: %v", err)
	}
	template := newChannelAffinityTemplate(ctx, channelAffinityKindResponses, "gpt-5", settings.Rules[0], "request_field", config.ChannelAffinityAliasResponseID, settings.DefaultTTLSeconds)
	key := template.BuildKey(responseID)
	manager.SetRecord(key, runtimeaffinity.Record{
		ChannelID:         ownerChannelID,
		ResumeFingerprint: "model:gpt-5",
	}, time.Minute)
	return candidate, key
}

func installResponsesWSReplayOpenProbe(t *testing.T) (chan struct{}, *int32) {
	t.Helper()
	started := make(chan struct{})
	var calls int32
	originalOpen := openAndPrimeResponsesWSSessionForActor
	openAndPrimeResponsesWSSessionForActor = func(ctx context.Context, _ *gin.Context, _ *responsesws.RawResponsesCreateFrame, _ *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-started:
		default:
			close(started)
		}
		<-ctx.Done()
		return nil, common.StringErrorWrapperLocal("retry probe stops before adoption", "ws_request_failed", http.StatusBadGateway)
	}
	t.Cleanup(func() {
		openAndPrimeResponsesWSSessionForActor = originalOpen
	})
	return started, &calls
}

func TestResponsesWSProviderRequestRejectionFromPayloadBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{name: "empty payload", payload: nil},
		{name: "invalid json", payload: []byte(`{"type":"error"`)},
		{name: "non error type", payload: []byte(`{"type":"response.failed","error":{"message":"nope"}}`)},
		{name: "response id blocks request rejection", payload: []byte(`{"type":"error","response_id":"resp_1","error":{"message":"nope"}}`)},
		{name: "response object blocks request rejection", payload: []byte(`{"type":"error","response":{"id":"resp_1"},"error":{"message":"nope"}}`)},
		{name: "null response request rejection", payload: []byte(`{"type":"error","response":null,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`), want: true},
		{name: "top level request rejection", payload: []byte(`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`), want: true},
		{name: "top level request rejection without event type", payload: []byte(`{"status":401,"error":{"type":"invalid_request_error","code":"token_invalidated","message":"token invalidated"}}`), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr, got := responsesWSProviderRequestRejectionFromPayload(tt.payload)
			if got != tt.want {
				t.Fatalf("ok=%v, want %v apiErr=%+v", got, tt.want, apiErr)
			}
			if got && apiErr == nil {
				t.Fatal("expected api error for accepted request-level rejection")
			}
		})
	}
}

func TestResponsesWSNativeRequestErrorBeforeAcceptRollsBackAndRetriesFirstTurn(t *testing.T) {
	actor, attempt, conn, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.active.attempt = attempt
	actor.turns.pending = responsesWSPendingTurn{}
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight
	started, calls := installResponsesWSReplayOpenProbe(t)

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame: responsesWSTestProviderTextFrame([]byte(
			`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`,
		)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   time.Now(),
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected first-turn replay to start a new open worker")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("expected one retry open call, got %d", got)
	}
	if !attempt.RolledBack || attempt.QuotaPreconsumed || attempt.QuotaFinalized {
		t.Fatalf("expected request-level provider error to rollback before retry, attempt=%+v", attempt)
	}
	if attempt.DownstreamCommitted {
		t.Fatalf("expected retry path not to surface downstream payload, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); got != "" {
		t.Fatalf("expected no downstream write before retry, got %q", got)
	}
	if actor.turns.active.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnOpening || actor.state != responsesWSStateOpening {
		t.Fatalf("expected actor to return to opening for replay, active=%+v phase=%v state=%v", actor.turns.active.attempt, actor.turns.pending.phase, actor.state)
	}
	skipIDs, _ := actor.Context().Get("skip_channel_ids")
	if !intSliceContains(skipIDs.([]int), 17) {
		t.Fatalf("expected failed channel to be skipped, got %#v", skipIDs)
	}
}

func TestResponsesWSAttemptScopedProxyLocalCommitsExplicitAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	attempt := &ResponsesWSTurnAttempt{AttemptID: "attempt-explicit-proxy-local"}
	payload := responsesWSErrorPayload(http.StatusBadGateway, "attempt_local_error", "attempt local error")

	actor.writeProxyLocalForAttempt(attempt, payload, "attempt_local_error")

	if !attempt.DownstreamCommitted {
		t.Fatalf("expected explicit attempt proxy-local write to commit downstream, attempt=%+v", attempt)
	}
	if attempt.DownstreamCommitKind != DownstreamCommitProxyError ||
		attempt.DownstreamCommitReason != "attempt_local_error" ||
		attempt.DownstreamCommitSeq == 0 {
		t.Fatalf("unexpected downstream commit metadata: attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "attempt_local_error") {
		t.Fatalf("expected attempt proxy-local payload to be written, got %q", got)
	}
}

func TestResponsesWSSessionBusyDoesNotCommitActiveAttemptBeforeProviderReplay(t *testing.T) {
	actor, attempt, conn, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.active.attempt = attempt
	actor.turns.pending = responsesWSPendingTurn{}
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.create"}`)))

	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "session_busy") {
		t.Fatalf("expected session_busy payload, got %q", got)
	}
	if attempt.DownstreamCommitted {
		t.Fatalf("session-level busy error must not commit current attempt, attempt=%+v", attempt)
	}
	busyWrites := atomic.LoadInt32(&conn.writeCount)
	started, calls := installResponsesWSReplayOpenProbe(t)

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame: responsesWSTestProviderTextFrame([]byte(
			`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`,
		)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   time.Now(),
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected session-local busy error not to block provider rejection replay")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("expected one retry open call, got %d", got)
	}
	if !attempt.RolledBack || attempt.QuotaPreconsumed || attempt.QuotaFinalized || attempt.DownstreamCommitted {
		t.Fatalf("expected provider request rejection to rollback and retry after session_busy, attempt=%+v", attempt)
	}
	if got := atomic.LoadInt32(&conn.writeCount); got != busyWrites {
		t.Fatalf("expected replay to avoid surfacing provider rejection, writes=%d busy_writes=%d", got, busyWrites)
	}
}

func TestResponsesWSInvalidEventDoesNotCommitActiveAttempt(t *testing.T) {
	actor, attempt, conn, _ := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.active.attempt = attempt
	actor.turns.pending = responsesWSPendingTurn{}
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleClientFrame(ResponsesWSEventClientFrame{Frame: responsesws.NewBinaryFrame([]byte{1, 2, 3})})

	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "invalid_event") {
		t.Fatalf("expected invalid_event payload, got %q", got)
	}
	if attempt.DownstreamCommitted {
		t.Fatalf("session-level invalid_event must not commit current attempt, attempt=%+v", attempt)
	}
}

func TestResponsesWSPendingRequestErrorBeforeAcceptReplaysAfterSendResult(t *testing.T) {
	actor, attempt, conn, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend
	started, calls := installResponsesWSReplayOpenProbe(t)

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame: responsesWSTestProviderTextFrame([]byte(
			`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`,
		)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   time.Now(),
	})
	if actor.hasPendingProviderEvidence() {
		t.Fatalf("request-level rejection must not contaminate provider activity evidence: %+v", actor.turns.pending.provider.journal.Project())
	}
	if got, _ := conn.lastWrite.Load().(string); got != "" {
		t.Fatalf("expected pending provider rejection not to be written before send result, got %q", got)
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAttempted,
		},
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected pending request-level rejection to start replay after send result")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("expected one retry open call, got %d", got)
	}
	if !attempt.RolledBack || attempt.QuotaFinalized || attempt.DownstreamCommitted {
		t.Fatalf("expected pending request-level rejection to rollback without downstream commit, attempt=%+v", attempt)
	}
}

func TestResponsesWSPendingCreateCancelBlocksRequestErrorReplay(t *testing.T) {
	actor, attempt, conn, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend
	started, calls := installResponsesWSReplayOpenProbe(t)

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame: responsesWSTestProviderTextFrame([]byte(
			`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`,
		)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   time.Now(),
	})
	actor.markPendingCreateCancel(attempt.AttemptID, responsesWSTestClientTextFrame([]byte(`{"type":"response.cancel"}`)))

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAttempted,
		},
	})

	select {
	case <-started:
		t.Fatal("expected pending cancel to block replay open worker")
	default:
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("expected no retry open calls after pending cancel, got %d", got)
	}
	if !attempt.RolledBack || attempt.QuotaFinalized || !attempt.DownstreamCommitted {
		t.Fatalf("expected pending cancel barrier to rollback and surface, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected pending cancel barrier to clear turn state, pending=%+v active=%+v state=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.state)
	}
	if actor.hasPendingCreateCancel(attempt.AttemptID) {
		t.Fatal("expected surfaced pending cancel barrier to clear pending cancel marker")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "rate_limit_exceeded") {
		t.Fatalf("expected provider rejection payload to be surfaced, got %q", got)
	}
	if _, ok := actor.Context().Get("skip_channel_ids"); ok {
		t.Fatalf("expected pending cancel barrier not to skip channel")
	}
}

func TestResponsesWSNativeContinuationMissRequestErrorClearsAffinityState(t *testing.T) {
	actor, attempt, conn, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	const previousResponseID = "resp_native_continuation_miss"
	candidate, affinityKey := setupResponsesWSTestResponseIDAffinity(t, actor.Context(), previousResponseID, 17)
	attempt.Candidate = candidate
	attempt.AttemptedPreviousResponseID = previousResponseID
	actor.turns.active.attempt = attempt
	actor.turns.pending = responsesWSPendingTurn{}
	actor.turns.active.affinity = CommitResponsesTurnAffinity(candidate, 17)
	actor.turns.active.channelID = 17
	actor.turns.history.lastFinal = &types.OpenAIResponsesResponses{ID: previousResponseID}
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame: responsesWSTestProviderTextFrame([]byte(
			`{"type":"error","status":409,"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"previous response was not found"}}`,
		)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   time.Now(),
	})

	if _, ok := channelAffinityManager().Get(affinityKey); ok {
		t.Fatal("expected native continuation miss to clear stale affinity binding")
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected native continuation miss to clear stale default previous response, got %+v", actor.turns.history.lastFinal)
	}
	if !attempt.RolledBack || attempt.QuotaFinalized || !attempt.DownstreamCommitted {
		t.Fatalf("expected continuation miss to rollback and surface, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected continuation miss to clear turn state, pending=%+v active=%+v state=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.state)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "previous_response_not_found") {
		t.Fatalf("expected continuation miss payload to be surfaced, got %q", got)
	}
}

func TestResponsesWSPendingCreateCancelNotAttemptedContinuationMissClearsAffinityState(t *testing.T) {
	actor, attempt, _, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	const previousResponseID = "resp_cancel_continuation_miss"
	candidate, affinityKey := setupResponsesWSTestResponseIDAffinity(t, actor.Context(), previousResponseID, 17)
	attempt.Candidate = candidate
	attempt.AttemptedPreviousResponseID = previousResponseID
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.turns.history.lastFinal = &types.OpenAIResponsesResponses{ID: previousResponseID}
	actor.state = responsesWSStatePendingSend
	actor.markPendingCreateCancel(attempt.AttemptID, responsesWSTestClientTextFrame([]byte(`{"type":"response.cancel"}`)))

	sendErr := responsesws.NewClientPayloadError(errors.New("provider previous_response_not_found"), responsesWSPreviousResponseNotFoundPayload())
	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    sendErr,
		},
	})

	if _, ok := channelAffinityManager().Get(affinityKey); ok {
		t.Fatal("expected canceled continuation miss to clear stale affinity binding")
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected canceled continuation miss to clear stale default previous response, got %+v", actor.turns.history.lastFinal)
	}
	if !attempt.RolledBack || attempt.QuotaFinalized {
		t.Fatalf("expected canceled not-sent continuation miss to rollback reserve, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected canceled not-sent continuation miss to clear turn state, pending=%+v active=%+v state=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.state)
	}
	if actor.hasPendingCreateCancel(attempt.AttemptID) {
		t.Fatal("expected canceled not-sent continuation miss to clear pending cancel marker")
	}
}

func TestResponsesWSPendingRequestErrorWithProviderCloseSettlesBeforeSurface(t *testing.T) {
	actor, attempt, conn, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame: responsesWSTestProviderTextFrame([]byte(
			`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`,
		)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   time.Now(),
	})
	actor.handleProviderClosed(ResponsesWSEventProviderClosed{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Code:                      int(wsconn.CloseGoingAway),
		Reason:                    "provider closed",
		DetailOrigin:              responsesws.RecvDetailOriginNativeProviderClose,
		DetailPhase:               responsesws.RecvDetailPhaseMapProviderClose,
		ReceivedAt:                time.Now(),
	})

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAttempted,
		},
	})

	if attempt.RolledBack || !attempt.QuotaFinalized || attempt.AppliedSettlement == nil {
		t.Fatalf("expected provider activity barrier to settle floor before surface, attempt=%+v", attempt)
	}
	if attempt.AppliedSettlement.Action != ResponsesWSSettlementFinalizeFloor {
		t.Fatalf("expected floor settlement for provider activity barrier, settlement=%+v", attempt.AppliedSettlement)
	}
	if !attempt.DownstreamCommitted {
		t.Fatalf("expected request rejection payload to be surfaced after settlement, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil {
		t.Fatalf("expected replay surface to release turn state, pending=%+v active=%+v", actor.turns.pending.attempt, actor.turns.active.attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "rate_limit_exceeded") {
		t.Fatalf("expected provider rejection payload after settlement, got %q", got)
	}
}

func TestResponsesWSReplaySurfaceAccountingSettlementFailureDoesNotClearOrSurface(t *testing.T) {
	actor, attempt, conn, _ := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight
	attempt.Quota = nil

	payload := []byte(`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`)
	command := ResponsesReplayCommand{
		Decision: ResponsesAttemptDecisionSurface,
		Failure:  ChannelFailureRateLimited,
		APIError: &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "slow down"},
			StatusCode:  http.StatusTooManyRequests,
		},
		Barrier: ReplayBarrierAccounting,
	}

	if !actor.executeResponsesAttemptReplayCommand(attempt, command, payload, command.APIError, ResponsesAttemptFailureOriginWSProviderRequestError, false) {
		t.Fatal("expected surface accounting command to be handled")
	}
	if actor.turns.active.attempt != attempt || attempt.RolledBack || attempt.QuotaFinalized {
		t.Fatalf("expected failed surface settlement to preserve attempt state, active=%+v attempt=%+v", actor.turns.active.attempt, attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "quota_settlement_failed") || strings.Contains(got, "rate_limit_exceeded") {
		t.Fatalf("expected settlement failure payload without provider surface, got %q", got)
	}
}

func TestResponsesWSPendingRequestErrorBeforeAcceptRollsBackOnClose(t *testing.T) {
	actor, attempt, _, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame: responsesWSTestProviderTextFrame([]byte(
			`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`,
		)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   time.Now(),
	})

	actor.close("client_closed_before_send_result")

	if !attempt.RolledBack || attempt.QuotaFinalized || attempt.QuotaPreconsumed {
		t.Fatalf("expected pending native request-level rejection to rollback on close, attempt=%+v", attempt)
	}
}

func TestResponsesWSPendingRequestErrorBeforeAcceptHasByteCap(t *testing.T) {
	actor, attempt, conn, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	payload := []byte(`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"` +
		strings.Repeat("x", config.ResponsesWSPendingProviderEventsMaxBytes()+1) + `"}}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame(payload),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:                time.Now(),
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected oversized pending request rejection to fail closed")
	}
	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected oversized request rejection fail-close to preserve quota floor, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_pending_provider_buffer_full") {
		t.Fatalf("expected buffer full protocol violation, got %q", got)
	}
}

func TestResponsesWSPendingRequestErrorBeforeAcceptHasEntryCap(t *testing.T) {
	actor, attempt, conn, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	for i := 0; i <= responsesWSPendingProviderEventsMax; i++ {
		actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
			AttemptID:                 attempt.AttemptID,
			UpstreamSessionGeneration: generation,
			ChannelID:                 17,
			Kind:                      ProviderDownstreamFrame,
			Frame: responsesWSTestProviderTextFrame([]byte(
				`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`,
			)),
			DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
			ReceivedAt:   time.Now(),
		})
		if actor.closing.closed.Load() {
			break
		}
	}

	if !actor.closing.closed.Load() {
		t.Fatal("expected excessive pending request rejections to fail closed")
	}
	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected request rejection entry overflow to preserve quota floor, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_pending_provider_buffer_full") {
		t.Fatalf("expected buffer full protocol violation, got %q", got)
	}
}

func TestResponsesWSBridgeOpenProviderErrorRetriesReplayableFirstTurn(t *testing.T) {
	actor, attempt, conn, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend
	started, calls := installResponsesWSReplayOpenProbe(t)

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendRejectedBeforeStream,
		},
	})
	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload: []byte(
			`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`,
		),
		ProviderAPIError: &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "slow down"},
			StatusCode:  http.StatusTooManyRequests,
		},
		Recoverable: true,
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected bridge provider rejection to start replay")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("expected one retry open call, got %d", got)
	}
	if !attempt.RolledBack || attempt.QuotaFinalized || attempt.DownstreamCommitted {
		t.Fatalf("expected bridge provider rejection to rollback and retry silently, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); got != "" {
		t.Fatalf("expected no downstream write before bridge replay, got %q", got)
	}
}

func TestResponsesWSTransportNotAttemptedRetriesViaReplayExecutor(t *testing.T) {
	actor, attempt, conn, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend
	started, calls := installResponsesWSReplayOpenProbe(t)

	var decisionOrigins []string
	var executedOrigins []string
	originalDecision := recordResponsesWSAttemptReplayDecision
	originalExecuted := recordResponsesWSAttemptReplayExecuted
	recordResponsesWSAttemptReplayDecision = func(_ string, origin string, _ int, _ string, _ string) {
		decisionOrigins = append(decisionOrigins, origin)
	}
	recordResponsesWSAttemptReplayExecuted = func(origin string, _ int, _ string) {
		executedOrigins = append(executedOrigins, origin)
	}
	t.Cleanup(func() {
		recordResponsesWSAttemptReplayDecision = originalDecision
		recordResponsesWSAttemptReplayExecuted = originalExecuted
	})

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    responsesws.ErrUpstreamClosed,
		},
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected transport-not-attempted replay to start opening next channel")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("expected one retry open call, got %d", got)
	}
	if !attempt.RolledBack || attempt.QuotaFinalized || attempt.DownstreamCommitted {
		t.Fatalf("expected transport-not-attempted replay to rollback before retry, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); got != "" {
		t.Fatalf("expected no downstream surface before retry, got %q", got)
	}
	if actor.turns.pending.phase != responsesWSPendingTurnOpening || actor.state != responsesWSStateOpening {
		t.Fatalf("expected actor to reopen first turn, phase=%v state=%v", actor.turns.pending.phase, actor.state)
	}
	skipIDs, _ := actor.Context().Get("skip_channel_ids")
	if !intSliceContains(skipIDs.([]int), 17) {
		t.Fatalf("expected failed channel to be skipped, got %#v", skipIDs)
	}
	if len(decisionOrigins) != 1 || decisionOrigins[0] != "transport_not_attempted" {
		t.Fatalf("expected replay decision origin transport_not_attempted, got %#v", decisionOrigins)
	}
	if len(executedOrigins) != 1 || executedOrigins[0] != "transport_not_attempted" {
		t.Fatalf("expected replay executed origin transport_not_attempted, got %#v", executedOrigins)
	}
}

func TestResponsesWSTransportNotAttemptedExplicitPinRollsBackAndSurfaces(t *testing.T) {
	actor, attempt, conn, generation := setupResponsesWSReplayableFirstTurnAttempt(t)
	attempt.Candidate = &ResponsesTurnAffinity{ExplicitPinID: 17}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	var blocked []string
	originalBlocked := recordResponsesWSAttemptReplayBlocked
	recordResponsesWSAttemptReplayBlocked = func(barrier, origin string, _ int) {
		blocked = append(blocked, barrier+":"+origin)
	}
	t.Cleanup(func() {
		recordResponsesWSAttemptReplayBlocked = originalBlocked
	})

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    errors.New("prepare failed"),
		},
	})

	if !attempt.RolledBack || attempt.QuotaFinalized || !attempt.DownstreamCommitted {
		t.Fatalf("expected explicit-pin not-attempted to rollback and surface downstream, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected blocked replay to release pending attempt, pending=%+v state=%v", actor.turns.pending.attempt, actor.state)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "upstream_error") {
		t.Fatalf("expected downstream error surface, got %q", got)
	}
	if len(blocked) != 1 || blocked[0] != "affinity:transport_not_attempted" {
		t.Fatalf("expected affinity blocked metric for transport_not_attempted, got %#v", blocked)
	}
	if _, ok := actor.Context().Get("skip_channel_ids"); ok {
		t.Fatalf("expected blocked replay not to skip channel")
	}
}

func TestResponsesWSTransportNotAttemptedWithProviderEvidenceDoesNotEnterReplayExecutor(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-not-attempted-evidence")
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend
	actor.turns.pending.provider.journal = responsesWSTestProviderFrameJournal()

	replayDecisions := 0
	originalDecision := recordResponsesWSAttemptReplayDecision
	recordResponsesWSAttemptReplayDecision = func(_ string, _ string, _ int, _ string, _ string) {
		replayDecisions++
	}
	t.Cleanup(func() {
		recordResponsesWSAttemptReplayDecision = originalDecision
		actor.close("test_cleanup")
	})

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    responsesws.ErrUpstreamClosed,
		},
	})

	if replayDecisions != 0 {
		t.Fatalf("expected provider evidence conflict not to enter replay executor, decisions=%d", replayDecisions)
	}
	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected provider evidence conflict to settle conservatively, attempt=%+v", attempt)
	}
}

func TestResponsesWSReplayRollbackUnknownOriginFailsClosed(t *testing.T) {
	actor, attempt, conn, _ := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	command := ResponsesReplayCommand{Decision: ResponsesAttemptDecisionRollbackAndRetryNextChannel}
	if !actor.executeResponsesAttemptReplayCommand(attempt, command, nil, nil, ResponsesAttemptFailureOriginUnknown, false) {
		t.Fatal("expected unknown-origin rollback command to be handled by fail-closed path")
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected unknown-origin replay rollback to close session")
	}
	if attempt.RolledBack {
		t.Fatalf("expected unknown-origin replay not to accept rollback proof, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "quota_settlement_failed") {
		t.Fatalf("expected quota settlement failure surface, got %q", got)
	}
}

func TestResponsesWSReplayRetryStateConflictDoesNotSkipOrReopen(t *testing.T) {
	actor, attempt, _, _ := setupResponsesWSReplayableFirstTurnAttempt(t)
	actor.turns.pending.attempt = &ResponsesWSTurnAttempt{AttemptID: "unrelated-pending"}
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.turns.active.attempt = attempt
	actor.state = responsesWSStateInFlight
	started, calls := installResponsesWSReplayOpenProbe(t)

	command := ResponsesReplayCommand{Decision: ResponsesAttemptDecisionRollbackAndRetryNextChannel}
	if actor.retryFirstTurnAfterReplayableFailure(attempt, command) {
		t.Fatal("expected replay retry state conflict not to report replayed")
	}
	select {
	case <-started:
		t.Fatal("expected no retry open worker on state conflict")
	default:
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("expected no retry open calls, got %d", got)
	}
	if _, ok := actor.Context().Get("skip_channel_ids"); ok {
		t.Fatalf("expected state conflict not to skip channel")
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected state conflict to fail closed")
	}
}

func TestOpenResponsesWSPreferredChannelHonorsSelectionEligibility(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	const (
		fallbackChannelID  = 11
		preferredChannelID = 22
	)

	model.ChannelGroup = buildRealtimeTestChannelGroup(fallbackChannelID, preferredChannelID)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "default")
	ctx.Set("skip_channel_ids", []int{preferredChannelID})

	openResult, apiErr := openResponsesWSPreferredChannel(ctx, "gpt-5", &ResponsesTurnAffinity{}, preferredChannelID)
	if apiErr == nil {
		t.Fatalf("expected preferred channel blocked by normal selection filters to fail, got result %#v", openResult)
	}
	if strings.Contains(apiErr.Message, "无效的渠道 Id") {
		t.Fatalf("expected normal preferred selection path, got raw channel id lookup error %q", apiErr.Message)
	}
	if got := ctx.GetInt("channel_id"); got == preferredChannelID {
		t.Fatalf("expected ineligible preferred channel not to be attached to context, got channel_id=%d", got)
	}
}

func TestOpenResponsesWSPreferredChannelTreatsResponsesWSAsStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	const preferredChannelID = 31
	disabledStream := datatypes.JSONSlice[string]{"gpt-5"}
	preferred := newRelayTestCodexChannel(preferredChannelID)
	preferred.DisabledStream = &disabledStream
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(preferred)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "default")

	openResult, apiErr := openResponsesWSPreferredChannel(ctx, "gpt-5", &ResponsesTurnAffinity{}, preferredChannelID)
	if apiErr == nil {
		t.Fatalf("expected stream-disabled preferred channel to fail selection, got result %#v", openResult)
	}
	if !ctx.GetBool("is_stream") {
		t.Fatalf("expected ResponsesWS preferred open to mark the request as streaming")
	}
	if got := ctx.GetInt("channel_id"); got == preferredChannelID {
		t.Fatalf("expected stream-disabled preferred channel not to be attached, got channel_id=%d", got)
	}
}

func TestOpenAndPrimeResponsesWSFreshSelectionTreatsResponsesWSAsStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	const disabledChannelID = 32
	disabledStream := datatypes.JSONSlice[string]{"gpt-5"}
	disabled := newRelayTestCodexChannel(disabledChannelID)
	disabled.DisabledStream = &disabledStream
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(disabled)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "default")

	openResult, apiErr := openAndPrimeResponsesWSSession(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"})
	if openResult != nil && openResult.Session != nil {
		openResult.Session.Abort("test_done")
	}
	if apiErr == nil {
		t.Fatalf("expected only stream-disabled fresh candidate to fail selection")
	}
	if !ctx.GetBool("is_stream") {
		t.Fatalf("expected ResponsesWS fresh open to mark the request as streaming")
	}
	if got := ctx.GetInt("channel_id"); got == disabledChannelID {
		t.Fatalf("expected stream-disabled fresh candidate not to be attached, got channel_id=%d", got)
	}
}

func TestOpenAndPrimeResponsesWSNonStrictUnsupportedPreferredFallsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	unsupportedServer := httptest.NewServer(http.NotFoundHandler())
	defer unsupportedServer.Close()

	upgraded := make(chan *wsconn.ManagedConn, 1)
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsconn.AcceptManaged(w, r, wsconn.Config{Label: "responses ws fallback test accept"}, wsconn.AcceptOptions{
			CheckOrigin: func(*http.Request) bool { return true },
		})
		if err != nil {
			t.Errorf("fallback upgrade failed: %v", err)
			return
		}
		upgraded <- conn
		<-r.Context().Done()
	}))
	defer fallbackServer.Close()

	const (
		preferredChannelID = 77
		fallbackChannelID  = 88
	)
	proxy := ""
	preferredBaseURL := unsupportedServer.URL
	fallbackBaseURL := fallbackServer.URL
	selfHostedOther := `{"responses_ws_native":true,"responses_ws_self_hosted":true}`
	preferred := &model.Channel{
		Id:      preferredChannelID,
		Type:    config.ChannelTypeOpenAI,
		Key:     "sk-preferred",
		BaseURL: &preferredBaseURL,
		Proxy:   &proxy,
		Other:   selfHostedOther,
	}
	fallback := &model.Channel{
		Id:      fallbackChannelID,
		Type:    config.ChannelTypeOpenAI,
		Key:     "sk-fallback",
		BaseURL: &fallbackBaseURL,
		Proxy:   &proxy,
		Other:   selfHostedOther,
	}
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(fallback, preferred)

	settings := config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 60,
		Rules: []config.ChannelAffinityRule{
			{
				Name:            "responses-prompt-nonstrict",
				Enabled:         true,
				Kind:            "responses",
				IncludeModel:    true,
				IncludeRuleName: true,
				Strict:          false,
				RecordOnSuccess: true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "request_field", Key: "prompt_cache_key", Alias: config.ChannelAffinityAliasPromptCacheKey},
				},
			},
		},
	}
	settings.Normalize()
	manager := withChannelAffinitySettings(t, settings)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "default")

	request := &types.OpenAIResponsesRequest{Model: "gpt-5", PromptCacheKey: "pc-fallback"}
	template := newChannelAffinityTemplate(ctx, channelAffinityKindResponses, "gpt-5", settings.Rules[0], "request_field", config.ChannelAffinityAliasPromptCacheKey, settings.DefaultTTLSeconds)
	manager.SetRecord(template.BuildKey(request.PromptCacheKey), runtimeaffinity.Record{
		ChannelID:         preferredChannelID,
		ResumeFingerprint: "model:gpt-5",
	}, time.Minute)

	openResult, apiErr := openAndPrimeResponsesWSSession(ctx, request)
	if apiErr != nil {
		t.Fatalf("expected non-strict unsupported preferred channel to fall back, got %v", apiErr)
	}
	if openResult == nil || openResult.Channel == nil || openResult.Channel.Id != fallbackChannelID {
		t.Fatalf("expected fallback channel #%d, got %#v", fallbackChannelID, openResult)
	}
	if openResult.Session != nil {
		openResult.Session.Abort("test_done")
	}
	select {
	case conn := <-upgraded:
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
	case <-time.After(time.Second):
		t.Fatalf("expected fallback websocket server to be used")
	}
	skipped, _ := ctx.Get("skip_channel_ids")
	skippedIDs, _ := skipped.([]int)
	if !intSliceContains(skippedIDs, preferredChannelID) {
		t.Fatalf("expected unsupported non-strict preferred channel to be skipped, got %#v", skipped)
	}
}

func TestOpenAndPrimeResponsesWSCodexTokenInvalidatedFallsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})
	originalRetryTimes := config.RetryTimes
	config.RetryTimes = 2
	t.Cleanup(func() {
		config.RetryTimes = originalRetryTimes
	})

	authFailedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{
			"error": {
				"message": "Your authentication token has been invalidated. Please try signing in again.",
				"type": "invalid_request_error",
				"code": "token_invalidated"
			},
			"status": 401
		}`))
	}))
	defer authFailedServer.Close()

	upgraded := make(chan *wsconn.ManagedConn, 1)
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsconn.AcceptManaged(w, r, wsconn.Config{Label: "responses ws codex token fallback accept"}, wsconn.AcceptOptions{
			CheckOrigin: func(*http.Request) bool { return true },
		})
		if err != nil {
			t.Errorf("fallback upgrade failed: %v", err)
			return
		}
		upgraded <- conn
		<-r.Context().Done()
	}))
	defer fallbackServer.Close()

	const (
		failedChannelID   = 91
		fallbackChannelID = 92
	)
	authFailedBaseURL := authFailedServer.URL
	fallbackBaseURL := fallbackServer.URL
	failed := newRelayTestCodexChannel(failedChannelID)
	failed.BaseURL = &authFailedBaseURL
	failed.Other = `{"websocket_mode":"force","responses_ws_self_hosted":true}`
	fallback := newRelayTestCodexChannel(fallbackChannelID)
	fallback.BaseURL = &fallbackBaseURL
	fallback.Other = `{"websocket_mode":"force","responses_ws_self_hosted":true}`
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(failed, fallback)
	model.ChannelGroup.Rule["default"]["gpt-5"] = [][]int{{failedChannelID}, {fallbackChannelID}}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "default")

	request := &types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hi"}
	openResult, apiErr := openAndPrimeResponsesWSSessionWithContextAndFrame(context.Background(), ctx, responsesWSTestOpenFrame(t), request)
	if apiErr != nil {
		if strings.Contains(apiErr.Message, "invalidated") || strings.Contains(apiErr.Message, "signing in") {
			t.Fatalf("expected token invalidated detail to remain hidden, got %+v", apiErr)
		}
		t.Fatalf("expected token-invalidated channel to fall back, got %v", apiErr)
	}
	if openResult == nil || openResult.Channel == nil || openResult.Channel.Id != fallbackChannelID {
		t.Fatalf("expected fallback channel #%d, got %#v", fallbackChannelID, openResult)
	}
	if openResult.Session != nil {
		openResult.Session.Abort("test_done")
	}
	select {
	case conn := <-upgraded:
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
	case <-time.After(time.Second):
		t.Fatalf("expected fallback websocket server to be used")
	}
	skipped, _ := ctx.Get("skip_channel_ids")
	skippedIDs, _ := skipped.([]int)
	if !intSliceContains(skippedIDs, failedChannelID) {
		t.Fatalf("expected token-invalidated channel to be skipped, got %#v", skipped)
	}
}

func TestOpenAndPrimeResponsesWSUnsupportedCandidateDoesNotConsumeRetryBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})
	originalRetryTimes := config.RetryTimes
	config.RetryTimes = 1
	t.Cleanup(func() {
		config.RetryTimes = originalRetryTimes
	})

	unsupportedServer := httptest.NewServer(http.NotFoundHandler())
	defer unsupportedServer.Close()

	upgraded := make(chan *wsconn.ManagedConn, 1)
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsconn.AcceptManaged(w, r, wsconn.Config{Label: "responses ws retry budget fallback"}, wsconn.AcceptOptions{
			CheckOrigin: func(*http.Request) bool { return true },
		})
		if err != nil {
			t.Errorf("fallback upgrade failed: %v", err)
			return
		}
		upgraded <- conn
		<-r.Context().Done()
	}))
	defer fallbackServer.Close()

	proxy := ""
	unsupportedBaseURL := unsupportedServer.URL
	fallbackBaseURL := fallbackServer.URL
	selfHostedOther := `{"responses_ws_native":true,"responses_ws_self_hosted":true}`
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(
		&model.Channel{Id: 81, Type: config.ChannelTypeOpenAI, Key: "sk-unsupported", BaseURL: &unsupportedBaseURL, Proxy: &proxy, Other: selfHostedOther},
		&model.Channel{Id: 82, Type: config.ChannelTypeOpenAI, Key: "sk-fallback", BaseURL: &fallbackBaseURL, Proxy: &proxy, Other: selfHostedOther},
	)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "default")

	openResult, apiErr := openAndPrimeResponsesWSSession(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"})
	if apiErr != nil {
		t.Fatalf("expected unsupported candidate not to consume retry budget, got %v", apiErr)
	}
	if openResult == nil || openResult.Channel == nil || openResult.Channel.Id != 82 {
		t.Fatalf("expected fallback channel #82, got %#v", openResult)
	}
	if openResult.Session != nil {
		openResult.Session.Abort("test_done")
	}
	select {
	case conn := <-upgraded:
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
	case <-time.After(time.Second):
		t.Fatalf("expected fallback websocket server to be used")
	}
}

func TestOpenAndPrimeResponsesWSNonStrictPreferredProviderErrorSurvivesUnsupportedFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	badGatewayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer badGatewayServer.Close()
	unsupportedServer := httptest.NewServer(http.NotFoundHandler())
	defer unsupportedServer.Close()

	const (
		preferredChannelID   = 83
		unsupportedChannelID = 84
	)
	proxy := ""
	preferredBaseURL := badGatewayServer.URL
	unsupportedBaseURL := unsupportedServer.URL
	selfHostedOther := `{"responses_ws_native":true,"responses_ws_self_hosted":true}`
	preferred := &model.Channel{Id: preferredChannelID, Type: config.ChannelTypeOpenAI, Key: "sk-preferred", BaseURL: &preferredBaseURL, Proxy: &proxy, Other: selfHostedOther}
	unsupported := &model.Channel{Id: unsupportedChannelID, Type: config.ChannelTypeOpenAI, Key: "sk-unsupported", BaseURL: &unsupportedBaseURL, Proxy: &proxy, Other: selfHostedOther}
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(unsupported, preferred)

	settings := config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 60,
		Rules: []config.ChannelAffinityRule{
			{
				Name:            "responses-prompt-nonstrict-error",
				Enabled:         true,
				Kind:            "responses",
				IncludeModel:    true,
				IncludeRuleName: true,
				Strict:          false,
				RecordOnSuccess: true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "request_field", Key: "prompt_cache_key", Alias: config.ChannelAffinityAliasPromptCacheKey},
				},
			},
		},
	}
	settings.Normalize()
	manager := withChannelAffinitySettings(t, settings)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "default")

	request := &types.OpenAIResponsesRequest{Model: "gpt-5", PromptCacheKey: "pc-provider-error"}
	template := newChannelAffinityTemplate(ctx, channelAffinityKindResponses, "gpt-5", settings.Rules[0], "request_field", config.ChannelAffinityAliasPromptCacheKey, settings.DefaultTTLSeconds)
	manager.SetRecord(template.BuildKey(request.PromptCacheKey), runtimeaffinity.Record{
		ChannelID:         preferredChannelID,
		ResumeFingerprint: "model:gpt-5",
	}, time.Minute)

	openResult, apiErr := openAndPrimeResponsesWSSession(ctx, request)
	if openResult != nil && openResult.Session != nil {
		openResult.Session.Abort("test_done")
	}
	if apiErr == nil || apiErr.StatusCode != http.StatusBadGateway || openAIErrorCodeString(apiErr.Code, "") != "provider_ws_request_failed" {
		t.Fatalf("expected preferred provider 5xx to survive unsupported fallback, result=%#v err=%+v", openResult, apiErr)
	}
}

func TestOpenAndPrimeResponsesWSAllUnsupportedReturnsFallbackError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	unsupportedServer := httptest.NewServer(http.NotFoundHandler())
	defer unsupportedServer.Close()

	proxy := ""
	baseURL := unsupportedServer.URL
	selfHostedOther := `{"responses_ws_native":true,"responses_ws_self_hosted":true}`
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(&model.Channel{
		Id:      91,
		Type:    config.ChannelTypeOpenAI,
		Key:     "sk-unsupported",
		BaseURL: &baseURL,
		Proxy:   &proxy,
		Other:   selfHostedOther,
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "default")

	openResult, apiErr := openAndPrimeResponsesWSSession(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"})
	if openResult != nil && openResult.Session != nil {
		openResult.Session.Abort("test_done")
	}
	if apiErr == nil || openAIErrorCodeString(apiErr.Code, "") != "responses_ws_unsupported_for_channel" || apiErr.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("expected all unsupported channels to return 426 fallback error, result=%#v err=%+v", openResult, apiErr)
	}
}

func TestOpenAndPrimeResponsesWSMixedUnsupportedPreservesProviderError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})
	originalRetryTimes := config.RetryTimes
	config.RetryTimes = 2
	t.Cleanup(func() {
		config.RetryTimes = originalRetryTimes
	})

	unsupportedServer := httptest.NewServer(http.NotFoundHandler())
	defer unsupportedServer.Close()
	badGatewayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer badGatewayServer.Close()

	proxy := ""
	unsupportedBaseURL := unsupportedServer.URL
	badGatewayBaseURL := badGatewayServer.URL
	selfHostedOther := `{"responses_ws_native":true,"responses_ws_self_hosted":true}`
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(
		&model.Channel{
			Id:      92,
			Type:    config.ChannelTypeOpenAI,
			Key:     "sk-unsupported",
			BaseURL: &unsupportedBaseURL,
			Proxy:   &proxy,
			Other:   selfHostedOther,
		},
		&model.Channel{
			Id:      93,
			Type:    config.ChannelTypeOpenAI,
			Key:     "sk-bad-gateway",
			BaseURL: &badGatewayBaseURL,
			Proxy:   &proxy,
			Other:   selfHostedOther,
		},
	)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "default")

	openResult, apiErr := openAndPrimeResponsesWSSession(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"})
	if openResult != nil && openResult.Session != nil {
		openResult.Session.Abort("test_done")
	}
	if apiErr == nil || openAIErrorCodeString(apiErr.Code, "") != "provider_ws_request_failed" || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected mixed unsupported/provider failure to preserve provider 5xx, result=%#v err=%+v", openResult, apiErr)
	}
}

func TestResponsesWSUnsupportedScanLimitCapsByConfigAndChannelCount(t *testing.T) {
	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})
	originalRetryTimes := config.RetryTimes
	config.RetryTimes = 10
	t.Cleanup(func() {
		config.RetryTimes = originalRetryTimes
	})
	setResponsesWSTestViperInt(t, "responses_ws.unsupported_scan_limit", 5)

	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(
		&model.Channel{Id: 101, Type: config.ChannelTypeOpenAI, Key: "sk-1"},
		&model.Channel{Id: 102, Type: config.ChannelTypeOpenAI, Key: "sk-2"},
	)

	if got := responsesWSUnsupportedScanLimit(); got != 2 {
		t.Fatalf("expected unsupported scan limit to cap at channel count 2, got %d", got)
	}
	if limit, limited := responsesWSUnsupportedScanPolicy(); limit != 2 || limited {
		t.Fatalf("expected high scan limit not to be marked as limited, limit=%d limited=%v", limit, limited)
	}
	setResponsesWSTestViperInt(t, "responses_ws.unsupported_scan_limit", 1)
	if limit, limited := responsesWSUnsupportedScanPolicy(); limit != 1 || !limited {
		t.Fatalf("expected low explicit scan limit to be marked limited, limit=%d limited=%v", limit, limited)
	}
}

func TestResponsesWSActorBackpressurePostsEventInsteadOfClosingDirectly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	t.Cleanup(actor.finish)

	for i := 0; i < cap(actor.events); i++ {
		actor.events <- ResponsesWSEventTimeout{Reason: "preloaded"}
	}

	actor.Post(ResponsesWSEventClientClosed{})
	if actor.closing.closed.Load() {
		t.Fatalf("expected event queue backpressure to be handled by actor event, got direct close")
	}
	select {
	case <-actor.Done():
		t.Fatalf("expected actor to remain open until it handles the backpressure event")
	default:
	}

	deadline := time.After(time.Second)
	for i := 0; i < cap(actor.events)+1; i++ {
		select {
		case event := <-actor.events:
			timeout, ok := event.(ResponsesWSEventTimeout)
			if ok && timeout.Reason == "responses_ws_event_backpressure" {
				return
			}
		case <-deadline:
			t.Fatalf("expected queued backpressure timeout event")
		}
	}
	t.Fatalf("expected queued backpressure timeout event")
}

func TestResponsesWSPostReliableTimesOutAndRequestsCloseIntent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	t.Cleanup(actor.finish)

	actor.reliablePostTimeout = 100 * time.Millisecond

	originalRecorder := recordResponsesWSEventPostTimeout
	recordedEventTypes := make(chan string, 4)
	recordResponsesWSEventPostTimeout = func(eventType string) {
		recordedEventTypes <- eventType
	}
	t.Cleanup(func() {
		recordResponsesWSEventPostTimeout = originalRecorder
	})

	for i := 0; i < cap(actor.events); i++ {
		actor.events <- ResponsesWSEventTimeout{Reason: "preloaded"}
	}

	start := time.Now()
	if actor.PostReliable(ResponsesWSEventClientClosed{}) {
		t.Fatal("expected reliable post to time out on a full actor queue")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected bounded reliable post to return promptly, took %s", elapsed)
	}
	select {
	case got := <-recordedEventTypes:
		if got != "client_closed" {
			t.Fatalf("expected client_closed metric label, got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected reliable post timeout metric")
	}
	if !actor.closing.closeIntentPosted.Load() {
		t.Fatal("expected reliable post timeout to request close intent")
	}

	for i := 0; i < cap(actor.events); i++ {
		<-actor.events
	}
	event := readResponsesWSEvent(t, actor)
	closeIntent, ok := event.(ResponsesWSEventCloseIntent)
	if !ok {
		t.Fatalf("expected close intent after queue drains, got %T", event)
	}
	if closeIntent.Reason != "reliable_post_timeout" {
		t.Fatalf("unexpected close intent reason %q", closeIntent.Reason)
	}
}

func TestResponsesWSOnClientConnClosedPostsAsyncWhenQueueFull(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	t.Cleanup(actor.finish)

	actor.reliablePostTimeout = 20 * time.Millisecond

	for i := 0; i < cap(actor.events); i++ {
		actor.events <- ResponsesWSEventTimeout{Reason: "preloaded"}
	}

	start := time.Now()
	actor.onClientConnClosed(wsconn.CloseInfo{Err: io.EOF})
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("expected OnClose callback path not to block on full actor queue, took %s", elapsed)
	}

	waitResponsesWSTestCondition(t, time.Second, time.Millisecond, func() bool {
		return actor.closing.closeIntentPosted.Load()
	}, func() string {
		return "expected async client close post timeout to request close intent"
	})
}

func TestResponsesWSProviderRecvPumpStopsAfterReliablePostFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	t.Cleanup(actor.finish)

	actor.reliablePostTimeout = 20 * time.Millisecond

	for i := 0; i < cap(actor.events); i++ {
		actor.events <- ResponsesWSEventTimeout{Reason: "preloaded"}
	}

	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 2)}
	session.responses <- responsesWSRecvResult{messageType: int(wsconn.TextMessage), payload: []byte(`{"type":"response.created","response":{"id":"resp_1"}}`)}
	session.responses <- responsesWSRecvResult{providerClose: &responsesws.ProviderClose{Code: int(wsconn.CloseNormalClosure), Reason: "second"}}

	bridge := NewResponsesWSIOBridge(nil, actor)
	t.Cleanup(bridge.Close)
	bridge.ArmProviderRecvPump("generation-1", 11, session)

	waitResponsesWSTestCondition(t, time.Second, time.Millisecond, func() bool {
		return actor.closing.closeIntentPosted.Load()
	}, func() string {
		return "expected failed provider event post to request close intent"
	})
	time.Sleep(3 * actor.reliablePostTimeout)

	if got := atomic.LoadInt32(&session.recvCalls); got != 1 {
		t.Fatalf("expected provider recv pump to stop after first failed post, recv calls=%d", got)
	}
}

func TestResponsesWSOnClientFrameCopiesPayloadAndPostsNonBlocking(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	payload := []byte(`{"type":"response.cancel"}`)

	actor.onClientFrame(context.Background(), wsconn.TextMessage, payload)
	payload[0] = '['

	event := readResponsesWSEvent(t, actor)
	clientFrame, ok := event.(ResponsesWSEventClientFrame)
	if !ok {
		t.Fatalf("expected client frame event, got %T", event)
	}
	if clientFrame.Frame.Kind() != responsesws.FrameKindText || string(clientFrame.Frame.Payload()) != `{"type":"response.cancel"}` {
		t.Fatalf("unexpected client frame event: %+v payload=%q", clientFrame, clientFrame.Frame.Payload())
	}
	if clientFrame.ReceivedAt.IsZero() {
		t.Fatal("expected client frame received time")
	}
}

func TestResponsesWSOnClientFrameDropsAfterActorDone(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	actor.finish()

	actor.onClientFrame(context.Background(), wsconn.TextMessage, []byte(`{"type":"response.cancel"}`))

	select {
	case event := <-actor.events:
		t.Fatalf("expected client frame after actor done to be discarded, got %#v", event)
	default:
	}
}

func TestResponsesWSOnClientFrameBackpressureRequestsCloseAndClosesManagedConn(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	client, server := wstest.Pair(t)
	defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})

	actor := NewResponsesWSSessionActor(ctx)
	actor.SetClientConn(client)
	for i := 0; i < cap(actor.events); i++ {
		actor.events <- ResponsesWSEventTimeout{Reason: "preloaded"}
	}

	actor.onClientFrame(context.Background(), wsconn.TextMessage, []byte(`{"type":"response.cancel"}`))

	var closeIntentSeen bool
	for i := 0; i < cap(actor.events)+1; i++ {
		event := <-actor.events
		if closeIntent, ok := event.(ResponsesWSEventCloseIntent); ok && closeIntent.Reason == "client_frame_backpressure" {
			closeIntentSeen = true
			break
		}
	}
	if !closeIntentSeen {
		t.Fatal("expected client frame backpressure close intent")
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for managed client close")
	}
	info := client.CloseInfo()
	if info.Kind != wsconn.CloseKindBackpressure || info.Code != wsconn.CloseTryAgainLater || info.Reason != "client_frame_backpressure" {
		t.Fatalf("expected managed client backpressure close info, got %+v", info)
	}
}

func TestResponsesWSTransportSendResultReliableWhenMailboxFull(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	session := &responsesWSTestSession{}
	for i := 0; i < cap(actor.events); i++ {
		actor.events <- ResponsesWSEventTimeout{Reason: "preloaded"}
	}

	done := make(chan struct{})
	go func() {
		actor.handleSendCommand(responsesWSSendCommand{
			AttemptID:         "attempt-reliable",
			SelectedChannelID: 17,
			Session:           session,
			Frame:             responsesWSFrameFromWireMessage(responsesWSTextMessageType, []byte(`{"type":"response.cancel"}`)),
		})
		close(done)
	}()

	<-actor.events
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reliable send result post")
	}
	for i := 0; i < cap(actor.events)-1; i++ {
		<-actor.events
	}
	event := <-actor.events
	sendResult, ok := event.(ResponsesWSEventSendResult)
	if !ok {
		t.Fatalf("expected send result after draining preloaded events, got %T", event)
	}
	if sendResult.AttemptID != "attempt-reliable" ||
		sendResult.TransportResult.Status != responsesws.ResponsesWSTransportSendAttempted {
		t.Fatalf("unexpected send result: %+v", sendResult)
	}
}

func TestResponsesWSControlLaneBypassesBlockedCreateSend(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	session := &responsesWSControlLaneTestSession{
		createStarted: make(chan struct{}),
		releaseCreate: make(chan struct{}),
		controlCalled: make(chan struct{}),
	}
	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()
	defer close(session.releaseCreate)
	actor.AttachUpstreamSession(session, 17)

	if !actor.SendProviderFrame("attempt-blocked-create", 17, session, responsesws.NewTextFrame([]byte(`{"type":"response.create"}`))) {
		t.Fatal("expected create send to enqueue")
	}
	select {
	case <-session.createStarted:
	case <-time.After(time.Second):
		t.Fatal("expected create send to start and block")
	}

	if !actor.SendProviderControlFrame("attempt-blocked-create", 17, session, responsesws.NewTextFrame([]byte(`{"type":"response.cancel"}`))) {
		t.Fatal("expected cancel control send to enqueue")
	}
	select {
	case <-session.controlCalled:
	case <-time.After(time.Second):
		t.Fatal("expected cancel control lane to bypass blocked create send")
	}
}

func TestResponsesWSControlFrameUsesQueuedContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	type contextKey string
	const key contextKey = "responses_ws_control_context"

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	reqCtx := context.WithValue(context.Background(), key, "queued")
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil).WithContext(reqCtx)

	session := &responsesWSControlLaneTestSession{
		createStarted:   make(chan struct{}),
		releaseCreate:   make(chan struct{}),
		controlCalled:   make(chan struct{}),
		controlContexts: make(chan context.Context, 1),
		controlRequests: make(chan responsesws.SendRequest, 1),
	}
	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()
	defer close(session.releaseCreate)
	actor.AttachUpstreamSession(session, 17)

	if !actor.SendProviderControlFrame("attempt-control-context", 17, session, responsesws.NewTextFrame([]byte(`{"type":"response.cancel"}`))) {
		t.Fatal("expected cancel control send to enqueue")
	}

	select {
	case controlCtx := <-session.controlContexts:
		if got := controlCtx.Value(key); got != "queued" {
			t.Fatalf("expected queued request context value, got %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for control context")
	}
	select {
	case req := <-session.controlRequests:
		if req.AttemptID != "attempt-control-context" {
			t.Fatalf("expected control request attempt id, got %q", req.AttemptID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for control request")
	}
}

func TestResponsesWSHandleControlCommandPropagatesSendResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	controlErr := errors.New("no active bridge cancel")
	session := &responsesWSControlLaneTestSession{
		controlCalled: make(chan struct{}),
		controlResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    controlErr,
			Reason: responsesws.ResponsesWSTransportSendReasonNoActiveBridgeCancel,
		},
	}
	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()

	actor.handleControlCommand(responsesWSSendCommand{
		AttemptID:                 "attempt-control-propagation",
		ResponseID:                "resp-control",
		UpstreamSessionGeneration: "generation-control",
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeControl,
		Session:                   session,
		Frame:                     responsesWSFrameFromWireMessage(responsesWSTextMessageType, []byte(`{"type":"response.cancel"}`)),
	})

	event := readResponsesWSEvent(t, actor)
	sendResult, ok := event.(ResponsesWSEventSendResult)
	if !ok {
		t.Fatalf("expected control send result event, got %T", event)
	}
	if sendResult.AttemptID != "attempt-control-propagation" ||
		sendResult.ResponseID != "resp-control" ||
		sendResult.UpstreamSessionGeneration != "generation-control" ||
		sendResult.SelectedChannelID != 17 ||
		sendResult.Purpose != ResponsesWSSendPurposeControl {
		t.Fatalf("expected control routing fields to be preserved, got %+v", sendResult)
	}
	if sendResult.TransportResult.Status != responsesws.ResponsesWSTransportSendNotAttempted ||
		sendResult.TransportResult.Reason != responsesws.ResponsesWSTransportSendReasonNoActiveBridgeCancel ||
		!errors.Is(sendResult.TransportResult.Err, controlErr) ||
		!errors.Is(sendResult.TransportResult.Err, controlErr) {
		t.Fatalf("expected control result details to propagate, got %+v", sendResult)
	}
}

func TestResponsesWSHandleControlCommandRejectsNonControlCapableSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()
	actor.handleControlCommand(responsesWSSendCommand{
		AttemptID:         "attempt-control-non-capable",
		SelectedChannelID: 17,
		Purpose:           ResponsesWSSendPurposeControl,
		Session:           &responsesWSTestSession{},
		Frame:             responsesWSFrameFromWireMessage(responsesWSTextMessageType, []byte(`{"type":"response.cancel"}`)),
	})

	event := readResponsesWSEvent(t, actor)
	sendResult, ok := event.(ResponsesWSEventSendResult)
	if !ok {
		t.Fatalf("expected control send result event, got %T", event)
	}
	if sendResult.TransportResult.Status != responsesws.ResponsesWSTransportSendNotAttempted ||
		!errors.Is(sendResult.TransportResult.Err, responsesws.ErrInvalidFrame) ||
		!errors.Is(sendResult.TransportResult.Err, responsesws.ErrInvalidFrame) ||
		sendResult.AttemptID != "attempt-control-non-capable" ||
		sendResult.Purpose != ResponsesWSSendPurposeControl {
		t.Fatalf("expected non-control capable session to return NotSent invalid-frame result, got %+v", sendResult)
	}
}

func TestResponsesWSControlFallbackPreservesAttemptID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	session := &responsesWSCaptureSendSession{requests: make(chan responsesws.SendRequest, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()
	actor.AttachUpstreamSession(session, 17)

	if !actor.SendProviderControlFrame("attempt-fallback-cancel", 17, session, responsesws.NewTextFrame([]byte(`{"type":"response.cancel"}`))) {
		t.Fatal("expected non-control cancel fallback to enqueue on send lane")
	}

	select {
	case req := <-session.requests:
		if req.AttemptID != "attempt-fallback-cancel" {
			t.Fatalf("expected cancel fallback request to preserve attempt id, got %q", req.AttemptID)
		}
		if string(req.Frame.Payload()) != `{"type":"response.cancel"}` {
			t.Fatalf("expected cancel payload to be preserved, got %s", req.Frame.Payload())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fallback send request")
	}

	event := readResponsesWSEvent(t, actor)
	sendResult, ok := event.(ResponsesWSEventSendResult)
	if !ok {
		t.Fatalf("expected fallback send result event, got %T", event)
	}
	if sendResult.Purpose != ResponsesWSSendPurposeResponseCancel ||
		sendResult.AttemptID != "attempt-fallback-cancel" ||
		sendResult.TransportResult.Status != responsesws.ResponsesWSTransportSendAttempted {
		t.Fatalf("expected fallback cancel send result to remain cancel-scoped, got %+v", sendResult)
	}
}

func TestResponsesWSClientCancelDuringInFlightUsesActiveAttemptControlLane(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	session := &responsesWSControlLaneTestSession{
		controlCalled:   make(chan struct{}),
		controlContexts: make(chan context.Context, 1),
		controlRequests: make(chan responsesws.SendRequest, 1),
		controlFrames:   make(chan responsesws.Frame, 1),
	}
	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()
	actor.AttachUpstreamSession(session, 17)
	actor.turns.active.attempt = &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-active-cancel",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	payload := []byte(`{"type":"response.cancel"}`)
	actor.handleClientCancel(responsesWSTestClientTextFrame(payload))

	select {
	case <-session.controlCalled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for in-flight client cancel control send")
	}
	select {
	case req := <-session.controlRequests:
		if req.AttemptID != "attempt-active-cancel" {
			t.Fatalf("expected active attempt id on cancel control request, got %q", req.AttemptID)
		}
	default:
		t.Fatal("expected cancel control request")
	}
	select {
	case frame := <-session.controlFrames:
		if frame.Kind() != responsesws.FrameKindText || string(frame.Payload()) != string(payload) {
			t.Fatalf("expected cancel control frame to preserve payload, got kind=%v payload=%s", frame.Kind(), frame.Payload())
		}
	default:
		t.Fatal("expected cancel control frame")
	}
	if calls := atomic.LoadInt32(&session.controlCalls); calls != 1 {
		t.Fatalf("expected exactly one provider control call, got %d", calls)
	}
}

func TestResponsesWSClientCancelWithoutProviderTurnDoesNotReachProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	session := &responsesWSControlLaneTestSession{
		controlCalled: make(chan struct{}),
	}
	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()
	actor.AttachUpstreamSession(session, 17)
	actor.state = responsesWSStateIdle

	actor.handleClientCancel(responsesWSTestClientTextFrame([]byte(`{"type":"response.cancel"}`)))

	if calls := atomic.LoadInt32(&session.controlCalls); calls != 0 {
		t.Fatalf("expected idle client cancel not to call provider control, got %d", calls)
	}
	select {
	case <-session.controlCalled:
		t.Fatal("idle client cancel must not enqueue provider control")
	default:
	}
}

func TestResponsesWSPendingSendCancelReplaysAfterCreateAttempted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	session := &responsesWSControlLaneTestSession{
		createStarted: make(chan struct{}),
		releaseCreate: make(chan struct{}),
		controlCalled: make(chan struct{}),
	}
	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()
	generation := actor.AttachUpstreamSession(session, 17)
	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-pending-send-cancel",
		SelectedChannelID: 17,
		Candidate:         &ResponsesTurnAffinity{},
		Quota:             &relay_util.Quota{},
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend
	actor.setPendingSendCancel(attempt.AttemptID, func() {
		cancelOnce.Do(func() {
			close(cancelled)
		})
	})

	actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.cancel"}`)))
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("expected pending create send context to be canceled")
	}
	select {
	case <-session.controlCalled:
	case <-time.After(time.Second):
		t.Fatal("expected early control cancel attempt")
	}
	if !actor.hasPendingCreateCancel(attempt.AttemptID) {
		t.Fatal("expected pending cancel marker to survive early control send")
	}
	if got := atomic.LoadInt32(&session.controlCalls); got != 1 {
		t.Fatalf("expected early control cancel attempt, got %d", got)
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAttempted,
		},
	})
	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != attempt || actor.state != responsesWSStateInFlight {
		t.Fatalf("expected pending attempt to commit before replayed cancel, pending=%+v active=%+v state=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.state)
	}
	if actor.hasPendingCreateCancel(attempt.AttemptID) {
		t.Fatal("expected pending cancel marker to be consumed after replay")
	}
	waitResponsesWSTestCondition(t, time.Second, time.Millisecond, func() bool {
		return atomic.LoadInt32(&session.controlCalls) >= 2
	}, func() string {
		return fmt.Sprintf("expected replayed control cancel after create attempted, got %d calls", atomic.LoadInt32(&session.controlCalls))
	})
}

func TestResponsesWSOpeningCancelCancelsSetup(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	actor.setSetupCancel(func() {
		cancelOnce.Do(func() {
			close(cancelled)
		})
	})
	actor.ReserveFirstTurnOpening(&responsesws.RawResponsesCreateFrame{})

	actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.cancel"}`)))

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("expected opening cancel to cancel setup")
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected opening cancel to close actor without adopting open result")
	}
}

func TestResponsesWSIdleCancelDoesNotReachProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	session := &responsesWSSendResultTestSession{
		result: responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAttempted},
	}
	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()
	actor.AttachUpstreamSession(session, 17)
	actor.turns.pending.phase = responsesWSPendingTurnNone
	actor.state = responsesWSStateIdle

	actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.cancel"}`)))

	if atomic.LoadInt32(&session.resultCalls) != 0 {
		t.Fatalf("expected idle cancel not to reach provider, result_calls=%d", session.resultCalls)
	}
}

func TestResponsesWSSendWorkerPrefersTypedSendResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	actor.upstream.sessionGeneration = "generation-typed"
	session := &responsesWSSendResultTestSession{
		result: responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAttempted},
	}

	actor.handleSendCommand(responsesWSSendCommand{
		AttemptID:                 "attempt-typed",
		UpstreamSessionGeneration: "generation-typed",
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		Session:                   session,
		Frame:                     responsesWSFrameFromWireMessage(responsesWSTextMessageType, []byte(`{"type":"response.create"}`)),
	})

	event := readResponsesWSEvent(t, actor)
	sendResult, ok := event.(ResponsesWSEventSendResult)
	if !ok {
		t.Fatalf("expected send result event, got %T", event)
	}
	if sendResult.TransportResult.Status != responsesws.ResponsesWSTransportSendAttempted || sendResult.TransportResult.Err != nil {
		t.Fatalf("expected attempted transport send result, got %+v", sendResult)
	}
	if sendResult.UpstreamSessionGeneration != "generation-typed" || sendResult.Purpose != ResponsesWSSendPurposeResponseCreate {
		t.Fatalf("expected generation and purpose to be preserved, got %+v", sendResult)
	}
	if atomic.LoadInt32(&session.resultCalls) != 1 {
		t.Fatalf("expected typed send path only, result_calls=%d", session.resultCalls)
	}
}

func TestResponsesWSSendWorkerUsesRequiredTypedSendResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	session := &responsesWSTestSession{}

	actor.handleSendCommand(responsesWSSendCommand{
		AttemptID:         "attempt-legacy",
		SelectedChannelID: 17,
		Purpose:           ResponsesWSSendPurposeResponseCreate,
		Session:           session,
		Frame:             responsesWSFrameFromWireMessage(responsesWSTextMessageType, []byte(`{"type":"response.create"}`)),
	})

	event := readResponsesWSEvent(t, actor)
	sendResult, ok := event.(ResponsesWSEventSendResult)
	if !ok {
		t.Fatalf("expected send result event, got %T", event)
	}
	if sendResult.TransportResult.Status != responsesws.ResponsesWSTransportSendAttempted || sendResult.TransportResult.Err != nil {
		t.Fatalf("expected attempted transport result, got %+v", sendResult)
	}
}

func TestResponsesWSSendWorkerReportsUnknownTypedStatusAsContractViolation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	session := &responsesWSSendResultTestSession{
		result: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendStatus("future_status"),
			Err:    errors.New("future status"),
		},
	}

	actor.handleSendCommand(responsesWSSendCommand{
		AttemptID:         "attempt-unknown",
		SelectedChannelID: 17,
		Purpose:           ResponsesWSSendPurposeResponseCreate,
		Session:           session,
		Frame:             responsesWSFrameFromWireMessage(responsesWSTextMessageType, []byte(`{"type":"response.create"}`)),
	})

	event := readResponsesWSEvent(t, actor)
	violation, ok := event.(ResponsesWSEventTransportContractViolation)
	if !ok {
		t.Fatalf("expected transport contract violation event, got %T", event)
	}
	if violation.TransportResult.Status != responsesws.ResponsesWSTransportSendStatus("future_status") ||
		!errors.Is(violation.Err, responsesws.ErrInvalidResponsesWSTransportSendResult) {
		t.Fatalf("expected invalid transport result to fail loud, got %+v", violation)
	}
}

func TestResponsesWSSendWorkerTreatsNotAttemptedReasonAsNotSent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	session := &responsesWSSendResultTestSession{
		result: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Reason: responsesws.ResponsesWSTransportSendReasonNoActiveBridgeCancel,
		},
	}

	actor.handleSendCommand(responsesWSSendCommand{
		AttemptID:         "attempt-cancel-noop",
		SelectedChannelID: 17,
		Purpose:           ResponsesWSSendPurposeResponseCancel,
		Session:           session,
		Frame:             responsesWSFrameFromWireMessage(responsesWSTextMessageType, []byte(`{"type":"response.cancel"}`)),
	})

	event := readResponsesWSEvent(t, actor)
	sendResult, ok := event.(ResponsesWSEventSendResult)
	if !ok {
		t.Fatalf("expected send result event, got %T", event)
	}
	if sendResult.TransportResult.Status != responsesws.ResponsesWSTransportSendNotAttempted {
		t.Fatalf("expected no-op not_attempted reason to stay not_attempted, got %+v", sendResult)
	}
}

func TestResponsesWSActorProviderJournalUsesTypedOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	actor.upstream.sessionGeneration = "generation-a"
	actor.upstream.channelID = 17
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-a",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	if err := actor.BeginCandidate(attempt); err != nil {
		t.Fatalf("begin candidate: %v", err)
	}
	if !actor.turns.pending.provider.journal.Project().IsZero() {
		t.Fatalf("expected fresh pending evidence state, got %+v", actor.turns.pending.provider.journal.Project())
	}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: "stale-generation",
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.created","response":{"id":"resp_stale"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})
	if actor.turns.pending.provider.journal.Project().HasActivity() {
		t.Fatal("expected stale generation provider event not to update pending evidence")
	}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: "generation-a",
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.created","response":{"id":"resp_1"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})
	if !actor.turns.pending.provider.journal.Project().HasActivity() ||
		actor.turns.pending.provider.journal.Project().LastActivityOrigin() != responsesws.RecvDetailOriginProviderFrame {
		t.Fatalf("expected typed provider frame evidence, got %+v", actor.turns.pending.provider.journal.Project())
	}

	actor.commitPendingAttempt(attempt)
	if !actor.turns.pending.provider.journal.Project().IsZero() || !actor.turns.active.evidence.HasActivity() {
		t.Fatalf("expected evidence projection to move from pending to active, pending=%+v active=%+v", actor.turns.pending.provider.journal.Project(), actor.turns.active.evidence)
	}
	actor.clearActiveTurn()
	if !actor.turns.active.evidence.IsZero() {
		t.Fatalf("expected active evidence to clear after turn teardown, got %+v", actor.turns.active.evidence)
	}

	next := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-b",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	if err := actor.BeginCandidate(next); err != nil {
		t.Fatalf("begin next candidate: %v", err)
	}
	if !actor.turns.pending.provider.journal.Project().IsZero() {
		t.Fatalf("expected sequential turn to get fresh evidence state, got %+v", actor.turns.pending.provider.journal.Project())
	}
}

func TestResponsesWSActorProviderJournalRejectsUnknownDetailOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	actor.upstream.sessionGeneration = "generation-a"
	actor.upstream.channelID = 17
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-a",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	if err := actor.BeginCandidate(attempt); err != nil {
		t.Fatalf("begin candidate: %v", err)
	}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: "generation-a",
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`)),
		DetailOrigin:              responsesws.RecvDetailOrigin("future_origin"),
	})
	if actor.turns.pending.provider.journal.Project().HasActivity() {
		t.Fatalf("expected unknown detail origin not to update provider evidence, got %+v", actor.turns.pending.provider.journal.Project())
	}
}

func TestResponsesWSActorProviderEvidenceRequiresGenerationWhenBound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	actor.upstream.sessionGeneration = "generation-a"
	actor.upstream.channelID = 17
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-a",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	if err := actor.BeginCandidate(attempt); err != nil {
		t.Fatalf("begin candidate: %v", err)
	}

	actor.handleProviderUsageObserved(ResponsesWSEventProviderUsageObserved{
		AttemptID:    responsesWSTestCurrentAttemptID(actor),
		ChannelID:    17,
		Usage:        &types.UsageEvent{InputTokens: 4, TotalTokens: 4},
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	})
	if actor.turns.pending.provider.journal.Project().HasActivity() || attempt.Usage.TotalTokens != 0 {
		t.Fatalf("expected missing generation evidence not to update state/accounting, evidence=%+v usage=%+v", actor.turns.pending.provider.journal.Project(), attempt.Usage)
	}
}

func TestResponsesWSWriteProxyLocalFailurePostsCloseIntent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{
		reads:    make(chan responsesWSReadResult, 1),
		writeErr: errors.New("client write failed"),
	}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))

	actor.writeProxyLocal([]byte(`{"type":"error"}`))
	if actor.closing.closed.Load() {
		t.Fatal("expected proxy-local write failure to post a close intent instead of closing directly")
	}

	event := readResponsesWSEvent(t, actor)
	closeIntent, ok := event.(ResponsesWSEventCloseIntent)
	if !ok {
		t.Fatalf("expected close intent event, got %T", event)
	}
	if closeIntent.Reason != "client_write_failed" {
		t.Fatalf("unexpected close intent reason %q", closeIntent.Reason)
	}

	actor.handleEvent(closeIntent)
	if !actor.closing.closed.Load() {
		t.Fatal("expected actor loop close intent handling to close session")
	}
}

func TestResponsesWSRequestCloseIntentIsOneShotPerActor(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()

	actor.requestCloseIntent("first_close")
	actor.requestCloseIntent("second_close")
	actor.requestCloseIntent("third_close")

	event := readResponsesWSEvent(t, actor)
	closeIntent, ok := event.(ResponsesWSEventCloseIntent)
	if !ok {
		t.Fatalf("expected close intent event, got %T", event)
	}
	if closeIntent.Reason != "first_close" {
		t.Fatalf("expected first close reason to win, got %q", closeIntent.Reason)
	}
	select {
	case extra := <-actor.events:
		t.Fatalf("expected repeated close intents to be suppressed, got %T", extra)
	default:
	}
}

func TestResponsesWSActorCloseSendsNormalCloseControl(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.close("test_close_reason")

	if atomic.LoadInt32(&conn.controlCount) == 0 {
		t.Fatal("expected close control frame before connection close")
	}
	if got, _ := conn.lastControl.Load().(string); !strings.Contains(got, "test_close_reason") {
		t.Fatalf("expected close control reason to be preserved, got %q", got)
	}
}

func TestResponsesWSActorStoresContextSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("id", 7)

	actor := NewResponsesWSSessionActor(ctx)
	if actor.Context() == ctx {
		t.Fatalf("expected actor to store a context snapshot, not the live handler context")
	}
	if got := actor.Context().GetInt("id"); got != 7 {
		t.Fatalf("expected actor context snapshot to preserve request values, got id=%d", got)
	}

	ctx.Set("id", 8)
	if got := actor.Context().GetInt("id"); got != 7 {
		t.Fatalf("expected existing actor snapshot not to track live context mutation, got id=%d", got)
	}

	actor.RefreshContext(ctx)
	if actor.Context() == ctx {
		t.Fatalf("expected refreshed actor context to remain a snapshot")
	}
	if got := actor.Context().GetInt("id"); got != 8 {
		t.Fatalf("expected refreshed actor context to include new values, got id=%d", got)
	}
}

func TestResponsesWSNoTurnProviderEventFailsClosedAndAbortsSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	actor.SetBridge(bridge)
	session := &responsesWSTestSession{}
	upstreamSessionGeneration := actor.AttachUpstreamSession(session, 17)

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: upstreamSessionGeneration,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_no_turn"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if !actor.closing.closed.Load() {
		t.Fatalf("expected provider event without turn to fail closed")
	}
	if session.abortReason != "responses_ws_provider_event_without_turn" {
		t.Fatalf("expected session abort on no-turn provider event, got %q", session.abortReason)
	}
	if actor.turns.history.lastFinal != nil || actor.turns.active.attempt != nil {
		t.Fatalf("expected no terminal classification or active turn commit, actor=%+v", actor)
	}
}

func TestResponsesWSProviderCloseAfterTerminalIsForwarded(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(conn, actor)
	actor.SetBridge(bridge)
	session := &responsesWSTestSession{}
	upstreamSessionGeneration := actor.AttachUpstreamSession(session, 17)
	actor.turns.history.lastFinal = &types.OpenAIResponsesResponses{ID: "resp_done"}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: upstreamSessionGeneration,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamClose,
		CloseCode:                 int(wsconn.CloseNormalClosure),
		CloseReason:               "bye",
		DetailOrigin:              responsesws.RecvDetailOriginNativeProviderClose,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected provider close to end the actor")
	}
	if session.abortReason == "responses_ws_provider_event_without_turn" {
		t.Fatalf("expected provider close after terminal not to be classified as protocol violation, got %q", session.abortReason)
	}
	if got := atomic.LoadInt32(&conn.controlCount); got != 1 {
		t.Fatalf("expected provider close frame to be forwarded once, got %d", got)
	}
}

func TestResponsesWSNativeProviderClosedAfterTurnClearedClosesSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.history.lastFinal = &types.OpenAIResponsesResponses{ID: "resp_done"}
	actor.state = responsesWSStateIdle

	actor.handleProviderClosed(ResponsesWSEventProviderClosed{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 "attempt-completed",
		Code:                      int(wsconn.CloseNormalClosure),
		Reason:                    "provider idle close",
		DetailOrigin:              responsesws.RecvDetailOriginNativeProviderClose,
		DetailPhase:               responsesws.RecvDetailPhaseMapProviderClose,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected idle native provider close to close the actor")
	}
	if session.abortReason != "provider_closed" {
		t.Fatalf("expected upstream abort reason provider_closed, got %q", session.abortReason)
	}
}

func TestResponsesWSNativeRecvFailureAfterTurnClearedClosesSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.history.lastFinal = &types.OpenAIResponsesResponses{ID: "resp_done"}
	actor.state = responsesWSStateIdle

	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Err:                       io.EOF,
		DetailOrigin:              responsesws.RecvDetailOriginNativeProviderEOF,
		DetailPhase:               responsesws.RecvDetailPhaseMapProviderClose,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected idle native provider EOF to close the actor")
	}
	if session.abortReason != "provider_recv_failed" {
		t.Fatalf("expected upstream abort reason provider_recv_failed, got %q", session.abortReason)
	}
}

func TestResponsesWSProviderRecvFailedDerivesCoarseOrigin(t *testing.T) {
	event := ResponsesWSEventProviderRecvFailed{
		AttemptID:    "attempt-origin",
		DetailOrigin: responsesws.RecvDetailOriginNativeProviderEOF,
		DetailPhase:  responsesws.RecvDetailPhaseMapProviderClose,
		Err:          io.EOF,
	}

	upstreamEvent := upstreamEventFromProviderRecvFailed(event)
	if responsesws.PayloadOriginForDetailOrigin(upstreamEvent.DetailOrigin) != responsesws.PayloadOriginProvider {
		t.Fatalf("expected provider coarse origin to be derived, got %d", responsesws.PayloadOriginForDetailOrigin(upstreamEvent.DetailOrigin))
	}
	expected, ok := responsesws.ExpectedPayloadOriginForRecvDetailOrigin(upstreamEvent.DetailOrigin)
	if !ok || expected != responsesws.PayloadOriginProvider || responsesws.PayloadOriginForDetailOrigin(upstreamEvent.DetailOrigin) != responsesws.PayloadOriginProvider {
		t.Fatalf("expected coarse/detail origin combination to be valid, got %+v", upstreamEvent)
	}
	if responsesws.UpstreamEventHasProviderEvidence(upstreamEvent) {
		t.Fatal("native provider EOF must not become provider request evidence")
	}
}

func TestResponsesWSBridgeStreamEOFAfterTurnClearedDoesNotCloseSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.history.lastFinal = &types.OpenAIResponsesResponses{ID: "resp_done"}
	actor.state = responsesWSStateIdle

	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		DetailOrigin:              responsesws.RecvDetailOriginBridgeStreamEOF,
	})

	if actor.closing.closed.Load() {
		t.Fatal("expected idle bridge stream EOF from an old turn not to close the session")
	}
	if session.abortReason != "" {
		t.Fatalf("expected upstream session to remain open, got abort reason %q", session.abortReason)
	}
}

func TestResponsesWSBridgeStreamLineLimitFailureSendsSafeProtocolError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-line-limit")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Err:                       requester.ErrStreamLineTooLarge,
		DetailOrigin:              responsesws.RecvDetailOriginBridgeStreamError,
	})

	got, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, got, http.StatusBadGateway, "responses_ws_provider_protocol_error", "stream line exceeds configured read limit")
	if !actor.closing.closed.Load() || session.abortReason != "provider_recv_failed" {
		t.Fatalf("expected line-limit bridge failure to close session safely, closed=%v abort=%q", actor.closing.closed.Load(), session.abortReason)
	}
}

func TestResponsesWSBridgeMalformedStreamPayloadSendsSafeProtocolError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-malformed-bridge-stream")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Err:                       responsesws.ErrInvalidBridgeStreamPayload,
		DetailOrigin:              responsesws.RecvDetailOriginBridgeStreamError,
	})

	got, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, got, http.StatusBadGateway, "responses_ws_provider_protocol_error", "malformed responses websocket bridge stream payload")
	if strings.Contains(got, "data:") {
		t.Fatalf("expected malformed bridge payload error not to include raw upstream data, got %q", got)
	}
	if !actor.closing.closed.Load() || session.abortReason != "provider_recv_failed" {
		t.Fatalf("expected malformed bridge stream failure to close session safely, closed=%v abort=%q", actor.closing.closed.Load(), session.abortReason)
	}
}

func TestResponsesWSProviderClosedFinalizesActiveAttemptAndForwardsSanitizedCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(conn, actor)
	actor.SetBridge(bridge)
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	lease := &responsesWSTestLease{}
	actor.lease.activeLease = lease
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderClosed(ResponsesWSEventProviderClosed{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Code:                      4408,
		Reason:                    "quota exhausted",
		Err:                       errors.New("provider close"),
		ReceivedAt:                time.Now(),
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected provider close to close actor")
	}
	if actor.turns.active.attempt != nil || actor.turns.active.channelID != 0 || actor.state != responsesWSStateClosed {
		t.Fatalf("expected provider close to clear active turn, active=%v channel=%d state=%v", actor.turns.active.attempt != nil, actor.turns.active.channelID, actor.state)
	}
	if !attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected provider close to finalize quota without rollback, finalized=%v rolled=%v", attempt.QuotaFinalized, attempt.RolledBack)
	}
	if channel := readResponsesWSChannelFixture(t); channel.UsedQuota != 100 {
		t.Fatalf("expected provider close to update channel used quota, got %d", channel.UsedQuota)
	}
	if got := atomic.LoadInt32(&lease.releases); got != 1 {
		t.Fatalf("expected provider close to release active lease once, got %d", got)
	}
	if got := atomic.LoadInt32(&conn.controlCount); got != 1 {
		t.Fatalf("expected one downstream close frame, got %d", got)
	}
	payload, _ := conn.lastControl.Load().(string)
	if len(payload) < 2 {
		t.Fatalf("expected downstream close payload, got %q", payload)
	}
	if code := int(binary.BigEndian.Uint16([]byte(payload)[:2])); code != 4408 {
		t.Fatalf("expected provider close code 4408 to be forwarded, got %d", code)
	}
	if reason := payload[2:]; reason != "quota exhausted" {
		t.Fatalf("expected provider close reason to be forwarded, got %q", reason)
	}
	if session.abortReason != "provider_closed" {
		t.Fatalf("expected provider close to abort upstream session during actor close, got %q", session.abortReason)
	}
}

func TestResponsesWSProviderClosedWriteFailureStillSendsCloseControl(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1), closeWriteErr: errors.New("close sentinel write failed")}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderClosed(ResponsesWSEventProviderClosed{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Code:                      4408,
		Reason:                    "quota exhausted",
		Err:                       errors.New("provider close"),
		ReceivedAt:                time.Now(),
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected provider close write failure to close actor")
	}
	if got := atomic.LoadInt32(&conn.controlCount); got != 1 {
		t.Fatalf("expected fallback downstream close control after write failure, got %d", got)
	}
	payload, _ := conn.lastControl.Load().(string)
	if !strings.Contains(payload, "client_write_failed") {
		t.Fatalf("expected fallback close control to carry client_write_failed, got %q", payload)
	}
	if !attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected provider close write failure to preserve finalized quota, finalized=%v rolled=%v", attempt.QuotaFinalized, attempt.RolledBack)
	}
}

func TestResponsesWSProviderClosedUsesManagedClientCloseWhenAvailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)

	client, server := wstest.Pair(t)
	defer client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
	defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(conn, actor)
	actor.SetBridge(bridge)
	actor.SetClientConn(server)
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	lease := &responsesWSTestLease{}
	actor.lease.activeLease = lease
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderClosed(ResponsesWSEventProviderClosed{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Code:                      4408,
		Reason:                    "quota exhausted",
		Err:                       errors.New("provider close"),
		ReceivedAt:                time.Now(),
	})

	_, _, err := client.ReadInitial(context.Background())
	var closeErr *wsconn.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected managed client to observe close error, got %T %v", err, err)
	}
	if closeErr.Code != 4408 || closeErr.Reason != "quota exhausted" {
		t.Fatalf("expected managed client close 4408 quota exhausted, got %+v", closeErr)
	}
	if got := atomic.LoadInt32(&conn.controlCount); got != 0 {
		t.Fatalf("expected managed close path not to write bridge close frame, got %d", got)
	}
	if got := atomic.LoadInt32(&lease.releases); got != 1 {
		t.Fatalf("expected provider close to release active lease once, got %d", got)
	}
	if !attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected provider close to finalize quota without rollback, finalized=%v rolled=%v", attempt.QuotaFinalized, attempt.RolledBack)
	}
	if session.abortReason != "provider_closed" {
		t.Fatalf("expected provider close to abort upstream session during actor close, got %q", session.abortReason)
	}
}

func TestResponsesWSProviderClosedDuringPendingAttemptIsBuffered(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(conn, actor)
	actor.SetBridge(bridge)
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.pending.attempt = &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-provider-close-pending",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.state = responsesWSStatePendingSend

	actor.handleProviderClosed(ResponsesWSEventProviderClosed{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Code:                      4408,
		Reason:                    "quota exhausted",
		DetailOrigin:              responsesws.RecvDetailOriginNativeProviderClose,
		ReceivedAt:                time.Now(),
	})

	if actor.closing.closed.Load() {
		t.Fatal("expected pending provider close to be buffered without closing actor")
	}
	if got := atomic.LoadInt32(&conn.controlCount); got != 0 {
		t.Fatalf("expected no downstream close while provider close is pending, got %d", got)
	}
	if !actor.hasPendingProviderEvidence() || len(actor.turns.pending.provider.journal.DownstreamEvents()) != 1 {
		t.Fatalf("expected provider close to be buffered as pending evidence, evidence=%v events=%d", actor.hasPendingProviderEvidence(), len(actor.turns.pending.provider.journal.DownstreamEvents()))
	}
	buffered := actor.turns.pending.provider.journal.DownstreamEvents()[0]
	if buffered.Kind != ProviderDownstreamClose || buffered.CloseCode != 4408 || buffered.CloseReason != "quota exhausted" || responsesws.PayloadOriginForDetailOrigin(buffered.DetailOrigin) != responsesws.PayloadOriginProvider {
		t.Fatalf("expected buffered provider close event, got %+v", buffered)
	}
}

func TestResponsesWSProviderClosedInvalidCodeIsSanitized(t *testing.T) {
	payload := responsesWSProviderClosePayload(1006, "abnormal")
	if len(payload) < 2 {
		t.Fatalf("expected close payload, got %q", payload)
	}
	if code := int(binary.BigEndian.Uint16(payload[:2])); code != int(wsconn.CloseInternalServerErr) {
		t.Fatalf("expected invalid provider code to sanitize to 1011, got %d", code)
	}
}

func TestResponsesWSAmbiguousSendWithBufferedProviderEventWaitsForTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-ambiguous")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(conn, actor)
	actor.SetBridge(bridge)
	session := &responsesWSTestSession{}
	upstreamSessionGeneration := actor.AttachUpstreamSession(session, 17)
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: upstreamSessionGeneration,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_buffered"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})
	if !actor.hasPendingProviderEvidence() || len(actor.turns.pending.provider.journal.DownstreamEvents()) != 1 {
		t.Fatalf("expected buffered provider evidence before send result")
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         "attempt-ambiguous",
		SelectedChannelID: 17,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("write timeout"),
		},
	})

	if actor.closing.closed.Load() {
		t.Fatalf("expected ambiguous send with provider evidence to stay open")
	}
	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_buffered" {
		t.Fatalf("expected buffered terminal to be consumed, got %+v", actor.turns.history.lastFinal)
	}
	if got, _ := conn.lastWrite.Load().(string); strings.Contains(got, "responses_ws_send_ambiguous") {
		t.Fatalf("expected no proxy-local ambiguous error after provider evidence, got %q", got)
	}
	if actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected terminal to clear active attempt, state=%v active=%+v", actor.state, actor.turns.active.attempt)
	}
}

func TestResponsesWSAmbiguousSendWithProviderClosePreservesQuotaFloorWithoutTerminal(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	attempt.AttemptID = "attempt-ambiguous-close"
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	actor.handleProviderClosed(ResponsesWSEventProviderClosed{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Code:                      int(wsconn.CloseGoingAway),
		Reason:                    "provider closed",
		DetailOrigin:              responsesws.RecvDetailOriginNativeProviderClose,
		ReceivedAt:                time.Now(),
	})
	if !actor.hasPendingProviderEvidence() {
		t.Fatal("expected native provider close to count as pending provider evidence")
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         "attempt-ambiguous-close",
		SelectedChannelID: 17,
		Purpose:           ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("write timeout"),
		},
	})

	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected provider evidence without terminal to preserve quota floor, rolled=%v finalized=%v", attempt.RolledBack, attempt.QuotaFinalized)
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected provider close without terminal payload not to record provider terminal, got %+v", actor.turns.history.lastFinal)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected provider close replay to close actor")
	}
	if got, _ := conn.lastWrite.Load().(string); strings.Contains(got, "ambiguous_close_no_provider_evidence") {
		t.Fatalf("expected no no-evidence ambiguous error after provider close evidence, got %q", got)
	}
}

func TestResponsesWSBridgeStreamOpenedThenEOFUsesProviderEvidencePolicy(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-bridge-opened")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		DetailOrigin:              responsesws.RecvDetailOriginBridgeStreamOpened,
		ReceivedAt:                time.Now(),
	})
	if !actor.hasPendingProviderEvidence() {
		t.Fatal("expected bridge_stream_opened to count as pending provider evidence")
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         "attempt-bridge-opened",
		SelectedChannelID: 17,
		Purpose:           ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("write timeout"),
		},
	})
	if actor.closing.closed.Load() || actor.turns.active.attempt != attempt {
		t.Fatalf("expected bridge open evidence to keep turn active after ambiguous send, closed=%v active=%+v", actor.closing.closed.Load(), actor.turns.active.attempt)
	}

	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		DetailOrigin:              responsesws.RecvDetailOriginBridgeStreamEOF,
	})
	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected bridge EOF after open evidence to preserve quota floor, rolled=%v finalized=%v", attempt.RolledBack, attempt.QuotaFinalized)
	}
	if got, _ := conn.lastWrite.Load().(string); strings.Contains(got, "ambiguous_close_no_provider_evidence") {
		t.Fatalf("expected no no-evidence ambiguous error after bridge open evidence, got %q", got)
	}
}

func TestResponsesWSRecvFailureUsesAccumulatedProviderEvidence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		firstOrigin  responsesws.RecvDetailOrigin
		failOrigin   responsesws.RecvDetailOrigin
		firstPayload string
	}{
		{
			name:         "provider stream then bridge stream error",
			firstOrigin:  responsesws.RecvDetailOriginProviderStream,
			failOrigin:   responsesws.RecvDetailOriginBridgeStreamError,
			firstPayload: `{"type":"response.created","response":{"id":"resp_stream","status":"in_progress"}}`,
		},
		{
			name:         "provider frame then native eof",
			firstOrigin:  responsesws.RecvDetailOriginProviderFrame,
			failOrigin:   responsesws.RecvDetailOriginNativeProviderEOF,
			firstPayload: `{"type":"response.created","response":{"id":"resp_native","status":"in_progress"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-accumulated-evidence")

			conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
			actor := NewResponsesWSSessionActor(ctx)
			actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
			generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
			actor.turns.active.attempt = attempt
			actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
			actor.turns.active.channelID = 17
			actor.state = responsesWSStateInFlight

			actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
				AttemptID:                 responsesWSTestCurrentAttemptID(actor),
				UpstreamSessionGeneration: generation,
				ChannelID:                 17,
				Kind:                      ProviderDownstreamFrame,
				Frame:                     responsesWSTestProviderTextFrame([]byte(tc.firstPayload)),
				DetailOrigin:              tc.firstOrigin,
				ReceivedAt:                time.Now(),
			})
			if !actor.turns.active.evidence.HasActivity() {
				t.Fatalf("expected first provider event to update accumulated evidence, got %+v", actor.turns.active.evidence)
			}

			actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
				AttemptID:                 responsesWSTestCurrentAttemptID(actor),
				UpstreamSessionGeneration: generation,
				ChannelID:                 17,
				Err:                       errors.New("stream ended"),
				DetailOrigin:              tc.failOrigin,
			})
			if attempt.RolledBack || !attempt.QuotaFinalized {
				t.Fatalf("expected accumulated provider evidence to preserve quota floor on recv failure, rolled=%v finalized=%v", attempt.RolledBack, attempt.QuotaFinalized)
			}
			if got, _ := conn.lastWrite.Load().(string); strings.Contains(got, "ambiguous_close_no_provider_evidence") {
				t.Fatalf("expected recv failure not to use no-evidence ambiguous policy, got %q", got)
			}
		})
	}
}

func TestResponsesWSAdapterPanicDuringProviderFrameRecordsActivityWithoutTerminal(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	attempt.AttemptID = "attempt-adapter-panic"
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderBusinessError(ResponsesWSEventProviderBusinessError{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Err:                       errors.New("adapter panic recovered"),
		DetailOrigin:              responsesws.RecvDetailOriginAdapterPanic,
		DetailPhase:               responsesws.RecvDetailPhaseHandleProviderFrame,
	})

	if !actor.turns.active.evidence.HasActivity() {
		t.Fatalf("expected adapter panic in provider-frame phase to record provider activity, got %+v", actor.turns.active.evidence)
	}
	if actor.turns.active.evidence.LastActivityOrigin() != responsesws.RecvDetailOriginAdapterPanic {
		t.Fatalf("expected adapter panic origin, got %q", actor.turns.active.evidence.LastActivityOrigin())
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected no provider terminal side effect, got %+v", actor.turns.history.lastFinal)
	}
	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected adapter panic with provider activity to preserve quota floor, rolled=%v finalized=%v", attempt.RolledBack, attempt.QuotaFinalized)
	}
}

func TestResponsesWSAdapterPanicDuringPrepareDoesNotProjectProviderActivity(t *testing.T) {
	attempt := &ResponsesWSTurnAttempt{
		AttemptID: "attempt-adapter-panic-prepare",
		Quota:     &relay_util.Quota{},
		Usage:     &types.Usage{},
	}
	projection := responsesWSTestProviderProjection(responsesws.UpstreamEvent{
		DetailOrigin: responsesws.RecvDetailOriginAdapterPanic,
		DetailPhase:  responsesws.RecvDetailPhasePrepareClientFrame,
	})
	actor := &ResponsesWSSessionActor{}
	input := actor.buildSettlementInputFromAttempt(attempt, projection, "adapter_panic_prepare_client_frame", ResponsesWSZeroChargeProof{})
	if input.Evidence.AnyProviderActivityEvidence {
		t.Fatalf("expected prepare adapter panic not to project provider activity, input=%+v projection=%+v", input, projection)
	}
	decision := decideResponsesWSSettlement(input)
	if decision.Action != ResponsesWSSettlementFinalizeFloor || !responsesWSSettlementHasFlag(decision, ResponsesWSSettlementFlagMissingSettlementFloor) {
		t.Fatalf("expected prepare adapter panic to remain no-proof local floor path, got %+v", decision)
	}
}

func TestResponsesWSBridgeOpenProviderErrorRollsBackPendingQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalProcess := processChannelRelayErrorFunc
	errCh := make(chan *types.OpenAIErrorWithStatusCode, 1)
	processChannelRelayErrorFunc = func(_ context.Context, _ int, _ string, apiErr *types.OpenAIErrorWithStatusCode, _ int) {
		errCh <- apiErr
	}
	defer func() {
		processChannelRelayErrorFunc = originalProcess
	}()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Name: "bridge-provider-error", Type: config.ChannelTypeOpenAI})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-bridge-open-error",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		Payload:                   responsesWSErrorPayload(http.StatusTooManyRequests, "provider_rate_limit", "provider rejected stream open"),
		ProviderAPIError: &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{
				Type:    "rate_limit_error",
				Code:    "rate_limit_exceeded",
				Message: "provider rejected stream open",
			},
			StatusCode: http.StatusTooManyRequests,
		},
		Recoverable: false,
	})

	if !attempt.RolledBack || attempt.QuotaPreconsumed || attempt.QuotaFinalized {
		t.Fatalf("expected bridge open provider error to rollback pending quota without finalize, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil {
		t.Fatalf("expected bridge open provider error to release attempt state, pending=%+v active=%+v", actor.turns.pending.attempt, actor.turns.active.attempt)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected bridge open provider error to close session")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "provider_rate_limit") {
		t.Fatalf("expected provider rejection payload, got %q", got)
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected bridge open provider error not to submit terminal side effect, got %+v", actor.turns.history.lastFinal)
	}
	select {
	case apiErr := <-errCh:
		if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Type != "rate_limit_error" || apiErr.Code != "rate_limit_exceeded" {
			t.Fatalf("expected bridge open provider rejection to be processed with provider status/code, got %#v", apiErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bridge open provider error control-plane handling")
	}
}

func TestResponsesWSBridgeOpenProviderErrorStopsActiveTurnWatchdog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setResponsesWSTestViperInt(t, "responses_ws.active_turn_timeout_ms", 30000)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-active-bridge-open-error")
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.evidence = responsesws.ProviderSettlementLogProjection{}
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight
	actor.armActiveTurnWatchdog()
	if actor.watchdog.activeTurnTimer == nil {
		t.Fatal("expected active turn watchdog to be armed before bridge provider error")
	}

	var traces []ResponsesWSSettlementTrace
	originalHook := responsesWSSettlementTraceHook
	responsesWSSettlementTraceHook = func(trace ResponsesWSSettlementTrace) {
		traces = append(traces, trace)
	}
	t.Cleanup(func() {
		responsesWSSettlementTraceHook = originalHook
	})

	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusTooManyRequests, "provider_rate_limit", "provider rejected stream open"),
		Recoverable:               true,
	})

	if actor.watchdog.activeTurnTimer != nil {
		t.Fatal("expected bridge open provider error to stop active turn watchdog before clearing active attempt")
	}
	if actor.turns.active.attempt != nil || actor.state != responsesWSStateClosed || !actor.closing.closed.Load() {
		t.Fatalf("expected active bridge provider error to clear active turn and close session, active=%+v state=%v closed=%v", actor.turns.active.attempt, actor.state, actor.closing.closed.Load())
	}
	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected active bridge provider error to finalize conservatively, attempt=%+v", attempt)
	}
	if len(traces) != 1 {
		t.Fatalf("expected one active settlement trace, got %d", len(traces))
	}
	if traces[0].Input.ZeroChargeProof.Present() ||
		traces[0].Input.ZeroChargeProof.Kind == ResponsesWSZeroChargeProofProviderRejectedBeforeStream ||
		responsesWSTraceHasDetailOrigin(traces[0], responsesws.RecvDetailOriginBridgeOpenProviderError) {
		t.Fatalf("active bridge provider error must not become rejected-before-stream proof, trace=%+v", traces[0])
	}
}

func TestResponsesWSActiveBridgeOpenProviderErrorWithObservedUsageFinalizesObservedOrFloor(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-active-bridge-open-error-observed")
	mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 150, TotalTokens: 150})
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.evidence = responsesWSTestProviderUsageProjection()
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	var traces []ResponsesWSSettlementTrace
	originalHook := responsesWSSettlementTraceHook
	responsesWSSettlementTraceHook = func(trace ResponsesWSSettlementTrace) {
		traces = append(traces, trace)
	}
	t.Cleanup(func() {
		responsesWSSettlementTraceHook = originalHook
	})

	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusTooManyRequests, "provider_rate_limit", "provider rejected stream open"),
		Recoverable:               true,
	})

	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected active observed bridge provider error to finalize, attempt=%+v", attempt)
	}
	if len(traces) != 1 {
		t.Fatalf("expected one active settlement trace, got %d", len(traces))
	}
	trace := traces[0]
	if trace.Decision.Action != ResponsesWSSettlementFinalizeObservedOrFloor ||
		trace.Decision.ExpectedFinalQuota != 150 ||
		trace.Applied.AppliedFinalQuota != 150 ||
		trace.Input.ZeroChargeProof.Present() {
		t.Fatalf("expected observed-or-floor settlement without zero proof, trace=%+v", trace)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected active bridge provider error with observed usage to close session")
	}
}

func TestResponsesWSBridgeLocalOpenErrorBeforeSendResultSettlesFloor(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	attempt.AttemptID = "attempt-bridge-local-open-error"
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleEvent(ResponsesWSEventBridgeOpenLocalError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusBadGateway, "ws_request_failed", "dial failed before provider status"),
		Recoverable:               false,
	})

	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected bridge local open error before send result to settle floor, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil {
		t.Fatalf("expected bridge local open error to release attempt state, pending=%+v active=%+v", actor.turns.pending.attempt, actor.turns.active.attempt)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected non-recoverable bridge local open error to close session")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "ws_request_failed") {
		t.Fatalf("expected local open error payload, got %q", got)
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("late local open failure"),
		},
	})
	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected late send result not to double-settle finalized attempt, attempt=%+v", attempt)
	}
}

func TestResponsesWSBridgeLocalOpenErrorAfterSendResultKeepsPrecisePayload(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	attempt.AttemptID = "attempt-bridge-local-open-error-late-event"
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	localOpenErr := common.StringErrorWrapperLocal("dial failed before provider status", "ws_request_failed", http.StatusBadGateway)
	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    localOpenErr,
		},
	})

	if attempt.RolledBack || attempt.QuotaFinalized {
		t.Fatalf("expected send result to wait for local-open payload before settling quota, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt == nil || actor.turns.pending.provider.bridgeOpenLocalErrorAttemptID != attempt.AttemptID {
		t.Fatalf("expected pending attempt to await bridge local open error, pending=%+v marker=%q", actor.turns.pending.attempt, actor.turns.pending.provider.bridgeOpenLocalErrorAttemptID)
	}
	if got, _ := conn.lastWrite.Load().(string); strings.Contains(got, "ambiguous_close_no_provider_evidence") {
		t.Fatalf("expected no generic ambiguous payload before local-open event, got %q", got)
	}

	actor.handleEvent(ResponsesWSEventBridgeOpenLocalError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusBadGateway, "ws_request_failed", "dial failed before provider status"),
		Recoverable:               false,
	})

	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected late local-open event to settle floor once, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.pending.provider.bridgeOpenLocalErrorAttemptID != "" {
		t.Fatalf("expected local-open event to release pending state, pending=%+v marker=%q", actor.turns.pending.attempt, actor.turns.pending.provider.bridgeOpenLocalErrorAttemptID)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected non-recoverable bridge local open error to close session")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "ws_request_failed") || strings.Contains(got, "ambiguous_close_no_provider_evidence") {
		t.Fatalf("expected precise local open error payload, got %q", got)
	}
}

func TestResponsesWSBridgeLocalOpenErrorWaitTimeoutReleasesPendingState(t *testing.T) {
	originalTimeout := responsesWSBridgeLocalOpenErrorWaitTimeout
	responsesWSBridgeLocalOpenErrorWaitTimeout = -1
	t.Cleanup(func() {
		responsesWSBridgeLocalOpenErrorWaitTimeout = originalTimeout
	})

	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	attempt.AttemptID = "attempt-bridge-local-open-timeout"
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	localOpenErr := common.StringErrorWrapperLocal("dial failed before provider status", "ws_request_failed", http.StatusBadGateway)
	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    localOpenErr,
		},
	})
	if attempt.QuotaFinalized || actor.turns.pending.attempt != attempt || actor.turns.pending.provider.bridgeOpenLocalErrorAttemptID != attempt.AttemptID {
		t.Fatalf("expected ambiguous local-open send result to await precise bridge event, attempt=%+v pending=%+v marker=%q", attempt, actor.turns.pending.attempt, actor.turns.pending.provider.bridgeOpenLocalErrorAttemptID)
	}

	actor.handleTimeout(ResponsesWSEventTimeout{
		Reason:                    responsesWSBridgeLocalOpenErrorWaitTimeoutReason,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
	})

	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected local-open timeout to settle floor, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone || actor.state != responsesWSStateClosed {
		t.Fatalf("expected local-open timeout to release pending state, pending=%+v active=%+v phase=%v state=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.turns.pending.phase, actor.state)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected local-open timeout to close uncertain bridge session")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "ws_request_failed") || strings.Contains(got, "ambiguous_close_no_provider_evidence") {
		t.Fatalf("expected generic ws_request_failed fallback payload, got %q", got)
	}
}

func TestResponsesWSBridgeLocalOpenErrorWaitTimeoutIgnoredAfterBridgeEvent(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	attempt.AttemptID = "attempt-bridge-local-open-timeout-stale"
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	localOpenErr := common.StringErrorWrapperLocal("dial failed before provider status", "ws_request_failed", http.StatusBadGateway)
	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    localOpenErr,
		},
	})
	actor.handleEvent(ResponsesWSEventBridgeOpenLocalError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusBadGateway, "ws_request_failed", "dial failed before provider status"),
		Recoverable:               false,
	})
	precisePayload, _ := conn.lastWrite.Load().(string)

	actor.handleTimeout(ResponsesWSEventTimeout{
		Reason:                    responsesWSBridgeLocalOpenErrorWaitTimeoutReason,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
	})

	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected precise bridge event settlement to survive stale timeout, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); got != precisePayload || !strings.Contains(got, "ws_request_failed") || strings.Contains(got, "ambiguous_close_no_provider_evidence") {
		t.Fatalf("expected stale timeout not to overwrite precise payload, before=%q after=%q", precisePayload, got)
	}
}

func TestResponsesWSBridgeOpenProviderAPIErrorPreservesProviderStatus(t *testing.T) {
	err := responsesws.NewBridgeOpenProviderError(http.StatusUnauthorized, "invalid_api_key", "authentication_error", "bad key")

	apiErr := responsesWSBridgeOpenProviderAPIError(err)
	if apiErr == nil {
		t.Fatal("expected bridge open provider error to convert to provider api error")
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Code != "invalid_api_key" || apiErr.Type != "authentication_error" || apiErr.Message != "bad key" || apiErr.LocalError {
		t.Fatalf("unexpected converted provider api error: %#v", apiErr)
	}
}

func TestResponsesWSRejectedBeforeStreamSendResultRollsBackThenBridgeEventCloses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-bridge-open-send-result",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 "attempt-bridge-open-send-result",
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendRejectedBeforeStream,
		},
	})

	if attempt.RolledBack || !attempt.QuotaPreconsumed || attempt.QuotaFinalized {
		t.Fatalf("expected rejected_before_stream send result to await bridge provider observation before rollback, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != attempt || actor.turns.active.attempt != nil || actor.state != responsesWSStatePendingSend {
		t.Fatalf("expected rejected_before_stream send result to retain pending attempt until bridge event, pending=%+v active=%+v state=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.state)
	}

	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusTooManyRequests, "provider_rate_limit", "provider rejected stream open"),
		Recoverable:               false,
	})

	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil {
		t.Fatalf("expected bridge open provider error to release attempt state, pending=%+v active=%+v state=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.state)
	}
	if !attempt.RolledBack || attempt.QuotaPreconsumed || attempt.QuotaFinalized {
		t.Fatalf("expected bridge open provider observation to rollback quota without finalize, attempt=%+v", attempt)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected bridge open provider error after rejected_before_stream send to close session")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "provider_rate_limit") {
		t.Fatalf("expected provider rejection payload, got %q", got)
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected bridge open provider error not to submit terminal side effect, got %+v", actor.turns.history.lastFinal)
	}
}

func TestResponsesWSRejectedBeforeStreamSendResultFallbackReleasesPendingState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalTimeout := responsesWSBridgeProviderRejectionWaitTimeout
	responsesWSBridgeProviderRejectionWaitTimeout = -1
	t.Cleanup(func() {
		responsesWSBridgeProviderRejectionWaitTimeout = originalTimeout
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-bridge-open-send-result-fallback",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendRejectedBeforeStream,
		},
	})

	if attempt.RolledBack || actor.turns.pending.attempt != attempt || actor.state != responsesWSStatePendingSend {
		t.Fatalf("expected rejected_before_stream send result to await bridge event before rollback, attempt=%+v pending=%+v state=%v", attempt, actor.turns.pending.attempt, actor.state)
	}

	actor.handleTimeout(ResponsesWSEventTimeout{
		Reason:                    responsesWSBridgeProviderRejectionWaitTimeoutReason,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
	})

	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone || actor.state != responsesWSStateIdle {
		t.Fatalf("expected fallback to release settled pending attempt, pending=%+v active=%+v phase=%v state=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.turns.pending.phase, actor.state)
	}
	if !attempt.RolledBack || attempt.QuotaPreconsumed || attempt.QuotaFinalized {
		t.Fatalf("expected fallback to rollback unsettled attempt, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, responsesWSBridgeProviderRejectionFallbackErrorCode) {
		t.Fatalf("expected generic provider rejection fallback payload, got %q", got)
	}
}

func TestResponsesWSRejectedBeforeStreamFallbackIgnoredAfterBridgeEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-bridge-open-send-result-fallback-ignored",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendRejectedBeforeStream,
		},
	})
	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusTooManyRequests, "provider_rate_limit", "provider rejected stream open"),
		Recoverable:               true,
	})

	actor.handleTimeout(ResponsesWSEventTimeout{
		Reason:                    responsesWSBridgeProviderRejectionWaitTimeoutReason,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
	})

	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle || actor.closing.closed.Load() {
		t.Fatalf("expected bridge event to release attempt and stale fallback to be ignored, pending=%+v active=%+v state=%v closed=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.state, actor.closing.closed.Load())
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "provider_rate_limit") || strings.Contains(got, responsesWSBridgeProviderRejectionFallbackErrorCode) {
		t.Fatalf("expected precise provider rejection payload to survive fallback, got %q", got)
	}
}

func TestResponsesWSRecoverableRejectedBeforeStreamSendResultWaitsForBridgeEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-recoverable-send-result-first",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendRejectedBeforeStream,
		},
	})

	if attempt.RolledBack || actor.turns.pending.attempt != attempt || actor.closing.closed.Load() {
		t.Fatalf("expected rejected_before_stream send result to await bridge event before rollback, attempt=%+v pending=%+v closed=%v", attempt, actor.turns.pending.attempt, actor.closing.closed.Load())
	}

	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusTooManyRequests, "provider_rate_limit", "provider rejected stream open"),
		Recoverable:               true,
	})

	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle || actor.closing.closed.Load() {
		t.Fatalf("expected recoverable bridge event after send result to clear attempt and keep session open, pending=%+v active=%+v state=%v closed=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.state, actor.closing.closed.Load())
	}
	if !attempt.RolledBack || attempt.QuotaPreconsumed || attempt.QuotaFinalized {
		t.Fatalf("expected recoverable bridge event to rollback quota without finalize, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "provider_rate_limit") {
		t.Fatalf("expected provider rejection payload, got %q", got)
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected bridge open provider error not to submit terminal side effect, got %+v", actor.turns.history.lastFinal)
	}
}

func TestResponsesWSRecoverableBridgeOpenProviderErrorKeepsSessionOpenAfterSendResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-recoverable-bridge-open-error",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusTooManyRequests, "provider_rate_limit", "provider rejected stream open"),
		Recoverable:               true,
	})

	if actor.closing.closed.Load() || actor.turns.pending.attempt != attempt {
		t.Fatalf("expected recoverable bridge provider error to wait for send result, closed=%v pending=%+v", actor.closing.closed.Load(), actor.turns.pending.attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); got != "" {
		t.Fatalf("expected provider rejection payload to wait for settlement, got %q", got)
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendRejectedBeforeStream,
		},
	})

	if !attempt.RolledBack || actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle || actor.closing.closed.Load() {
		t.Fatalf("expected recoverable bridge rejection to rollback and keep session idle/open, attempt=%+v pending=%+v active=%+v state=%v closed=%v", attempt, actor.turns.pending.attempt, actor.turns.active.attempt, actor.state, actor.closing.closed.Load())
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "provider_rate_limit") {
		t.Fatalf("expected provider rejection payload after settlement, got %q", got)
	}
}

func TestResponsesWSBridgeOpenContinuationMissClearsDefaultPreviousResponseState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 60,
		Rules: []config.ChannelAffinityRule{
			{
				Name:            "responses-response-id",
				Enabled:         true,
				Kind:            "responses",
				IncludeModel:    true,
				IncludeRuleName: true,
				RecordOnSuccess: true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "request_field", Key: "previous_response_id", Alias: config.ChannelAffinityAliasResponseID},
				},
			},
		},
	}
	settings.Normalize()

	for _, eventFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("event_first_%t", eventFirst), func(t *testing.T) {
			manager := withChannelAffinitySettings(t, settings)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			request := &types.OpenAIResponsesRequest{Model: "gpt-5"}
			candidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{Context: ctx, Request: request})
			if err != nil {
				t.Fatalf("prepare affinity: %v", err)
			}
			template := newChannelAffinityTemplate(ctx, channelAffinityKindResponses, "gpt-5", settings.Rules[0], "request_field", config.ChannelAffinityAliasResponseID, settings.DefaultTTLSeconds)
			key := template.BuildKey("resp_stale_default")
			manager.SetRecord(key, runtimeaffinity.Record{
				ChannelID:         17,
				ResumeFingerprint: "model:gpt-5",
			}, time.Minute)

			conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
			actor := NewResponsesWSSessionActor(ctx)
			actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
			generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
			attempt := &ResponsesWSTurnAttempt{
				AttemptID:                   "attempt-bridge-continuation-miss",
				Candidate:                   candidate,
				SelectedChannelID:           17,
				AttemptedPreviousResponseID: "resp_stale_default",
				QuotaPreconsumed:            true,
				Usage:                       &types.Usage{},
			}
			actor.turns.pending.attempt = attempt
			actor.turns.pending.provider.journal = responsesWSProviderJournal{}
			actor.turns.pending.phase = responsesWSPendingTurnSend
			actor.state = responsesWSStatePendingSend
			actor.turns.history.lastFinal = &types.OpenAIResponsesResponses{ID: "resp_stale_default"}

			sendResult := ResponsesWSEventSendResult{
				AttemptID:                 attempt.AttemptID,
				UpstreamSessionGeneration: generation,
				SelectedChannelID:         17,
				Purpose:                   ResponsesWSSendPurposeResponseCreate,
				TransportResult: responsesws.ResponsesWSTransportSendResult{
					Status: responsesws.ResponsesWSTransportSendRejectedBeforeStream,
				},
			}
			providerEvent := ResponsesWSEventBridgeOpenProviderError{
				UpstreamSessionGeneration: generation,
				ChannelID:                 17,
				AttemptID:                 attempt.AttemptID,
				Payload:                   responsesWSPreviousResponseNotFoundPayload(),
				Recoverable:               true,
			}

			if eventFirst {
				actor.handleEvent(providerEvent)
				actor.handleSendResult(sendResult)
			} else {
				actor.handleSendResult(sendResult)
				actor.handleEvent(providerEvent)
			}

			if _, ok := manager.Get(key); ok {
				t.Fatal("expected stale default previous_response_id affinity binding to be cleared")
			}
			if actor.turns.history.lastFinal != nil {
				t.Fatalf("expected stale bridge default previous response to be cleared, got %+v", actor.turns.history.lastFinal)
			}
			if !attempt.RolledBack || actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle || actor.closing.closed.Load() {
				t.Fatalf("expected recoverable continuation miss to rollback and keep session idle/open, attempt=%+v pending=%+v active=%+v state=%v closed=%v", attempt, actor.turns.pending.attempt, actor.turns.active.attempt, actor.state, actor.closing.closed.Load())
			}
			if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "previous_response_not_found") {
				t.Fatalf("expected continuation miss payload, got %q", got)
			}
		})
	}
}

func TestResponsesWSUnknownProviderDetailOriginDoesNotFinalizeTerminal(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-unknown-origin")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_unknown","status":"completed"}}`)),
		DetailOrigin:              responsesws.RecvDetailOrigin("future_provider_origin"),
	})

	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected unknown detail origin not to drive terminal side effect, final=%+v", actor.turns.history.lastFinal)
	}
	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected unknown detail origin close to preserve quota floor, finalized=%v rolled=%v", attempt.QuotaFinalized, attempt.RolledBack)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected unknown provider detail origin to fail closed")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_unknown_provider_event_origin") {
		t.Fatalf("expected proxy-local protocol error for unknown origin, got %q", got)
	}
}

func TestResponsesWSSyntheticCancelDefersAccountingUntilBridgeEOF(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-synthetic-cancel-active")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.cancelled","response":{"id":"resp_cancel","status":"cancelled"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginSyntheticBridge,
		ReceivedAt:                time.Now(),
	})

	if actor.turns.active.attempt != attempt || actor.turns.active.channelID != 17 || actor.state != responsesWSStateInFlight {
		t.Fatalf("expected synthetic cancel to keep active turn pending, active=%+v channel=%d state=%v", actor.turns.active.attempt, actor.turns.active.channelID, actor.state)
	}
	if attempt.QuotaFinalized || attempt.RolledBack || !attempt.CompletedAt.IsZero() {
		t.Fatalf("expected synthetic cancel not to finalize active quota, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "resp_cancel") {
		t.Fatalf("expected synthetic cancel payload to be forwarded, got %q", got)
	}

	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		DetailOrigin:              responsesws.RecvDetailOriginBridgeStreamEOF,
	})
	if actor.turns.active.attempt != nil || actor.turns.active.channelID != 0 || actor.state != responsesWSStateIdle || actor.closing.closed.Load() {
		t.Fatalf("expected matching bridge EOF to clear active turn and keep session open, active=%+v channel=%d state=%v closed=%v", actor.turns.active.attempt, actor.turns.active.channelID, actor.state, actor.closing.closed.Load())
	}
	if !attempt.QuotaFinalized || attempt.RolledBack || attempt.CompletedAt.IsZero() {
		t.Fatalf("expected bridge EOF after cancel to finalize active quota once, attempt=%+v", attempt)
	}
}

func TestResponsesWSSyntheticCancelBuffersUntilPendingSendResult(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-synthetic-cancel-pending")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.cancelled","response":{"id":"resp_cancel_pending","status":"cancelled"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginSyntheticBridge,
		ReceivedAt:                time.Now(),
	})
	if actor.turns.pending.attempt != attempt || actor.turns.active.attempt != nil || len(actor.turns.pending.provider.journal.DownstreamEvents()) != 1 {
		t.Fatalf("expected pending synthetic cancel to buffer, pending=%+v active=%+v events=%d", actor.turns.pending.attempt, actor.turns.active.attempt, len(actor.turns.pending.provider.journal.DownstreamEvents()))
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 "attempt-synthetic-cancel-pending",
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAttempted,
		},
	})

	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != attempt || actor.state != responsesWSStateInFlight {
		t.Fatalf("expected buffered synthetic cancel to replay and keep active turn pending, pending=%+v active=%+v state=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.state)
	}
	if attempt.QuotaFinalized || attempt.RolledBack || !attempt.CompletedAt.IsZero() {
		t.Fatalf("expected replayed synthetic cancel not to finalize committed attempt, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "resp_cancel_pending") {
		t.Fatalf("expected buffered synthetic cancel payload to be forwarded after send result, got %q", got)
	}

	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		DetailOrigin:              responsesws.RecvDetailOriginBridgeStreamEOF,
	})
	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle || actor.closing.closed.Load() {
		t.Fatalf("expected bridge EOF after buffered synthetic cancel to clear turn, pending=%+v active=%+v state=%v closed=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.state, actor.closing.closed.Load())
	}
	if !attempt.QuotaFinalized || attempt.RolledBack || attempt.CompletedAt.IsZero() {
		t.Fatalf("expected bridge EOF after buffered cancel to finalize committed attempt, attempt=%+v", attempt)
	}
}

func TestResponsesWSSyntheticCancelThenProviderTerminalProviderWins(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-cancel-then-provider-terminal")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.cancelled","response":{"id":"resp_synthetic","status":"cancelled"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginSyntheticBridge,
		ReceivedAt:                time.Now(),
	})
	if attempt.QuotaFinalized || actor.turns.active.attempt != attempt {
		t.Fatalf("expected synthetic cancel to defer provider decision, attempt=%+v active=%+v", attempt, actor.turns.active.attempt)
	}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_provider_wins_after_cancel","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderStream,
		ReceivedAt:                time.Now(),
	})
	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_provider_wins_after_cancel" || actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected provider terminal to win and clear turn, final=%+v active=%+v state=%v", actor.turns.history.lastFinal, actor.turns.active.attempt, actor.state)
	}
	if !attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected provider terminal to finalize once, attempt=%+v", attempt)
	}

	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		DetailOrigin:              responsesws.RecvDetailOriginBridgeStreamEOF,
	})
	if actor.closing.closed.Load() || !attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected late bridge EOF after provider terminal to be ignored, closed=%v attempt=%+v", actor.closing.closed.Load(), attempt)
	}
}

func TestResponsesWSSyntheticCancelDoesNotOverrideProviderTerminal(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-terminal-before-cancel")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_provider_wins","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderStream,
		ReceivedAt:                time.Now(),
	})
	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_provider_wins" || !attempt.QuotaFinalized {
		t.Fatalf("expected provider terminal to finalize once, final=%+v finalized=%v", actor.turns.history.lastFinal, attempt.QuotaFinalized)
	}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.cancelled","response":{"id":"resp_synthetic","status":"cancelled"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginSyntheticBridge,
		ReceivedAt:                time.Now(),
	})
	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_provider_wins" {
		t.Fatalf("expected synthetic cancel not to override provider terminal, final=%+v", actor.turns.history.lastFinal)
	}
	if !attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected synthetic cancel not to mutate finalized quota, finalized=%v rolled=%v", attempt.QuotaFinalized, attempt.RolledBack)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "resp_synthetic") {
		t.Fatalf("expected synthetic cancel payload to remain proxy-local downstream diagnostic, got %q", got)
	}
}

func TestResponsesWSDuplicateProviderTerminalDoesNotDoubleFinalize(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-duplicate-terminal")

	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(&responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	event := ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_once","status":"completed","usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:                time.Now(),
	}
	actor.handleProviderDownstream(event)
	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_once" || !attempt.QuotaFinalized {
		t.Fatalf("expected first terminal to finalize once, final=%+v finalized=%v", actor.turns.history.lastFinal, attempt.QuotaFinalized)
	}
	firstUsage := *attempt.Usage

	actor.handleProviderDownstream(event)
	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_once" {
		t.Fatalf("expected duplicate terminal not to overwrite final, got %+v", actor.turns.history.lastFinal)
	}
	if !attempt.QuotaFinalized || attempt.Usage.TotalTokens != firstUsage.TotalTokens {
		t.Fatalf("expected duplicate terminal not to mutate quota/usage, finalized=%v usage=%+v first=%+v", attempt.QuotaFinalized, attempt.Usage, firstUsage)
	}
}

func TestResponsesWSAmbiguousSendWithoutProviderEvidencePreservesPreConsumedFloor(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	attempt.AttemptID = "attempt-ambiguous-no-evidence"
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal.AppendDownstreamReplay(ResponsesWSEventProviderDownstream{
		ChannelID: 17,
	})
	actor.turns.pending.provider.journal.bytes = 456
	actor.turns.pending.provider.journal.AppendFailureReplay(ResponsesWSEventProviderRecvFailed{
		ChannelID: 17,
		Err:       errors.New("stale pending failure"),
	})
	actor.turns.pending.provider.bridgeOpenLocalErrorAttemptID = "stale-marker"
	actor.state = responsesWSStatePendingSend

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         "attempt-ambiguous-no-evidence",
		SelectedChannelID: 17,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("write timeout"),
		},
	})

	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected ambiguous no-evidence send to settle floor, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil {
		t.Fatalf("expected ambiguous no-evidence send to release attempt state, pending=%+v active=%+v", actor.turns.pending.attempt, actor.turns.active.attempt)
	}
	if len(actor.turns.pending.provider.journal.DownstreamEvents()) != 0 || actor.turns.pending.provider.journal.bytes != 0 || len(actor.turns.pending.provider.journal.Failures()) != 0 || !actor.turns.pending.provider.journal.Project().IsZero() || actor.turns.pending.provider.bridgeOpenLocalErrorAttemptID != "" {
		t.Fatalf("expected ambiguous cleanup to clear pending provider state, events=%d bytes=%d failures=%d evidence=%+v marker=%q",
			len(actor.turns.pending.provider.journal.DownstreamEvents()), actor.turns.pending.provider.journal.bytes, len(actor.turns.pending.provider.journal.Failures()), actor.turns.pending.provider.journal.Project(), actor.turns.pending.provider.bridgeOpenLocalErrorAttemptID)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected ambiguous no-evidence send to close actor")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "ambiguous_close_no_provider_evidence") {
		t.Fatalf("expected proxy-local ambiguous error, got %q", got)
	}
}

func TestResponsesWSPrepareFailureNotAttemptedRollsBackWithoutTerminalFinalize(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-prepare-failed",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	prepareErr := errors.New("prepare failed before local write")
	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 "attempt-prepare-failed",
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    prepareErr,
		},
	})

	if !attempt.RolledBack || attempt.QuotaPreconsumed || attempt.QuotaFinalized {
		t.Fatalf("expected prepare failure to rollback without finalizing terminal quota, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected prepare failure to clear attempt state, pending=%+v active=%+v state=%v", actor.turns.pending.attempt, actor.turns.active.attempt, actor.state)
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected prepare failure not to create terminal side effect, got %+v", actor.turns.history.lastFinal)
	}
}

func TestResponsesWSNotSentUpstreamClosedAfterIgnoredNativeCloseSurfacesViaReplayExecutor(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-new",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleProviderClosed(ResponsesWSEventProviderClosed{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 "attempt-old",
		Code:                      int(wsconn.CloseNormalClosure),
		Reason:                    "idle provider close",
		Err:                       responsesws.ErrUpstreamClosed,
		DetailOrigin:              responsesws.RecvDetailOriginNativeProviderClose,
	})
	if actor.closing.closed.Load() || actor.turns.pending.attempt != attempt || actor.state != responsesWSStatePendingSend {
		t.Fatalf("expected stale-attempt native close to be ignored while current send is pending, closed=%v pending=%+v state=%v", actor.closing.closed.Load(), actor.turns.pending.attempt, actor.state)
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 "attempt-new",
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    responsesws.ErrUpstreamClosed,
		},
	})

	if !attempt.RolledBack || attempt.QuotaPreconsumed || attempt.QuotaFinalized {
		t.Fatalf("expected upstream-closed NotSent to rollback without terminal finalization, attempt=%+v", attempt)
	}
	if actor.closing.closed.Load() || actor.state != responsesWSStateIdle {
		t.Fatalf("expected non-replayable upstream-closed NotSent to surface and release actor, closed=%v state=%v", actor.closing.closed.Load(), actor.state)
	}
	if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone {
		t.Fatalf("expected non-replayable upstream-closed NotSent to clear pending turn, pending=%+v phase=%v", actor.turns.pending.attempt, actor.turns.pending.phase)
	}
	if session.abortReason != "" {
		t.Fatalf("expected no bypass session abort outside replay executor, got %q", session.abortReason)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "upstream_error") {
		t.Fatalf("expected downstream error surface, got %q", got)
	}
}

func TestResponsesWSAmbiguousSendWithImmediateNativeEOFWithoutEvidenceSettlesFloor(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	attempt.AttemptID = "attempt-eof-no-evidence"
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Err:                       io.EOF,
		DetailOrigin:              responsesws.RecvDetailOriginNativeProviderEOF,
	})
	if actor.closing.closed.Load() || actor.turns.pending.attempt != attempt {
		t.Fatalf("expected immediate native EOF without evidence to wait for send result, closed=%v pending=%+v", actor.closing.closed.Load(), actor.turns.pending.attempt)
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 "attempt-eof-no-evidence",
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("write failed"),
		},
	})
	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected ambiguous send plus EOF without evidence to settle floor, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "ambiguous_close_no_provider_evidence") {
		t.Fatalf("expected ambiguous no-evidence error, got %q", got)
	}
}

func TestResponsesWSAmbiguousSendWithImmediateBridgeStreamErrorWithoutEvidenceSettlesFloor(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	attempt.AttemptID = "attempt-bridge-stream-error-no-evidence"
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	streamErr := errors.New("bridge stream failed before send result")
	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Err:                       streamErr,
		DetailOrigin:              responsesws.RecvDetailOriginBridgeStreamError,
	})
	if actor.closing.closed.Load() || actor.turns.pending.attempt != attempt {
		t.Fatalf("expected immediate bridge stream error without evidence to wait for send result, closed=%v pending=%+v", actor.closing.closed.Load(), actor.turns.pending.attempt)
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 "attempt-bridge-stream-error-no-evidence",
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("write failed"),
		},
	})
	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected ambiguous send plus bridge stream error without evidence to settle floor, attempt=%+v", attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "ambiguous_close_no_provider_evidence") {
		t.Fatalf("expected ambiguous no-evidence error, got %q", got)
	}
}

func TestResponsesWSNotSentWithUsageOnlyProviderEvidenceSettlesWithoutFailClosed(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-usage")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(conn, actor)
	actor.SetBridge(bridge)
	session := &responsesWSTestSession{}
	upstreamSessionGeneration := actor.AttachUpstreamSession(session, 17)
	attempt.TransportResult = responsesws.ResponsesWSTransportSendResult{
		Status: responsesws.ResponsesWSTransportSendNotAttempted,
		Reason: responsesws.ResponsesWSTransportSendReasonNoActiveBridgeCancel,
	}
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	conflictCalls := 0
	originalRecorder := recordResponsesWSSettlementConflict
	recordResponsesWSSettlementConflict = func(kind string) {
		if kind != "not_sent_with_provider_evidence" {
			t.Fatalf("unexpected conflict kind %q", kind)
		}
		conflictCalls++
	}
	t.Cleanup(func() {
		recordResponsesWSSettlementConflict = originalRecorder
	})

	actor.handleProviderUsageObserved(ResponsesWSEventProviderUsageObserved{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: upstreamSessionGeneration,
		ChannelID:                 17,
		Usage:                     &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})
	if !actor.hasPendingProviderEvidence() {
		t.Fatalf("expected usage-only provider event to count as pending evidence")
	}
	if attempt.Usage.PromptTokens != 4 || attempt.Usage.CompletionTokens != 2 || attempt.Usage.TotalTokens != 6 {
		t.Fatalf("expected usage-only evidence to merge into pending attempt, got %+v", attempt.Usage)
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         "attempt-usage",
		SelectedChannelID: 17,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Reason: responsesws.ResponsesWSTransportSendReasonNoActiveBridgeCancel,
		},
	})

	if actor.closing.closed.Load() {
		t.Fatalf("expected not-sent proof conflict with usage evidence not to fail closed")
	}
	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected proof conflict to settle conservatively, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone {
		t.Fatalf("expected proof conflict to clear pending turn, pending=%+v phase=%v", actor.turns.pending.attempt, actor.turns.pending.phase)
	}
	if session.abortReason != "" {
		t.Fatalf("expected no protocol violation abort, got %q", session.abortReason)
	}
	if conflictCalls != 1 {
		t.Fatalf("expected one settlement conflict metric, got %d", conflictCalls)
	}
}

func TestResponsesWSProofConflictSettlementFailurePreservesPendingAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-proof-conflict-settlement-fails",
		SelectedChannelID: 17,
		Quota:             &relay_util.Quota{},
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal = responsesWSTestProviderFrameJournal()
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	var traces []ResponsesWSSettlementTrace
	originalHook := responsesWSSettlementTraceHook
	responsesWSSettlementTraceHook = func(trace ResponsesWSSettlementTrace) {
		traces = append(traces, trace)
	}
	t.Cleanup(func() {
		responsesWSSettlementTraceHook = originalHook
	})

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         attempt.AttemptID,
		SelectedChannelID: 17,
		Purpose:           ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    responsesws.ErrUpstreamClosed,
		},
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected proof-conflict settlement failure to close session")
	}
	if actor.turns.pending.attempt != attempt {
		t.Fatalf("expected failed proof-conflict settlement to preserve pending attempt, pending=%+v", actor.turns.pending.attempt)
	}
	if attempt.RolledBack || attempt.QuotaFinalized {
		t.Fatalf("expected failed proof-conflict settlement not to mutate accounting state, attempt=%+v", attempt)
	}
	if len(traces) != 0 {
		t.Fatalf("expected failed settlement not to emit successful trace, got %+v", traces)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "quota_settlement_failed") {
		t.Fatalf("expected quota settlement failure payload, got %q", got)
	}
}

func TestResponsesWSPendingLocalRollbackClearsBusyState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.state = responsesWSStateIdle
	actor.turns.pending.phase = responsesWSPendingTurnNone
	attempt := &ResponsesWSTurnAttempt{
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}

	if err := actor.BeginCandidate(attempt); err != nil {
		t.Fatalf("expected pending attempt to begin, got %v", err)
	}
	if !actor.isBusy() || actor.turns.pending.phase != responsesWSPendingTurnPrepare || actor.state != responsesWSStatePendingPrepare {
		t.Fatalf("expected BeginCandidate to make actor busy, state=%v phase=%v", actor.state, actor.turns.pending.phase)
	}
	if err := actor.settlePendingAttemptBeforeLocalWrite("rewrite_failed", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofRewriteFailed, "rewrite_failed")); err != nil {
		t.Fatalf("expected rollback cleanup to succeed, got %v", err)
	}

	if actor.isBusy() {
		t.Fatalf("expected local rollback to leave actor idle, state=%v phase=%v pending=%+v", actor.state, actor.turns.pending.phase, actor.turns.pending.attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone || actor.state != responsesWSStateIdle {
		t.Fatalf("expected pending turn transaction to be cleared, state=%v phase=%v pending=%+v", actor.state, actor.turns.pending.phase, actor.turns.pending.attempt)
	}
}

func TestResponsesWSSubsequentModelMismatchRejectsBeforeAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group", "default")
	ctx.Set("original_model", "gpt-5")
	ctx.Set("new_model", "gpt-5")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.state = responsesWSStateIdle
	actor.turns.pending.phase = responsesWSPendingTurnNone

	actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"gpt-4","input":[]}`), time.Now())

	if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone || actor.state != responsesWSStateIdle {
		t.Fatalf("expected model mismatch before attempt creation, state=%v phase=%v pending=%+v", actor.state, actor.turns.pending.phase, actor.turns.pending.attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, `"code":"responses_ws_model_mismatch"`) {
		t.Fatalf("expected model mismatch error, got %q", got)
	}
}

func TestResponsesWSClientFrameParseErrorUsesStaticMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))

	actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":`)))

	got, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, got, http.StatusBadRequest, "invalid_event", responsesWSMessageInvalidWebsocketEvent)
	if strings.Contains(got, "unexpected end of JSON input") {
		t.Fatalf("expected client payload to hide parser detail, got %q", got)
	}
}

func TestResponsesWSClientFrameRejectsDuplicateKeyCancelEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))

	actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.create","model":"gpt-5","type":"response.cancel"}`)))

	got, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, got, http.StatusBadRequest, "invalid_event", responsesWSMessageInvalidWebsocketEvent)
}

func TestResponsesWSSubsequentFrameParseErrorUsesInvalidResponseCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))

	actor.startSubsequentTurn([]byte(`{"type":"response.cancel","model":"gpt-5"}`), time.Now())

	got, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, got, http.StatusBadRequest, responsesWSErrorCodeInvalidResponseCreate, responsesWSMessageInvalidResponseCreate)
	if strings.Contains(got, "unsupported responses websocket event type") {
		t.Fatalf("expected client payload to hide parser detail, got %q", got)
	}
}

func TestResponsesWSSubsequentRewriteFailureUsesInternalErrorCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	installResponsesWSTestAPILimiter(t, 60)
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"gpt-5": {
			Model:  "gpt-5",
			Type:   model.TokensPriceType,
			Input:  1,
			Output: 1,
		},
	}}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("id", 7)
	ctx.Set("group", "default")
	ctx.Set("group_ratio", 1.0)
	ctx.Set("original_model", "gpt-5")
	ctx.Set("billing_original_model", true)
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.state = responsesWSStateIdle
	actor.turns.pending.phase = responsesWSPendingTurnNone

	actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"gpt-5","input":[]}`), time.Now())

	if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone || actor.state != responsesWSStateIdle {
		t.Fatalf("expected rewrite rollback to clear pending turn, state=%v phase=%v pending=%+v", actor.state, actor.turns.pending.phase, actor.turns.pending.attempt)
	}
	got, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, got, http.StatusInternalServerError, "responses_ws_payload_rewrite_failed", "internal payload rewrite failed")
}

func TestResponsesWSSubsequentRPMFailureDoesNotCreateAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"gpt-5": {
			Model: "gpt-5",
		},
	}}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group", "responses-ws-missing-limiter")
	ctx.Set("original_model", "gpt-5")
	ctx.Set("new_model", "gpt-5")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.state = responsesWSStateIdle
	actor.turns.pending.phase = responsesWSPendingTurnNone

	actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"gpt-5","input":[]}`), time.Now())

	if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone || actor.state != responsesWSStateIdle {
		t.Fatalf("expected RPM failure before attempt creation, state=%v phase=%v pending=%+v", actor.state, actor.turns.pending.phase, actor.turns.pending.attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, `"code":"api_requests_not_allowed"`) || strings.Contains(got, "API requests are not allowed") {
		t.Fatalf("expected local RPM error, got %q", got)
	}
}

func TestResponsesWSSubsequentStalePreflightRejectsBeforeRPMAndCloses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"gpt-5": {
			Model: "gpt-5",
		},
	}}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group", "responses-ws-missing-limiter")
	ctx.Set("original_model", "gpt-5")
	ctx.Set("new_model", "gpt-5")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	session := &responsesWSTestSession{preflightErr: responsesws.NewClientPayloadError(responsesws.ErrStaleContinuation, responsesWSPreviousResponseNotFoundPayload())}
	actor.AttachUpstreamSession(session, 17)
	actor.state = responsesWSStateIdle
	actor.turns.pending.phase = responsesWSPendingTurnNone

	actor.startSubsequentTurn([]byte(`{"type":"response.create","event_id":"evt_stale","model":"gpt-5","previous_response_id":"resp_old","input":[]}`), time.Now())

	if got := atomic.LoadInt32(&session.preflightCalls); got != 1 {
		t.Fatalf("expected stale preflight once, got %d", got)
	}
	if session.preflightEventID != "evt_stale" || session.preflightRequest == nil || session.preflightRequest.PreviousResponseID != "resp_old" {
		t.Fatalf("expected preflight request to carry event and previous response, event=%q request=%+v", session.preflightEventID, session.preflightRequest)
	}
	if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone {
		t.Fatalf("expected stale preflight before attempt creation, phase=%v pending=%+v", actor.turns.pending.phase, actor.turns.pending.attempt)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected stale preflight to close downstream session")
	}
	got, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, got, http.StatusConflict, "previous_response_not_found", "previous response was not found")
	if !strings.Contains(got, `"param":"previous_response_id"`) {
		t.Fatalf("expected previous_response_id param in stale payload, got %q", got)
	}
	if strings.Contains(got, "api_requests_not_allowed") {
		t.Fatalf("expected stale preflight before RPM limiter, got %q", got)
	}
}

func TestResponsesWSBusyCreateBudgetClosesAbusiveClient(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-busy-rate-limit")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.turns.active.attempt = attempt
	actor.state = responsesWSStateInFlight

	for i := 0; i < responsesWSBusyRejectLimit+1; i++ {
		actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`)))
	}

	if !actor.closing.closed.Load() {
		t.Fatal("expected excessive busy response.create frames to close the session")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_busy_rate_limited") {
		t.Fatalf("expected busy rate-limit error, got %q", got)
	}
}

func TestResponsesWSPendingProviderBufferHasByteCap(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-buffer")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(strings.Repeat("x", config.ResponsesWSPendingProviderEventsMaxBytes()+1))),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected oversized pending provider buffer to fail closed")
	}
	if session.abortReason != "responses_ws_pending_provider_buffer_full" {
		t.Fatalf("expected buffer cap abort reason, got %q", session.abortReason)
	}
}

func TestResponsesWSPendingProviderLifecycleHasEntryCap(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-lifecycle-cap")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	var traces []ResponsesWSSettlementTrace
	originalHook := responsesWSSettlementTraceHook
	responsesWSSettlementTraceHook = func(trace ResponsesWSSettlementTrace) {
		traces = append(traces, trace)
	}
	t.Cleanup(func() {
		responsesWSSettlementTraceHook = originalHook
	})

	for i := 0; i <= responsesWSPendingProviderEventsMax; i++ {
		actor.handleProviderUsageObserved(ResponsesWSEventProviderUsageObserved{
			AttemptID:                 attempt.AttemptID,
			UpstreamSessionGeneration: generation,
			ChannelID:                 17,
			Usage:                     &types.UsageEvent{TotalTokens: 1},
			DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
		})
	}

	if !actor.closing.closed.Load() {
		t.Fatal("expected excessive pending provider lifecycle observations to fail closed")
	}
	if session.abortReason != "responses_ws_pending_provider_buffer_full" {
		t.Fatalf("expected buffer cap abort reason, got %q", session.abortReason)
	}
	if len(traces) != 1 || !traces[0].Input.Diagnostics.ProviderUsageSeen {
		t.Fatalf("expected overflow lifecycle observation to reach settlement projection, traces=%+v", traces)
	}
	if got := traces[0].Input.Diagnostics.DetailOrigins; len(got) != responsesWSPendingProviderEventsMax+1 || got[len(got)-1] != string(responsesws.RecvDetailOriginProviderFrame) {
		t.Fatalf("expected overflow lifecycle origin in settlement trace, origins=%+v", got)
	}
}

func TestResponsesWSPendingProviderFailureHasEntryCap(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-failure-cap")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	var traces []ResponsesWSSettlementTrace
	originalHook := responsesWSSettlementTraceHook
	responsesWSSettlementTraceHook = func(trace ResponsesWSSettlementTrace) {
		traces = append(traces, trace)
	}
	t.Cleanup(func() {
		responsesWSSettlementTraceHook = originalHook
	})

	for i := 0; i <= responsesWSPendingProviderEventsMax; i++ {
		actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
			AttemptID:                 attempt.AttemptID,
			UpstreamSessionGeneration: generation,
			ChannelID:                 17,
			Err:                       responsesws.ErrInvalidProviderEventPayload,
			DetailOrigin:              responsesws.RecvDetailOriginProviderMalformed,
		})
	}

	if !actor.closing.closed.Load() {
		t.Fatal("expected excessive pending provider failures to fail closed")
	}
	if session.abortReason != "responses_ws_pending_provider_buffer_full" {
		t.Fatalf("expected buffer cap abort reason, got %q", session.abortReason)
	}
	if len(traces) != 1 || !traces[0].Input.Diagnostics.ProviderFrameSeen {
		t.Fatalf("expected overflow failure observation to reach settlement projection, traces=%+v", traces)
	}
	if got := traces[0].Input.Diagnostics.DetailOrigins; len(got) != responsesWSPendingProviderEventsMax+1 || got[len(got)-1] != string(responsesws.RecvDetailOriginProviderMalformed) {
		t.Fatalf("expected overflow failure origin in settlement trace, origins=%+v", got)
	}
}

func TestResponsesWSMaxLifetimeClosesActor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setResponsesWSTestViperInt(t, "responses_ws.max_lifetime_ms", 10)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	go actor.loop()
	stop := armResponsesWSMaxLifetime(actor)
	defer stop()

	select {
	case <-actor.Done():
	case <-time.After(time.Second):
		t.Fatal("expected max lifetime timer to close actor")
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected actor to be marked closed after max lifetime")
	}
}

func TestResponsesWSActiveTurnTimeoutFinalizesAndCloses(t *testing.T) {
	setResponsesWSTestViperInt(t, "responses_ws.active_turn_timeout_ms", 30000)
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-active-timeout")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight
	actor.armActiveTurnWatchdog()
	timerGen := actor.watchdog.activeTurnTimerGen

	actor.handleTimeout(ResponsesWSEventTimeout{
		Reason:                    responsesWSActiveTurnTimeoutReason,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 "attempt-active-timeout",
		TimeoutGeneration:         timerGen,
	})

	if actor.turns.active.attempt != nil || !actor.closing.closed.Load() || actor.state != responsesWSStateClosed {
		t.Fatalf("expected active timeout to clear and close, active=%+v closed=%v state=%v", actor.turns.active.attempt, actor.closing.closed.Load(), actor.state)
	}
	if !attempt.QuotaFinalized || attempt.RolledBack || attempt.CompletedAt.IsZero() {
		t.Fatalf("expected active timeout to finalize quota preserving floor, attempt=%+v", attempt)
	}
	if session.abortReason != responsesWSActiveTurnTimeoutReason {
		t.Fatalf("expected active timeout to abort upstream, got %q", session.abortReason)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, responsesWSActiveTurnTimeoutReason) {
		t.Fatalf("expected client timeout payload, got %q", got)
	}
}

func TestResponsesWSActiveTurnWatchdogTimerPostsTimeout(t *testing.T) {
	setResponsesWSTestViperInt(t, "responses_ws.active_turn_timeout_ms", 10)
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-active-timeout-timer")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	go actor.loop()
	actor.armActiveTurnWatchdog()

	select {
	case <-actor.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("expected active turn watchdog timer to close actor")
	}
	if actor.turns.active.attempt != nil || !actor.closing.closed.Load() || actor.state != responsesWSStateClosed {
		t.Fatalf("expected timer timeout to clear and close, active=%+v closed=%v state=%v", actor.turns.active.attempt, actor.closing.closed.Load(), actor.state)
	}
	if !attempt.QuotaFinalized || attempt.RolledBack || attempt.CompletedAt.IsZero() {
		t.Fatalf("expected timer timeout to finalize quota preserving floor, attempt=%+v", attempt)
	}
	if session.abortReason != responsesWSActiveTurnTimeoutReason {
		t.Fatalf("expected timer timeout to abort upstream, got %q", session.abortReason)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, responsesWSActiveTurnTimeoutReason) {
		t.Fatalf("expected client timeout payload, got %q", got)
	}
}

func TestResponsesWSActiveTurnWatchdogRefreshAndStaleTimeoutIgnored(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setResponsesWSTestViperInt(t, "responses_ws.active_turn_timeout_ms", 30000)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-watchdog-refresh",
		SelectedChannelID: 17,
		Quota:             &relay_util.Quota{},
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight
	actor.armActiveTurnWatchdog()
	firstGen := actor.watchdog.activeTurnTimerGen

	frame := responsesws.NewTextFrame([]byte(`{"type":"response.created","response":{"id":"resp_refresh","status":"in_progress"}}`))
	actor.updateActiveProviderEvidence(responsesws.UpstreamEvent{
		Frame:        &frame,
		AttemptID:    "attempt-watchdog-refresh",
		DetailOrigin: responsesws.RecvDetailOriginProviderStream,
	})
	refreshedGen := actor.watchdog.activeTurnTimerGen
	if refreshedGen == firstGen {
		t.Fatalf("expected provider evidence to refresh active turn watchdog, gen=%d", refreshedGen)
	}
	actor.updateActiveProviderEvidence(responsesws.UpstreamEvent{
		AttemptID:    "attempt-watchdog-refresh",
		DetailOrigin: responsesws.RecvDetailOriginSyntheticBridge,
	})
	if actor.watchdog.activeTurnTimerGen != refreshedGen {
		t.Fatalf("expected synthetic bridge event not to refresh watchdog, before=%d after=%d", refreshedGen, actor.watchdog.activeTurnTimerGen)
	}

	actor.handleTimeout(ResponsesWSEventTimeout{
		Reason:                    responsesWSActiveTurnTimeoutReason,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 "attempt-watchdog-refresh",
		TimeoutGeneration:         firstGen,
	})
	if actor.closing.closed.Load() || actor.turns.active.attempt != attempt {
		t.Fatalf("expected stale active timeout to be ignored, closed=%v active=%+v", actor.closing.closed.Load(), actor.turns.active.attempt)
	}
	actor.stopActiveTurnWatchdog()
}

func TestResponsesWSTerminalSideEffectsSurviveClientWriteFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-terminal")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1), writeErr: errors.New("client write failed")}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_write_failed","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_write_failed" {
		t.Fatalf("expected terminal side effects before client write failure, got %+v", actor.turns.history.lastFinal)
	}
	if actor.turns.active.attempt != nil || !actor.closing.closed.Load() {
		t.Fatalf("expected active turn cleared and session closed, active=%+v closed=%v", actor.turns.active.attempt, actor.closing.closed.Load())
	}
}

func TestResponsesWSFailedTerminalDoesNotCloseSessionForTurnScopedError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-failed")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"type":"invalid_request_error","code":"bad_input","message":"bad input"}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if actor.closing.closed.Load() {
		t.Fatal("expected turn-scoped provider failed terminal to keep websocket session open")
	}
	if actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected failed terminal to clear only the active turn, state=%v active=%+v", actor.state, actor.turns.active.attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "response.failed") || !strings.Contains(got, "bad_input") {
		t.Fatalf("expected failed terminal payload to be forwarded, got %q", got)
	}
}

func TestResponsesWSIncompleteTerminalWithoutErrorDoesNotCloseSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-incomplete")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_incomplete","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if actor.closing.closed.Load() {
		t.Fatal("expected incomplete terminal without explicit error detail to keep websocket session open")
	}
	if actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected incomplete terminal to clear only the active turn, state=%v active=%+v", actor.state, actor.turns.active.attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "response.failed") || !strings.Contains(got, "max_output_tokens") {
		t.Fatalf("expected incomplete terminal payload to be forwarded, got %q", got)
	}
}

func TestResponsesWSFailedTerminalProcessesProviderErrorWithoutClosingSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalProcess := processChannelRelayErrorFunc
	errCh := make(chan *types.OpenAIErrorWithStatusCode, 1)
	processChannelRelayErrorFunc = func(_ context.Context, _ int, _ string, apiErr *types.OpenAIErrorWithStatusCode, _ int) {
		errCh <- apiErr
	}
	defer func() {
		processChannelRelayErrorFunc = originalProcess
	}()

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-limit")
	ctx.Set("original_model", "gpt-test")
	ctx.Set("channel_type", config.ChannelTypeOpenAI)
	ctx.Set("channel_id", 17)
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Name: "provider-error", Type: config.ChannelTypeOpenAI})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.failed","response":{"id":"resp_limit","status":"failed","error":{"type":"usage_limit_reached","message":"monthly usage limit reached"}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if actor.closing.closed.Load() {
		t.Fatal("expected provider api error not to close websocket session")
	}
	select {
	case apiErr := <-errCh:
		if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Type != "usage_limit_reached" {
			t.Fatalf("expected parsed usage limit provider error, got %#v", apiErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for provider error control-plane handling")
	}
}

func TestResponsesWSProviderAPIErrorDedupesWithinTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	originalProcess := processChannelRelayErrorFunc
	errCh := make(chan *types.OpenAIErrorWithStatusCode, 2)
	processChannelRelayErrorFunc = func(_ context.Context, _ int, _ string, apiErr *types.OpenAIErrorWithStatusCode, _ int) {
		errCh <- apiErr
	}
	t.Cleanup(func() {
		processChannelRelayErrorFunc = originalProcess
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Name: "provider-error", Type: config.ChannelTypeOpenAI})

	actor := NewResponsesWSSessionActor(ctx)
	actor.turns.active.attempt = &ResponsesWSTurnAttempt{AttemptID: "attempt-limit", SelectedChannelID: 17}

	payload := []byte(`{"type":"error","error":{"type":"usage_limit_reached","message":"usage limit reached"}}`)
	actor.processProviderPayloadAPIError(payload, 17, "responses_ws_provider_frame")
	actor.processProviderPayloadAPIError(payload, 17, "responses_ws_provider_frame")

	select {
	case apiErr := <-errCh:
		if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Type != "usage_limit_reached" {
			t.Fatalf("expected parsed usage-limit provider error, got %#v", apiErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first provider error control-plane handling")
	}

	select {
	case apiErr := <-errCh:
		t.Fatalf("expected duplicate provider error to be suppressed, got %#v", apiErr)
	case <-time.After(100 * time.Millisecond):
	}

	actor.turns.active.attempt = &ResponsesWSTurnAttempt{AttemptID: "attempt-limit-next", SelectedChannelID: 17}
	actor.processProviderPayloadAPIError(payload, 17, "responses_ws_provider_frame")

	select {
	case apiErr := <-errCh:
		if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Type != "usage_limit_reached" {
			t.Fatalf("expected next turn to process provider error independently, got %#v", apiErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for next-turn provider error control-plane handling")
	}
}

func TestResponsesWSCloseReplaysBufferedTerminalForUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-buffered-close")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	terminalReceivedAt := time.Now().Add(-250 * time.Millisecond)
	attempt.TransportResult = responsesws.ResponsesWSTransportSendResult{
		Status: responsesws.ResponsesWSTransportSendAmbiguous,
		Err:    errors.New("ambiguous send"),
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal = responsesWSTestProviderFrameJournal()
	actor.turns.pending.provider.journal.AppendDownstreamReplay(ResponsesWSEventProviderDownstream{
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_close_buffered","status":"completed","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   terminalReceivedAt,
	})
	actor.turns.pending.provider.journal.AppendDownstreamReplay(ResponsesWSEventProviderDownstream{
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_close_buffered_duplicate","status":"completed","usage":{"input_tokens":30,"output_tokens":40,"total_tokens":70}}}`)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   terminalReceivedAt.Add(100 * time.Millisecond),
	})
	actor.state = responsesWSStatePendingSend

	actor.close("test_close_buffered")

	if attempt.Usage.PromptTokens != 3 || attempt.Usage.CompletionTokens != 4 || attempt.Usage.TotalTokens != 7 {
		t.Fatalf("expected buffered terminal usage to be merged before close settlement, got %+v", attempt.Usage)
	}
	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_close_buffered" {
		t.Fatalf("expected buffered terminal final response to be recorded, got %+v", actor.turns.history.lastFinal)
	}
	if !attempt.CompletedAt.Equal(terminalReceivedAt) {
		t.Fatalf("expected close replay to preserve provider terminal timestamp, got %s want %s", attempt.CompletedAt, terminalReceivedAt)
	}
}

func TestResponsesWSCloseBufferedTerminalExactZeroIgnoresObservedUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-buffered-close-zero")
	mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 10, OutputTokens: 90, TotalTokens: 100})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	terminalReceivedAt := time.Now().Add(-250 * time.Millisecond)
	attempt.TransportResult = responsesws.ResponsesWSTransportSendResult{
		Status: responsesws.ResponsesWSTransportSendAmbiguous,
		Err:    errors.New("ambiguous send"),
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal = responsesWSTestProviderFrameJournal()
	actor.turns.pending.provider.journal.AppendDownstreamReplay(ResponsesWSEventProviderDownstream{
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_close_buffered_zero","status":"completed","usage":{}}}`)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   terminalReceivedAt,
	})
	actor.state = responsesWSStatePendingSend

	actor.close("test_close_buffered_zero")

	if attempt.AppliedSettlement == nil || attempt.AppliedSettlement.AppliedFinalQuota != 0 {
		t.Fatalf("expected buffered terminal exact zero settlement, applied=%+v", attempt.AppliedSettlement)
	}
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 1000 || user.UsedQuota != 0 || token.RemainQuota != 1000 || token.UsedQuota != 0 {
		t.Fatalf("expected buffered terminal exact zero to refund observed/floor reserve, user=%+v token=%+v", user, token)
	}
	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_close_buffered_zero" {
		t.Fatalf("expected buffered terminal side effects after exact zero settlement, last=%+v", actor.turns.history.lastFinal)
	}
}

func TestResponsesWSCloseBufferedTerminalSettlementFailureSkipsSuccessSideEffects(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	terminalReceivedAt := time.Now().Add(-250 * time.Millisecond)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-buffered-close-settlement-fails",
		SelectedChannelID: 17,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("ambiguous send"),
		},
		Usage: &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal.AppendDownstreamReplay(ResponsesWSEventProviderDownstream{
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_close_settlement_fails","status":"completed","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   terminalReceivedAt,
	})
	actor.state = responsesWSStatePendingSend

	actor.close("test_close_buffered_settlement_fails")

	if attempt.Usage.PromptTokens != 3 || attempt.Usage.CompletionTokens != 4 || attempt.Usage.TotalTokens != 7 {
		t.Fatalf("expected terminal usage evidence to be projected before failed settlement, got %+v", attempt.Usage)
	}
	if attempt.TerminalEvidence == nil || !attempt.CompletedAt.Equal(terminalReceivedAt) {
		t.Fatalf("expected terminal evidence/timestamp before settlement, evidence=%+v completed=%s", attempt.TerminalEvidence, attempt.CompletedAt)
	}
	if attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected nil quota settlement failure not to mark attempt settled, attempt=%+v", attempt)
	}
	if actor.turns.history.lastFinal != nil || len(actor.turns.history.recentFinalizedResponseIDs) != 0 {
		t.Fatalf("expected terminal success side effects to wait for settlement success, last=%+v recent=%+v", actor.turns.history.lastFinal, actor.turns.history.recentFinalizedResponseIDs)
	}
	if session.abortReason != "quota_settlement_failed" {
		t.Fatalf("expected pending close settlement failure to abort with quota_settlement_failed, got %q", session.abortReason)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "quota_settlement_failed") {
		t.Fatalf("expected quota settlement failure payload, got %q", got)
	}
	if got, _ := conn.lastControl.Load().(string); !strings.Contains(got, "quota_settlement_failed") {
		t.Fatalf("expected downstream close control to use quota_settlement_failed, got %q", got)
	}
}

func TestResponsesWSCloseBufferedTerminalSettlementFailureSkipsProviderAPIErrorSideEffect(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalProcess := processChannelRelayErrorFunc
	errCh := make(chan *types.OpenAIErrorWithStatusCode, 1)
	processChannelRelayErrorFunc = func(_ context.Context, _ int, _ string, apiErr *types.OpenAIErrorWithStatusCode, _ int) {
		errCh <- apiErr
	}
	defer func() {
		processChannelRelayErrorFunc = originalProcess
	}()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Name: "provider-error", Type: config.ChannelTypeOpenAI})

	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(&responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-buffered-error-settlement-fails",
		SelectedChannelID: 17,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("ambiguous send"),
		},
		Usage: &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal.AppendDownstreamReplay(ResponsesWSEventProviderDownstream{
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.failed","response":{"id":"resp_error_settlement_fails","status":"failed","error":{"type":"usage_limit_reached","message":"monthly usage limit reached"}}}`)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   time.Now(),
	})
	actor.state = responsesWSStatePendingSend

	actor.close("test_close_buffered_error_settlement_fails")

	if attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected nil quota settlement failure not to mark attempt settled, attempt=%+v", attempt)
	}
	if len(actor.turns.history.recentFinalizedResponseIDs) != 0 {
		t.Fatalf("expected finalized response id side effect to wait for settlement success, recent=%+v", actor.turns.history.recentFinalizedResponseIDs)
	}
	select {
	case apiErr := <-errCh:
		t.Fatalf("expected provider api error side effect to wait for settlement success, got %#v", apiErr)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestResponsesWSCloseRejectedBeforeStreamWithBufferedTerminalFailsClosedWithoutTerminalSideEffects(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 60,
		Rules: []config.ChannelAffinityRule{{
			Name:            "responses-response-id",
			Enabled:         true,
			Kind:            "responses",
			IncludeModel:    true,
			IncludeRuleName: true,
			RecordOnSuccess: true,
			KeySources: []config.ChannelAffinityKeySource{
				{Source: "request_field", Key: "previous_response_id", Alias: config.ChannelAffinityAliasResponseID},
			},
		}},
	}
	settings.Normalize()
	manager := withChannelAffinitySettings(t, settings)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-close-rejected-terminal-conflict")
	candidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{
		Context: ctx,
		Request: &types.OpenAIResponsesRequest{Model: "gpt-5"},
	})
	if err != nil {
		t.Fatalf("prepare affinity: %v", err)
	}
	attempt.Candidate = candidate

	template := newChannelAffinityTemplate(ctx, channelAffinityKindResponses, "gpt-5", settings.Rules[0], "request_field", config.ChannelAffinityAliasResponseID, settings.DefaultTTLSeconds)
	affinityKey := template.BuildKey("resp_close_conflict")
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	var traces []ResponsesWSSettlementTrace
	originalHook := responsesWSSettlementTraceHook
	responsesWSSettlementTraceHook = func(trace ResponsesWSSettlementTrace) {
		traces = append(traces, trace)
	}
	t.Cleanup(func() {
		responsesWSSettlementTraceHook = originalHook
	})

	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusTooManyRequests, "provider_rate_limit", "provider rejected stream open"),
		Recoverable:               true,
	})
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_close_conflict","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:                time.Now(),
	})

	actor.close("client_closed_after_rejected_before_stream")

	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected proof conflict close to finalize conservatively, attempt=%+v", attempt)
	}
	if len(traces) != 1 || !responsesWSSettlementHasFlag(traces[0].Decision, ResponsesWSSettlementFlagContradictoryInput) {
		t.Fatalf("expected contradictory settlement trace, traces=%+v", traces)
	}
	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_close_conflict" || len(actor.turns.history.recentFinalizedResponseIDs) == 0 {
		t.Fatalf("expected terminal side effects to be applied, last=%+v recent=%+v", actor.turns.history.lastFinal, actor.turns.history.recentFinalizedResponseIDs)
	}
	if _, ok := manager.Get(affinityKey); !ok {
		t.Fatal("expected success affinity to be recorded for close proof conflict")
	}
	if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone {
		t.Fatalf("expected close proof conflict to clear pending turn, pending=%+v phase=%v", actor.turns.pending.attempt, actor.turns.pending.phase)
	}
	if session.abortReason != "client_closed_after_rejected_before_stream" {
		t.Fatalf("expected ordinary close abort reason, got %q", session.abortReason)
	}
}

func TestResponsesWSCloseRejectedBeforeStreamWithBufferedProviderAPIErrorSkipsSideEffect(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalProcess := processChannelRelayErrorFunc
	errCh := make(chan *types.OpenAIErrorWithStatusCode, 1)
	processChannelRelayErrorFunc = func(_ context.Context, _ int, _ string, apiErr *types.OpenAIErrorWithStatusCode, _ int) {
		errCh <- apiErr
	}
	t.Cleanup(func() {
		processChannelRelayErrorFunc = originalProcess
	})

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-close-rejected-api-error-conflict")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Name: "provider-error", Type: config.ChannelTypeOpenAI})
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusTooManyRequests, "provider_rate_limit", "provider rejected stream open"),
		Recoverable:               true,
	})
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.failed","response":{"id":"resp_close_conflict_error","status":"failed","error":{"type":"invalid_request_error","code":"bad_input","message":"bad input"}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:                time.Now(),
	})

	actor.close("client_closed_after_rejected_before_stream")

	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected proof conflict close to finalize conservatively, attempt=%+v", attempt)
	}
	if len(actor.turns.history.recentFinalizedResponseIDs) == 0 {
		t.Fatalf("expected failed terminal response id side effect to be applied, recent=%+v", actor.turns.history.recentFinalizedResponseIDs)
	}
	select {
	case apiErr := <-errCh:
		if apiErr == nil || apiErr.Code != "bad_input" {
			t.Fatalf("expected provider API error side effect, got %#v", apiErr)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected provider API error side effect")
	}
	if session.abortReason != "client_closed_after_rejected_before_stream" {
		t.Fatalf("expected ordinary close abort reason, got %q", session.abortReason)
	}
}

func TestResponsesWSCloseReplayProcessesBufferedProviderAPIError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalProcess := processChannelRelayErrorFunc
	errCh := make(chan *types.OpenAIErrorWithStatusCode, 1)
	processChannelRelayErrorFunc = func(_ context.Context, _ int, _ string, apiErr *types.OpenAIErrorWithStatusCode, _ int) {
		errCh <- apiErr
	}
	defer func() {
		processChannelRelayErrorFunc = originalProcess
	}()

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-buffered-error-close")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Name: "provider-error", Type: config.ChannelTypeOpenAI})

	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(&responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt.TransportResult = responsesws.ResponsesWSTransportSendResult{
		Status: responsesws.ResponsesWSTransportSendAmbiguous,
		Err:    errors.New("ambiguous send"),
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal = responsesWSTestProviderFrameJournal()
	actor.turns.pending.provider.journal.AppendDownstreamReplay(ResponsesWSEventProviderDownstream{
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.failed","response":{"id":"resp_limit","status":"failed","error":{"type":"usage_limit_reached","message":"monthly usage limit reached"}}}`)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	})
	actor.state = responsesWSStatePendingSend

	actor.close("test_close_buffered_error")

	select {
	case apiErr := <-errCh:
		if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Type != "usage_limit_reached" {
			t.Fatalf("expected parsed usage-limit provider error, got %#v", apiErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for replayed provider error control-plane handling")
	}
}

func TestResponsesWSCloseReplayProcessesBufferedProviderRecvFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-buffered-failure-close")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	failedAt := time.Now().Add(-300 * time.Millisecond)
	attempt.TransportResult = responsesws.ResponsesWSTransportSendResult{
		Status: responsesws.ResponsesWSTransportSendAmbiguous,
		Err:    errors.New("ambiguous send"),
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Err:                       errors.New("buffered read failure"),
		DetailOrigin:              responsesws.RecvDetailOriginProviderMalformed,
		DetailPhase:               responsesws.RecvDetailPhaseHandleProviderFrame,
		ReceivedAt:                failedAt,
	})
	if actor.closing.closed.Load() || len(actor.turns.pending.provider.journal.Failures()) != 1 {
		t.Fatalf("expected pending provider failure to be buffered, closed=%v failures=%d", actor.closing.closed.Load(), len(actor.turns.pending.provider.journal.Failures()))
	}

	actor.close("test_close_buffered_failure")

	if !attempt.CompletedAt.Equal(failedAt) {
		t.Fatalf("expected close replay to preserve provider failure timestamp, got %s want %s", attempt.CompletedAt, failedAt)
	}
	if attempt.RolledBack {
		t.Fatal("expected buffered provider failure evidence to preserve quota floor, not rollback")
	}
	payload, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, payload, http.StatusBadGateway, "responses_ws_provider_protocol_error", "malformed responses websocket frame")
}

func TestResponsesWSProviderRecvPumpEmitsClientPayloadErrorAfterProviderPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()

	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}
	quotaErr := types.NewErrorEvent("evt_quota", "system_error", "system_error", "user quota is not enough")
	bridge.ArmProviderRecvPump("session-payload-error", 17, session)
	session.responses <- responsesWSRecvResult{
		messageType:  responsesWSTextMessageType,
		payload:      []byte(`{"type":"response.output_text.delta","delta":"hi"}`),
		detailOrigin: responsesws.RecvDetailOriginProviderFrame,
		err:          responsesws.NewClientPayloadError(quotaErr, []byte(quotaErr.Error())),
	}

	first := readResponsesWSEvent(t, actor)
	downstream, ok := first.(ResponsesWSEventProviderDownstream)
	if !ok || downstream.Kind != ProviderDownstreamFrame || responsesws.PayloadOriginForDetailOrigin(downstream.DetailOrigin) != responsesws.PayloadOriginProvider {
		t.Fatalf("expected provider payload to be emitted first, got %#v", first)
	}
	if payload := responsesWSTestProviderEventPayload(downstream); len(payload) == 0 || !strings.Contains(string(payload), "response.output_text.delta") {
		t.Fatalf("expected provider payload to be preserved, got %q", payload)
	}

	second := readResponsesWSEvent(t, actor)
	localErr, ok := second.(ResponsesWSEventProxyLocalError)
	if !ok {
		t.Fatalf("expected client payload error after provider payload, got %#v", second)
	}
	if !strings.Contains(string(localErr.Payload), "user quota is not enough") {
		t.Fatalf("expected quota error payload, got %q", localErr.Payload)
	}
	if localErr.Recoverable {
		t.Fatalf("expected provider recv client payload error to be non-recoverable")
	}
}

func TestResponsesWSProviderRecvPumpPreservesNoPayloadBridgeStreamOpened(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()

	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}
	bridge.ArmProviderRecvPump("session-bridge-opened", 17, session)
	session.responses <- responsesWSRecvResult{
		detailOrigin: responsesws.RecvDetailOriginBridgeStreamOpened,
	}

	event := readResponsesWSEvent(t, actor)
	downstream, ok := event.(ResponsesWSEventProviderDownstream)
	if !ok {
		t.Fatalf("expected no-payload bridge_stream_opened to reach actor event path, got %#v", event)
	}
	if downstream.DetailOrigin != responsesws.RecvDetailOriginBridgeStreamOpened || downstream.Frame != nil || responsesws.PayloadOriginForDetailOrigin(downstream.DetailOrigin) != responsesws.PayloadOriginProvider {
		t.Fatalf("expected bridge_stream_opened evidence-only downstream event, got %+v", downstream)
	}
}

func TestResponsesWSProviderRecvPumpDoesNotEmitTimeoutAfterProviderErrorPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()

	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}
	payload := []byte(`{"type":"error","error":{"type":"usage_limit_reached","message":"usage limit reached"}}`)
	bridge.ArmProviderRecvPump("session-provider-error-payload", 17, session)
	session.responses <- responsesWSRecvResult{
		messageType:  responsesWSTextMessageType,
		payload:      payload,
		detailOrigin: responsesws.RecvDetailOriginProviderFrame,
		err:          errors.New("provider closed after error payload"),
	}

	first := readResponsesWSEvent(t, actor)
	downstream, ok := first.(ResponsesWSEventProviderDownstream)
	if !ok || downstream.Kind != ProviderDownstreamFrame || responsesws.PayloadOriginForDetailOrigin(downstream.DetailOrigin) != responsesws.PayloadOriginProvider {
		t.Fatalf("expected provider payload event, got %#v", first)
	}
	if got := string(responsesWSTestProviderEventPayload(downstream)); got != string(payload) {
		t.Fatalf("expected provider payload to be preserved, got %q", got)
	}

	select {
	case event := <-actor.events:
		t.Fatalf("expected no duplicate proxy-local timeout/error event, got %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestResponsesWSProviderRecvPumpEmitsProviderBusinessErrorForRecvEventErr(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()

	businessErr := errors.New("provider parse failed")
	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}
	bridge.ArmProviderRecvPump("session-business-error", 17, session)
	session.responses <- responsesWSRecvResult{err: businessErr}

	event := readResponsesWSEvent(t, actor)
	providerErr, ok := event.(ResponsesWSEventProviderBusinessError)
	if !ok {
		t.Fatalf("expected provider business error event, got %#v", event)
	}
	if providerErr.UpstreamSessionGeneration != "session-business-error" || providerErr.ChannelID != 17 || !errors.Is(providerErr.Err, businessErr) {
		t.Fatalf("expected provider business error metadata and error to be preserved, got %+v", providerErr)
	}
}

func TestResponsesWSProviderRecvPumpEmitsProviderRecvFailedForTopLevelRecvError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()

	recvErr := errors.New("provider recv failed")
	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}
	bridge.ArmProviderRecvPump("session-recv-failed", 17, session)
	session.responses <- responsesWSRecvResult{topErr: recvErr}

	event := readResponsesWSEvent(t, actor)
	recvFailed, ok := event.(ResponsesWSEventProviderRecvFailed)
	if !ok {
		t.Fatalf("expected provider recv failed event, got %#v", event)
	}
	if recvFailed.UpstreamSessionGeneration != "session-recv-failed" || recvFailed.ChannelID != 17 || !errors.Is(recvFailed.Err, recvErr) {
		t.Fatalf("expected provider recv failed metadata and error to be preserved, got %+v", recvFailed)
	}

	select {
	case event := <-actor.events:
		t.Fatalf("expected recv loop to exit after top-level error, got extra event %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestResponsesWSProviderRecvPumpEmitsProviderRecvFailedForLifecycleEventErr(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()

	recvErr := errors.New("bridge stream failed")
	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}
	bridge.ArmProviderRecvPump("session-lifecycle-failed", 17, session)
	session.responses <- responsesWSRecvResult{
		err:          recvErr,
		detailOrigin: responsesws.RecvDetailOriginBridgeStreamError,
	}

	event := readResponsesWSEvent(t, actor)
	recvFailed, ok := event.(ResponsesWSEventProviderRecvFailed)
	if !ok {
		t.Fatalf("expected provider recv failed event, got %#v", event)
	}
	if recvFailed.UpstreamSessionGeneration != "session-lifecycle-failed" || recvFailed.ChannelID != 17 || !errors.Is(recvFailed.Err, recvErr) ||
		responsesws.PayloadOriginForDetailOrigin(recvFailed.DetailOrigin) != responsesws.PayloadOriginProxyLocal || recvFailed.DetailOrigin != responsesws.RecvDetailOriginBridgeStreamError {
		t.Fatalf("expected lifecycle recv failure metadata to be preserved, got %+v", recvFailed)
	}

	select {
	case event := <-actor.events:
		t.Fatalf("expected recv loop to exit after lifecycle recv failure, got extra event %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestResponsesWSProviderRecvPumpEmitsProviderRecvFailedForProviderMalformed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()

	recvErr := errors.New("provider frame parse failed")
	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}
	bridge.ArmProviderRecvPump("session-provider-malformed", 17, session)
	session.responses <- responsesWSRecvResult{
		err:          recvErr,
		detailOrigin: responsesws.RecvDetailOriginProviderMalformed,
		detailPhase:  responsesws.RecvDetailPhaseHandleProviderFrame,
	}

	event := readResponsesWSEvent(t, actor)
	recvFailed, ok := event.(ResponsesWSEventProviderRecvFailed)
	if !ok {
		t.Fatalf("expected provider malformed to stay on ProviderRecvFailed path, got %#v", event)
	}
	if recvFailed.DetailOrigin != responsesws.RecvDetailOriginProviderMalformed || recvFailed.DetailPhase != responsesws.RecvDetailPhaseHandleProviderFrame ||
		recvFailed.UpstreamSessionGeneration != "session-provider-malformed" || recvFailed.ChannelID != 17 || !errors.Is(recvFailed.Err, recvErr) {
		t.Fatalf("expected provider malformed recv failure metadata to be preserved, got %+v", recvFailed)
	}
}

func TestResponsesWSProviderRecvPumpEmitsFrameBeforeProviderBusinessErrorPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()

	providerErr := types.NewErrorEvent("evt_provider", "system_error", "provider_error", "provider failed")
	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}
	bridge.ArmProviderRecvPump("session-frame-business-error", 17, session)
	session.responses <- responsesWSRecvResult{
		messageType:  responsesWSTextMessageType,
		payload:      []byte(`{"type":"response.output_text.delta","delta":"hi"}`),
		detailOrigin: responsesws.RecvDetailOriginProviderFrame,
		err:          responsesws.NewClientPayloadError(providerErr, []byte(providerErr.Error())),
	}

	first := readResponsesWSEvent(t, actor)
	downstream, ok := first.(ResponsesWSEventProviderDownstream)
	if !ok || downstream.Kind != ProviderDownstreamFrame || downstream.Err != nil {
		t.Fatalf("expected clean provider frame first, got %#v", first)
	}
	if got := string(responsesWSTestProviderEventPayload(downstream)); !strings.Contains(got, "response.output_text.delta") {
		t.Fatalf("expected provider frame payload to be preserved, got %q", got)
	}

	second := readResponsesWSEvent(t, actor)
	localErr, ok := second.(ResponsesWSEventProxyLocalError)
	if !ok {
		t.Fatalf("expected client payload error after provider frame, got %#v", second)
	}
	if !strings.Contains(string(localErr.Payload), "provider failed") {
		t.Fatalf("expected provider error payload, got %q", localErr.Payload)
	}
}

func TestResponsesWSProviderRecvPumpEmitsProviderClosedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()

	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}
	bridge.ArmProviderRecvPump("session-provider-close", 17, session)
	session.responses <- responsesWSRecvResult{
		providerClose: &responsesws.ProviderClose{
			Code:   4408,
			Reason: "quota exhausted",
			Err:    responsesws.ErrUpstreamClosed,
		},
		detailOrigin: responsesws.RecvDetailOriginNativeProviderClose,
	}

	event := readResponsesWSEvent(t, actor)
	closed, ok := event.(ResponsesWSEventProviderClosed)
	if !ok {
		t.Fatalf("expected provider closed event, got %#v", event)
	}
	if closed.UpstreamSessionGeneration != "session-provider-close" || closed.ChannelID != 17 {
		t.Fatalf("expected provider close routing metadata, got %+v", closed)
	}
	if closed.Code != 4408 || closed.Reason != "quota exhausted" || !errors.Is(closed.Err, responsesws.ErrUpstreamClosed) {
		t.Fatalf("expected provider close fields to be preserved, got %+v", closed)
	}

	select {
	case event := <-actor.events:
		t.Fatalf("expected provider close not to emit timeout or extra event, got %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestResponsesWSProviderRecvPumpMarksActivityForProviderEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()
	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 2)}

	old := time.Now().Add(-time.Hour)
	actor.setLastActivity(old)
	bridge.ArmProviderRecvPump("session-activity", 17, session)

	session.responses <- responsesWSRecvResult{
		usage:        &types.UsageEvent{TotalTokens: 1},
		detailOrigin: responsesws.RecvDetailOriginProviderFrame,
	}
	event := readResponsesWSEvent(t, actor)
	if usage, ok := event.(ResponsesWSEventProviderUsageObserved); !ok || usage.Usage == nil || usage.Usage.TotalTokens != 1 {
		t.Fatalf("expected provider usage event, got %#v", event)
	}
	if got := actor.lastActivity(); !got.After(old) {
		t.Fatalf("expected provider usage to refresh activity, got %s old %s", got, old)
	}

	old = time.Now().Add(-time.Hour)
	actor.setLastActivity(old)
	session.responses <- responsesWSRecvResult{
		messageType:  responsesWSTextMessageType,
		payload:      []byte(`{"type":"response.output_text.delta","delta":"hi"}`),
		detailOrigin: responsesws.RecvDetailOriginProviderFrame,
	}
	event = readResponsesWSEvent(t, actor)
	if downstream, ok := event.(ResponsesWSEventProviderDownstream); !ok || downstream.Kind != ProviderDownstreamFrame {
		t.Fatalf("expected provider frame event, got %#v", event)
	}
	if got := actor.lastActivity(); !got.After(old) {
		t.Fatalf("expected provider frame to refresh activity, got %s old %s", got, old)
	}
}

func TestResponsesWSProviderRecvPumpPostsInputAudioTranscriptionUsageOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()
	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}

	bridge.ArmProviderRecvPump("session-transcription-usage", 17, session)
	session.responses <- responsesWSRecvResult{
		usage: &types.UsageEvent{
			InputTokens:     7,
			TotalTokens:     7,
			Source:          types.UsageSourceInputAudioTranscription,
			BillingBasis:    types.UsageBillingBasisTokens,
			ItemID:          "item_1",
			ProviderEventID: "evt_transcription",
		},
		detailOrigin: responsesws.RecvDetailOriginProviderFrame,
	}

	event := readResponsesWSEvent(t, actor)
	usageEvent, ok := event.(ResponsesWSEventProviderUsageObserved)
	if !ok || usageEvent.Usage == nil {
		t.Fatalf("expected ProviderUsageObserved, got %#v", event)
	}
	if usageEvent.Usage.Source != types.UsageSourceInputAudioTranscription ||
		usageEvent.Usage.BillingBasis != types.UsageBillingBasisTokens ||
		usageEvent.Usage.ItemID != "item_1" ||
		usageEvent.Usage.TotalTokens != 7 {
		t.Fatalf("expected transcription usage-only event to preserve attribution, got %+v", usageEvent.Usage)
	}
	select {
	case extra := <-actor.events:
		t.Fatalf("expected no downstream frame event for usage-only transcription, got %#v", extra)
	default:
	}
}

func TestResponsesWSProviderRecvPumpKeepsFrameAndUsageTogether(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()
	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}

	bridge.ArmProviderRecvPump("session-frame-usage", 17, session)
	session.responses <- responsesWSRecvResult{
		messageType:  responsesWSTextMessageType,
		payload:      []byte(`{"type":"response.output_text.delta","delta":"hi"}`),
		usage:        &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
		detailOrigin: responsesws.RecvDetailOriginProviderFrame,
	}

	event := readResponsesWSEvent(t, actor)
	downstream, ok := event.(ResponsesWSEventProviderDownstream)
	if !ok {
		t.Fatalf("expected provider downstream event, got %#v", event)
	}
	if downstream.Kind != ProviderDownstreamFrame || downstream.Usage == nil || downstream.Usage.TotalTokens != 6 {
		t.Fatalf("expected frame and usage in one event, got %+v", downstream)
	}
	select {
	case event := <-actor.events:
		t.Fatalf("expected no separate usage event after frame+usage, got %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestResponsesWSSessionFrameMessageMappingIsTextBinaryOnly(t *testing.T) {
	textFrame := responsesWSFrameFromWireMessage(responsesWSTextMessageType, []byte("text"))
	if textFrame.Kind() != responsesws.FrameKindText || string(textFrame.Payload()) != "text" {
		t.Fatalf("expected websocket text to map to session text frame, got kind=%v payload=%q", textFrame.Kind(), textFrame.Payload())
	}
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	bridge := NewResponsesWSIOBridge(conn, nil)
	if err := bridge.WriteClientTypedFrame(textFrame, ResponsesWSWriteProvider); err != nil {
		t.Fatalf("write text typed frame: %v", err)
	}
	if mt := atomic.LoadInt32(&conn.lastMessageType); int(mt) != responsesWSTextMessageType {
		t.Fatalf("expected session text frame to map to websocket text, mt=%d", mt)
	}
	if got, _ := conn.lastWrite.Load().(string); got != "text" {
		t.Fatalf("expected text payload to write unchanged, got %q", got)
	}

	binaryFrame := responsesWSFrameFromWireMessage(responsesWSBinaryMessageType, []byte{1, 2, 3})
	if binaryFrame.Kind() != responsesws.FrameKindBinary || string(binaryFrame.Payload()) != string([]byte{1, 2, 3}) {
		t.Fatalf("expected websocket binary to map to session binary frame, got kind=%v payload=%v", binaryFrame.Kind(), binaryFrame.Payload())
	}
	if err := bridge.WriteClientTypedFrame(binaryFrame, ResponsesWSWriteProvider); err != nil {
		t.Fatalf("write binary typed frame: %v", err)
	}
	if mt := atomic.LoadInt32(&conn.lastMessageType); int(mt) != responsesWSBinaryMessageType {
		t.Fatalf("expected session binary frame to map to websocket binary, mt=%d", mt)
	}
	if got, _ := conn.lastWrite.Load().(string); got != string([]byte{1, 2, 3}) {
		t.Fatalf("expected binary payload to write unchanged, got %q", got)
	}
}

func TestResponsesWSProviderBinaryFrameForwardsWithoutTerminalClassification(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-binary-active",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	payload := []byte(`{"type":"response.completed","response":{"id":"resp_binary","status":"completed"}}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderBinaryFrame(payload),
		Usage:                     &types.UsageEvent{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if actor.closing.closed.Load() {
		t.Fatal("expected provider binary frame not to close as malformed JSON")
	}
	if actor.turns.active.attempt != attempt || attempt.QuotaFinalized {
		t.Fatalf("expected binary frame to remain non-terminal, active=%+v finalized=%v", actor.turns.active.attempt, attempt.QuotaFinalized)
	}
	if got := atomic.LoadInt32(&conn.writeCount); got != 1 {
		t.Fatalf("expected one binary downstream write, got %d", got)
	}
	if got := atomic.LoadInt32(&conn.lastMessageType); int(got) != responsesWSBinaryMessageType {
		t.Fatalf("expected downstream binary message type, got %d", got)
	}
	if got, _ := conn.lastWrite.Load().(string); got != string(payload) {
		t.Fatalf("expected binary payload to forward unchanged, got %q", got)
	}
	if attempt.Usage.PromptTokens != 1 || attempt.Usage.CompletionTokens != 2 || attempt.Usage.TotalTokens != 3 {
		t.Fatalf("expected binary-attached usage to merge once, got %+v", attempt.Usage)
	}
}

func TestResponsesWSProviderRecvPumpKeepsTerminalStatusFramesWithUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name    string
		payload []byte
	}{
		{
			name:    "failed",
			payload: []byte(`{"type":"response.done","response":{"id":"resp_failed","status":"failed","error":{"type":"invalid_request_error","code":"bad_input","message":"bad input"},"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`),
		},
		{
			name:    "incomplete",
			payload: []byte(`{"type":"response.done","response":{"id":"resp_incomplete","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`),
		},
		{
			name:    "cancelled",
			payload: []byte(`{"type":"response.done","response":{"id":"resp_cancelled","status":"cancelled","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

			actor := NewResponsesWSSessionActor(ctx)
			bridge := NewResponsesWSIOBridge(nil, actor)
			defer bridge.Close()
			session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}

			bridge.ArmProviderRecvPump("session-terminal-"+tc.name, 17, session)
			session.responses <- responsesWSRecvResult{
				messageType:  responsesWSTextMessageType,
				payload:      tc.payload,
				usage:        &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
				detailOrigin: responsesws.RecvDetailOriginProviderFrame,
			}

			event := readResponsesWSEvent(t, actor)
			downstream, ok := event.(ResponsesWSEventProviderDownstream)
			if !ok {
				t.Fatalf("expected terminal status to arrive as provider downstream frame, got %#v", event)
			}
			if downstream.Kind != ProviderDownstreamFrame || downstream.Usage == nil || downstream.Usage.TotalTokens != 6 {
				t.Fatalf("expected terminal status frame with usage, got %+v", downstream)
			}
			if got := string(responsesWSTestProviderEventPayload(downstream)); !strings.Contains(got, `"status":"`+tc.name+`"`) {
				t.Fatalf("expected terminal status payload to be preserved, got %q", got)
			}

			select {
			case event := <-actor.events:
				t.Fatalf("expected no provider business error after terminal status frame, got %#v", event)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestResponsesWSProviderRecvPumpContinuesAfterRecoverableBridgeOpenError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()
	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 2)}

	bridge.ArmProviderRecvPump("session-recoverable-bridge-open", 17, session)
	session.responses <- responsesWSRecvResult{
		attemptID:    "attempt-recoverable",
		detailOrigin: responsesws.RecvDetailOriginBridgeOpenProviderError,
		err:          responsesws.NewBridgeOpenProviderError(http.StatusTooManyRequests, "rate_limit", "provider_error", "provider busy"),
	}

	first := readResponsesWSEvent(t, actor)
	providerErr, ok := first.(ResponsesWSEventBridgeOpenProviderError)
	if !ok || !providerErr.Recoverable {
		t.Fatalf("expected recoverable bridge open provider error, got %#v", first)
	}

	payload := []byte(`{"type":"response.created","response":{"id":"resp_after_recoverable","status":"in_progress"}}`)
	session.responses <- responsesWSRecvResult{
		messageType:  responsesWSTextMessageType,
		payload:      payload,
		attemptID:    "attempt-next",
		detailOrigin: responsesws.RecvDetailOriginProviderStream,
	}

	second := readResponsesWSEvent(t, actor)
	downstream, ok := second.(ResponsesWSEventProviderDownstream)
	if !ok {
		t.Fatalf("expected recv pump to keep forwarding after recoverable error, got %#v", second)
	}
	if downstream.AttemptID != "attempt-next" || string(responsesWSTestProviderEventPayload(downstream)) != string(payload) {
		t.Fatalf("expected next provider frame after recoverable bridge error, got %+v", downstream)
	}
}

func TestResponsesWSProviderDownstreamFrameWithUsageMergesBeforeWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-frame-usage",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	payload := []byte(`{"type":"response.output_text.delta","delta":"hi"}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame(payload),
		Usage:                     &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if got := atomic.LoadInt32(&conn.writeCount); got != 1 {
		t.Fatalf("expected provider frame to be written once, got %d writes", got)
	}
	if got, _ := conn.lastWrite.Load().(string); got != string(payload) {
		t.Fatalf("expected provider frame payload to be forwarded, got %q", got)
	}
	if attempt.Usage.PromptTokens != 4 || attempt.Usage.CompletionTokens != 2 || attempt.Usage.TotalTokens != 6 {
		t.Fatalf("expected attached usage to merge into active attempt, got %+v", attempt.Usage)
	}
	if attempt.CompletedAt.IsZero() == false || attempt.QuotaFinalized || actor.turns.active.attempt != attempt {
		t.Fatalf("expected non-terminal frame+usage not to complete/finalize/clear turn, completed=%s finalized=%v active=%v", attempt.CompletedAt, attempt.QuotaFinalized, actor.turns.active.attempt == attempt)
	}
}

func TestResponsesWSTerminalFrameWithResponseUsageAndAttachedUsageBillsOnce(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-terminal-usage-once")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	payload := []byte(`{"type":"response.completed","response":{"id":"resp_usage_once","status":"completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame(payload),
		Usage:                     &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if attempt.Usage.PromptTokens != 4 || attempt.Usage.CompletionTokens != 2 || attempt.Usage.TotalTokens != 6 {
		t.Fatalf("expected terminal response usage and attached usage to bill once, got %+v", attempt.Usage)
	}
	if actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected terminal to clear active turn, active=%+v state=%v", actor.turns.active.attempt, actor.state)
	}
}

func TestResponsesWSPendingProviderFrameWithUsageReplaysWithoutDoubleBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-pending-frame-usage",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	payload := []byte(`{"type":"response.output_text.delta","delta":"hi"}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame(payload),
		Usage:                     &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if attempt.Usage.PromptTokens != 4 || attempt.Usage.CompletionTokens != 2 || attempt.Usage.TotalTokens != 6 {
		t.Fatalf("expected pending usage to merge once before replay, got %+v", attempt.Usage)
	}
	if len(actor.turns.pending.provider.journal.DownstreamEvents()) != 1 || actor.turns.pending.provider.journal.DownstreamEvents()[0].Usage != nil {
		t.Fatalf("expected buffered replay event to clear usage after merge, got %+v", actor.turns.pending.provider.journal.DownstreamEvents())
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         "attempt-pending-frame-usage",
		SelectedChannelID: 17,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAttempted,
		},
	})

	if got := atomic.LoadInt32(&conn.writeCount); got != 1 {
		t.Fatalf("expected buffered provider frame to be written once, got %d writes", got)
	}
	if got, _ := conn.lastWrite.Load().(string); got != string(payload) {
		t.Fatalf("expected provider payload to be replayed, got %q", got)
	}
	if attempt.Usage.PromptTokens != 4 || attempt.Usage.CompletionTokens != 2 || attempt.Usage.TotalTokens != 6 {
		t.Fatalf("expected usage not to double count after replay, got %+v", attempt.Usage)
	}
}

func TestResponsesWSPendingProviderBinaryFrameReplaysWithoutTerminalSideEffects(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-pending-binary",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	payload := []byte(`{"type":"response.completed","response":{"id":"resp_binary_pending","status":"completed"}}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderBinaryFrame(payload),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if !actor.hasPendingProviderEvidence() || len(actor.turns.pending.provider.journal.DownstreamEvents()) != 1 {
		t.Fatalf("expected pending binary frame to be buffered as provider evidence, evidence=%v events=%d", actor.hasPendingProviderEvidence(), len(actor.turns.pending.provider.journal.DownstreamEvents()))
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         "attempt-pending-binary",
		SelectedChannelID: 17,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAttempted,
		},
	})

	if actor.closing.closed.Load() {
		t.Fatal("expected pending binary replay not to close as malformed JSON")
	}
	if actor.turns.active.attempt != attempt || attempt.QuotaFinalized || actor.turns.history.lastFinal != nil {
		t.Fatalf("expected binary replay to stay non-terminal, active=%+v finalized=%v last_final=%+v", actor.turns.active.attempt, attempt.QuotaFinalized, actor.turns.history.lastFinal)
	}
	if got := atomic.LoadInt32(&conn.lastMessageType); int(got) != responsesWSBinaryMessageType {
		t.Fatalf("expected pending binary replay as binary message type, got %d", got)
	}
	if got, _ := conn.lastWrite.Load().(string); got != string(payload) {
		t.Fatalf("expected pending binary payload to replay unchanged, got %q", got)
	}
}

func TestResponsesWSPendingTerminalWithUsageReplaysSideEffectsWithoutDoubleBilling(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000000, "attempt-pending-terminal-usage")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	payload := []byte(`{"type":"response.completed","response":{"id":"resp_pending_usage","status":"completed"}}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame(payload),
		Usage: &types.UsageEvent{
			InputTokens:  3,
			OutputTokens: 4,
			TotalTokens:  7,
			ExtraBilling: map[string]types.ExtraBilling{
				types.APIToolTypeWebSearchPreview: {CallCount: 1},
			},
		},
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	})

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         "attempt-pending-terminal-usage",
		SelectedChannelID: 17,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAttempted,
		},
	})

	if attempt.Usage.PromptTokens != 3 || attempt.Usage.CompletionTokens != 4 || attempt.Usage.TotalTokens != 7 {
		t.Fatalf("expected terminal attached usage to remain single-counted after replay, got %+v", attempt.Usage)
	}
	if got := attempt.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview].CallCount; got != 1 {
		t.Fatalf("expected extra billing not to double count after replay, got %d in %+v", got, attempt.Usage.ExtraBilling)
	}
	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_pending_usage" {
		t.Fatalf("expected terminal side effects to run during replay, final=%+v closed=%v state=%v active=%+v finalized=%v rolled_back=%v settlement=%+v recent=%+v",
			actor.turns.history.lastFinal, actor.closing.closed.Load(), actor.state, actor.turns.active.attempt, attempt.QuotaFinalized, attempt.RolledBack, attempt.AppliedSettlement, actor.turns.history.recentFinalizedResponseIDs)
	}
	if got, _ := conn.lastWrite.Load().(string); got != string(payload) {
		t.Fatalf("expected terminal provider payload to be replayed, got %q", got)
	}
}

func TestResponsesWSProviderUsageObservedOnlyUpdatesSettlementState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-usage-observed",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderUsageObserved(ResponsesWSEventProviderUsageObserved{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Usage:                     &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
		ReceivedAt:                time.Now(),
	})

	if got := atomic.LoadInt32(&conn.writeCount); got != 0 {
		t.Fatalf("expected usage-only event not to write downstream, got %d writes", got)
	}
	if got := atomic.LoadInt32(&conn.controlCount); got != 0 {
		t.Fatalf("expected usage-only event not to write close control, got %d controls", got)
	}
	if !attempt.CompletedAt.IsZero() {
		t.Fatalf("expected usage-only event not to mark turn completed, got %s", attempt.CompletedAt)
	}
	if attempt.QuotaFinalized {
		t.Fatal("expected usage-only event not to finalize quota")
	}
	if actor.turns.active.attempt != attempt || actor.turns.active.channelID != 17 || actor.state != responsesWSStateInFlight {
		t.Fatalf("expected usage-only event not to clear active turn, active=%v channel=%d state=%v", actor.turns.active.attempt == attempt, actor.turns.active.channelID, actor.state)
	}
	if attempt.Usage.PromptTokens != 4 || attempt.Usage.CompletionTokens != 2 || attempt.Usage.TotalTokens != 6 {
		t.Fatalf("expected usage-only event to merge settlement usage, got %+v", attempt.Usage)
	}
}

func TestResponsesWSProviderUsageObservedRejectsProxyLocalUsage(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-proxy-local-usage")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderUsageObserved(ResponsesWSEventProviderUsageObserved{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Usage:                     &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
		DetailOrigin:              responsesws.RecvDetailOriginBridgeStreamEOF,
		ReceivedAt:                time.Now(),
	})

	if attempt.Usage.PromptTokens != 0 || attempt.Usage.CompletionTokens != 0 || attempt.Usage.TotalTokens != 0 {
		t.Fatalf("expected invalid-origin usage not to enter settlement state, got %+v", attempt.Usage)
	}
	if actor.turns.active.evidence.HasActivity() {
		t.Fatalf("expected invalid-origin usage not to enter provider observation log, got %+v", actor.turns.active.evidence)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected proxy-local usage evidence violation to fail closed")
	}
	written, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, written, http.StatusBadGateway, "responses_ws_protocol_violation", "responses_ws_provider_usage_without_provider_evidence")
}

func TestResponsesWSProviderUsageObservedDropsUnpricedInputAudioTranscription(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"gpt-5": {
			Model:  "gpt-5",
			Type:   model.TokensPriceType,
			Input:  1,
			Output: 1,
		},
	}}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	var recordedSource string
	var recordedModel string
	originalRecorder := recordUsageObservedUnbilled
	recordUsageObservedUnbilled = func(source, model string) {
		recordedSource = source
		recordedModel = model
	}
	t.Cleanup(func() {
		recordUsageObservedUnbilled = originalRecorder
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group_ratio", 1.0)
	ctx.Set("channel_id", 17)
	ctx.Set("channel_type", config.ChannelTypeOpenAI)
	ctx.Set("original_model", "gpt-5")
	ctx.Set("new_model", "gpt-5")
	ctx.Set("billing_original_model", true)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-transcription-unpriced",
		SelectedChannelID: 17,
		Quota:             relay_util.NewQuota(ctx, "gpt-5", 0),
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderUsageObserved(ResponsesWSEventProviderUsageObserved{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Usage: &types.UsageEvent{
			InputTokens:  4,
			TotalTokens:  4,
			Source:       types.UsageSourceInputAudioTranscription,
			BillingBasis: types.UsageBillingBasisTokens,
		},
		ReceivedAt: time.Now(),
	})

	if recordedSource != string(types.UsageSourceInputAudioTranscription) || recordedModel != "gpt-5" {
		t.Fatalf("expected unbilled transcription metric labels, source=%q model=%q", recordedSource, recordedModel)
	}
	if got := atomic.LoadInt32(&conn.writeCount); got != 0 {
		t.Fatalf("expected unpriced transcription usage not to write downstream, got %d writes", got)
	}
	if attempt.Usage.PromptTokens != 0 || attempt.Usage.CompletionTokens != 0 || attempt.Usage.TotalTokens != 0 {
		t.Fatalf("expected unpriced transcription usage not to enter settlement, got %+v", attempt.Usage)
	}
	if !attempt.CompletedAt.IsZero() || attempt.QuotaFinalized || actor.turns.active.attempt != attempt || actor.state != responsesWSStateInFlight {
		t.Fatalf("expected unpriced transcription usage not to mutate lifecycle, completed=%s finalized=%v active=%v state=%v", attempt.CompletedAt, attempt.QuotaFinalized, actor.turns.active.attempt == attempt, actor.state)
	}
}

func TestResponsesWSPendingUnpricedInputAudioUsagePreservesProviderEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"gpt-5": {
			Model: "gpt-5",
			Type:  model.TokensPriceType,
			Input: 1,
		},
	}}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	originalRecorder := recordUsageObservedUnbilled
	recordUsageObservedUnbilled = func(source, model string) {}
	t.Cleanup(func() {
		recordUsageObservedUnbilled = originalRecorder
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group_ratio", 1.0)
	ctx.Set("channel_id", 17)
	ctx.Set("channel_type", config.ChannelTypeOpenAI)
	ctx.Set("original_model", "gpt-5")
	ctx.Set("new_model", "gpt-5")
	ctx.Set("billing_original_model", true)

	actor := NewResponsesWSSessionActor(ctx)
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-pending-unpriced-transcription",
		SelectedChannelID: 17,
		Quota:             relay_util.NewQuota(ctx, "gpt-5", 0),
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal = responsesWSProviderJournal{}
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend
	t.Cleanup(func() {
		actor.finish()
	})

	actor.handleProviderUsageObserved(ResponsesWSEventProviderUsageObserved{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Usage: &types.UsageEvent{
			InputTokens:  4,
			TotalTokens:  4,
			Source:       types.UsageSourceInputAudioTranscription,
			BillingBasis: types.UsageBillingBasisTokens,
		},
		ReceivedAt: time.Now(),
	})

	if !actor.hasPendingProviderEvidence() {
		t.Fatal("expected unpriced usage-only provider event to preserve pending provider evidence")
	}
	if attempt.Usage.TotalTokens != 0 {
		t.Fatalf("expected unpriced usage not to become billable usage, usage=%+v", attempt.Usage)
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("write result unknown"),
		},
	})

	if attempt.RolledBack || actor.turns.active.attempt != attempt || actor.state != responsesWSStateInFlight {
		t.Fatalf("expected ambiguous send with provider evidence to commit attempt, rolledBack=%v active=%v state=%v", attempt.RolledBack, actor.turns.active.attempt == attempt, actor.state)
	}
	if !actor.turns.active.evidence.HasActivity() {
		t.Fatalf("expected committed active attempt to retain provider evidence, got %+v", actor.turns.active.evidence)
	}
	if attempt.Usage.TotalTokens != 0 {
		t.Fatalf("expected unpriced usage to remain excluded from settlement, got %+v", attempt.Usage)
	}
}

func TestResponsesWSProviderUsageObservedMergesPricedInputAudioTranscription(t *testing.T) {
	gin.SetMode(gin.TestMode)

	extraRatios := datatypes.NewJSONType(map[string]float64{
		config.UsageExtraInputAudioTranscription: 1,
	})
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"gpt-5": {
			Model:       "gpt-5",
			Type:        model.TokensPriceType,
			Input:       1,
			Output:      1,
			ExtraRatios: &extraRatios,
		},
	}}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	metricCalls := 0
	originalRecorder := recordUsageObservedUnbilled
	recordUsageObservedUnbilled = func(source, model string) {
		metricCalls++
	}
	t.Cleanup(func() {
		recordUsageObservedUnbilled = originalRecorder
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group_ratio", 1.0)
	ctx.Set("channel_id", 17)
	ctx.Set("channel_type", config.ChannelTypeOpenAI)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-transcription-priced",
		SelectedChannelID: 17,
		Quota:             relay_util.NewQuota(ctx, "gpt-5", 0),
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderUsageObserved(ResponsesWSEventProviderUsageObserved{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Usage: &types.UsageEvent{
			InputTokens:  4,
			TotalTokens:  4,
			Source:       types.UsageSourceInputAudioTranscription,
			BillingBasis: types.UsageBillingBasisTokens,
		},
		ReceivedAt: time.Now(),
	})

	if metricCalls != 0 {
		t.Fatalf("expected priced transcription usage not to record unbilled metric, got %d calls", metricCalls)
	}
	if attempt.Usage.PromptTokens != 4 || attempt.Usage.TotalTokens != 4 {
		t.Fatalf("expected priced transcription usage to merge into settlement, got %+v", attempt.Usage)
	}
}

func TestResponsesWSProviderUsageObservedWithoutTurnHasNoQuotaSemantics(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.state = responsesWSStateIdle

	actor.handleProviderUsageObserved(ResponsesWSEventProviderUsageObserved{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Usage:                     &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
		ReceivedAt:                time.Now(),
	})

	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil || actor.hasPendingProviderEvidence() || len(actor.turns.pending.provider.journal.DownstreamEvents()) != 0 {
		t.Fatalf("expected orphan usage not to create turn quota state, pending=%v active=%v evidence=%v events=%d", actor.turns.pending.attempt != nil, actor.turns.active.attempt != nil, actor.hasPendingProviderEvidence(), len(actor.turns.pending.provider.journal.DownstreamEvents()))
	}
	if got := atomic.LoadInt32(&conn.writeCount); got != 0 {
		t.Fatalf("expected orphan usage not to write downstream, got %d writes", got)
	}
	if actor.closing.closed.Load() || actor.state != responsesWSStateIdle {
		t.Fatalf("expected orphan usage not to close or mutate actor lifecycle, closed=%v state=%v", actor.closing.closed.Load(), actor.state)
	}
}

func TestResponsesWSProviderRecvPumpDeletesArmedGenerationOnExit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	defer bridge.Close()
	session := &responsesWSRecvSequenceSession{responses: make(chan responsesWSRecvResult, 1)}

	const generation = "session-armed-cleanup"
	bridge.ArmProviderRecvPump(generation, 17, session)
	if _, ok := bridge.armed.Load(generation); !ok {
		t.Fatal("expected provider generation to be armed")
	}

	session.responses <- responsesWSRecvResult{err: responsesws.ErrUpstreamClosed}
	event := readResponsesWSEvent(t, actor)
	if providerErr, ok := event.(ResponsesWSEventProviderBusinessError); !ok || providerErr.UpstreamSessionGeneration != generation {
		t.Fatalf("expected provider business error event, got %#v", event)
	}

	waitResponsesWSTestCondition(t, time.Second, 10*time.Millisecond, func() bool {
		if _, ok := bridge.armed.Load(generation); !ok {
			return true
		}
		return false
	}, func() string {
		return "expected provider recv pump to delete armed generation on exit"
	})
}

func TestResponsesWSTurnAttemptFinalUsageStartsWithoutPromptEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalApproximate := config.ApproximateTokenEnabled
	config.ApproximateTokenEnabled = true
	t.Cleanup(func() {
		config.ApproximateTokenEnabled = originalApproximate
	})

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"gpt-5": {
				Model:  "gpt-5",
				Type:   model.TokensPriceType,
				Input:  1,
				Output: 1,
			},
		},
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group_ratio", 1.0)

	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:      ctx,
		BillingModel: "gpt-5",
		PromptModel:  "gpt-5",
		Request:      &types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hello world"},
	})
	if apiErr != nil {
		t.Fatalf("expected attempt preparation to succeed, got %v", apiErr)
	}
	if attempt.Usage.PromptTokens != 0 {
		t.Fatalf("expected final usage not to be seeded with prompt estimate, got %d", attempt.Usage.PromptTokens)
	}

	mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 7, OutputTokens: 3, TotalTokens: 10})
	if attempt.Usage.PromptTokens != 7 || attempt.Usage.CompletionTokens != 3 || attempt.Usage.TotalTokens != 10 {
		t.Fatalf("expected provider usage to be authoritative, got %+v", attempt.Usage)
	}
}

func TestResponsesWSTerminalResponseAddsToolBillingWithoutDoubleCounting(t *testing.T) {
	response := &types.OpenAIResponsesResponses{
		Usage: &types.ResponsesUsage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8},
		Tools: []types.ResponsesTools{
			{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"},
		},
		Output: []types.ResponsesOutput{
			{Type: types.InputTypeWebSearchCall, ID: "ws_1"},
		},
	}

	usage := &types.Usage{}
	mergeResponsesWSTerminalResponse(usage, response)
	entry, ok := usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok || entry.CallCount != 1 || entry.Type != "high" {
		t.Fatalf("expected terminal response tool billing to be applied, got %+v", usage.ExtraBilling)
	}
	if usage.PromptTokens != 3 || usage.CompletionTokens != 5 || usage.TotalTokens != 8 {
		t.Fatalf("expected terminal response usage to be copied, got %+v", usage)
	}

	usageWithProviderBilling := &types.Usage{}
	mergeResponsesWSUsageEvent(usageWithProviderBilling, &types.UsageEvent{
		InputTokens:  3,
		OutputTokens: 5,
		TotalTokens:  8,
		ExtraBilling: map[string]types.ExtraBilling{
			types.APIToolTypeWebSearchPreview: {
				ServiceType: types.APIToolTypeWebSearchPreview,
				Type:        "high",
				CallCount:   1,
			},
		},
	})
	mergeResponsesWSTerminalResponse(usageWithProviderBilling, response)
	if got := usageWithProviderBilling.ExtraBilling[types.APIToolTypeWebSearchPreview].CallCount; got != 1 {
		t.Fatalf("expected provider and terminal tool billing to stay idempotent, got %d", got)
	}
}

func TestResponsesWSUsageDetailsMapAudioAndCacheFields(t *testing.T) {
	usage := &types.Usage{}
	mergeResponsesWSUsageEvent(usage, &types.UsageEvent{
		InputTokens: 1,
		InputTokenDetails: types.PromptTokensDetails{
			AudioTokens:       2,
			CachedTokens:      3,
			CachedReadTokens:  4,
			CachedWriteTokens: 5,
		},
		OutputTokenDetails: types.CompletionTokensDetails{
			ReasoningTokens: 6,
		},
	})
	mergeResponsesWSUsageEvent(usage, &types.UsageEvent{
		InputTokens: 1,
		InputTokenDetails: types.PromptTokensDetails{
			AudioTokens:       7,
			CachedTokens:      11,
			CachedReadTokens:  13,
			CachedWriteTokens: 17,
		},
		OutputTokenDetails: types.CompletionTokensDetails{
			ReasoningTokens: 19,
		},
	})

	if usage.PromptTokensDetails.AudioTokens != 9 ||
		usage.PromptTokensDetails.CachedTokens != 14 ||
		usage.PromptTokensDetails.CachedReadTokens != 17 ||
		usage.PromptTokensDetails.CachedWriteTokens != 22 ||
		usage.CompletionTokensDetails.ReasoningTokens != 25 {
		t.Fatalf("expected usage event details to accumulate, got %+v / %+v", usage.PromptTokensDetails, usage.CompletionTokensDetails)
	}

	responseUsage := (&types.Usage{
		PromptTokens: 3,
		PromptTokensDetails: types.PromptTokensDetails{
			AudioTokens:       23,
			CachedTokens:      29,
			CachedReadTokens:  31,
			CachedWriteTokens: 37,
			TextTokens:        41,
			ImageTokens:       43,
		},
	}).ToResponsesUsage()
	if responseUsage.InputTokensDetails == nil ||
		responseUsage.InputTokensDetails.AudioTokens != 23 ||
		responseUsage.InputTokensDetails.CachedReadTokens != 31 ||
		responseUsage.InputTokensDetails.CachedWriteTokens != 37 {
		t.Fatalf("expected Usage.ToResponsesUsage to preserve audio/cache details, got %+v", responseUsage.InputTokensDetails)
	}

	usageFromResponses := responseUsage.ToOpenAIUsage()
	if usageFromResponses.PromptTokensDetails.AudioTokens != 23 ||
		usageFromResponses.PromptTokensDetails.CachedReadTokens != 31 ||
		usageFromResponses.PromptTokensDetails.CachedWriteTokens != 37 {
		t.Fatalf("expected ResponsesUsage.ToOpenAIUsage to preserve audio/cache details, got %+v", usageFromResponses.PromptTokensDetails)
	}

	mergeResponsesWSResponsesUsage(usage, &types.ResponsesUsage{
		InputTokens: 5,
		InputTokensDetails: &types.ResponsesUsageInputTokensDetails{
			AudioTokens:       47,
			CachedTokens:      53,
			CachedReadTokens:  59,
			CachedWriteTokens: 61,
		},
	})
	if usage.PromptTokens != 5 ||
		usage.PromptTokensDetails.AudioTokens != 47 ||
		usage.PromptTokensDetails.CachedReadTokens != 59 ||
		usage.PromptTokensDetails.CachedWriteTokens != 61 {
		t.Fatalf("expected terminal response usage to map full input details, got %+v", usage)
	}
}

func TestResponsesWSUsageEventAccumulatesExtraBilling(t *testing.T) {
	usage := &types.Usage{}
	mergeResponsesWSUsageEvent(usage, &types.UsageEvent{
		ExtraBilling: map[string]types.ExtraBilling{
			types.APIToolTypeWebSearchPreview: {
				ServiceType: types.APIToolTypeWebSearchPreview,
				Type:        "high",
				CallCount:   1,
			},
			" image_generation|high-1024x1024 ": {
				CallCount: 1,
			},
		},
	})
	mergeResponsesWSUsageEvent(usage, &types.UsageEvent{
		ExtraBilling: map[string]types.ExtraBilling{
			types.APIToolTypeWebSearchPreview: {
				ServiceType: types.APIToolTypeWebSearchPreview,
				Type:        "high",
				CallCount:   2,
			},
			types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "high-1024x1024"): {
				CallCount: 2,
			},
		},
	})

	if got := usage.ExtraBilling[types.APIToolTypeWebSearchPreview].CallCount; got != 3 {
		t.Fatalf("expected repeated web search billing events to accumulate, got %d in %+v", got, usage.ExtraBilling)
	}
	imageKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "high-1024x1024")
	if got := usage.ExtraBilling[imageKey].CallCount; got != 3 {
		t.Fatalf("expected image billing keys to normalize and accumulate, got %d in %+v", got, usage.ExtraBilling)
	}
}

func TestResponsesWSLocalStaleContinuationDoesNotClearProviderAffinity(t *testing.T) {
	if isProviderReportedContinuationMiss(responsesws.ErrStaleContinuation) {
		t.Fatal("expected local stale continuation guard not to clear provider affinity")
	}
	payload := string(responsesWSErrorFromErr(responsesws.ErrStaleContinuation))
	assertResponsesWSErrorPayload(t, payload, http.StatusConflict, "previous_response_not_found", "previous response was not found")
	if !strings.Contains(payload, `"param":"previous_response_id"`) {
		t.Fatalf("expected previous_response_id param in stale payload, got %q", payload)
	}
}

func TestResponsesWSActorCloseRollsBackPendingAttemptWithoutProviderEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	prepareActor := NewResponsesWSSessionActor(ctx)
	prepareAttempt := &ResponsesWSTurnAttempt{AttemptID: "attempt-pending-prepare-close"}
	prepareActor.turns.pending.attempt = prepareAttempt
	prepareActor.state = responsesWSStatePendingPrepare
	prepareActor.close("test_pending_prepare")
	if !prepareAttempt.RolledBack {
		t.Fatalf("expected pending_prepare close to rollback not-sent attempt")
	}

	sendActor := NewResponsesWSSessionActor(ctx)
	sendAttempt := &ResponsesWSTurnAttempt{}
	sendActor.turns.pending.attempt = sendAttempt
	sendActor.state = responsesWSStatePendingSend
	sendActor.close("test_pending_send")
	if sendAttempt.RolledBack {
		t.Fatalf("expected pending_send close with unknown send outcome to preserve preconsume")
	}

	notSentActor := NewResponsesWSSessionActor(ctx)
	notSentAttempt := &ResponsesWSTurnAttempt{
		AttemptID: "attempt-not-sent-close",
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    responsesws.ErrUpstreamClosed,
		},
	}
	notSentActor.turns.pending.attempt = notSentAttempt
	notSentActor.state = responsesWSStatePendingSend
	notSentActor.close("test_not_sent")
	if !notSentAttempt.RolledBack {
		t.Fatalf("expected explicit not-sent outcome to rollback pending attempt")
	}
}

func TestResponsesWSSendQueueFullNotSentBeatsQueuedClientClose(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-send-queue-full-close-race",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal = responsesWSProviderJournal{}
	actor.turns.pending.provider.journal.AppendDownstreamReplay(ResponsesWSEventProviderDownstream{
		ChannelID: 17,
	})
	actor.turns.pending.provider.journal.bytes = 123
	actor.turns.pending.provider.journal.AppendFailureReplay(ResponsesWSEventProviderRecvFailed{
		ChannelID: 17,
		Err:       errors.New("stale pending failure"),
	})
	actor.turns.pending.provider.bridgeOpenLocalErrorAttemptID = "stale-marker"
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend
	if !actor.Post(ResponsesWSEventClientClosed{}) {
		t.Fatal("expected queued client close")
	}

	actor.handleSendQueueFull(attempt.AttemptID, 17)

	if !attempt.RolledBack || attempt.QuotaPreconsumed {
		t.Fatalf("expected synchronous NotSent to rollback before queued close can settle, attempt=%+v", attempt)
	}
	if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone || actor.state != responsesWSStateIdle {
		t.Fatalf("expected actor to return idle after send queue full, state=%v phase=%v pending=%+v", actor.state, actor.turns.pending.phase, actor.turns.pending.attempt)
	}
	if len(actor.turns.pending.provider.journal.DownstreamEvents()) != 0 || actor.turns.pending.provider.journal.bytes != 0 || len(actor.turns.pending.provider.journal.Failures()) != 0 || !actor.turns.pending.provider.journal.Project().IsZero() || actor.turns.pending.provider.bridgeOpenLocalErrorAttemptID != "" {
		t.Fatalf("expected NotSent cleanup to clear pending provider state, events=%d bytes=%d failures=%d evidence=%+v marker=%q",
			len(actor.turns.pending.provider.journal.DownstreamEvents()), actor.turns.pending.provider.journal.bytes, len(actor.turns.pending.provider.journal.Failures()), actor.turns.pending.provider.journal.Project(), actor.turns.pending.provider.bridgeOpenLocalErrorAttemptID)
	}
	if got, _ := conn.lastWrite.Load().(string); got == "" {
		t.Fatal("expected client-facing send failure payload")
	}
	if generation == "" {
		t.Fatal("expected attached generation")
	}
}

func TestResponsesWSActorCloseAfterRecoverableBridgeOpenProviderErrorRollsBackPendingQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-bridge-open-provider-error-close",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusTooManyRequests, "provider_rate_limit", "provider rejected stream open"),
		Recoverable:               true,
	})

	if actor.hasPendingProviderEvidence() || !actor.pendingBridgeOpenProviderErrorRecorded() {
		t.Fatalf("expected bridge open provider error proof without provider activity, evidence=%+v", actor.turns.pending.provider.journal.Project())
	}
	actor.markPendingCreateCancel(attempt.AttemptID, responsesWSTestClientTextFrame([]byte(`{"type":"response.cancel"}`)))

	actor.close("client_closed_before_rejected_send_result")

	if !attempt.RolledBack || attempt.QuotaPreconsumed || attempt.QuotaFinalized {
		t.Fatalf("expected close after bridge open provider error to rollback without finalize, attempt=%+v", attempt)
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected bridge open provider error close not to submit terminal side effect, got %+v", actor.turns.history.lastFinal)
	}
	if actor.turns.pending.cancel.createAttemptID != "" || !actor.turns.pending.cancel.createFrame.IsZero() {
		t.Fatalf("expected close to clear pending create cancel marker, attempt_id=%q frame=%+v",
			actor.turns.pending.cancel.createAttemptID, actor.turns.pending.cancel.createFrame)
	}
}

func TestResponsesWSRecoverableBridgeOpenProviderErrorWithActivitySettlesWithoutFailClosed(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-bridge-open-provider-conflict")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal = responsesWSTestProviderFrameJournal()
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusTooManyRequests, "provider_rate_limit", "provider rejected stream open"),
		Recoverable:               true,
	})

	if attempt.RolledBack {
		t.Fatalf("expected provider activity to suppress bridge rejection rollback, attempt=%+v", attempt)
	}
	if !attempt.QuotaFinalized {
		t.Fatalf("expected provider rejection/activity conflict to finalize conservatively, attempt=%+v", attempt)
	}
	if actor.closing.closed.Load() {
		t.Fatal("expected provider rejection/activity conflict not to fail closed")
	}
	if actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil {
		t.Fatalf("expected conflict cleanup to clear turn state, pending=%+v active=%+v", actor.turns.pending.attempt, actor.turns.active.attempt)
	}
}

func TestResponsesWSRejectedBeforeStreamAfterRecoveredProviderActivitySettlesWithoutFailClosed(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-bridge-open-provider-late-conflict")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		Payload:                   responsesWSErrorPayload(http.StatusTooManyRequests, "provider_rate_limit", "provider rejected stream open"),
		Recoverable:               true,
	})
	if actor.closing.closed.Load() || attempt.RolledBack || !actor.pendingBridgeOpenProviderErrorRecorded() {
		t.Fatalf("expected recoverable provider rejection proof to wait for send result, closed=%v attempt=%+v evidence=%+v", actor.closing.closed.Load(), attempt, actor.turns.pending.provider.journal.Project())
	}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.created","response":{"id":"resp_conflict","status":"in_progress"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})
	if !actor.hasPendingProviderEvidence() {
		t.Fatalf("expected provider activity to be retained before rejected send result, evidence=%+v", actor.turns.pending.provider.journal.Project())
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendRejectedBeforeStream,
		},
	})

	if attempt.RolledBack {
		t.Fatalf("expected provider activity to suppress rejected_before_stream rollback, attempt=%+v", attempt)
	}
	if !attempt.QuotaFinalized {
		t.Fatalf("expected rejected_before_stream/activity conflict to finalize conservatively, attempt=%+v", attempt)
	}
	if actor.closing.closed.Load() {
		t.Fatal("expected rejected_before_stream/activity conflict not to fail closed")
	}
	if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone {
		t.Fatalf("expected rejected_before_stream/activity conflict to clear pending turn, pending=%+v phase=%v", actor.turns.pending.attempt, actor.turns.pending.phase)
	}
}

func TestResponsesWSActorClosePendingSendUnknownPreservesPreConsumedQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)

	setupRelayTestDB(t, &model.User{}, &model.Token{}, &model.Log{})

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"gpt-5": {
				Model: "gpt-5",
				Type:  model.TimesPriceType,
				Input: 0.1,
			},
		},
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	originalBatchUpdate := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	t.Cleanup(func() {
		config.BatchUpdateEnabled = originalBatchUpdate
		config.RedisEnabled = originalRedisEnabled
	})

	if err := model.DB.Create(&model.User{
		Id:          1,
		Username:    "alice",
		Password:    "password123",
		AccessToken: "access-token-1",
		Quota:       1000,
		Group:       "default",
		Status:      config.UserStatusEnabled,
		Role:        config.RoleCommonUser,
		DisplayName: "Alice",
		CreatedTime: 1,
	}).Error; err != nil {
		t.Fatalf("expected user fixture to persist, got %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id:          1,
		UserId:      1,
		Key:         "token-key-1",
		Name:        "token-alpha",
		RemainQuota: 1000,
		Group:       "default",
	}).Error; err != nil {
		t.Fatalf("expected token fixture to persist, got %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("token_group", "default")
	ctx.Set("group_ratio", 1.0)

	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:      ctx,
		BillingModel: "gpt-5",
		PromptModel:  "gpt-5",
		Request:      &types.OpenAIResponsesRequest{Model: "gpt-5", Input: []types.ChatCompletionMessage{}},
	})
	if apiErr != nil {
		t.Fatalf("expected attempt preparation to succeed, got %v", apiErr)
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		t.Fatalf("expected quota preconsume to succeed, got %v", apiErr)
	}
	attempt.AttemptID = "attempt-pending-send-unknown-close"

	actor := NewResponsesWSSessionActor(ctx)
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend
	actor.close("test_no_terminal_settle")

	if attempt.RolledBack {
		t.Fatalf("expected unknown pending_send close to preserve quota")
	}
	if !attempt.QuotaFinalized {
		t.Fatalf("expected unknown pending_send close to finalize quota floor")
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup after no-terminal settle to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup after no-terminal settle to succeed, got %v", err)
	}
	if user.Quota != 900 || user.UsedQuota != 100 || user.RequestCount != 1 {
		t.Fatalf("expected unknown pending_send close to keep preconsume, quota=%d used=%d requests=%d", user.Quota, user.UsedQuota, user.RequestCount)
	}
	if token.RemainQuota != 900 || token.UsedQuota != 100 {
		t.Fatalf("expected unknown pending_send close to keep token preconsume, remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}

func TestResponsesWSActorCloseProviderAcceptedWithoutUsagePreservesPreConsumedFloor(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)

	actor := NewResponsesWSSessionActor(ctx)
	actor.turns.active.attempt = attempt
	actor.turns.active.evidence = responsesWSTestProviderFrameProjection()
	actor.state = responsesWSStateInFlight
	actor.close("test_provider_accepted_no_usage")

	if attempt.RolledBack {
		t.Fatalf("expected accepted provider evidence without usage to preserve quota floor")
	}
	if !attempt.QuotaFinalized {
		t.Fatalf("expected accepted provider evidence without usage to finalize quota floor")
	}

	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 900 || user.UsedQuota != 100 || user.RequestCount != 1 {
		t.Fatalf("expected accepted no-usage close to keep preconsume, quota=%d used=%d requests=%d", user.Quota, user.UsedQuota, user.RequestCount)
	}
	if token.RemainQuota != 900 || token.UsedQuota != 100 {
		t.Fatalf("expected accepted no-usage close to keep token preconsume, remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}

func TestResponsesWSActorCloseUsageSeenWithoutTokenCountsPreservesPreConsumedFloor(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)

	actor := NewResponsesWSSessionActor(ctx)
	actor.turns.active.attempt = attempt
	actor.turns.active.evidence = responsesWSTestProviderUsageProjection()
	actor.state = responsesWSStateInFlight
	actor.close("test_provider_usage_seen_no_tokens")

	if attempt.RolledBack {
		t.Fatalf("expected usage-seen evidence to keep quota floor")
	}
	if !attempt.QuotaFinalized {
		t.Fatalf("expected usage-seen evidence to finalize quota")
	}

	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 900 || user.UsedQuota != 100 || user.RequestCount != 1 {
		t.Fatalf("expected usage-seen no-token settle to keep preconsumed quota, quota=%d used=%d requests=%d", user.Quota, user.UsedQuota, user.RequestCount)
	}
	if token.RemainQuota != 900 || token.UsedQuota != 100 {
		t.Fatalf("expected usage-seen no-token settle to keep token preconsume, remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}

func TestResponsesWSActorCancelledTerminalFinalizesAndClearsActiveTurn(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)

	actor := NewResponsesWSSessionActor(ctx)
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	bridge := NewResponsesWSIOBridge(conn, actor)
	actor.SetBridge(bridge)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:    responsesWSTestCurrentAttemptID(actor),
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.cancelled","response":{"id":"resp_cancel","status":"cancelled","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	})

	if actor.turns.active.attempt != nil || actor.turns.active.channelID != 0 || actor.state != responsesWSStateIdle {
		t.Fatalf("expected cancelled terminal to clear active turn, active=%v channel=%d state=%v", actor.turns.active.attempt != nil, actor.turns.active.channelID, actor.state)
	}
	if !attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected cancelled terminal with usage to finalize quota, finalized=%v rolled=%v", attempt.QuotaFinalized, attempt.RolledBack)
	}
	if got := atomic.LoadInt32(&conn.writeCount); got != 1 {
		t.Fatalf("expected cancelled provider frame to be forwarded once, got %d", got)
	}
}

func TestResponsesWSSettlementLogMarksStreamProtocolAndTiming(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	startedAt := time.Now().Add(-1 * time.Second)
	firstResponseAt := startedAt.Add(250 * time.Millisecond)
	completedAt := startedAt.Add(700 * time.Millisecond)
	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:           ctx,
		SelectedChannelID: 17,
		BillingModel:      "gpt-5",
		PromptModel:       "gpt-5",
		Request:           &types.OpenAIResponsesRequest{Model: "gpt-5", Input: []types.ChatCompletionMessage{}},
		StartedAt:         startedAt,
	})
	if apiErr != nil {
		t.Fatalf("expected attempt preparation to succeed, got %v", apiErr)
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		t.Fatalf("expected quota preconsume to succeed, got %v", apiErr)
	}
	attempt.AttemptID = "attempt-settlement-log"
	attempt.MarkFirstProviderResponse(firstResponseAt)

	actor := NewResponsesWSSessionActor(ctx)
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	bridge := NewResponsesWSIOBridge(conn, actor)
	actor.SetBridge(bridge)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:    responsesWSTestCurrentAttemptID(actor),
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_done","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   completedAt,
	})

	var log model.Log
	if err := model.DB.Order("id desc").First(&log).Error; err != nil {
		t.Fatalf("expected consume log to be written, got %v", err)
	}
	if !log.IsStream {
		t.Fatal("expected responses websocket log to set is_stream=true")
	}
	if log.RequestTime != 700 {
		t.Fatalf("expected responses websocket log to record provider completion request time 700ms, got %d", log.RequestTime)
	}
	meta := log.Metadata.Data()
	if got := meta["protocol"]; got != relay_util.LogProtocolResponsesWS {
		t.Fatalf("expected responses websocket protocol metadata %q, got %#v", relay_util.LogProtocolResponsesWS, got)
	}
	if got := meta["first_response"]; got != float64(250) && got != int64(250) && got != int(250) {
		t.Fatalf("expected first_response metadata 250ms, got %#v", got)
	}
}

func TestResponsesWSActorCloseActiveNoProviderEvidencePreservesPreConsumedFloor(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)

	actor := NewResponsesWSSessionActor(ctx)
	actor.turns.active.attempt = attempt
	actor.state = responsesWSStateInFlight
	actor.close("client_closed")

	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected active no-evidence close to preserve quota floor, rolled=%v finalized=%v", attempt.RolledBack, attempt.QuotaFinalized)
	}
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 900 || user.UsedQuota != 100 || user.RequestCount != 1 {
		t.Fatalf("expected active no-evidence close to keep user preconsume, quota=%d used=%d requests=%d", user.Quota, user.UsedQuota, user.RequestCount)
	}
	if token.RemainQuota != 900 || token.UsedQuota != 100 {
		t.Fatalf("expected active no-evidence close to keep token preconsume, remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}

func configureResponsesWSTokenPricingFloor(t *testing.T, floor int) {
	t.Helper()
	originalPricing := model.PricingInstance
	originalPreConsumedQuota := config.PreConsumedQuota
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"gpt-5": {
				Model:  "gpt-5",
				Type:   model.TokensPriceType,
				Input:  1,
				Output: 1,
			},
		},
	}
	config.PreConsumedQuota = floor
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.PreConsumedQuota = originalPreConsumedQuota
	})
}

func TestResponsesWSApplySettlementDecisionUsesFixedFinalQuota(t *testing.T) {
	t.Run("terminal exact below floor refunds to exact", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		response := &types.OpenAIResponsesResponses{
			ID:     "resp_exact",
			Status: types.ResponseStatusCompleted,
			Usage:  &types.ResponsesUsage{InputTokens: 10, TotalTokens: 10},
		}
		mergeResponsesWSTerminalResponse(attempt.Usage, response)
		attempt.MarkProviderTerminalEvidence(responsesws.ClassifyResponsesWSTerminal("response.completed", response, false))

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		_, applied, err := actor.applyActiveSettlement(actor.buildActiveSettlementInput("terminal", ResponsesWSZeroChargeProof{}))
		if err != nil {
			t.Fatalf("expected exact settlement to succeed, got %v", err)
		}
		if applied.AppliedFinalQuota != 10 {
			t.Fatalf("expected terminal exact applied quota 10 below floor, got %d", applied.AppliedFinalQuota)
		}
		user, token := readResponsesWSQuotaFixture(t)
		if user.Quota != 990 || user.UsedQuota != 10 || token.RemainQuota != 990 || token.UsedQuota != 10 {
			t.Fatalf("expected exact final quota 10, user=%+v token=%+v", user, token)
		}
		log := readResponsesWSConsumeLog(t)
		if log.Quota != 10 || log.PromptTokens != 10 || log.CompletionTokens != 0 {
			t.Fatalf("expected exact consume log to preserve terminal usage, got %+v", log)
		}
	})

	t.Run("terminal explicit zero usage refunds to zero", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		response := &types.OpenAIResponsesResponses{
			ID:     "resp_exact_zero",
			Status: types.ResponseStatusCompleted,
			Usage:  &types.ResponsesUsage{},
		}
		mergeResponsesWSTerminalResponse(attempt.Usage, response)
		attempt.MarkProviderTerminalEvidence(responsesws.ClassifyResponsesWSTerminal("response.completed", response, false))

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		decision, applied, err := actor.applyActiveSettlement(actor.buildActiveSettlementInput("terminal_zero", ResponsesWSZeroChargeProof{}))
		if err != nil {
			t.Fatalf("expected exact zero settlement to succeed, got %v decision=%+v", err, decision)
		}
		if decision.Action != ResponsesWSSettlementFinalizeExactUsage || decision.Basis != ResponsesWSSettlementBasisTerminalUsage {
			t.Fatalf("expected explicit terminal usage to use exact settlement, decision=%+v", decision)
		}
		if applied.AppliedFinalQuota != 0 {
			t.Fatalf("expected terminal exact zero applied quota, got %d", applied.AppliedFinalQuota)
		}
		user, token := readResponsesWSQuotaFixture(t)
		if user.Quota != 1000 || user.UsedQuota != 0 || token.RemainQuota != 1000 || token.UsedQuota != 0 {
			t.Fatalf("expected exact final quota 0 to refund preconsume, user=%+v token=%+v", user, token)
		}
	})

	t.Run("terminal explicit zero ignores previously observed usage", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 10, TotalTokens: 10})
		response := &types.OpenAIResponsesResponses{
			ID:     "resp_exact_zero_after_observed",
			Status: types.ResponseStatusCompleted,
			Usage:  &types.ResponsesUsage{},
		}
		mergeResponsesWSTerminalResponse(attempt.Usage, response)
		attempt.MarkProviderTerminalEvidence(responsesws.ClassifyResponsesWSTerminal("response.completed", response, false))

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		decision, applied, err := actor.applyActiveSettlement(actor.buildActiveSettlementInput("terminal_zero_after_observed", ResponsesWSZeroChargeProof{}))
		if err != nil {
			t.Fatalf("expected exact zero settlement to succeed, got %v decision=%+v", err, decision)
		}
		if decision.ExpectedFinalQuota != 0 || applied.AppliedFinalQuota != 0 {
			t.Fatalf("expected terminal snapshot zero to win over observed usage, decision=%+v applied=%+v", decision, applied)
		}
		user, token := readResponsesWSQuotaFixture(t)
		if user.Quota != 1000 || user.UsedQuota != 0 || token.RemainQuota != 1000 || token.UsedQuota != 0 {
			t.Fatalf("expected exact final quota 0 after observed usage, user=%+v token=%+v", user, token)
		}
	})

	t.Run("terminal smaller snapshot ignores stale observed output tokens", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 10, OutputTokens: 90, TotalTokens: 100})
		response := &types.OpenAIResponsesResponses{
			ID:     "resp_exact_ten_after_observed",
			Status: types.ResponseStatusCompleted,
			Usage:  &types.ResponsesUsage{InputTokens: 10, TotalTokens: 10},
		}
		mergeResponsesWSTerminalResponse(attempt.Usage, response)
		attempt.MarkProviderTerminalEvidence(responsesws.ClassifyResponsesWSTerminal("response.completed", response, false))

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		decision, applied, err := actor.applyActiveSettlement(actor.buildActiveSettlementInput("terminal_ten_after_observed", ResponsesWSZeroChargeProof{}))
		if err != nil {
			t.Fatalf("expected exact smaller settlement to succeed, got %v decision=%+v", err, decision)
		}
		if decision.ExpectedFinalQuota != 10 || applied.AppliedFinalQuota != 10 {
			t.Fatalf("expected terminal snapshot quota 10 to win over observed usage, decision=%+v applied=%+v", decision, applied)
		}
		log := readResponsesWSConsumeLog(t)
		if log.Quota != 10 || log.PromptTokens != 10 || log.CompletionTokens != 0 {
			t.Fatalf("expected exact consume log to use terminal snapshot, got %+v", log)
		}
	})

	t.Run("terminal positive tokens on zero price model refunds to zero", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		originalPricing := model.PricingInstance
		originalPreConsumedQuota := config.PreConsumedQuota
		model.PricingInstance = &model.Pricing{
			Prices: map[string]*model.Price{
				"gpt-5": {
					Model:  "gpt-5",
					Type:   model.TokensPriceType,
					Input:  0,
					Output: 0,
				},
			},
		}
		config.PreConsumedQuota = 100
		t.Cleanup(func() {
			model.PricingInstance = originalPricing
			config.PreConsumedQuota = originalPreConsumedQuota
		})
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		response := &types.OpenAIResponsesResponses{
			ID:     "resp_exact_zero_price",
			Status: types.ResponseStatusCompleted,
			Usage:  &types.ResponsesUsage{InputTokens: 10, TotalTokens: 10},
		}
		mergeResponsesWSTerminalResponse(attempt.Usage, response)
		attempt.MarkProviderTerminalEvidence(responsesws.ClassifyResponsesWSTerminal("response.completed", response, false))

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		decision, applied, err := actor.applyActiveSettlement(actor.buildActiveSettlementInput("terminal_zero_price", ResponsesWSZeroChargeProof{}))
		if err != nil {
			t.Fatalf("expected zero-price terminal settlement to succeed, got %v decision=%+v", err, decision)
		}
		if decision.Action != ResponsesWSSettlementFinalizeExactUsage || decision.Basis != ResponsesWSSettlementBasisTerminalUsage {
			t.Fatalf("expected zero-price terminal usage to use exact settlement, decision=%+v", decision)
		}
		if applied.AppliedFinalQuota != 0 {
			t.Fatalf("expected zero-price terminal exact applied quota 0, got %d", applied.AppliedFinalQuota)
		}
		user, token := readResponsesWSQuotaFixture(t)
		if user.Quota != 1000 || user.UsedQuota != 0 || token.RemainQuota != 1000 || token.UsedQuota != 0 {
			t.Fatalf("expected zero-price exact final quota 0 to refund preconsume, user=%+v token=%+v", user, token)
		}
	})

	t.Run("observed below floor preserves floor", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 10, TotalTokens: 10})
		floor := int64(attempt.Quota.PreConsumedQuota())

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		actor.turns.active.evidence = responsesWSTestProviderUsageProjection()
		_, applied, err := actor.applyActiveSettlement(actor.buildActiveSettlementInput("usage_only", ResponsesWSZeroChargeProof{}))
		if err != nil {
			t.Fatalf("expected observed settlement to succeed, got %v", err)
		}
		if applied.AppliedFinalQuota != floor {
			t.Fatalf("expected observed below floor to apply floor %d, got %d", floor, applied.AppliedFinalQuota)
		}
		user, token := readResponsesWSQuotaFixture(t)
		if user.Quota != 1000-int(floor) || user.UsedQuota != int(floor) || token.RemainQuota != 1000-int(floor) || token.UsedQuota != int(floor) {
			t.Fatalf("expected floor final quota 100, user=%+v token=%+v", user, token)
		}
		log := readResponsesWSConsumeLog(t)
		if log.Quota != int(floor) || log.PromptTokens != 10 || log.CompletionTokens != 0 {
			t.Fatalf("expected observed floor consume log to preserve observed usage, got %+v", log)
		}
	})

	t.Run("pure floor has empty usage summary", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		floor := int64(attempt.Quota.PreConsumedQuota())

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		_, applied, err := actor.applyActiveSettlement(actor.buildActiveSettlementInput("uncertain_floor", ResponsesWSZeroChargeProof{}))
		if err != nil {
			t.Fatalf("expected floor settlement to succeed, got %v", err)
		}
		if applied.AppliedFinalQuota != floor {
			t.Fatalf("expected pure floor applied quota %d, got %d", floor, applied.AppliedFinalQuota)
		}
		log := readResponsesWSConsumeLog(t)
		if log.Quota != int(floor) || log.PromptTokens != 0 || log.CompletionTokens != 0 {
			t.Fatalf("expected pure floor consume log to omit usage details, got %+v", log)
		}
	})

	t.Run("zero proof rolls back reserve", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.pending.attempt = attempt
		_, applied, err := actor.applyPendingSettlement(actor.buildPendingSettlementInput("send_not_sent", responsesWSZeroChargeProof(ResponsesWSZeroChargeProofTransportNotAttempted, "send_not_sent")))
		if err != nil {
			t.Fatalf("expected rollback settlement to succeed, got %v", err)
		}
		if applied.AppliedFinalQuota != 0 || !attempt.RolledBack {
			t.Fatalf("expected rollback applied quota 0, applied=%+v attempt=%+v", applied, attempt)
		}
		user, token := readResponsesWSQuotaFixture(t)
		if user.Quota != 1000 || user.UsedQuota != 0 || token.RemainQuota != 1000 || token.UsedQuota != 0 {
			t.Fatalf("expected rollback to restore reserve, user=%+v token=%+v", user, token)
		}
	})
}

func TestResponsesWSApplySettlementDecisionFailsLoudForInvalidFinalSettlement(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)

	t.Run("nil quota", func(t *testing.T) {
		attempt := &ResponsesWSTurnAttempt{AttemptID: "attempt-nil-quota"}
		_, err := attempt.ApplyResponsesWSSettlementDecision(ctx, ResponsesWSSettlementDecision{
			Action:             ResponsesWSSettlementFinalizeFloor,
			Basis:              ResponsesWSSettlementBasisFloor,
			ExpectedFinalQuota: 100,
		})
		if err == nil || attempt.QuotaFinalized {
			t.Fatalf("expected nil quota finalize to fail without finalizing, err=%v attempt=%+v", err, attempt)
		}
	})

	t.Run("empty model", func(t *testing.T) {
		attempt := &ResponsesWSTurnAttempt{AttemptID: "attempt-empty-model", Quota: &relay_util.Quota{}}
		_, err := attempt.ApplyResponsesWSSettlementDecision(ctx, ResponsesWSSettlementDecision{
			Action:             ResponsesWSSettlementFinalizeFloor,
			Basis:              ResponsesWSSettlementBasisFloor,
			ExpectedFinalQuota: 100,
		})
		if err == nil || attempt.QuotaFinalized {
			t.Fatalf("expected empty model finalize to fail without finalizing, err=%v attempt=%+v", err, attempt)
		}
	})

	t.Run("missing attempt id finalize", func(t *testing.T) {
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		attempt.AttemptID = ""
		_, err := attempt.ApplyResponsesWSSettlementDecision(ctx, ResponsesWSSettlementDecision{
			Action:             ResponsesWSSettlementFinalizeFloor,
			Basis:              ResponsesWSSettlementBasisFloor,
			ExpectedFinalQuota: int64(attempt.Quota.PreConsumedQuota()),
		})
		if err == nil || attempt.QuotaFinalized {
			t.Fatalf("expected missing attempt id finalize to fail without finalizing, err=%v attempt=%+v", err, attempt)
		}
	})

	t.Run("missing attempt id rollback", func(t *testing.T) {
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		attempt.AttemptID = ""
		_, err := attempt.ApplyResponsesWSSettlementDecision(ctx, ResponsesWSSettlementDecision{
			Action:             ResponsesWSSettlementRollbackReserve,
			Basis:              ResponsesWSSettlementBasisZeroChargeProof,
			ExpectedFinalQuota: 0,
		})
		if err == nil || attempt.RolledBack {
			t.Fatalf("expected missing attempt id rollback to fail without rollback, err=%v attempt=%+v", err, attempt)
		}
	})

	t.Run("rollback non zero expected quota", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		floor := attempt.Quota.PreConsumedQuota()
		_, err := attempt.ApplyResponsesWSSettlementDecision(ctx, ResponsesWSSettlementDecision{
			Action:             ResponsesWSSettlementRollbackReserve,
			Basis:              ResponsesWSSettlementBasisZeroChargeProof,
			ExpectedFinalQuota: 1,
		})
		if err == nil || attempt.RolledBack || attempt.AppliedSettlement != nil {
			t.Fatalf("expected malformed rollback decision to fail without applying, err=%v attempt=%+v", err, attempt)
		}
		user, token := readResponsesWSQuotaFixture(t)
		if user.Quota != 1000-floor || user.UsedQuota != 0 || token.RemainQuota != 1000-floor || token.UsedQuota != floor {
			t.Fatalf("expected failed rollback to leave preconsume untouched, user=%+v token=%+v", user, token)
		}
	})

	t.Run("missing floor zero", func(t *testing.T) {
		attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
			Context:           ctx,
			SelectedChannelID: 17,
			BillingModel:      "gpt-5",
			PromptModel:       "gpt-5",
			Request:           &types.OpenAIResponsesRequest{Model: "gpt-5", Input: []types.ChatCompletionMessage{}},
		})
		if apiErr != nil {
			t.Fatalf("prepare attempt: %v", apiErr)
		}
		attempt.AttemptID = "attempt-missing-floor-zero"
		_, err := attempt.ApplyResponsesWSSettlementDecision(ctx, ResponsesWSSettlementDecision{
			Action:             ResponsesWSSettlementFinalizeFloor,
			Basis:              ResponsesWSSettlementBasisFloor,
			ExpectedFinalQuota: 0,
			Flags:              []ResponsesWSSettlementFlag{ResponsesWSSettlementFlagMissingSettlementFloor},
		})
		if err == nil || attempt.QuotaFinalized {
			t.Fatalf("expected missing floor zero to fail without finalizing, err=%v attempt=%+v", err, attempt)
		}
	})
}

func TestResponsesWSApplySettlementDecisionDuplicateFinalizeUsesStoredApplied(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	decision := ResponsesWSSettlementDecision{
		Action:             ResponsesWSSettlementFinalizeExactUsage,
		Basis:              ResponsesWSSettlementBasisTerminalUsage,
		ExpectedFinalQuota: 10,
	}
	first, err := attempt.ApplyResponsesWSSettlementDecision(ctx, decision)
	if err != nil {
		t.Fatalf("expected first finalize to succeed, got %v", err)
	}
	second, err := attempt.ApplyResponsesWSSettlementDecision(ctx, decision)
	if err != nil {
		t.Fatalf("expected duplicate same finalize to be idempotent, got %v", err)
	}
	if second != first {
		t.Fatalf("expected duplicate finalize to return stored applied, first=%+v second=%+v", first, second)
	}
	decision.ExpectedFinalQuota = 11
	if _, err := attempt.ApplyResponsesWSSettlementDecision(ctx, decision); err == nil {
		t.Fatal("expected duplicate finalize with different amount to fail")
	}
}

func TestResponsesWSApplySettlementDecisionRejectsRollbackAfterFinalize(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	finalize := ResponsesWSSettlementDecision{
		Action:             ResponsesWSSettlementFinalizeFloor,
		Basis:              ResponsesWSSettlementBasisFloor,
		ExpectedFinalQuota: int64(attempt.Quota.PreConsumedQuota()),
	}
	if _, err := attempt.ApplyResponsesWSSettlementDecision(ctx, finalize); err != nil {
		t.Fatalf("expected finalize to succeed, got %v", err)
	}
	rollback := ResponsesWSSettlementDecision{
		Action:             ResponsesWSSettlementRollbackReserve,
		Basis:              ResponsesWSSettlementBasisZeroChargeProof,
		ExpectedFinalQuota: 0,
	}
	if _, err := attempt.ApplyResponsesWSSettlementDecision(ctx, rollback); err == nil {
		t.Fatal("expected rollback after finalize to fail")
	}
	if attempt.RolledBack {
		t.Fatal("expected failed rollback after finalize not to mark attempt rolled back")
	}
}

func TestResponsesWSApplySettlementDecisionDuplicateRollbackUsesStoredApplied(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	decision := ResponsesWSSettlementDecision{
		Action:             ResponsesWSSettlementRollbackReserve,
		Basis:              ResponsesWSSettlementBasisZeroChargeProof,
		ExpectedFinalQuota: 0,
	}
	first, err := attempt.ApplyResponsesWSSettlementDecision(ctx, decision)
	if err != nil {
		t.Fatalf("expected first rollback to succeed, got %v", err)
	}
	second, err := attempt.ApplyResponsesWSSettlementDecision(ctx, decision)
	if err != nil {
		t.Fatalf("expected duplicate rollback to be idempotent, got %v", err)
	}
	if second != first {
		t.Fatalf("expected duplicate rollback to return stored applied, first=%+v second=%+v", first, second)
	}
}

func TestResponsesWSSettlementFailureDoesNotEmitSuccessfulTrace(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	actor := NewResponsesWSSessionActor(ctx)
	actor.turns.active.attempt = &ResponsesWSTurnAttempt{
		AttemptID: "attempt-invalid-settlement",
		Quota:     &relay_util.Quota{},
		Usage:     &types.Usage{},
	}

	var traces []ResponsesWSSettlementTrace
	originalHook := responsesWSSettlementTraceHook
	responsesWSSettlementTraceHook = func(trace ResponsesWSSettlementTrace) {
		traces = append(traces, trace)
	}
	t.Cleanup(func() {
		responsesWSSettlementTraceHook = originalHook
	})

	_, _, err := actor.applyActiveSettlement(actor.buildActiveSettlementInput("invalid_settlement", ResponsesWSZeroChargeProof{}))
	if err == nil {
		t.Fatal("expected invalid settlement to fail")
	}
	if len(traces) != 0 {
		t.Fatalf("expected failed settlement not to emit successful trace, got %+v", traces)
	}
}

func TestResponsesWSProviderActivityWithMissingFloorFailsLoud(t *testing.T) {
	providerOpened := responsesws.UpstreamEvent{
		DetailOrigin: responsesws.RecvDetailOriginBridgeStreamOpened,
	}
	cases := []struct {
		name  string
		apply func(*ResponsesWSSessionActor, *ResponsesWSTurnAttempt) (ResponsesWSSettlementInput, ResponsesWSSettlementDecision, error)
	}{
		{
			name: "pending journal",
			apply: func(actor *ResponsesWSSessionActor, attempt *ResponsesWSTurnAttempt) (ResponsesWSSettlementInput, ResponsesWSSettlementDecision, error) {
				actor.turns.pending.attempt = attempt
				actor.turns.pending.provider.journal.AppendLifecycle(providerOpened)
				input := actor.buildPendingSettlementInput("missing_floor_pending", ResponsesWSZeroChargeProof{})
				decision, _, err := actor.applyPendingSettlement(input)
				return input, decision, err
			},
		},
		{
			name: "active projection",
			apply: func(actor *ResponsesWSSessionActor, attempt *ResponsesWSTurnAttempt) (ResponsesWSSettlementInput, ResponsesWSSettlementDecision, error) {
				actor.turns.active.attempt = attempt
				actor.turns.active.evidence = responsesWSTestProviderProjection(providerOpened)
				input := actor.buildActiveSettlementInput("missing_floor_active", ResponsesWSZeroChargeProof{})
				decision, _, err := actor.applyActiveSettlement(input)
				return input, decision, err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupResponsesWSQuotaFixture(t, 1000)
			attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
				Context:           ctx,
				SelectedChannelID: 17,
				BillingModel:      "gpt-5",
				PromptModel:       "gpt-5",
				Request:           &types.OpenAIResponsesRequest{Model: "gpt-5", Input: []types.ChatCompletionMessage{}},
			})
			if apiErr != nil {
				t.Fatalf("prepare attempt: %v", apiErr)
			}
			attempt.AttemptID = "attempt-missing-floor-" + strings.ReplaceAll(tc.name, " ", "-")

			actor := NewResponsesWSSessionActor(ctx)
			var traces []ResponsesWSSettlementTrace
			originalHook := responsesWSSettlementTraceHook
			responsesWSSettlementTraceHook = func(trace ResponsesWSSettlementTrace) {
				traces = append(traces, trace)
			}
			t.Cleanup(func() {
				responsesWSSettlementTraceHook = originalHook
			})

			input, decision, err := tc.apply(actor, attempt)
			if !input.Evidence.AnyProviderActivityEvidence {
				t.Fatalf("expected provider activity evidence in settlement input, input=%+v", input)
			}
			if decision.Action != ResponsesWSSettlementFinalizeFloor && decision.Action != ResponsesWSSettlementFinalizeObservedOrFloor {
				t.Fatalf("expected floor settlement path, got decision=%+v", decision)
			}
			if decision.ExpectedFinalQuota != 0 || !responsesWSSettlementHasFlag(decision, ResponsesWSSettlementFlagMissingSettlementFloor) {
				t.Fatalf("expected missing-floor zero decision, got %+v", decision)
			}
			if err == nil {
				t.Fatal("expected missing floor settlement to fail loud")
			}
			if attempt.QuotaFinalized || attempt.RolledBack || attempt.AppliedSettlement != nil {
				t.Fatalf("expected failed settlement not to mutate settlement state, attempt=%+v", attempt)
			}
			if len(traces) != 0 {
				t.Fatalf("expected failed settlement not to emit successful trace, got %+v", traces)
			}
		})
	}
}

func TestResponsesWSActiveSettlementFailureStopsTerminalSideEffects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-terminal-settlement-fails",
		SelectedChannelID: 17,
		Quota:             &relay_util.Quota{},
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_no_settle","status":"completed","usage":{"input_tokens":1,"total_tokens":1}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected settlement failure to close session")
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected terminal success side effects not to run, lastFinal=%+v", actor.turns.history.lastFinal)
	}
	if actor.turns.active.attempt != attempt || attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected failed settlement to preserve active attempt state, active=%v finalized=%v rolled=%v", actor.turns.active.attempt == attempt, attempt.QuotaFinalized, attempt.RolledBack)
	}
}

func TestResponsesWSActiveTerminalNilQuotaFailsBeforeSideEffects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-terminal-nil-quota",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_nil_quota","status":"completed","usage":{"input_tokens":1,"total_tokens":1}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected nil quota settlement failure to close session")
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected nil quota terminal side effects not to run, lastFinal=%+v", actor.turns.history.lastFinal)
	}
	if actor.turns.active.attempt != attempt || attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected nil quota settlement failure to preserve active attempt, active=%v finalized=%v rolled=%v", actor.turns.active.attempt == attempt, attempt.QuotaFinalized, attempt.RolledBack)
	}
}

func TestResponsesWSActiveCloseSettlementFailureUsesQuotaSettlementFailedReason(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-active-close-settlement-fails",
		SelectedChannelID: 17,
		Quota:             &relay_util.Quota{},
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.close("provider_closed")

	if !actor.closing.closed.Load() {
		t.Fatal("expected active settlement failure during close to close session")
	}
	if attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected failed active close settlement not to mutate attempt, attempt=%+v", attempt)
	}
	if session.abortReason != "quota_settlement_failed" {
		t.Fatalf("expected active close settlement failure to abort with quota_settlement_failed, got %q", session.abortReason)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "quota_settlement_failed") {
		t.Fatalf("expected quota settlement failure payload, got %q", got)
	}
	if got, _ := conn.lastControl.Load().(string); !strings.Contains(got, "quota_settlement_failed") {
		t.Fatalf("expected downstream close control to use quota_settlement_failed, got %q", got)
	}
}

func TestResponsesWSActiveProviderCloseMissingAttemptIDFailsLoud(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(session, 17)
	attempt := &ResponsesWSTurnAttempt{
		SelectedChannelID: 17,
		Quota:             &relay_util.Quota{},
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderClosed(ResponsesWSEventProviderClosed{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Code:                      1000,
		Reason:                    "provider closed",
		DetailOrigin:              responsesws.RecvDetailOriginNativeProviderClose,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected missing attempt id provider close to close session")
	}
	if actor.turns.active.attempt != attempt || attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected missing attempt id failure to preserve active attempt, active=%v finalized=%v rolled=%v", actor.turns.active.attempt == attempt, attempt.QuotaFinalized, attempt.RolledBack)
	}
	if session.abortReason != "quota_settlement_failed" {
		t.Fatalf("expected provider close settlement failure to abort with quota_settlement_failed, got %q", session.abortReason)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "quota_settlement_failed") {
		t.Fatalf("expected quota settlement failure payload, got %q", got)
	}
}

func TestResponsesWSActiveSessionCloseMissingAttemptIDFailsLoud(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	attempt := &ResponsesWSTurnAttempt{
		SelectedChannelID: 17,
		Quota:             &relay_util.Quota{},
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.close("provider_closed")

	if !actor.closing.closed.Load() {
		t.Fatal("expected missing attempt id session close to close session")
	}
	if actor.turns.active.attempt != attempt || attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected missing attempt id close failure to preserve active attempt, active=%v finalized=%v rolled=%v", actor.turns.active.attempt == attempt, attempt.QuotaFinalized, attempt.RolledBack)
	}
	if session.abortReason != "quota_settlement_failed" {
		t.Fatalf("expected session close settlement failure to abort with quota_settlement_failed, got %q", session.abortReason)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "quota_settlement_failed") {
		t.Fatalf("expected quota settlement failure payload, got %q", got)
	}
	if got, _ := conn.lastControl.Load().(string); !strings.Contains(got, "quota_settlement_failed") {
		t.Fatalf("expected downstream close control to use quota_settlement_failed, got %q", got)
	}
}

func TestResponsesWSSettlementTraceUsesAttemptOpeningID(t *testing.T) {
	t.Run("subsequent turn does not inherit actor opening id", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		attempt.OpeningID = ""

		var traces []ResponsesWSSettlementTrace
		originalHook := responsesWSSettlementTraceHook
		responsesWSSettlementTraceHook = func(trace ResponsesWSSettlementTrace) {
			traces = append(traces, trace)
		}
		t.Cleanup(func() {
			responsesWSSettlementTraceHook = originalHook
		})

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.opening.openingID = "first-turn-opening-id"
		actor.turns.active.attempt = attempt
		_, _, err := actor.applyActiveSettlement(actor.buildActiveSettlementInput("subsequent_turn", ResponsesWSZeroChargeProof{}))
		if err != nil {
			t.Fatalf("expected subsequent turn settlement to succeed, got %v", err)
		}
		if len(traces) != 1 {
			t.Fatalf("expected one settlement trace, got %d", len(traces))
		}
		if traces[0].OpeningID != "" || traces[0].Input.OpeningID != "" {
			t.Fatalf("expected subsequent turn trace not to inherit actor opening id, trace=%+v", traces[0])
		}
	})

	t.Run("first turn uses attempt opening id", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		attempt.OpeningID = "current-attempt-opening-id"

		var traces []ResponsesWSSettlementTrace
		originalHook := responsesWSSettlementTraceHook
		responsesWSSettlementTraceHook = func(trace ResponsesWSSettlementTrace) {
			traces = append(traces, trace)
		}
		t.Cleanup(func() {
			responsesWSSettlementTraceHook = originalHook
		})

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.opening.openingID = "stale-actor-opening-id"
		actor.turns.active.attempt = attempt
		_, _, err := actor.applyActiveSettlement(actor.buildActiveSettlementInput("first_turn", ResponsesWSZeroChargeProof{}))
		if err != nil {
			t.Fatalf("expected first turn settlement to succeed, got %v", err)
		}
		if len(traces) != 1 {
			t.Fatalf("expected one settlement trace, got %d", len(traces))
		}
		if traces[0].OpeningID != "current-attempt-opening-id" || traces[0].Input.OpeningID != "current-attempt-opening-id" {
			t.Fatalf("expected trace opening id to come from attempt/input, trace=%+v", traces[0])
		}
	})
}

func TestResponsesWSSettlementTraceReplayAndAppliedEquality(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T, attempt *ResponsesWSTurnAttempt)
		evidence responsesws.ProviderSettlementLogProjection
		want     int64
	}{
		{
			name: "ambiguous no evidence floor",
			want: -1,
		},
		{
			name: "terminal exact below floor",
			setup: func(t *testing.T, attempt *ResponsesWSTurnAttempt) {
				response := &types.OpenAIResponsesResponses{
					ID:     "resp_trace_exact",
					Status: types.ResponseStatusCompleted,
					Usage:  &types.ResponsesUsage{InputTokens: 10, TotalTokens: 10},
				}
				mergeResponsesWSTerminalResponse(attempt.Usage, response)
				attempt.MarkProviderTerminalEvidence(responsesws.ClassifyResponsesWSTerminal("response.completed", response, false))
			},
			want: 10,
		},
		{
			name: "observed below floor",
			setup: func(t *testing.T, attempt *ResponsesWSTurnAttempt) {
				mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 10, TotalTokens: 10})
			},
			evidence: responsesWSTestProviderUsageProjection(),
			want:     -1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupResponsesWSQuotaFixture(t, 1000)
			configureResponsesWSTokenPricingFloor(t, 100)
			attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
			if tc.setup != nil {
				tc.setup(t, attempt)
			}
			want := tc.want
			if want < 0 {
				want = int64(attempt.Quota.PreConsumedQuota())
			}

			var traces []ResponsesWSSettlementTrace
			originalHook := responsesWSSettlementTraceHook
			responsesWSSettlementTraceHook = func(trace ResponsesWSSettlementTrace) {
				traces = append(traces, trace)
			}
			t.Cleanup(func() {
				responsesWSSettlementTraceHook = originalHook
			})

			actor := NewResponsesWSSessionActor(ctx)
			actor.turns.active.attempt = attempt
			actor.turns.active.evidence = tc.evidence
			_, applied, err := actor.applyActiveSettlement(actor.buildActiveSettlementInput(tc.name, ResponsesWSZeroChargeProof{}))
			if err != nil {
				t.Fatalf("expected traced settlement to succeed, got %v", err)
			}
			if applied.AppliedFinalQuota != want {
				t.Fatalf("expected applied quota %d, got %d", want, applied.AppliedFinalQuota)
			}
			if len(traces) != 1 {
				t.Fatalf("expected one settlement trace, got %d", len(traces))
			}
			trace := traces[0]
			if replayed := decideResponsesWSSettlement(trace.Input); replayed.DecisionKey != trace.Decision.DecisionKey {
				t.Fatalf("expected replay decision key %q, got %q", trace.Decision.DecisionKey, replayed.DecisionKey)
			}
			if trace.Decision.ExpectedFinalQuota != trace.Applied.AppliedFinalQuota {
				t.Fatalf("expected trace applied equality, decision=%+v applied=%+v", trace.Decision, trace.Applied)
			}
		})
	}
}

func TestResponsesWSTransportSendResultSettlementTraceTransportStatus(t *testing.T) {
	tests := []struct {
		name   string
		result responsesws.ResponsesWSTransportSendResult
		want   string
	}{
		{name: "not_attempted", result: responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendNotAttempted, Err: errors.New("not attempted")}, want: "not_attempted"},
		{name: "rejected_before_stream", result: responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendRejectedBeforeStream}, want: "rejected_before_stream"},
		{name: "ambiguous", result: responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAmbiguous, Err: errors.New("ambiguous write")}, want: "ambiguous"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-trace-"+tc.name)
			actor := NewResponsesWSSessionActor(ctx)
			actor.turns.pending.attempt = attempt
			actor.turns.pending.phase = responsesWSPendingTurnSend
			actor.state = responsesWSStatePendingSend

			var traces []ResponsesWSSettlementTrace
			originalHook := responsesWSSettlementTraceHook
			responsesWSSettlementTraceHook = func(trace ResponsesWSSettlementTrace) {
				traces = append(traces, trace)
			}
			t.Cleanup(func() {
				responsesWSSettlementTraceHook = originalHook
			})

			actor.handleSendResult(ResponsesWSEventSendResult{
				AttemptID:         attempt.AttemptID,
				SelectedChannelID: 17,
				Purpose:           ResponsesWSSendPurposeResponseCreate,
				TransportResult:   tc.result,
			})
			if responsesWSTransportSendStatus(tc.result) == responsesws.ResponsesWSTransportSendRejectedBeforeStream {
				actor.handleEvent(ResponsesWSEventBridgeOpenProviderError{
					AttemptID:   attempt.AttemptID,
					Payload:     responsesWSErrorPayload(http.StatusBadGateway, "provider_rejected_before_stream", "provider rejected stream open"),
					Recoverable: true,
				})
			}

			if len(traces) != 1 {
				t.Fatalf("expected one settlement trace, got %d", len(traces))
			}
			if got := traces[0].Input.Diagnostics.TransportStatus; got != tc.want {
				t.Fatalf("expected transport status %q, got %q trace=%+v", tc.want, got, traces[0])
			}
			if responsesWSTransportSendStatus(tc.result) == responsesws.ResponsesWSTransportSendRejectedBeforeStream &&
				!responsesWSTraceHasDetailOrigin(traces[0], responsesws.RecvDetailOriginBridgeOpenProviderError) {
				t.Fatalf("expected rejected_before_stream trace to include bridge_open_provider_error, trace=%+v", traces[0])
			}
		})
	}
}

func TestResponsesWSAmbiguousSendDoesNotRetryAndSettlesFloor(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":[]}`))
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	actor := NewResponsesWSSessionActor(ctx)
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	openingID := actor.ReserveFirstTurnOpening(frame)
	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:           ctx,
		OpeningID:         openingID,
		Admission:         NewResponsesWSTurnAdmission(),
		SelectedChannelID: 17,
		BillingModel:      "gpt-5",
		PromptModel:       "gpt-5",
		Request:           &frame.Projection,
	})
	if apiErr != nil {
		t.Fatalf("prepare attempt: %v", apiErr)
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		t.Fatalf("preconsume attempt: %v", apiErr)
	}
	attempt.AttemptID = "attempt-ambiguous-no-retry"
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	originalOpen := openAndPrimeResponsesWSSessionForActor
	var openCalls int32
	openAndPrimeResponsesWSSessionForActor = func(context.Context, *gin.Context, *responsesws.RawResponsesCreateFrame, *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
		atomic.AddInt32(&openCalls, 1)
		return &responsesWSOpenResult{Session: &responsesWSTestSession{}, BillingModel: "gpt-5"}, nil
	}
	t.Cleanup(func() {
		openAndPrimeResponsesWSSessionForActor = originalOpen
	})

	var trace ResponsesWSSettlementTrace
	originalHook := responsesWSSettlementTraceHook
	responsesWSSettlementTraceHook = func(got ResponsesWSSettlementTrace) {
		trace = got
	}
	t.Cleanup(func() {
		responsesWSSettlementTraceHook = originalHook
	})

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         attempt.AttemptID,
		SelectedChannelID: 17,
		Purpose:           ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("ambiguous write"),
		},
	})

	if got := atomic.LoadInt32(&openCalls); got != 0 {
		t.Fatalf("expected ambiguous send not to retry/open another channel, got %d opens", got)
	}
	if !attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected ambiguous no-retry path to settle floor, attempt=%+v", attempt)
	}
	if trace.Decision.Action != ResponsesWSSettlementFinalizeFloor || trace.Applied.AppliedFinalQuota != int64(attempt.Quota.PreConsumedQuota()) {
		t.Fatalf("expected trace floor settlement, trace=%+v", trace)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "ambiguous_close_no_provider_evidence") {
		t.Fatalf("expected proxy-local ambiguous error, got %q", got)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected ambiguous no-retry path to close session")
	}
}

func TestResponsesWSInvalidTransportResultSettlesFloorThenFailsClosed(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":[]}`))
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	actor := NewResponsesWSSessionActor(ctx)
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	openingID := actor.ReserveFirstTurnOpening(frame)
	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:           ctx,
		OpeningID:         openingID,
		Admission:         NewResponsesWSTurnAdmission(),
		SelectedChannelID: 17,
		BillingModel:      "gpt-5",
		PromptModel:       "gpt-5",
		Request:           &frame.Projection,
	})
	if apiErr != nil {
		t.Fatalf("prepare attempt: %v", apiErr)
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		t.Fatalf("preconsume attempt: %v", apiErr)
	}
	attempt.AttemptID = "attempt-invalid-transport-result"
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	var trace ResponsesWSSettlementTrace
	originalHook := responsesWSSettlementTraceHook
	responsesWSSettlementTraceHook = func(got ResponsesWSSettlementTrace) {
		trace = got
	}
	t.Cleanup(func() {
		responsesWSSettlementTraceHook = originalHook
	})

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         attempt.AttemptID,
		SelectedChannelID: 17,
		Purpose:           ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAttempted,
			Err:    errors.New("attempted cannot carry err"),
		},
	})

	if !attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected invalid transport result to settle floor, attempt=%+v", attempt)
	}
	if trace.Decision.Action != ResponsesWSSettlementFinalizeFloor ||
		trace.Input.ZeroChargeProof.Present() ||
		trace.Input.Diagnostics.TransportStatus != "invalid" {
		t.Fatalf("expected invalid transport settlement without zero proof, trace=%+v", trace)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_transport_contract_violation") ||
		strings.Contains(got, "ambiguous_close_no_provider_evidence") {
		t.Fatalf("expected contract violation payload, got %q", got)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected invalid transport result to fail closed")
	}
}

func TestResponsesWSPreConsumeForcesFloorForHighBalanceUser(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 200000)

	trustedQuota := relay_util.NewQuota(ctx, "gpt-5", 0)
	if apiErr := trustedQuota.PreQuotaConsumptionRollbackable(); apiErr != nil {
		t.Fatalf("expected ordinary trusted preconsume to succeed, got %v", apiErr)
	}
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 200000 || token.RemainQuota != 200000 || token.UsedQuota != 0 {
		t.Fatalf("expected ordinary quota path to skip high-balance reserve, user=%+v token=%+v", user, token)
	}

	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:           ctx,
		SelectedChannelID: 17,
		BillingModel:      "gpt-5",
		PromptModel:       "gpt-5",
		Request:           &types.OpenAIResponsesRequest{Model: "gpt-5", Input: []types.ChatCompletionMessage{}},
	})
	if apiErr != nil {
		t.Fatalf("expected attempt preparation to succeed, got %v", apiErr)
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		t.Fatalf("expected responses websocket forced preconsume to succeed, got %v", apiErr)
	}
	if !attempt.QuotaPreconsumed || !attempt.Quota.HasPreConsumedSideEffect() || attempt.Quota.PreConsumedQuota() <= 0 {
		t.Fatalf("expected responses websocket preconsume to force a floor, attempt=%+v floor=%d", attempt, attempt.Quota.PreConsumedQuota())
	}
	user, token = readResponsesWSQuotaFixture(t)
	floor := attempt.Quota.PreConsumedQuota()
	if user.Quota != 200000-floor || token.RemainQuota != 200000-floor || token.UsedQuota != floor {
		t.Fatalf("expected forced responses websocket reserve floor %d, user=%+v token=%+v", floor, user, token)
	}
}

func TestMergeResponsesWSResponsesUsagePreservesAccumulatedDetailsWhenTerminalOmitsFields(t *testing.T) {
	usage := &types.Usage{
		PromptTokens:     11,
		CompletionTokens: 7,
		TotalTokens:      18,
		PromptTokensDetails: types.PromptTokensDetails{
			AudioTokens:      9,
			CachedReadTokens: 4,
			TextTokens:       2,
		},
		CompletionTokensDetails: types.CompletionTokensDetails{
			ReasoningTokens: 3,
		},
	}

	mergeResponsesWSResponsesUsage(usage, &types.ResponsesUsage{
		InputTokens:  0,
		OutputTokens: 0,
		TotalTokens:  0,
		InputTokensDetails: &types.ResponsesUsageInputTokensDetails{
			AudioTokens: 0,
			TextTokens:  5,
		},
		OutputTokensDetails: &types.ResponsesUsageOutputTokensDetails{},
	})

	if usage.PromptTokens != 11 || usage.CompletionTokens != 7 || usage.TotalTokens != 18 {
		t.Fatalf("expected zero terminal totals to preserve accumulated totals, got %+v", usage)
	}
	if usage.PromptTokensDetails.AudioTokens != 9 || usage.PromptTokensDetails.CachedReadTokens != 4 || usage.PromptTokensDetails.TextTokens != 5 {
		t.Fatalf("expected positive detail fields to override without clearing omitted fields, got %+v", usage.PromptTokensDetails)
	}
	if usage.CompletionTokensDetails.ReasoningTokens != 3 {
		t.Fatalf("expected zero reasoning detail to preserve accumulated value, got %+v", usage.CompletionTokensDetails)
	}
}

func TestResponsesWSActorCloseActiveUsageEvidenceSettlesQuota(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6})

	actor := NewResponsesWSSessionActor(ctx)
	actor.turns.active.attempt = attempt
	actor.turns.active.evidence = responsesWSTestProviderUsageProjection()
	actor.state = responsesWSStateInFlight
	actor.close("client_closed")

	if attempt.RolledBack || !attempt.QuotaFinalized {
		t.Fatalf("expected active usage-evidence close to finalize without rollback, rolled=%v finalized=%v", attempt.RolledBack, attempt.QuotaFinalized)
	}
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 900 || user.UsedQuota != 100 || user.RequestCount != 1 {
		t.Fatalf("expected usage evidence to settle one turn, quota=%d used=%d requests=%d", user.Quota, user.UsedQuota, user.RequestCount)
	}
	if token.RemainQuota != 900 || token.UsedQuota != 100 {
		t.Fatalf("expected usage evidence to settle token quota, remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}

func TestResponsesWSSameSessionTwoMessagesAccumulateQuota(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.upstream.channelID = 17

	for i, responseID := range []string{"resp_one", "resp_two"} {
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		actor.turns.active.attempt = attempt
		actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
		actor.turns.active.channelID = 17
		actor.state = responsesWSStateInFlight
		actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
			AttemptID: responsesWSTestCurrentAttemptID(actor),
			ChannelID: 17,
			Kind:      ProviderDownstreamFrame,
			Frame: responsesWSTestProviderTextFrame([]byte(fmt.Sprintf(
				`{"type":"response.completed","response":{"id":%q,"status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
				responseID,
			))),
			DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		})
		if attempt.RolledBack || !attempt.QuotaFinalized {
			t.Fatalf("expected turn %d to finalize without rollback, rolled=%v finalized=%v", i+1, attempt.RolledBack, attempt.QuotaFinalized)
		}
		if actor.state != responsesWSStateIdle || actor.turns.active.attempt != nil {
			t.Fatalf("expected turn %d to leave actor idle, state=%v active=%+v", i+1, actor.state, actor.turns.active.attempt)
		}
	}

	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 800 || user.UsedQuota != 200 || user.RequestCount != 2 {
		t.Fatalf("expected two messages to settle as two turns, quota=%d used=%d requests=%d", user.Quota, user.UsedQuota, user.RequestCount)
	}
	if token.RemainQuota != 800 || token.UsedQuota != 200 {
		t.Fatalf("expected two messages to accumulate token quota, remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}

func TestResponsesWSTransportSendResultMismatchWithoutEvidenceIsDiagnosticOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-current",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         "attempt-stale",
		SelectedChannelID: 17,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAttempted,
		},
	})

	if attempt.RolledBack || actor.turns.pending.attempt != attempt {
		t.Fatalf("expected mismatch to leave pending attempt untouched, rolled=%v pending=%+v", attempt.RolledBack, actor.turns.pending.attempt)
	}
	if actor.closing.closed.Load() {
		t.Fatal("expected mismatch to stay diagnostic-only")
	}
}

func TestResponsesWSProviderEventAttemptMismatchDoesNotUpdateEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-current",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal = responsesWSProviderJournal{}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 "attempt-stale",
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.created","response":{"id":"resp_stale","status":"in_progress"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if actor.turns.pending.provider.journal.Project().HasActivity() || len(actor.turns.pending.provider.journal.DownstreamEvents()) != 0 {
		t.Fatalf("expected stale attempt provider event not to update evidence/buffer, evidence=%+v buffered=%d", actor.turns.pending.provider.journal.Project(), len(actor.turns.pending.provider.journal.DownstreamEvents()))
	}
	if actor.closing.closed.Load() || actor.turns.pending.attempt != attempt {
		t.Fatalf("expected stale attempt provider event to be diagnostic-only, closed=%v pending=%+v", actor.closing.closed.Load(), actor.turns.pending.attempt)
	}
}

func TestResponsesWSProviderDownstreamFinalizedResponseIDIgnoredForNewAttempt(t *testing.T) {
	ctx, firstAttempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-old")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = firstAttempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_old","status":"completed"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})
	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_old" {
		t.Fatalf("expected first terminal response to be finalized, got %+v", actor.turns.history.lastFinal)
	}

	nextAttempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-new",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = nextAttempt
	actor.turns.pending.provider.journal = responsesWSProviderJournal{}
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_old","status":"completed"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if nextAttempt.SeenProviderResponseID != "" || actor.turns.pending.provider.journal.Project().HasActivity() || len(actor.turns.pending.provider.journal.DownstreamEvents()) != 0 {
		t.Fatalf("expected finalized response id event to be ignored, attempt=%+v evidence=%+v buffered=%d", nextAttempt, actor.turns.pending.provider.journal.Project(), len(actor.turns.pending.provider.journal.DownstreamEvents()))
	}
	if actor.turns.pending.attempt != nextAttempt || actor.closing.closed.Load() {
		t.Fatalf("expected new pending attempt to remain open, pending=%+v closed=%v", actor.turns.pending.attempt, actor.closing.closed.Load())
	}
}

func TestResponsesWSProviderDownstreamResponseIDMismatchFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-current",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.created","response":{"id":"resp_current","status":"in_progress"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})
	if attempt.SeenProviderResponseID != "resp_current" || actor.closing.closed.Load() {
		t.Fatalf("expected first response id to be accepted, attempt=%+v closed=%v", attempt, actor.closing.closed.Load())
	}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.output_text.delta","response_id":"resp_other","delta":"mismatch"}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected response id mismatch to fail closed")
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected response id mismatch not to submit terminal side effect, got %+v", actor.turns.history.lastFinal)
	}
}

func TestResponsesWSTransportSendResultGenerationMismatchIsDiagnosticOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	actor.upstream.sessionGeneration = "generation-current"
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-current",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 "attempt-current",
		UpstreamSessionGeneration: "generation-stale",
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    responsesws.ErrUpstreamClosed,
		},
	})

	if attempt.RolledBack || actor.turns.pending.attempt != attempt {
		t.Fatalf("expected stale generation send result to leave pending attempt untouched, rolled=%v pending=%+v", attempt.RolledBack, actor.turns.pending.attempt)
	}
	if actor.closing.closed.Load() {
		t.Fatal("expected stale generation send result not to close actor")
	}
}

func TestResponsesWSNonCreateSendResultIsDiagnosticOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-current",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	for _, purpose := range []ResponsesWSSendPurpose{
		ResponsesWSSendPurposeResponseCancel,
		ResponsesWSSendPurposeControl,
		ResponsesWSSendPurposePingPong,
	} {
		actor.handleSendResult(ResponsesWSEventSendResult{
			AttemptID:         "attempt-current",
			SelectedChannelID: 17,
			Purpose:           purpose,
			TransportResult: responsesws.ResponsesWSTransportSendResult{
				Status: responsesws.ResponsesWSTransportSendNotAttempted,
				Err:    responsesws.ErrUpstreamClosed,
			},
		})
	}

	if attempt.RolledBack || actor.turns.pending.attempt != attempt {
		t.Fatalf("expected non-create send result to leave pending attempt untouched, rolled=%v pending=%+v", attempt.RolledBack, actor.turns.pending.attempt)
	}
	if actor.closing.closed.Load() {
		t.Fatal("expected non-create send result not to close actor")
	}
}

func TestResponsesWSUpstreamEventConversionPreservesTypedRouting(t *testing.T) {
	frameEvent := upstreamEventFromProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 "attempt-conversion",
		UpstreamSessionGeneration: "generation-a",
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.created"}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderStream,
		DetailPhase:               responsesws.RecvDetailPhaseHandleProviderFrame,
	})
	if frameEvent.Frame == nil || frameEvent.DetailOrigin != responsesws.RecvDetailOriginProviderStream || frameEvent.DetailPhase != responsesws.RecvDetailPhaseHandleProviderFrame || responsesws.PayloadOriginForDetailOrigin(frameEvent.DetailOrigin) != responsesws.PayloadOriginProvider {
		t.Fatalf("expected downstream conversion to preserve typed provider routing, got %+v", frameEvent)
	}

	usageEvent := upstreamEventFromProviderUsage(ResponsesWSEventProviderUsageObserved{
		AttemptID:                 "attempt-conversion",
		UpstreamSessionGeneration: "generation-a",
		ChannelID:                 17,
		Usage:                     &types.UsageEvent{TotalTokens: 3},
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
		DetailPhase:               responsesws.RecvDetailPhaseHandleProviderFrame,
	})
	if usageEvent.Usage == nil || usageEvent.DetailOrigin != responsesws.RecvDetailOriginProviderFrame || usageEvent.DetailPhase != responsesws.RecvDetailPhaseHandleProviderFrame || responsesws.PayloadOriginForDetailOrigin(usageEvent.DetailOrigin) != responsesws.PayloadOriginProvider {
		t.Fatalf("expected usage conversion to preserve typed provider routing, got %+v", usageEvent)
	}

	closeEvent := upstreamEventFromProviderClosed(ResponsesWSEventProviderClosed{
		UpstreamSessionGeneration: "generation-a",
		ChannelID:                 17,
		Code:                      int(wsconn.CloseNormalClosure),
		Reason:                    "done",
		DetailOrigin:              responsesws.RecvDetailOriginNativeProviderClose,
		DetailPhase:               responsesws.RecvDetailPhaseMapProviderClose,
	})
	if closeEvent.ProviderClose == nil || closeEvent.DetailOrigin != responsesws.RecvDetailOriginNativeProviderClose || closeEvent.DetailPhase != responsesws.RecvDetailPhaseMapProviderClose || responsesws.PayloadOriginForDetailOrigin(closeEvent.DetailOrigin) != responsesws.PayloadOriginProvider {
		t.Fatalf("expected close conversion to preserve typed provider routing, got %+v", closeEvent)
	}
}

func TestResponsesWSMalformedProviderFramePreservesPreConsumedFloor(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-malformed")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if attempt.RolledBack {
		t.Fatalf("expected malformed provider frame on active turn to preserve quota floor")
	}
	if !attempt.QuotaFinalized {
		t.Fatalf("expected malformed provider frame on active turn to finalize quota floor")
	}
	if !actor.closing.closed.Load() {
		t.Fatalf("expected malformed provider frame to close session")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_provider_protocol_error") {
		t.Fatalf("expected provider protocol error payload, got %q", got)
	}
}

func TestResponsesWSProviderMalformedRecvFailureEmitsProtocolErrorPayload(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-provider-malformed")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Err:                       errors.New("provider frame parse failed"),
		DetailOrigin:              responsesws.RecvDetailOriginProviderMalformed,
		DetailPhase:               responsesws.RecvDetailPhaseHandleProviderFrame,
	})

	if attempt.RolledBack {
		t.Fatalf("expected provider malformed recv failure to preserve quota floor")
	}
	if !attempt.QuotaFinalized {
		t.Fatalf("expected provider malformed recv failure to finalize active quota floor")
	}
	if actor.turns.active.attempt != nil || !actor.turns.active.evidence.IsZero() || actor.state != responsesWSStateClosed {
		t.Fatalf("expected malformed recv failure to clear active turn and close, active=%+v evidence=%+v state=%v", actor.turns.active.attempt, actor.turns.active.evidence, actor.state)
	}
	payload, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, payload, http.StatusBadGateway, "responses_ws_provider_protocol_error", "malformed responses websocket frame")
}

func TestResponsesWSProviderMalformedRecvFailureClosesIdleSessionWithPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.state = responsesWSStateIdle

	actor.handleProviderRecvFailed(ResponsesWSEventProviderRecvFailed{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Err:                       errors.New("provider frame parse failed"),
		DetailOrigin:              responsesws.RecvDetailOriginProviderMalformed,
		DetailPhase:               responsesws.RecvDetailPhaseHandleProviderFrame,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected idle provider malformed recv failure to close session")
	}
	payload, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, payload, http.StatusBadGateway, "responses_ws_provider_protocol_error", "malformed responses websocket frame")
}

func TestResponsesWSTurnAttemptRollbackRestoresQuotaSynchronously(t *testing.T) {
	gin.SetMode(gin.TestMode)

	setupRelayTestDB(t, &model.User{}, &model.Token{})

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"gpt-5": {
				Model: "gpt-5",
				Type:  model.TimesPriceType,
				Input: 0.1,
			},
		},
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	originalBatchUpdate := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	t.Cleanup(func() {
		config.BatchUpdateEnabled = originalBatchUpdate
		config.RedisEnabled = originalRedisEnabled
	})

	if err := model.DB.Create(&model.User{
		Id:          1,
		Username:    "alice",
		Password:    "password123",
		AccessToken: "access-token-1",
		Quota:       1000,
		Group:       "default",
		Status:      config.UserStatusEnabled,
		Role:        config.RoleCommonUser,
		DisplayName: "Alice",
		CreatedTime: 1,
	}).Error; err != nil {
		t.Fatalf("expected user fixture to persist, got %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id:          1,
		UserId:      1,
		Key:         "token-key-1",
		Name:        "token-alpha",
		RemainQuota: 1000,
		Group:       "default",
	}).Error; err != nil {
		t.Fatalf("expected token fixture to persist, got %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("token_group", "default")
	ctx.Set("group_ratio", 1.0)

	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:      ctx,
		BillingModel: "gpt-5",
		PromptModel:  "gpt-5",
		Request:      &types.OpenAIResponsesRequest{Model: "gpt-5", Input: []types.ChatCompletionMessage{}},
	})
	if apiErr != nil {
		t.Fatalf("expected attempt preparation to succeed, got %v", apiErr)
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		t.Fatalf("expected quota preconsume to succeed, got %v", apiErr)
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup after preconsume to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup after preconsume to succeed, got %v", err)
	}
	if user.Quota != 900 || token.RemainQuota != 900 || token.UsedQuota != 100 {
		t.Fatalf("expected preconsume to reserve 100 quota, user=%d token_remain=%d token_used=%d", user.Quota, token.RemainQuota, token.UsedQuota)
	}

	if err := attempt.RollbackBeforeLocalWriteOK("test_sync_rollback"); err != nil {
		t.Fatalf("expected synchronous rollback to succeed, got %v", err)
	}
	if !attempt.RolledBack || attempt.QuotaPreconsumed {
		t.Fatalf("expected attempt rollback flags to be updated, rolled_back=%v preconsumed=%v", attempt.RolledBack, attempt.QuotaPreconsumed)
	}
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup after rollback to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup after rollback to succeed, got %v", err)
	}
	if user.Quota != 1000 || token.RemainQuota != 1000 || token.UsedQuota != 0 {
		t.Fatalf("expected rollback to restore quota before returning, user=%d token_remain=%d token_used=%d", user.Quota, token.RemainQuota, token.UsedQuota)
	}
}
