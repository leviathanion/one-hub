package model

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"

	"github.com/spf13/viper"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useConsumeLogTestDB(t *testing.T) {
	t.Helper()

	originalDB := DB
	originalRedisEnabled := config.RedisEnabled

	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
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
	RecordConsumeLog(context.Background(), 1, 2, 3, 4, 0, 0, 0, "gpt-test", "token-test", 5, infoMarker, 6, false, nil, "127.0.0.1")
	if countLatestRuntimeLogs(infoMarker) != 0 {
		t.Fatal("expected consume runtime log to be hidden at info level")
	}

	viper.Set("log_level", "debug")
	config.LogConsumeEnabled = false
	disabledMarker := "consume-log-disabled-hidden"
	RecordConsumeLog(context.Background(), 1, 2, 3, 4, 0, 0, 0, "gpt-test", "token-test", 5, disabledMarker, 6, false, nil, "127.0.0.1")
	if countLatestRuntimeLogs(disabledMarker) != 0 {
		t.Fatal("expected disabled consume logging to skip runtime debug log")
	}

	config.LogConsumeEnabled = true
	debugMarker := "consume-log-debug-visible"
	RecordConsumeLog(context.Background(), 1, 2, 3, 4, 0, 0, 0, "gpt-test", "token-test", 5, debugMarker, 6, false, nil, "127.0.0.1")
	if countLatestRuntimeLogs(debugMarker) != 1 {
		t.Fatal("expected consume runtime log to be visible at debug level")
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
