package model

import (
	"errors"
	"testing"

	"one-api/common/logger"
	"one-api/internal/testutil/sqlitetest"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useTaskLookupTestDB(t *testing.T) {
	t.Helper()

	logger.Logger = zap.NewNop()

	originalDB := DB
	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Task{}); err != nil {
		t.Fatalf("expected task lookup schema migration to succeed, got %v", err)
	}

	DB = testDB
	t.Cleanup(func() {
		DB = originalDB
	})
}

func TestGetTaskByTaskIdFailsClosedOnDuplicateMatches(t *testing.T) {
	useTaskLookupTestDB(t)
	if err := DB.Migrator().DropIndex(&Task{}, "idx_task_provider_identity"); err != nil {
		t.Fatalf("drop provider identity index: %v", err)
	}
	if err := DB.Migrator().DropIndex(&Task{}, "idx_task_public_identity"); err != nil {
		t.Fatalf("drop public identity index: %v", err)
	}

	duplicateTasks := []Task{
		{Platform: TaskPlatformSuno, UserId: 1},
		{Platform: TaskPlatformSuno, UserId: 1},
	}
	SetTaskProviderID(&duplicateTasks[0], "dup-task")
	SetTaskProviderID(&duplicateTasks[1], "dup-task")
	for i := range duplicateTasks {
		if err := DB.Create(&duplicateTasks[i]).Error; err != nil {
			t.Fatalf("expected duplicate task fixture insert to succeed, got %v", err)
		}
	}

	task, err := GetTaskByTaskId(TaskPlatformSuno, 1, "dup-task")
	if task != nil {
		t.Fatalf("expected duplicate lookup to fail before returning a task, got %+v", task)
	}
	if !errors.Is(err, ErrTaskLookupConflict) {
		t.Fatalf("expected duplicate lookup to return ErrTaskLookupConflict, got %v", err)
	}
}

func TestGetTaskByTaskIdsFailsClosedOnDuplicateMatches(t *testing.T) {
	useTaskLookupTestDB(t)
	if err := DB.Migrator().DropIndex(&Task{}, "idx_task_provider_identity"); err != nil {
		t.Fatalf("drop provider identity index: %v", err)
	}
	if err := DB.Migrator().DropIndex(&Task{}, "idx_task_public_identity"); err != nil {
		t.Fatalf("drop public identity index: %v", err)
	}

	fixtures := []Task{
		{Platform: TaskPlatformSuno, UserId: 1},
		{Platform: TaskPlatformSuno, UserId: 1},
		{Platform: TaskPlatformSuno, UserId: 1},
	}
	SetTaskProviderID(&fixtures[0], "dup-task")
	SetTaskProviderID(&fixtures[1], "dup-task")
	SetTaskProviderID(&fixtures[2], "ok-task")
	for i := range fixtures {
		if err := DB.Create(&fixtures[i]).Error; err != nil {
			t.Fatalf("expected task fixture insert to succeed, got %v", err)
		}
	}

	tasks, err := GetTaskByTaskIds(TaskPlatformSuno, 1, []string{"dup-task", "ok-task"})
	if len(tasks) != 0 {
		t.Fatalf("expected duplicate batch lookup to fail before returning tasks, got %d records", len(tasks))
	}
	if !errors.Is(err, ErrTaskLookupConflict) {
		t.Fatalf("expected duplicate batch lookup to return ErrTaskLookupConflict, got %v", err)
	}
}
