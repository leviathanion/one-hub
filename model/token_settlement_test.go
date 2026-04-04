package model

import (
	"fmt"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useTokenSettlementTestDB(t *testing.T) {
	t.Helper()

	logger.Logger = zap.NewNop()

	originalDB := DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&User{}, &Token{}); err != nil {
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

func resetBatchUpdateStoresForTest() {
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateLocks[i].Lock()
		batchUpdateStores[i] = make(map[int]int)
		batchUpdateLocks[i].Unlock()
	}
}

func TestApplyTokenUserQuotaDeltaDirectAbsorbsPendingBatchReserve(t *testing.T) {
	useTokenSettlementTestDB(t)
	insertTokenSettlementFixtures(t)

	originalBatch := config.BatchUpdateEnabled
	config.BatchUpdateEnabled = true
	resetBatchUpdateStoresForTest()
	t.Cleanup(func() {
		config.BatchUpdateEnabled = originalBatch
		resetBatchUpdateStoresForTest()
	})

	if err := DecreaseUserQuota(1, 100); err != nil {
		t.Fatalf("expected user reserve enqueue to succeed, got %v", err)
	}
	if err := DecreaseTokenQuota(1, 100); err != nil {
		t.Fatalf("expected token reserve enqueue to succeed, got %v", err)
	}

	var user User
	if err := DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	if user.Quota != 1000 {
		t.Fatalf("expected batched reserve to stay pending before settlement, got %d", user.Quota)
	}

	if err := ApplyTokenUserQuotaDeltaDirect(1, 1, false, 150); err != nil {
		t.Fatalf("expected direct settlement to succeed, got %v", err)
	}

	var token Token
	if err := DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup after settlement to succeed, got %v", err)
	}
	if err := DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup after settlement to succeed, got %v", err)
	}
	if user.Quota != 750 {
		t.Fatalf("expected final user quota 750 after absorbing pending reserve, got %d", user.Quota)
	}
	if token.RemainQuota != 750 || token.UsedQuota != 250 {
		t.Fatalf("expected final token quota after absorbing pending reserve, got remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}

	batchUpdate()

	if err := DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup after batch flush to succeed, got %v", err)
	}
	if err := DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup after batch flush to succeed, got %v", err)
	}
	if user.Quota != 750 {
		t.Fatalf("expected batch flush not to re-apply absorbed reserve, got %d", user.Quota)
	}
	if token.RemainQuota != 750 || token.UsedQuota != 250 {
		t.Fatalf("expected batch flush not to mutate final token quota, got remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}

func TestApplyTokenUserQuotaDeltaDirectFlushesPendingBatchReserveAtZeroDelta(t *testing.T) {
	useTokenSettlementTestDB(t)
	insertTokenSettlementFixtures(t)

	originalBatch := config.BatchUpdateEnabled
	config.BatchUpdateEnabled = true
	resetBatchUpdateStoresForTest()
	t.Cleanup(func() {
		config.BatchUpdateEnabled = originalBatch
		resetBatchUpdateStoresForTest()
	})

	if err := DecreaseUserQuota(1, 100); err != nil {
		t.Fatalf("expected user reserve enqueue to succeed, got %v", err)
	}
	if err := DecreaseTokenQuota(1, 100); err != nil {
		t.Fatalf("expected token reserve enqueue to succeed, got %v", err)
	}

	if err := ApplyTokenUserQuotaDeltaDirect(1, 1, false, 0); err != nil {
		t.Fatalf("expected zero-delta settlement to flush pending reserve, got %v", err)
	}

	var user User
	var token Token
	if err := DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup after zero-delta settlement to succeed, got %v", err)
	}
	if err := DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup after zero-delta settlement to succeed, got %v", err)
	}
	if user.Quota != 900 {
		t.Fatalf("expected zero-delta settlement to flush reserve into DB, got %d", user.Quota)
	}
	if token.RemainQuota != 900 || token.UsedQuota != 100 {
		t.Fatalf("expected zero-delta settlement to flush token reserve into DB, got remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}
