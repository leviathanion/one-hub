package relay

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/providers/codex"
	runtimeaffinity "one-api/runtime/channelaffinity"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type relayTestRealtimeSession struct{}

func init() {
	logger.Logger = zap.NewNop()
}

func (relayTestRealtimeSession) SendClient(context.Context, runtimesession.Frame) error { return nil }
func (relayTestRealtimeSession) Recv(context.Context) (runtimesession.RecvEvent, error) {
	return runtimesession.RecvEvent{}, nil
}
func (relayTestRealtimeSession) Detach(string) {}
func (relayTestRealtimeSession) Abort(string)  {}
func (relayTestRealtimeSession) SetTurnObserverFactory(runtimesession.TurnObserverFactory) {
}

type relayActorTestSession struct {
	sendCh chan runtimesession.Frame
	recvCh chan runtimesession.RecvEvent

	mu            sync.Mutex
	detachReasons []string
	abortReasons  []string
}

func newRelayActorTestSession() *relayActorTestSession {
	return &relayActorTestSession{
		sendCh: make(chan runtimesession.Frame, 8),
		recvCh: make(chan runtimesession.RecvEvent, 8),
	}
}

func (s *relayActorTestSession) SendClient(ctx context.Context, frame runtimesession.Frame) error {
	select {
	case s.sendCh <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *relayActorTestSession) Recv(ctx context.Context) (runtimesession.RecvEvent, error) {
	select {
	case event := <-s.recvCh:
		return event, nil
	case <-ctx.Done():
		return runtimesession.RecvEvent{}, ctx.Err()
	}
}

func (s *relayActorTestSession) Detach(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detachReasons = append(s.detachReasons, reason)
}

func (s *relayActorTestSession) Abort(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abortReasons = append(s.abortReasons, reason)
}

func (s *relayActorTestSession) SetTurnObserverFactory(runtimesession.TurnObserverFactory) {}

type relayTestBaseProvider struct {
	channel *model.Channel
}

func (p *relayTestBaseProvider) GetRequestHeaders() map[string]string { return nil }
func (p *relayTestBaseProvider) GetUsage() *types.Usage               { return nil }
func (p *relayTestBaseProvider) SetUsage(usage *types.Usage)          { _ = usage }
func (p *relayTestBaseProvider) SetContext(c *gin.Context)            { _ = c }
func (p *relayTestBaseProvider) SetOriginalModel(modelName string)    { _ = modelName }
func (p *relayTestBaseProvider) GetOriginalModel() string             { return "" }
func (p *relayTestBaseProvider) GetChannel() *model.Channel           { return p.channel }
func (p *relayTestBaseProvider) ModelMappingHandler(modelName string) (string, error) {
	return modelName, nil
}
func (p *relayTestBaseProvider) GetRequester() *requester.HTTPRequester { return nil }
func (p *relayTestBaseProvider) CustomParameterHandler() (map[string]interface{}, error) {
	return nil, nil
}
func (p *relayTestBaseProvider) GetSupportedResponse() bool { return false }

type relayTestRealtimeProvider struct {
	relayTestBaseProvider
	openFn func(modelName string, options runtimesession.RealtimeOpenOptions) (runtimesession.RealtimeSession, *types.OpenAIErrorWithStatusCode)
}

func (p *relayTestRealtimeProvider) OpenRealtimeSession(modelName string) (runtimesession.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	if p.openFn != nil {
		return p.openFn(modelName, runtimesession.RealtimeOpenOptions{})
	}
	return relayTestRealtimeSession{}, nil
}

func (p *relayTestRealtimeProvider) OpenRealtimeSessionWithOptions(modelName string, options runtimesession.RealtimeOpenOptions) (runtimesession.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	if p.openFn != nil {
		return p.openFn(modelName, options)
	}
	return relayTestRealtimeSession{}, nil
}

type relayTestClientFrame struct {
	messageType wsconn.MessageType
	payload     []byte
}

type relayTestManagedClient struct {
	conn   *wsconn.ManagedConn
	frames chan relayTestClientFrame
	closed chan wsconn.CloseInfo
}

func newRelayWebsocketPair(t *testing.T) (*wsconn.ManagedConn, *relayTestManagedClient) {
	t.Helper()

	clientConn, serverConn := wstest.Pair(t)
	client := &relayTestManagedClient{
		conn:   clientConn,
		frames: make(chan relayTestClientFrame, 8),
		closed: make(chan wsconn.CloseInfo, 1),
	}
	go wsconn.Pump{
		Conn: clientConn,
		Handle: func(_ context.Context, messageType wsconn.MessageType, payload []byte) {
			client.frames <- relayTestClientFrame{messageType: messageType, payload: append([]byte(nil), payload...)}
		},
		OnClose: func(info wsconn.CloseInfo) {
			client.closed <- info
		},
	}.Run(context.Background())
	t.Cleanup(func() {
		clientConn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"})
		serverConn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"})
	})
	return serverConn, client
}

func (c *relayTestManagedClient) readFrame(t *testing.T) relayTestClientFrame {
	t.Helper()
	select {
	case frame := <-c.frames:
		return frame
	case info := <-c.closed:
		t.Fatalf("expected downstream frame before close, got close %+v", info)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for downstream frame")
	}
	return relayTestClientFrame{}
}

func (c *relayTestManagedClient) readClose(t *testing.T) wsconn.CloseInfo {
	t.Helper()
	select {
	case info := <-c.closed:
		return info
	case frame := <-c.frames:
		t.Fatalf("expected downstream close before frame, got frame %+v", frame)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for downstream close")
	}
	return wsconn.CloseInfo{}
}

func TestRealtimeClientSessionIDFromRequestPrefersExplicitHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	req.Header.Set("x-session-id", "execution-session-456")
	req.Header.Set("session_id", "legacy-session")

	if got := realtimeClientSessionIDFromRequest(req); got != "execution-session-456" {
		t.Fatalf("expected x-session-id to win, got %q", got)
	}
}

func TestRealtimeWebSocketOriginPolicy(t *testing.T) {
	originalAllowed := viper.Get("realtime.allowed_origins")
	originalCORSAllowed := viper.Get("cors.allow_origins")
	originalUnsafe := viper.Get("realtime.unsafe_allow_credential_subprotocol_any_origin")
	t.Cleanup(func() {
		viper.Set("realtime.allowed_origins", originalAllowed)
		viper.Set("cors.allow_origins", originalCORSAllowed)
		viper.Set("realtime.unsafe_allow_credential_subprotocol_any_origin", originalUnsafe)
	})

	tests := []struct {
		name        string
		allowed     []string
		corsAllowed []string
		origin      string
		protocols   string
		unsafe      bool
		wantAllowed bool
	}{
		{name: "server call without origin", wantAllowed: true},
		{name: "origin allowed when allowlist empty", origin: "https://app.example", wantAllowed: true},
		{name: "credential subprotocol rejects empty allowlist", origin: "https://app.example", protocols: "openai-insecure-api-key.sk-test", wantAllowed: false},
		{name: "credential subprotocol unsafe opt-out allows empty allowlist", origin: "https://app.example", protocols: "openai-insecure-api-key.sk-test", unsafe: true, wantAllowed: true},
		{name: "exact origin allowed", allowed: []string{"https://app.example"}, origin: "https://app.example", wantAllowed: true},
		{name: "cors allowlist fallback allows origin", corsAllowed: []string{"https://app.example"}, origin: "https://app.example", wantAllowed: true},
		{name: "unlisted origin rejected", allowed: []string{"https://app.example"}, origin: "https://evil.example", wantAllowed: false},
		{name: "wildcard allows non credential origin", allowed: []string{"*"}, origin: "https://app.example", wantAllowed: true},
		{name: "wildcard rejects credential subprotocol", allowed: []string{"*"}, origin: "https://app.example", protocols: "realtime, openai-insecure-api-key.sk-test", wantAllowed: false},
		{name: "credential subprotocol requires origin", allowed: []string{"https://app.example"}, protocols: "openai-insecure-api-key.sk-test", wantAllowed: false},
		{name: "credential subprotocol accepts exact origin", allowed: []string{"https://app.example"}, origin: "https://app.example", protocols: "openai-insecure-api-key.sk-test, realtime", wantAllowed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			viper.Set("realtime.allowed_origins", tt.allowed)
			viper.Set("cors.allow_origins", tt.corsAllowed)
			viper.Set("realtime.unsafe_allow_credential_subprotocol_any_origin", tt.unsafe)
			req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.protocols != "" {
				req.Header.Set("Sec-WebSocket-Protocol", tt.protocols)
			}
			if got := realtimeWebSocketOriginAllowed(req); got != tt.wantAllowed {
				t.Fatalf("origin policy allowed=%v, want %v", got, tt.wantAllowed)
			}
		})
	}
}

func TestRealtimeHandlersRejectOriginBeforeUpgrade(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalAllowed := viper.Get("realtime.allowed_origins")
	t.Cleanup(func() {
		viper.Set("realtime.allowed_origins", originalAllowed)
	})
	viper.Set("realtime.allowed_origins", []string{"https://app.example"})

	router := gin.New()
	router.GET("/v1/realtime", ChatRealtime)
	router.GET("/v1/responses", ResponsesWebSocket)

	for _, path := range []string{"/v1/realtime?model=gpt-5", "/v1/responses"} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Origin", "https://evil.example")
		router.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("expected %s to reject invalid origin with 403, got %d", path, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "realtime_origin_not_allowed") && !strings.Contains(recorder.Body.String(), "websocket origin is not allowed") {
			t.Fatalf("expected %s to return an explicit origin error, got %s", path, recorder.Body.String())
		}
	}
}

func TestWebSocketSubprotocolNegotiationOnlyAllowsKnownValues(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "evil-auth, openai-beta.realtime-v1, openai-insecure-api-key.sk-client, realtime")

	allowed := allowedClientWebSocketSubprotocols(req)
	if strings.Join(allowed, ",") != "openai-beta.realtime-v1,openai-insecure-api-key.sk-client,realtime" {
		t.Fatalf("unexpected allowed protocols: %#v", allowed)
	}
	if got := selectWebSocketSubprotocol(req); got != "realtime" {
		t.Fatalf("expected realtime to be negotiated when present, got %q", got)
	}
	echoable := echoableClientWebSocketSubprotocols(req)
	if strings.Join(echoable, ",") != "realtime" {
		t.Fatalf("unexpected echoable protocols: %#v", echoable)
	}
}

func TestWebSocketSubprotocolNegotiationNeverEchoesCredential(t *testing.T) {
	tests := []struct {
		name      string
		protocols string
		want      string
	}{
		{
			name:      "credential only is never echoed",
			protocols: "openai-insecure-api-key.sk-secret",
			want:      "",
		},
		{
			name:      "credential with beta picks beta",
			protocols: "openai-insecure-api-key.sk-secret, openai-beta.realtime-v1",
			want:      "openai-beta.realtime-v1",
		},
		{
			name:      "realtime always wins over credential",
			protocols: "openai-insecure-api-key.sk-secret, realtime",
			want:      "realtime",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
			req.Header.Set("Sec-WebSocket-Protocol", tt.protocols)
			if got := selectWebSocketSubprotocol(req); got != tt.want {
				t.Fatalf("selectWebSocketSubprotocol() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWebSocketUpgradeDoesNotEchoCredentialSubprotocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsconn.AcceptManaged(w, r, wsconn.Config{Label: "subprotocol-test"}, wsconn.AcceptOptions{
			CheckOrigin:    func(*http.Request) bool { return true },
			ResponseHeader: websocketUpgradeResponseHeader(r),
			Subprotocols:   echoableClientWebSocketSubprotocols(r),
		})
		if err != nil {
			return
		}
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
	}))
	defer server.Close()

	tests := []struct {
		name      string
		protocols []string
		want      string
	}{
		{
			name:      "credential before realtime",
			protocols: []string{"openai-insecure-api-key.sk-secret", "realtime"},
			want:      "realtime",
		},
		{
			name:      "credential only",
			protocols: []string{"openai-insecure-api-key.sk-secret"},
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := wsconn.DialManaged(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil, wsconn.Config{Label: "subprotocol-client-test"},
				wsconn.WithSubprotocols(tt.protocols...),
				wsconn.WithDialSecurityPolicy(wsconn.DialSecurityPolicy{
					AllowInsecureWS: true,
					AllowPrivateIP:  true,
				}),
			)
			if err != nil {
				t.Fatalf("websocket dial failed: %v", err)
			}
			defer conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})

			got := conn.Subprotocol()
			if got != tt.want {
				t.Fatalf("negotiated subprotocol=%q, want %q", got, tt.want)
			}
			if strings.Contains(got, "openai-insecure-api-key.") {
				t.Fatalf("negotiated credential subprotocol: %q", got)
			}
		})
	}
}

func TestRelayModeChatRealtimeGetProviderUsesAffinityChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	const (
		sessionID         = "client-session-affinity-hit"
		defaultChannelID  = 11
		affinityChannelID = 424299
	)

	model.ChannelGroup = buildRealtimeTestChannelGroup(defaultChannelID, affinityChannelID)

	ctx := newRelayTestContext(map[string]string{
		"X-Session-Id": sessionID,
	})
	ctx.Set("token_id", 301)
	ctx.Set("token_group", "default")
	rememberChannelAffinityKey(ctx, channelAffinityKindRealtime, sessionID)
	recordCurrentChannelAffinity(ctx, channelAffinityKindRealtime, affinityChannelID)

	relay := &RelayModeChatRealtime{
		relayBase: relayBase{
			c: ctx,
		},
	}
	relay.setOriginalModel("gpt-5")

	if !relay.getProvider() {
		t.Fatal("expected realtime provider selection to succeed")
	}
	t.Cleanup(func() {
		if relay.session != nil {
			relay.session.Abort("test_cleanup")
		}
	})

	if got := relay.provider.GetChannel().Id; got != affinityChannelID {
		t.Fatalf("expected affinity channel #%d, got #%d", affinityChannelID, got)
	}
	if got, ok := lookupChannelAffinity(ctx, channelAffinityKindRealtime, sessionID); !ok || got != affinityChannelID {
		t.Fatalf("expected affinity record to stay on channel #%d, got channel=%d ok=%v", affinityChannelID, got, ok)
	}
}

func TestRelayModeChatRealtimeGetProviderFallsBackWhenAffinityChannelUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	const (
		sessionID        = "client-session-affinity-miss"
		defaultChannelID = 11
		staleAffinityID  = 424299
	)

	model.ChannelGroup = buildRealtimeTestChannelGroup(defaultChannelID)

	ctx := newRelayTestContext(map[string]string{
		"X-Session-Id": sessionID,
	})
	ctx.Set("token_id", 301)
	ctx.Set("token_group", "default")
	rememberChannelAffinityKey(ctx, channelAffinityKindRealtime, sessionID)
	recordCurrentChannelAffinity(ctx, channelAffinityKindRealtime, staleAffinityID)

	relay := &RelayModeChatRealtime{
		relayBase: relayBase{
			c: ctx,
		},
	}
	relay.setOriginalModel("gpt-5")

	if !relay.getProvider() {
		t.Fatal("expected realtime provider selection to succeed after affinity miss")
	}
	t.Cleanup(func() {
		if relay.session != nil {
			relay.session.Abort("test_cleanup")
		}
	})

	if got := relay.provider.GetChannel().Id; got != defaultChannelID {
		t.Fatalf("expected fallback to channel #%d, got #%d", defaultChannelID, got)
	}
	if got, ok := lookupChannelAffinity(ctx, channelAffinityKindRealtime, sessionID); !ok || got != defaultChannelID {
		t.Fatalf("expected affinity to be rewritten onto channel #%d, got channel=%d ok=%v", defaultChannelID, got, ok)
	}
}

func TestRelayModeChatRealtimeGetProviderForceFreshOnSameAffinityChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	const (
		sessionID         = "client-session-affinity-force-fresh"
		affinityChannelID = 424299
	)

	sourceChannel := newRelayTestCodexChannel(affinityChannelID)
	sourceHeaders := `{"x-codex-beta-features":"feature-a"}`
	sourceChannel.ModelHeaders = &sourceHeaders
	sourceProvider := newRelayTestCodexProviderForChannel(t, sourceChannel, map[string]string{
		"X-Session-Id": sessionID,
	})
	sourceProvider.Context.Set("token_id", 301)

	sourceSession, errWithCode := sourceProvider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected source realtime session to open, got %v", errWithCode)
	}
	sourceSession.Detach("test_detach")
	t.Cleanup(func() {
		sourceSession.Abort("test_cleanup")
	})

	routedChannel := newRelayTestCodexChannel(affinityChannelID)
	routedHeaders := `{"x-codex-beta-features":"feature-b"}`
	routedChannel.ModelHeaders = &routedHeaders
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(routedChannel)

	ctx := newRelayTestContext(map[string]string{
		"X-Session-Id": sessionID,
	})
	ctx.Set("token_id", 301)
	ctx.Set("token_group", "default")
	rememberChannelAffinityKey(ctx, channelAffinityKindRealtime, sessionID)
	recordCurrentChannelAffinity(ctx, channelAffinityKindRealtime, affinityChannelID)

	relay := &RelayModeChatRealtime{
		relayBase: relayBase{
			c: ctx,
		},
	}
	relay.setOriginalModel("gpt-5")

	if !relay.getProvider() {
		t.Fatal("expected same-channel force-fresh reopen to succeed")
	}
	t.Cleanup(func() {
		if relay.session != nil {
			relay.session.Abort("test_cleanup")
		}
	})

	if got := relay.provider.GetChannel().Id; got != affinityChannelID {
		t.Fatalf("expected force-fresh reopen to stay on affinity channel #%d, got #%d", affinityChannelID, got)
	}
	if got, ok := lookupChannelAffinity(ctx, channelAffinityKindRealtime, sessionID); !ok || got != affinityChannelID {
		t.Fatalf("expected affinity record to stay on channel #%d after force-fresh reopen, got channel=%d ok=%v", affinityChannelID, got, ok)
	}
}

func TestRelayModeChatRealtimeGetProviderFreshRerouteReplacesStaleBindingAfterAffinityMiss(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	const (
		sessionID        = "client-session-affinity-stale-binding-reroute"
		defaultChannelID = 11
		staleAffinityID  = 424299
	)

	sourceProvider := newRelayTestCodexProviderForChannel(t, newRelayTestCodexChannel(staleAffinityID), map[string]string{
		"X-Session-Id": sessionID,
	})
	sourceProvider.Context.Set("token_id", 301)

	sourceSession, errWithCode := sourceProvider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected source realtime session to open, got %v", errWithCode)
	}
	sourceSession.Detach("test_detach")
	t.Cleanup(func() {
		sourceSession.Abort("test_cleanup")
	})

	model.ChannelGroup = buildRealtimeTestChannelGroup(defaultChannelID)

	ctx := newRelayTestContext(map[string]string{
		"X-Session-Id": sessionID,
	})
	ctx.Set("token_id", 301)
	ctx.Set("token_group", "default")
	rememberChannelAffinityKey(ctx, channelAffinityKindRealtime, sessionID)
	recordCurrentChannelAffinity(ctx, channelAffinityKindRealtime, staleAffinityID)

	relay := &RelayModeChatRealtime{
		relayBase: relayBase{
			c: ctx,
		},
	}
	relay.setOriginalModel("gpt-5")

	if !relay.getProvider() {
		t.Fatal("expected stale binding not to block fresh reroute after affinity miss")
	}
	t.Cleanup(func() {
		if relay.session != nil {
			relay.session.Abort("test_cleanup")
		}
	})

	if got := relay.provider.GetChannel().Id; got != defaultChannelID {
		t.Fatalf("expected reroute to channel #%d, got #%d", defaultChannelID, got)
	}
	if got, ok := lookupChannelAffinity(ctx, channelAffinityKindRealtime, sessionID); !ok || got != defaultChannelID {
		t.Fatalf("expected affinity to move onto channel #%d after fresh reroute, got channel=%d ok=%v", defaultChannelID, got, ok)
	}
}

func TestRelayModeChatRealtimeGetProviderPinnedChannelOverridesAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("expected channel schema migration, got %v", err)
	}
	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})

	const (
		sessionID         = "client-session-pinned-force-fresh"
		pinnedChannelID   = 11
		affinityChannelID = 424299
	)

	sourceProvider := newRelayTestCodexProviderForChannel(t, newRelayTestCodexChannel(affinityChannelID), map[string]string{
		"X-Session-Id": sessionID,
	})
	sourceProvider.Context.Set("token_id", 301)

	sourceSession, errWithCode := sourceProvider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected source realtime session to open, got %v", errWithCode)
	}
	sourceSession.Detach("test_detach")
	t.Cleanup(func() {
		sourceSession.Abort("test_cleanup")
	})

	pinnedChannel := newRelayTestCodexChannel(pinnedChannelID)
	if err := model.DB.Create(pinnedChannel).Error; err != nil {
		t.Fatalf("expected pinned channel fixture to persist, got %v", err)
	}

	seedCtx := newRelayTestContext(map[string]string{
		"X-Session-Id": sessionID,
	})
	seedCtx.Set("token_id", 301)
	rememberChannelAffinityKey(seedCtx, channelAffinityKindRealtime, sessionID)
	recordCurrentChannelAffinity(seedCtx, channelAffinityKindRealtime, affinityChannelID)

	ctx := newRelayTestContext(map[string]string{
		"X-Session-Id": sessionID,
	})
	ctx.Set("token_id", 301)
	ctx.Set("specific_channel_id", pinnedChannelID)
	ctx.Set("specific_channel_id_ignore", false)
	rememberChannelAffinityKey(ctx, channelAffinityKindRealtime, sessionID)

	relay := &RelayModeChatRealtime{
		relayBase: relayBase{
			c: ctx,
		},
	}
	relay.setOriginalModel("gpt-5")

	if !relay.getProvider() {
		t.Fatal("expected pinned channel force-fresh open to succeed")
	}
	t.Cleanup(func() {
		if relay.session != nil {
			relay.session.Abort("test_cleanup")
		}
	})

	if got := relay.provider.GetChannel().Id; got != pinnedChannelID {
		t.Fatalf("expected pinned realtime routing to stay on channel #%d, got #%d", pinnedChannelID, got)
	}
	if got, ok := lookupChannelAffinity(seedCtx, channelAffinityKindRealtime, sessionID); !ok || got != affinityChannelID {
		t.Fatalf("expected pinned request not to rewrite shared affinity, got channel=%d ok=%v", got, ok)
	}
}

func TestRelayModeChatRealtimeGetProviderStrictAffinityUnavailableAborts(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	const (
		sessionID        = "client-session-strict-affinity-miss"
		defaultChannelID = 11
		staleAffinityID  = 424299
	)

	settings := config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 60,
		MaxEntries:        20,
		Rules: []config.ChannelAffinityRule{
			{
				Name:            "realtime-session-strict",
				Enabled:         true,
				Kind:            "realtime",
				Strict:          true,
				IncludeRuleName: true,
				RecordOnSuccess: true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "header", Key: "x-session-id", Alias: config.ChannelAffinityAliasSessionID},
				},
			},
		},
	}
	settings.Normalize()
	manager := withChannelAffinitySettings(t, settings)

	model.ChannelGroup = buildRealtimeTestChannelGroup(defaultChannelID)

	serverConn, client := newRelayWebsocketPair(t)
	ctx := newRelayTestContext(map[string]string{
		"X-Session-Id": sessionID,
	})
	ctx.Set("token_id", 301)
	ctx.Set("token_group", "default")
	realtimeBinding := defaultChannelAffinityBinding(ctx, channelAffinityKindRealtime, sessionID)
	if realtimeBinding == nil {
		t.Fatal("expected strict realtime affinity binding")
	}
	manager.SetRecord(realtimeBinding.Key, runtimeaffinity.Record{
		ChannelID: staleAffinityID,
	}, realtimeBinding.Template.TTL)

	relay := &RelayModeChatRealtime{
		relayBase: relayBase{c: ctx},
		userConn:  serverConn,
	}
	relay.setOriginalModel("gpt-5")

	if relay.getProvider() {
		t.Fatal("expected strict affinity miss to abort realtime provider selection")
	}

	frame := client.readFrame(t)
	if frame.messageType != wsconn.TextMessage {
		t.Fatalf("expected websocket abort payload, got type=%d", frame.messageType)
	}
	if !strings.Contains(string(frame.payload), "preferred realtime channel is unavailable") {
		t.Fatalf("expected strict affinity abort message, got %s", frame.payload)
	}
}

func TestRelayModeChatRealtimeOpenFreshRealtimeSessionSkipsUnsupportedProviderWithoutPin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalRetryTimes := config.RetryTimes
	config.RetryTimes = 1
	t.Cleanup(func() {
		config.RetryTimes = originalRetryTimes
	})

	ctx := newRelayTestContext(nil)
	ctx.Set("channel_id", 11)
	ctx.Set("channel_type", config.ChannelTypeCodex)

	cacheProviderSelection(ctx, "gpt-5", &relayTestBaseProvider{channel: newRelayTestCodexChannel(11)}, "gpt-5")

	relay := &RelayModeChatRealtime{
		relayBase: relayBase{c: ctx},
	}
	relay.setOriginalModel("gpt-5")

	if relay.openFreshRealtimeSession("", false) {
		t.Fatal("expected unsupported realtime provider to fail fresh session opening")
	}

	skipped, ok := ctx.Get("skip_channel_ids")
	if !ok {
		t.Fatal("expected unsupported provider path to mark the channel as skipped")
	}
	channelIDs, ok := skipped.([]int)
	if !ok || len(channelIDs) != 1 || channelIDs[0] != 11 {
		t.Fatalf("unexpected skipped channel ids payload: %#v", skipped)
	}
}

func TestRelayModeChatRealtimeOpenFreshRealtimeSessionRejectsUnsupportedPinnedProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalRetryTimes := config.RetryTimes
	config.RetryTimes = 1
	t.Cleanup(func() {
		config.RetryTimes = originalRetryTimes
	})

	ctx := newRelayTestContext(nil)
	ctx.Set("specific_channel_id", 11)
	ctx.Set("specific_channel_id_ignore", false)
	ctx.Set("channel_id", 11)
	ctx.Set("channel_type", config.ChannelTypeCodex)

	cacheProviderSelection(ctx, "gpt-5", &relayTestBaseProvider{channel: newRelayTestCodexChannel(11)}, "gpt-5")

	relay := &RelayModeChatRealtime{
		relayBase: relayBase{c: ctx},
	}
	relay.setOriginalModel("gpt-5")

	if relay.openFreshRealtimeSession("", false) {
		t.Fatal("expected pinned unsupported realtime provider to fail fresh session opening")
	}
	if _, ok := ctx.Get("skip_channel_ids"); ok {
		t.Fatal("expected pinned unsupported provider not to continue retrying through skip_channel_ids")
	}
}

func newRelayTestContext(headers map[string]string) *gin.Context {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil)
	for key, value := range headers {
		ctx.Request.Header.Set(key, value)
	}
	return ctx
}

func buildRealtimeTestChannelGroup(channelIDs ...int) model.ChannelsChooser {
	channels := make([]*model.Channel, 0, len(channelIDs))
	for _, channelID := range channelIDs {
		channels = append(channels, newRelayTestCodexChannel(channelID))
	}
	return buildRealtimeTestChannelGroupForChannels(channels...)
}

func buildRealtimeTestChannelGroupForChannels(channels ...*model.Channel) model.ChannelsChooser {
	weight := uint(1)
	choices := make(map[int]*model.ChannelChoice, len(channels))
	priority := make([]int, 0, len(channels))

	for _, channel := range channels {
		if channel == nil {
			continue
		}
		if channel.Weight == nil {
			channel.Weight = &weight
		}
		priority = append(priority, channel.Id)
		choices[channel.Id] = &model.ChannelChoice{Channel: channel}
	}

	return model.ChannelsChooser{
		Channels: choices,
		Rule: map[string]map[string][][]int{
			"default": {
				"gpt-5": {priority},
			},
		},
		ModelGroup: map[string]map[string]bool{
			"gpt-5": {
				"default": true,
			},
		},
	}
}

func newRelayTestCodexChannel(channelID int) *model.Channel {
	weight := uint(1)
	proxy := ""
	return &model.Channel{
		Id:     channelID,
		Type:   config.ChannelTypeCodex,
		Key:    `{"access_token":"access-token","account_id":"acct-123"}`,
		Status: config.ChannelStatusEnabled,
		Group:  "default",
		Models: "gpt-5",
		Weight: &weight,
		Proxy:  &proxy,
		Other:  `{"websocket_mode":"off"}`,
	}
}

func newRelayTestCodexProviderForChannel(t *testing.T, channel *model.Channel, headers map[string]string) *codex.CodexProvider {
	t.Helper()

	provider, ok := codex.CodexProviderFactory{}.Create(channel).(*codex.CodexProvider)
	if !ok || provider == nil {
		t.Fatal("expected Codex provider instance")
	}
	provider.Context = newRelayTestContext(headers)
	return provider
}

func TestRealtimeHelperFunctionsAndFallbacks(t *testing.T) {
	if got := openAIErrorCodeString(" session_closed ", "fallback"); got != "session_closed" {
		t.Fatalf("expected string error code to trim whitespace, got %q", got)
	}
	if got := openAIErrorCodeString(409, "fallback"); got != "409" {
		t.Fatalf("expected numeric error code to stringify, got %q", got)
	}
	if got := openAIErrorCodeString(nil, "fallback"); got != "fallback" {
		t.Fatalf("expected nil error code to use fallback, got %q", got)
	}

	if !strings.Contains(string(buildRealtimeMessageErrorPayload("boom")), `"message":"boom"`) {
		t.Fatal("expected realtime message payload to preserve message")
	}
	if !strings.Contains(string(buildRealtimeErrorPayload(nil)), `"code":"system_error"`) {
		t.Fatal("expected nil realtime error payload to fall back to system_error")
	}
	if payload := string(buildRealtimeErrorPayload(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Type:    "provider_error",
			Code:    429,
			Message: "rate limited",
		},
	})); !strings.Contains(payload, `"type":"provider_error"`) || !strings.Contains(payload, `"code":"429"`) {
		t.Fatalf("expected realtime error payload to preserve type/code, got %s", payload)
	}

	originalRetryTimes := config.RetryTimes
	config.RetryTimes = 0
	if got := realtimeOpenRetryBudget(); got != 1 {
		t.Fatalf("expected retry budget floor of 1, got %d", got)
	}
	config.RetryTimes = 3
	if got := realtimeOpenRetryBudget(); got != 3 {
		t.Fatalf("expected configured retry budget, got %d", got)
	}
	config.RetryTimes = originalRetryTimes

	if providerSupportsRealtime(nil) {
		t.Fatal("expected nil provider not to support realtime")
	}
	if providerSupportsRealtime(&relayTestBaseProvider{}) {
		t.Fatal("expected base provider not to support realtime")
	}
	if !providerSupportsRealtime(&relayTestRealtimeProvider{}) {
		t.Fatal("expected realtime-capable provider to support realtime")
	}

	if shouldForceFreshRealtimeSession(nil) {
		t.Fatal("expected nil error not to force fresh")
	}
	if shouldForceFreshRealtimeSession(&types.OpenAIErrorWithStatusCode{
		LocalError: true,
		OpenAIError: types.OpenAIError{
			Code: "session_closed",
		},
	}) != true {
		t.Fatal("expected session_closed local error to force fresh")
	}
	if shouldForceFreshRealtimeSession(&types.OpenAIErrorWithStatusCode{
		LocalError: true,
		OpenAIError: types.OpenAIError{
			Code: "other",
		},
	}) {
		t.Fatal("expected unrelated local error not to force fresh")
	}

	calls := make([]runtimesession.RealtimeOpenOptions, 0, 2)
	provider := &relayTestRealtimeProvider{
		relayTestBaseProvider: relayTestBaseProvider{channel: newRelayTestCodexChannel(99)},
		openFn: func(modelName string, options runtimesession.RealtimeOpenOptions) (runtimesession.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
			calls = append(calls, options)
			if len(calls) == 1 {
				return nil, &types.OpenAIErrorWithStatusCode{
					LocalError: true,
					OpenAIError: types.OpenAIError{
						Code: "session_binding_mismatch",
					},
				}
			}
			return relayTestRealtimeSession{}, nil
		},
	}
	session, apiErr := openRealtimeSessionWithFreshFallback(provider, "gpt-5", runtimesession.RealtimeOpenOptions{
		ClientSessionID: "session-123",
	})
	if apiErr != nil || session == nil {
		t.Fatalf("expected fresh fallback reopen to succeed, got session=%v err=%v", session, apiErr)
	}
	if len(calls) != 2 || calls[0].ForceFresh || !calls[1].ForceFresh {
		t.Fatalf("expected second realtime open attempt to force fresh, got %+v", calls)
	}

	if _, apiErr := openRealtimeSessionWithOptions(&relayTestBaseProvider{}, "gpt-5", runtimesession.RealtimeOpenOptions{}); apiErr == nil || apiErr.Message != "channel not implemented" {
		t.Fatalf("expected unsupported provider to return channel-not-implemented, got %v", apiErr)
	}
}

func TestRelayModeChatRealtimeAbortAndStateHelpers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	serverConn, client := newRelayWebsocketPair(t)
	ctx := newRelayTestContext(nil)
	relay := &RelayModeChatRealtime{
		relayBase: relayBase{c: ctx},
		userConn:  serverConn,
	}

	relay.abortWithError(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Type:    "provider_error",
			Code:    "quota_exhausted",
			Message: "quota exhausted",
		},
	})

	frame := client.readFrame(t)
	if frame.messageType != wsconn.TextMessage {
		t.Fatalf("expected text abort payload, got type=%d", frame.messageType)
	}
	if !strings.Contains(string(frame.payload), `"code":"quota_exhausted"`) || !strings.Contains(string(frame.payload), `"message":"quota exhausted"`) {
		t.Fatalf("unexpected abort payload: %s", frame.payload)
	}

	if info := client.readClose(t); info.Kind != wsconn.CloseKindPeerClose || info.Code != wsconn.CloseNormalClosure || info.Reason != "quota_exhausted" {
		t.Fatalf("expected websocket connection to close after abort payload, got %+v", info)
	}

	var nilRelay *RelayModeChatRealtime
	nilRelay.writeAbortPayload([]byte(`{"type":"error"}`), "system_error")

	relay2 := &RelayModeChatRealtime{relayBase: relayBase{c: ctx}}
	relay2.abortWithMessage("no-connection")

	session := relayTestRealtimeSession{}
	relay.activateRealtimeSession(&relayTestRealtimeProvider{relayTestBaseProvider: relayTestBaseProvider{channel: newRelayTestCodexChannel(88)}}, "gpt-5", session, 88)
	if relay.session == nil || relay.modelName != "gpt-5" || relay.provider.GetChannel().Id != 88 {
		t.Fatalf("expected activateRealtimeSession to capture provider/session/model, got relay=%+v", relay)
	}

	relay.skipChannelIds(9)
	relay.skipChannelIds(10)
	if got, ok := ctx.Get("skip_channel_ids"); !ok {
		t.Fatal("expected skip_channel_ids to be present")
	} else if typed, ok := got.([]int); !ok || len(typed) != 2 || typed[0] != 9 || typed[1] != 10 {
		t.Fatalf("unexpected skip channel ids payload: %#v", got)
	}

	relay.excludeRealtimePreferredChannelForCurrentRequest(12, &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Code:    "session_closed",
			Message: "session closed",
		},
	})
	if got, ok := ctx.Get("skip_channel_ids"); !ok {
		t.Fatal("expected failed preferred realtime channel to be excluded")
	} else if typed, ok := got.([]int); !ok || len(typed) != 3 || typed[2] != 12 {
		t.Fatalf("unexpected preferred exclusion skip list: %#v", got)
	}
	if meta := currentChannelAffinityLogMeta(ctx); meta["channel_affinity_preferred_open_failed_excluded"] != true || meta["channel_affinity_preferred_open_failed_id"] != 12 {
		t.Fatalf("expected preferred exclusion metadata, got %#v", meta)
	}
}

func TestFetchPreferredRealtimeChannelValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	model.ChannelGroup = buildRealtimeTestChannelGroup(11, 22)
	ctx := newRelayTestContext(nil)
	ctx.Set("token_group", "default")

	if _, err := fetchPreferredRealtimeChannel(nil, "gpt-5", 11); err == nil {
		t.Fatal("expected nil context to be rejected")
	}
	if _, err := fetchPreferredRealtimeChannel(ctx, "gpt-5", 0); err == nil {
		t.Fatal("expected zero preferred channel id to be rejected")
	}
	if channel, err := fetchPreferredRealtimeChannel(ctx, "gpt-5", 22); err != nil || channel == nil || channel.Id != 22 {
		t.Fatalf("expected preferred channel #22 to be selected, got channel=%#v err=%v", channel, err)
	}

	model.ChannelGroup = buildRealtimeTestChannelGroup(11)
	if _, err := fetchPreferredRealtimeChannel(ctx, "gpt-5", 22); err == nil {
		t.Fatal("expected unavailable preferred channel to return an error")
	}
}

func TestRealtimeRelayActorClientFrameBackpressureClosesTryAgainLater(t *testing.T) {
	actorConn, peerConn := wstest.Pair(t)
	session := newRelayActorTestSession()
	actor := newRealtimeRelayActor(actorConn, session, time.Second)
	for i := 0; i < cap(actor.clientFrames); i++ {
		actor.clientFrames <- realtimeRelayClientFrame{mt: wsconn.TextMessage, payload: []byte(`{"type":"noop"}`)}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		actor.clientPump()
	}()

	if err := peerConn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.cancel"}`)); err != nil {
		t.Fatalf("expected peer write to reach actor pump, got %v", err)
	}
	select {
	case <-actorConn.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for actor connection to close on backpressure")
	}
	info := actorConn.CloseInfo()
	if info.Kind != wsconn.CloseKindBackpressure || info.Code != wsconn.CloseTryAgainLater || info.Reason != "client_frame_backpressure" {
		t.Fatalf("expected backpressure close 1013, got %+v", info)
	}
	peerConn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for client pump to exit")
	}
}

func TestRealtimeRelayActorProviderFrameWritesDownstream(t *testing.T) {
	serverConn, client := newRelayWebsocketPair(t)
	actor := newRealtimeRelayActor(serverConn, newRelayActorTestSession(), time.Second)

	textFrame := runtimesession.NewTextFrame([]byte(`{"type":"response.text.delta","delta":"hi"}`))
	if !actor.deliverEventFrame(runtimesession.RecvEvent{
		Frame:  &textFrame,
		Origin: runtimesession.RealtimePayloadOriginProvider,
	}) {
		t.Fatal("expected text provider frame to be delivered")
	}
	frame := client.readFrame(t)
	if frame.messageType != wsconn.TextMessage || string(frame.payload) != string(textFrame.Payload()) {
		t.Fatalf("unexpected downstream text frame mt=%d payload=%s", frame.messageType, frame.payload)
	}

	binaryFrame := runtimesession.NewBinaryFrame([]byte{1, 2, 3})
	if !actor.deliverEventFrame(runtimesession.RecvEvent{
		Frame:  &binaryFrame,
		Origin: runtimesession.RealtimePayloadOriginProvider,
	}) {
		t.Fatal("expected binary provider frame to be delivered")
	}
	frame = client.readFrame(t)
	if frame.messageType != wsconn.BinaryMessage || string(frame.payload) != string(binaryFrame.Payload()) {
		t.Fatalf("unexpected downstream binary frame mt=%d payload=%v", frame.messageType, frame.payload)
	}
}

func TestRealtimeRelayActorCoordinateExitsOnContextCancel(t *testing.T) {
	actor := newRealtimeRelayActor(nil, nil, time.Second)
	actor.cancel()
	go actor.coordinate()
	select {
	case <-actor.done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for coordinator to exit on context cancel")
	}
}

func TestRealtimeRelayActorSessionToClientHandlesNilSession(t *testing.T) {
	actor := newRealtimeRelayActor(nil, nil, time.Second)
	actor.sessionToClient()

	select {
	case <-actor.supplierClosed:
	default:
		t.Fatal("expected supplierClosed to close when session is nil")
	}
	select {
	case exit := <-actor.exitCh:
		if exit.source != "supplier" || !errors.Is(exit.err, net.ErrClosed) {
			t.Fatalf("unexpected nil session exit: %+v", exit)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for nil session exit")
	}
}

func TestRealtimeRelayActorProviderClosePreservesPrivateWireCode(t *testing.T) {
	serverConn, client := newRelayWebsocketPair(t)
	actor := newRealtimeRelayActor(serverConn, newRelayActorTestSession(), time.Second)

	exit := actor.providerCloseExit(&runtimesession.ProviderClose{
		Code:   4408,
		Reason: "session_expired",
	}, nil)
	if !exit.hasDownstreamClose || exit.downstreamCloseCode != wsconn.CloseCode(4408) {
		t.Fatalf("expected provider close 4408 to sanitize without replacement, got %+v", exit)
	}
	actor.closeDownstream(exit.downstreamCloseCode, exit.downstreamCloseReason)

	if info := client.readClose(t); info.Code != wsconn.CloseCode(4408) || info.Reason != "session_expired" {
		t.Fatalf("expected downstream close 4408 session_expired, got %+v", info)
	}
}

func TestRealtimeRelayActorProviderCloseClosesDownstreamAtRecvPoint(t *testing.T) {
	serverConn, client := newRelayWebsocketPair(t)
	session := newRelayActorTestSession()
	actor := newRealtimeRelayActor(serverConn, session, time.Second)
	actor.workers.Add(1)
	go actor.runWorker(actor.sessionToClient)

	session.recvCh <- runtimesession.RecvEvent{
		ProviderClose: &runtimesession.ProviderClose{
			Code:   4408,
			Reason: "session_expired",
			Err:    runtimesession.ErrSessionClosed,
		},
	}

	if info := client.readClose(t); info.Code != wsconn.CloseCode(4408) || info.Reason != "session_expired" {
		t.Fatalf("expected Recv ProviderClose to close downstream with 4408 session_expired, got %+v", info)
	}

	actor.cancel()
	select {
	case <-actor.supplierClosed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for supplier worker to exit")
	}
}

var _ providersBase.ProviderInterface = (*relayTestBaseProvider)(nil)
var _ providersBase.RealtimeSessionProvider = (*relayTestRealtimeProvider)(nil)
var _ providersBase.RealtimeSessionProviderWithOptions = (*relayTestRealtimeProvider)(nil)
