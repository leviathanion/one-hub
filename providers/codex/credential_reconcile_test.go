package codex

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/model"
)

func isolatePendingCredentialJournal(t *testing.T) {
	t.Helper()
	pendingCredentialCommits.Lock()
	oldItems := pendingCredentialCommits.items
	pendingCredentialCommits.items = make(map[int]pendingCredentialCommit)
	pendingCredentialCommits.Unlock()
	ambiguousCredentialRefreshes.Lock()
	oldAmbiguous := ambiguousCredentialRefreshes.expectedKeys
	ambiguousCredentialRefreshes.expectedKeys = make(map[int]string)
	ambiguousCredentialRefreshes.Unlock()
	t.Cleanup(func() {
		pendingCredentialCommits.Lock()
		pendingCredentialCommits.items = oldItems
		pendingCredentialCommits.Unlock()
		ambiguousCredentialRefreshes.Lock()
		ambiguousCredentialRefreshes.expectedKeys = oldAmbiguous
		ambiguousCredentialRefreshes.Unlock()
	})
}

func TestAmbiguousOAuthOutcomeBlocksReuseUntilManualKeyChange(t *testing.T) {
	isolatePendingCredentialJournal(t)
	const channelID = 99820
	oldKey := credentialRound5TestKey(t, "old-access", "one-time-refresh")
	provider := CodexProviderFactory{}.Create(&model.Channel{Id: channelID, Key: oldKey}).(*CodexProvider)

	originalRefresh := refreshOAuthCredentials
	var calls atomic.Int32
	refreshOAuthCredentials = func(*OAuth2Credentials, context.Context, string) error {
		calls.Add(1)
		return ErrOAuthRefreshOutcomeAmbiguous
	}
	t.Cleanup(func() { refreshOAuthCredentials = originalRefresh })

	if err := provider.refreshCredentials(context.Background()); !errors.Is(err, ErrOAuthRefreshOutcomeAmbiguous) {
		t.Fatalf("expected ambiguous refresh result, got %v", err)
	}
	if err := provider.refreshCredentials(context.Background()); !errors.Is(err, ErrOAuthCredentialsRequireReauthorization) {
		t.Fatalf("old refresh token was not blocked after ambiguity: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("ambiguous refresh token was exchanged %d times", calls.Load())
	}

	provider.syncRuntimeKey(credentialRound5TestKey(t, "manual-access", "manual-refresh"))
	if err := requireUnambiguousCredentialRefresh(channelID, provider.codexChannel().Key); err != nil {
		t.Fatalf("manual credential replacement did not clear ambiguity: %v", err)
	}
}

func TestAutoRefreshReconcilesPendingCredentialBeforeNotDueSkip(t *testing.T) {
	isolatePendingCredentialJournal(t)
	cache.InitCacheManager()
	originalRedis := config.RedisEnabled
	config.RedisEnabled = false
	t.Cleanup(func() { config.RedisEnabled = originalRedis })

	const channelID = 99821
	expected := credentialRound5TestKey(t, "old-access", "one-time-refresh")
	rotatedCredentials := &OAuth2Credentials{
		AccessToken: "rotated-access", RefreshToken: "rotated-refresh",
		ExpiresAt: time.Now().Add(2 * time.Hour),
	}
	rotated, err := rotatedCredentials.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := rememberPendingCredentialCommit(channelID, expected, rotated); err != nil {
		t.Fatal(err)
	}

	dbKey := expected
	originalCAS := compareAndSetChannelKey
	originalLoadLatest := loadLatestChannelByID
	originalLoadChannels := loadAutoRefreshChannels
	originalRefresh := refreshOAuthCredentials
	var oauthCalls atomic.Int32
	compareAndSetChannelKey = func(_ context.Context, id int, oldKey, newKey string) (bool, error) {
		if id != channelID || dbKey != oldKey {
			return false, nil
		}
		dbKey = newKey
		return true, nil
	}
	loadLatestChannelByID = func(context.Context, int) (*model.Channel, error) {
		return &model.Channel{Id: channelID, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled, Key: dbKey}, nil
	}
	loadAutoRefreshChannels = func(context.Context) ([]*model.Channel, error) {
		return []*model.Channel{{Id: channelID, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled, Key: dbKey}}, nil
	}
	refreshOAuthCredentials = func(*OAuth2Credentials, context.Context, string) error {
		oauthCalls.Add(1)
		return nil
	}
	t.Cleanup(func() {
		compareAndSetChannelKey = originalCAS
		loadLatestChannelByID = originalLoadLatest
		loadAutoRefreshChannels = originalLoadChannels
		refreshOAuthCredentials = originalRefresh
	})

	summary := RefreshChannelsInBackground(context.Background())
	if summary.Failed != 0 || summary.SkippedNotDue != 1 {
		t.Fatalf("unexpected maintenance summary: %+v", summary)
	}
	if dbKey != rotated {
		t.Fatal("pending credential was not committed before due-time evaluation")
	}
	if oauthCalls.Load() != 0 {
		t.Fatalf("reconciliation performed a second OAuth exchange: %d", oauthCalls.Load())
	}
	if _, ok := getPendingCredentialCommit(channelID); ok {
		t.Fatal("durably committed journal entry was not cleared")
	}
}

func TestUsageCacheHitIsIndependentFromPendingCredentialJournal(t *testing.T) {
	isolatePendingCredentialJournal(t)
	cache.InitCacheManager()
	originalRedis := config.RedisEnabled
	config.RedisEnabled = false
	t.Cleanup(func() { config.RedisEnabled = originalRedis })

	channel := &model.Channel{
		Id: 99822, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled,
		CreatedTime: 123, Key: credentialRound5TestKey(t, "old-access", "old-refresh"),
	}
	generation, err := usageCacheGeneration(context.Background(), channel.Id)
	if err != nil {
		t.Fatal(err)
	}
	preview := &CodexUsagePreview{ChannelID: channel.Id, PlanType: "cached", FetchedAt: time.Now().Unix()}
	if err := cacheUsagePreviewForGeneration(context.Background(), channel, preview, generation, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := rememberPendingCredentialCommit(channel.Id, channel.Key, credentialRound5TestKey(t, "new-access", "new-refresh")); err != nil {
		t.Fatal(err)
	}

	provider := CodexProviderFactory{}.Create(channel).(*CodexProvider)
	got, err := provider.GetUsagePreview(context.Background(), false)
	if err != nil || got == nil || got.PlanType != "cached" {
		t.Fatalf("usage projection was not served: preview=%+v err=%v", got, err)
	}
	if _, ok := getPendingCredentialCommit(channel.Id); !ok {
		t.Fatal("usage cache path discarded credential recovery state")
	}
}
