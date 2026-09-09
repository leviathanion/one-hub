package model

import (
	"errors"
	"testing"
	"time"

	"one-api/internal/testutil/sqlitetest"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newResponseOwnerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ResponseOwner{}); err != nil {
		t.Fatalf("migrate response owner: %v", err)
	}
	return db
}

func TestCheckResponseOwnerSchemaRequiresDurableRoutingColumns(t *testing.T) {
	partialDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("open partial response owner database: %v", err)
	}
	if err := partialDB.Exec("CREATE TABLE response_owners (response_id text primary key)").Error; err != nil {
		t.Fatalf("create partial response owner schema: %v", err)
	}
	if err := checkResponseOwnerSchema(partialDB); err == nil {
		t.Fatal("expected partial response owner schema to fail readiness probe")
	}

	completeDB := newResponseOwnerTestDB(t)
	if err := checkResponseOwnerSchema(completeDB); err != nil {
		t.Fatalf("expected complete response owner schema to pass readiness probe: %v", err)
	}
	if err := completeDB.Migrator().DropIndex(&ResponseOwner{}, "idx_response_owner_public_identity"); err != nil {
		t.Fatalf("drop public identity index: %v", err)
	}
	if err := checkResponseOwnerSchema(completeDB); err == nil {
		t.Fatal("expected missing public identity index to fail readiness probe")
	}
}

func TestResponseOwnerCreateLookupAndConflict(t *testing.T) {
	db := newResponseOwnerTestDB(t)
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	owner, err := NewResponseOwner("resp_owner", 11, 22, 33, now)
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}
	if err := createResponseOwner(db, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := createResponseOwner(db, owner); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}

	got, err := getResponseOwner(db, owner.ResponseID, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("get owner: %v", err)
	}
	if got.UserID != 11 || got.TokenID != 22 || got.ChannelID != 33 || got.State != ResponseOwnerStateActive {
		t.Fatalf("unexpected owner: %+v", got)
	}

	conflict := *owner
	conflict.UserID = 12
	if err := createResponseOwner(db, &conflict); !errors.Is(err, ErrResponseOwnerConflict) {
		t.Fatalf("expected ownership conflict, got %v", err)
	}
}

func TestResponseOwnerIdentityConstraints(t *testing.T) {
	db := newResponseOwnerTestDB(t)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	create := func(responseID string, userID, channelID int, namespace, scope string) error {
		t.Helper()
		owner, err := NewResponseOwner(responseID, userID, 1, channelID, now, namespace, scope)
		if err != nil {
			return err
		}
		return createResponseOwner(db, owner)
	}

	if err := create("resp_shared", 11, 31, "openai", "account-a"); err != nil {
		t.Fatalf("create first physical owner: %v", err)
	}
	if err := create("resp_shared", 12, 32, "openai", "account-a"); !errors.Is(err, ErrResponseOwnerConflict) {
		t.Fatalf("same physical provider identity must conflict across users, got %v", err)
	}
	if err := create("resp_shared", 11, 33, "openai", "account-b"); !errors.Is(err, ErrResponseOwnerConflict) {
		t.Fatalf("same public identity must conflict across account scopes, got %v", err)
	}
	if err := create("resp_shared", 12, 34, "openai", "account-b"); err != nil {
		t.Fatalf("different public and physical identities should coexist: %v", err)
	}

	first, err := getResponseOwner(db, "resp_shared", now.Add(time.Minute), 11)
	if err != nil || first.ChannelID != 31 {
		t.Fatalf("user-scoped lookup returned wrong owner: owner=%+v err=%v", first, err)
	}
	second, err := getResponseOwner(db, "resp_shared", now.Add(time.Minute), 12)
	if err != nil || second.ChannelID != 34 {
		t.Fatalf("second user-scoped lookup returned wrong owner: owner=%+v err=%v", second, err)
	}
	if _, err := getResponseOwner(db, "resp_shared", now.Add(time.Minute)); !errors.Is(err, ErrResponseOwnerConflict) {
		t.Fatalf("unscoped ambiguous lookup must fail closed, got %v", err)
	}
}

func TestResponseOwnerTombstoneExpiryAndCleanup(t *testing.T) {
	db := newResponseOwnerTestDB(t)
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	owner, err := NewResponseOwner("resp_deleted", 11, 22, 33, now)
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}
	if err := createResponseOwner(db, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	deleteAt := owner.ExpiresAt.Add(-time.Hour)
	if err := tombstoneResponseOwner(db, owner.ResponseID, 12, deleteAt); !errors.Is(err, ErrResponseOwnerNotFound) {
		t.Fatalf("cross-user tombstone must look missing, got %v", err)
	}
	if err := tombstoneResponseOwner(db, owner.ResponseID, 11, deleteAt); err != nil {
		t.Fatalf("tombstone owner: %v", err)
	}
	got, err := getResponseOwner(db, owner.ResponseID, deleteAt.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("get tombstone: %v", err)
	}
	if got.State != ResponseOwnerStateDeleted || got.DeletedAt == nil {
		t.Fatalf("expected tombstone, got %+v", got)
	}
	if !got.ExpiresAt.Equal(owner.ExpiresAt) {
		t.Fatalf("tombstone must preserve original expiry, got %s want %s", got.ExpiresAt, owner.ExpiresAt)
	}
	if _, err := getResponseOwner(db, owner.ResponseID, owner.ExpiresAt); !errors.Is(err, ErrResponseOwnerNotFound) {
		t.Fatalf("owner must be expired at the exact boundary, got %v", err)
	}
	if _, err := getResponseOwner(db, owner.ResponseID, owner.ExpiresAt.Add(time.Second)); !errors.Is(err, ErrResponseOwnerNotFound) {
		t.Fatalf("expired owner must look missing, got %v", err)
	}
	result := db.Where("expires_at <= ?", owner.ExpiresAt.Add(time.Second)).Delete(&ResponseOwner{})
	if result.Error != nil || result.RowsAffected != 1 {
		t.Fatalf("cleanup expired owner: rows=%d err=%v", result.RowsAffected, result.Error)
	}
}
