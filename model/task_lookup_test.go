package model

import (
	"errors"
	"fmt"
	"testing"

	"one-api/common/logger"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useTaskLookupTestDB(t *testing.T) {
	t.Helper()

	logger.Logger = zap.NewNop()

	originalDB := DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
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

	duplicateTasks := []Task{
		{Platform: TaskPlatformSuno, UserId: 1, TaskID: "dup-task"},
		{Platform: TaskPlatformSuno, UserId: 1, TaskID: "dup-task"},
	}
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

	fixtures := []Task{
		{Platform: TaskPlatformSuno, UserId: 1, TaskID: "dup-task"},
		{Platform: TaskPlatformSuno, UserId: 1, TaskID: "dup-task"},
		{Platform: TaskPlatformSuno, UserId: 1, TaskID: "ok-task"},
	}
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
