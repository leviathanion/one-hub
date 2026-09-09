package model

import (
	"testing"

	"one-api/internal/testutil/sqlitetest"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPriceMigrationRejectsLegacyDuplicates(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.Exec("CREATE TABLE prices (model varchar(100))").Error; err != nil {
		t.Fatalf("create legacy prices table: %v", err)
	}
	if err := db.Exec("INSERT INTO prices (model) VALUES (?), (?)", "duplicate-model", "duplicate-model").Error; err != nil {
		t.Fatalf("insert legacy duplicates: %v", err)
	}
	if err := db.AutoMigrate(&Price{}); err == nil {
		t.Fatal("migration must fail while legacy duplicate models exist")
	}

	if err := db.Exec("DELETE FROM prices WHERE model = ?", "duplicate-model").Error; err != nil {
		t.Fatalf("remove legacy duplicates: %v", err)
	}
	if err := db.Exec("INSERT INTO prices (model) VALUES (?)", "duplicate-model").Error; err != nil {
		t.Fatalf("restore one legacy row: %v", err)
	}
	if err := db.AutoMigrate(&Price{}); err != nil {
		t.Fatalf("migration after repairing duplicates: %v", err)
	}
	if !db.Migrator().HasIndex(&Price{}, "idx_prices_model_unique") {
		t.Fatal("migration did not create the price model unique index")
	}
	if err := db.Create(&Price{Model: "duplicate-model", Type: TokensPriceType}).Error; err == nil {
		t.Fatal("database must reject a later duplicate model")
	}
}
