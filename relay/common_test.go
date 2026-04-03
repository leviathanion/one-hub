package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type fakeRelayStream struct {
	dataChan chan string
	errChan  chan error
}

var _ requester.StreamReaderInterface[string] = (*fakeRelayStream)(nil)

func (s *fakeRelayStream) Recv() (<-chan string, <-chan error) {
	return s.dataChan, s.errChan
}

func (s *fakeRelayStream) Close() {}

func TestResponseStreamClientDoesNotReturnMidStreamError(t *testing.T) {
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
		stream.errChan <- errors.New("upstream stream broken")
	}()

	firstResponseTime, errWithCode := responseStreamClient(ctx, stream, nil)
	if errWithCode != nil {
		t.Fatalf("expected nil error, got: %v", errWithCode.Message)
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
	if _, ok := lookupChannelAffinity(ctx, channelAffinityKindRealtime, sessionID); ok {
		t.Fatal("expected stale strict affinity binding to be cleared after rejection")
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
