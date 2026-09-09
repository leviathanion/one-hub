package model

import (
	"errors"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/internal/testutil/sqlitetest"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useTokenSettlementTestDB(t *testing.T) {
	t.Helper()

	logger.Logger = zap.NewNop()

	originalDB := DB
	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&User{}, &Token{}, &UserGroup{}); err != nil {
		t.Fatalf("expected token settlement schema migration to succeed, got %v", err)
	}

	DB = testDB
	t.Cleanup(func() {
		DB = originalDB
	})
}

func insertTokenSettlementFixtures(t *testing.T) {
	t.Helper()

	if err := DB.Create(&User{
		Id:          1,
		Username:    "alice",
		Password:    "password123",
		AccessToken: "access-token-1",
		Quota:       1000,
		Group:       "default",
		Status:      config.UserStatusEnabled,
		Role:        config.RoleCommonUser,
		DisplayName: "Alice",
		CreatedTime: 1,
	}).Error; err != nil {
		t.Fatalf("expected user fixture to persist, got %v", err)
	}
	if err := DB.Session(&gorm.Session{SkipHooks: true}).Create(&Token{
		Id:          1,
		UserId:      1,
		Key:         "token-key-1",
		Name:        "token-alpha",
		RemainQuota: 1000,
		Group:       "default",
	}).Error; err != nil {
		t.Fatalf("expected token fixture to persist, got %v", err)
	}
}

func TestTokenMutableUpdateRejectsConcurrentQuotaDeltaAndPreservesPrincipal(t *testing.T) {
	useTokenSettlementTestDB(t)
	insertTokenSettlementFixtures(t)

	snapshot, err := GetTokenById(1)
	if err != nil {
		t.Fatalf("expected token snapshot, got %v", err)
	}
	if err := DB.Model(&Token{}).Where("id = ?", 1).Update("remain_quota", gorm.Expr("remain_quota - ?", 125)).Error; err != nil {
		t.Fatalf("expected concurrent billing delta, got %v", err)
	}

	expectedQuota := snapshot.RemainQuota
	snapshot.UserId = 999
	snapshot.Name = "renamed"
	snapshot.RemainQuota = 1200
	if err := snapshot.UpdateMutableFields(&expectedQuota); !errors.Is(err, ErrTokenQuotaConflict) {
		t.Fatalf("expected quota CAS conflict, got %v", err)
	}

	var persisted Token
	if err := DB.First(&persisted, 1).Error; err != nil {
		t.Fatalf("expected persisted token, got %v", err)
	}
	if persisted.UserId != 1 {
		t.Fatalf("token principal changed in place: got user %d", persisted.UserId)
	}
	if persisted.RemainQuota != 875 {
		t.Fatalf("concurrent billing delta was overwritten: got %d want 875", persisted.RemainQuota)
	}
	if persisted.Name != "token-alpha" {
		t.Fatalf("metadata changed despite quota conflict: got %q", persisted.Name)
	}

	snapshot.UserId = 999
	snapshot.RemainQuota = 1075
	expectedQuota = persisted.RemainQuota
	if err := snapshot.UpdateMutableFields(&expectedQuota); err != nil {
		t.Fatalf("expected refreshed quota CAS to succeed, got %v", err)
	}
	if err := DB.First(&persisted, 1).Error; err != nil {
		t.Fatalf("expected updated token, got %v", err)
	}
	if persisted.UserId != 1 || persisted.RemainQuota != 1075 || persisted.Name != "renamed" {
		t.Fatalf("unexpected successful token update: %+v", persisted)
	}
}

func resetBatchUpdateStoresForTest() {
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateLocks[i].Lock()
		batchUpdateStores[i] = make(map[int]int)
		batchUpdateLocks[i].Unlock()
	}
}
