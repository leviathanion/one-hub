package catalogmigration

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"one-api/internal/catalogschema"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "catalog.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return db
}

func exec(t *testing.T, db *gorm.DB, sql string, args ...interface{}) {
	t.Helper()
	if err := db.Exec(sql, args...).Error; err != nil {
		t.Fatal(err)
	}
}

func seedLegacy(t *testing.T, db *gorm.DB) {
	t.Helper()
	idType := "integer PRIMARY KEY"
	priceModel := "varchar(100)"
	switch db.Dialector.Name() {
	case "postgres":
		idType = "bigserial PRIMARY KEY"
	case "mysql":
		idType = "bigint AUTO_INCREMENT PRIMARY KEY"
		priceModel = "varchar(100) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"
	}
	for _, statement := range []string{
		"CREATE TABLE model_info (id " + idType + ", model varchar(100), name varchar(100), description text, created_at bigint, updated_at bigint)",
		"CREATE INDEX idx_model_info_model ON model_info (model)",
		"CREATE TABLE model_owned_by (id integer PRIMARY KEY, name varchar(100))",
		"CREATE TABLE prices (model " + priceModel + " PRIMARY KEY, channel_type bigint, input decimal(20,8), output decimal(20,8), locked boolean, extra_ratios text, rate_rules text)",
		"CREATE TABLE channels (id integer PRIMARY KEY, models text, status integer)",
		"CREATE TABLE price_publications (id integer PRIMARY KEY, version bigint)",
		"CREATE TABLE logs (id integer PRIMARY KEY, quota bigint)",
	} {
		exec(t, db, statement)
	}
	exec(t, db, "INSERT INTO model_owned_by (id, name) VALUES (1, 'OpenAI'), (1001, '托管分类')")
	exec(t, db, "INSERT INTO model_info (id, model, name, description, created_at, updated_at) VALUES (1, 'M-exact', '标题', '说明', 11, 12), (2, 'no-price', '保留描述', '', 21, 22), (3, 'M-dangling', '孤立归属', '', 31, 32)")
	if db.Dialector.Name() == "postgres" {
		exec(t, db, "SELECT setval(pg_get_serial_sequence('model_info', 'id'), 3)")
	}
	exec(t, db, "INSERT INTO prices (model, channel_type, input, output, locked, extra_ratios, rate_rules) VALUES ('M*', 1, 0.123, 0.456, true, '{}', '[]'), ('M-long*', 1001, 2, 3, false, '{}', '[]'), ('M-exact', 1001, 4, 5, true, '{}', '[]'), ('M-dangling', 9999, 6, 7, false, '{}', '[]'), ('M-none', 0, 8, 9, false, '{}', '[]'), ('Case', 1, 1, 1, false, '{}', '[]'), ('case', 1001, 1, 1, false, '{}', '[]')")
	exec(t, db, "INSERT INTO channels (id, models, status) VALUES (1, 'M-long-disabled,M-short,M-none,M-dangling,no-owner,M*', 2)")
	exec(t, db, "INSERT INTO prices (model, channel_type, input, output, locked, extra_ratios, rate_rules) VALUES ('M-multi***', 1001, 1, 2, false, '{}', '[]'), ('literal*middle', 1, 1, 2, false, '{}', '[]')")
	exec(t, db, "INSERT INTO channels (id, models, status) VALUES (3, 'M-multi-disabled', 2)")
	exec(t, db, "INSERT INTO price_publications (id, version) VALUES (1, 42)")
	exec(t, db, "INSERT INTO logs (id, quota) VALUES (1, 123456)")
}

func TestMigrationMovesExactAndLongestPrefixAttributionWithoutChangingBilling(t *testing.T) {
	db := openSQLite(t)
	verifyMigration(t, db)
}

func verifyMigration(t *testing.T, db *gorm.DB) {
	t.Helper()
	seedLegacy(t, db)
	var before []map[string]interface{}
	if err := db.Table("prices").Select("model, input, output, locked, extra_ratios, rate_rules").Order("model").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	report, err := Run(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied || report.Created != 6 || report.Assigned != 7 || len(report.Dangling) != 1 || report.Dangling[0] != "M-dangling" {
		t.Fatalf("unexpected preview: %+v", report)
	}
	if db.Migrator().HasColumn("model_info", "owned_by_id") {
		t.Fatal("preview mutated schema")
	}
	if err := catalogschema.Validate(db); err == nil {
		t.Fatal("runtime accepted legacy schema")
	}
	report, err = Run(db, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Applied {
		t.Fatal("apply not reported")
	}
	if err := catalogschema.Validate(db); err != nil {
		t.Fatal(err)
	}
	var after []map[string]interface{}
	if err := db.Table("prices").Select("model, input, output, locked, extra_ratios, rate_rules").Order("model").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("prices changed: %v -> %v", before, after)
	}
	for name, expected := range map[string]int{"M-exact": 1001, "M-long-disabled": 1001, "M-short": 1, "Case": 1, "case": 1001, "M-multi-disabled": 1001, "literal*middle": 1} {
		var owner int
		if err := db.Table("model_info").Where("model = ?", name).Pluck("owned_by_id", &owner).Error; err != nil {
			t.Fatal(err)
		}
		if owner != expected {
			t.Fatalf("%s owner=%d, want %d", name, owner, expected)
		}
	}
	var emptyCount int64
	db.Table("model_info").Where("model IN ?", []string{"M-none", "no-owner", "M*"}).Count(&emptyCount)
	if emptyCount != 0 {
		t.Fatal("created empty or wildcard metadata")
	}
	var retained struct {
		Name        string
		Description string
		CreatedAt   int64
		UpdatedAt   int64
	}
	db.Table("model_info").Where("id = 1").First(&retained)
	if retained.Name != "标题" || retained.Description != "说明" || retained.CreatedAt != 11 || retained.UpdatedAt != 12 {
		t.Fatalf("metadata changed: %+v", retained)
	}
	var unset int64
	db.Table("model_info").Where("model IN ? AND owned_by_id IS NULL", []string{"no-price", "M-dangling"}).Count(&unset)
	if unset != 2 {
		t.Fatal("unset owners were not retained as NULL")
	}
	var version, quota int64
	db.Table("price_publications").Select("version").Scan(&version)
	db.Table("logs").Select("quota").Scan(&quota)
	if version != 42 || quota != 123456 {
		t.Fatal("publication or accounting changed")
	}
	if err := db.Exec("INSERT INTO model_info (model) VALUES ('M-exact')").Error; err == nil {
		t.Fatal("duplicate identity accepted")
	}
	if _, err := Run(db, true); !errors.Is(err, ErrAlreadyMigrated) {
		t.Fatalf("repeat apply: %v", err)
	}
}

func TestMigrationRejectsDuplicateMetadataBeforeAnyWrite(t *testing.T) {
	db := openSQLite(t)
	seedLegacy(t, db)
	exec(t, db, "INSERT INTO model_info (id, model, name) VALUES (4, 'M-exact', '另一条')")
	report, err := Run(db, true)
	if err == nil || len(report.Conflicts) != 1 || len(report.Conflicts[0].IDs) != 2 {
		t.Fatalf("missing conflicts: %+v %v", report, err)
	}
	if db.Migrator().HasColumn("model_info", "owned_by_id") {
		t.Fatal("preflight failure mutated schema")
	}
}

func TestMigrationRollsBackSchemaAndDataWhenUpdateFails(t *testing.T) {
	db := openSQLite(t)
	seedLegacy(t, db)
	exec(t, db, "CREATE TRIGGER fail_catalog_update BEFORE UPDATE ON model_info BEGIN SELECT RAISE(ABORT, 'test failure'); END")
	if _, err := Run(db, true); err == nil {
		t.Fatal("expected update failure")
	}
	if db.Migrator().HasColumn("model_info", "owned_by_id") || !db.Migrator().HasColumn("prices", "channel_type") {
		t.Fatal("transaction did not roll back DDL")
	}
}

func TestSchemaGuardRejectsPartialMigrationAndNonUniqueIndex(t *testing.T) {
	db := openSQLite(t)
	if err := catalogschema.Validate(db); err != nil {
		t.Fatalf("empty database: %v", err)
	}
	seedLegacy(t, db)
	exec(t, db, "ALTER TABLE model_info ADD COLUMN owned_by_id bigint")
	if err := catalogschema.Validate(db); err == nil {
		t.Fatal("partial schema accepted")
	}
	if _, err := Run(db, true); err == nil {
		t.Fatal("partial migration resumed")
	}
	if err := db.Exec("ALTER TABLE prices DROP COLUMN channel_type").Error; err != nil {
		t.Fatal(err)
	}
	if err := catalogschema.Validate(db); err == nil {
		t.Fatal("nonunique model index accepted")
	}
	if err := catalogschema.EnsureIdentity(db); err != nil {
		t.Fatal(err)
	}
	if err := catalogschema.Validate(db); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaGuardRejectsPartialUniqueIndex(t *testing.T) {
	db := openSQLite(t)
	seedLegacy(t, db)
	exec(t, db, "ALTER TABLE model_info ADD COLUMN owned_by_id bigint")
	exec(t, db, "ALTER TABLE prices DROP COLUMN channel_type")
	exec(t, db, "DROP INDEX idx_model_info_model")
	exec(t, db, "CREATE UNIQUE INDEX idx_model_info_model ON model_info (model) WHERE model <> 'M-exact'")
	if err := catalogschema.Validate(db); err == nil {
		t.Fatal("partial unique index accepted")
	}
}

func TestMigrationPreflightsUnrepresentableModelBeforeWrites(t *testing.T) {
	db := openSQLite(t)
	seedLegacy(t, db)
	exec(t, db, "INSERT INTO channels (id, models, status) VALUES (2, ?, 2)", "M"+strings.Repeat("x", 100))
	if _, err := Run(db, true); err == nil {
		t.Fatal("unrepresentable model accepted")
	}
	if db.Migrator().HasColumn("model_info", "owned_by_id") {
		t.Fatal("preflight failure mutated schema")
	}
}
