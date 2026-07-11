package codex

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useCodexFenceDB(t *testing.T) {
	t.Helper()
	original := model.DB
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatal(err)
	}
	model.DB = db
	t.Cleanup(func() { model.DB = original })
	cache.InitCacheManager()
}

func insertCodexFenceProvider(t *testing.T, id int, credentials *OAuth2Credentials) *CodexProvider {
	t.Helper()
	key, err := credentials.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	channel := &model.Channel{Id: id, Type: config.ChannelTypeCodex, Key: key, Status: config.ChannelStatusEnabled, Name: "fence", Models: "gpt-5", Group: "default"}
	if err := model.DB.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	return CodexProviderFactory{}.Create(channel).(*CodexProvider)
}

func TestRotateOnceAmbiguousOutcomeDurablyBlocksEveryPeer(t *testing.T) {
	useCodexFenceDB(t)
	provider := insertCodexFenceProvider(t, 32001, &OAuth2Credentials{AccessToken: "old-access", RefreshToken: "one-time", ExpiresAt: time.Now().Add(-time.Minute)})

	originalRefresh := refreshOAuthCredentials
	var calls atomic.Int32
	refreshOAuthCredentials = func(*OAuth2Credentials, context.Context, string) error {
		calls.Add(1)
		return ErrOAuthRefreshOutcomeAmbiguous
	}
	t.Cleanup(func() { refreshOAuthCredentials = originalRefresh })

	if _, err := provider.refreshTokenIfNeeded(context.Background(), time.Minute); !errors.Is(err, ErrCredentialReauthorizationRequired) {
		t.Fatalf("ambiguous refresh error = %v", err)
	}
	snapshot, err := model.LoadCredentialRotationSnapshot(context.Background(), 32001)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Fence == nil || snapshot.Revision != 0 || snapshot.Key == "" {
		t.Fatalf("ambiguous exchange did not preserve fence: %+v", snapshot)
	}

	peerChannel, err := model.GetChannelById(32001)
	if err != nil {
		t.Fatal(err)
	}
	peer := CodexProviderFactory{}.Create(peerChannel).(*CodexProvider)
	if _, err := peer.refreshTokenIfNeeded(context.Background(), time.Minute); !errors.Is(err, ErrCredentialRefreshInProgress) && !errors.Is(err, ErrCredentialRefreshUnresolved) {
		t.Fatalf("peer fence error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fenced peer dispatched OAuth; calls=%d", got)
	}
}

func TestRotateOncePublishesOnlyAfterDurableCommit(t *testing.T) {
	useCodexFenceDB(t)
	provider := insertCodexFenceProvider(t, 32002, &OAuth2Credentials{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)})

	originalRefresh := refreshOAuthCredentials
	refreshOAuthCredentials = func(credentials *OAuth2Credentials, _ context.Context, _ string) error {
		credentials.AccessToken = "new-access"
		credentials.RefreshToken = "new-refresh"
		credentials.ExpiresAt = time.Now().Add(time.Hour)
		return nil
	}
	t.Cleanup(func() { refreshOAuthCredentials = originalRefresh })

	refreshed, err := provider.refreshTokenIfNeeded(context.Background(), time.Minute)
	if err != nil || !refreshed {
		t.Fatalf("refresh = %v, %v", refreshed, err)
	}
	snapshot, err := model.LoadCredentialRotationSnapshot(context.Background(), 32002)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := FromJSON(snapshot.Key)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Fence != nil || snapshot.Revision != 1 || durable.AccessToken != "new-access" || provider.Credentials.AccessToken != "new-access" {
		t.Fatalf("rotation was not atomically published: snapshot=%+v provider=%+v", snapshot, provider.Credentials)
	}
}
