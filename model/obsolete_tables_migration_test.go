package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

func seedObsoleteTables(t *testing.T, db *gorm.DB, count int) {
	t.Helper()
	if err := db.AutoMigrate(&Task{}); err != nil {
		t.Fatal(err)
	}
	execStartupFixture(t, db,
		"CREATE TABLE midjourneys (id INTEGER PRIMARY KEY, mj_id varchar(100), user_id integer, channel_id integer, status varchar(20), progress varchar(20), quota integer, submit_time bigint)",
		"CREATE TABLE abilities (channel_id integer)",
		"INSERT INTO abilities VALUES (2)",
		"CREATE TABLE unrelated_archive (id integer)",
		"INSERT INTO unrelated_archive VALUES (1)",
	)
	for id := 1; id <= count; id++ {
		if err := db.Exec("INSERT INTO midjourneys VALUES (?,?,1,2,'SUCCESS','100%',17,123000)", id, fmt.Sprintf("mj-%d", id)).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func runObsoleteTableCleanup(db *gorm.DB) error {
	return gormigrate.New(db, gormigrate.DefaultOptions, []*gormigrate.Migration{dropObsoleteTablesAfterMigration()}).Migrate()
}

func TestObsoleteTableCleanupAfterPreviouslyCompletedImport(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		seedObsoleteTables(t, db, 205)
		if err := gormigrate.New(db, gormigrate.DefaultOptions, []*gormigrate.Migration{migrateHistoricalMidjourney()}).Migrate(); err != nil {
			t.Fatal(err)
		}
		for restart := 0; restart < 2; restart++ {
			if err := runObsoleteTableCleanup(db); err != nil {
				t.Fatal(err)
			}
		}
		for _, table := range []string{"midjourneys", "abilities"} {
			if db.Migrator().HasTable(table) {
				t.Fatalf("迁移成功后仍保留旧表 %s", table)
			}
		}
		var count int64
		if err := db.Model(&Task{}).Where("provider_state = ? AND charged_quota = ?", TaskProviderStateClosed, 17).Count(&count).Error; err != nil || count != 205 {
			t.Fatalf("删表后任务丢失: count=%d err=%v", count, err)
		}
		if !db.Migrator().HasTable("unrelated_archive") {
			t.Fatal("删除了清理名单以外的表")
		}
	})
}

func TestObsoleteTableCleanupKeepsSourceOnConflict(t *testing.T) {
	for _, change := range []string{
		"UPDATE tasks SET token_id=99",
		"UPDATE tasks SET status='FAILURE'",
		"UPDATE tasks SET provider_namespace='different'",
		"UPDATE tasks SET charged_quota=0",
		"UPDATE midjourneys SET status='IN_PROGRESS'",
		"ALTER TABLE midjourneys ADD COLUMN unconverted_detail text",
	} {
		t.Run(change, func(t *testing.T) {
			db := paymentSchemaDB(t, false)
			seedObsoleteTables(t, db, 1)
			if err := migrateHistoricalMidjourney().Migrate(db); err != nil {
				t.Fatal(err)
			}
			execStartupFixture(t, db, change)
			if err := runObsoleteTableCleanup(db); err == nil {
				t.Fatal("未核实目标记录就删除了旧表")
			}
			for _, table := range []string{"midjourneys", "abilities"} {
				if !db.Migrator().HasTable(table) {
					t.Fatalf("核对失败仍删除了 %s", table)
				}
			}
		})
	}
}

func TestObsoleteTableCleanupResumesAfterDDLFailure(t *testing.T) {
	db := paymentSchemaDB(t, false)
	seedObsoleteTables(t, db, 2)
	const callback = "test:fail_obsolete_table_drop"
	if err := db.Callback().Raw().Before("gorm:raw").Register(callback, func(tx *gorm.DB) {
		if strings.HasPrefix(tx.Statement.SQL.String(), "DROP TABLE") && strings.Contains(tx.Statement.SQL.String(), "abilities") {
			tx.AddError(errors.New("模拟删表失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Raw().Remove(callback) })
	if err := runObsoleteTableCleanup(db); err == nil || !strings.Contains(err.Error(), "abilities") {
		t.Fatalf("未传播 DDL 错误: %v", err)
	}
	if db.Migrator().HasTable("midjourneys") || !db.Migrator().HasTable("abilities") {
		t.Fatal("未模拟出部分 DDL 已提交的状态")
	}
	var count int64
	if err := db.Table("migrations").Where("id = ?", dropObsoleteTablesAfterMigration().ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("删表失败被记录为迁移成功: count=%d err=%v", count, err)
	}
	if err := db.Callback().Raw().Remove(callback); err != nil {
		t.Fatal(err)
	}
	if err := runObsoleteTableCleanup(db); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasTable("abilities") {
		t.Fatal("未继续清理剩余旧表")
	}
	if err := db.Model(&Task{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("恢复后目标任务丢失或重复: count=%d err=%v", count, err)
	}
}

func TestObsoleteTableCleanupEmptySource(t *testing.T) {
	db := paymentSchemaDB(t, false)
	execStartupFixture(t, db, "CREATE TABLE midjourneys (id INTEGER PRIMARY KEY)")
	for restart := 0; restart < 2; restart++ {
		if err := dropObsoleteTablesAfterMigration().Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	if db.Migrator().HasTable("midjourneys") {
		t.Fatal("空旧表未清理")
	}
}

func TestHistoricalMidjourneyComparisonPreservesJSONIntegers(t *testing.T) {
	expected := Task{Data: []byte(`{"id":9007199254740992,"prompt":"original"}`)}
	existing := Task{Data: []byte("{\n  \"prompt\": \"original\", \"id\": 9007199254740992\n}")}
	if !sameHistoricalMidjourneyTask(existing, expected) {
		t.Fatal("数据库 JSON 排版变化不应阻止清理")
	}
	existing.Data = []byte(`{"id":9007199254740993,"prompt":"original"}`)
	if sameHistoricalMidjourneyTask(existing, expected) {
		t.Fatal("不能因浮点精度丢失而将不同历史记录视为相同")
	}
}
