package model

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"

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
		fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelID):        "cached-token",
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelID): "cached-preview",
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelID):  "cached-detail",
	}

	for key, value := range cacheEntries {
		if err := cache.SetCache(key, value, time.Minute); err != nil {
			t.Fatalf("expected cache priming to succeed for %s, got %v", key, err)
		}
	}
}

func assertChannelDerivedCachesCleared(t *testing.T, channelID int) {
	t.Helper()

	cacheKeys := []string{
		fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelID),
	}

	for _, key := range cacheKeys {
		if _, err := cache.GetCache[string](key); !errors.Is(err, cache.CacheNotFound) {
			t.Fatalf("expected cache key %s to be cleared, got err=%v", key, err)
		}
	}
}

func assertChannelDerivedCachesPresent(t *testing.T, channelID int) {
	t.Helper()

	cacheKeys := []string{
		fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelID),
	}

	for _, key := range cacheKeys {
		if _, err := cache.GetCache[string](key); err != nil {
			t.Fatalf("expected cache key %s to still exist, got err=%v", key, err)
		}
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
		BaseURL: stringPtr("https://new.example"),
	}); err != nil {
		t.Fatalf("expected tag update to succeed, got %v", err)
	}

	assertChannelDerivedCachesCleared(t, 1)
	assertChannelDerivedCachesCleared(t, 2)
}

func TestBatchUpdateChannelsAzureApiRejectsUnsupportedCodexOtherWhenTypeOmitted(t *testing.T) {
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
		Ids:   []int{1},
		Value: `{"user_agent_regex":"^Codex/"}`,
	}); err == nil {
		t.Fatal("expected batch other update to reject unsupported Codex fields when payload omits type")
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Codex channel lookup to succeed, got %v", err)
	}
	if persisted.Other != "" {
		t.Fatalf("expected rejected batch update not to mutate other, got %q", persisted.Other)
	}
}

func TestBatchUpdateChannelsAzureApiClearsCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-batch",
		Key:    "sk-batch",
		Group:  "default",
		Models: "gpt-5",
		Other:  `{"user_agent":"old-agent"}`,
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Type:   3,
		Name:   "azure-batch",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-4o",
		Other:  "2024-05-01-preview",
	})
	primeChannelDerivedCaches(t, 1)
	primeChannelDerivedCaches(t, 2)

	count, err := BatchUpdateChannelsAzureApi(&BatchChannelsParams{
		Ids:   []int{1, 2},
		Value: `{"user_agent":"new-agent"}`,
	})
	if err != nil {
		t.Fatalf("expected batch update to succeed, got %v", err)
	}
	if count != 2 {
		t.Fatalf("expected both rows to update, got %d", count)
	}

	assertChannelDerivedCachesCleared(t, 1)

	for _, key := range []string{
		fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, 2),
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, 2),
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, 2),
	} {
		if _, err := cache.GetCache[string](key); err != nil {
			t.Fatalf("expected non-Codex batch update not to clear cache key %s, got %v", key, err)
		}
	}
}

func TestBatchUpdateChannelsAzureApiAllowsPlainOtherForAzureChannels(t *testing.T) {
	useTestChannelDB(t)
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   3,
		Name:   "azure-batch",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-4o",
		Other:  "2024-05-01-preview",
	})

	count, err := BatchUpdateChannelsAzureApi(&BatchChannelsParams{
		Ids:   []int{1},
		Value: "2024-06-01",
	})
	if err != nil {
		t.Fatalf("expected Azure batch other update to stay valid, got %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one Azure channel update, got %d", count)
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure channel lookup to succeed, got %v", err)
	}
	if persisted.Other != "2024-06-01" {
		t.Fatalf("expected Azure batch update to persist plain other value, got %q", persisted.Other)
	}
}
