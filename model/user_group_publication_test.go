package model

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/limit"
	"one-api/internal/testutil/sqlitetest"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type publicationTestLimiter struct {
	limit.RateLimiter
	stops int
}

func (limiter *publicationTestLimiter) Stop() {
	limiter.stops++
	if stopper, ok := limiter.RateLimiter.(interface{ Stop() }); ok {
		stopper.Stop()
	}
}

func setupUserGroupPublicationTest(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&UserGroup{}, &PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	savedDB, savedGroups, savedRedis := DB, GlobalUserGroupRatio, config.RedisEnabled
	DB, GlobalUserGroupRatio, config.RedisEnabled = db, &UserGroupRatio{}, false
	t.Cleanup(func() {
		stopUserGroupAPILimiters(GlobalUserGroupRatio.APILimiter)
		DB, GlobalUserGroupRatio, config.RedisEnabled = savedDB, savedGroups, savedRedis
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	})
	return db
}

func createPublicationTestGroup(t *testing.T, symbol string, rpm int) *UserGroup {
	t.Helper()
	group := &UserGroup{Symbol: symbol, Name: symbol, Ratio: 1, APIRate: rpm, Public: true}
	if err := group.Create(); err != nil {
		t.Fatal(err)
	}
	return group
}

func TestUserGroupPublicationPropagatesWithoutResettingUnchangedLimiters(t *testing.T) {
	db := setupUserGroupPublicationTest(t)
	first := createPublicationTestGroup(t, "first", 1)
	second := createPublicationTestGroup(t, "second", 1)
	other := &UserGroupRatio{}
	t.Cleanup(func() { stopUserGroupAPILimiters(other.APILimiter) })
	ctx := context.Background()
	if err := other.SyncPublication(ctx); err != nil {
		t.Fatal(err)
	}
	firstLimiter := &publicationTestLimiter{RateLimiter: other.GetAPILimiter(first.Symbol)}
	other.APILimiter[first.Symbol] = firstLimiter
	secondLimiter := other.GetAPILimiter(second.Symbol)
	if !firstLimiter.Allow("user-first") || !secondLimiter.Allow("user-second") {
		t.Fatal("first requests were rejected")
	}
	var fullLoads atomic.Int64
	if err := db.Callback().Query().Before("gorm:query").Register("publication_count_group_load", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_groups" {
			fullLoads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := other.SyncPublication(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if fullLoads.Load() != 0 || firstLimiter != other.GetAPILimiter(first.Symbol) || firstLimiter.Allow("user-first") {
		t.Fatal("unchanged publication reloaded data or reset request count")
	}

	first.Ratio = 2
	if err := first.Update(); err != nil {
		t.Fatal(err)
	}
	if other.GetBySymbol(first.Symbol).Ratio != 1 {
		t.Fatal("independent cache changed before synchronization")
	}
	if err := other.SyncPublication(ctx); err != nil {
		t.Fatal(err)
	}
	if other.GetBySymbol(first.Symbol).Ratio != 2 || other.GetAPILimiter(first.Symbol) != firstLimiter || firstLimiter.Allow("user-first") || firstLimiter.stops != 0 {
		t.Fatal("ratio publication lost limiter identity or consumed count")
	}
	first.APIRate = 2
	if err := first.Update(); err != nil {
		t.Fatal(err)
	}
	if err := other.SyncPublication(ctx); err != nil {
		t.Fatal(err)
	}
	if other.GetAPILimiter(first.Symbol) == firstLimiter || firstLimiter.stops != 1 || other.GetAPILimiter(second.Symbol) != secondLimiter || secondLimiter.Allow("user-second") {
		t.Fatal("API rate publication reset an unaffected group")
	}
	changedLimiter := other.GetAPILimiter(first.Symbol)
	if !changedLimiter.Allow("rate-test") || !changedLimiter.Allow("rate-test") || changedLimiter.Allow("rate-test") {
		t.Fatal("updated API rate was not enforced")
	}
	if err := ChangeUserGroupEnable(first.Id, false); err != nil {
		t.Fatal(err)
	}
	if err := other.SyncPublication(ctx); err != nil {
		t.Fatal(err)
	}
	if other.GetBySymbol(first.Symbol) != nil || other.GetAPILimiter(first.Symbol) != nil || len(other.GetPublicGroupList()) != 1 {
		t.Fatal("disabled group remained available")
	}
	if err := second.Delete(); err != nil {
		t.Fatal(err)
	}
	if err := other.SyncPublication(ctx); err != nil || len(other.GetAll()) != 0 {
		t.Fatalf("deleted group remained available: %v", err)
	}
}

func TestUserGroupPublicationWatcherRunsOnMasterWithoutMemoryCache(t *testing.T) {
	setupUserGroupPublicationTest(t)
	group := createPublicationTestGroup(t, "watched", 1)
	other := &UserGroupRatio{}
	if err := other.Load(); err != nil {
		t.Fatal(err)
	}
	savedMaster, savedMemory := config.IsMasterNode, config.MemoryCacheEnabled
	config.IsMasterNode, config.MemoryCacheEnabled = true, false
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		other.watchPublication(ctx, userGroupPublicationWatchInterval)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		stopUserGroupAPILimiters(other.APILimiter)
		config.IsMasterNode, config.MemoryCacheEnabled = savedMaster, savedMemory
	})
	group.Ratio = 3
	if err := group.Update(); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(2 * userGroupPublicationWatchInterval)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("user group policy did not propagate within two watcher intervals")
		case <-poll.C:
			if other.GetBySymbol(group.Symbol).Ratio == 3 {
				return
			}
		}
	}
}

func TestUserGroupMutationAndPublicationVersionRollbackTogether(t *testing.T) {
	db := setupUserGroupPublicationTest(t)
	group := createPublicationTestGroup(t, "atomic", 1)
	before := GlobalUserGroupRatio.PublicationStatus()
	fail := errors.New("group mutation failed")
	if err := db.Callback().Update().Before("gorm:update").Register("publication_fail_group_update", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_groups" {
			tx.AddError(fail)
		}
	}); err != nil {
		t.Fatal(err)
	}
	group.Ratio = 9
	if err := group.Update(); !errors.Is(err, fail) {
		t.Fatalf("expected group write failure, got %v", err)
	}
	if err := db.Callback().Update().Remove("publication_fail_group_update"); err != nil {
		t.Fatal(err)
	}
	stored, err := GetUserGroupsById(group.Id)
	if err != nil || stored.Ratio != 1 {
		t.Fatalf("failed write persisted: %+v %v", stored, err)
	}
	head, err := ReadPublicationVersion(context.Background(), db, PublicationOwnerUserGroup)
	if err != nil || head != before.DatabaseHead || GlobalUserGroupRatio.PublicationStatus() != before {
		t.Fatalf("failed group write advanced publication: head=%d err=%v", head, err)
	}
	if err := db.Callback().Update().Before("gorm:update").Register("publication_fail_version_update", func(tx *gorm.DB) {
		if tx.Statement.Table == "publication_versions" {
			tx.AddError(fail)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Update(); !errors.Is(err, fail) {
		t.Fatalf("expected version write failure, got %v", err)
	}
	stored, err = GetUserGroupsById(group.Id)
	if err != nil || stored.Ratio != 1 {
		t.Fatalf("version failure did not prevent group write: %+v %v", stored, err)
	}
}

func TestUserGroupPublicationDoesNotInstallMixedVersions(t *testing.T) {
	db := setupUserGroupPublicationTest(t)
	group := createPublicationTestGroup(t, "consistent", 1)
	other := &UserGroupRatio{}
	if err := other.Load(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopUserGroupAPILimiters(other.APILimiter) })
	group.Ratio = 2
	if err := group.Update(); err != nil {
		t.Fatal(err)
	}
	var loads int
	if err := db.Callback().Query().After("gorm:query").Register("publication_write_during_load", func(tx *gorm.DB) {
		if tx.Statement.Table != "user_groups" {
			return
		}
		loads++
		if loads != 1 {
			return
		}
		err := db.Transaction(func(writeTx *gorm.DB) error {
			head, err := ReadPublicationVersion(context.Background(), writeTx, PublicationOwnerUserGroup)
			if err != nil {
				return err
			}
			if err := writeTx.Model(&UserGroup{}).Where("id = ?", group.Id).Update("api_rate", 7).Error; err != nil {
				return err
			}
			_, err = BumpPublicationVersionCAS(context.Background(), writeTx, PublicationOwnerUserGroup, head)
			return err
		})
		if err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := other.SyncPublication(context.Background()); err != nil {
		t.Fatal(err)
	}
	if row := other.GetBySymbol(group.Symbol); loads != 2 || row.Ratio != 2 || row.APIRate != 7 {
		t.Fatalf("mixed policy snapshot installed: loads=%d row=%+v", loads, row)
	}
}

func TestUserGroupPublicationLoadFailurePreservesSnapshotAndBlocksNewWork(t *testing.T) {
	db := setupUserGroupPublicationTest(t)
	group := createPublicationTestGroup(t, "stable", 1)
	before := GlobalUserGroupRatio.PublicationStatus()
	limiter := GlobalUserGroupRatio.GetAPILimiter(group.Symbol)
	fail := errors.New("policy query unavailable")
	if err := db.Callback().Query().After("gorm:query").Register("publication_fail_group_load", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_groups" {
			tx.AddError(fail)
		}
	}); err != nil {
		t.Fatal(err)
	}
	group.Ratio = 4
	// SQL 成功不伪装成写入失败；新工作必须通过发布可用性门禁。
	if err := group.Update(); err != nil {
		t.Fatal(err)
	}
	if err := EnsureUserGroupPolicyAvailable(context.Background()); !errors.Is(err, fail) {
		t.Fatalf("new work accepted after policy load failure: %v", err)
	}
	status := GlobalUserGroupRatio.PublicationStatus()
	if status.PublishedVersion != before.PublishedVersion || status.DatabaseHead != before.DatabaseHead+1 || status.LastSyncError == "" {
		t.Fatalf("failed publication status is not visible: %+v", status)
	}
	if GlobalUserGroupRatio.GetBySymbol(group.Symbol).Ratio != 1 || GlobalUserGroupRatio.GetAPILimiter(group.Symbol) != limiter {
		t.Fatal("failed full load installed a partial snapshot")
	}
	if err := db.Callback().Query().Remove("publication_fail_group_load"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureUserGroupPolicyAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	status = GlobalUserGroupRatio.PublicationStatus()
	if status.PublishedVersion != before.PublishedVersion+1 || status.LastSyncError != "" || GlobalUserGroupRatio.GetBySymbol(group.Symbol).Ratio != 4 {
		t.Fatalf("new work failed to refresh recovered policy: %+v", status)
	}
	stopUserGroupAPILimiters(GlobalUserGroupRatio.APILimiter)
	GlobalUserGroupRatio = &UserGroupRatio{}
	DB = nil
	if err := EnsureUserGroupPolicyAvailable(context.Background()); err == nil {
		t.Fatal("uninitialized snapshot accepted new work")
	}
	if status := GlobalUserGroupRatio.PublicationStatus(); status.PublishedVersion != 0 || status.LastSyncError == "" {
		t.Fatalf("initial load failure was hidden: %+v", status)
	}
}
