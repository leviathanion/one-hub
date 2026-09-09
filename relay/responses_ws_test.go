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
	"one-api/common/groupctx"
	ratelimit "one-api/common/limit"
	"one-api/common/logger"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/internal/testutil/sqlitetest"
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

const responsesWSTestSelfHostedOther = `{"responses_ws_native":true,"responses_ws_self_hosted":true,"responses_stored_lifecycle":true}`

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

func NewResponsesWSIOPump(conn *responsesWSFakeUserConn, actor *ResponsesWSSessionActor) *ResponsesWSIOPump {
	if conn == nil {
		return newResponsesWSPumpForTest(nil, actor)
	}
	return newResponsesWSPumpForTest(conn, actor)
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

func (j *responsesWSProviderJournal) appendDownstreamFixture(event ResponsesWSEventProviderDownstream) {
	j.AppendDownstream(event, upstreamEventFromProviderDownstream(event), 1<<30)
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

	session, apiErr := openResponsesWSUpstreamWithFrame(context.Background(), nil, provider, "gpt-5", responsesWSUpstreamOpenParams{}, responsesWSTestOpenFrame(t))
	if session != nil {
		t.Fatalf("expected no session from provider without OpenResponsesWS support, got %T", session)
	}
	if apiErr == nil || apiErr.StatusCode != http.StatusUpgradeRequired || openAIErrorCodeString(apiErr.Code, "") != "responses_ws_unsupported_for_channel" {
		t.Fatalf("expected unsupported ResponsesWS provider error, got %+v", apiErr)
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

func TestResponsesWSRejectsStreamIDBeforeChannelSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","stream_id":"lane-a","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse response.create: %v", err)
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	result, apiErr := openAndPrimeResponsesWSSessionWithContextAndFrame(context.Background(), ctx, frame, &frame.Projection)
	if result != nil || apiErr == nil || apiErr.Param != "stream_id" || openAIErrorCodeString(apiErr.Code, "") != unsupportedCapabilityCode {
		t.Fatalf("stream_id must fail before provider selection, result=%+v err=%+v", result, apiErr)
	}
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

type responsesWSSendResultTestSession struct {
	result      responsesws.ResponsesWSTransportSendResult
	resultCalls int32
}

func TestResponsesWSSendWorkerSkipsProviderAfterClientClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	actor := NewResponsesWSSessionActor(ctx)
	actor.markClientClosed(errors.New("client closed"))
	session := &responsesWSSendResultTestSession{
		result: responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAttempted},
	}

	result := actor.sendResultForCommand(context.Background(), responsesWSSendCommand{
		AttemptID: "attempt-client-closed",
		Session:   session,
		Frame:     responsesws.NewTextFrame([]byte(`{"type":"response.create"}`)),
	})

	if result.Status != responsesws.ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("expected a not-attempted result after client close, got %+v", result)
	}
	if calls := atomic.LoadInt32(&session.resultCalls); calls != 0 {
		t.Fatalf("expected no provider send after client close, got %d calls", calls)
	}
}

func TestResponsesWSClientCloseCallbackMarksTransportBeforeActorEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	actor := NewResponsesWSSessionActor(ctx)

	actor.onClientConnClosed(wsconn.CloseInfo{Err: io.EOF})

	if !actor.closing.clientClosed.Load() {
		t.Fatal("client close callback must publish the transport fact before actor event delivery")
	}
	result := actor.sendResultForCommand(context.Background(), responsesWSSendCommand{})
	if result.Status != responsesws.ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("provider work must be rejected immediately after the close callback, got %+v", result)
	}
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
	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}, &model.Channel{}, &model.ResponseOwner{}, &model.Price{}, &model.ModelInfo{}, &model.UserGroup{}, &model.PublicationVersion{}); err != nil {
		t.Fatalf("expected quota settlement schema migration to succeed, got %v", err)
	}
	if err := model.EnsurePublicationVersionRows(testDB); err != nil {
		t.Fatal(err)
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
	if err := model.DB.Create(&model.Price{Model: "gpt-5", Type: model.TimesPriceType, Input: 0.1}).Error; err != nil {
		t.Fatalf("expected SQL price fixture to persist, got %v", err)
	}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatalf("expected SQL price publication, got %v", err)
	}
	enabled := true
	if err := model.DB.Create(&model.UserGroup{Symbol: "default", Name: "Default", Ratio: 1, Enable: &enabled}).Error; err != nil {
		t.Fatalf("expected SQL group fixture to persist, got %v", err)
	}
	model.GlobalUserGroupRatio.Lock()
	originalGroups := model.GlobalUserGroupRatio.UserGroup
	groups := make(map[string]*model.UserGroup, len(originalGroups)+1)
	for key, value := range originalGroups {
		groups[key] = value
	}
	groups["default"] = &model.UserGroup{Symbol: "default", Name: "Default", Ratio: 1, Enable: &enabled}
	model.GlobalUserGroupRatio.UserGroup = groups
	model.GlobalUserGroupRatio.Unlock()
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.UserGroup = originalGroups
		model.GlobalUserGroupRatio.Unlock()
	})

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
		Request:           &types.OpenAIResponsesRequest{Model: "gpt-5", Input: []types.ChatCompletionMessage{}, MaxOutputTokens: config.PreConsumedQuota},
	})
	if apiErr != nil {
		t.Fatalf("expected attempt preparation to succeed, got %v", apiErr)
	}
	if apiErr := attempt.PreConsumeQuota(); apiErr != nil {
		t.Fatalf("expected quota preconsume to succeed, got %v", apiErr)
	}
	if apiErr := attempt.ClaimSubmission(); apiErr != nil {
		t.Fatalf("expected submission claim to succeed, got %v", apiErr)
	}
	attempt.AttemptID = "attempt-" + strings.ReplaceAll(t.Name(), "/", "_")
	return attempt
}

func TestPrepareResponsesWSTurnAttemptDoesNotSeedRequestedServiceTier(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:      ctx,
		BillingModel: "gpt-5",
		PromptModel:  "gpt-5",
		Request: &types.OpenAIResponsesRequest{
			Model:       "gpt-5",
			ServiceTier: "priority",
		},
	})
	if apiErr != nil {
		t.Fatalf("prepare attempt: %v", apiErr)
	}
	if attempt.Usage == nil || attempt.Usage.ServiceTier != "" {
		t.Fatalf("request tier must not seed settlement usage: %+v", attempt.Usage)
	}
}

func TestPrepareResponsesWSTurnAttemptCarriesMultiAgentAdmission(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	attempt, apiErr := PrepareResponsesWSTurnAttempt(ResponsesWSTurnAttemptInput{
		Context:           ctx,
		BillingModel:      "gpt-5",
		PromptModel:       "gpt-5",
		Request:           &types.OpenAIResponsesRequest{Model: "gpt-5"},
		MultiAgentEnabled: true,
	})
	if apiErr != nil {
		t.Fatalf("real multi-agent admission was rejected: %+v", apiErr)
	}
	if !attempt.MultiAgentEnabled || attempt.Billing == nil {
		t.Fatalf("multi-agent attempt lost billing/feature state: %+v", attempt)
	}
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

func TestResponsesWSSelectedChannelFactsAttachAndClear(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("original_model", "gpt-5")
	ctx.Set("billing_original_model", true)
	snapshot := NewResponsesWSRequestSnapshot(ctx)

	channel := &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, PreCost: 42}
	attachResponsesWSSelectedChannelFacts(snapshot, channel, "gpt-5-upstream")
	attached := snapshot.Context()
	if attached.GetInt("channel_id") != 17 || attached.GetInt("channel_type") != config.ChannelTypeOpenAI {
		t.Fatalf("expected selected channel ids in snapshot, got channel_id=%d type=%d", attached.GetInt("channel_id"), attached.GetInt("channel_type"))
	}
	if attached.GetString("new_model") != "gpt-5-upstream" || !attached.GetBool("billing_original_model") {
		t.Fatalf("expected selected model billing state in snapshot, new_model=%q billing_original=%v", attached.GetString("new_model"), attached.GetBool("billing_original_model"))
	}
	selected, ok := attached.Get("responses_ws_selected_channel")
	if !ok || selected != channel {
		t.Fatalf("expected actual selected channel fact with pre-cost, got %#v", selected)
	}

	clearResponsesWSSelectedChannelFacts(snapshot)
	cleared := snapshot.Context()
	for _, key := range []string{"responses_ws_selected_channel", "channel_id", "channel_type", "new_model", "billing_original_model"} {
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
	openAndPrimeResponsesWSSessionForActor = func(context.Context, *gin.Context, *responsesws.RawResponsesCreateFrame, *types.OpenAIResponsesRequest, responsesWSOpenAdmission) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
		atomic.AddInt32(&openCalls, 1)
		return nil, common.StringErrorWrapperLocal("unexpected open", "unexpected_open", http.StatusInternalServerError)
	}
	t.Cleanup(func() {
		openAndPrimeResponsesWSSessionForActor = originalOpen
	})

	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(nil, actor))
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

func TestResponsesWSProviderPayloadPreservesExactNativeCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("channel_type", config.ChannelTypeCodex)

	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","event_id":"evt_codex","model":"gpt-5","input":"hi","generate":true,"unknown_number":12345678901234567890}`))
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	request := frame.Projection

	payload, err := responsesWSProviderPayload(ctx, frame, &request, "gpt-5")
	if err != nil {
		t.Fatalf("build provider payload: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decode provider payload: %v", err)
	}
	if string(got["model"]) != `"gpt-5"` {
		t.Fatalf("expected exact top-level model, got %s", got["model"])
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
	if _, err := responsesWSProviderPayload(ctx, frame, &request, "gpt-5-mini"); err == nil {
		t.Fatal("expected native Responses websocket model rewrite to be rejected")
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
	openAndPrimeResponsesWSSessionForActor = func(context.Context, *gin.Context, *responsesws.RawResponsesCreateFrame, *types.OpenAIResponsesRequest, responsesWSOpenAdmission) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
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
	bridge := NewResponsesWSIOPump(conn, actor)
	actor.SetPump(bridge)
	t.Cleanup(func() {
		actor.finish()
		bridge.Close()
	})
	actor.ReserveFirstTurnOpening(frame)

	eventSnapshot := NewResponsesWSRequestSnapshot(ctx)
	eventSnapshot.Set("snapshot_marker", []string{"event"})
	adoptedCh := make(chan bool, 1)
	selectedChannel := &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Name: "openai", Models: "gpt-5", Group: "default"}
	actor.handleFirstTurnOpenResult(ResponsesWSEventFirstTurnOpenResult{
		OpeningID: actor.turns.opening.openingID,
		Snapshot:  eventSnapshot,
		OpenResult: &responsesWSOpenResult{
			Session:       session,
			ProviderModel: "gpt-5",
			BillingModel:  "gpt-5",
			Channel:       selectedChannel,
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
	rawSelected, ok := attached.Get("responses_ws_selected_channel")
	selected, okSelected := rawSelected.(*model.Channel)
	if !ok || !okSelected || selected != selectedChannel || selected.Id != 17 || attached.GetString("new_model") != "gpt-5" {
		t.Fatalf("expected selected channel facts to be attached, got channel=%#v model=%q", rawSelected, attached.GetString("new_model"))
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
			actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
	openAndPrimeResponsesWSSessionForActor = func(_ context.Context, openContext *gin.Context, _ *responsesws.RawResponsesCreateFrame, _ *types.OpenAIResponsesRequest, admit responsesWSOpenAdmission) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
		if _, apiErr := admit(openContext); apiErr != nil {
			return nil, apiErr
		}
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
	startResponsesWSTestActor(t, actor)
	if !actor.PostReliable(ResponsesWSEventFirstTurnSetup{
		Frame:        frame,
		PendingLease: &responsesWSTestLease{},
	}) {
		t.Fatal("expected first-turn setup event to be queued")
	}

	select {
	case <-openCalled:
		t.Fatal("expected RPM rejection before upstream open")
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case <-actor.Done():
	case <-time.After(time.Second):
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
	originalOpen := openAndPrimeResponsesWSSessionForActor
	openAndPrimeResponsesWSSessionForActor = func(_ context.Context, openContext *gin.Context, _ *responsesws.RawResponsesCreateFrame, _ *types.OpenAIResponsesRequest, admit responsesWSOpenAdmission) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
		if _, apiErr := admit(openContext); apiErr != nil {
			return nil, apiErr
		}
		return nil, common.StringErrorWrapperLocal("unexpected open", "unexpected_open", http.StatusInternalServerError)
	}
	t.Cleanup(func() {
		openAndPrimeResponsesWSSessionForActor = originalOpen
	})

	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(&responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}, actor))
	startResponsesWSTestActor(t, actor)
	if !actor.PostReliable(ResponsesWSEventFirstTurnSetup{
		Frame:        frame,
		PendingLease: &responsesWSTestLease{},
	}) {
		t.Fatal("expected first-turn setup event to be queued")
	}
	select {
	case <-actor.Done():
	case <-time.After(time.Second):
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

func TestResponsesWSClosureCutReducesQueuedTerminalBeforeSettlement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-closure-cut")
	actor := NewResponsesWSSessionActor(ctx)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.upstream.channelID = 17
	actor.state = responsesWSStateInFlight

	if !actor.PostReliable(ResponsesWSEventClientClosed{Err: io.EOF}) {
		t.Fatal("expected client close to enter actor mailbox")
	}
	terminal := responsesws.NewTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_closure_cut","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`))
	if !actor.PostReliable(ResponsesWSEventProviderDownstream{
		AttemptID:    attempt.AttemptID,
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        &terminal,
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   time.Now(),
	}) {
		t.Fatal("expected provider terminal to enter actor mailbox behind close")
	}

	startResponsesWSTestActor(t, actor)
	select {
	case <-actor.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for closure-cut settlement")
	}
	actor.waitStartedGoroutines()

	if !attempt.QuotaFinalized || attempt.RolledBack || attempt.Usage.PromptTokens != 3 || attempt.Usage.CompletionTokens != 2 || attempt.Usage.TotalTokens != 5 {
		t.Fatalf("queued terminal was not reduced before close settlement: attempt=%+v usage=%+v", attempt, attempt.Usage)
	}
	if actor.turns.history.lastFinal == nil || actor.turns.history.lastFinal.ID != "resp_closure_cut" {
		t.Fatalf("expected queued terminal lifecycle side effects, got %+v", actor.turns.history.lastFinal)
	}
	if got := actor.closing.closureCutSequence; got != 2 {
		t.Fatalf("expected closure cut to include both accepted mailbox events, got sequence %d", got)
	}
	if got := actor.eventBytes.Load(); got != 0 {
		t.Fatalf("closure cut leaked actor event bytes: %d", got)
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
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)
	ctx.Set("group", "default")
	ctx.Set("group_ratio", 1.0)

	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatalf("parse first frame: %v", err)
	}

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
	owner, ownerErr := model.NewResponseOwner(responseID, ctx.GetInt("id"), ctx.GetInt("token_id"), ownerChannelID, time.Now())
	if ownerErr != nil {
		t.Fatalf("create durable response owner: %v", ownerErr)
	}
	if ownerErr = model.CreateResponseOwner(ctx.Request.Context(), owner); ownerErr != nil {
		t.Fatalf("persist durable response owner: %v", ownerErr)
	}
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

func TestRecordResponsesTurnSuccessUsesCommittedTurnState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 60,
		Rules: []config.ChannelAffinityRule{
			{
				Name:            "responses-prompt-per-turn",
				Enabled:         true,
				Kind:            "responses",
				IncludeModel:    true,
				IncludeRuleName: true,
				RecordOnSuccess: true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "request_field", Key: "prompt_cache_key", Alias: config.ChannelAffinityAliasPromptCacheKey},
					{Source: "request_field", Key: "previous_response_id", Alias: config.ChannelAffinityAliasResponseID},
				},
			},
		},
	}
	settings.Normalize()
	manager := withChannelAffinitySettings(t, settings)

	newContext := func() *gin.Context {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		ctx.Set("token_group", "default")
		return ctx
	}
	staleContext := newContext()
	staleCandidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{
		Context: staleContext,
		Request: &types.OpenAIResponsesRequest{Model: "gpt-4", PromptCacheKey: "prompt-old"},
	})
	if err != nil || staleCandidate.State == nil || len(staleCandidate.State.RequestBindings) != 1 {
		t.Fatalf("prepare stale affinity: candidate=%+v err=%v", staleCandidate, err)
	}
	turnContext := newContext()
	turnCandidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{
		Context: turnContext,
		Request: &types.OpenAIResponsesRequest{Model: "gpt-5", PromptCacheKey: "prompt-new"},
	})
	if err != nil || turnCandidate.State == nil || len(turnCandidate.State.RequestBindings) != 1 {
		t.Fatalf("prepare current turn affinity: candidate=%+v err=%v", turnCandidate, err)
	}
	oldKey := staleCandidate.State.RequestBindings[0].Key
	newKey := turnCandidate.State.RequestBindings[0].Key
	if len(staleCandidate.State.DerivedRecorders[config.ChannelAffinityAliasResponseID]) != 1 || len(turnCandidate.State.DerivedRecorders[config.ChannelAffinityAliasResponseID]) != 1 {
		t.Fatalf("response id recorders missing: stale=%+v current=%+v", staleCandidate.State.DerivedRecorders, turnCandidate.State.DerivedRecorders)
	}
	oldResponseKey := staleCandidate.State.DerivedRecorders[config.ChannelAffinityAliasResponseID][0].BuildKey("resp-new")
	newResponseKey := turnCandidate.State.DerivedRecorders[config.ChannelAffinityAliasResponseID][0].BuildKey("resp-new")
	if oldKey == newKey {
		t.Fatalf("test requires distinct turn keys, got %q", oldKey)
	}
	if oldResponseKey == newResponseKey {
		t.Fatalf("test requires model-scoped response keys, got %q", oldResponseKey)
	}

	RecordResponsesTurnSuccess(staleContext, CommitResponsesTurnAffinity(turnCandidate, 17), &types.OpenAIResponsesResponses{ID: "resp-new"})
	if record, ok := manager.Get(newKey); !ok || record.ChannelID != 17 {
		t.Fatalf("current turn key was not recorded on channel 17: record=%+v ok=%v", record, ok)
	}
	if record, ok := manager.Get(oldKey); ok {
		t.Fatalf("stale actor context key was recorded: %+v", record)
	}
	if record, ok := manager.Get(newResponseKey); !ok || record.ChannelID != 17 {
		t.Fatalf("current turn response id was not recorded on channel 17: record=%+v ok=%v", record, ok)
	}
	if record, ok := manager.Get(oldResponseKey); ok {
		t.Fatalf("response id was recorded with stale turn model/template: %+v", record)
	}
}

func installResponsesWSReplayOpenProbe(t *testing.T) (chan struct{}, *int32) {
	t.Helper()
	started := make(chan struct{})
	var calls int32
	originalOpen := openAndPrimeResponsesWSSessionForActor
	openAndPrimeResponsesWSSessionForActor = func(ctx context.Context, _ *gin.Context, _ *responsesws.RawResponsesCreateFrame, _ *types.OpenAIResponsesRequest, _ responsesWSOpenAdmission) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
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

func TestResponsesWSAttemptScopedProxyLocalCommitsExplicitAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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

func TestResponsesWSNonTextFrameReturnsErrorAndClosesWithUnsupportedData(t *testing.T) {
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
	if !actor.closing.closed.Load() {
		t.Fatal("expected a non-text client frame to close the session")
	}
	control, _ := conn.lastControl.Load().(string)
	code, reason := parseResponsesWSClosePayload([]byte(control))
	if code != int(wsconn.CloseUnsupportedData) || reason != "text_only" {
		t.Fatalf("expected websocket close 1003/text_only, got code=%d reason=%q", code, reason)
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

func TestOpenAndPrimeResponsesWSOwnerPinnedFirstTurnRequiresModelAdmissionBeforeUpstreamOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name           string
		models         string
		disabledStream bool
		tokenSetting   *model.TokenSetting
		wantStatus     int
		wantCode       string
	}{
		{
			name:   "令牌模型白名单",
			models: "gpt-5",
			tokenSetting: &model.TokenSetting{Limits: model.LimitsConfig{
				LimitModelSetting: model.LimitModelSetting{Enabled: true, Models: []string{"gpt-4"}},
			}},
			wantStatus: http.StatusNotFound,
			wantCode:   "model_not_found",
		},
		{
			name:           "渠道禁用流式",
			models:         "gpt-5",
			disabledStream: true,
			wantStatus:     http.StatusUpgradeRequired,
			wantCode:       "responses_ws_unsupported_for_channel",
		},
		{
			name:       "渠道不支持精确模型",
			models:     "gpt-4",
			wantStatus: http.StatusConflict,
			wantCode:   "responses_ws_model_unsupported_by_channel",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setupRelayTestDB(t, &model.Channel{}, &model.ResponseOwner{})

			var upstreamCalls int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&upstreamCalls, 1)
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer upstream.Close()

			baseURL := upstream.URL
			proxy := ""
			channel := &model.Channel{
				Id:      71,
				Type:    config.ChannelTypeOpenAI,
				Name:    "owner-pinned-responses-ws",
				Key:     "sk-test",
				BaseURL: &baseURL,
				Proxy:   &proxy,
				Status:  config.ChannelStatusEnabled,
				Group:   "default",
				Models:  test.models,
				Other:   responsesWSTestSelfHostedOther,
			}
			if test.disabledStream {
				disabled := datatypes.JSONSlice[string]{"gpt-5"}
				channel.DisabledStream = &disabled
			}
			if err := model.DB.Create(channel).Error; err != nil {
				t.Fatalf("persist owner-pinned channel: %v", err)
			}

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			ctx.Set("id", 1)
			ctx.Set("token_id", 1)
			ctx.Set("group", "default")
			ctx.Set("token_group", "default")
			if test.tokenSetting != nil {
				ctx.Set("token_setting", test.tokenSetting)
			}
			owner, err := model.NewResponseOwner("resp_owner_pinned", 1, 1, channel.Id, time.Now())
			if err != nil {
				t.Fatalf("create stored response owner: %v", err)
			}
			if err := model.CreateResponseOwner(ctx.Request.Context(), owner); err != nil {
				t.Fatalf("persist stored response owner: %v", err)
			}

			frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","previous_response_id":"resp_owner_pinned","store":false,"input":"hi"}`))
			if err != nil {
				t.Fatalf("parse owner-pinned first frame: %v", err)
			}
			result, apiErr := openAndPrimeResponsesWSSessionWithContextAndFrame(context.Background(), ctx, frame, &frame.Projection)

			if result != nil || apiErr == nil || apiErr.StatusCode != test.wantStatus || openAIErrorCodeString(apiErr.Code, "") != test.wantCode {
				t.Fatalf("owner-pinned first turn result=%#v err=%+v", result, apiErr)
			}
			if calls := atomic.LoadInt32(&upstreamCalls); calls != 0 {
				t.Fatalf("expected admission failure before upstream open, got %d upstream calls", calls)
			}
		})
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
	preferred := &model.Channel{
		Id:      preferredChannelID,
		Type:    config.ChannelTypeOpenAI,
		Key:     "sk-preferred",
		BaseURL: &preferredBaseURL,
		Proxy:   &proxy,
		Other:   responsesWSTestSelfHostedOther,
	}
	fallback := &model.Channel{
		Id:      fallbackChannelID,
		Type:    config.ChannelTypeOpenAI,
		Key:     "sk-fallback",
		BaseURL: &fallbackBaseURL,
		Proxy:   &proxy,
		Other:   responsesWSTestSelfHostedOther,
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

	openResult, apiErr := openAndPrimeResponsesWSSessionWithContextAndFrameAdmissionAndBudget(context.Background(), ctx, nil, request, nil, 2)
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

func TestOpenAndPrimeResponsesWSAdmitsFinalBackupGroupBeforeUpstreamOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})
	originalUserGroups := model.GlobalUserGroupRatio.UserGroup
	model.GlobalUserGroupRatio.UserGroup = map[string]*model.UserGroup{
		"backup": {Symbol: "backup", Ratio: 1},
	}
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.UserGroup = originalUserGroups
	})

	upgraded := make(chan *wsconn.ManagedConn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsconn.AcceptManaged(w, r, wsconn.Config{Label: "responses ws capacity group accept"}, wsconn.AcceptOptions{
			CheckOrigin: func(*http.Request) bool { return true },
		})
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		upgraded <- conn
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	weight := uint(1)
	proxy := ""
	baseURL := server.URL
	channel := &model.Channel{
		Id: 301, Type: config.ChannelTypeOpenAI, Key: "sk-test", BaseURL: &baseURL, Proxy: &proxy,
		Status: config.ChannelStatusEnabled, Group: "backup", Models: "gpt-5", Weight: &weight,
		Other: responsesWSTestSelfHostedOther,
	}
	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{channel.Id: {Channel: channel}},
		Rule: map[string]map[string][][]int{
			"backup": {"gpt-5": {{channel.Id}}},
		},
		ModelGroup: map[string]map[string]bool{
			"gpt-5": {"backup": true},
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "primary")
	ctx.Set("token_backup_group", "backup")
	ctx.Set("id", 7)
	ctx.Set("token_id", 101)

	store := false
	request := &types.OpenAIResponsesRequest{Model: "gpt-5", Store: &store, Input: "hi"}
	lease := &responsesWSTestLease{}
	admittedGroup := ""
	openResult, apiErr := openAndPrimeResponsesWSSessionWithContextAndFrameAndAdmission(
		context.Background(), ctx, responsesWSTestOpenFrame(t), request,
		func(openContext *gin.Context) (middleware.ResponsesWSLease, *types.OpenAIErrorWithStatusCode) {
			admittedGroup = groupctx.CurrentRoutingGroup(openContext)
			return lease, nil
		},
	)
	if apiErr != nil || openResult == nil || openResult.Channel == nil || openResult.Channel.Id != channel.Id {
		t.Fatalf("expected backup channel to open, result=%#v err=%+v", openResult, apiErr)
	}
	if admittedGroup != "backup" || openResult.ActiveLease != lease {
		t.Fatalf("active capacity used group %q instead of final backup group, lease=%T", admittedGroup, openResult.ActiveLease)
	}
	cleanupResponsesWSOpenResult(openResult, "test_done")
	if got := atomic.LoadInt32(&lease.releases); got != 1 {
		t.Fatalf("expected final group lease to release once, got %d", got)
	}
	select {
	case conn := <-upgraded:
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
	case <-time.After(time.Second):
		t.Fatal("expected backup websocket server to be used")
	}
}

func TestOpenAndPrimeResponsesWSCodexTokenInvalidatedFallsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
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
	failed.Other = `{"responses_ws_self_hosted":true}`
	fallback := newRelayTestCodexChannel(fallbackChannelID)
	fallback.BaseURL = &fallbackBaseURL
	fallback.Other = `{"responses_ws_self_hosted":true}`
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(failed, fallback)
	model.ChannelGroup.Rule["default"]["gpt-5"] = [][]int{{failedChannelID}, {fallbackChannelID}}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "default")

	store := false
	request := &types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hi", Store: &store}
	openResult, apiErr := openAndPrimeResponsesWSSessionWithContextAndFrameAdmissionAndBudget(context.Background(), ctx, responsesWSTestOpenFrame(t), request, nil, 2)
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

func TestOpenAndPrimeResponsesWSUnsupportedHandshakeUsesConfiguredFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
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
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(
		&model.Channel{Id: 81, Type: config.ChannelTypeOpenAI, Key: "sk-unsupported", BaseURL: &unsupportedBaseURL, Proxy: &proxy, Other: responsesWSTestSelfHostedOther},
		&model.Channel{Id: 82, Type: config.ChannelTypeOpenAI, Key: "sk-fallback", BaseURL: &fallbackBaseURL, Proxy: &proxy, Other: responsesWSTestSelfHostedOther},
	)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "default")

	openResult, apiErr := openAndPrimeResponsesWSSessionWithContextAndFrameAdmissionAndBudget(context.Background(), ctx, nil, &types.OpenAIResponsesRequest{Model: "gpt-5"}, nil, 2)
	if apiErr != nil {
		t.Fatalf("expected one configured fallback after unsupported handshake, got %v", apiErr)
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
	preferred := &model.Channel{Id: preferredChannelID, Type: config.ChannelTypeOpenAI, Key: "sk-preferred", BaseURL: &preferredBaseURL, Proxy: &proxy, Other: responsesWSTestSelfHostedOther}
	unsupported := &model.Channel{Id: unsupportedChannelID, Type: config.ChannelTypeOpenAI, Key: "sk-unsupported", BaseURL: &unsupportedBaseURL, Proxy: &proxy, Other: responsesWSTestSelfHostedOther}
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
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(&model.Channel{
		Id:      91,
		Type:    config.ChannelTypeOpenAI,
		Key:     "sk-unsupported",
		BaseURL: &baseURL,
		Proxy:   &proxy,
		Other:   responsesWSTestSelfHostedOther,
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

func TestOpenAndPrimeResponsesWSOrdinaryCapabilityExhaustionPreservesError(t *testing.T) {
	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(channelGroupSnapshot) })

	tests := []struct {
		name       string
		channel    *model.Channel
		store      *bool
		wantStatus int
		wantCode   string
		wantText   string
	}{
		{
			name:       "adapter has no native websocket",
			channel:    &model.Channel{Id: 92, Type: config.ChannelTypeAnthropic, Key: "sk-test"},
			store:      boolPointer(false),
			wantStatus: http.StatusUpgradeRequired,
			wantCode:   "responses_ws_unsupported_for_channel",
			wantText:   "not enabled for native Responses WebSocket",
		},
		{
			name:       "stored lifecycle unsupported",
			channel:    &model.Channel{Id: 93, Type: config.ChannelTypeCodex, Key: "sk-test"},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   unsupportedCapabilityCode,
			wantText:   "complete Stored Responses lifecycle",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(test.channel)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			ctx.Set("token_group", "default")

			result, apiErr := openAndPrimeResponsesWSSession(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5", Store: test.store})
			if result != nil || apiErr == nil || apiErr.StatusCode != test.wantStatus || openAIErrorCodeString(apiErr.Code, "") != test.wantCode || !strings.Contains(apiErr.Message, test.wantText) {
				t.Fatalf("ordinary capability exhaustion result=%#v err=%+v", result, apiErr)
			}
		})
	}
}

func TestOpenAndPrimeResponsesWSMixedUnsupportedPreservesProviderError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
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
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(
		&model.Channel{
			Id:      92,
			Type:    config.ChannelTypeOpenAI,
			Key:     "sk-unsupported",
			BaseURL: &unsupportedBaseURL,
			Proxy:   &proxy,
			Other:   responsesWSTestSelfHostedOther,
		},
		&model.Channel{
			Id:      93,
			Type:    config.ChannelTypeOpenAI,
			Key:     "sk-bad-gateway",
			BaseURL: &badGatewayBaseURL,
			Proxy:   &proxy,
			Other:   responsesWSTestSelfHostedOther,
		},
	)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("token_group", "default")

	openResult, apiErr := openAndPrimeResponsesWSSessionWithContextAndFrameAdmissionAndBudget(context.Background(), ctx, nil, &types.OpenAIResponsesRequest{Model: "gpt-5"}, nil, 2)
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
	setResponsesWSTestViperInt(t, "responses_ws.unsupported_scan_limit", 5)

	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(
		&model.Channel{Id: 101, Type: config.ChannelTypeOpenAI, Key: "sk-1"},
		&model.Channel{Id: 102, Type: config.ChannelTypeOpenAI, Key: "sk-2"},
	)

	if got, _ := responsesWSUnsupportedScanPolicyForConfigured(10); got != 2 {
		t.Fatalf("expected unsupported scan limit to cap at channel count 2, got %d", got)
	}
	if limit, limited := responsesWSUnsupportedScanPolicyForConfigured(10); limit != 2 || limited {
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

	bridge := NewResponsesWSIOPump(nil, actor)
	t.Cleanup(bridge.Close)
	bridge.ArmProviderRecvPump("generation-1", 11, session)

	waitResponsesWSTestCondition(t, time.Second, time.Millisecond, func() bool {
		return actor.closing.closeIntentPosted.Load()
	}, func() string {
		return "expected failed provider event post to request close intent"
	})
	bridge.Close()

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

func TestResponsesWSResponseCancelIsNotAPublicClientEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
		t.Fatal("response.cancel must not cancel a native Responses websocket turn")
	default:
	}
	if actor.closing.closed.Load() {
		t.Fatal("unsupported response.cancel should not be translated into session cancellation")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "unsupported_client_event") {
		t.Fatalf("expected response.cancel to be rejected as unsupported, got %q", got)
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
			Reason: responsesws.ResponsesWSTransportSendReason("provider_queue_full"),
		},
	}

	actor.handleSendCommand(responsesWSSendCommand{
		AttemptID:         "attempt-not-sent",
		SelectedChannelID: 17,
		Purpose:           ResponsesWSSendPurposeResponseCreate,
		Session:           session,
		Frame:             responsesWSFrameFromWireMessage(responsesWSTextMessageType, []byte(`{"type":"response.create","model":"gpt-5"}`)),
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
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","status":"completed"}}`)),
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))

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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
	bridge := NewResponsesWSIOPump(nil, actor)
	actor.SetPump(bridge)
	session := &responsesWSTestSession{}
	upstreamSessionGeneration := actor.AttachUpstreamSession(session, 17)

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: upstreamSessionGeneration,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_no_turn"}}`)),
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
	bridge := NewResponsesWSIOPump(conn, actor)
	actor.SetPump(bridge)
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
	payload, _ := conn.lastControl.Load().(string)
	if len(payload) < 2 || int(binary.BigEndian.Uint16([]byte(payload)[:2])) != int(wsconn.CloseInternalServerErr) {
		t.Fatalf("provider EOF must use close code 1011, got %q", payload)
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

func TestResponsesWSProviderClosedDuringPendingAttemptIsBuffered(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOPump(conn, actor)
	actor.SetPump(bridge)
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
		AttemptID:                 "attempt-provider-close-pending",
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

func TestResponsesWSDuplicateProviderTerminalDoesNotDoubleFinalize(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-duplicate-terminal")

	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(&responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}, actor))
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
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_once","status":"completed","usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}}`)),
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

func TestResponsesWSProofConflictSettlementFailurePreservesPendingAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-proof-conflict-settlement-fails",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal = responsesWSTestProviderFrameJournal()
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

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
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "quota_settlement_failed") {
		t.Fatalf("expected quota settlement failure payload, got %q", got)
	}
}

func TestResponsesWSPendingFatalSettlementDoesNotStartQueuedTurn(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-pending-fatal")
	installResponsesWSTestAPILimiter(t, 100)
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Models: "gpt-5"})

	actor := NewResponsesWSSessionActor(ctx)
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnPrepare
	actor.state = responsesWSStatePendingPrepare
	queuedPayload := []byte(`{"type":"response.create","model":"gpt-5","store":false,"input":[]}`)
	if !actor.turns.queue.Push(responsesWSTestClientTextFrame(queuedPayload), responsesWSQueuedCreateMaxFrames, responsesWSQueuedCreateMaxBytes) {
		t.Fatal("expected next turn to enter the FIFO")
	}

	if err := actor.settlePendingAttemptBeforeLocalWrite("rewrite_failed"); err != nil {
		t.Fatalf("expected pending fatal settlement to succeed, got %v", err)
	}

	if len(actor.turns.queue.items) != 1 || actor.turns.pending.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("fatal pending cleanup must not start the queued turn, queue=%d pending=%+v state=%v", len(actor.turns.queue.items), actor.turns.pending.attempt, actor.state)
	}
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 1000 || token.RemainQuota != 1000 {
		t.Fatalf("queued turn must not preconsume before the fatal path closes, user=%+v token=%+v", user, token)
	}
}

func TestResponsesWSSubsequentModelMustBeSupportedBySelectedChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group", "default")
	ctx.Set("original_model", "gpt-5")
	ctx.Set("new_model", "gpt-5")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Models: "gpt-5"})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.state = responsesWSStateIdle
	actor.turns.pending.phase = responsesWSPendingTurnNone

	actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"gpt-4","input":[]}`), time.Now())

	if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone || actor.state != responsesWSStateIdle {
		t.Fatalf("expected unsupported model before attempt creation, state=%v phase=%v pending=%+v", actor.state, actor.turns.pending.phase, actor.turns.pending.attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, `"code":"responses_ws_model_unsupported_by_channel"`) {
		t.Fatalf("expected selected-channel model error, got %q", got)
	} else if !strings.Contains(got, `"type":"invalid_request_error"`) {
		t.Fatalf("expected selected-channel model error to preserve websocket error type, got %q", got)
	}
}

func TestResponsesWSSubsequentTurnRejectsDisabledStreamModelBeforeProviderSend(t *testing.T) {
	gin.SetMode(gin.TestMode)

	disabled := datatypes.JSONSlice[string]{"gpt-5"}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group", "default")
	ctx.Set("original_model", "gpt-5")
	ctx.Set("new_model", "gpt-5")
	ctx.Set("responses_ws_selected_channel", &model.Channel{
		Id:             17,
		Type:           config.ChannelTypeOpenAI,
		Models:         "gpt-5",
		DisabledStream: &disabled,
	})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSCaptureSendSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	actor.state = responsesWSStateIdle
	actor.turns.pending.phase = responsesWSPendingTurnNone

	actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"gpt-5","store":false,"input":[]}`), time.Now())

	got, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, got, http.StatusUpgradeRequired, "responses_ws_unsupported_for_channel", "does not allow streaming")
	if calls := atomic.LoadInt32(&session.calls); calls != 0 {
		t.Fatalf("expected stream admission failure before provider send, got %d sends", calls)
	}
}

func TestResponsesWSSubsequentTurnRejectsDurableOwnerOnAnotherChannelBeforeProviderSend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayTestDB(t, &model.ResponseOwner{})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group", "default")
	ctx.Set("original_model", "gpt-5")
	ctx.Set("new_model", "gpt-5")
	ctx.Set("id", 11)
	ctx.Set("token_id", 21)
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Models: "gpt-5"})
	owner, err := model.NewResponseOwner("resp_other_channel", 11, 21, 31, time.Now())
	if err != nil {
		t.Fatalf("create response owner: %v", err)
	}
	if err := model.CreateResponseOwner(ctx.Request.Context(), owner); err != nil {
		t.Fatalf("persist response owner: %v", err)
	}

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSCaptureSendSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	actor.state = responsesWSStateIdle
	actor.turns.pending.phase = responsesWSPendingTurnNone

	actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"gpt-5","previous_response_id":"resp_other_channel","input":[]}`), time.Now())

	got, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, got, http.StatusConflict, "responses_affinity_conflict", responsesWSStaticErrorMessage("responses_affinity_conflict"))
	if calls := atomic.LoadInt32(&session.calls); calls != 0 {
		t.Fatalf("expected owner conflict before any provider send, got %d sends", calls)
	}
	if actor.turns.pending.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected owner conflict before attempt creation, state=%v pending=%+v", actor.state, actor.turns.pending.attempt)
	}
}

func TestResponsesWSSubsequentTurnUsesConfiguredModelNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group", "responses-ws-configured-model")
	ctx.Set("original_model", "model-alias")
	ctx.Set("new_model", "model-alias")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Models: "model-alias"})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.state = responsesWSStateIdle
	actor.turns.pending.phase = responsesWSPendingTurnNone

	actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"provider-model","store":false,"input":[]}`), time.Now())

	got, _ := conn.lastWrite.Load().(string)
	if !strings.Contains(got, "responses_ws_model_unsupported_by_channel") {
		t.Fatalf("expected the channel's configured model names to remain authoritative, got %q", got)
	}
}

func TestResponsesWSSubsequentTurnRevalidatesSupportedSurface(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group", "default")
	ctx.Set("original_model", "gpt-5")
	ctx.Set("new_model", "gpt-5")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Models: "gpt-5"})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.state = responsesWSStateIdle
	actor.turns.pending.phase = responsesWSPendingTurnNone

	actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"gpt-5","background":true,"input":[]}`), time.Now())

	if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone || actor.state != responsesWSStateIdle {
		t.Fatalf("expected unsupported surface before attempt creation, state=%v phase=%v pending=%+v", actor.state, actor.turns.pending.phase, actor.turns.pending.attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, `"code":"unsupported_capability"`) {
		t.Fatalf("expected supported-surface capability error, got %q", got)
	}
}

func TestResponsesWSSubsequentTurnChecksStoredLifecycleForCurrentStore(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newActor := func(t *testing.T) (*ResponsesWSSessionActor, *responsesWSFakeUserConn) {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		ctx.Set("group", "responses-ws-missing-limiter")
		ctx.Set("original_model", "gpt-5")
		ctx.Set("new_model", "gpt-5")
		ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeCodex, Models: "gpt-5"})

		conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
		actor := NewResponsesWSSessionActor(ctx)
		actor.SetPump(NewResponsesWSIOPump(conn, actor))
		actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
		actor.state = responsesWSStateIdle
		actor.turns.pending.phase = responsesWSPendingTurnNone
		return actor, conn
	}

	t.Run("store false keeps native websocket path", func(t *testing.T) {
		actor, conn := newActor(t)
		actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"gpt-5","store":false,"input":[]}`), time.Now())

		if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, `"code":"api_requests_not_allowed"`) {
			t.Fatalf("expected store=false to pass channel capability and reach RPM admission, got %q", got)
		}
	})

	t.Run("default store requires complete lifecycle", func(t *testing.T) {
		actor, conn := newActor(t)
		actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"gpt-5","input":[]}`), time.Now())

		if actor.turns.pending.attempt != nil || actor.turns.pending.phase != responsesWSPendingTurnNone || actor.state != responsesWSStateIdle {
			t.Fatalf("expected stored lifecycle rejection before attempt creation, state=%v phase=%v pending=%+v", actor.state, actor.turns.pending.phase, actor.turns.pending.attempt)
		}
		if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, `"code":"responses_ws_unsupported_for_channel"`) {
			t.Fatalf("expected selected-channel stored lifecycle error, got %q", got)
		}
	})
}

func TestResponsesWSSubsequentTurnLeavesCodexParameterSemanticsUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Set("group", "default")
	ctx.Set("original_model", "gpt-5")
	ctx.Set("new_model", "gpt-5")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeCodex, Models: "gpt-5"})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	actor.state = responsesWSStateIdle
	actor.turns.pending.phase = responsesWSPendingTurnNone

	actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"gpt-5","store":false,"truncation":"auto","input":[]}`), time.Now())

	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, `"code":"api_requests_not_allowed"`) || strings.Contains(got, `"param":"truncation"`) {
		t.Fatalf("expected truncation to pass capability gate and reach RPM admission, got %q", got)
	}
}

func TestResponsesWSChannelSupportsExactAndWildcardModels(t *testing.T) {
	channel := &model.Channel{Models: "gpt-5, gpt-5.6-*"}
	if !responsesWSChannelSupportsExactModel(channel, "gpt-5") || !responsesWSChannelSupportsExactModel(channel, "gpt-5.6-sol") {
		t.Fatal("expected exact and configured wildcard models to be supported")
	}
	if responsesWSChannelSupportsExactModel(channel, "gpt-4") {
		t.Fatal("expected unconfigured model to be rejected")
	}

	alias := &model.Channel{Models: "gpt-5.6"}
	if responsesWSChannelSupportsExactModel(alias, "gpt-5.6-sol") {
		t.Fatal("model aliases must be configured explicitly instead of being inferred in the relay")
	}

	unrelatedMapping := `{"gpt-4":"provider-gpt-4"}`
	channel.ModelMapping = &unrelatedMapping
	if !responsesWSChannelSupportsExactModel(channel, "gpt-5") {
		t.Fatal("an unrelated channel model mapping must not reject the current model")
	}
	currentMapping := `{"gpt-5":"provider-gpt-5"}`
	channel.ModelMapping = &currentMapping
	if responsesWSChannelSupportsExactModel(channel, "gpt-5") {
		t.Fatal("a mapping that changes the current provider model must be rejected")
	}
}

func TestResponsesWSClientFrameParseErrorUsesStaticMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))

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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))

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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))

	actor.startSubsequentTurn([]byte(`{"type":"response.cancel","model":"gpt-5"}`), time.Now())

	got, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, got, http.StatusBadRequest, responsesWSErrorCodeInvalidResponseCreate, responsesWSMessageInvalidResponseCreate)
	if strings.Contains(got, "unsupported responses websocket event type") {
		t.Fatalf("expected client payload to hide parser detail, got %q", got)
	}
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
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Models: "gpt-5"})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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

func TestResponsesWSSubsequentStalePreflightRejectsBeforeRPMAndKeepsSessionOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayTestDB(t, &model.ResponseOwner{})

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
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Models: "gpt-5"})
	owner, ownerErr := model.NewResponseOwner("resp_old", 1, 1, 17, time.Now())
	if ownerErr != nil {
		t.Fatalf("create response owner: %v", ownerErr)
	}
	if ownerErr = model.CreateResponseOwner(ctx.Request.Context(), owner); ownerErr != nil {
		t.Fatalf("persist response owner: %v", ownerErr)
	}
	recordResponsesEphemeralProof(ctx, "resp_old", 17)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
	if actor.closing.closed.Load() || actor.state != responsesWSStateIdle {
		t.Fatalf("expected stale request preflight to keep downstream session idle, closed=%v state=%v", actor.closing.closed.Load(), actor.state)
	}
	got, _ := conn.lastWrite.Load().(string)
	assertResponsesWSErrorPayload(t, got, http.StatusBadRequest, "previous_response_not_found", "previous response was not found")
	if !strings.Contains(got, `"param":"previous_response_id"`) {
		t.Fatalf("expected previous_response_id param in stale payload, got %q", got)
	}
	if strings.Contains(got, "api_requests_not_allowed") {
		t.Fatalf("expected stale preflight before RPM limiter, got %q", got)
	}
	storedOwner, ownerErr := model.GetResponseOwner(ctx.Request.Context(), "resp_old", ctx.GetInt("id"))
	if ownerErr != nil || storedOwner.State != model.ResponseOwnerStateActive {
		t.Fatalf("a provider continuation miss must preserve its durable owner, owner=%+v err=%v", storedOwner, ownerErr)
	}
	if channelID, ok := lookupResponsesEphemeralProof(ctx, "resp_old"); ok || channelID != 0 {
		t.Fatalf("provider continuation miss must clear its ephemeral proof, channel=%d ok=%v", channelID, ok)
	}
}

func TestResponsesWSCreateQueueClosesOnBoundedOverflow(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-busy-rate-limit")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.turns.active.attempt = attempt
	actor.state = responsesWSStateInFlight

	for i := 0; i < responsesWSQueuedCreateMaxFrames+1; i++ {
		actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`)))
	}

	if !actor.closing.closed.Load() {
		t.Fatal("expected excessive busy response.create frames to close the session")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_turn_queue_full") {
		t.Fatalf("expected bounded queue overflow error, got %q", got)
	}
}

func TestResponsesWSSendQueueHasAggregateByteBudget(t *testing.T) {
	actor := NewResponsesWSSessionActor(nil)
	frame := responsesws.NewTextFrame(make([]byte, responsesWSSendQueueMaxBytes/2+1))
	command := responsesWSSendCommand{Frame: frame}
	if !actor.enqueueProviderSend(command) {
		t.Fatal("expected first command within aggregate byte budget")
	}
	if actor.enqueueProviderSend(command) {
		t.Fatal("second command must exceed aggregate send byte budget")
	}
	queued := <-actor.workers.sendCommands
	actor.workers.sendBytes.Add(-int64(queued.Frame.PayloadLen()))
}

func TestResponsesWSEventQueueHasAggregateByteBudget(t *testing.T) {
	actor := NewResponsesWSSessionActor(nil)
	actor.closing.backpressurePosted.Store(true)
	payload := make([]byte, responsesWSEventQueueMaxBytes/2+1)
	frame := responsesws.NewTextFrame(payload)
	event := ResponsesWSEventProviderDownstream{Frame: &frame}
	if !actor.Post(event) {
		t.Fatal("first provider event should fit the aggregate byte budget")
	}
	if actor.Post(event) {
		t.Fatal("second provider event should exceed the aggregate byte budget")
	}
	if got := actor.eventBytes.Load(); got != int64(len(payload)) {
		t.Fatalf("reserved event bytes=%d, want %d", got, len(payload))
	}
	queued := <-actor.events
	actor.releaseEventBytes(queued)
	if !actor.Post(event) {
		t.Fatal("consuming an event should release its byte budget")
	}
}

func TestResponsesWSCreateQueuePreservesFIFOOrderAndByteBudget(t *testing.T) {
	firstPayload := []byte(`{"type":"response.create","event_id":"first","model":"gpt-5"}`)
	secondPayload := []byte(`{"type":"response.create","event_id":"second","model":"gpt-5"}`)
	firstAt := time.Now()
	secondAt := firstAt.Add(time.Millisecond)

	var queue responsesWSCreateQueue
	if !queue.Push(ResponsesWSEventClientFrame{Frame: responsesws.NewTextFrame(firstPayload), ReceivedAt: firstAt}, 2, len(firstPayload)+len(secondPayload)) {
		t.Fatal("expected first response.create to enter the queue")
	}
	if !queue.Push(ResponsesWSEventClientFrame{Frame: responsesws.NewTextFrame(secondPayload), ReceivedAt: secondAt}, 2, len(firstPayload)+len(secondPayload)) {
		t.Fatal("expected second response.create to enter the queue")
	}
	if queue.bytes != len(firstPayload)+len(secondPayload) {
		t.Fatalf("unexpected queued byte count: %d", queue.bytes)
	}

	first, ok := queue.Pop()
	if !ok || string(first.payload) != string(firstPayload) || !first.receivedAt.Equal(firstAt) {
		t.Fatalf("expected first queued turn first, got %+v", first)
	}
	second, ok := queue.Pop()
	if !ok || string(second.payload) != string(secondPayload) || !second.receivedAt.Equal(secondAt) {
		t.Fatalf("expected second queued turn second, got %+v", second)
	}
	if _, ok := queue.Pop(); ok || queue.bytes != 0 {
		t.Fatalf("expected empty queue after FIFO drain, bytes=%d", queue.bytes)
	}
}

func TestResponsesWSPendingProviderBufferHasByteCap(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-buffer")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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

func TestResponsesWSIdleTimeoutDoesNotInterruptActiveTurn(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-idle-cleanup")
	actor := NewResponsesWSSessionActor(ctx)
	actor.turns.active.attempt = attempt
	actor.state = responsesWSStateInFlight

	actor.handleTimeout(ResponsesWSEventTimeout{Reason: "idle_timeout"})

	if actor.closing.closed.Load() || actor.turns.active.attempt != attempt {
		t.Fatalf("idle cleanup must not interrupt an active turn, closed=%v active=%v", actor.closing.closed.Load(), actor.turns.active.attempt == attempt)
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-watchdog-refresh",
		SelectedChannelID: 17,
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
		DetailOrigin: responsesws.RecvDetailOriginNativeProviderEOF,
	})
	if actor.watchdog.activeTurnTimerGen != refreshedGen {
		t.Fatalf("expected provider EOF without activity not to refresh watchdog, before=%d after=%d", refreshedGen, actor.watchdog.activeTurnTimerGen)
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

func TestResponsesWSTerminalSideEffectsRequireSuccessfulClientDelivery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-terminal")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1), writeErr: errors.New("client write failed")}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_write_failed","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if actor.turns.history.lastFinal != nil {
		t.Fatalf("terminal affinity side effects must not run when client delivery fails, got %+v", actor.turns.history.lastFinal)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected client terminal write failure to close the session")
	}
}

func TestResponsesWSRejectsNonIncreasingProviderSequence(t *testing.T) {
	for _, tc := range []struct {
		name           string
		secondSequence int
	}{
		{name: "duplicate", secondSequence: 2},
		{name: "out of order", secondSequence: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-sequence-"+strings.ReplaceAll(tc.name, " ", "-"))
			conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
			actor := NewResponsesWSSessionActor(ctx)
			actor.SetPump(NewResponsesWSIOPump(conn, actor))
			generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
			actor.turns.active.attempt = attempt
			actor.turns.active.channelID = 17
			actor.state = responsesWSStateInFlight

			actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
				AttemptID:                 attempt.AttemptID,
				UpstreamSessionGeneration: generation,
				ChannelID:                 17,
				Kind:                      ProviderDownstreamFrame,
				Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.created","sequence_number":2,"response":{"id":"resp_sequence","status":"in_progress"}}`)),
				DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
			})
			actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
				AttemptID:                 attempt.AttemptID,
				UpstreamSessionGeneration: generation,
				ChannelID:                 17,
				Kind:                      ProviderDownstreamFrame,
				Frame: responsesWSTestProviderTextFrame([]byte(fmt.Sprintf(
					`{"type":"response.in_progress","sequence_number":%d,"response":{"id":"resp_sequence","status":"in_progress"}}`,
					tc.secondSequence,
				))),
				DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
			})

			if !actor.closing.closed.Load() || attempt.TerminalObserved {
				t.Fatalf("expected non-increasing sequence to fail closed without terminal result, closed=%v terminal=%v", actor.closing.closed.Load(), attempt.TerminalObserved)
			}
			if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_provider_protocol_error") {
				t.Fatalf("expected provider protocol error, got %q", got)
			}
		})
	}
}

func TestResponsesWSMultiAgentTerminalWaitsForAcceptedInjectAcknowledgement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-multi-agent-terminal")
	attempt.MultiAgentEnabled = true
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.turns.inject.pending = 1
	injectContext := actor.turns.inject.Context(ctx.Request.Context())
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_multi_agent","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})
	terminalWrite, _ := conn.lastWrite.Load().(string)

	if actor.turns.active.attempt != attempt || actor.state != responsesWSStateInFlight || actor.turns.inject.pending != 1 || !actor.turns.inject.terminalSeen {
		t.Fatalf("terminal must retain the turn until accepted injects are acknowledged, state=%v active=%+v inject=%+v", actor.state, actor.turns.active.attempt, actor.turns.inject)
	}
	select {
	case <-injectContext.Done():
		t.Fatal("terminal must not cancel an already accepted inject")
	default:
	}
	queuedSession := &responsesWSCaptureSendSession{requests: make(chan responsesws.SendRequest, 1)}
	result := actor.sendResultForCommand(injectContext, responsesWSSendCommand{
		AttemptID: attempt.AttemptID,
		Purpose:   ResponsesWSSendPurposeResponseInject,
		Session:   queuedSession,
		Frame:     responsesws.NewTextFrame([]byte(`{"type":"response.inject"}`)),
	})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted {
		t.Fatalf("accepted inject must still reach the provider after terminal: %+v", result)
	}
	select {
	case <-queuedSession.requests:
	default:
		t.Fatal("accepted inject did not reach provider after terminal")
	}
	if !strings.Contains(terminalWrite, "response.completed") {
		t.Fatalf("expected terminal to be delivered, got %q", terminalWrite)
	}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.inject.created","sequence_number":2,"response_id":"resp_multi_agent"}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle || actor.turns.inject.pending != 0 {
		t.Fatalf("final inject acknowledgement must release the turn, state=%v active=%+v inject=%+v", actor.state, actor.turns.active.attempt, actor.turns.inject)
	}
	select {
	case <-injectContext.Done():
	default:
		t.Fatal("turn completion must release the inject send context")
	}
}

func TestResponsesWSInjectAcknowledgementTimeoutAfterTerminalDoesNotEmitSecondError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-inject-timeout")
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt.TerminalObserved = true
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.turns.inject.pending = 1
	actor.turns.inject.terminalSeen = true
	actor.state = responsesWSStateInFlight
	actor.armActiveTurnWatchdog()

	actor.watchdog.activeTurnMu.Lock()
	timerGeneration := actor.watchdog.activeTurnTimerGen
	actor.watchdog.activeTurnMu.Unlock()
	actor.handleTimeout(ResponsesWSEventTimeout{
		Reason:                    responsesWSActiveTurnTimeoutReason,
		UpstreamSessionGeneration: actor.upstream.sessionGeneration,
		ChannelID:                 17,
		AttemptID:                 attempt.AttemptID,
		TimeoutGeneration:         timerGeneration,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("inject acknowledgement timeout must close the stalled connection")
	}
	if got := atomic.LoadInt32(&conn.writeCount); got != 0 {
		t.Fatalf("terminal acknowledgement timeout must not emit a second wire result, writes=%d last=%q", got, conn.lastWrite.Load())
	}
}

func TestResponsesWSResponseInjectIsForwardedUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	session := &responsesWSCaptureSendSession{requests: make(chan responsesws.SendRequest, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()
	actor.AttachUpstreamSession(session, 17)
	attempt := &ResponsesWSTurnAttempt{AttemptID: "attempt-inject", SelectedChannelID: 17, MultiAgentEnabled: true}
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight
	payload := []byte(`{"type":"response.inject","event_id":"inject-client-1","input":{"future":true}}`)

	actor.handleClientFrame(responsesWSTestClientTextFrame(payload))

	select {
	case req := <-session.requests:
		if req.AttemptID != attempt.AttemptID || string(req.Frame.Payload()) != string(payload) {
			t.Fatalf("expected unchanged response.inject with active attempt identity, got attempt=%q payload=%s", req.AttemptID, req.Frame.Payload())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for response.inject upstream send")
	}
	if actor.turns.inject.pending != 1 {
		t.Fatalf("expected one pending inject acknowledgement, got %d", actor.turns.inject.pending)
	}
}

func TestResponsesWSResponseInjectRequiresMultiAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSCaptureSendSession{requests: make(chan responsesws.SendRequest, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	actor.turns.active.attempt = &ResponsesWSTurnAttempt{AttemptID: "attempt-no-multi-agent", SelectedChannelID: 17}
	actor.state = responsesWSStateInFlight

	actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.inject","event_id":"inject-disabled"}`)))

	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_multi_agent_required") {
		t.Fatalf("expected multi-agent requirement error, got %q", got)
	}
	select {
	case req := <-session.requests:
		t.Fatalf("disabled response.inject must not reach upstream, got %+v", req)
	default:
	}
}

func TestResponsesWSConnectionLocalEphemeralContinuationBypassesExternalOwnerStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	actor := NewResponsesWSSessionActor(ctx)
	actor.upstream.channelID = 17
	actor.rememberConnectionLocalEphemeralResponseID("resp_local")
	storeFalse := false
	storeTrue := true
	for _, test := range []struct {
		name  string
		store *bool
	}{
		{name: "ephemeral next response", store: &storeFalse},
		{name: "stored next response", store: &storeTrue},
		{name: "default stored next response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, ok := actor.connectionLocalTurnAffinity(ctx, &types.OpenAIResponsesRequest{
				Store:              test.store,
				PreviousResponseID: "resp_local",
			})
			if !ok || candidate == nil || candidate.OwnershipChannelID != 17 || candidate.PreviousResponseID != "resp_local" {
				t.Fatalf("expected current-connection response ownership, candidate=%+v ok=%v", candidate, ok)
			}
		})
	}
	if _, ok := actor.connectionLocalTurnAffinity(ctx, &types.OpenAIResponsesRequest{
		Store:              &storeFalse,
		PreviousResponseID: "resp_other",
	}); ok {
		t.Fatal("unknown response id must still use the ordinary ownership path")
	}
}

func TestResponsesWSConnectionLocalContinuationCapturesCurrentTurnAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 60,
		Rules: []config.ChannelAffinityRule{
			{
				Name:            "responses-local-prompt",
				Enabled:         true,
				Kind:            "responses",
				RecordOnSuccess: true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "request_field", Key: "prompt_cache_key", Alias: config.ChannelAffinityAliasPromptCacheKey},
				},
			},
		},
	}
	settings.Normalize()
	withChannelAffinitySettings(t, settings)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	actor := NewResponsesWSSessionActor(ctx)
	actor.upstream.channelID = 17
	actor.rememberConnectionLocalEphemeralResponseID("resp-local-current")
	store := true
	candidate, ok := actor.connectionLocalTurnAffinity(ctx, &types.OpenAIResponsesRequest{
		Model:              "gpt-5",
		Store:              &store,
		PreviousResponseID: "resp-local-current",
		PromptCacheKey:     "prompt-current",
	})
	if !ok || candidate == nil || candidate.State == nil || len(candidate.State.RequestBindings) != 1 {
		t.Fatalf("connection-local continuation lost current turn affinity state: candidate=%+v ok=%v", candidate, ok)
	}
	if candidate.State.RequestBindings[0].Value != "prompt-current" {
		t.Fatalf("captured affinity value = %q, want prompt-current", candidate.State.RequestBindings[0].Value)
	}
}

func TestResponsesWSFailedTerminalDoesNotCloseSessionForTurnScopedError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-failed")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.failed","sequence_number":1,"response":{"id":"resp_failed","status":"failed","error":{"type":"invalid_request_error","code":"bad_input","message":"bad input"}}}`)),
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

func TestResponsesWSRequestErrorFinalizesTurnAndStartsQueuedCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-request-error")
	installResponsesWSTestAPILimiter(t, 100)
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Models: "gpt-5"})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSCaptureSendSession{requests: make(chan responsesws.SendRequest, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(session, 17)
	actor.upstream.recvArmed = true
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight
	queuedPayload := []byte(`{"type":"response.create","model":"gpt-5","store":false,"input":[]}`)
	if !actor.turns.queue.Push(responsesWSTestClientTextFrame(queuedPayload), responsesWSQueuedCreateMaxFrames, responsesWSQueuedCreateMaxBytes) {
		t.Fatal("expected next turn to enter the FIFO")
	}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"previous response was not found","param":"previous_response_id"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if actor.closing.closed.Load() {
		t.Fatal("expected request-level provider error to keep websocket session open")
	}
	if !attempt.RolledBack || attempt.QuotaFinalized || attempt.CompletedAt.IsZero() {
		t.Fatalf("expected request-error attempt to be completed and settled, attempt=%+v", attempt)
	}
	if len(actor.turns.queue.items) != 0 {
		t.Fatalf("expected queued create to be dequeued, queue=%d", len(actor.turns.queue.items))
	}
	select {
	case req := <-session.requests:
		if string(req.Frame.Payload()) != string(queuedPayload) {
			t.Fatalf("expected queued response.create to reach the same upstream session unchanged, got %s", req.Frame.Payload())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for queued response.create after request error")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, `"previous_response_not_found"`) {
		t.Fatalf("expected original request error to be delivered, got %q", got)
	}
}

func TestResponsesWSConnectionErrorClosesSessionAndDiscardsQueue(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-connection-error")
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight
	queuedPayload := []byte(`{"type":"response.create","model":"gpt-5","store":false,"input":[]}`)
	if !actor.turns.queue.Push(responsesWSTestClientTextFrame(queuedPayload), responsesWSQueuedCreateMaxFrames, responsesWSQueuedCreateMaxBytes) {
		t.Fatal("expected next turn to enter the FIFO")
	}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached","message":"create a new websocket connection"}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected connection-level provider error to close websocket session")
	}
	if len(actor.turns.queue.items) != 0 {
		t.Fatalf("expected queued work to be discarded on connection error, queue=%d", len(actor.turns.queue.items))
	}
}

func TestResponsesWSIncompleteTerminalWithoutErrorDoesNotCloseSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-incomplete")
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.incomplete","sequence_number":1,"response":{"id":"resp_incomplete","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if actor.closing.closed.Load() {
		t.Fatal("expected incomplete terminal without explicit error detail to keep websocket session open")
	}
	if actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected incomplete terminal to clear only the active turn, state=%v active=%+v", actor.state, actor.turns.active.attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "response.incomplete") || !strings.Contains(got, "max_output_tokens") {
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.failed","sequence_number":1,"account_id":"acct-secret","response":{"id":"resp_limit","status":"failed","error":{"type":"usage_limit_reached","message":"monthly usage limit reached for org-secret"}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if actor.closing.closed.Load() {
		t.Fatal("expected provider api error not to close websocket session")
	}
	select {
	case apiErr := <-errCh:
		if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Code != "provider_account_error" || !apiErr.ProviderQuotaExhausted {
			t.Fatalf("expected safe usage-limit control error, got %#v", apiErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for provider error control-plane handling")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, `"code":"provider_account_error"`) || strings.Contains(got, "org-secret") || strings.Contains(got, "acct-secret") || !strings.Contains(got, `"id":"resp_limit"`) {
		t.Fatalf("expected safe provider error with preserved lifecycle envelope, got %q", got)
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
		if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Code != "provider_account_error" || !apiErr.ProviderQuotaExhausted {
			t.Fatalf("expected safe usage-limit provider error, got %#v", apiErr)
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
		if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Code != "provider_account_error" || !apiErr.ProviderQuotaExhausted {
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	terminalReceivedAt := time.Now().Add(-250 * time.Millisecond)
	attempt.TransportResult = responsesws.ResponsesWSTransportSendResult{
		Status: responsesws.ResponsesWSTransportSendAmbiguous,
		Err:    errors.New("ambiguous send"),
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal = responsesWSTestProviderFrameJournal()
	actor.turns.pending.provider.journal.appendDownstreamFixture(ResponsesWSEventProviderDownstream{
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_close_buffered","status":"completed","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   terminalReceivedAt,
	})
	actor.turns.pending.provider.journal.appendDownstreamFixture(ResponsesWSEventProviderDownstream{
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_close_buffered_duplicate","status":"completed","usage":{"input_tokens":30,"output_tokens":40,"total_tokens":70}}}`)),
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	terminalReceivedAt := time.Now().Add(-250 * time.Millisecond)
	attempt.TransportResult = responsesws.ResponsesWSTransportSendResult{
		Status: responsesws.ResponsesWSTransportSendAmbiguous,
		Err:    errors.New("ambiguous send"),
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal = responsesWSTestProviderFrameJournal()
	actor.turns.pending.provider.journal.appendDownstreamFixture(ResponsesWSEventProviderDownstream{
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_close_buffered_zero","status":"completed","usage":{}}}`)),
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
	actor.turns.pending.provider.journal.appendDownstreamFixture(ResponsesWSEventProviderDownstream{
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_close_settlement_fails","status":"completed","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		ReceivedAt:   terminalReceivedAt,
	})
	actor.state = responsesWSStatePendingSend

	actor.close("test_close_buffered_settlement_fails")

	if attempt.Usage.PromptTokens != 3 || attempt.Usage.CompletionTokens != 4 || attempt.Usage.TotalTokens != 7 {
		t.Fatalf("expected terminal usage evidence to be projected before failed settlement, got %+v", attempt.Usage)
	}
	if !attempt.TerminalObserved || !attempt.CompletedAt.Equal(terminalReceivedAt) {
		t.Fatalf("expected terminal result/timestamp before settlement, terminal=%v completed=%s", attempt.TerminalObserved, attempt.CompletedAt)
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
	actor.SetPump(NewResponsesWSIOPump(&responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}, actor))
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
	actor.turns.pending.provider.journal.appendDownstreamFixture(ResponsesWSEventProviderDownstream{
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.failed","sequence_number":1,"response":{"id":"resp_error_settlement_fails","status":"failed","error":{"type":"usage_limit_reached","message":"monthly usage limit reached"}}}`)),
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
	actor.SetPump(NewResponsesWSIOPump(&responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt.TransportResult = responsesws.ResponsesWSTransportSendResult{
		Status: responsesws.ResponsesWSTransportSendAmbiguous,
		Err:    errors.New("ambiguous send"),
	}
	actor.turns.pending.attempt = attempt
	actor.turns.pending.provider.journal = responsesWSTestProviderFrameJournal()
	actor.turns.pending.provider.journal.appendDownstreamFixture(ResponsesWSEventProviderDownstream{
		ChannelID:    17,
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.failed","sequence_number":1,"response":{"id":"resp_limit","status":"failed","error":{"type":"usage_limit_reached","message":"monthly usage limit reached"}}}`)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	})
	actor.state = responsesWSStatePendingSend

	actor.close("test_close_buffered_error")

	select {
	case apiErr := <-errCh:
		if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Code != "provider_account_error" || !apiErr.ProviderQuotaExhausted {
			t.Fatalf("expected safe usage-limit provider error, got %#v", apiErr)
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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

	if !attempt.CompletedAt.IsZero() {
		t.Fatalf("provider receive failure must not be recorded as a response terminal, got %s", attempt.CompletedAt)
	}
	if !attempt.RolledBack {
		t.Fatal("expected buffered provider failure without usage to cancel the reservation")
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
	bridge := NewResponsesWSIOPump(nil, actor)
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

func TestResponsesWSProviderRecvPumpDoesNotEmitTimeoutAfterProviderErrorPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOPump(nil, actor)
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
	bridge := NewResponsesWSIOPump(nil, actor)
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
	bridge := NewResponsesWSIOPump(nil, actor)
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

func TestResponsesWSProviderRecvPumpEmitsProviderRecvFailedForProviderMalformed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOPump(nil, actor)
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
	bridge := NewResponsesWSIOPump(nil, actor)
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
	bridge := NewResponsesWSIOPump(nil, actor)
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
	bridge := NewResponsesWSIOPump(nil, actor)
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
	bridge := NewResponsesWSIOPump(nil, actor)
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
	bridge := NewResponsesWSIOPump(nil, actor)
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
	bridge := NewResponsesWSIOPump(conn, nil)
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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

	payload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_binary","status":"completed"}}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderBinaryFrame(payload),
		Usage:                     &types.UsageEvent{InputTokens: 1, OutputTokens: 2, TotalTokens: 3, ProviderTokenEvidence: true},
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
			payload: []byte(`{"type":"response.failed","sequence_number":1,"response":{"id":"resp_failed","status":"failed","error":{"type":"invalid_request_error","code":"bad_input","message":"bad input"},"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`),
		},
		{
			name:    "incomplete",
			payload: []byte(`{"type":"response.incomplete","sequence_number":1,"response":{"id":"resp_incomplete","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

			actor := NewResponsesWSSessionActor(ctx)
			bridge := NewResponsesWSIOPump(nil, actor)
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

func TestResponsesWSProviderDownstreamFrameWithUsageMergesBeforeWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
		Usage:                     &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6, ProviderTokenEvidence: true},
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

func TestResponsesWSRejectsOversizedImageIdentityBeforeProviderFrameDelivery(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "active"
		if pending {
			name = "pending"
		}
		t.Run(name, func(t *testing.T) {
			ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-image-limit-"+name)
			conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
			actor := NewResponsesWSSessionActor(ctx)
			actor.SetPump(NewResponsesWSIOPump(conn, actor))
			generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
			if pending {
				actor.turns.pending.attempt = attempt
				actor.state = responsesWSStatePendingSend
			} else {
				actor.turns.active.attempt = attempt
				actor.turns.active.channelID = 17
				actor.state = responsesWSStateInFlight
			}

			itemID := strings.Repeat("i", 257)
			payload := []byte(fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":1,"output_index":0,"item":{"id":%q,"type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}}`, itemID))
			actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
				AttemptID:                 attempt.AttemptID,
				UpstreamSessionGeneration: generation,
				ChannelID:                 17,
				Kind:                      ProviderDownstreamFrame,
				Frame:                     responsesWSTestProviderTextFrame(payload),
				DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
			})

			if !actor.closing.closed.Load() {
				t.Fatal("oversized image identity did not close Responses WebSocket session")
			}
			if got := atomic.LoadInt32(&conn.writeCount); got != 1 {
				t.Fatalf("writes=%d, want only one proxy-local error and no provider frame", got)
			}
			got, _ := conn.lastWrite.Load().(string)
			if !strings.Contains(got, "responses_ws_provider_usage_state_limit") || strings.Contains(got, itemID) {
				t.Fatalf("unexpected downstream payload after image state rejection: %q", got)
			}
			if len(attempt.Usage.ExtraBilling) != 0 {
				t.Fatalf("rejected image identity changed billing: %+v", attempt.Usage.ExtraBilling)
			}
		})
	}
}

func TestResponsesWSRejectsConflictingImageIdentityAsProviderProtocolError(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-image-conflict")
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight
	payload := []byte(`{"type":"response.output_item.done","sequence_number":1,"item_id":"img_top","output_index":0,"item":{"id":"img_item","type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID: attempt.AttemptID, UpstreamSessionGeneration: generation, ChannelID: 17,
		Kind: ProviderDownstreamFrame, Frame: responsesWSTestProviderTextFrame(payload), DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("conflicting image identity did not close Responses WebSocket session")
	}
	if got := atomic.LoadInt32(&conn.writeCount); got != 1 {
		t.Fatalf("writes=%d, want only one proxy-local error and no provider frame", got)
	}
	got, _ := conn.lastWrite.Load().(string)
	if !strings.Contains(got, "responses_ws_provider_protocol_error") || strings.Contains(got, "img_top") || strings.Contains(got, "img_item") {
		t.Fatalf("unexpected downstream payload after image identity conflict: %q", got)
	}
	if len(attempt.Usage.ExtraBilling) != 0 {
		t.Fatalf("conflicting image identity changed billing: %+v", attempt.Usage.ExtraBilling)
	}
}

func TestResponsesWSTerminalFrameWithResponseUsageAndAttachedUsageBillsOnce(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 100000, "attempt-terminal-usage-once")

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	payload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_usage_once","status":"completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame(payload),
		Usage: &types.UsageEvent{
			InputTokens: 4, OutputTokens: 2, TotalTokens: 6, ProviderTokenEvidence: true,
			ExtraBilling: map[string]types.ExtraBilling{
				types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high"): {
					ServiceType: types.APIToolTypeWebSearchPreview, Type: "high", CallCount: 1,
				},
			},
			ProviderExtraBilling: map[string]bool{
				types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high"): true,
			},
			BillingDiagnostics: map[string]bool{"stream_tool_evidence": true},
		},
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	})

	if attempt.Usage.PromptTokens != 4 || attempt.Usage.CompletionTokens != 2 || attempt.Usage.TotalTokens != 6 {
		t.Fatalf("expected terminal response usage and attached usage to bill once, got %+v", attempt.Usage)
	}
	toolKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")
	if attempt.Usage.ExtraBilling[toolKey].CallCount != 1 || attempt.TerminalUsage == nil || attempt.TerminalUsage.ExtraBilling[toolKey].CallCount != 1 {
		t.Fatalf("expected attached tool billing in both observed and exact terminal usage, observed=%+v terminal=%+v", attempt.Usage, attempt.TerminalUsage)
	}
	if !attempt.TerminalUsage.BillingDiagnostics["stream_tool_evidence"] {
		t.Fatalf("expected billing diagnostics in exact terminal usage, got %+v", attempt.TerminalUsage.BillingDiagnostics)
	}
	if actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected terminal to clear active turn, active=%+v state=%v", actor.turns.active.attempt, actor.state)
	}
}

func TestResponsesWSAttachedToolBillingAddsIncrementalCalls(t *testing.T) {
	usage := &types.Usage{}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	increment := &types.UsageEvent{
		ExtraBilling: map[string]types.ExtraBilling{
			key: {
				ServiceType: types.APIToolTypeWebSearchPreview,
				Type:        "medium",
				CallCount:   1,
			},
		},
		ProviderExtraBilling: map[string]bool{key: true},
	}
	mergeResponsesWSAttachedFrameUsage(usage, responsesws.ResponsesTerminalResult{}, increment)
	mergeResponsesWSAttachedFrameUsage(usage, responsesws.ResponsesTerminalResult{}, increment)
	if got := usage.ExtraBilling[key].CallCount; got != 2 {
		t.Fatalf("incremental tool call count=%d, want 2", got)
	}
}

func TestResponsesWSActiveTerminalDeduplicatesAttachedToolDelta(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 100000, "attempt-terminal-tool-delta")
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	payload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_tool_delta","status":"completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6},"tools":[{"type":"web_search_preview","search_context_size":"medium"}],"output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search"}}]}}`)
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame(payload),
		Usage: &types.UsageEvent{
			ExtraBilling: map[string]types.ExtraBilling{
				key: {ServiceType: types.APIToolTypeWebSearchPreview, Type: "medium", CallCount: 1},
			},
			ProviderExtraBilling: map[string]bool{key: true},
		},
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	})

	if got := attempt.Usage.ExtraBilling[key].CallCount; got != 1 {
		t.Fatalf("observed terminal tool count=%d, want 1", got)
	}
	if attempt.TerminalUsage == nil || attempt.TerminalUsage.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("exact terminal tool count must remain 1, terminal=%+v", attempt.TerminalUsage)
	}
}

func TestResponsesWSPendingProviderFrameWithUsageReplaysWithoutDoubleBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
		Usage:                     &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6, ProviderTokenEvidence: true},
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-pending-binary",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	payload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_binary_pending","status":"completed"}}`)
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend

	payload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_pending_usage","status":"completed"}}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame(payload),
		Usage: &types.UsageEvent{
			InputTokens:           3,
			OutputTokens:          4,
			TotalTokens:           7,
			ProviderTokenEvidence: true,
			ExtraBilling: map[string]types.ExtraBilling{
				types.APIToolTypeWebSearchPreview: {CallCount: 1},
			},
			ProviderExtraBilling: map[string]bool{types.APIToolTypeWebSearchPreview: true},
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

func TestResponsesWSPendingReplayDrainsOldLifecycleBeforeStartingQueuedTurn(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-pending-terminal-close")
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSTestSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.pending.attempt = attempt
	actor.state = responsesWSStatePendingSend
	queuedPayload := []byte(`{"type":"response.create","model":"gpt-5","store":false,"input":[]}`)
	if !actor.turns.queue.Push(responsesWSTestClientTextFrame(queuedPayload), responsesWSQueuedCreateMaxFrames, responsesWSQueuedCreateMaxBytes) {
		t.Fatal("expected next turn to enter the FIFO")
	}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_pending_close","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamClose,
		CloseCode:                 int(wsconn.CloseNormalClosure),
		CloseReason:               "bye",
		DetailOrigin:              responsesws.RecvDetailOriginNativeProviderClose,
	})

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         attempt.AttemptID,
		SelectedChannelID: 17,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAttempted,
		},
	})

	if !actor.closing.closed.Load() || atomic.LoadInt32(&conn.controlCount) != 1 {
		t.Fatalf("expected buffered close to end the session after terminal replay, closed=%v controls=%d", actor.closing.closed.Load(), atomic.LoadInt32(&conn.controlCount))
	}
	if len(actor.turns.queue.items) != 0 || actor.turns.pending.attempt != nil || actor.turns.active.attempt != nil {
		t.Fatalf("expected queued work to be discarded by the provider close, queue=%d pending=%+v active=%+v", len(actor.turns.queue.items), actor.turns.pending.attempt, actor.turns.active.attempt)
	}
}

func TestResponsesWSAmbiguousSendWithBufferedTerminalHasSingleTerminalResult(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-ambiguous-buffered-terminal")
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend

	payload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_ambiguous_buffered","status":"completed","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame(payload),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("ambiguous write after provider terminal"),
		},
	})

	if got := atomic.LoadInt32(&conn.writeCount); got != 1 {
		t.Fatalf("expected one provider terminal and no ambiguous proxy error, got %d writes", got)
	}
	if got, _ := conn.lastWrite.Load().(string); got != string(payload) || strings.Contains(got, "ambiguous_upstream_write") {
		t.Fatalf("expected the buffered provider terminal to be the only result, got %q", got)
	}
	if actor.closing.closed.Load() || actor.state != responsesWSStateIdle || actor.turns.active.attempt != nil || actor.turns.pending.attempt != nil {
		t.Fatalf("expected definitive terminal to keep the session reusable, closed=%v state=%v active=%+v pending=%+v", actor.closing.closed.Load(), actor.state, actor.turns.active.attempt, actor.turns.pending.attempt)
	}
	if !attempt.QuotaFinalized || attempt.RolledBack || attempt.Usage.TotalTokens != 7 {
		t.Fatalf("expected exact terminal settlement after ambiguous send, attempt=%+v usage=%+v", attempt, attempt.Usage)
	}
}

func TestResponsesWSAmbiguousBufferedTerminalFlushesDeferredInjectBeforeRelease(t *testing.T) {
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-ambiguous-buffered-inject")
	attempt.MultiAgentEnabled = true
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSCaptureSendSession{requests: make(chan responsesws.SendRequest, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	defer actor.finish()
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(session, 17)
	actor.turns.pending.attempt = attempt
	actor.turns.pending.phase = responsesWSPendingTurnSend
	actor.state = responsesWSStatePendingSend
	injectPayload := []byte(`{"type":"response.inject","event_id":"inject-buffered","input":{"text":"continue"}}`)
	actor.handleClientFrame(responsesWSTestClientTextFrame(injectPayload))
	if actor.turns.inject.pending != 1 || len(actor.turns.inject.deferred) != 1 {
		t.Fatalf("expected inject to wait for pending create admission, inject=%+v", actor.turns.inject)
	}

	terminalPayload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_ambiguous_inject","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame(terminalPayload),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})
	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		SelectedChannelID:         17,
		Purpose:                   ResponsesWSSendPurposeResponseCreate,
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendAmbiguous,
			Err:    errors.New("ambiguous write after provider terminal"),
		},
	})

	select {
	case request := <-session.requests:
		if string(request.Frame.Payload()) != string(injectPayload) {
			t.Fatalf("expected unchanged deferred inject, got %s", request.Frame.Payload())
		}
	case <-time.After(time.Second):
		t.Fatal("accepted deferred inject was not sent after terminal replay")
	}
	if actor.closing.closed.Load() || actor.turns.active.attempt != attempt || actor.state != responsesWSStateInFlight || actor.turns.inject.pending != 1 || !actor.turns.inject.terminalSeen {
		t.Fatalf("expected terminal to wait for deferred inject acknowledgement, closed=%v state=%v active=%+v inject=%+v", actor.closing.closed.Load(), actor.state, actor.turns.active.attempt, actor.turns.inject)
	}
	if got, _ := conn.lastWrite.Load().(string); strings.Contains(got, "ambiguous_upstream_write") {
		t.Fatalf("buffered terminal must not be followed by an ambiguous proxy error, got %q", got)
	}

	if !attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected buffered terminal to finalize quota exactly once, attempt=%+v", attempt)
	}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID: attempt.AttemptID, UpstreamSessionGeneration: generation, ChannelID: 17,
		Kind: ProviderDownstreamFrame, DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		Frame: responsesWSTestProviderTextFrame([]byte(`{"type":"response.inject.created","sequence_number":2,"response_id":"resp_ambiguous_inject"}`)),
	})
	if actor.closing.closed.Load() || actor.turns.active.attempt != nil || actor.state != responsesWSStateIdle || actor.turns.inject.pending != 0 {
		t.Fatalf("expected inject acknowledgement to release replayed terminal turn, closed=%v state=%v active=%+v inject=%+v", actor.closing.closed.Load(), actor.state, actor.turns.active.attempt, actor.turns.inject)
	}
}

func TestResponsesWSProviderUsageObservedOnlyUpdatesSettlementState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
		Usage:                     &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6, ProviderTokenEvidence: true},
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderUsageObserved(ResponsesWSEventProviderUsageObserved{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Usage:                     &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
		DetailOrigin:              responsesws.RecvDetailOriginProxyLocal,
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-transcription-priced",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	transcription := &types.UsageEvent{
		InputTokens:  4,
		TotalTokens:  4,
		Source:       types.UsageSourceInputAudioTranscription,
		BillingBasis: types.UsageBillingBasisTokens,
	}
	transcription.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 4)
	actor.handleProviderUsageObserved(ResponsesWSEventProviderUsageObserved{
		AttemptID:                 responsesWSTestCurrentAttemptID(actor),
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Usage:                     transcription,
		ReceivedAt:                time.Now(),
	})

	if metricCalls != 0 {
		t.Fatalf("expected priced transcription usage not to record unbilled metric, got %d calls", metricCalls)
	}
	if attempt.Usage.PromptTokens != 0 || attempt.Usage.TotalTokens != 0 || attempt.Usage.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != 4 {
		t.Fatalf("expected priced transcription usage to remain in its own settlement dimension, got %+v", attempt.Usage)
	}
}

func TestResponsesWSProviderUsageObservedWithoutTurnHasNoQuotaSemantics(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
	bridge := NewResponsesWSIOPump(nil, actor)
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

	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)
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

	mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 7, OutputTokens: 3, TotalTokens: 10, ProviderTokenEvidence: true})
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
			{Type: types.InputTypeWebSearchCall, ID: "ws_1", Status: "completed", Action: map[string]any{"type": "search"}},
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
		InputTokens:           3,
		OutputTokens:          5,
		TotalTokens:           8,
		ProviderTokenEvidence: true,
		ExtraBilling: map[string]types.ExtraBilling{
			types.APIToolTypeWebSearchPreview: {
				ServiceType: types.APIToolTypeWebSearchPreview,
				Type:        "high",
				CallCount:   1,
			},
		},
		ProviderExtraBilling: map[string]bool{types.APIToolTypeWebSearchPreview: true},
	})
	mergeResponsesWSTerminalResponse(usageWithProviderBilling, response)
	if got := usageWithProviderBilling.ExtraBilling[types.APIToolTypeWebSearchPreview].CallCount; got != 1 {
		t.Fatalf("expected provider and terminal tool billing to stay idempotent, got %d", got)
	}
}

func TestResponsesWSImageBillingUsesObservedPartialsInTerminalEvidence(t *testing.T) {
	attempt := &ResponsesWSTurnAttempt{Usage: &types.Usage{}}
	payloads := [][]byte{
		[]byte(`{"type":"response.created","response":{"status":"in_progress","tools":[{"type":"image_generation","model":"gpt-image-2","quality":"auto","size":"auto","partial_images":3}]}}`),
		[]byte(`{"type":"response.image_generation_call.partial_image","item_id":"img_1","output_index":0,"partial_image_index":0,"partial_image_b64":"preview-0"}`),
		[]byte(`{"type":"response.image_generation_call.partial_image","item_id":"img_1","output_index":0,"partial_image_index":0,"partial_image_b64":"preview-0-replay"}`),
		[]byte(`{"type":"response.image_generation_call.partial_image","item_id":"img_1","output_index":0,"partial_image_index":1,"partial_image_b64":"preview-1"}`),
		[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"img_1","type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}}`),
	}
	for _, payload := range payloads {
		attempt.ObserveResponsesStreamPayload(payload)
	}
	if len(attempt.Usage.ExtraBilling) != 0 {
		t.Fatalf("expected observed partials not to bill before the terminal, got %+v", attempt.Usage.ExtraBilling)
	}

	terminalPayload := []byte(`{"type":"response.completed","sequence_number":5,"response":{"id":"resp_img","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"tools":[{"type":"image_generation","model":"gpt-image-2","quality":"auto","size":"auto","partial_images":3}],"output":[{"id":"img_1","type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}]}}`)
	attempt.ObserveResponsesStreamPayload(terminalPayload)
	classified := responsesws.ClassifyResponsesWSEvent(terminalPayload)
	if classified.Kind != responsesws.ResponsesSuccessTerminal || classified.Response == nil {
		t.Fatalf("expected successful terminal classification, got %+v", classified)
	}
	mergeResponsesWSTerminalResponse(attempt.Usage, classified.Response, &attempt.imageGenerationTracker)
	attempt.MarkProviderTerminalEvidence(classified)

	actualKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-2|high|1024x1024|2")
	requestLimitKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-2|high|1024x1024|3")
	if attempt.Usage.ExtraBilling[actualKey].CallCount != 1 {
		t.Fatalf("expected WS usage to use two distinct observed partials, got %+v", attempt.Usage.ExtraBilling)
	}
	if _, exists := attempt.Usage.ExtraBilling[requestLimitKey]; exists {
		t.Fatalf("expected WS usage not to use request partial_images limit, got %+v", attempt.Usage.ExtraBilling)
	}
	if attempt.TerminalUsage == nil || attempt.TerminalUsage.ExtraBilling[actualKey].CallCount != 1 {
		t.Fatalf("expected exact terminal evidence to retain actual partial count, got %+v", attempt.TerminalUsage)
	}
}

func TestResponsesWSUsageDetailsMapAudioAndCacheFields(t *testing.T) {
	usage := &types.Usage{}
	mergeResponsesWSUsageEvent(usage, &types.UsageEvent{
		InputTokens:           1,
		ProviderTokenEvidence: true,
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
		InputTokens:           1,
		ProviderTokenEvidence: true,
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
	firstImageKey := " image_generation|high-1024x1024 "
	mergeResponsesWSUsageEvent(usage, &types.UsageEvent{
		ExtraBilling: map[string]types.ExtraBilling{
			types.APIToolTypeWebSearchPreview: {
				ServiceType: types.APIToolTypeWebSearchPreview,
				Type:        "high",
				CallCount:   1,
			},
			firstImageKey: {
				CallCount: 1,
			},
		},
		ProviderExtraBilling: map[string]bool{
			types.APIToolTypeWebSearchPreview: true,
			firstImageKey:                     true,
		},
	})
	imageKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "high-1024x1024")
	if usage.ExtraBilling[imageKey].CallCount != 1 || !usage.ProviderExtraBilling[imageKey] {
		t.Fatalf("trusted raw billing key was not normalized with its marker: %+v", usage)
	}
	mergeResponsesWSUsageEvent(usage, &types.UsageEvent{
		ExtraBilling: map[string]types.ExtraBilling{
			types.APIToolTypeWebSearchPreview: {
				ServiceType: types.APIToolTypeWebSearchPreview,
				Type:        "high",
				CallCount:   2,
			},
			imageKey: {
				CallCount: 2,
			},
		},
		ProviderExtraBilling: map[string]bool{
			types.APIToolTypeWebSearchPreview: true,
			imageKey:                          true,
		},
	})

	if got := usage.ExtraBilling[types.APIToolTypeWebSearchPreview].CallCount; got != 3 {
		t.Fatalf("expected repeated web search billing events to accumulate, got %d in %+v", got, usage.ExtraBilling)
	}
	if got := usage.ExtraBilling[imageKey].CallCount; got != 3 {
		t.Fatalf("expected image billing keys to normalize and accumulate, got %d in %+v", got, usage.ExtraBilling)
	}
}

func TestResponsesWSUsageMergeDoesNotAuthorizeEarlierUntrustedComponents(t *testing.T) {
	usage := &types.Usage{}
	unitKey := config.UsageExtraInputAudioTranscription
	toolKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	mergeResponsesWSUsageEvent(usage, &types.UsageEvent{
		InputTokens:     100,
		TotalTokens:     100,
		ExtraUsageUnits: map[string]float64{unitKey: 100},
		ExtraBilling: map[string]types.ExtraBilling{
			toolKey: {ServiceType: types.APIToolTypeWebSearchPreview, Type: "medium", CallCount: 100},
		},
	})
	mergeResponsesWSUsageEvent(usage, &types.UsageEvent{
		InputTokens:                   5,
		TotalTokens:                   5,
		ProviderTokenEvidence:         true,
		ExtraUsageUnits:               map[string]float64{unitKey: 2},
		ProviderIndependentUsageUnits: map[string]bool{unitKey: true},
		ExtraBilling:                  map[string]types.ExtraBilling{toolKey: {ServiceType: types.APIToolTypeWebSearchPreview, Type: "medium", CallCount: 1}},
		ProviderExtraBilling:          map[string]bool{toolKey: true},
	})

	if usage.PromptTokens != 5 || usage.TotalTokens != 5 || usage.ExtraUsageUnits[unitKey] != 2 || usage.ExtraBilling[toolKey].CallCount != 1 {
		t.Fatalf("later provider markers authorized earlier untrusted values: %+v", usage)
	}
}

func TestResponsesWSUsageClonePreservesNonIntegralBillingUnits(t *testing.T) {
	usage := &types.Usage{ExtraUsageUnits: map[string]float64{config.UsageExtraInputAudioTranscription: 2.5}}
	cloned := cloneResponsesWSUsage(usage)
	cloned.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] = 7
	if usage.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != 2.5 {
		t.Fatalf("usage clone shares non-integral units with source: %+v", usage.ExtraUsageUnits)
	}
}

func TestResponsesWSLocalStaleContinuationDoesNotClearProviderAffinity(t *testing.T) {
	if isProviderReportedContinuationMiss(responsesws.ErrStaleContinuation) {
		t.Fatal("expected local stale continuation guard not to clear provider affinity")
	}
	payload := string(responsesWSErrorFromErr(responsesws.ErrStaleContinuation))
	assertResponsesWSErrorPayload(t, payload, http.StatusBadRequest, "previous_response_not_found", "previous response was not found")
	if !strings.Contains(payload, `"param":"previous_response_id"`) {
		t.Fatalf("expected previous_response_id param in stale payload, got %q", payload)
	}

	providerPayload := string(responsesWSErrorFromOpenAI(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Code:    "previous_response_not_found",
			Message: "provider-specific stale response message",
		},
		StatusCode: http.StatusNotFound,
	}))
	assertResponsesWSErrorPayload(t, providerPayload, http.StatusBadRequest, "previous_response_not_found", "previous response was not found")
	if !strings.Contains(providerPayload, `"param":"previous_response_id"`) {
		t.Fatalf("expected normalized provider miss param, got %q", providerPayload)
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
	if apiErr := attempt.ClaimSubmission(); apiErr != nil {
		t.Fatalf("expected submission claim to succeed, got %v", apiErr)
	}
	attempt.AttemptID = "attempt-settlement-log"
	attempt.MarkFirstProviderResponse(firstResponseAt)

	actor := NewResponsesWSSessionActor(ctx)
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	bridge := NewResponsesWSIOPump(conn, actor)
	actor.SetPump(bridge)
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:    responsesWSTestCurrentAttemptID(actor),
		Kind:         ProviderDownstreamFrame,
		Frame:        responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_done","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`)),
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

func configureResponsesWSTokenPricingFloor(t *testing.T, floor int) {
	t.Helper()
	originalPreConsumedQuota := config.PreConsumedQuota
	config.PreConsumedQuota = floor
	head, err := model.ReadPublicationVersion(context.Background(), model.DB, model.PublicationOwnerPrice)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.PricingInstance.UpdatePriceAtVersion("gpt-5", &model.Price{Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1}, true, head); err != nil {
		t.Fatalf("publish token price fixture: %v", err)
	}
	t.Cleanup(func() {
		config.PreConsumedQuota = originalPreConsumedQuota
	})
}

func TestResponsesWSApplySettlementDecisionUsesCurrentPolicyAtApply(t *testing.T) {
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
		_, applied, err := actor.applyActiveSettlement()
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
		decision, applied, err := actor.applyActiveSettlement()
		if err != nil {
			t.Fatalf("expected exact zero settlement to succeed, got %v decision=%+v", err, decision)
		}
		if decision.Action != ResponsesWSSettlementFinalizeExactUsage {
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
		mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 10, TotalTokens: 10, Source: types.UsageSourceResponsesResponse, ProviderTokenEvidence: true})
		response := &types.OpenAIResponsesResponses{
			ID:     "resp_exact_zero_after_observed",
			Status: types.ResponseStatusCompleted,
			Usage:  &types.ResponsesUsage{},
		}
		mergeResponsesWSTerminalResponse(attempt.Usage, response)
		attempt.MarkProviderTerminalEvidence(responsesws.ClassifyResponsesWSTerminal("response.completed", response, false))

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		decision, applied, err := actor.applyActiveSettlement()
		if err != nil {
			t.Fatalf("expected exact zero settlement to succeed, got %v decision=%+v", err, decision)
		}
		if applied.AppliedFinalQuota != 0 {
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
		mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 10, OutputTokens: 90, TotalTokens: 100, ProviderTokenEvidence: true})
		response := &types.OpenAIResponsesResponses{
			ID:     "resp_exact_ten_after_observed",
			Status: types.ResponseStatusCompleted,
			Usage:  &types.ResponsesUsage{InputTokens: 10, TotalTokens: 10},
		}
		mergeResponsesWSTerminalResponse(attempt.Usage, response)
		attempt.MarkProviderTerminalEvidence(responsesws.ClassifyResponsesWSTerminal("response.completed", response, false))

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		decision, applied, err := actor.applyActiveSettlement()
		if err != nil {
			t.Fatalf("expected exact smaller settlement to succeed, got %v decision=%+v", err, decision)
		}
		if applied.AppliedFinalQuota != 10 {
			t.Fatalf("expected terminal snapshot quota 10 to win over observed usage, decision=%+v applied=%+v", decision, applied)
		}
		log := readResponsesWSConsumeLog(t)
		if log.Quota != 10 || log.PromptTokens != 10 || log.CompletionTokens != 0 {
			t.Fatalf("expected exact consume log to use terminal snapshot, got %+v", log)
		}
	})

	t.Run("terminal positive tokens on zero price model refunds to zero", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		originalPreConsumedQuota := config.PreConsumedQuota
		head, err := model.ReadPublicationVersion(context.Background(), model.DB, model.PublicationOwnerPrice)
		if err != nil {
			t.Fatal(err)
		}
		if err := model.PricingInstance.UpdatePriceAtVersion("gpt-5", &model.Price{Model: "gpt-5", Type: model.TokensPriceType}, true, head); err != nil {
			t.Fatal(err)
		}
		config.PreConsumedQuota = 100
		t.Cleanup(func() {
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
		decision, applied, err := actor.applyActiveSettlement()
		if err != nil {
			t.Fatalf("expected zero-price terminal settlement to succeed, got %v decision=%+v", err, decision)
		}
		if decision.Action != ResponsesWSSettlementFinalizeExactUsage {
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

	t.Run("terminal usage reads price at settlement apply", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		response := &types.OpenAIResponsesResponses{
			ID:     "resp_price_changed_before_apply",
			Status: types.ResponseStatusCompleted,
			Usage:  &types.ResponsesUsage{InputTokens: 10, TotalTokens: 10},
		}
		mergeResponsesWSTerminalResponse(attempt.Usage, response)
		attempt.MarkProviderTerminalEvidence(responsesws.ClassifyResponsesWSTerminal("response.completed", response, false))

		head, err := model.ReadPublicationVersion(context.Background(), model.DB, model.PublicationOwnerPrice)
		if err != nil {
			t.Fatal(err)
		}
		if err := model.PricingInstance.UpdatePriceAtVersion("gpt-5", &model.Price{Model: "gpt-5", Type: model.TokensPriceType, Input: 2, Output: 2}, true, head); err != nil {
			t.Fatalf("publish changed price: %v", err)
		}

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		decision, applied, err := actor.applyActiveSettlement()
		if err != nil {
			t.Fatalf("settle with current price: %v", err)
		}
		if decision.Action != ResponsesWSSettlementFinalizeExactUsage || applied.AppliedFinalQuota != 20 {
			t.Fatalf("settlement did not use apply-time price: decision=%+v applied=%+v", decision, applied)
		}
	})

	t.Run("observed usage settles to current exact price", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 10, TotalTokens: 10, Source: types.UsageSourceResponsesResponse, ProviderTokenEvidence: true})
		const finalQuota int64 = 10

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		_, applied, err := actor.applyActiveSettlement()
		if err != nil {
			t.Fatalf("expected observed settlement to succeed, got %v", err)
		}
		if applied.AppliedFinalQuota != finalQuota {
			t.Fatalf("expected observed provider usage quota %d, got %d", finalQuota, applied.AppliedFinalQuota)
		}
		user, token := readResponsesWSQuotaFixture(t)
		if user.Quota != 1000-int(finalQuota) || user.UsedQuota != int(finalQuota) || token.RemainQuota != 1000-int(finalQuota) || token.UsedQuota != int(finalQuota) {
			t.Fatalf("expected provider usage final quota, user=%+v token=%+v", user, token)
		}
		log := readResponsesWSConsumeLog(t)
		if log.Quota != int(finalQuota) || log.PromptTokens != 10 || log.CompletionTokens != 0 {
			t.Fatalf("expected observed usage log, got %+v", log)
		}
	})

	t.Run("tool-only provider evidence reaches shared component settlement", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 100000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		attempt.Usage.SetProviderExtraBilling(types.APIToolTypeWebSearchPreview, "medium", 1)

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		decision, applied, err := actor.applyActiveSettlement()
		if err != nil {
			t.Fatalf("settle tool-only provider evidence: %v", err)
		}
		if decision.Action != ResponsesWSSettlementFinalizeProviderUsage || applied.AppliedFinalQuota <= 0 || !attempt.QuotaFinalized || attempt.RolledBack {
			t.Fatalf("tool-only evidence was not confirmed by shared reducer: decision=%+v applied=%+v attempt=%+v", decision, applied, attempt)
		}
	})

	t.Run("transcription-only provider evidence remains an independent component", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		extraRatios := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudioTranscription: 1})
		head, err := model.ReadPublicationVersion(context.Background(), model.DB, model.PublicationOwnerPrice)
		if err != nil {
			t.Fatal(err)
		}
		if err := model.PricingInstance.UpdatePriceAtVersion("gpt-5", &model.Price{Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1, ExtraRatios: &extraRatios}, true, head); err != nil {
			t.Fatal(err)
		}
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		transcription := &types.UsageEvent{Source: types.UsageSourceInputAudioTranscription, BillingBasis: types.UsageBillingBasisTokens}
		transcription.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 4)
		mergeResponsesWSUsageEvent(attempt.Usage, transcription)
		if attempt.Usage.ProviderReported || attempt.Usage.HasProviderUsage() || !attempt.Usage.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription] {
			t.Fatalf("transcription evidence leaked into the token component or lost its independent marker: %+v", attempt.Usage)
		}

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		_, applied, err := actor.applyActiveSettlement()
		if err != nil {
			t.Fatalf("settle transcription-only provider evidence: %v", err)
		}
		if applied.AppliedFinalQuota != 4 || !attempt.QuotaFinalized || attempt.RolledBack {
			t.Fatalf("independent transcription component was not confirmed exactly: applied=%+v attempt=%+v", applied, attempt)
		}
	})

	t.Run("terminal tokens retain earlier independent transcription units", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		extraRatios := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudioTranscription: 1})
		head, err := model.ReadPublicationVersion(context.Background(), model.DB, model.PublicationOwnerPrice)
		if err != nil {
			t.Fatal(err)
		}
		if err := model.PricingInstance.UpdatePriceAtVersion("gpt-5", &model.Price{Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1, ExtraRatios: &extraRatios}, true, head); err != nil {
			t.Fatal(err)
		}
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		transcription := &types.UsageEvent{Source: types.UsageSourceInputAudioTranscription, BillingBasis: types.UsageBillingBasisTokens}
		transcription.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 4)
		mergeResponsesWSUsageEvent(attempt.Usage, transcription)
		response := &types.OpenAIResponsesResponses{
			ID:     "resp_tokens_and_transcription",
			Status: types.ResponseStatusCompleted,
			Usage:  &types.ResponsesUsage{InputTokens: 10, TotalTokens: 10},
		}
		mergeResponsesWSTerminalResponse(attempt.Usage, response)
		attempt.MarkProviderTerminalEvidence(responsesws.ClassifyResponsesWSTerminal("response.completed", response, false))

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		_, applied, err := actor.applyActiveSettlement()
		if err != nil {
			t.Fatalf("settle terminal tokens with independent transcription: %v", err)
		}
		if attempt.TerminalUsage == nil || !attempt.TerminalUsage.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription] || applied.AppliedFinalQuota != 14 {
			t.Fatalf("terminal snapshot erased the independent component: terminal=%+v applied=%+v", attempt.TerminalUsage, applied)
		}
	})

	t.Run("terminal attribution conflict cannot charge dependent tokens", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{
			InputTokens: 3, TotalTokens: 3, Source: types.UsageSourceResponsesResponse, ResponseModel: "gpt-other", ProviderTokenEvidence: true,
		})
		response := &types.OpenAIResponsesResponses{
			ID: "resp_conflicting_model", Model: "gpt-5", Status: types.ResponseStatusCompleted,
			Usage: &types.ResponsesUsage{InputTokens: 10, TotalTokens: 10},
		}
		mergeResponsesWSTerminalResponse(attempt.Usage, response)
		attempt.MarkProviderTerminalEvidence(responsesws.ClassifyResponsesWSTerminal("response.completed", response, false))

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		_, applied, err := actor.applyActiveSettlement()
		if err != nil {
			t.Fatalf("settle conflicting attribution: %v", err)
		}
		if attempt.TerminalUsage == nil || !attempt.TerminalUsage.AttributionConflict || applied.AppliedFinalQuota != 0 || !attempt.RolledBack {
			t.Fatalf("conflicting attribution charged a dependent token component: terminal=%+v applied=%+v attempt=%+v", attempt.TerminalUsage, applied, attempt)
		}
	})

	t.Run("no provider usage cancels", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.active.attempt = attempt
		_, applied, err := actor.applyActiveSettlement()
		if err != nil {
			t.Fatalf("expected floor settlement to succeed, got %v", err)
		}
		if applied.AppliedFinalQuota != 0 || !attempt.RolledBack {
			t.Fatalf("expected missing usage to cancel, applied=%+v attempt=%+v", applied, attempt)
		}
		user, token := readResponsesWSQuotaFixture(t)
		if user.Quota != 1000 || token.RemainQuota != 1000 {
			t.Fatalf("expected reservation refund, user=%+v token=%+v", user, token)
		}
	})

	t.Run("zero proof rolls back reserve", func(t *testing.T) {
		ctx := setupResponsesWSQuotaFixture(t, 1000)
		configureResponsesWSTokenPricingFloor(t, 100)
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)

		actor := NewResponsesWSSessionActor(ctx)
		actor.turns.pending.attempt = attempt
		_, applied, err := actor.applyPendingSettlement()
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

func TestResponsesWSSettlementFailureDoesNotRecordAppliedOutcome(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 10, TotalTokens: 10, Source: types.UsageSourceResponsesResponse, ProviderTokenEvidence: true})
	sqlDB, err := model.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	actor := NewResponsesWSSessionActor(ctx)
	actor.turns.active.attempt = attempt
	_, applied, err := actor.applyActiveSettlement()
	if err == nil {
		t.Fatal("expected settlement against a closed database to fail")
	}
	if applied.AppliedFinalQuota != 0 || attempt.AppliedSettlement != nil || attempt.QuotaFinalized || attempt.RolledBack || !attempt.QuotaPreconsumed {
		t.Fatalf("failed settlement recorded a successful accounting outcome: applied=%+v attempt=%+v", applied, attempt)
	}
}

func TestResponsesWSApplySettlementDecisionFinalGuardUsesStoredApplied(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	decision := ResponsesWSSettlementDecision{
		Action: ResponsesWSSettlementFinalizeExactUsage,
	}
	first, err := attempt.ApplyResponsesWSSettlementDecision(ctx, decision)
	if err != nil {
		t.Fatalf("expected first settlement to succeed, got %v", err)
	}
	second, err := attempt.ApplyResponsesWSSettlementDecision(ctx, decision)
	if err != nil {
		t.Fatalf("expected duplicate same settlement to be idempotent, got %v", err)
	}
	if second != first {
		t.Fatalf("expected duplicate settlement to return stored applied, first=%+v second=%+v", first, second)
	}
	decision.Action = ResponsesWSSettlementRollbackReserve
	third, err := attempt.ApplyResponsesWSSettlementDecision(ctx, decision)
	if err != nil || third != first {
		t.Fatalf("expected final guard to return the first result, third=%+v err=%v", third, err)
	}
}

func TestResponsesWSApplySettlementDecisionDuplicateRollbackUsesStoredApplied(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	configureResponsesWSTokenPricingFloor(t, 100)
	attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
	decision := ResponsesWSSettlementDecision{
		Action: ResponsesWSSettlementRollbackReserve,
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

func TestResponsesWSSettlementRequiresBillingAttempt(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	actor := NewResponsesWSSessionActor(ctx)
	actor.turns.active.attempt = &ResponsesWSTurnAttempt{
		AttemptID: "attempt-invalid-settlement",
		Usage:     &types.Usage{},
	}

	_, _, err := actor.applyActiveSettlement()
	if err == nil {
		t.Fatal("expected invalid settlement to fail")
	}
}

func TestResponsesWSActiveSettlementFailureStopsTerminalSideEffects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-terminal-settlement-fails",
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
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_no_settle","status":"completed","usage":{"input_tokens":1,"total_tokens":1}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected settlement failure to close session")
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected terminal success side effects not to run, lastFinal=%+v", actor.turns.history.lastFinal)
	}
	if actor.turns.active.attempt != nil || attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected failed settlement to clear actor state without changing quota, active=%v attempt=%+v", actor.turns.active.attempt, attempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "response.completed") || strings.Contains(got, "quota_settlement_failed") {
		t.Fatalf("expected provider terminal to remain the final data event when settlement fails, got %q", got)
	}
}

func TestResponsesWSActiveTerminalNilQuotaFailsBeforeSideEffects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_nil_quota","status":"completed","usage":{"input_tokens":1,"total_tokens":1}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})

	if !actor.closing.closed.Load() {
		t.Fatal("expected nil quota settlement failure to close session")
	}
	if actor.turns.history.lastFinal != nil {
		t.Fatalf("expected nil quota terminal side effects not to run, lastFinal=%+v", actor.turns.history.lastFinal)
	}
	if actor.turns.active.attempt != nil || attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("expected nil quota settlement failure to clear actor state without changing quota, active=%v attempt=%+v", actor.turns.active.attempt, attempt)
	}
}

func TestResponsesWSPreconsumeDeadlineStopsBlockingDBBeforeProviderSend(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	originalTimeout := responsesLifecycleIOTimeout
	responsesLifecycleIOTimeout = 20 * time.Millisecond
	t.Cleanup(func() { responsesLifecycleIOTimeout = originalTimeout })

	blocked := make(chan struct{})
	release := make(chan struct{})
	var blockedOnce sync.Once
	const callbackName = "test:responses_ws_preconsume_deadline"
	if err := model.DB.Callback().Query().Before("gorm:query").Register(callbackName, func(db *gorm.DB) {
		blockedOnce.Do(func() { close(blocked) })
		select {
		case <-db.Statement.Context.Done():
			db.AddError(db.Statement.Context.Err())
		case <-release:
			db.AddError(errors.New("test released blocked preconsume query"))
		}
	}); err != nil {
		t.Fatalf("register blocking query callback: %v", err)
	}
	t.Cleanup(func() {
		close(release)
		_ = model.DB.Callback().Query().Remove(callbackName)
	})

	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":[]}`))
	if err != nil {
		t.Fatalf("parse first frame: %v", err)
	}
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSCaptureSendSession{}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.ReserveFirstTurnOpening(frame)

	done := make(chan struct{})
	startedAt := time.Now()
	go func() {
		actor.prepareAndSendFirstTurn(&responsesWSOpenResult{
			Session:       session,
			ProviderModel: "gpt-5",
			BillingModel:  "gpt-5",
			Channel:       &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Models: "gpt-5"},
			Candidate:     &ResponsesTurnAffinity{},
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocking preconsume did not honor the ResponsesWS lifecycle deadline")
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded preconsume took too long: %s", elapsed)
	}
	select {
	case <-blocked:
	default:
		t.Fatal("expected preconsume to reach the blocking database query")
	}
	if calls := atomic.LoadInt32(&session.calls); calls != 0 {
		t.Fatalf("provider send must not start after preconsume timeout, got %d sends", calls)
	}
	if !actor.closing.closed.Load() {
		t.Fatal("expected preconsume timeout to fail closed")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "billing_admission_unavailable") {
		t.Fatalf("expected bounded billing admission error, got %q", got)
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

func TestResponsesWSSameSessionTwoMessagesAccumulateQuota(t *testing.T) {
	ctx := setupResponsesWSQuotaFixture(t, 1000)
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.upstream.channelID = 17

	for i, responseID := range []string{"resp_one", "resp_two"} {
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		attempt.AttemptID = fmt.Sprintf("attempt-%s-%d", strings.ReplaceAll(t.Name(), "/", "_"), i+1)
		actor.turns.active.attempt = attempt
		actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
		actor.turns.active.channelID = 17
		actor.state = responsesWSStateInFlight
		actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
			AttemptID: responsesWSTestCurrentAttemptID(actor),
			ChannelID: 17,
			Kind:      ProviderDownstreamFrame,
			Frame: responsesWSTestProviderTextFrame([]byte(fmt.Sprintf(
				`{"type":"response.completed","sequence_number":1,"response":{"id":%q,"status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_old","status":"completed"}}`)),
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
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_old","status":"completed"}}`)),
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
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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

func TestResponsesWSInjectFailedMayReferenceRejectedTargetID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{AttemptID: "attempt-inject-target", SelectedChannelID: 17, Usage: &types.Usage{}}
	actor.turns.active.attempt = attempt
	actor.turns.active.channelID = 17
	actor.turns.inject.AddPending([]byte(`{"type":"response.inject","response_id":"resp_missing","input":[]}`))
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID: attempt.AttemptID, UpstreamSessionGeneration: generation, ChannelID: 17,
		Kind: ProviderDownstreamFrame, DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		Frame: responsesWSTestProviderTextFrame([]byte(`{"type":"response.created","response":{"id":"resp_current","status":"in_progress"}}`)),
	})
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID: attempt.AttemptID, UpstreamSessionGeneration: generation, ChannelID: 17,
		Kind: ProviderDownstreamFrame, DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
		Frame: responsesWSTestProviderTextFrame([]byte(`{"type":"response.inject.failed","response_id":"resp_missing","error":{"code":"response_not_found"},"input":[]}`)),
	})

	if actor.closing.closed.Load() || actor.turns.inject.pending != 0 || attempt.SeenProviderResponseID != "resp_current" {
		t.Fatalf("valid inject failure acknowledgement was rejected: closed=%v pending=%d attempt=%+v", actor.closing.closed.Load(), actor.turns.inject.pending, attempt)
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

func TestResponsesWSUnknownSendPurposeIsDiagnosticOnly(t *testing.T) {
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

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         "attempt-current",
		SelectedChannelID: 17,
		Purpose:           ResponsesWSSendPurpose("future_purpose"),
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendNotAttempted,
			Err:    responsesws.ErrUpstreamClosed,
		},
	})

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

func TestResponsesWSProviderMalformedRecvFailureClosesIdleSessionWithPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
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

	setupRelayTestDB(t, &model.User{}, &model.Token{}, &model.Price{}, &model.ModelInfo{}, &model.PublicationVersion{})
	if err := model.EnsurePublicationVersionRows(model.DB); err != nil {
		t.Fatal(err)
	}

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
	if err := model.DB.Create(&model.Price{Model: "gpt-5", Type: model.TimesPriceType, Input: 0.1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatal(err)
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
