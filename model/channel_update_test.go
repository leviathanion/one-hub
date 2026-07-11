package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"

	"go.uber.org/zap"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useTestChannelDB(t *testing.T) {
	t.Helper()

	if logger.Logger == nil {
		logger.SetupLogger()
	}

	originalDB := DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Channel{}); err != nil {
		t.Fatalf("expected channel schema migration for test database, got %v", err)
	}

	DB = testDB
	t.Cleanup(func() {
		DB = originalDB
	})
}

func insertTestChannel(t *testing.T, channel *Channel) {
	t.Helper()
	if err := DB.Create(channel).Error; err != nil {
		t.Fatalf("expected channel fixture to persist, got %v", err)
	}
}

func assertJSONObjectsEqual(t *testing.T, got string, want string) {
	t.Helper()
	var gotObject map[string]any
	if err := json.Unmarshal([]byte(got), &gotObject); err != nil {
		t.Fatalf("expected got value to be JSON object, got %q err=%v", got, err)
	}
	var wantObject map[string]any
	if err := json.Unmarshal([]byte(want), &wantObject); err != nil {
		t.Fatalf("expected want value to be JSON object, got %q err=%v", want, err)
	}
	if !reflect.DeepEqual(gotObject, wantObject) {
		t.Fatalf("JSON object mismatch:\ngot:  %s\nwant: %s", got, want)
	}
}

type testChannelGroupSnapshot struct {
	channels          map[int]*ChannelChoice
	rule              map[string]map[string][][]int
	match             []string
	modelGroup        map[string]map[string]bool
	cooldowns         map[any]any
	dirtyGeneration   uint64
	cleanGeneration   uint64
	publishGeneration uint64
}

func snapshotTestChannelGroup(t *testing.T) testChannelGroupSnapshot {
	t.Helper()

	ChannelGroup.RLock()
	defer ChannelGroup.RUnlock()

	snapshot := testChannelGroupSnapshot{
		channels:          ChannelGroup.Channels,
		rule:              ChannelGroup.Rule,
		match:             append([]string(nil), ChannelGroup.Match...),
		modelGroup:        ChannelGroup.ModelGroup,
		cooldowns:         make(map[any]any),
		dirtyGeneration:   ChannelGroup.dirtyGeneration.Load(),
		cleanGeneration:   ChannelGroup.cleanGeneration.Load(),
		publishGeneration: ChannelGroup.publishGeneration.Load(),
	}
	ChannelGroup.Cooldowns.Range(func(key, value any) bool {
		snapshot.cooldowns[key] = value
		return true
	})
	return snapshot
}

func restoreTestChannelGroup(t *testing.T, snapshot testChannelGroupSnapshot) {
	t.Helper()

	ChannelGroup.Lock()
	defer ChannelGroup.Unlock()

	ChannelGroup.Channels = snapshot.channels
	ChannelGroup.Rule = snapshot.rule
	ChannelGroup.Match = append([]string(nil), snapshot.match...)
	ChannelGroup.ModelGroup = snapshot.modelGroup
	ChannelGroup.Cooldowns = sync.Map{}
	for key, value := range snapshot.cooldowns {
		ChannelGroup.Cooldowns.Store(key, value)
	}
	ChannelGroup.dirtyGeneration.Store(snapshot.dirtyGeneration)
	ChannelGroup.cleanGeneration.Store(snapshot.cleanGeneration)
	ChannelGroup.publishGeneration.Store(snapshot.publishGeneration)
}

func requireChannelGroupLoad(t *testing.T) {
	t.Helper()

	if err := ChannelGroup.Load(); err != nil {
		t.Fatalf("expected channel group load to succeed, got %v", err)
	}
}

func failNextChannelQuery(t *testing.T, expectedErr error) {
	t.Helper()

	failChannelQueries(t, expectedErr, 1)
}

func failChannelQueries(t *testing.T, expectedErr error, count int) {
	t.Helper()

	failChannelQueriesAfter(t, expectedErr, 0, count)
}

func failChannelQueriesAfter(t *testing.T, expectedErr error, skip int, count int) {
	t.Helper()

	failuresRemaining := count
	callbackName := "test:fail_channel_query:" + t.Name()
	if err := DB.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if skip > 0 {
			skip--
			return
		}
		if failuresRemaining <= 0 {
			return
		}
		failuresRemaining--
		tx.AddError(expectedErr)
	}); err != nil {
		t.Fatalf("expected query failure callback registration to succeed, got %v", err)
	}
	t.Cleanup(func() {
		_ = DB.Callback().Query().Remove(callbackName)
	})
}

type blockedChannelQuery struct {
	started     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func newBlockedChannelQuery() *blockedChannelQuery {
	return &blockedChannelQuery{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *blockedChannelQuery) unblock() {
	b.releaseOnce.Do(func() {
		close(b.release)
	})
}

func blockChannelQueries(t *testing.T, blocks ...*blockedChannelQuery) {
	t.Helper()

	nextBlock := 0
	var mu sync.Mutex
	callbackName := "test:block_channel_query:" + t.Name()
	if err := DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		mu.Lock()
		if nextBlock >= len(blocks) {
			mu.Unlock()
			return
		}
		block := blocks[nextBlock]
		nextBlock++
		mu.Unlock()

		close(block.started)
		<-block.release
	}); err != nil {
		t.Fatalf("expected query block callback registration to succeed, got %v", err)
	}
	t.Cleanup(func() {
		for _, block := range blocks {
			block.unblock()
		}
		_ = DB.Callback().Query().Remove(callbackName)
	})
}

func waitForBlockedQuery(t *testing.T, block *blockedChannelQuery, description string) {
	t.Helper()

	select {
	case <-block.started:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForLoadResult(t *testing.T, errCh <-chan error, description string) error {
	t.Helper()

	select {
	case err := <-errCh:
		return err
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
		return nil
	}
}

func stringPtr(value string) *string {
	return &value
}

func primeChannelDerivedCaches(t *testing.T, channelID int) {
	t.Helper()

	cacheEntries := map[string]string{
		fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelID):           "cached-token",
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelID):    "cached-preview",
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelID):     "cached-detail",
		fmt.Sprintf("%s:%d", codexUsageGenerationCacheKeyPrefix, channelID): "generation-before-mutation",
	}

	for key, value := range cacheEntries {
		if err := cache.SetCache(key, value, time.Minute); err != nil {
			t.Fatalf("expected cache priming to succeed for %s, got %v", key, err)
		}
	}
}

func assertChannelDerivedCachesCleared(t *testing.T, channelID int) {
	t.Helper()

	legacyKeys := []string{
		fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelID),
	}
	for _, key := range legacyKeys {
		if _, err := cache.GetCache[string](key); !errors.Is(err, cache.CacheNotFound) {
			t.Fatalf("expected legacy cache key %s to be cleared, got err=%v", key, err)
		}
	}
	generationKey := fmt.Sprintf("%s:%d", codexUsageGenerationCacheKeyPrefix, channelID)
	generation, err := cache.GetCache[string](generationKey)
	if err != nil || generation == "generation-before-mutation" || generation == "" {
		t.Fatalf("expected generation %s to be atomically rotated, value=%q err=%v", generationKey, generation, err)
	}
}

func assertChannelDerivedCachesPresent(t *testing.T, channelID int) {
	t.Helper()

	cacheKeys := []string{
		fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsageGenerationCacheKeyPrefix, channelID),
	}

	for _, key := range cacheKeys {
		if _, err := cache.GetCache[string](key); err != nil {
			t.Fatalf("expected cache key %s to still exist, got err=%v", key, err)
		}
	}
}

func TestChannelCredentialDatabaseOperationsHonorCanceledContext(t *testing.T) {
	useTestChannelDB(t)
	insertTestChannel(t, &Channel{Id: 991, Type: config.ChannelTypeCodex, Name: "codex", Key: "old-key"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := GetChannelByIdWithContext(ctx, 991); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled credential load, got %v", err)
	}
	if err := UpdateChannelKeyWithContext(ctx, 991, "new-key"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled credential save, got %v", err)
	}

	persisted, err := GetChannelById(991)
	if err != nil {
		t.Fatalf("expected channel lookup after cancellation to succeed, got %v", err)
	}
	if persisted.Key != "old-key" {
		t.Fatalf("canceled save changed persisted credentials: got %q", persisted.Key)
	}
}

func TestChannelUpdateRawRejectsInvalidCodexOtherWhenTypeOmitted(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex",
		Key:    "sk-codex",
		Group:  "default",
		Models: "gpt-5",
	})

	update := &Channel{
		Id:    1,
		Other: `{"prompt_cache_key_strategy":`,
	}
	if err := update.UpdateRaw(false); err == nil {
		t.Fatal("expected invalid Codex other JSON to be rejected when update payload omits type")
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted channel lookup to succeed, got %v", err)
	}
	if persisted.Type != config.ChannelTypeCodex {
		t.Fatalf("expected persisted channel type to remain Codex, got %d", persisted.Type)
	}
	if persisted.Other != "" {
		t.Fatalf("expected rejected update not to mutate other, got %q", persisted.Other)
	}
}

func TestChannelUpdateRawOverwritePreservesPersistedTypeWhenTypeOmitted(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex",
		Key:    "sk-codex",
		Group:  "default",
		Models: "gpt-5",
	})

	update := &Channel{
		Id:     1,
		Name:   "codex-updated",
		Key:    "sk-codex-updated",
		Group:  "default",
		Models: "gpt-5",
	}
	if err := update.UpdateRaw(true); err != nil {
		t.Fatalf("expected overwrite update to succeed without zeroing type, got %v", err)
	}
	if update.Type != config.ChannelTypeCodex {
		t.Fatalf("expected in-memory channel type to be hydrated from persistence, got %d", update.Type)
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted channel lookup to succeed, got %v", err)
	}
	if persisted.Type != config.ChannelTypeCodex {
		t.Fatalf("expected overwrite update to preserve persisted type, got %d", persisted.Type)
	}
	if persisted.Name != "codex-updated" {
		t.Fatalf("expected overwrite update to persist requested fields, got %q", persisted.Name)
	}
}

func TestChannelUpdateRawClearsCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex",
		Key:    "sk-codex",
		Group:  "default",
		Models: "gpt-5",
	})
	primeChannelDerivedCaches(t, 1)

	update := &Channel{
		Id:   1,
		Name: "codex-updated",
	}
	if err := update.UpdateRaw(false); err != nil {
		t.Fatalf("expected channel update to succeed, got %v", err)
	}

	assertChannelDerivedCachesCleared(t, 1)
}

func TestChannelUpdateRawIgnoresMissingCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-4o",
		Other:  `{"api_version":"2024-05-01-preview"}`,
	})

	update := &Channel{
		Id:   1,
		Name: "azure-updated",
	}
	if err := update.UpdateRaw(false); err != nil {
		t.Fatalf("expected channel update to ignore missing derived caches, got %v", err)
	}
}

func TestUpdateChannelKeyClearsCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex",
		Key:    "sk-codex",
		Group:  "default",
		Models: "gpt-5",
	})
	primeChannelDerivedCaches(t, 1)

	if err := UpdateChannelKey(1, "sk-codex-updated"); err != nil {
		t.Fatalf("expected key update to succeed, got %v", err)
	}

	assertChannelDerivedCachesCleared(t, 1)
}

func TestUpdateChannelStatusClearsCodexDerivedCachesAfterCommit(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Status: config.ChannelStatusEnabled,
		Name:   "codex-status",
		Key:    "sk-codex",
		Group:  "default",
		Models: "gpt-5",
	})
	primeChannelDerivedCaches(t, 1)

	updated, err := UpdateChannelStatusIfCurrent(1, config.ChannelStatusEnabled, config.ChannelStatusAutoDisabled)
	if err != nil || !updated {
		t.Fatalf("status update failed: updated=%v err=%v", updated, err)
	}
	assertChannelDerivedCachesCleared(t, 1)
}

func TestUpdateChannelStatusIfCurrentUpdatesMatchingStatus(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Status: config.ChannelStatusAutoDisabled,
		Name:   "status-match",
		Key:    "sk-match",
		Group:  "default",
		Models: "gpt-5",
	})

	updated, err := UpdateChannelStatusIfCurrent(1, config.ChannelStatusAutoDisabled, config.ChannelStatusEnabled)
	if err != nil {
		t.Fatalf("expected conditional status update to succeed, got %v", err)
	}
	if !updated {
		t.Fatal("expected conditional status update to report a change")
	}

	channel, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected channel lookup to succeed, got %v", err)
	}
	if channel.Status != config.ChannelStatusEnabled {
		t.Fatalf("expected matching conditional status update to persist, got %d", channel.Status)
	}
}

func TestUpdateChannelStatusReloadsRoutingWhenEnablingPreviouslyUnloadedChannel(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() {
		restoreTestChannelGroup(t, snapshot)
	})

	insertTestChannel(t, &Channel{
		Id:     1,
		Status: config.ChannelStatusAutoDisabled,
		Name:   "status-enable-route",
		Key:    "sk-enable-route",
		Group:  "default",
		Models: "gpt-5",
	})
	requireChannelGroupLoad(t)

	if _, err := ChannelGroup.Next("default", "gpt-5"); err == nil {
		t.Fatal("expected disabled channel to be absent from routing before enable")
	}

	updated, err := UpdateChannelStatusIfCurrent(1, config.ChannelStatusAutoDisabled, config.ChannelStatusEnabled)
	if err != nil {
		t.Fatalf("expected conditional status update to succeed, got %v", err)
	}
	if !updated {
		t.Fatal("expected conditional status update to report a change")
	}

	channel, err := ChannelGroup.Next("default", "gpt-5")
	if err != nil {
		t.Fatalf("expected enabled channel to be routable after status update, got %v", err)
	}
	if channel == nil || channel.Id != 1 {
		t.Fatalf("expected channel 1 after route reload, got %#v", channel)
	}
}

func TestChannelGroupLoadSkipsInvalidRuntimeConfig(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() {
		restoreTestChannelGroup(t, snapshot)
	})

	insertTestChannel(t, &Channel{
		Id:     1,
		Status: config.ChannelStatusEnabled,
		Type:   config.ChannelTypeOpenAI,
		Name:   "legacy-invalid-other",
		Key:    "sk-invalid",
		Group:  "default",
		Models: "gpt-5",
		Other:  "2024-05-01-preview",
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Status: config.ChannelStatusEnabled,
		Type:   config.ChannelTypeOpenAI,
		Name:   "valid-json-other",
		Key:    "sk-valid",
		Group:  "default",
		Models: "gpt-5",
		Other:  `{"vendor_extra":{"api_version":"2024-05-01-preview"}}`,
	})
	requireChannelGroupLoad(t)

	if channel := ChannelGroup.GetChannel(1); channel != nil {
		t.Fatalf("expected invalid runtime config channel to be excluded, got %+v", channel)
	}
	channel, err := ChannelGroup.Next("default", "gpt-5")
	if err != nil || channel == nil || channel.Id != 2 {
		t.Fatalf("expected routing to use valid sibling channel, channel=%+v err=%v", channel, err)
	}
}

func TestUpdateChannelStatusReloadsRoutingWhenDisablingLastChannel(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() {
		restoreTestChannelGroup(t, snapshot)
	})

	insertTestChannel(t, &Channel{
		Id:     1,
		Status: config.ChannelStatusEnabled,
		Name:   "status-disable-route",
		Key:    "sk-disable-route",
		Group:  "default",
		Models: "gpt-5",
	})
	requireChannelGroupLoad(t)

	if channel, err := ChannelGroup.Next("default", "gpt-5"); err != nil || channel == nil || channel.Id != 1 {
		t.Fatalf("expected enabled channel to be routable before disable, channel=%#v err=%v", channel, err)
	}

	updated, err := UpdateChannelStatusIfCurrent(1, config.ChannelStatusEnabled, config.ChannelStatusAutoDisabled)
	if err != nil {
		t.Fatalf("expected conditional status update to succeed, got %v", err)
	}
	if !updated {
		t.Fatal("expected conditional status update to report a change")
	}

	if _, err := ChannelGroup.GetGroupModels("default"); err == nil {
		t.Fatal("expected route reload to remove the empty group/model index")
	}
	if _, err := ChannelGroup.Next("default", "gpt-5"); err == nil {
		t.Fatal("expected disabled last channel to be unroutable")
	}
}

func TestUpdateChannelStatusRoutesToTagSiblingWhenReloadFails(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() {
		restoreTestChannelGroup(t, snapshot)
	})

	insertTestChannel(t, &Channel{
		Id:     1,
		Status: config.ChannelStatusEnabled,
		Name:   "tag-sibling-disabled",
		Key:    "sk-tag-sibling-disabled",
		Group:  "codex",
		Models: "gpt-5",
		Tag:    "codex-proxy",
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Status: config.ChannelStatusEnabled,
		Name:   "tag-sibling-available",
		Key:    "sk-tag-sibling-available",
		Group:  "codex",
		Models: "gpt-5",
		Tag:    "codex-proxy",
	})
	requireChannelGroupLoad(t)

	loadErr := errors.New("forced sibling status reload failure")
	failNextChannelQuery(t, loadErr)
	updated, err := UpdateChannelStatusIfCurrent(1, config.ChannelStatusEnabled, config.ChannelStatusAutoDisabled)
	if err != nil {
		t.Fatalf("expected status update to keep committed DB mutation successful, got %v", err)
	}
	if !updated {
		t.Fatal("expected status update to report the committed row change")
	}

	channel, err := ChannelGroup.Next("codex", "gpt-5")
	if err != nil {
		t.Fatalf("expected disabled tag member to fail closed and route sibling, got %v", err)
	}
	if channel == nil || channel.Id != 2 {
		t.Fatalf("expected channel 2 after channel 1 was disabled, got %#v", channel)
	}
}

func TestChannelGroupLoadFailureMarksDirtyAndNextReadReloads(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() {
		restoreTestChannelGroup(t, snapshot)
	})

	insertTestChannel(t, &Channel{
		Id:     1,
		Status: config.ChannelStatusEnabled,
		Name:   "load-failure-preserve",
		Key:    "sk-load-failure-preserve",
		Group:  "default",
		Models: "gpt-5",
	})
	requireChannelGroupLoad(t)

	if err := DB.Model(&Channel{}).Where("id = ?", 1).Update("status", config.ChannelStatusAutoDisabled).Error; err != nil {
		t.Fatalf("expected fixture status update to succeed, got %v", err)
	}

	loadErr := errors.New("forced channel group load failure")
	failNextChannelQuery(t, loadErr)
	if err := ChannelGroup.Load(); !errors.Is(err, loadErr) {
		t.Fatalf("expected channel group load to return injected error, got %v", err)
	}
	if !ChannelGroup.isDirty() {
		t.Fatal("expected failed load to mark routing snapshot dirty")
	}

	if _, err := ChannelGroup.Next("default", "gpt-5"); err == nil {
		t.Fatal("expected next read to reload and remove disabled channel from routing")
	}
	if ChannelGroup.isDirty() {
		t.Fatal("expected successful read-triggered reload to clear dirty marker")
	}
}

func TestFailClosedGenerationRejectsOlderInFlightLoad(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() {
		restoreTestChannelGroup(t, snapshot)
	})

	insertTestChannel(t, &Channel{
		Id:     1,
		Status: config.ChannelStatusEnabled,
		Name:   "stale-load-fail-closed",
		Key:    "sk-stale-load",
		Group:  "default",
		Models: "gpt-5",
	})
	requireChannelGroupLoad(t)

	staleLoadBlock := newBlockedChannelQuery()
	blockChannelQueries(t, staleLoadBlock)

	staleLoadErr := make(chan error, 1)
	go func() {
		staleLoadErr <- ChannelGroup.Load()
	}()
	waitForBlockedQuery(t, staleLoadBlock, "stale load to read the old enabled row")

	if err := DB.Model(&Channel{}).Where("id = ?", 1).Update("status", config.ChannelStatusAutoDisabled).Error; err != nil {
		t.Fatalf("expected fixture status update to succeed, got %v", err)
	}
	ChannelGroup.failClosedChannels([]int{1})

	staleLoadBlock.unblock()
	if err := waitForLoadResult(t, staleLoadErr, "stale load to publish"); err != nil {
		t.Fatalf("expected stale load to succeed, got %v", err)
	}

	if _, err := ChannelGroup.Next("default", "gpt-5"); err == nil {
		t.Fatal("expected stale in-flight load to keep the disabled channel fail-closed")
	}
}

func TestChannelGroupLoadDoesNotClearNewerDirtyGeneration(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() {
		restoreTestChannelGroup(t, snapshot)
	})

	insertTestChannel(t, &Channel{
		Id:     1,
		Status: config.ChannelStatusEnabled,
		Name:   "dirty-generation",
		Key:    "sk-dirty-generation",
		Group:  "default",
		Models: "gpt-5",
	})
	requireChannelGroupLoad(t)

	loadBlock := newBlockedChannelQuery()
	blockChannelQueries(t, loadBlock)

	loadErr := make(chan error, 1)
	go func() {
		loadErr <- ChannelGroup.Load()
	}()
	waitForBlockedQuery(t, loadBlock, "load to read before dirty marker")

	ChannelGroup.markDirty()
	loadBlock.unblock()
	if err := waitForLoadResult(t, loadErr, "load with concurrent dirty marker"); err != nil {
		t.Fatalf("expected load to succeed, got %v", err)
	}
	if !ChannelGroup.isDirty() {
		t.Fatal("expected concurrent dirty marker to survive older successful load")
	}

	channel, err := ChannelGroup.Next("default", "gpt-5")
	if err != nil {
		t.Fatalf("expected dirty read to retry load successfully, got %v", err)
	}
	if channel == nil || channel.Id != 1 {
		t.Fatalf("expected channel 1 after dirty retry, got %#v", channel)
	}
	if ChannelGroup.isDirty() {
		t.Fatal("expected successful dirty retry to clear dirty marker")
	}
}

func TestUpdateChannelStatusReloadFailureRetriesOnNextAccess(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() {
		restoreTestChannelGroup(t, snapshot)
	})

	insertTestChannel(t, &Channel{
		Id:     1,
		Status: config.ChannelStatusAutoDisabled,
		Name:   "status-reload-failure",
		Key:    "sk-status-reload-failure",
		Group:  "default",
		Models: "gpt-5",
	})
	requireChannelGroupLoad(t)

	loadErr := errors.New("forced status reload failure")
	failNextChannelQuery(t, loadErr)
	updated, err := UpdateChannelStatusIfCurrent(1, config.ChannelStatusAutoDisabled, config.ChannelStatusEnabled)
	if err != nil {
		t.Fatalf("expected status update to keep committed DB mutation successful, got %v", err)
	}
	if !updated {
		t.Fatal("expected status update to report the committed row change")
	}
	if !ChannelGroup.isDirty() {
		t.Fatal("expected reload failure to mark routing snapshot dirty")
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted channel lookup to succeed, got %v", err)
	}
	if persisted.Status != config.ChannelStatusEnabled {
		t.Fatalf("expected DB status update to remain committed, got %d", persisted.Status)
	}
	channel, err := ChannelGroup.Next("default", "gpt-5")
	if err != nil {
		t.Fatalf("expected next access to retry reload and route enabled channel, got %v", err)
	}
	if channel == nil || channel.Id != 1 {
		t.Fatalf("expected channel 1 after read-triggered reload, got %#v", channel)
	}
	if ChannelGroup.isDirty() {
		t.Fatal("expected successful read-triggered reload to clear dirty marker")
	}
}

func TestUpdateChannelStatusFailClosesDisabledChannelWhenReloadsFail(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() {
		restoreTestChannelGroup(t, snapshot)
	})

	insertTestChannel(t, &Channel{
		Id:     1,
		Status: config.ChannelStatusEnabled,
		Name:   "status-disable-fail-closed",
		Key:    "sk-disable-fail-closed",
		Group:  "default",
		Models: "gpt-5",
	})
	requireChannelGroupLoad(t)

	loadErr := errors.New("forced disable reload failure")
	failChannelQueries(t, loadErr, 2)
	updated, err := UpdateChannelStatusIfCurrent(1, config.ChannelStatusEnabled, config.ChannelStatusAutoDisabled)
	if err != nil {
		t.Fatalf("expected status update to keep committed DB mutation successful, got %v", err)
	}
	if !updated {
		t.Fatal("expected status update to report the committed row change")
	}

	if _, err := ChannelGroup.Next("default", "gpt-5"); err == nil {
		t.Fatal("expected fail-closed disabled channel to be unroutable while reload is still failing")
	}
	if !ChannelGroup.isDirty() {
		t.Fatal("expected dirty marker to remain while read-triggered reload still fails")
	}

	if _, err := ChannelGroup.Next("default", "gpt-5"); err == nil {
		t.Fatal("expected successful retry reload to keep disabled channel unroutable")
	}
	if ChannelGroup.isDirty() {
		t.Fatal("expected successful retry reload to clear dirty marker")
	}
}

func TestDeleteChannelFailClosesDeletedChannelWhenReloadsFail(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() {
		restoreTestChannelGroup(t, snapshot)
	})

	insertTestChannel(t, &Channel{
		Id:     1,
		Status: config.ChannelStatusEnabled,
		Name:   "delete-fail-closed",
		Key:    "sk-delete-fail-closed",
		Group:  "default",
		Models: "gpt-5",
	})
	requireChannelGroupLoad(t)

	loadErr := errors.New("forced delete reload failure")
	failChannelQueriesAfter(t, loadErr, 1, 2)
	rowsAffected, err := BatchDeleteChannel([]int{1})
	if err != nil {
		t.Fatalf("expected delete to keep committed DB mutation successful, got %v", err)
	}
	if rowsAffected != 1 {
		t.Fatalf("expected one deleted channel, got %d", rowsAffected)
	}

	if _, err := ChannelGroup.Next("default", "gpt-5"); err == nil {
		t.Fatal("expected fail-closed deleted channel to be unroutable while reload is still failing")
	}
	if !ChannelGroup.isDirty() {
		t.Fatal("expected dirty marker to remain while read-triggered reload still fails")
	}

	if _, err := ChannelGroup.Next("default", "gpt-5"); err == nil {
		t.Fatal("expected successful retry reload to keep deleted channel unroutable")
	}
	if ChannelGroup.isDirty() {
		t.Fatal("expected successful retry reload to clear dirty marker")
	}
}

func TestUpdateChannelStatusIfCurrentSkipsStaleStatus(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     2,
		Status: config.ChannelStatusManuallyDisabled,
		Name:   "status-stale",
		Key:    "sk-stale",
		Group:  "default",
		Models: "gpt-5",
	})

	updated, err := UpdateChannelStatusIfCurrent(2, config.ChannelStatusAutoDisabled, config.ChannelStatusEnabled)
	if err != nil {
		t.Fatalf("expected stale conditional status update to return cleanly, got %v", err)
	}
	if updated {
		t.Fatal("expected stale conditional status update not to report a change")
	}

	channel, err := GetChannelById(2)
	if err != nil {
		t.Fatalf("expected channel lookup to succeed, got %v", err)
	}
	if channel.Status != config.ChannelStatusManuallyDisabled {
		t.Fatalf("expected stale conditional status update to preserve manual status, got %d", channel.Status)
	}
}

func TestChannelDeleteClearsCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-delete",
		Key:    "sk-delete",
		Group:  "default",
		Models: "gpt-5",
	})
	primeChannelDerivedCaches(t, 1)

	channel := &Channel{Id: 1}
	if err := channel.Delete(); err != nil {
		t.Fatalf("expected channel delete to succeed, got %v", err)
	}

	assertChannelDerivedCachesCleared(t, 1)
}

func TestBatchDeleteChannelClearsCodexDerivedCachesOnlyForCodexRows(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-batch-delete",
		Key:    "sk-delete-1",
		Group:  "default",
		Models: "gpt-5",
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Type:   3,
		Name:   "azure-batch-delete",
		Key:    "sk-delete-2",
		Group:  "default",
		Models: "gpt-4o",
	})
	primeChannelDerivedCaches(t, 1)
	primeChannelDerivedCaches(t, 2)

	rows, err := BatchDeleteChannel([]int{1, 2})
	if err != nil {
		t.Fatalf("expected batch delete to succeed, got %v", err)
	}
	if rows != 2 {
		t.Fatalf("expected two channels to be deleted, got %d", rows)
	}

	assertChannelDerivedCachesCleared(t, 1)
	assertChannelDerivedCachesPresent(t, 2)
}

func TestDeleteDisabledChannelClearsCodexDerivedCachesOnlyForDeletedRows(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Status: config.ChannelStatusAutoDisabled,
		Name:   "codex-disabled",
		Key:    "sk-disabled",
		Group:  "default",
		Models: "gpt-5",
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Type:   config.ChannelTypeCodex,
		Status: config.ChannelStatusEnabled,
		Name:   "codex-enabled",
		Key:    "sk-enabled",
		Group:  "default",
		Models: "gpt-5",
	})
	insertTestChannel(t, &Channel{
		Id:     3,
		Type:   3,
		Status: config.ChannelStatusManuallyDisabled,
		Name:   "azure-disabled",
		Key:    "sk-azure-disabled",
		Group:  "default",
		Models: "gpt-4o",
	})
	primeChannelDerivedCaches(t, 1)
	primeChannelDerivedCaches(t, 2)
	primeChannelDerivedCaches(t, 3)

	rows, err := DeleteDisabledChannel()
	if err != nil {
		t.Fatalf("expected delete disabled channels to succeed, got %v", err)
	}
	if rows != 2 {
		t.Fatalf("expected exactly two disabled channels to be deleted, got %d", rows)
	}

	assertChannelDerivedCachesCleared(t, 1)
	assertChannelDerivedCachesPresent(t, 2)
	assertChannelDerivedCachesPresent(t, 3)
}

func TestDeleteChannelsTagClearsCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-tag-delete-1",
		Key:    "sk-tag-delete-1",
		Group:  "default",
		Models: "gpt-5",
		Tag:    "codex-team",
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-tag-delete-2",
		Key:    "sk-tag-delete-2",
		Group:  "default",
		Models: "gpt-5",
		Tag:    "codex-team",
	})
	insertTestChannel(t, &Channel{
		Id:     3,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-other-tag",
		Key:    "sk-other-tag",
		Group:  "default",
		Models: "gpt-5",
		Tag:    "other-team",
	})
	primeChannelDerivedCaches(t, 1)
	primeChannelDerivedCaches(t, 2)
	primeChannelDerivedCaches(t, 3)

	if err := DeleteChannelsTag("codex-team", false); err != nil {
		t.Fatalf("expected tag delete to succeed, got %v", err)
	}

	assertChannelDerivedCachesCleared(t, 1)
	assertChannelDerivedCachesCleared(t, 2)
	assertChannelDerivedCachesPresent(t, 3)
}

func TestUpdateChannelsTagRejectsInvalidCodexOtherWhenTypeOmitted(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-tag",
		Key:    "sk-tagged",
		Group:  "default",
		Models: "gpt-5",
		Tag:    "codex-team",
	})

	if err := UpdateChannelsTag("codex-team", &Channel{
		Key:   "sk-tagged",
		Other: `{"prompt_cache_key_strategy":`,
	}); err == nil {
		t.Fatal("expected tag update to reject invalid Codex other JSON when payload omits type")
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted tagged channel lookup to succeed, got %v", err)
	}
	if persisted.Type != config.ChannelTypeCodex {
		t.Fatalf("expected tagged channel type to remain Codex, got %d", persisted.Type)
	}
	if persisted.Other != "" {
		t.Fatalf("expected rejected tag update not to mutate other, got %q", persisted.Other)
	}
}

func TestAddChannelToTagCreatesMemberWithInheritedConfig(t *testing.T) {
	useTestChannelDB(t)

	priority := int64(7)
	weight := uint(3)
	insertTestChannel(t, &Channel{
		Id:                 1,
		Type:               config.ChannelTypeOpenAI,
		Name:               "tagged-one",
		Key:                "sk-one",
		Status:             config.ChannelStatusEnabled,
		Weight:             &weight,
		Balance:            12.5,
		BalanceUpdatedTime: 111,
		UsedQuota:          222,
		ResponseTime:       333,
		TestTime:           444,
		Group:              "legacy",
		Models:             "gpt-old",
		Tag:                "member-team",
		BaseURL:            stringPtr("https://old.example"),
		Other:              `{"vendor_extra":{"legacy":"other"}}`,
		ModelMapping:       stringPtr(`{"old":"model"}`),
		ModelHeaders:       stringPtr(`{"X-Old":"1"}`),
		CustomParameter:    stringPtr(`{"old":true}`),
		Proxy:              stringPtr("http://proxy-%s.example"),
		TestModel:          "gpt-old",
		OnlyChat:           true,
		PreCost:            config.PreCostNotImage,
		CompatibleResponse: true,
		AllowExtraBody:     true,
		Priority:           &priority,
	})

	added, err := AddChannelToTag("member-team", &Channel{
		Key: "sk-two",
	})
	if err != nil {
		t.Fatalf("expected tag member create to succeed, got %v", err)
	}

	if added.Id == 0 {
		t.Fatal("expected inserted channel id to be returned")
	}
	if added.Name != "tagged-one_1" {
		t.Fatalf("expected generated member name to follow existing tag rule, got %q", added.Name)
	}
	if added.Key != "sk-two" || added.Tag != "member-team" || added.Type != config.ChannelTypeOpenAI {
		t.Fatalf("unexpected added channel identity: %+v", added)
	}
	if added.Models != "gpt-old" || added.Group != "legacy" || added.TestModel != "gpt-old" || !added.AllowExtraBody {
		t.Fatalf("expected new member to inherit tag config, got %+v", added)
	}
	if added.Status != config.ChannelStatusEnabled || added.Priority == nil || *added.Priority != priority || added.Weight == nil || *added.Weight != weight {
		t.Fatalf("expected lifecycle/routing fields to inherit representative values, got %+v", added)
	}
	if added.UsedQuota != 0 || added.ResponseTime != 0 || added.TestTime != 0 || added.Balance != 0 || added.BalanceUpdatedTime != 0 {
		t.Fatalf("expected runtime counters to reset, got %+v", added)
	}
	if added.Proxy == nil || *added.Proxy != "http://proxy-%s.example" {
		t.Fatalf("expected stored proxy template to remain unexpanded, got %+v", added.Proxy)
	}
	if ChannelGroup.GetChannel(added.Id) == nil {
		t.Fatal("expected routing cache to include added tag member")
	}
}

func TestAddChannelToTagAllowsDuplicateDisplayName(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeOpenAI,
		Name:   "same-name",
		Key:    "sk-one",
		Group:  "default",
		Models: "gpt-old",
		Tag:    "name-team",
	})

	added, err := AddChannelToTag("name-team", &Channel{
		Name: "same-name",
		Key:  "sk-two",
	})
	if err != nil {
		t.Fatalf("expected duplicate display name to remain allowed, got %v", err)
	}
	if added.Name != "same-name" {
		t.Fatalf("expected submitted display name to be preserved, got %q", added.Name)
	}
}

func TestAddChannelToTagRejectsMissingTagEmptyDuplicateAndMultilineKey(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeOpenAI,
		Name:   "tagged-one",
		Key:    "sk-one",
		Group:  "default",
		Models: "gpt-old",
		Tag:    "member-team",
	})

	if _, err := AddChannelToTag("missing-team", &Channel{Key: "sk-two"}); err == nil {
		t.Fatal("expected missing tag to be rejected")
	}
	if _, err := AddChannelToTag("member-team", &Channel{}); err == nil {
		t.Fatal("expected empty key to be rejected")
	}
	if _, err := AddChannelToTag("member-team", &Channel{Key: "sk-one"}); err == nil {
		t.Fatal("expected duplicate key to be rejected")
	}
	if _, err := AddChannelToTag("member-team", &Channel{Key: "sk-two\nsk-three"}); err == nil {
		t.Fatal("expected single-member create to reject multiline non-Codex key")
	}
}

func TestUpdateChannelsTagRejectsWhitespaceOnlyKeyWithoutDeletingMembers(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeOpenAI,
		Name:   "tagged-one",
		Key:    "sk-one",
		Group:  "default",
		Models: "gpt-old",
		Tag:    "whitespace-team",
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Type:   config.ChannelTypeOpenAI,
		Name:   "tagged-two",
		Key:    "sk-two",
		Group:  "default",
		Models: "gpt-old",
		Tag:    "whitespace-team",
	})

	err := UpdateChannelsTag("whitespace-team", &Channel{Key: " \n\t "})
	if err == nil || !strings.Contains(err.Error(), "key不能为空") {
		t.Fatalf("expected whitespace-only key to be rejected, got %v", err)
	}

	channels, err := GetChannelsByTag("whitespace-team")
	if err != nil {
		t.Fatalf("expected tagged channels lookup to succeed, got %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("expected rejected whitespace update to preserve members, got %d", len(channels))
	}
}

func TestAddChannelToTagNormalizesCodexJSONKeyAndRejectsDuplicate(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-one",
		Key:    `{"access_token":"old-token","refresh_token":"old-refresh"}`,
		Group:  "default",
		Models: "gpt-5",
		Tag:    "codex-team",
		Other:  `{"prompt_cache_key_strategy":"auto"}`,
	})

	prettyKey := "{\n  \"access_token\": \"new-token\",\n  \"refresh_token\": \"new-refresh\"\n}"
	added, err := AddChannelToTag("codex-team", &Channel{Key: prettyKey})
	if err != nil {
		t.Fatalf("expected pretty Codex JSON key to be accepted, got %v", err)
	}
	if strings.Contains(added.Key, "\n") {
		t.Fatalf("expected Codex key to be compacted, got %q", added.Key)
	}
	if added.Key != `{"access_token":"new-token","refresh_token":"new-refresh"}` {
		t.Fatalf("unexpected compacted Codex key: %q", added.Key)
	}

	if _, err := AddChannelToTag("codex-team", &Channel{Key: prettyKey}); err == nil {
		t.Fatal("expected duplicate pretty Codex JSON key to be rejected")
	}
}

func TestUpdateChannelsTagNormalizesSingleCodexPrettyJSONKey(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-one",
		Key:    `{"access_token":"old-token","refresh_token":"old-refresh"}`,
		Group:  "default",
		Models: "gpt-5",
		Tag:    "codex-team",
	})

	prettyExistingKey := "{\n  \"access_token\": \"old-token\",\n  \"refresh_token\": \"old-refresh\"\n}"
	if err := UpdateChannelsTag("codex-team", &Channel{
		Key:    prettyExistingKey,
		Group:  "default",
		Models: "gpt-5",
		Tag:    "codex-team",
	}); err != nil {
		t.Fatalf("expected tag update to accept single pretty Codex JSON key, got %v", err)
	}

	channels, err := GetChannelsByTag("codex-team")
	if err != nil {
		t.Fatalf("expected tagged channel lookup to succeed, got %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("expected duplicate-normalized pretty key not to add/delete members, got %d", len(channels))
	}
	if channels[0].Id != 1 || channels[0].Key != `{"access_token":"old-token","refresh_token":"old-refresh"}` {
		t.Fatalf("expected existing compact key to be preserved, got %+v", channels[0])
	}
}

func TestAddChannelToTagRejectsInvalidInheritedCodexOther(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-one",
		Key:    `{"access_token":"old-token"}`,
		Group:  "default",
		Models: "gpt-5",
		Tag:    "codex-team",
		Other:  `{"unsupported":true}`,
	})

	if _, err := AddChannelToTag("codex-team", &Channel{Key: `{"access_token":"new-token"}`}); err == nil {
		t.Fatal("expected invalid inherited Codex other config to be rejected")
	}
}

func TestUpdateChannelsTagRejectsNewMemberWithInvalidInheritedCodexOther(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-one",
		Key:    `{"access_token":"old-token"}`,
		Group:  "default",
		Models: "gpt-5",
		Tag:    "codex-team",
		Other:  `{"unsupported":true}`,
	})

	err := UpdateChannelsTagWithSubmittedFields("codex-team", &Channel{
		Key:    `{"access_token":"old-token"}` + "\n" + `{"access_token":"new-token"}`,
		Models: "gpt-5",
	}, ChannelTagSubmittedFields{
		"key":    struct{}{},
		"models": struct{}{},
	})
	if err == nil {
		t.Fatal("expected invalid inherited Codex other config to reject new tag member")
	}

	channels, lookupErr := GetChannelsByTag("codex-team")
	if lookupErr != nil {
		t.Fatalf("expected tagged channel lookup to succeed, got %v", lookupErr)
	}
	if len(channels) != 1 || channels[0].Key != `{"access_token":"old-token"}` {
		t.Fatalf("expected rejected tag update not to mutate members, got %+v", channels)
	}
}

func TestUpdateChannelsTagSynchronizesSubmittedTagConfigZeroValues(t *testing.T) {
	useTestChannelDB(t)

	priority := int64(9)
	weight := uint(3)
	oldDisabledStream := datatypes.JSONSlice[string]{"gpt-old"}
	insertTestChannel(t, &Channel{
		Id:                 1,
		Type:               config.ChannelTypeOpenAI,
		Name:               "tagged-one",
		Key:                "sk-one",
		Status:             config.ChannelStatusManuallyDisabled,
		Weight:             &weight,
		Balance:            12.5,
		BalanceUpdatedTime: 111,
		UsedQuota:          222,
		ResponseTime:       333,
		TestTime:           444,
		Group:              "legacy",
		Models:             "gpt-old",
		Tag:                "sync-team",
		BaseURL:            stringPtr("https://old.example"),
		Other:              `{"vendor_extra":{"legacy":"other"}}`,
		ModelMapping:       stringPtr(`{"old":"model"}`),
		ModelHeaders:       stringPtr(`{"X-Old":"1"}`),
		CustomParameter:    stringPtr(`{"old":true}`),
		Proxy:              stringPtr("http://old-proxy"),
		TestModel:          "gpt-old",
		OnlyChat:           true,
		PreCost:            config.PreCostNotImage,
		CompatibleResponse: true,
		AllowExtraBody:     true,
		DisabledStream:     &oldDisabledStream,
		Priority:           &priority,
	})
	insertTestChannel(t, &Channel{
		Id:                 2,
		Type:               config.ChannelTypeOpenAI,
		Name:               "tagged-two",
		Key:                "sk-two",
		Status:             config.ChannelStatusEnabled,
		Weight:             &weight,
		Balance:            23.5,
		BalanceUpdatedTime: 555,
		UsedQuota:          666,
		ResponseTime:       777,
		TestTime:           888,
		Group:              "legacy",
		Models:             "gpt-old",
		Tag:                "sync-team",
		BaseURL:            stringPtr("https://old.example"),
		Other:              `{"vendor_extra":{"legacy":"other"}}`,
		ModelMapping:       stringPtr(`{"old":"model"}`),
		ModelHeaders:       stringPtr(`{"X-Old":"1"}`),
		CustomParameter:    stringPtr(`{"old":true}`),
		Proxy:              stringPtr("http://old-proxy"),
		TestModel:          "gpt-old",
		OnlyChat:           true,
		PreCost:            config.PreCostNotImage,
		CompatibleResponse: true,
		AllowExtraBody:     true,
		DisabledStream:     &oldDisabledStream,
		Priority:           &priority,
	})

	empty := ""
	emptyObject := "{}"
	emptyDisabledStream := datatypes.JSONSlice[string]{}
	if err := UpdateChannelsTag("sync-team", &Channel{
		Name:               "ignored-name",
		Key:                "sk-one\nsk-two",
		Status:             config.ChannelStatusEnabled,
		Group:              "default",
		Models:             "gpt-new",
		Tag:                "sync-team",
		BaseURL:            &empty,
		Other:              "",
		ModelMapping:       &emptyObject,
		ModelHeaders:       &emptyObject,
		CustomParameter:    &empty,
		Proxy:              &empty,
		TestModel:          "",
		OnlyChat:           false,
		PreCost:            config.PreCostDefault,
		CompatibleResponse: false,
		AllowExtraBody:     false,
		DisabledStream:     &emptyDisabledStream,
	}); err != nil {
		t.Fatalf("expected tag update to succeed, got %v", err)
	}

	channels, err := GetChannelsByTag("sync-team")
	if err != nil {
		t.Fatalf("expected tagged channels lookup to succeed, got %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("expected two tagged channels, got %d", len(channels))
	}
	for _, channel := range channels {
		if channel.Models != "gpt-new" || channel.Group != "default" {
			t.Fatalf("expected routing config to sync, got models=%q group=%q", channel.Models, channel.Group)
		}
		if channel.BaseURL == nil || *channel.BaseURL != "" || channel.Other != "" || channel.TestModel != "" {
			t.Fatalf("expected string config to clear, got base_url=%v other=%q test_model=%q", channel.BaseURL, channel.Other, channel.TestModel)
		}
		if channel.ModelMapping == nil || *channel.ModelMapping != "{}" {
			t.Fatalf("expected model_mapping to clear to {}, got %#v", channel.ModelMapping)
		}
		if channel.ModelHeaders == nil || *channel.ModelHeaders != "{}" {
			t.Fatalf("expected model_headers to clear to {}, got %#v", channel.ModelHeaders)
		}
		if channel.CustomParameter == nil || *channel.CustomParameter != "" {
			t.Fatalf("expected custom_parameter to clear, got %#v", channel.CustomParameter)
		}
		if channel.Proxy == nil || *channel.Proxy != "" {
			t.Fatalf("expected proxy to clear, got %#v", channel.Proxy)
		}
		if channel.OnlyChat || channel.CompatibleResponse || channel.AllowExtraBody {
			t.Fatalf("expected bool config to clear, got only_chat=%v compatible_response=%v allow_extra_body=%v", channel.OnlyChat, channel.CompatibleResponse, channel.AllowExtraBody)
		}
		if channel.PreCost != config.PreCostDefault {
			t.Fatalf("expected pre_cost to sync, got %d", channel.PreCost)
		}
		if channel.DisabledStream == nil || len(*channel.DisabledStream) != 0 {
			t.Fatalf("expected disabled_stream to clear, got %#v", channel.DisabledStream)
		}
		if channel.Name != "tagged-one" && channel.Name != "tagged-two" {
			t.Fatalf("expected name to remain channel-local, got %q", channel.Name)
		}
		if channel.Weight == nil || *channel.Weight != weight {
			t.Fatalf("expected weight to remain channel-local, got %#v", channel.Weight)
		}
		if channel.Id == 1 && (channel.Status != config.ChannelStatusManuallyDisabled || channel.Balance != 12.5 || channel.UsedQuota != 222 || channel.ResponseTime != 333 || channel.TestTime != 444) {
			t.Fatalf("expected runtime fields to remain channel-local for channel 1, got %+v", channel)
		}
		if channel.Id == 2 && (channel.Status != config.ChannelStatusEnabled || channel.Balance != 23.5 || channel.UsedQuota != 666 || channel.ResponseTime != 777 || channel.TestTime != 888) {
			t.Fatalf("expected runtime fields to remain channel-local for channel 2, got %+v", channel)
		}
	}
}

func TestUpdateChannelsTagWithSubmittedFieldsPreservesOmittedTagConfig(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:                 1,
		Type:               config.ChannelTypeOpenAI,
		Name:               "tagged-one",
		Key:                "sk-one",
		Group:              "legacy",
		Models:             "gpt-old",
		Tag:                "partial-team",
		Other:              `{"vendor_extra":{"legacy":"other"}}`,
		TestModel:          "gpt-old",
		OnlyChat:           true,
		CompatibleResponse: true,
		AllowExtraBody:     true,
	})

	if err := UpdateChannelsTagWithSubmittedFields("partial-team", &Channel{
		Key:            "sk-one",
		Models:         "gpt-new",
		Other:          "",
		TestModel:      "",
		AllowExtraBody: false,
	}, ChannelTagSubmittedFields{"models": struct{}{}}); err != nil {
		t.Fatalf("expected partial tag update to succeed, got %v", err)
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted tagged channel lookup to succeed, got %v", err)
	}
	if persisted.Models != "gpt-new" {
		t.Fatalf("expected submitted models update, got %q", persisted.Models)
	}
	if persisted.Group != "legacy" || persisted.Other != `{"vendor_extra":{"legacy":"other"}}` || persisted.TestModel != "gpt-old" {
		t.Fatalf("expected omitted string fields to remain, got group=%q other=%q test_model=%q", persisted.Group, persisted.Other, persisted.TestModel)
	}
	if !persisted.OnlyChat || !persisted.CompatibleResponse || !persisted.AllowExtraBody {
		t.Fatalf("expected omitted bool fields to remain true, got only_chat=%v compatible_response=%v allow_extra_body=%v", persisted.OnlyChat, persisted.CompatibleResponse, persisted.AllowExtraBody)
	}
}

func TestChannelPartialUpdatePreservesRequiredVertexAIOther(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeVertexAI,
		Name:   "vertex",
		Key:    "vertex-key",
		Models: "gemini-old",
		Other:  `{"region":"us-central1","project_id":"project-a"}`,
	})

	update := &Channel{
		Id:     1,
		Type:   config.ChannelTypeUnknown,
		Models: "gemini-new",
		Other:  "",
	}
	if err := update.Update(false); err != nil {
		t.Fatalf("expected partial VertexAI update to preserve required other, got %v", err)
	}
	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted VertexAI channel lookup to succeed, got %v", err)
	}
	if persisted.Models != "gemini-new" || persisted.Other != `{"region":"us-central1","project_id":"project-a"}` {
		t.Fatalf("expected partial update to preserve VertexAI other while updating models, models=%q other=%q", persisted.Models, persisted.Other)
	}
}

func TestChannelOverwriteUpdatePreservesRequiredOtherWhenOmitted(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure",
		Key:    "azure-key",
		Group:  "default",
		Status: config.ChannelStatusEnabled,
		Models: "gpt-old",
		Other:  `{"api_version":"2024-05-01-preview","responses_ws_transport":"http_bridge"}`,
	})

	update := &Channel{
		Id:     1,
		Type:   config.ChannelTypeUnknown,
		Name:   "azure",
		Key:    "azure-key",
		Group:  "default",
		Status: config.ChannelStatusEnabled,
		Models: "gpt-new",
		Other:  "",
	}
	if err := update.UpdateRawWithOptions(true, ChannelUpdateOptions{OtherSubmitted: false}); err != nil {
		t.Fatalf("expected overwrite update with omitted Azure other to preserve persisted value, got %v", err)
	}
	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure channel lookup to succeed, got %v", err)
	}
	if persisted.Models != "gpt-new" || persisted.Other != `{"api_version":"2024-05-01-preview","responses_ws_transport":"http_bridge"}` {
		t.Fatalf("expected omitted other to be preserved while updating models, models=%q other=%q", persisted.Models, persisted.Other)
	}
}

func TestChannelOverwriteUpdatePreservesRequiredBaseURLWhenOmitted(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:      1,
		Type:    config.ChannelTypeAzureV1,
		Name:    "azure-v1",
		Key:     "azure-v1-key",
		Group:   "default",
		Status:  config.ChannelStatusEnabled,
		Models:  "gpt-old",
		Other:   `{}`,
		BaseURL: stringPtr("https://resource.openai.azure.com"),
	})

	update := &Channel{
		Id:     1,
		Type:   config.ChannelTypeUnknown,
		Name:   "azure-v1",
		Key:    "azure-v1-key",
		Group:  "default",
		Status: config.ChannelStatusEnabled,
		Models: "gpt-new",
		Other:  "",
	}
	if err := update.UpdateRawWithOptions(true, ChannelUpdateOptions{OtherSubmitted: false, BaseURLSubmitted: false}); err != nil {
		t.Fatalf("expected overwrite update with omitted Azure V1 base_url to preserve persisted value, got %v", err)
	}
	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure V1 channel lookup to succeed, got %v", err)
	}
	if persisted.Models != "gpt-new" || persisted.BaseURL == nil || *persisted.BaseURL != "https://resource.openai.azure.com" {
		t.Fatalf("expected omitted base_url to be preserved while updating models, got %+v", persisted)
	}
}

func TestChannelOverwriteUpdatePreservesOptionalBaseURLWhenOmitted(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:      1,
		Type:    config.ChannelTypeOpenAI,
		Name:    "openai",
		Key:     "sk-openai",
		Group:   "default",
		Status:  config.ChannelStatusEnabled,
		Models:  "gpt-old",
		Other:   `{}`,
		BaseURL: stringPtr("https://proxy.example.com/v1"),
	})

	update := &Channel{
		Id:     1,
		Type:   config.ChannelTypeUnknown,
		Name:   "openai",
		Key:    "sk-openai",
		Group:  "default",
		Status: config.ChannelStatusEnabled,
		Models: "gpt-new",
		Other:  "",
	}
	if err := update.UpdateRawWithOptions(true, ChannelUpdateOptions{OtherSubmitted: false, BaseURLSubmitted: false}); err != nil {
		t.Fatalf("expected overwrite update with omitted optional base_url to preserve persisted value, got %v", err)
	}
	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted OpenAI channel lookup to succeed, got %v", err)
	}
	if persisted.Models != "gpt-new" || persisted.BaseURL == nil || *persisted.BaseURL != "https://proxy.example.com/v1" {
		t.Fatalf("expected omitted optional base_url to be preserved while updating models, got %+v", persisted)
	}
}

func TestChannelOverwriteUpdateAllowsExplicitNullOptionalBaseURL(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:      1,
		Type:    config.ChannelTypeOpenAI,
		Name:    "openai",
		Key:     "sk-openai",
		Group:   "default",
		Status:  config.ChannelStatusEnabled,
		Models:  "gpt-old",
		Other:   `{}`,
		BaseURL: stringPtr("https://proxy.example.com/v1"),
	})

	update := &Channel{
		Id:      1,
		Type:    config.ChannelTypeOpenAI,
		Name:    "openai",
		Key:     "sk-openai",
		Group:   "default",
		Status:  config.ChannelStatusEnabled,
		Models:  "gpt-new",
		Other:   `{}`,
		BaseURL: nil,
	}
	if err := update.UpdateRawWithOptions(true, ChannelUpdateOptions{OtherSubmitted: true, BaseURLSubmitted: true}); err != nil {
		t.Fatalf("expected overwrite update with explicit null optional base_url to succeed, got %v", err)
	}
	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted OpenAI channel lookup to succeed, got %v", err)
	}
	if persisted.BaseURL != nil || persisted.Models != "gpt-new" {
		t.Fatalf("expected explicit null optional base_url to clear value, got %+v", persisted)
	}
}

func TestChannelOverwriteUpdatePreservesOptionalRuntimeOtherWhenOmitted(t *testing.T) {
	useTestChannelDB(t)

	const originalOther = `{"responses_ws_transport":"native","prompt_cache_key_strategy":"session_id"}`
	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex",
		Key:    "codex-key",
		Group:  "default",
		Status: config.ChannelStatusEnabled,
		Models: "gpt-old",
		Other:  originalOther,
	})

	update := &Channel{
		Id:     1,
		Type:   config.ChannelTypeUnknown,
		Name:   "codex",
		Key:    "codex-key",
		Group:  "default",
		Status: config.ChannelStatusEnabled,
		Models: "gpt-new",
		Other:  "",
	}
	if err := update.UpdateRawWithOptions(true, ChannelUpdateOptions{OtherSubmitted: false}); err != nil {
		t.Fatalf("expected overwrite update with omitted optional other to preserve persisted value, got %v", err)
	}
	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Codex channel lookup to succeed, got %v", err)
	}
	if persisted.Models != "gpt-new" || persisted.Other != originalOther {
		t.Fatalf("expected omitted optional other to be preserved while updating models, models=%q other=%q", persisted.Models, persisted.Other)
	}
}

func TestChannelOverwriteUpdateAllowsExplicitEmptyOptionalOther(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeOpenAI,
		Name:   "openai",
		Key:    "sk-openai",
		Group:  "default",
		Status: config.ChannelStatusEnabled,
		Models: "gpt-old",
		Other:  `{"responses_ws_transport":"http_bridge"}`,
	})

	update := &Channel{
		Id:     1,
		Type:   config.ChannelTypeOpenAI,
		Name:   "openai",
		Key:    "sk-openai",
		Group:  "default",
		Status: config.ChannelStatusEnabled,
		Models: "gpt-new",
		Other:  "",
	}
	if err := update.UpdateRawWithOptions(true, ChannelUpdateOptions{OtherSubmitted: true}); err != nil {
		t.Fatalf("expected explicit empty optional other to be accepted, got %v", err)
	}
	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted OpenAI channel lookup to succeed, got %v", err)
	}
	if persisted.Models != "gpt-new" || persisted.Other != "" {
		t.Fatalf("expected explicit empty optional other to clear value, models=%q other=%q", persisted.Models, persisted.Other)
	}
}

func TestChannelOverwriteUpdateRejectsTypeChangeWhenOtherOmitted(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeOpenAI,
		Name:   "openai",
		Key:    "sk-openai",
		Group:  "default",
		Status: config.ChannelStatusEnabled,
		Models: "gpt-old",
		Other:  `{"responses_ws_transport":"http_bridge"}`,
	})

	update := &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex",
		Key:    "codex-key",
		Group:  "default",
		Status: config.ChannelStatusEnabled,
		Models: "gpt-new",
		Other:  "",
	}
	if err := update.UpdateRawWithOptions(true, ChannelUpdateOptions{OtherSubmitted: false}); err == nil {
		t.Fatal("expected channel type change with omitted other to be rejected")
	}
	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted OpenAI channel lookup to succeed, got %v", err)
	}
	if persisted.Type != config.ChannelTypeOpenAI || persisted.Models != "gpt-old" || persisted.Other != `{"responses_ws_transport":"http_bridge"}` {
		t.Fatalf("expected rejected type change not to mutate channel, type=%d models=%q other=%q", persisted.Type, persisted.Models, persisted.Other)
	}
}

func TestChannelPartialUpdateWritesSubmittedZeroValueRuntimeConfig(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:      1,
		Type:    config.ChannelTypeOpenAI,
		Name:    "openai",
		Key:     "sk-openai",
		Group:   "default",
		Status:  config.ChannelStatusEnabled,
		Models:  "gpt-old",
		Other:   `{"responses_ws_transport":"http_bridge"}`,
		BaseURL: stringPtr("https://old.example.com"),
	})

	clearOther := &Channel{
		Id:     1,
		Type:   config.ChannelTypeUnknown,
		Status: config.ChannelStatusEnabled,
		Other:  "",
	}
	if err := clearOther.UpdateRawWithOptions(false, ChannelUpdateOptions{OtherSubmitted: true}); err != nil {
		t.Fatalf("expected partial update with explicit empty other to succeed, got %v", err)
	}
	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted OpenAI channel lookup to succeed, got %v", err)
	}
	if persisted.Other != "" || persisted.BaseURL == nil || *persisted.BaseURL != "https://old.example.com" {
		t.Fatalf("expected explicit other clear and omitted base_url preserve, got %+v", persisted)
	}

	clearBaseURL := &Channel{
		Id:      1,
		Type:    config.ChannelTypeUnknown,
		Status:  config.ChannelStatusEnabled,
		BaseURL: nil,
	}
	if err := clearBaseURL.UpdateRawWithOptions(false, ChannelUpdateOptions{BaseURLSubmitted: true}); err != nil {
		t.Fatalf("expected partial update with explicit null base_url to succeed, got %v", err)
	}
	persisted, err = GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted OpenAI channel lookup to succeed, got %v", err)
	}
	if persisted.BaseURL != nil || persisted.Other != "" {
		t.Fatalf("expected explicit base_url null to clear and omitted other to stay empty, got %+v", persisted)
	}
}

func TestChannelOverwriteUpdateRejectsExplicitEmptyRequiredOther(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure",
		Key:    "azure-key",
		Group:  "default",
		Status: config.ChannelStatusEnabled,
		Models: "gpt-old",
		Other:  `{"api_version":"2024-05-01-preview"}`,
	})

	update := &Channel{
		Id:     1,
		Type:   config.ChannelTypeUnknown,
		Name:   "azure",
		Key:    "azure-key",
		Group:  "default",
		Status: config.ChannelStatusEnabled,
		Models: "gpt-new",
		Other:  "",
	}
	if err := update.UpdateRawWithOptions(true, ChannelUpdateOptions{OtherSubmitted: true}); err == nil {
		t.Fatal("expected explicit empty Azure other to be rejected")
	}
	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure channel lookup to succeed, got %v", err)
	}
	if persisted.Models != "gpt-old" || persisted.Other != `{"api_version":"2024-05-01-preview"}` {
		t.Fatalf("expected rejected update not to mutate channel, models=%q other=%q", persisted.Models, persisted.Other)
	}
}

func TestUpdateChannelsTagPartialSubmitPreservesRequiredVertexAIOther(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeVertexAI,
		Name:   "vertex-tagged",
		Key:    "vertex-key",
		Models: "gemini-old",
		Tag:    "vertex-team",
		Other:  `{"region":"us-central1","project_id":"project-a"}`,
	})

	if err := UpdateChannelsTagWithSubmittedFields("vertex-team", &Channel{
		Key:    "vertex-key",
		Models: "gemini-new",
		Other:  "",
	}, ChannelTagSubmittedFields{"models": struct{}{}}); err != nil {
		t.Fatalf("expected partial VertexAI tag update to preserve required other, got %v", err)
	}
	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted tagged VertexAI lookup to succeed, got %v", err)
	}
	if persisted.Models != "gemini-new" || persisted.Other != `{"region":"us-central1","project_id":"project-a"}` {
		t.Fatalf("expected tag partial update to preserve VertexAI other while updating models, models=%q other=%q", persisted.Models, persisted.Other)
	}
}

func TestUpdateChannelsTagPartialSubmitNewMemberInheritsExistingTagConfig(t *testing.T) {
	useTestChannelDB(t)

	oldDisabledStream := datatypes.JSONSlice[string]{"gpt-old"}
	oldPlugin := datatypes.NewJSONType(PluginType{
		"claude": {
			"enabled":  true,
			"base_url": "https://old-plugin.example.com",
		},
	})
	insertTestChannel(t, &Channel{
		Id:                 1,
		Type:               config.ChannelTypeCustom,
		Name:               "tagged-one",
		Key:                "sk-one",
		Status:             config.ChannelStatusManuallyDisabled,
		Group:              "legacy",
		Models:             "gpt-old",
		Tag:                "partial-member-team",
		BaseURL:            stringPtr("https://old.example"),
		Other:              `{"vendor_extra":{"legacy":"other"}}`,
		ModelMapping:       stringPtr(`{"old":"model"}`),
		ModelHeaders:       stringPtr(`{"X-Old":"1"}`),
		CustomParameter:    stringPtr(`{"old":true}`),
		Proxy:              stringPtr("http://old-proxy"),
		TestModel:          "gpt-old",
		OnlyChat:           true,
		PreCost:            config.PreCostNotImage,
		CompatibleResponse: true,
		AllowExtraBody:     true,
		DisabledStream:     &oldDisabledStream,
		Plugin:             &oldPlugin,
	})

	if err := UpdateChannelsTagWithSubmittedFields("partial-member-team", &Channel{
		Key:    "sk-one\nsk-two",
		Models: "gpt-new",
	}, ChannelTagSubmittedFields{
		"key":    struct{}{},
		"models": struct{}{},
	}); err != nil {
		t.Fatalf("expected partial tag update with new key to succeed, got %v", err)
	}

	channels, err := GetChannelsByTag("partial-member-team")
	if err != nil {
		t.Fatalf("expected tagged channels lookup to succeed, got %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("expected two tagged channels after member add, got %d", len(channels))
	}

	channelsByKey := make(map[string]*Channel, len(channels))
	for _, channel := range channels {
		channelsByKey[channel.Key] = channel
	}
	for _, key := range []string{"sk-one", "sk-two"} {
		channel := channelsByKey[key]
		if channel == nil {
			t.Fatalf("expected tagged channel for key %q", key)
		}
		if channel.Models != "gpt-new" {
			t.Fatalf("expected submitted models to sync for %q, got %q", key, channel.Models)
		}
		if channel.Group != "legacy" || channel.Other != `{"vendor_extra":{"legacy":"other"}}` || channel.TestModel != "gpt-old" {
			t.Fatalf("expected omitted string config to be inherited for %q, got group=%q other=%q test_model=%q", key, channel.Group, channel.Other, channel.TestModel)
		}
		if channel.BaseURL == nil || *channel.BaseURL != "https://old.example" {
			t.Fatalf("expected omitted base_url to be inherited for %q, got %#v", key, channel.BaseURL)
		}
		if channel.Proxy == nil || *channel.Proxy != "http://old-proxy" {
			t.Fatalf("expected omitted proxy to be inherited for %q, got %#v", key, channel.Proxy)
		}
		if channel.CustomParameter == nil || *channel.CustomParameter != `{"old":true}` {
			t.Fatalf("expected omitted custom_parameter to be inherited for %q, got %#v", key, channel.CustomParameter)
		}
		if !channel.OnlyChat || !channel.CompatibleResponse || !channel.AllowExtraBody {
			t.Fatalf("expected omitted bool config to be inherited for %q, got only_chat=%v compatible_response=%v allow_extra_body=%v", key, channel.OnlyChat, channel.CompatibleResponse, channel.AllowExtraBody)
		}
		if channel.DisabledStream == nil || len(*channel.DisabledStream) != 1 || (*channel.DisabledStream)[0] != "gpt-old" {
			t.Fatalf("expected omitted disabled_stream to be inherited for %q, got %#v", key, channel.DisabledStream)
		}
		if channel.Plugin == nil || channel.Plugin.Data()["claude"]["base_url"] != "https://old-plugin.example.com" {
			t.Fatalf("expected omitted plugin to be inherited for %q, got %#v", key, channel.Plugin)
		}
	}

	added := channelsByKey["sk-two"]
	if added.Balance != 0 || added.UsedQuota != 0 || added.ResponseTime != 0 || added.TestTime != 0 {
		t.Fatalf("expected new member runtime fields to be reset, got %+v", added)
	}
}

func TestUpdateChannelsTagWithSubmittedFieldsWritesNilPointersAndPlugin(t *testing.T) {
	useTestChannelDB(t)

	oldPlugin := datatypes.NewJSONType(PluginType{
		"claude": {
			"enabled":  true,
			"base_url": "https://old-plugin.example.com",
		},
	})
	insertTestChannel(t, &Channel{
		Id:      1,
		Type:    config.ChannelTypeCustom,
		Name:    "tagged-one",
		Key:     "sk-one",
		Group:   "legacy",
		Models:  "gpt-old",
		Tag:     "nil-plugin-team",
		BaseURL: stringPtr("https://old.example"),
		Proxy:   stringPtr("http://old-proxy"),
		Plugin:  &oldPlugin,
	})

	newPlugin := datatypes.NewJSONType(PluginType{
		"claude": {
			"enabled":  true,
			"base_url": "https://new-plugin.example.com",
		},
	})
	if err := UpdateChannelsTagWithSubmittedFields("nil-plugin-team", &Channel{
		Key:     "sk-one",
		BaseURL: nil,
		Proxy:   nil,
		Plugin:  &newPlugin,
	}, ChannelTagSubmittedFields{
		"base_url": struct{}{},
		"proxy":    struct{}{},
		"plugin":   struct{}{},
	}); err != nil {
		t.Fatalf("expected nil pointer and plugin tag update to succeed, got %v", err)
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted tagged channel lookup to succeed, got %v", err)
	}
	if persisted.BaseURL != nil {
		t.Fatalf("expected submitted base_url=null to write NULL, got %#v", persisted.BaseURL)
	}
	if persisted.Proxy != nil {
		t.Fatalf("expected submitted proxy=null to write NULL, got %#v", persisted.Proxy)
	}
	if persisted.Plugin == nil {
		t.Fatal("expected submitted plugin config to sync")
	}
	claudeConfig := persisted.Plugin.Data()["claude"]
	if claudeConfig["base_url"] != "https://new-plugin.example.com" {
		t.Fatalf("expected submitted plugin config to sync, got %#v", persisted.Plugin.Data())
	}
}

func TestUpdateChannelsTagPartialAzureSpeechInheritsRuntimeEndpoint(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:      1,
		Type:    config.ChannelTypeAzureSpeech,
		Name:    "speech-tagged",
		Key:     "speech-key",
		Group:   "legacy",
		Models:  "tts-old",
		Tag:     "speech-team",
		BaseURL: stringPtr("https://speech.example.com"),
	})

	if err := UpdateChannelsTagWithSubmittedFields("speech-team", &Channel{
		Key:    "speech-key",
		Models: "tts-new",
	}, ChannelTagSubmittedFields{
		"key":    struct{}{},
		"models": struct{}{},
	}); err != nil {
		t.Fatalf("expected Azure Speech partial tag update to inherit base_url and pass validation, got %v", err)
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure Speech channel lookup to succeed, got %v", err)
	}
	if persisted.BaseURL == nil || *persisted.BaseURL != "https://speech.example.com" || persisted.Models != "tts-new" {
		t.Fatalf("expected Azure Speech partial update to preserve base_url and update models, got %+v", persisted)
	}
}

func TestUpdateChannelsTagPartialAzureV1InheritsRuntimeEndpoint(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:      1,
		Type:    config.ChannelTypeAzureV1,
		Name:    "azure-v1-tagged",
		Key:     "azure-v1-key",
		Group:   "legacy",
		Models:  "gpt-old",
		Tag:     "azure-v1-team",
		Other:   `{}`,
		BaseURL: stringPtr("https://resource.openai.azure.com"),
	})

	if err := UpdateChannelsTagWithSubmittedFields("azure-v1-team", &Channel{
		Key:    "azure-v1-key\nazure-v1-new-key",
		Models: "gpt-new",
	}, ChannelTagSubmittedFields{
		"key":    struct{}{},
		"models": struct{}{},
	}); err != nil {
		t.Fatalf("expected Azure V1 partial tag update to inherit base_url and pass validation, got %v", err)
	}

	channels, err := GetChannelsByTag("azure-v1-team")
	if err != nil {
		t.Fatalf("expected persisted Azure V1 tag lookup to succeed, got %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("expected two Azure V1 tagged channels after member add, got %d", len(channels))
	}
	for _, persisted := range channels {
		if persisted.BaseURL == nil || *persisted.BaseURL != "https://resource.openai.azure.com" || persisted.Models != "gpt-new" {
			t.Fatalf("expected Azure V1 partial tag update to preserve base_url and update models, got %+v", persisted)
		}
	}
}

func TestUpdateChannelsTagNewMembersInheritSubmittedTagConfig(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeOpenAI,
		Name:   "tagged-one",
		Key:    "sk-one",
		Group:  "legacy",
		Models: "gpt-old",
		Tag:    "member-team",
	})

	if err := UpdateChannelsTag("member-team", &Channel{
		Name:           "member-team",
		Key:            "sk-one\nsk-two",
		Group:          "default",
		Models:         "gpt-new",
		Tag:            "member-team",
		TestModel:      "gpt-new",
		AllowExtraBody: true,
	}); err != nil {
		t.Fatalf("expected tag update with new key to succeed, got %v", err)
	}

	channels, err := GetChannelsByTag("member-team")
	if err != nil {
		t.Fatalf("expected tagged channels lookup to succeed, got %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("expected two tagged channels after member add, got %d", len(channels))
	}
	var added *Channel
	for _, channel := range channels {
		if channel.Key == "sk-two" {
			added = channel
			break
		}
	}
	if added == nil {
		t.Fatal("expected new key to create a tagged channel")
	}
	if added.Models != "gpt-new" || added.Group != "default" || added.TestModel != "gpt-new" || !added.AllowExtraBody {
		t.Fatalf("expected new member to inherit tag config, got %+v", added)
	}
}

func TestUpdateChannelsTagCanonicalizesSubmittedLegacyCodexOther(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-one",
		Key:    `{"access_token":"old-token"}`,
		Group:  "default",
		Models: "gpt-5",
		Tag:    "codex-team",
	})

	err := UpdateChannelsTagWithSubmittedFields("codex-team", &Channel{
		Key:    `{"access_token":"old-token"}` + "\n" + `{"access_token":"new-token"}`,
		Group:  "default",
		Models: "gpt-5",
		Other:  `{"websocket_mode":"required"}`,
	}, ChannelTagSubmittedFields{
		"key":    struct{}{},
		"group":  struct{}{},
		"models": struct{}{},
		"other":  struct{}{},
	})
	if err != nil {
		t.Fatalf("expected tag update to canonicalize submitted Codex legacy other, got %v", err)
	}

	channels, err := GetChannelsByTag("codex-team")
	if err != nil {
		t.Fatalf("expected tagged channel lookup to succeed, got %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("expected tag update to add one member, got %d", len(channels))
	}
	for _, persisted := range channels {
		assertJSONObjectsEqual(t, persisted.Other, `{"websocket_mode":"force"}`)
	}
}

func TestUpdateChannelsTagClearsCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:      1,
		Type:    config.ChannelTypeCodex,
		Name:    "codex-tag",
		Key:     "sk-tagged",
		Group:   "default",
		Models:  "gpt-5",
		Tag:     "codex-team",
		BaseURL: stringPtr("https://old.example"),
	})
	insertTestChannel(t, &Channel{
		Id:      2,
		Type:    config.ChannelTypeCodex,
		Name:    "codex-tag-2",
		Key:     "sk-tagged-2",
		Group:   "default",
		Models:  "gpt-5",
		Tag:     "codex-team",
		BaseURL: stringPtr("https://old.example"),
	})
	primeChannelDerivedCaches(t, 1)
	primeChannelDerivedCaches(t, 2)

	if err := UpdateChannelsTag("codex-team", &Channel{
		Name:    "codex-tag",
		Key:     "sk-tagged\nsk-tagged-2",
		Group:   "default",
		Models:  "gpt-5",
		Tag:     "codex-team",
		BaseURL: stringPtr("https://new.example"),
	}); err != nil {
		t.Fatalf("expected tag update to succeed, got %v", err)
	}

	assertChannelDerivedCachesCleared(t, 1)
	assertChannelDerivedCachesCleared(t, 2)
}

func TestUpdateChannelsTagRenameRefreshesRoutesAndCaches(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() { restoreTestChannelGroup(t, snapshot) })

	const (
		firstID  = 23501
		secondID = 23502
	)
	for _, channel := range []*Channel{
		{
			Id: firstID, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled,
			Name: "rename-a", Key: `{"access_token":"token-a"}`, Tag: "old-tag",
			Group: "old-group", Models: "old-model", Other: `{"websocket_mode":"force"}`,
		},
		{
			Id: secondID, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled,
			Name: "rename-b", Key: `{"access_token":"token-b"}`, Tag: "old-tag",
			Group: "old-group", Models: "old-model", Other: `{"websocket_mode":"force"}`,
		},
	} {
		insertTestChannel(t, channel)
	}
	requireChannelGroupLoad(t)

	invalidated := make(map[int]bool)
	originalInvalidate := invalidateChannelCodexDerivedCaches
	invalidateChannelCodexDerivedCaches = func(ids []int) {
		for _, id := range ids {
			invalidated[id] = true
		}
	}
	t.Cleanup(func() { invalidateChannelCodexDerivedCaches = originalInvalidate })

	err := UpdateChannelsTagWithSubmittedFields("old-tag", &Channel{
		Key:    `{"access_token":"token-a"}` + "\n" + `{"access_token":"token-b"}`,
		Tag:    "new-tag",
		Group:  "new-group",
		Models: "new-model",
	}, ChannelTagSubmittedFields{
		"tag": {}, "group": {}, "models": {},
	})
	if err != nil {
		t.Fatalf("rename tag config: %v", err)
	}

	var durable []Channel
	if err := DB.Where("id IN ?", []int{firstID, secondID}).Order("id").Find(&durable).Error; err != nil {
		t.Fatalf("load renamed channels: %v", err)
	}
	if len(durable) != 2 {
		t.Fatalf("expected two renamed channels, got %d", len(durable))
	}
	for _, channel := range durable {
		if channel.Tag != "new-tag" || channel.Group != "new-group" || channel.Models != "new-model" {
			t.Fatalf("tag update was not durable: %+v", channel)
		}
		if !invalidated[channel.Id] {
			t.Errorf("Codex caches were not invalidated for channel %d", channel.Id)
		}
	}

	if _, err := ChannelGroup.Next("old-group", "old-model"); err == nil {
		t.Fatal("old chooser route survived the tag rename")
	}
	routed, err := ChannelGroup.Next("new-group", "new-model")
	if err != nil {
		t.Fatalf("new chooser route was not published: %v", err)
	}
	if routed.Id != firstID && routed.Id != secondID {
		t.Fatalf("unexpected channel on new chooser route: %+v", routed)
	}
}

func TestBatchUpdateChannelsAzureApiRejectsNonAzureChannels(t *testing.T) {
	useTestChannelDB(t)
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-batch",
		Key:    "sk-batch",
		Group:  "default",
		Models: "gpt-5",
	})

	if _, err := BatchUpdateChannelsAzureApi(&BatchChannelsParams{
		Ids:        []int{1},
		APIVersion: "2024-06-01",
	}); err == nil {
		t.Fatal("expected batch Azure api_version update to reject non-Azure channels")
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Codex channel lookup to succeed, got %v", err)
	}
	if persisted.Other != "" {
		t.Fatalf("expected rejected batch update not to mutate other, got %q", persisted.Other)
	}
}

func TestGetChannelsListFiltersAzureAPIVersionAcrossJSONFormatting(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure-compact",
		Key:    "sk-compact",
		Group:  "default",
		Models: "gpt-5",
		Other:  `{"api_version":"2024-06-01","responses_ws_transport":"native"}`,
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Type:   config.ChannelTypeAzure,
		Name:   "azure-pretty",
		Key:    "sk-pretty",
		Group:  "default",
		Models: "gpt-5",
		Other:  "{\n  \"api_version\": \"2024-06-15\",\n  \"responses_ws_transport\": \"http_bridge\"\n}",
	})
	insertTestChannel(t, &Channel{
		Id:     3,
		Type:   config.ChannelTypeAzure,
		Name:   "azure-legacy",
		Key:    "sk-legacy",
		Group:  "default",
		Models: "gpt-5",
		Other:  "2024-06-01",
	})
	insertTestChannel(t, &Channel{
		Id:     4,
		Type:   config.ChannelTypeOpenAI,
		Name:   "openai-other",
		Key:    "sk-openai",
		Group:  "default",
		Models: "gpt-5",
		Other:  `{"api_version":"2024-06-01"}`,
	})

	result, err := GetChannelsList(&SearchChannelsParams{
		AzureAPIVersion: "2024-06",
		PaginationParams: PaginationParams{
			Page: 1,
			Size: 10,
		},
	})
	if err != nil {
		t.Fatalf("expected Azure api_version search to succeed, got %v", err)
	}
	if result == nil || result.Data == nil || len(*result.Data) != 2 {
		t.Fatalf("expected compact and pretty Azure JSON channels only, got %+v", result)
	}
	names := map[string]bool{}
	for _, channel := range *result.Data {
		names[channel.Name] = true
	}
	if !names["azure-compact"] || !names["azure-pretty"] || names["azure-legacy"] || names["openai-other"] {
		t.Fatalf("unexpected Azure api_version search result names: %#v", names)
	}
}

func TestGetChannelsListAzureAPIVersionPreservesOrderPagingAndWarnsLargeCandidateScan(t *testing.T) {
	useTestChannelDB(t)
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	originalThreshold := azureAPIVersionSearchCandidateWarnThreshold
	azureAPIVersionSearchCandidateWarnThreshold = 2
	t.Cleanup(func() {
		logger.Logger = originalLogger
		azureAPIVersionSearchCandidateWarnThreshold = originalThreshold
	})

	version := "2026-unique-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	for _, channel := range []*Channel{
		{Id: 1, Type: config.ChannelTypeAzure, Name: "azure-one", Key: "sk-one", Group: "default", Models: "gpt-5", Other: `{"api_version":"` + version + `-a"}`},
		{Id: 2, Type: config.ChannelTypeAzure, Name: "azure-two", Key: "sk-two", Group: "default", Models: "gpt-5", Other: `{"api_version":"` + version + `-b"}`},
		{Id: 3, Type: config.ChannelTypeAzure, Name: "azure-three", Key: "sk-three", Group: "default", Models: "gpt-5", Other: `{"api_version":"` + version + `-c"}`},
		{Id: 4, Type: config.ChannelTypeAzure, Name: "azure-other", Key: "sk-four", Group: "default", Models: "gpt-5", Other: `{"api_version":"2024-01-01"}`},
	} {
		insertTestChannel(t, channel)
	}

	result, err := GetChannelsList(&SearchChannelsParams{
		AzureAPIVersion: version,
		PaginationParams: PaginationParams{
			Page: 2,
			Size: 1,
		},
	})
	if err != nil {
		t.Fatalf("expected Azure api_version search to succeed, got %v", err)
	}
	if result.TotalCount != 3 || result.Data == nil || len(*result.Data) != 1 {
		t.Fatalf("expected three semantic matches with one row on page, got %+v", result)
	}
	if got := (*result.Data)[0].Name; got != "azure-two" {
		t.Fatalf("expected default id desc order to place azure-two on page 2, got %q", got)
	}

	entries, _ := logger.GetLatestLogs(500)
	found := false
	for _, entry := range entries {
		if strings.Contains(entry.Message, "Azure api_version search scanning") && strings.Contains(entry.Message, version) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected large Azure api_version candidate scan warning")
	}
}

func TestBatchDelModelChannelsRequiresValueAndCountsActualRemovals(t *testing.T) {
	useTestChannelDB(t)
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeOpenAI,
		Name:   "openai-one",
		Key:    "sk-one",
		Group:  "default",
		Models: "gpt-4o,gpt-5",
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Type:   config.ChannelTypeOpenAI,
		Name:   "openai-two",
		Key:    "sk-two",
		Group:  "default",
		Models: "gpt-4o",
	})
	insertTestChannel(t, &Channel{
		Id:     3,
		Type:   config.ChannelTypeOpenAI,
		Name:   "openai-three",
		Key:    "sk-three",
		Group:  "default",
		Models: "gpt-5",
	})

	if _, err := BatchDelModelChannels(&BatchDelModelChannelsParams{Ids: []int{1, 2, 3}}); err == nil {
		t.Fatal("expected empty model value to be rejected")
	}

	count, err := BatchDelModelChannels(&BatchDelModelChannelsParams{
		Ids:   []int{1, 2, 3},
		Value: "gpt-5",
	})
	if err != nil {
		t.Fatalf("expected batch delete model to succeed, got %v", err)
	}
	if count != 2 {
		t.Fatalf("expected two channels to be mutated, got %d", count)
	}

	channelOne, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected channel one lookup to succeed, got %v", err)
	}
	if channelOne.Models != "gpt-4o" {
		t.Fatalf("expected gpt-5 to be removed from channel one, got %q", channelOne.Models)
	}
	channelTwo, err := GetChannelById(2)
	if err != nil {
		t.Fatalf("expected channel two lookup to succeed, got %v", err)
	}
	if channelTwo.Models != "gpt-4o" {
		t.Fatalf("expected channel without target model to remain unchanged, got %q", channelTwo.Models)
	}
	channelThree, err := GetChannelById(3)
	if err != nil {
		t.Fatalf("expected channel three lookup to succeed, got %v", err)
	}
	if channelThree.Models != "" {
		t.Fatalf("expected sole model to be removed from channel three, got %q", channelThree.Models)
	}
}

func TestBatchDelModelChannelsRemovesAllTrimmedDuplicatesOncePerChannel(t *testing.T) {
	useTestChannelDB(t)
	logger.SetupLogger()

	testCases := []struct {
		id     int
		models string
		want   string
	}{
		{id: 1, models: "gpt-5,gpt-5,other", want: "other"},
		{id: 2, models: " first , gpt-5 ,second,  gpt-5, third ", want: " first ,second, third "},
	}
	for _, testCase := range testCases {
		insertTestChannel(t, &Channel{
			Id:     testCase.id,
			Type:   config.ChannelTypeOpenAI,
			Name:   fmt.Sprintf("duplicate-models-%d", testCase.id),
			Key:    fmt.Sprintf("sk-%d", testCase.id),
			Group:  "default",
			Models: testCase.models,
		})
	}

	count, err := BatchDelModelChannels(&BatchDelModelChannelsParams{
		Ids:   []int{1, 2},
		Value: " gpt-5 ",
	})
	if err != nil {
		t.Fatalf("delete duplicate models: %v", err)
	}
	if count != 2 {
		t.Fatalf("affected count = %d, want one per mutated channel (2)", count)
	}

	for _, testCase := range testCases {
		var channel Channel
		if err := DB.First(&channel, testCase.id).Error; err != nil {
			t.Fatalf("load channel %d: %v", testCase.id, err)
		}
		if channel.Models != testCase.want {
			t.Errorf("channel %d models = %q, want %q", testCase.id, channel.Models, testCase.want)
		}
		for _, model := range strings.Split(channel.Models, ",") {
			if strings.TrimSpace(model) == "gpt-5" {
				t.Errorf("channel %d still contains target model in %q", testCase.id, channel.Models)
			}
		}
	}
}

func TestBatchDelModelChannelsRollsBackAndSkipsPostCommitEffects(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	for id := 1; id <= 2; id++ {
		insertTestChannel(t, &Channel{
			Id: id, Type: config.ChannelTypeCodex, Name: fmt.Sprintf("codex-%d", id),
			Key: fmt.Sprintf("sk-%d", id), Group: "default", Models: "gpt-4o,gpt-5",
		})
		primeChannelDerivedCaches(t, id)
	}

	queries := 0
	queryCallback := "test:count_batch_model_queries:" + t.Name()
	if err := DB.Callback().Query().Before("gorm:query").Register(queryCallback, func(*gorm.DB) {
		queries++
	}); err != nil {
		t.Fatalf("register query callback: %v", err)
	}
	updates := 0
	updateErr := errors.New("forced second update failure")
	updateCallback := "test:fail_second_batch_model_update:" + t.Name()
	if err := DB.Callback().Update().Before("gorm:update").Register(updateCallback, func(tx *gorm.DB) {
		updates++
		if updates == 2 {
			tx.AddError(updateErr)
		}
	}); err != nil {
		t.Fatalf("register update callback: %v", err)
	}
	t.Cleanup(func() {
		_ = DB.Callback().Query().Remove(queryCallback)
		_ = DB.Callback().Update().Remove(updateCallback)
	})

	count, err := BatchDelModelChannels(&BatchDelModelChannelsParams{Ids: []int{1, 2}, Value: "gpt-5"})
	if !errors.Is(err, updateErr) {
		t.Fatalf("expected second update failure, got count=%d err=%v", count, err)
	}
	if count != 0 {
		t.Fatalf("rolled-back batch reported count %d, want 0", count)
	}
	if queries != 1 {
		t.Fatalf("failed transaction triggered post-commit refresh: query count=%d, want 1", queries)
	}

	for id := 1; id <= 2; id++ {
		var channel Channel
		if err := DB.First(&channel, id).Error; err != nil {
			t.Fatalf("load channel %d: %v", id, err)
		}
		if channel.Models != "gpt-4o,gpt-5" {
			t.Fatalf("channel %d was not rolled back: models=%q", id, channel.Models)
		}
		assertChannelDerivedCachesPresent(t, id)
		generationKey := fmt.Sprintf("%s:%d", codexUsageGenerationCacheKeyPrefix, id)
		generation, err := cache.GetCache[string](generationKey)
		if err != nil || generation != "generation-before-mutation" {
			t.Fatalf("channel %d generation was invalidated: value=%q err=%v", id, generation, err)
		}
	}
}

func TestBatchDelModelChannelsCASConflictRollsBackWholeBatch(t *testing.T) {
	useTestChannelDB(t)
	if err := DB.Exec("DELETE FROM channels").Error; err != nil {
		t.Fatalf("clear repeated-test fixtures: %v", err)
	}
	for id := 1; id <= 2; id++ {
		insertTestChannel(t, &Channel{
			Id: id, Type: config.ChannelTypeOpenAI, Name: fmt.Sprintf("channel-%d", id),
			Key: fmt.Sprintf("sk-%d", id), Group: "default", Models: "gpt-4o,gpt-5",
		})
	}

	updates := 0
	callback := "test:simulate_batch_model_cas_conflict:" + t.Name()
	if err := DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		updates++
		if updates == 2 {
			// Deterministically model another writer changing models after the batch
			// SELECT but before this row's optimistic update.
			if err := tx.Exec("UPDATE channels SET models = ? WHERE id = ?", "gpt-4o,gpt-5,gpt-x", 2).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatalf("register conflict callback: %v", err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(callback) })

	count, err := BatchDelModelChannels(&BatchDelModelChannelsParams{Ids: []int{1, 2}, Value: "gpt-5"})
	if !errors.Is(err, errBatchDelModelChannelsConflict) {
		t.Fatalf("expected explicit CAS conflict, got count=%d err=%v", count, err)
	}
	if count != 0 {
		t.Fatalf("conflicted batch reported count %d, want 0", count)
	}
	for id := 1; id <= 2; id++ {
		var channel Channel
		if err := DB.First(&channel, id).Error; err != nil {
			t.Fatalf("load channel %d: %v", id, err)
		}
		if channel.Models != "gpt-4o,gpt-5" {
			t.Fatalf("channel %d escaped whole-batch rollback: models=%q", id, channel.Models)
		}
	}
}

func TestBatchUpdateChannelsAzureApiRejectsMixedChannelTypes(t *testing.T) {
	useTestChannelDB(t)
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure-batch",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-4o",
		Other:  `{"api_version":"2024-05-01-preview","responses_ws_transport":"http_bridge"}`,
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Type:   config.ChannelTypeOpenAI,
		Name:   "openai-batch",
		Key:    "sk-openai",
		Group:  "default",
		Models: "gpt-4o",
		Other:  `{"responses_ws_transport":"native"}`,
	})

	if _, err := BatchUpdateChannelsAzureApi(&BatchChannelsParams{
		Ids:        []int{1, 2},
		APIVersion: "2024-06-01",
	}); err == nil {
		t.Fatal("expected mixed Azure batch update to fail")
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure channel lookup to succeed, got %v", err)
	}
	if persisted.Other != `{"api_version":"2024-05-01-preview","responses_ws_transport":"http_bridge"}` {
		t.Fatalf("expected rejected mixed update not to mutate Azure other, got %q", persisted.Other)
	}
}

func TestBatchUpdateChannelsAzureApiRequiresAPIVersion(t *testing.T) {
	useTestChannelDB(t)
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure-batch",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-4o",
		Other:  `{"api_version":"2024-05-01-preview"}`,
	})

	if _, err := BatchUpdateChannelsAzureApi(&BatchChannelsParams{
		Ids: []int{1},
	}); err == nil {
		t.Fatal("expected Azure batch update without api_version to fail")
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure channel lookup to succeed, got %v", err)
	}
	if persisted.Other != `{"api_version":"2024-05-01-preview"}` {
		t.Fatalf("expected rejected Azure batch update not to mutate other, got %q", persisted.Other)
	}
}

func TestBatchUpdateChannelsAzureApiAcceptsJSONOtherForAzureChannels(t *testing.T) {
	useTestChannelDB(t)
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure-batch",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-4o",
		Other:  `{"api_version":"2024-05-01-preview","responses_ws_transport":"http_bridge","responses_ws_self_hosted":false}`,
	})

	count, err := BatchUpdateChannelsAzureApi(&BatchChannelsParams{
		Ids:        []int{1},
		APIVersion: "2024-06-01",
	})
	if err != nil {
		t.Fatalf("expected Azure batch api_version update to stay valid, got %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one Azure channel update, got %d", count)
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure channel lookup to succeed, got %v", err)
	}
	assertJSONObjectsEqual(t, persisted.Other, `{"api_version":"2024-06-01","responses_ws_transport":"http_bridge","responses_ws_self_hosted":false}`)
}

func TestBatchUpdateChannelsAzureApiPreservesOpaqueOtherNamespaces(t *testing.T) {
	useTestChannelDB(t)
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure-batch",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-4o",
		Other:  `{"api_version":"2024-05-01-preview","responses_ws_transport":"native","extra":{"deployment":"x","enabled":true},"vendor_extra":{"owner":"ops"}}`,
	})

	count, err := BatchUpdateChannelsAzureApi(&BatchChannelsParams{
		Ids:        []int{1},
		APIVersion: "2024-06-01",
	})
	if err != nil {
		t.Fatalf("expected Azure batch api_version update to preserve opaque namespaces, got %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one Azure channel update, got %d", count)
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure channel lookup to succeed, got %v", err)
	}
	assertJSONObjectsEqual(t, persisted.Other, `{"api_version":"2024-06-01","responses_ws_transport":"native","extra":{"deployment":"x","enabled":true},"vendor_extra":{"owner":"ops"}}`)
}

func TestBatchUpdateChannelsAzureApiPreservesRealtimeSelfHosted(t *testing.T) {
	useTestChannelDB(t)
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure-batch",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-4o",
		Other:  `{"api_version":"2024-05-01-preview","self_hosted":true,"responses_ws_self_hosted":false}`,
	})

	count, err := BatchUpdateChannelsAzureApi(&BatchChannelsParams{
		Ids:        []int{1},
		APIVersion: "2024-06-01",
	})
	if err != nil {
		t.Fatalf("expected Azure batch api_version update to preserve self_hosted, got %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one Azure channel update, got %d", count)
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure channel lookup to succeed, got %v", err)
	}
	assertJSONObjectsEqual(t, persisted.Other, `{"api_version":"2024-06-01","self_hosted":true,"responses_ws_self_hosted":false}`)
}
