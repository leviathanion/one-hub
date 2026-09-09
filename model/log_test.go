package model

import (
	"context"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/internal/testutil/sqlitetest"

	"github.com/spf13/viper"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useConsumeLogTestDB(t *testing.T) {
	t.Helper()

	originalDB := DB
	originalRedisEnabled := config.RedisEnabled

	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&User{}, &Log{}); err != nil {
		t.Fatalf("expected consume log schema migration to succeed, got %v", err)
	}

	DB = testDB
	config.RedisEnabled = false
	t.Cleanup(func() {
		DB = originalDB
		config.RedisEnabled = originalRedisEnabled
	})
}

func TestRecordConsumeLogDebugRuntimeLogVisibility(t *testing.T) {
	useConsumeLogTestDB(t)

	originalLogger := logger.Logger
	originalLogLevel := viper.GetString("log_level")
	originalLogConsume := config.LogConsumeEnabled
	originalBatchUpdate := config.BatchUpdateEnabled
	logger.Logger = nil
	config.BatchUpdateEnabled = false
	t.Cleanup(func() {
		logger.Logger = originalLogger
		viper.Set("log_level", originalLogLevel)
		config.LogConsumeEnabled = originalLogConsume
		config.BatchUpdateEnabled = originalBatchUpdate
	})

	if err := DB.Create(&User{
		Id:          1,
		Username:    "alice",
		Password:    "password123",
		AccessToken: "access-token-1",
		Quota:       1000,
		Group:       "default",
		Status:      config.UserStatusEnabled,
		Role:        config.RoleCommonUser,
		CreatedTime: 1,
	}).Error; err != nil {
		t.Fatalf("expected user fixture to persist, got %v", err)
	}

	viper.Set("log_level", "info")
	config.LogConsumeEnabled = true
	infoMarker := "consume-log-info-hidden"
	infoBefore := countLatestRuntimeLogs(infoMarker)
	RecordConsumeLog(context.Background(), 1, 2, 3, 4, 0, 0, 0, "gpt-test", "token-test", 5, infoMarker, 6, false, nil, "127.0.0.1")
	if got := countLatestRuntimeLogs(infoMarker); got != infoBefore {
		t.Fatal("expected consume runtime log to be hidden at info level")
	}

	viper.Set("log_level", "debug")
	config.LogConsumeEnabled = false
	disabledMarker := "consume-log-disabled-hidden"
	disabledBefore := countLatestRuntimeLogs(disabledMarker)
	RecordConsumeLog(context.Background(), 1, 2, 3, 4, 0, 0, 0, "gpt-test", "token-test", 5, disabledMarker, 6, false, nil, "127.0.0.1")
	if got := countLatestRuntimeLogs(disabledMarker); got != disabledBefore {
		t.Fatal("expected disabled consume logging to skip runtime debug log")
	}

	config.LogConsumeEnabled = true
	debugMarker := "consume-log-debug-visible"
	debugBefore := countLatestRuntimeLogs(debugMarker)
	RecordConsumeLog(context.Background(), 1, 2, 3, 4, 0, 0, 0, "gpt-test", "token-test", 5, debugMarker, 6, false, nil, "127.0.0.1")
	if got := countLatestRuntimeLogs(debugMarker); got != debugBefore+1 {
		t.Fatal("expected consume runtime log to be visible at debug level")
	}
}

func TestRecordConsumeLogSurvivesRequestCancellation(t *testing.T) {
	useConsumeLogTestDB(t)
	originalLogConsume := config.LogConsumeEnabled
	originalBatchUpdate := config.BatchUpdateEnabled
	config.LogConsumeEnabled = true
	config.BatchUpdateEnabled = false
	t.Cleanup(func() {
		config.LogConsumeEnabled = originalLogConsume
		config.BatchUpdateEnabled = originalBatchUpdate
	})

	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	RecordConsumeLog(requestCtx, 99, 2, 0, 0, 0, 0, 0, "", "token-test", 0, "canceled-request-audit", 1, false, nil, "127.0.0.1")

	var count int64
	if err := DB.Model(&Log{}).Where("content = ?", "canceled-request-audit").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected consume audit to survive request cancellation, got %d rows", count)
	}
}

func countLatestRuntimeLogs(marker string) int {
	entries, _ := logger.GetLatestLogs(500)
	count := 0
	for _, entry := range entries {
		if strings.Contains(entry.Message, marker) {
			count++
		}
	}
	return count
}
