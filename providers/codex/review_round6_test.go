package codex

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/model"
)

func TestAutoRefreshRecoversPendingRotationBeforeOAuth(t *testing.T) {
	channelID := 99601
	expected := credentialRound5TestKey(t, "old", "one-time-refresh")
	rotatedCredentials := &OAuth2Credentials{AccessToken: "rotated", RefreshToken: "next-refresh", ExpiresAt: time.Now().Add(time.Hour)}
	rotated, err := rotatedCredentials.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := rememberPendingCredentialCommit(channelID, expected, rotated); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clearPendingCredentialCommit(channelID, rotated) })

	dbKey := expected
	originalCAS, originalLoad, originalOAuth := compareAndSetChannelKey, loadLatestChannelByID, refreshOAuthCredentials
	var oauthCalls atomic.Int32
	compareAndSetChannelKey = func(_ context.Context, _ int, oldKey, newKey string) (bool, error) {
		if dbKey != oldKey {
			return false, nil
		}
		dbKey = newKey
		return true, nil
	}
	loadLatestChannelByID = func(context.Context, int) (*model.Channel, error) {
		return &model.Channel{Id: channelID, Type: config.ChannelTypeCodex, Key: dbKey}, nil
	}
	refreshOAuthCredentials = func(*OAuth2Credentials, context.Context, string) error {
		oauthCalls.Add(1)
		return errors.New("must not be called")
	}
	t.Cleanup(func() {
		compareAndSetChannelKey, loadLatestChannelByID, refreshOAuthCredentials = originalCAS, originalLoad, originalOAuth
	})

	provider := CodexProviderFactory{}.Create(&model.Channel{Id: channelID, Type: config.ChannelTypeCodex, Key: expected}).(*CodexProvider)
	refreshed, err := provider.refreshTokenIfNeeded(context.Background(), 3*time.Minute)
	if err != nil {
		t.Fatalf("pending recovery failed: %v", err)
	}
	if refreshed {
		t.Fatal("pending commit is recovery, not a second OAuth refresh")
	}
	if oauthCalls.Load() != 0 || dbKey != rotated {
		t.Fatalf("expected pending CAS without OAuth: oauth=%d dbKey=%q", oauthCalls.Load(), dbKey)
	}
}

func TestOAuthFailureDoesNotCreatePendingCredentialJournal(t *testing.T) {
	channelID := 99605
	expected := credentialRound5TestKey(t, "old", "refresh")
	provider := CodexProviderFactory{}.Create(&model.Channel{Id: channelID, Type: config.ChannelTypeCodex, Key: expected}).(*CodexProvider)
	originalOAuth := refreshOAuthCredentials
	refreshOAuthCredentials = func(*OAuth2Credentials, context.Context, string) error { return errors.New("oauth failed") }
	t.Cleanup(func() { refreshOAuthCredentials = originalOAuth })

	if err := provider.refreshCredentials(context.Background()); err == nil {
		t.Fatal("expected OAuth failure")
	}
	if _, ok := getPendingCredentialCommit(channelID); ok {
		t.Fatal("OAuth failure created a pending commit without rotated credentials")
	}
}
