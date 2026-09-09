package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"one-api/internal/testutil/sqlitetest"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPublicationReloadLockHonorsContext(t *testing.T) {
	var mutex sync.Mutex
	mutex.Lock()
	defer mutex.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := lockPublicationReload(ctx, &mutex)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait returned %v", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("lock ignored context deadline: %s", elapsed)
	}
}

func TestPublicationVersionOwnersAdvanceOnlyByCAS(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, owner := range []string{PublicationOwnerPrice, PublicationOwnerOptions, PublicationOwnerUserGroup} {
		version, err := ReadPublicationVersion(ctx, db, owner)
		if err != nil || version != 1 {
			t.Fatalf("owner %s initial version=%d err=%v", owner, version, err)
		}
		next, err := BumpPublicationVersionCAS(ctx, db, owner, version)
		if err != nil || next != 2 {
			t.Fatalf("owner %s next version=%d err=%v", owner, next, err)
		}
		if _, err := BumpPublicationVersionCAS(ctx, db, owner, version); !errors.Is(err, ErrPublicationVersionConflict) {
			t.Fatalf("owner %s stale CAS returned %v", owner, err)
		}
	}
	price, _ := ReadPublicationVersion(ctx, db, PublicationOwnerPrice)
	options, _ := ReadPublicationVersion(ctx, db, PublicationOwnerOptions)
	if price != 2 || options != 2 {
		t.Fatalf("independent owners diverged unexpectedly: price=%d options=%d", price, options)
	}
}

func TestCheckPublicationVersionSchemaRequiresAllOwnerRows(t *testing.T) {
	originalDB := DB
	t.Cleanup(func() { DB = originalDB })
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	DB = db
	if err := db.AutoMigrate(&PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := CheckPublicationVersionSchema(context.Background()); err == nil {
		t.Fatal("schema check accepted missing owner rows")
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	if err := CheckPublicationVersionSchema(context.Background()); err != nil {
		t.Fatalf("valid publication schema rejected: %v", err)
	}
}

func TestPublicationVersionSchemaRequiresUserGroupOwner(t *testing.T) {
	originalDB := DB
	t.Cleanup(func() { DB = originalDB })
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	DB = db
	if err := db.AutoMigrate(&PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("owner = ?", PublicationOwnerUserGroup).Delete(&PublicationVersion{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := CheckPublicationVersionSchema(context.Background()); err == nil {
		t.Fatal("missing user group owner accepted")
	}
}
