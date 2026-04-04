package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/providers/codex"
	runtimeaffinity "one-api/runtime/channelaffinity"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type relayTestRealtimeSession struct{}

func init() {
	logger.Logger = zap.NewNop()
}

func (relayTestRealtimeSession) SendClient(context.Context, int, []byte) error { return nil }
func (relayTestRealtimeSession) Recv(context.Context) (int, []byte, *types.UsageEvent, error) {
	return 0, nil, nil, nil
}
func (relayTestRealtimeSession) Detach(string) {}
func (relayTestRealtimeSession) Abort(string)  {}
func (relayTestRealtimeSession) SetTurnObserverFactory(runtimesession.TurnObserverFactory) {
}

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

func newRelayWebsocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()

	serverConnCh := make(chan *websocket.Conn, 1)
	serverErrCh := make(chan error, 1)
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		serverConnCh <- conn
	}))
	t.Cleanup(func() {
		testServer.Close()
	})

	wsURL := "ws" + strings.TrimPrefix(testServer.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial websocket test server: %v", err)
	}
	t.Cleanup(func() {
		_ = clientConn.Close()
	})

	select {
	case err := <-serverErrCh:
		t.Fatalf("failed to upgrade websocket on test server: %v", err)
	case serverConn := <-serverConnCh:
		t.Cleanup(func() {
			_ = serverConn.Close()
		})
		return serverConn, clientConn
	}

	return nil, nil
}

func TestRealtimeClientSessionIDFromRequestPrefersExplicitHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	req.Header.Set("x-session-id", "execution-session-456")
	req.Header.Set("session_id", "legacy-session")

	if got := realtimeClientSessionIDFromRequest(req); got != "execution-session-456" {
		t.Fatalf("expected x-session-id to win, got %q", got)
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

	serverConn, clientConn := newRelayWebsocketPair(t)
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

	messageType, payload, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("expected strict affinity abort payload, got %v", err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("expected websocket abort payload, got type=%d", messageType)
	}
	if !strings.Contains(string(payload), "preferred realtime channel is unavailable") {
		t.Fatalf("expected strict affinity abort message, got %s", payload)
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

	serverConn, clientConn := newRelayWebsocketPair(t)
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

	messageType, payload, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("expected abort payload to reach websocket client, got %v", err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("expected text abort payload, got type=%d", messageType)
	}
	if !strings.Contains(string(payload), `"code":"quota_exhausted"`) || !strings.Contains(string(payload), `"message":"quota exhausted"`) {
		t.Fatalf("unexpected abort payload: %s", payload)
	}

	if _, _, err := clientConn.ReadMessage(); err == nil {
		t.Fatal("expected websocket connection to close after abort payload")
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

var _ providersBase.ProviderInterface = (*relayTestBaseProvider)(nil)
var _ providersBase.RealtimeSessionProvider = (*relayTestRealtimeProvider)(nil)
var _ providersBase.RealtimeSessionProviderWithOptions = (*relayTestRealtimeProvider)(nil)
