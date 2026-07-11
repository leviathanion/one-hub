package codex

import (
	"context"
	"strings"
	"testing"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/model"
)

func TestTokenV2FingerprintRejectsOldProviderLateWrite(t *testing.T) {
	cache.InitCacheManager()
	channelID := 99701
	oldKey := `{"access_token":"old-secret","refresh_token":"old-refresh"}`
	newKey := `{"access_token":"new-secret","refresh_token":"new-refresh"}`

	oldProvider := CodexProviderFactory{}.Create(&model.Channel{Id: channelID, Type: config.ChannelTypeCodex, Key: oldKey}).(*CodexProvider)
	newProvider := CodexProviderFactory{}.Create(&model.Channel{Id: channelID, Type: config.ChannelTypeCodex, Key: newKey}).(*CodexProvider)

	// Simulate an old request completing after the durable key was manually
	// replaced and all known legacy keys were deleted.
	oldProvider.cacheCurrentToken(context.Background())
	if snapshot := newProvider.getCachedCredentialSnapshot(context.Background(), 0); snapshot.AccessToken != "" {
		t.Fatalf("new provider consumed old in-flight write: %+v", snapshot)
	}
	newProvider.cacheCurrentToken(context.Background())
	if snapshot := newProvider.getCachedCredentialSnapshot(context.Background(), 0); snapshot.AccessToken != "new-secret" {
		t.Fatalf("new provider could not consume its own fingerprint cache: %+v", snapshot)
	}

	oldPhysical := tokenCacheKeyV2(channelID, oldKey)
	newPhysical := tokenCacheKeyV2(channelID, newKey)
	if oldPhysical == newPhysical || strings.Contains(oldPhysical, "old-secret") || strings.Contains(oldPhysical, "old-refresh") {
		t.Fatalf("token physical keys must be distinct SHA-256 fingerprints: old=%q new=%q", oldPhysical, newPhysical)
	}
	if len(strings.TrimPrefix(oldPhysical, TokenCacheKey+":v2:")) < 64 {
		t.Fatalf("token physical key lacks SHA-256 digest: %q", oldPhysical)
	}
}
