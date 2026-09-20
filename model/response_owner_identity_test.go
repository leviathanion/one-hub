package model

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// SQLite NOCASE 夹具模拟 MySQL 宽松 collation 的候选命中；授权仍须逐字核对。
func responseIdentityCasefoldTable(t *testing.T, db *gorm.DB, table, column, columnType string, record any) {
	t.Helper()
	var schema string
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&schema).Error; err != nil {
		t.Fatal(err)
	}
	original := "`" + column + "` " + columnType
	changed := strings.Replace(schema, original, original+" COLLATE NOCASE", 1)
	if changed == schema {
		t.Fatalf("column missing in fixture schema: %s", schema)
	}
	if err := db.Migrator().DropTable(record); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(changed).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(record); err != nil {
		t.Fatal(err)
	}
}

func TestResponseOwnerPreservesRawIdentity(t *testing.T) {
	db := newResponseOwnerTestDB(t)
	now := time.Now()
	const id = " resp-x "
	owner, err := NewResponseOwner(id, 11, 22, 33, now)
	if err != nil {
		t.Fatal(err)
	}
	if owner.ResponseID != id {
		t.Fatalf("constructor rewrote ID: %q", owner.ResponseID)
	}
	if err := createResponseOwner(db, owner); err != nil {
		t.Fatal(err)
	}
	got, err := getResponseOwner(db, id, now, 11)
	if err != nil || got.ResponseID != id {
		t.Fatalf("raw lookup: %+v %v", got, err)
	}
	if _, err := getResponseOwner(db, strings.TrimSpace(id), now, 11); !errors.Is(err, ErrResponseOwnerNotFound) {
		t.Fatalf("trimmed alias authorized: %v", err)
	}
	if err := tombstoneResponseOwner(db, strings.TrimSpace(id), 11, now); !errors.Is(err, ErrResponseOwnerNotFound) {
		t.Fatalf("trimmed alias deleted: %v", err)
	}
	if err := tombstoneResponseOwner(db, id, 11, now); err != nil {
		t.Fatal(err)
	}
	got, err = getResponseOwner(db, id, now, 11)
	if err != nil || got.State != ResponseOwnerStateDeleted || got.ResponseID != id {
		t.Fatalf("raw tombstone: %+v %v", got, err)
	}
}

func TestResponseOwnerRejectsCollationAlias(t *testing.T) {
	db := newResponseOwnerTestDB(t)
	responseIdentityCasefoldTable(t, db, "response_owners", "response_id", "text", &ResponseOwner{})
	now := time.Now()
	owner, err := NewResponseOwner("resp_case", 11, 22, 33, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := createResponseOwner(db, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := getResponseOwner(db, "RESP_CASE", now, 11); !errors.Is(err, ErrResponseOwnerNotFound) {
		t.Fatalf("SQL collation alias authorized: %v", err)
	}
	if err := tombstoneResponseOwner(db, "RESP_CASE", 11, now); !errors.Is(err, ErrResponseOwnerNotFound) {
		t.Fatalf("SQL collation alias deleted: %v", err)
	}
	duplicate, err := NewResponseOwner("RESP_CASE", 11, 22, 33, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := createResponseOwner(db, duplicate); !errors.Is(err, ErrResponseOwnerConflict) {
		t.Fatalf("SQL collation alias deduplicated: %v", err)
	}
	got, err := getResponseOwner(db, "resp_case", now, 11)
	if err != nil || got.State != ResponseOwnerStateActive {
		t.Fatalf("alias mutated real owner: %+v %v", got, err)
	}
}
