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
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

type relayTestRealtimeSession struct{}

func init() {
	logger.Logger = zap.NewNop()
}

func (relayTestRealtimeSession) SendClient(context.Context, runtimerealtime.Frame) error { return nil }
func (relayTestRealtimeSession) Recv(context.Context) (runtimerealtime.RecvEvent, error) {
	return runtimerealtime.RecvEvent{}, nil
}
func (relayTestRealtimeSession) Detach(string) {}
func (relayTestRealtimeSession) Abort(string)  {}
func (relayTestRealtimeSession) SetTurnObserverFactory(runtimesession.TurnObserverFactory) {
}

type relayActorTestSession struct {
	sendCh chan runtimerealtime.Frame
	recvCh chan runtimerealtime.RecvEvent

	mu            sync.Mutex
	detachReasons []string
	abortReasons  []string
}

type relayActorRecoverableSendSession struct {
	mu       sync.Mutex
	calls    int
	accepted chan runtimerealtime.Frame
}

type relayActorConcurrentControlSession struct {
	relayTestRealtimeSession
	createStarted  chan struct{}
	createRelease  chan struct{}
	controlHandled chan struct{}
	controlReturn  chan struct{}
	serialFrames   chan runtimerealtime.Frame
	startOnce      sync.Once
	releaseOnce    sync.Once
	controlOnce    sync.Once
}

func (s *relayActorConcurrentControlSession) SendClient(ctx context.Context, frame runtimerealtime.Frame) error {
	if strings.Contains(string(frame.Payload()), `"type":"response.create"`) {
		s.startOnce.Do(func() { close(s.createStarted) })
		select {
		case <-s.createRelease:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.serialFrames <- frame
	return nil
}

func (s *relayActorConcurrentControlSession) TrySendClientControl(_ context.Context, active runtimerealtime.Frame, control runtimerealtime.Frame) (bool, error) {
	if !strings.Contains(string(active.Payload()), `"type":"response.create"`) || !strings.Contains(string(control.Payload()), `"type":"response.cancel"`) {
		return false, nil
	}
	s.controlOnce.Do(func() { close(s.controlHandled) })
	<-s.controlReturn
	s.releaseOnce.Do(func() { close(s.createRelease) })
	return true, nil
}

func (s *relayActorRecoverableSendSession) SendClient(_ context.Context, frame runtimerealtime.Frame) error {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		return runtimerealtime.NewRecoverableClientPayloadError(
			errors.New("stale continuation"),
			[]byte(`{"type":"error","status":400,"error":{"code":"previous_response_not_found","message":"previous response was not found"}}`),
		)
	}
	s.accepted <- frame
	return nil
}

func (s *relayActorRecoverableSendSession) Recv(ctx context.Context) (runtimerealtime.RecvEvent, error) {
	<-ctx.Done()
	return runtimerealtime.RecvEvent{}, ctx.Err()
}

func (s *relayActorRecoverableSendSession) Detach(string) {}
func (s *relayActorRecoverableSendSession) Abort(string)  {}
func (s *relayActorRecoverableSendSession) SetTurnObserverFactory(runtimesession.TurnObserverFactory) {
}

func newRelayActorTestSession() *relayActorTestSession {
	return &relayActorTestSession{
		sendCh: make(chan runtimerealtime.Frame, 8),
		recvCh: make(chan runtimerealtime.RecvEvent, 8),
	}
}

func (s *relayActorTestSession) SendClient(ctx context.Context, frame runtimerealtime.Frame) error {
	select {
	case s.sendCh <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *relayActorTestSession) Recv(ctx context.Context) (runtimerealtime.RecvEvent, error) {
	select {
	case event := <-s.recvCh:
		return event, nil
	case <-ctx.Done():
		return runtimerealtime.RecvEvent{}, ctx.Err()
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

type relayTestRealtimeProvider struct {
	relayTestBaseProvider
	openFn func(modelName string, options runtimerealtime.RealtimeOpenOptions) (runtimerealtime.RealtimeSession, *types.OpenAIErrorWithStatusCode)
}

func (p *relayTestRealtimeProvider) OpenRealtimeSession(modelName string) (runtimerealtime.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	if p.openFn != nil {
		return p.openFn(modelName, runtimerealtime.RealtimeOpenOptions{})
	}
	return relayTestRealtimeSession{}, nil
}

func (p *relayTestRealtimeProvider) OpenRealtimeSessionWithOptions(modelName string, options runtimerealtime.RealtimeOpenOptions) (runtimerealtime.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
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
	events chan relayTestClientEvent
}

type relayTestClientEvent struct {
	frame relayTestClientFrame
	close *wsconn.CloseInfo
}

func newRelayWebsocketPair(t *testing.T) (*wsconn.ManagedConn, *relayTestManagedClient) {
	t.Helper()

	clientConn, serverConn := wstest.Pair(t)
	client := &relayTestManagedClient{
		conn:   clientConn,
		events: make(chan relayTestClientEvent, 9),
	}
	go wsconn.Pump{
		Conn: clientConn,
		Handle: func(_ context.Context, messageType wsconn.MessageType, payload []byte) {
			client.events <- relayTestClientEvent{frame: relayTestClientFrame{messageType: messageType, payload: append([]byte(nil), payload...)}}
		},
		OnClose: func(info wsconn.CloseInfo) {
			client.events <- relayTestClientEvent{close: &info}
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
	case event := <-c.events:
		if event.close != nil {
			t.Fatalf("expected downstream frame before close, got close %+v", *event.close)
		}
		return event.frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for downstream frame")
	}
	return relayTestClientFrame{}
}

func (c *relayTestManagedClient) readClose(t *testing.T) wsconn.CloseInfo {
	t.Helper()
	select {
	case event := <-c.events:
		if event.close == nil {
			t.Fatalf("expected downstream close before frame, got frame %+v", event.frame)
		}
		return *event.close
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for downstream close")
	}
	return wsconn.CloseInfo{}
}

func TestRelayTestManagedClientKeepsFrameCloseOrder(t *testing.T) {
	client := &relayTestManagedClient{events: make(chan relayTestClientEvent, 2)}
	info := wsconn.CloseInfo{Kind: wsconn.CloseKindPeerClose, Code: wsconn.CloseNormalClosure}
	client.events <- relayTestClientEvent{frame: relayTestClientFrame{messageType: wsconn.TextMessage, payload: []byte("terminal")}}
	client.events <- relayTestClientEvent{close: &info}
	if got := client.readFrame(t); string(got.payload) != "terminal" {
		t.Fatalf("queued terminal frame lost: %+v", got)
	}
	if got := client.readClose(t); got.Code != wsconn.CloseNormalClosure {
		t.Fatalf("queued close lost: %+v", got)
	}
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

	model.ChannelGroup = buildRealtimeNativeWSTestChannelGroup(t, defaultChannelID, affinityChannelID)

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

	model.ChannelGroup = buildRealtimeNativeWSTestChannelGroup(t, defaultChannelID)

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

	routedChannel := newRelayNativeCodexChannel(t, affinityChannelID)
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

	model.ChannelGroup = buildRealtimeNativeWSTestChannelGroup(t, defaultChannelID)

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

	setupRelayTestDB(t, &model.Channel{})

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

	pinnedChannel := newRelayNativeCodexChannel(t, pinnedChannelID)
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

	model.ChannelGroup = buildRealtimeNativeWSTestChannelGroup(t, defaultChannelID)

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
	ctx.Set("token_group", "default")
	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(channelGroupSnapshot) })
	weight := uint(1)
	proxy := ""
	model.ChannelGroup = buildRealtimeTestChannelGroupForChannels(&model.Channel{Id: 11, Type: config.ChannelTypeAnthropic, Status: config.ChannelStatusEnabled, Group: "default", Models: "gpt-5", Weight: &weight, Proxy: &proxy})

	relay := &RelayModeChatRealtime{
		relayBase: relayBase{c: ctx},
	}
	relay.setOriginalModel("gpt-5")

	if relay.openFreshRealtimeSession("", false, realtimeOpenRetryBudget()) {
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
	weight := uint(1)
	proxy := ""
	pinned := &model.Channel{Id: 11, Type: config.ChannelTypeAnthropic, Status: config.ChannelStatusEnabled, Group: "default", Models: "gpt-5", Weight: &weight, Proxy: &proxy}
	testDB := setupRelayTestDB(t, &model.Channel{})
	if err := testDB.Create(pinned).Error; err != nil {
		t.Fatalf("create pinned channel: %v", err)
	}
	ctx.Set("specific_channel_id", 11)
	ctx.Set("specific_channel_id_ignore", false)

	relay := &RelayModeChatRealtime{
		relayBase: relayBase{c: ctx},
	}
	relay.setOriginalModel("gpt-5")

	if relay.openFreshRealtimeSession("", false, realtimeOpenRetryBudget()) {
		t.Fatal("expected pinned unsupported realtime provider to fail fresh session opening")
	}
	if _, ok := ctx.Get("skip_channel_ids"); ok {
		t.Fatal("expected pinned unsupported provider not to continue retrying through skip_channel_ids")
	}
}

func TestRelayModeChatRealtimeOpenFreshRealtimeSessionPassesRequestContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := newRelayTestContext(nil)
	requestCtx := context.WithValue(ctx.Request.Context(), logger.RequestIdKey, "req-realtime-open")
	ctx.Request = ctx.Request.WithContext(requestCtx)

	relay := &RelayModeChatRealtime{
		relayBase: relayBase{c: ctx},
	}
	gotContext := relay.realtimeOpenContext()
	if got := gotContext.Value(logger.RequestIdKey); got != "req-realtime-open" {
		t.Fatalf("expected request id in realtime open context, got %v", got)
	}
}

func TestRealtimeRelayActorContextPreservesRequestValuesWithoutCancel(t *testing.T) {
	base, cancel := context.WithCancel(context.WithValue(context.Background(), logger.RequestIdKey, "req-realtime-actor"))
	actor := newRealtimeRelayActorWithContext(base, nil, nil, time.Second)
	defer actor.cancel()
	cancel()

	if got := actor.ctx.Value(logger.RequestIdKey); got != "req-realtime-actor" {
		t.Fatalf("expected request id to be preserved, got %v", got)
	}
	select {
	case <-actor.ctx.Done():
		t.Fatal("expected actor context to ignore request cancellation")
	default:
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

func buildRealtimeNativeWSTestChannelGroup(t *testing.T, channelIDs ...int) model.ChannelsChooser {
	t.Helper()
	channels := make([]*model.Channel, 0, len(channelIDs))
	for _, id := range channelIDs {
		channels = append(channels, newRelayNativeCodexChannel(t, id))
	}
	return buildRealtimeTestChannelGroupForChannels(channels...)
}

func newRelayNativeCodexChannel(t *testing.T, channelID int) *model.Channel {
	t.Helper()
	channel := newRelayTestCodexChannel(channelID)
	upstreamURL, cleanup := wstest.Server(t, func(conn *wsconn.ManagedConn) {
		(&wsconn.Pump{Conn: conn, Handle: func(context.Context, wsconn.MessageType, []byte) {}}).Run(t.Context())
	})
	t.Cleanup(cleanup)
	channel.BaseURL = &upstreamURL
	return channel
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
		Other:  `{"self_hosted":true}`,
	}
}

func newRelayTestCodexProviderForChannel(t *testing.T, channel *model.Channel, headers map[string]string) *codex.CodexProvider {
	t.Helper()
	if channel.BaseURL == nil {
		upstream := newRelayNativeCodexChannel(t, channel.Id)
		channel.BaseURL = upstream.BaseURL
	}

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
		t.Fatalf("expected configured total attempt budget, got %d", got)
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

	calls := make([]runtimerealtime.RealtimeOpenOptions, 0, 2)
	provider := &relayTestRealtimeProvider{
		relayTestBaseProvider: relayTestBaseProvider{channel: newRelayTestCodexChannel(99)},
		openFn: func(modelName string, options runtimerealtime.RealtimeOpenOptions) (runtimerealtime.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
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
	session, apiErr := openRealtimeSessionWithFreshFallback(provider, "gpt-5", runtimerealtime.RealtimeOpenOptions{
		ClientSessionID: "session-123",
	})
	if apiErr != nil || session == nil {
		t.Fatalf("expected fresh fallback reopen to succeed, got session=%v err=%v", session, apiErr)
	}
	if len(calls) != 2 || calls[0].ForceFresh || !calls[1].ForceFresh {
		t.Fatalf("expected second realtime open attempt to force fresh, got %+v", calls)
	}

	if _, apiErr := openRealtimeSessionWithOptions(&relayTestBaseProvider{}, "gpt-5", runtimerealtime.RealtimeOpenOptions{}); apiErr == nil || apiErr.Message != "channel not implemented" {
		t.Fatalf("expected unsupported provider to return channel-not-implemented, got %v", apiErr)
	}
}

func TestRealtimeOpenRetryBudgetReadsLatestRuntimePublication(t *testing.T) {
	originalManager := config.GlobalOption
	manager := config.NewOptionManager()
	retryTimes := 0
	manager.RegisterIntOption("RetryTimes", &retryTimes, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"RetryTimes": "0"}); err != nil {
		t.Fatalf("publish initial retry budget: %v", err)
	}
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = originalManager })

	if got := realtimeOpenRetryBudget(); got != 1 {
		t.Fatalf("initial attempt budget=%d, want 1", got)
	}
	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{"RetryTimes": "3"}); err != nil {
		t.Fatalf("publish updated retry budget: %v", err)
	}
	if got := realtimeOpenRetryBudget(); got != 3 {
		t.Fatalf("a later open should observe the newer publication, budget=%d", got)
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

func TestRealtimeRelayFrameCreditTransfersAndReleases(t *testing.T) {
	clientBudget := runtimerealtime.NewByteBudget(8)
	pendingBudget := runtimerealtime.NewByteBudget(8)
	credit, ok := clientBudget.TryAcquire(8)
	if !ok {
		t.Fatal("expected client stage credit")
	}
	frame := realtimeRelayClientFrame{mt: wsconn.TextMessage, payload: make([]byte, 8), credit: credit}
	if _, ok := clientBudget.TryAcquire(1); ok {
		t.Fatal("client stage exceeded its byte budget")
	}
	if !frame.transferCredit(pendingBudget) {
		t.Fatal("expected handoff to acquire destination before releasing source")
	}
	if got := clientBudget.Used(); got != 0 {
		t.Fatalf("expected source credit released after handoff, got %d", got)
	}
	if got := pendingBudget.Used(); got != 8 {
		t.Fatalf("expected destination to own frame bytes, got %d", got)
	}
	frame.release()
	frame.release()
	if got := pendingBudget.Used(); got != 0 {
		t.Fatalf("expected idempotent release to restore destination budget, got %d", got)
	}
}

func TestRealtimeRelayFrameCreditFailedHandoffKeepsSourceOwnership(t *testing.T) {
	clientBudget := runtimerealtime.NewByteBudget(8)
	pendingBudget := runtimerealtime.NewByteBudget(4)
	credit, ok := clientBudget.TryAcquire(8)
	if !ok {
		t.Fatal("expected client stage credit")
	}
	frame := realtimeRelayClientFrame{mt: wsconn.TextMessage, payload: make([]byte, 8), credit: credit}
	if frame.transferCredit(pendingBudget) {
		t.Fatal("oversized handoff unexpectedly succeeded")
	}
	if got := clientBudget.Used(); got != 8 {
		t.Fatalf("failed handoff released source credit early, got %d", got)
	}
	if got := pendingBudget.Used(); got != 0 {
		t.Fatalf("failed handoff leaked destination credit, got %d", got)
	}
	frame.release()
}

func TestRealtimeRelayActorProviderFrameWritesDownstream(t *testing.T) {
	serverConn, client := newRelayWebsocketPair(t)
	actor := newRealtimeRelayActor(serverConn, newRelayActorTestSession(), time.Second)

	textFrame := runtimerealtime.NewTextFrame([]byte(`{"type":"response.text.delta","delta":"hi"}`))
	if !actor.deliverEventFrame(runtimerealtime.RecvEvent{
		Frame:  &textFrame,
		Origin: runtimerealtime.RealtimePayloadOriginProvider,
	}) {
		t.Fatal("expected text provider frame to be delivered")
	}
	frame := client.readFrame(t)
	if frame.messageType != wsconn.TextMessage || string(frame.payload) != string(textFrame.Payload()) {
		t.Fatalf("unexpected downstream text frame mt=%d payload=%s", frame.messageType, frame.payload)
	}

	binaryFrame := runtimerealtime.NewBinaryFrame([]byte{1, 2, 3})
	if !actor.deliverEventFrame(runtimerealtime.RecvEvent{
		Frame:  &binaryFrame,
		Origin: runtimerealtime.RealtimePayloadOriginProvider,
	}) {
		t.Fatal("expected binary provider frame to be delivered")
	}
	frame = client.readFrame(t)
	if frame.messageType != wsconn.BinaryMessage || string(frame.payload) != string(binaryFrame.Payload()) {
		t.Fatalf("unexpected downstream binary frame mt=%d payload=%v", frame.messageType, frame.payload)
	}

	var observedPayload string
	actor.providerPayloadObserver = func(_ wsconn.MessageType, payload []byte) {
		observedPayload = string(payload)
	}
	errorFrame := runtimerealtime.NewTextFrame([]byte(`{"type":"error","status_code":401,"error":{"type":"authentication_error","code":"invalid_api_key","message":"account org-secret rejected"},"account_id":"acct-secret"}`))
	if !actor.deliverEventFrame(runtimerealtime.RecvEvent{Frame: &errorFrame, Origin: runtimerealtime.RealtimePayloadOriginProvider}) {
		t.Fatal("expected provider error frame to be delivered")
	}
	frame = client.readFrame(t)
	if !strings.Contains(string(frame.payload), `"code":"invalid_api_key"`) || strings.Contains(string(frame.payload), "org-secret") {
		t.Fatalf("expected safe downstream provider error, got %s", frame.payload)
	}
	if !strings.Contains(observedPayload, "invalid_api_key") || !strings.Contains(observedPayload, "org-secret") {
		t.Fatalf("control-plane observer must retain original provider evidence, got %q", observedPayload)
	}
}

func TestRealtimeRelayActorContinuesAfterRecoverableTypedClientError(t *testing.T) {
	serverConn, client := newRelayWebsocketPair(t)
	session := &relayActorRecoverableSendSession{accepted: make(chan runtimerealtime.Frame, 1)}
	actor := newRealtimeRelayActor(serverConn, session, time.Second)
	actor.workers.Add(1)
	go actor.runWorker(actor.clientToSession)

	actor.clientFrames <- realtimeRelayClientFrame{mt: wsconn.TextMessage, payload: []byte(`{"type":"response.create","previous_response_id":"resp_stale"}`)}
	errorFrame := client.readFrame(t)
	if !strings.Contains(string(errorFrame.payload), `"previous_response_not_found"`) || strings.Contains(string(errorFrame.payload), `"system_error"`) {
		t.Fatalf("expected typed stale-continuation payload without generic fallback, got %s", errorFrame.payload)
	}

	secondPayload := []byte(`{"type":"response.create","input":"full context"}`)
	actor.clientFrames <- realtimeRelayClientFrame{mt: wsconn.TextMessage, payload: secondPayload}
	select {
	case accepted := <-session.accepted:
		if string(accepted.Payload()) != string(secondPayload) {
			t.Fatalf("expected second request to reach the session unchanged, got %s", accepted.Payload())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second request after recoverable client error")
	}
	select {
	case exit := <-actor.exitCh:
		t.Fatalf("recoverable client error must not exit the actor, got %+v", exit)
	default:
	}

	close(actor.clientFrames)
	actor.workers.Wait()
}

func TestRealtimeRelayActorDispatchesOptInControlWhileCreateIsBlocked(t *testing.T) {
	session := &relayActorConcurrentControlSession{
		createStarted:  make(chan struct{}),
		createRelease:  make(chan struct{}),
		controlHandled: make(chan struct{}),
		controlReturn:  make(chan struct{}),
		serialFrames:   make(chan runtimerealtime.Frame, 1),
	}
	actor := newRealtimeRelayActor(nil, session, time.Second)
	actor.workers.Add(1)
	go actor.runWorker(actor.clientToSession)

	actor.clientFrames <- realtimeRelayClientFrame{mt: wsconn.TextMessage, payload: []byte(`{"type":"response.create","input":"hello"}`)}
	select {
	case <-session.createStarted:
	case <-time.After(time.Second):
		t.Fatal("response.create did not start")
	}
	queued := []byte(`{"type":"session.update"}`)
	actor.clientFrames <- realtimeRelayClientFrame{mt: wsconn.TextMessage, payload: queued}
	actor.clientFrames <- realtimeRelayClientFrame{mt: wsconn.TextMessage, payload: []byte(`{"type":"response.cancel"}`)}

	select {
	case <-session.controlHandled:
	case <-time.After(time.Second):
		t.Fatal("response.cancel waited behind the blocked create")
	}
	select {
	case frame := <-session.serialFrames:
		t.Fatalf("ordinary frame overtook the active create: %s", frame.Payload())
	default:
	}
	close(session.controlReturn)
	select {
	case frame := <-session.serialFrames:
		if string(frame.Payload()) != string(queued) {
			t.Fatalf("queued ordinary frame changed: %s", frame.Payload())
		}
	case <-time.After(time.Second):
		t.Fatal("queued ordinary frame was not sent after create cancellation")
	}

	close(actor.clientFrames)
	actor.workers.Wait()
	select {
	case exit := <-actor.exitCh:
		t.Fatalf("successful concurrent control exited actor: %+v", exit)
	default:
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

	exit := actor.providerCloseExit(&runtimerealtime.ProviderClose{
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

func TestRealtimeRelayActorProviderCloseRedactsReason(t *testing.T) {
	actor := newRealtimeRelayActor(nil, newRelayActorTestSession(), time.Second)
	exit := actor.providerCloseExit(&runtimerealtime.ProviderClose{
		Code:   4408,
		Reason: "organization org-secret access_token=provider-secret",
	}, nil)
	if strings.Contains(exit.downstreamCloseReason, "org-secret") || strings.Contains(exit.downstreamCloseReason, "provider-secret") {
		t.Fatalf("provider close reason leaked: %q", exit.downstreamCloseReason)
	}
	if !strings.Contains(exit.downstreamCloseReason, "organization [redacted]") {
		t.Fatalf("provider close diagnostic lost safe context: %q", exit.downstreamCloseReason)
	}
}

func TestRealtimeRelayActorProviderCloseClosesDownstreamAtRecvPoint(t *testing.T) {
	serverConn, client := newRelayWebsocketPair(t)
	session := newRelayActorTestSession()
	actor := newRealtimeRelayActor(serverConn, session, time.Second)
	actor.workers.Add(1)
	go actor.runWorker(actor.sessionToClient)

	session.recvCh <- runtimerealtime.RecvEvent{
		ProviderClose: &runtimerealtime.ProviderClose{
			Code:   4408,
			Reason: "session_expired",
			Err:    runtimerealtime.ErrSessionClosed,
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
