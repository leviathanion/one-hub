package kling

import (
	"fmt"
	"testing"

	"one-api/common/logger"
	"one-api/model"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useKlingTaskSyncTestDB(t *testing.T) {
	t.Helper()

	logger.Logger = zap.NewNop()

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.Task{}); err != nil {
		t.Fatalf("expected task schema migration to succeed, got %v", err)
	}

	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})
}

func TestGetKlingTaskActionsForSyncUsesCanonicalPlatformName(t *testing.T) {
	useKlingTaskSyncTestDB(t)

	fixture := &model.Task{
		Platform: model.TaskPlatformKling,
		Action:   "text2video",
		TaskID:   "kling-task-1",
	}
	if err := model.DB.Create(fixture).Error; err != nil {
		t.Fatalf("expected kling task fixture insert to succeed, got %v", err)
	}

	tasks, err := getKlingTaskActionsForSync([]string{"kling-task-1"})
	if err != nil {
		t.Fatalf("expected kling task action lookup to succeed, got %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected one kling task action result, got %d", len(tasks))
	}
	if tasks[0].Action != "text2video" || tasks[0].TaskID != "kling-task-1" {
		t.Fatalf("expected kling task action lookup to return stored task, got %+v", tasks[0])
	}
}
