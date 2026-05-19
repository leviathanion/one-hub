package relay

import (
	"context"
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
	"one-api/common/responsesws"
	"one-api/middleware"
	"one-api/relay/relay_util"
	runtimeaffinity "one-api/runtime/channelaffinity"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/model"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/spf13/viper"
	"go.uber.org/zap"
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
	reads        chan responsesWSReadResult
	readLimit    int64
	writeErr     error
	writeCount   int32
	closeCount   int32
	controlCount int32
	lastWrite    atomic.Value
	lastControl  atomic.Value
}

func (c *responsesWSFakeUserConn) ReadMessage() (int, []byte, error) {
	result := <-c.reads
	return result.messageType, result.payload, result.err
}

func (c *responsesWSFakeUserConn) WriteMessage(_ int, payload []byte) error {
	atomic.AddInt32(&c.writeCount, 1)
	if c.writeErr != nil {
		return c.writeErr
	}
	c.lastWrite.Store(string(payload))
	return nil
}

func (c *responsesWSFakeUserConn) SetReadLimit(limit int64) {
	c.readLimit = limit
}

func (c *responsesWSFakeUserConn) SetReadDeadline(time.Time) error  { return nil }
func (c *responsesWSFakeUserConn) SetWriteDeadline(time.Time) error { return nil }
func (c *responsesWSFakeUserConn) Close() error {
	atomic.AddInt32(&c.closeCount, 1)
	return nil
}
func (c *responsesWSFakeUserConn) WriteControl(_ int, payload []byte, _ time.Time) error {
	atomic.AddInt32(&c.controlCount, 1)
	c.lastControl.Store(string(payload))
	return nil
}

type responsesWSTestProxyLocalReadError struct {
	payload     []byte
	recoverable bool
}

func (e responsesWSTestProxyLocalReadError) Error() string {
	return "proxy-local read error"
}

func (e responsesWSTestProxyLocalReadError) ProxyLocalPayload() []byte {
	return e.payload
}

func (e responsesWSTestProxyLocalReadError) Recoverable() bool {
	return e.recoverable
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

func intSliceContains(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type responsesWSTestSession struct {
	abortReason string
	abortCh     chan string
	abortCount  int32
}

func (s *responsesWSTestSession) SendClient(context.Context, int, []byte) error { return nil }

func (s *responsesWSTestSession) Recv(context.Context) (int, []byte, *types.UsageEvent, runtimesession.RealtimePayloadOrigin, error) {
	return 0, nil, nil, runtimesession.RealtimePayloadOriginProxyLocal, runtimesession.ErrSessionClosed
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

func (s *responsesWSTestSession) SetTurnObserverFactory(runtimesession.TurnObserverFactory) {}

type responsesWSRecvResult struct {
	messageType int
	payload     []byte
	usage       *types.UsageEvent
	origin      runtimesession.RealtimePayloadOrigin
	err         error
}

type responsesWSRecvSequenceSession struct {
	responses chan responsesWSRecvResult
}

func (s *responsesWSRecvSequenceSession) SendClient(context.Context, int, []byte) error {
	return nil
}

func (s *responsesWSRecvSequenceSession) Recv(ctx context.Context) (int, []byte, *types.UsageEvent, runtimesession.RealtimePayloadOrigin, error) {
	select {
	case result := <-s.responses:
		origin := result.origin
		if origin == runtimesession.RealtimePayloadOriginProxyLocal && result.err == nil && len(result.payload) > 0 {
			origin = runtimesession.RealtimePayloadOriginProvider
		}
		return result.messageType, result.payload, result.usage, origin, result.err
	case <-ctx.Done():
		return 0, nil, nil, runtimesession.RealtimePayloadOriginProxyLocal, ctx.Err()
	}
}

func (s *responsesWSRecvSequenceSession) Detach(string) {}
func (s *responsesWSRecvSequenceSession) Abort(string)  {}
func (s *responsesWSRecvSequenceSession) SetTurnObserverFactory(runtimesession.TurnObserverFactory) {
}

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

var responsesWSConnectionAttemptTokenSeq int64 = 91000

func nextResponsesWSConnectionAttemptTokenID() int {
	return int(atomic.AddInt64(&responsesWSConnectionAttemptTokenSeq, 1))
}

func setResponsesWSTestViperInt(t *testing.T, key string, value int) {
	t.Helper()
	previous := viper.Get(key)
	viper.Set(key, value)
	t.Cleanup(func() {
		viper.Set(key, previous)
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
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}); err != nil {
		t.Fatalf("expected quota settlement schema migration to succeed, got %v", err)
	}
	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
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
	return attempt
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

	firstConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("expected first websocket connection to upgrade, got %v", err)
	}

	_, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected second websocket connection to be rejected by connection limiter")
	}
	if response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 from connection limiter, got response=%v err=%v", response, err)
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "too many responses websocket connection attempts") {
		t.Fatalf("expected connection limiter response before pending acquisition, body=%q", string(body))
	}
	_ = firstConn.Close()
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

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("expected websocket connection to upgrade, got %v", err)
	}
	defer func() {
		_ = conn.Close()
		select {
		case <-handlerDone:
		case <-time.After(time.Second):
			t.Errorf("timed out waiting for oversized websocket handler to exit")
		}
	}()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", 1024))); err != nil {
		t.Fatalf("expected oversized first frame write to reach server, got %v", err)
	}
	_, payload, err := conn.ReadMessage()
	if err == nil {
		if !strings.Contains(string(payload), "invalid_event") {
			t.Fatalf("expected oversized frame guidance payload, got %q", payload)
		}
		return
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("expected oversized frame close or invalid_event payload, err=%v payload=%q", err, payload)
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
	openAndPrimeResponsesWSSessionForActor = func(context.Context, *gin.Context, *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
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
	if !actor.closed.Load() {
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
	openAndPrimeResponsesWSSessionForActor = func(context.Context, *gin.Context, *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
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
	actor.Start()
	actor.startFirstTurnOpenWorker(actor.openingID, frame)

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
	if actor.session != nil || actor.upstreamSessionGeneration != "" {
		t.Fatalf("expected closed actor not to adopt opened session, upstreamSessionGeneration=%q session=%#v", actor.upstreamSessionGeneration, actor.session)
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
	openAndPrimeResponsesWSSessionForActor = func(context.Context, *gin.Context, *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
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
	if !actor.closed.Load() {
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
	if !actor.closed.Load() {
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
	actor.activeLease = lease
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
	actor.activeLease = lease
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
			err:  &websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "unexpected EOF"},
			want: true,
		},
		{
			name: "normal close",
			err:  &websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "bye"},
			want: true,
		},
		{
			name: "raw unexpected eof",
			err:  io.ErrUnexpectedEOF,
			want: true,
		},
		{
			name: "message too big remains visible",
			err:  &websocket.CloseError{Code: websocket.CloseMessageTooBig, Text: "too large"},
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
	if !actor.closed.Load() {
		t.Fatal("expected actor to close after first-turn rewrite failure")
	}
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
	openAndPrimeResponsesWSSessionForActor = func(ctx context.Context, _ *gin.Context, _ *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
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
	actor.firstTurnAdmission = admission
	actor.session = &responsesWSTestSession{}
	actor.upstreamSessionGeneration = "old-session"
	actor.sessionChannelID = 17
	actor.providerRecvArmed = true
	actor.pendingAttempt = &ResponsesWSTurnAttempt{
		OpeningID:         openingID,
		AttemptID:         "attempt-old",
		Admission:         admission,
		Candidate:         &ResponsesTurnAffinity{},
		SelectedChannelID: 17,
		Session:           actor.session,
		snapshot:          NewResponsesWSRequestSnapshot(ctx),
	}
	actor.state = responsesWSStatePendingSend
	actor.Start()

	if !actor.Post(ResponsesWSEventSendResult{
		AttemptID:         "attempt-old",
		SelectedChannelID: 17,
		Outcome:           SendOutcomeNotSent,
		Err:               runtimesession.ErrSessionClosed,
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

	upgraded := make(chan *websocket.Conn, 1)
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
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
	selfHostedOther := `{"responses_ws_self_hosted":true}`
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
		_ = conn.Close()
	case <-time.After(time.Second):
		t.Fatalf("expected fallback websocket server to be used")
	}
	skipped, _ := ctx.Get("skip_channel_ids")
	skippedIDs, _ := skipped.([]int)
	if !intSliceContains(skippedIDs, preferredChannelID) {
		t.Fatalf("expected unsupported non-strict preferred channel to be skipped, got %#v", skipped)
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
	selfHostedOther := `{"responses_ws_self_hosted":true}`
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
	selfHostedOther := `{"responses_ws_self_hosted":true}`
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
	if actor.closed.Load() {
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

func TestResponsesWSSendResultReliableWhenMailboxFull(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	bridge := NewResponsesWSIOBridge(nil, actor)
	session := &responsesWSTestSession{}
	for i := 0; i < cap(actor.events); i++ {
		actor.events <- ResponsesWSEventTimeout{Reason: "preloaded"}
	}

	done := make(chan struct{})
	go func() {
		bridge.handleSendCommand(responsesWSSendCommand{
			AttemptID:         "attempt-reliable",
			SelectedChannelID: 17,
			Session:           session,
			MessageType:       websocket.TextMessage,
			Payload:           []byte(`{"type":"response.cancel"}`),
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
	if sendResult.AttemptID != "attempt-reliable" || sendResult.Outcome != SendOutcomeLocalWriteOK {
		t.Fatalf("unexpected send result: %+v", sendResult)
	}
}

func TestResponsesWSSendQueueFullPostsNotSentEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-queue-full",
		SelectedChannelID: 17,
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.pendingAttempt = attempt
	actor.state = responsesWSStatePendingSend

	actor.postSendQueueFull(attempt.AttemptID, attempt.SelectedChannelID)
	if attempt.RolledBack || actor.pendingAttempt == nil {
		t.Fatal("expected send queue full to wait for actor event handling before mutating attempt state")
	}

	event := readResponsesWSEvent(t, actor)
	sendResult, ok := event.(ResponsesWSEventSendResult)
	if !ok {
		t.Fatalf("expected send result event, got %T", event)
	}
	if sendResult.Outcome != SendOutcomeNotSent || !errors.Is(sendResult.Err, errResponsesWSSendQueueFull) {
		t.Fatalf("unexpected send queue full result: %+v", sendResult)
	}

	actor.handleEvent(sendResult)
	if !attempt.RolledBack || actor.pendingAttempt != nil {
		t.Fatalf("expected actor event handling to rollback queue-full attempt, rolled=%v pending=%+v", attempt.RolledBack, actor.pendingAttempt)
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
	if actor.closed.Load() {
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
	if !actor.closed.Load() {
		t.Fatal("expected actor loop close intent handling to close session")
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

func TestResponsesWSClientReadPumpProxyLocalReadErrorDelivery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 3)}
	bridge := NewResponsesWSIOBridge(conn, actor)
	defer bridge.Close()

	if conn.readLimit != config.RealtimeWebsocketReadLimit() {
		t.Fatalf("expected bridge to install read limit before reads, got %d", conn.readLimit)
	}

	bridge.StartClientReadPump()
	conn.reads <- responsesWSReadResult{err: responsesWSTestProxyLocalReadError{
		payload:     []byte(`{"type":"error","error":{"code":"invalid_event"}}`),
		recoverable: true,
	}}
	event := readResponsesWSEvent(t, actor)
	localErr, ok := event.(ResponsesWSEventProxyLocalError)
	if !ok {
		t.Fatalf("expected proxy-local error event, got %T", event)
	}
	if !localErr.Recoverable || string(localErr.Payload) == "" {
		t.Fatalf("expected recoverable proxy-local payload, got %#v", localErr)
	}

	conn.reads <- responsesWSReadResult{messageType: websocket.TextMessage, payload: []byte(`{"type":"response.cancel"}`)}
	event = readResponsesWSEvent(t, actor)
	frame, ok := event.(ResponsesWSEventClientFrame)
	if !ok {
		t.Fatalf("expected client frame after recoverable read error, got %T", event)
	}
	if frame.MessageType != websocket.TextMessage || string(frame.Payload) != `{"type":"response.cancel"}` {
		t.Fatalf("unexpected forwarded client frame: %#v", frame)
	}

	conn.reads <- responsesWSReadResult{err: responsesWSTestProxyLocalReadError{
		payload:     []byte(`{"type":"error","error":{"code":"fatal_event"}}`),
		recoverable: false,
	}}
	event = readResponsesWSEvent(t, actor)
	localErr, ok = event.(ResponsesWSEventProxyLocalError)
	if !ok {
		t.Fatalf("expected final proxy-local error event, got %T", event)
	}
	if localErr.Recoverable {
		t.Fatalf("expected non-recoverable proxy-local error")
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
		UpstreamSessionGeneration: upstreamSessionGeneration,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		MessageType:               websocket.TextMessage,
		Payload:                   []byte(`{"type":"response.completed","response":{"id":"resp_no_turn"}}`),
		Origin:                    runtimesession.RealtimePayloadOriginProvider,
	})

	if !actor.closed.Load() {
		t.Fatalf("expected provider event without turn to fail closed")
	}
	if session.abortReason != "responses_ws_provider_event_without_turn" {
		t.Fatalf("expected session abort on no-turn provider event, got %q", session.abortReason)
	}
	if actor.lastFinal != nil || actor.activeAttempt != nil {
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
	actor.lastFinal = &types.OpenAIResponsesResponses{ID: "resp_done"}

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		UpstreamSessionGeneration: upstreamSessionGeneration,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		MessageType:               websocket.CloseMessage,
		Payload:                   []byte("bye"),
		Origin:                    runtimesession.RealtimePayloadOriginProvider,
	})

	if !actor.closed.Load() {
		t.Fatal("expected provider close to end the actor")
	}
	if session.abortReason == "responses_ws_provider_event_without_turn" {
		t.Fatalf("expected provider close after terminal not to be classified as protocol violation, got %q", session.abortReason)
	}
	if got := atomic.LoadInt32(&conn.controlCount); got != 1 {
		t.Fatalf("expected provider close frame to be forwarded once, got %d", got)
	}
}

func TestResponsesWSAmbiguousSendWithBufferedProviderEventWaitsForTerminal(t *testing.T) {
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
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-ambiguous",
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}
	actor.pendingAttempt = attempt
	actor.state = responsesWSStatePendingSend

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		UpstreamSessionGeneration: upstreamSessionGeneration,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		MessageType:               websocket.TextMessage,
		Payload:                   []byte(`{"type":"response.completed","response":{"id":"resp_buffered"}}`),
		Origin:                    runtimesession.RealtimePayloadOriginProvider,
	})
	if !actor.hasPendingProviderEvidence() || len(actor.pendingProviderEvents) != 1 {
		t.Fatalf("expected buffered provider evidence before send result")
	}

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         "attempt-ambiguous",
		SelectedChannelID: 17,
		Outcome:           SendOutcomeAmbiguous,
		Err:               errors.New("write timeout"),
	})

	if actor.closed.Load() {
		t.Fatalf("expected ambiguous send with provider evidence to stay open")
	}
	if actor.lastFinal == nil || actor.lastFinal.ID != "resp_buffered" {
		t.Fatalf("expected buffered terminal to be consumed, got %+v", actor.lastFinal)
	}
	if got, _ := conn.lastWrite.Load().(string); strings.Contains(got, "responses_ws_send_ambiguous") {
		t.Fatalf("expected no proxy-local ambiguous error after provider evidence, got %q", got)
	}
	if actor.activeAttempt != nil || actor.state != responsesWSStateIdle {
		t.Fatalf("expected terminal to clear active attempt, state=%v active=%+v", actor.state, actor.activeAttempt)
	}
}

func TestResponsesWSNotSentWithUsageOnlyProviderEvidenceFailsClosed(t *testing.T) {
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
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:              "attempt-usage",
		SelectedChannelID:      17,
		SendOutcome:            SendOutcomeNotSent,
		Usage:                  &types.Usage{},
		QuotaPreconsumed:       true,
		QuotaFinalized:         true,
		QuotaEventSinkAttached: false,
	}
	actor.pendingAttempt = attempt
	actor.state = responsesWSStatePendingSend

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		UpstreamSessionGeneration: upstreamSessionGeneration,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamUsage,
		Usage:                     &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
		Origin:                    runtimesession.RealtimePayloadOriginProvider,
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
		Outcome:           SendOutcomeNotSent,
		Err:               runtimesession.ErrSessionClosed,
	})

	if !actor.closed.Load() {
		t.Fatalf("expected not-sent proof conflict with usage evidence to fail closed")
	}
	if attempt.RolledBack {
		t.Fatalf("expected proof conflict not to rollback quota")
	}
	if session.abortReason != "responses_ws_not_sent_with_provider_evidence" {
		t.Fatalf("expected session abort for proof conflict, got %q", session.abortReason)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_not_sent_with_provider_evidence") {
		t.Fatalf("expected protocol violation payload for proof conflict, got %q", got)
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
	actor.pendingTurnPhase = responsesWSPendingTurnNone
	attempt := &ResponsesWSTurnAttempt{
		SelectedChannelID: 17,
		Usage:             &types.Usage{},
	}

	if err := actor.BeginCandidate(attempt); err != nil {
		t.Fatalf("expected pending attempt to begin, got %v", err)
	}
	if !actor.isBusy() || actor.pendingTurnPhase != responsesWSPendingTurnPrepare || actor.state != responsesWSStatePendingPrepare {
		t.Fatalf("expected BeginCandidate to make actor busy, state=%v phase=%v", actor.state, actor.pendingTurnPhase)
	}
	if err := actor.rollbackPendingAttemptBeforeLocalWrite("rewrite_failed"); err != nil {
		t.Fatalf("expected rollback cleanup to succeed, got %v", err)
	}

	if actor.isBusy() {
		t.Fatalf("expected local rollback to leave actor idle, state=%v phase=%v pending=%+v", actor.state, actor.pendingTurnPhase, actor.pendingAttempt)
	}
	if actor.pendingAttempt != nil || actor.pendingTurnPhase != responsesWSPendingTurnNone || actor.state != responsesWSStateIdle {
		t.Fatalf("expected pending turn transaction to be cleared, state=%v phase=%v pending=%+v", actor.state, actor.pendingTurnPhase, actor.pendingAttempt)
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
	actor.pendingTurnPhase = responsesWSPendingTurnNone

	actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"gpt-4","input":[]}`), time.Now())

	if actor.pendingAttempt != nil || actor.pendingTurnPhase != responsesWSPendingTurnNone || actor.state != responsesWSStateIdle {
		t.Fatalf("expected model mismatch before attempt creation, state=%v phase=%v pending=%+v", actor.state, actor.pendingTurnPhase, actor.pendingAttempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, `"code":"responses_ws_model_mismatch"`) {
		t.Fatalf("expected model mismatch error, got %q", got)
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
	ctx.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI})

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.state = responsesWSStateIdle
	actor.pendingTurnPhase = responsesWSPendingTurnNone

	actor.startSubsequentTurn([]byte(`{"type":"response.create","model":"gpt-5","input":[]}`), time.Now())

	if actor.pendingAttempt != nil || actor.pendingTurnPhase != responsesWSPendingTurnNone || actor.state != responsesWSStateIdle {
		t.Fatalf("expected RPM failure before attempt creation, state=%v phase=%v pending=%+v", actor.state, actor.pendingTurnPhase, actor.pendingAttempt)
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, `"code":"api_requests_not_allowed"`) || strings.Contains(got, "API requests are not allowed") {
		t.Fatalf("expected local RPM error, got %q", got)
	}
}

func TestResponsesWSBusyCreateBudgetClosesAbusiveClient(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	actor.activeAttempt = &ResponsesWSTurnAttempt{Usage: &types.Usage{}}
	actor.state = responsesWSStateInFlight

	for i := 0; i < responsesWSBusyRejectLimit+1; i++ {
		actor.handleClientFrame(ResponsesWSEventClientFrame{
			MessageType: websocket.TextMessage,
			Payload:     []byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`),
		})
	}

	if !actor.closed.Load() {
		t.Fatal("expected excessive busy response.create frames to close the session")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_busy_rate_limited") {
		t.Fatalf("expected busy rate-limit error, got %q", got)
	}
}

func TestResponsesWSPendingProviderBufferHasByteCap(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	session := &responsesWSTestSession{}
	generation := actor.AttachUpstreamSession(session, 17)
	actor.pendingAttempt = &ResponsesWSTurnAttempt{AttemptID: "attempt-buffer", SelectedChannelID: 17, Usage: &types.Usage{}}
	actor.state = responsesWSStatePendingSend

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		MessageType:               websocket.TextMessage,
		Payload:                   []byte(strings.Repeat("x", config.ResponsesWSPendingProviderEventsMaxBytes()+1)),
		Origin:                    runtimesession.RealtimePayloadOriginProvider,
	})

	if !actor.closed.Load() {
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
	if !actor.closed.Load() {
		t.Fatal("expected actor to be marked closed after max lifetime")
	}
}

func TestResponsesWSTerminalSideEffectsSurviveClientWriteFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1), writeErr: errors.New("client write failed")}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	actor.activeAttempt = &ResponsesWSTurnAttempt{AttemptID: "attempt-terminal", SelectedChannelID: 17, Usage: &types.Usage{}}
	actor.activeTurn = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.activeChannelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		MessageType:               websocket.TextMessage,
		Payload:                   []byte(`{"type":"response.completed","response":{"id":"resp_write_failed","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`),
		Origin:                    runtimesession.RealtimePayloadOriginProvider,
	})

	if actor.lastFinal == nil || actor.lastFinal.ID != "resp_write_failed" {
		t.Fatalf("expected terminal side effects before client write failure, got %+v", actor.lastFinal)
	}
	if actor.activeAttempt != nil || !actor.closed.Load() {
		t.Fatalf("expected active turn cleared and session closed, active=%+v closed=%v", actor.activeAttempt, actor.closed.Load())
	}
}

func TestResponsesWSCloseReplaysBufferedTerminalForUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(&responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}, actor))
	actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-buffered-close",
		SelectedChannelID: 17,
		SendOutcome:       SendOutcomeAmbiguous,
		Usage:             &types.Usage{},
	}
	actor.pendingAttempt = attempt
	actor.pendingProviderEvidenceSeen = true
	actor.pendingProviderEvents = []ResponsesWSEventProviderDownstream{{
		ChannelID:   17,
		Kind:        ProviderDownstreamFrame,
		MessageType: websocket.TextMessage,
		Payload:     []byte(`{"type":"response.completed","response":{"id":"resp_close_buffered","status":"completed","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`),
		Origin:      runtimesession.RealtimePayloadOriginProvider,
	}}
	actor.state = responsesWSStatePendingSend

	actor.close("test_close_buffered")

	if attempt.Usage.PromptTokens != 3 || attempt.Usage.CompletionTokens != 4 || attempt.Usage.TotalTokens != 7 {
		t.Fatalf("expected buffered terminal usage to be merged before close settlement, got %+v", attempt.Usage)
	}
	if actor.lastFinal == nil || actor.lastFinal.ID != "resp_close_buffered" {
		t.Fatalf("expected buffered terminal final response to be recorded, got %+v", actor.lastFinal)
	}
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
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.output_text.delta","delta":"hi"}`),
		origin:      runtimesession.RealtimePayloadOriginProvider,
		err:         runtimesession.NewClientPayloadError(quotaErr, []byte(quotaErr.Error())),
	}

	first := readResponsesWSEvent(t, actor)
	downstream, ok := first.(ResponsesWSEventProviderDownstream)
	if !ok || downstream.Kind != ProviderDownstreamFrame || downstream.Origin != runtimesession.RealtimePayloadOriginProvider {
		t.Fatalf("expected provider payload to be emitted first, got %#v", first)
	}
	if string(downstream.Payload) == "" || !strings.Contains(string(downstream.Payload), "response.output_text.delta") {
		t.Fatalf("expected provider payload to be preserved, got %q", downstream.Payload)
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
	actor.lastActivityUnixNano.Store(old.UnixNano())
	bridge.ArmProviderRecvPump("session-activity", 17, session)

	session.responses <- responsesWSRecvResult{usage: &types.UsageEvent{TotalTokens: 1}}
	event := readResponsesWSEvent(t, actor)
	if downstream, ok := event.(ResponsesWSEventProviderDownstream); !ok || downstream.Kind != ProviderDownstreamUsage {
		t.Fatalf("expected provider usage event, got %#v", event)
	}
	if got := time.Unix(0, actor.lastActivityUnixNano.Load()); !got.After(old) {
		t.Fatalf("expected provider usage to refresh activity, got %s old %s", got, old)
	}

	old = time.Now().Add(-time.Hour)
	actor.lastActivityUnixNano.Store(old.UnixNano())
	session.responses <- responsesWSRecvResult{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.output_text.delta","delta":"hi"}`),
	}
	event = readResponsesWSEvent(t, actor)
	if downstream, ok := event.(ResponsesWSEventProviderDownstream); !ok || downstream.Kind != ProviderDownstreamFrame {
		t.Fatalf("expected provider frame event, got %#v", event)
	}
	if got := time.Unix(0, actor.lastActivityUnixNano.Load()); !got.After(old) {
		t.Fatalf("expected provider frame to refresh activity, got %s old %s", got, old)
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

	session.responses <- responsesWSRecvResult{err: runtimesession.ErrSessionClosed}
	event := readResponsesWSEvent(t, actor)
	if timeout, ok := event.(ResponsesWSEventTimeout); !ok || timeout.UpstreamSessionGeneration != generation {
		t.Fatalf("expected provider close timeout event, got %#v", event)
	}

	deadline := time.After(time.Second)
	for {
		if _, ok := bridge.armed.Load(generation); !ok {
			return
		}
		select {
		case <-deadline:
			t.Fatal("expected provider recv pump to delete armed generation on exit")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
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

func TestResponsesWSSendOutcomeFromErrorClassifiesWriteFailureAmbiguous(t *testing.T) {
	if got := responsesWSSendOutcomeFromError(types.NewErrorEvent("", "provider_error", "ws_write_failed", "timeout")); got != SendOutcomeAmbiguous {
		t.Fatalf("expected websocket write failure to be ambiguous, got %v", got)
	}
	if got := responsesWSSendOutcomeFromError(types.NewErrorEvent("", "provider_error", "previous_response_not_found", "missing")); got != SendOutcomeNotSent {
		t.Fatalf("expected local provider rejection before write proof to be not-sent, got %v", got)
	}
	if got := responsesWSSendOutcomeFromError(types.NewErrorEvent("", "provider_error", "upstream_timeout", "timeout")); got != SendOutcomeAmbiguous {
		t.Fatalf("expected unknown provider error to be ambiguous, got %v", got)
	}
	if got := responsesWSSendOutcomeFromError(runtimesession.ErrSessionClosed); got != SendOutcomeNotSent {
		t.Fatalf("expected closed session before write to be not-sent, got %v", got)
	}
}

func TestResponsesWSActorCloseRollsBackPendingAttemptWithoutProviderEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	prepareActor := NewResponsesWSSessionActor(ctx)
	prepareAttempt := &ResponsesWSTurnAttempt{}
	prepareActor.pendingAttempt = prepareAttempt
	prepareActor.state = responsesWSStatePendingPrepare
	prepareActor.close("test_pending_prepare")
	if !prepareAttempt.RolledBack {
		t.Fatalf("expected pending_prepare close to rollback not-sent attempt")
	}

	sendActor := NewResponsesWSSessionActor(ctx)
	sendAttempt := &ResponsesWSTurnAttempt{}
	sendActor.pendingAttempt = sendAttempt
	sendActor.state = responsesWSStatePendingSend
	sendActor.close("test_pending_send")
	if sendAttempt.RolledBack {
		t.Fatalf("expected pending_send close with unknown send outcome to preserve preconsume")
	}

	notSentActor := NewResponsesWSSessionActor(ctx)
	notSentAttempt := &ResponsesWSTurnAttempt{SendOutcome: SendOutcomeNotSent}
	notSentActor.pendingAttempt = notSentAttempt
	notSentActor.state = responsesWSStatePendingSend
	notSentActor.close("test_not_sent")
	if !notSentAttempt.RolledBack {
		t.Fatalf("expected explicit not-sent outcome to rollback pending attempt")
	}
}

func TestResponsesWSActorClosePendingSendUnknownPreservesPreConsumedQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}); err != nil {
		t.Fatalf("expected quota settlement schema migration to succeed, got %v", err)
	}
	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
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

	actor := NewResponsesWSSessionActor(ctx)
	actor.pendingAttempt = attempt
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
	attempt.MarkProviderAcceptedTurnEvidence()

	actor := NewResponsesWSSessionActor(ctx)
	actor.activeAttempt = attempt
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
	attempt.MarkProviderUsageSeen()

	actor := NewResponsesWSSessionActor(ctx)
	actor.activeAttempt = attempt
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
	actor.activeAttempt = attempt
	actor.activeChannelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		Kind:        ProviderDownstreamFrame,
		MessageType: websocket.TextMessage,
		Payload:     []byte(`{"type":"response.cancelled","response":{"id":"resp_cancel","status":"cancelled","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`),
		Origin:      runtimesession.RealtimePayloadOriginProvider,
	})

	if actor.activeAttempt != nil || actor.activeChannelID != 0 || actor.state != responsesWSStateIdle {
		t.Fatalf("expected cancelled terminal to clear active turn, active=%v channel=%d state=%v", actor.activeAttempt != nil, actor.activeChannelID, actor.state)
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
	attempt.MarkFirstProviderResponse(firstResponseAt)

	actor := NewResponsesWSSessionActor(ctx)
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	bridge := NewResponsesWSIOBridge(conn, actor)
	actor.SetBridge(bridge)
	actor.activeAttempt = attempt
	actor.activeChannelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		Kind:        ProviderDownstreamFrame,
		MessageType: websocket.TextMessage,
		Payload:     []byte(`{"type":"response.completed","response":{"id":"resp_done","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`),
		Origin:      runtimesession.RealtimePayloadOriginProvider,
		ReceivedAt:  completedAt,
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
	actor.activeAttempt = attempt
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
	attempt.MarkProviderUsageSeen()
	mergeResponsesWSUsageEvent(attempt.Usage, &types.UsageEvent{InputTokens: 4, OutputTokens: 2, TotalTokens: 6})

	actor := NewResponsesWSSessionActor(ctx)
	actor.activeAttempt = attempt
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
	actor.sessionChannelID = 17

	for i, responseID := range []string{"resp_one", "resp_two"} {
		attempt := preparePreconsumedResponsesWSTestAttempt(t, ctx)
		actor.activeAttempt = attempt
		actor.activeTurn = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
		actor.activeChannelID = 17
		actor.state = responsesWSStateInFlight
		actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
			ChannelID:   17,
			Kind:        ProviderDownstreamFrame,
			MessageType: websocket.TextMessage,
			Payload: []byte(fmt.Sprintf(
				`{"type":"response.completed","response":{"id":%q,"status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
				responseID,
			)),
			Origin: runtimesession.RealtimePayloadOriginProvider,
		})
		if attempt.RolledBack || !attempt.QuotaFinalized {
			t.Fatalf("expected turn %d to finalize without rollback, rolled=%v finalized=%v", i+1, attempt.RolledBack, attempt.QuotaFinalized)
		}
		if actor.state != responsesWSStateIdle || actor.activeAttempt != nil {
			t.Fatalf("expected turn %d to leave actor idle, state=%v active=%+v", i+1, actor.state, actor.activeAttempt)
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

func TestResponsesWSSendResultMismatchWithoutEvidenceRollsBack(t *testing.T) {
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
	actor.pendingAttempt = attempt
	actor.state = responsesWSStatePendingSend

	actor.handleSendResult(ResponsesWSEventSendResult{
		AttemptID:         "attempt-stale",
		SelectedChannelID: 17,
		Outcome:           SendOutcomeLocalWriteOK,
	})

	if !attempt.RolledBack || actor.pendingAttempt != nil {
		t.Fatalf("expected mismatch without evidence to rollback and clear pending attempt, rolled=%v pending=%+v", attempt.RolledBack, actor.pendingAttempt)
	}
	if !actor.closed.Load() {
		t.Fatalf("expected mismatch to fail closed after rollback")
	}
}

func TestResponsesWSMalformedProviderFramePreservesPreConsumedFloor(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetBridge(NewResponsesWSIOBridge(conn, actor))
	generation := actor.AttachUpstreamSession(&responsesWSTestSession{}, 17)
	attempt := &ResponsesWSTurnAttempt{
		AttemptID:         "attempt-malformed",
		SelectedChannelID: 17,
		Quota:             &relay_util.Quota{},
		QuotaPreconsumed:  true,
		Usage:             &types.Usage{},
	}
	actor.activeAttempt = attempt
	actor.activeTurn = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.activeChannelID = 17
	actor.state = responsesWSStateInFlight

	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		MessageType:               websocket.TextMessage,
		Payload:                   []byte(`{"type":`),
		Origin:                    runtimesession.RealtimePayloadOriginProvider,
	})

	if attempt.RolledBack {
		t.Fatalf("expected malformed provider frame on active turn to preserve quota floor")
	}
	if !attempt.QuotaFinalized {
		t.Fatalf("expected malformed provider frame on active turn to finalize quota floor")
	}
	if !actor.closed.Load() {
		t.Fatalf("expected malformed provider frame to close session")
	}
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_provider_protocol_error") {
		t.Fatalf("expected provider protocol error payload, got %q", got)
	}
}

func TestResponsesWSTurnAttemptRollbackRestoresQuotaSynchronously(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}); err != nil {
		t.Fatalf("expected quota rollback schema migration to succeed, got %v", err)
	}
	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
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
