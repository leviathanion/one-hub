package router

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/common/wsconn"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var realtimeRouteUserID atomic.Int64
var realtimeRouteLoggerOnce sync.Once

type realtimeUpgradeCounter struct {
	gin.ResponseWriter
	count *atomic.Int64
}

func (writer realtimeUpgradeCounter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := writer.ResponseWriter.Hijack()
	if err == nil {
		writer.count.Add(1)
	}
	return conn, rw, err
}

type realtimeRouteFixture struct {
	url         string
	token       string
	dials       atomic.Int64
	upgrades    atomic.Int64
	upstream    chan *wsconn.ManagedConn
	frames      chan []byte
	queryModels chan string
	db          *gorm.DB
	userID      int
	shutdown    func()
}

func newRealtimeRouteFixture(t *testing.T, failUpstream bool) *realtimeRouteFixture {
	return newLongLivedRouteFixture(t, failUpstream, false)
}

func newLongLivedRouteFixture(t *testing.T, failUpstream, responses bool) *realtimeRouteFixture {
	return newLongLivedRouteFixtureWithHTTP(t, failUpstream, responses, nil)
}

func newLongLivedRouteFixtureWithHTTP(t *testing.T, failUpstream, responses bool, httpHandler http.HandlerFunc) *realtimeRouteFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	realtimeRouteLoggerOnce.Do(func() { logger.Logger = zap.NewNop() })
	for key, value := range map[string]any{
		"realtime_ws.connect_per_user_per_minute": 1,
		"realtime.allowed_origins":                []string{},
		"user_token_secret":                       "realtime-route-test-secret-at-least-32-characters",
	} {
		old := viper.Get(key)
		viper.Set(key, value)
		t.Cleanup(func() { viper.Set(key, old) })
	}
	if err := common.InitUserToken(); err != nil {
		t.Fatal(err)
	}
	oldRedis, oldLog, oldBatch := config.RedisEnabled, config.LogConsumeEnabled, config.BatchUpdateEnabled
	config.RedisEnabled, config.LogConsumeEnabled, config.BatchUpdateEnabled = false, false, false
	t.Cleanup(func() {
		config.RedisEnabled, config.LogConsumeEnabled, config.BatchUpdateEnabled = oldRedis, oldLog, oldBatch
	})
	fixture := &realtimeRouteFixture{
		queryModels: make(chan string, 4),
		upstream:    make(chan *wsconn.ManagedConn, 4),
		frames:      make(chan []byte, 32),
		userID:      71000 + int(realtimeRouteUserID.Add(1)),
	}
	upstreamCtx, stopUpstream := context.WithCancel(context.Background())
	t.Cleanup(stopUpstream)
	var upstreamHandlers, relayHandlers sync.WaitGroup
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHandlers.Add(1)
		defer upstreamHandlers.Done()
		if httpHandler != nil && !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			httpHandler(w, r)
			return
		}
		fixture.dials.Add(1)
		fixture.queryModels <- r.URL.Query().Get("model")
		if failUpstream {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		conn, err := wsconn.AcceptManaged(w, r, wsconn.Config{}, wsconn.AcceptOptions{})
		if err != nil {
			t.Errorf("upstream upgrade: %v", err)
			return
		}
		fixture.upstream <- conn
		if !responses {
			if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"session.created","session":{"id":"route-session"}}`)); err != nil {
				return
			}
		}
		wsconn.Pump{
			Conn: conn,
			Handle: func(_ context.Context, _ wsconn.MessageType, payload []byte) {
				var event struct {
					Type    string          `json:"type"`
					Session json.RawMessage `json:"session"`
				}
				if json.Unmarshal(payload, &event) == nil && (event.Type == "session.update" || event.Type == "transcription_session.update") {
					event.Type += "d"
					ack, _ := json.Marshal(event)
					if err := conn.WriteMessage(wsconn.TextMessage, ack); err != nil {
						t.Errorf("session update acknowledgement: %v", err)
						return
					}
				}
				fixture.frames <- append([]byte(nil), payload...)
			},
		}.Run(upstreamCtx)
	}))
	t.Cleanup(upstream.Close)
	if httpHandler != nil {
		oldHTTPClient := requester.HTTPClient
		requester.HTTPClient = upstream.Client()
		t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })
	}

	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.UserGroup{}, &model.Price{}, &model.ModelInfo{}, &model.PublicationVersion{}, &model.Log{}); err != nil {
		t.Fatal(err)
	}
	if responses {
		if err := db.AutoMigrate(&model.ResponseOwner{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := model.EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	fixture.db = db
	oldDB, oldPricing := model.DB, model.PricingInstance
	oldGroupPolicy := model.GlobalUserGroupRatio
	model.GlobalUserGroupRatio = &model.UserGroupRatio{}
	model.DB = db
	model.PricingInstance = &model.Pricing{Prices: make(map[string]*model.Price)}
	t.Cleanup(func() {
		model.DB, model.PricingInstance = oldDB, oldPricing
		for _, limiter := range model.GlobalUserGroupRatio.APILimiter {
			if stopper, ok := limiter.(interface{ Stop() }); ok {
				stopper.Stop()
			}
		}
		model.GlobalUserGroupRatio = oldGroupPolicy
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	})
	user := &model.User{Id: fixture.userID, Username: "realtime-route-user", Password: "password", AccessToken: "route-access", Quota: 100000, Status: config.UserStatusEnabled, Group: "realtime-route"}
	if err := db.Create(user).Error; err != nil {
		t.Fatal(err)
	}
	fixture.token, err = common.GenerateToken(fixture.userID, fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	token := &model.Token{Id: fixture.userID, UserId: fixture.userID, Key: fixture.token, Status: config.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100000}
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(token).Error; err != nil {
		t.Fatal(err)
	}
	enabled := true
	group := &model.UserGroup{Symbol: "realtime-route", Ratio: 1, APIRate: 1, Enable: &enabled}
	if err := db.Create(group).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.GlobalUserGroupRatio.Load(); err != nil {
		t.Fatal(err)
	}
	modelNames := []string{"gpt-realtime", "gpt-transcribe"}
	if httpHandler != nil {
		modelNames = append(modelNames, "gpt-4o")
	}
	channel := &model.Channel{Type: config.ChannelTypeOpenAI, Key: "upstream-secret", Other: `{"self_hosted":true}`, Status: config.ChannelStatusEnabled, BaseURL: &upstream.URL, Models: strings.Join(modelNames, ","), Group: group.Symbol}
	if responses {
		channel.Other = `{"self_hosted":true,"responses_ws_native":true,"responses_ws_self_hosted":true}`
	}
	if httpHandler != nil {
		channel.Other = `{"self_hosted":true,"responses_ws_native":true,"responses_ws_self_hosted":true,"responses_stored_lifecycle":true}`
	}
	if err := db.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.ChannelGroup.Load(); err != nil {
		t.Fatal(err)
	}
	for _, name := range modelNames {
		extraKey := config.UsageExtraInputAudioTranscription
		if name == "gpt-transcribe" {
			// Realtime token ASR reports the audio/text partition under
			// UsageExtraInputAudio; the duration-only HTTP key is a different
			// billing component.
			extraKey = config.UsageExtraInputAudio
		}
		extra := datatypes.NewJSONType(map[string]float64{extraKey: 4})
		if err := db.Create(&model.Price{Model: name, Type: model.TokensPriceType, Input: 1, Output: 1, ExtraRatios: &extra}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		relayHandlers.Add(1)
		defer relayHandlers.Done()
		c.Writer = realtimeUpgradeCounter{ResponseWriter: c.Writer, count: &fixture.upgrades}
		c.Next()
	})
	SetRelayRouter(engine)
	server := httptest.NewServer(engine)
	var shutdownOnce sync.Once
	fixture.shutdown = func() {
		shutdownOnce.Do(func() {
			stopUpstream()
			server.Close()
			upstream.Close()
			upstreamHandlers.Wait()
			relayHandlers.Wait()
		})
	}
	t.Cleanup(fixture.shutdown)
	fixture.url = "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?model=gpt-realtime"
	if responses {
		fixture.url = "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	}
	return fixture
}

func (fixture *realtimeRouteFixture) dial(t *testing.T) (*wsconn.ManagedConn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	return wsconn.DialManaged(ctx, fixture.url, http.Header{"Authorization": {"Bearer " + fixture.token}}, wsconn.Config{}, wsconn.WithDialSecurityPolicy(wsconn.DialSecurityPolicy{AllowInsecureWS: true, AllowPrivateIP: true}))
}

func (fixture *realtimeRouteFixture) awaitQuota(t *testing.T, charged int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var user model.User
		var token model.Token
		if err := fixture.db.First(&user, fixture.userID).Error; err != nil {
			t.Fatal(err)
		}
		if err := fixture.db.First(&token, fixture.userID).Error; err != nil {
			t.Fatal(err)
		}
		if user.UsedQuota == charged && user.Quota == 100000-charged && token.RemainQuota == 100000-charged {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("实际结算错误：期望%d，user=%d/%d，token=%d", charged, user.UsedQuota, user.Quota, token.RemainQuota)
		}
		time.Sleep(time.Millisecond)
	}
}

func realtimeRouteReceive(t *testing.T, frames <-chan []byte) []byte {
	t.Helper()
	select {
	case payload := <-frames:
		return payload
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for realtime frame")
		return nil
	}
}

func realtimeRouteClientFrames(t *testing.T, conn *wsconn.ManagedConn) <-chan []byte {
	t.Helper()
	frames := make(chan []byte, 32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		wsconn.Pump{Conn: conn, Handle: func(_ context.Context, _ wsconn.MessageType, payload []byte) {
			var event struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(payload, &event) == nil && (event.Type == "session.updated" || event.Type == "transcription_session.updated") {
				return
			}
			frames <- append([]byte(nil), payload...)
		}}.Run(t.Context())
	}()
	t.Cleanup(func() {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"})
		<-done
	})
	return frames
}

func TestRealtimeRouteHandshakeAndManualWorkHaveSeparateLimits(t *testing.T) {
	fixture := newRealtimeRouteFixture(t, false)
	client, err := fixture.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	frames := realtimeRouteClientFrames(t, client)
	if payload := realtimeRouteReceive(t, frames); !strings.Contains(string(payload), "session.created") {
		t.Fatalf("bootstrap = %s", payload)
	}
	upstream := <-fixture.upstream
	t.Cleanup(func() { upstream.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"}) })
	for _, event := range []string{
		`{"type":"session.update","session":{"turn_detection":null}}`,
		`{"type":"response.create","response":{"max_output_tokens":100}}`,
	} {
		if err := client.WriteMessage(wsconn.TextMessage, []byte(event)); err != nil {
			t.Fatal(err)
		}
		forwarded := realtimeRouteReceive(t, fixture.frames)
		var want, got map[string]any
		_ = json.Unmarshal([]byte(event), &want)
		_ = json.Unmarshal(forwarded, &got)
		if got["type"] != want["type"] {
			t.Fatalf("first work did not reach upstream: %s", forwarded)
		}
	}
	if err := upstream.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.done","response":{"id":"route-response","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)); err != nil {
		t.Fatal(err)
	}
	if payload := realtimeRouteReceive(t, frames); !strings.Contains(string(payload), "response.done") {
		t.Fatalf("terminal = %s", payload)
	}
	if err := client.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.create","response":{"max_output_tokens":100}}`)); err != nil {
		t.Fatal(err)
	}
	if payload := realtimeRouteReceive(t, frames); !strings.Contains(string(payload), "rate_limit_exceeded") {
		t.Fatalf("second work was not rate limited: %s", payload)
	}
	assertRealtimeRouteReconnectLimited(t, fixture)
}

func assertRealtimeRouteReconnectLimited(t *testing.T, fixture *realtimeRouteFixture) {
	t.Helper()
	conn, err := fixture.dial(t)
	if conn != nil {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "unexpected_connection"})
	}
	var dialErr *wsconn.DialError
	if !errors.As(err, &dialErr) || dialErr.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(dialErr.BodySnippet), "realtime_ws_connection_rate_limited") {
		t.Fatalf("reconnect = %#v, %v", dialErr, err)
	}
	if got := fixture.upgrades.Load(); got != 1 {
		t.Fatalf("downstream upgrades = %d, want 1", got)
	}
	if got := fixture.dials.Load(); got != 1 {
		t.Fatalf("upstream dial attempts = %d, want 1", got)
	}
}

func TestRealtimeRouteFailedUpstreamAttemptStillConsumesConnectionLimit(t *testing.T) {
	fixture := newRealtimeRouteFixture(t, true)
	client, err := fixture.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	frames := realtimeRouteClientFrames(t, client)
	if payload := realtimeRouteReceive(t, frames); !strings.Contains(string(payload), "error") {
		t.Fatalf("expected upstream failure: %s", payload)
	}
	assertRealtimeRouteReconnectLimited(t, fixture)
}

func TestRealtimeRouteAutomaticAndTranscriptionWorkCountOnce(t *testing.T) {
	for _, mode := range []struct {
		name          string
		transcription bool
		automatic     bool
	}{
		{name: "automatic", automatic: true},
		{name: "transcription", transcription: true},
		{name: "automatic_with_transcription", automatic: true, transcription: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			transcriptionOnly := mode.transcription && !mode.automatic
			fixture := newRealtimeRouteFixture(t, false)
			client, err := fixture.dial(t)
			if err != nil {
				t.Fatal(err)
			}
			frames := realtimeRouteClientFrames(t, client)
			realtimeRouteReceive(t, frames)
			upstream := <-fixture.upstream
			t.Cleanup(func() { upstream.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"}) })
			update := `{"type":"session.update","session":{"turn_detection":{"type":"server_vad"}}}`
			if mode.transcription {
				update = `{"type":"session.update","session":{"turn_detection":{"type":"server_vad"},"input_audio_transcription":{"model":"gpt-transcribe"}}}`
			}
			if transcriptionOnly {
				update = `{"type":"session.update","session":{"turn_detection":null,"input_audio_transcription":{"model":"gpt-transcribe"}}}`
			}
			firstInput := []string{update, `{"type":"input_audio_buffer.append","audio":"AAAA"}`}
			if transcriptionOnly {
				firstInput = append(firstInput, `{"type":"input_audio_buffer.commit"}`)
			}
			for _, event := range firstInput {
				if err := client.WriteMessage(wsconn.TextMessage, []byte(event)); err != nil {
					t.Fatal(err)
				}
				realtimeRouteReceive(t, fixture.frames)
			}
			result := []string{`{"type":"input_audio_buffer.committed","item_id":"route-input"}`}
			if mode.transcription {
				result = append(result, `{"type":"conversation.item.input_audio_transcription.completed","item_id":"route-input","content_index":0,"transcript":"hello","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"input_token_details":{"audio_tokens":1,"text_tokens":0},"output_token_details":{"text_tokens":1}}}`)
			}
			if mode.automatic {
				result = append(result,
					`{"type":"response.created","response":{"id":"route-auto","status":"in_progress"}}`,
					`{"type":"response.done","response":{"id":"route-auto","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
				)
			}
			for _, event := range result {
				if err := upstream.WriteMessage(wsconn.TextMessage, []byte(event)); err != nil {
					t.Fatal(err)
				}
				if payload := realtimeRouteReceive(t, frames); strings.Contains(string(payload), `"error"`) {
					t.Fatalf("first logical work must complete at RPM=1: %s", payload)
				}
			}
			wantCharge := 0
			if mode.transcription {
				// The ASR token payload carries one audio input token and one
				// output token. With the gpt-transcribe audio ratio of four,
				// the provider usage settles at 4+1=5 quota.
				wantCharge += 5
			}
			if mode.automatic {
				wantCharge += 2
			}
			var user model.User
			if err := fixture.db.First(&user, fixture.userID).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != 100000-wantCharge || user.UsedQuota != wantCharge {
				t.Fatalf("settlement quota=%d used=%d want charge=%d", user.Quota, user.UsedQuota, wantCharge)
			}
			if err := client.WriteMessage(wsconn.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`)); err != nil {
				t.Fatal(err)
			}
			if transcriptionOnly {
				realtimeRouteReceive(t, fixture.frames)
				if err := client.WriteMessage(wsconn.TextMessage, []byte(`{"type":"input_audio_buffer.commit"}`)); err != nil {
					t.Fatal(err)
				}
			}
			if payload := realtimeRouteReceive(t, frames); !strings.Contains(string(payload), "rate_limit_exceeded") {
				t.Fatalf("second logical work must hit work RPM: %s", payload)
			}
			assertRealtimeRouteReconnectLimited(t, fixture)
		})
	}
}

func TestRealtimeRouteBillingQualificationPrecedesConnectionPermit(t *testing.T) {
	fixture := newRealtimeRouteFixture(t, false)
	if err := fixture.db.Model(&model.User{}).Where("id = ?", fixture.userID).Update("quota", 0).Error; err != nil {
		t.Fatal(err)
	}
	client, err := fixture.dial(t)
	if client != nil {
		client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "unexpected_connection"})
	}
	var dialErr *wsconn.DialError
	if !errors.As(err, &dialErr) || dialErr.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("insufficient balance handshake = %#v, %v", dialErr, err)
	}
	if fixture.dials.Load() != 0 || fixture.upgrades.Load() != 0 {
		t.Fatalf("qualification failure performed transport work: dial=%d upgrade=%d", fixture.dials.Load(), fixture.upgrades.Load())
	}
	if err := fixture.db.Model(&model.User{}).Where("id = ?", fixture.userID).Update("quota", 100000).Error; err != nil {
		t.Fatal(err)
	}
	client, err = fixture.dial(t)
	if err != nil {
		t.Fatalf("qualification failure consumed the connection permit: %v", err)
	}
	frames := realtimeRouteClientFrames(t, client)
	realtimeRouteReceive(t, frames)
	upstream := <-fixture.upstream
	t.Cleanup(func() { upstream.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"}) })
	assertRealtimeRouteReconnectLimited(t, fixture)
}
