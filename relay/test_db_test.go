package relay

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var relayTestDBCounter uint64

func setupRelayTestDB(t *testing.T, models ...any) *gorm.DB {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "_")
	id := atomic.AddUint64(&relayTestDBCounter, 1)
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", name, id)), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if len(models) > 0 {
		if err := testDB.AutoMigrate(models...); err != nil {
			t.Fatalf("expected test database schema migration, got %v", err)
		}
	}
	originalDB := model.DB
	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
		if sqlDB, dbErr := testDB.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	return testDB
}
