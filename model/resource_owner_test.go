package model

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func resourceOwnerFixture(t *testing.T) (*ResourceOwnerRepository, ResourceOwnerReservation, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resource-owner.db")
	db, err := gorm.Open(sqlite.Open(path+"?_busy_timeout=5000"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&User{}, &ResourceOwner{}, &ResponseOwner{}); err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= 2; id++ {
		if err := db.Create(&User{Id: id, Username: fmt.Sprintf("resource-user-%d", id), AccessToken: fmt.Sprintf("resource-token-%d", id), AffCode: fmt.Sprintf("resource-aff-%d", id)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return NewResourceOwnerRepository(db), ResourceOwnerReservation{Kind: "file", UserID: 1, TokenID: 1, ChannelID: 10, ProviderNamespace: "openai", ProviderScope: "provider-wide", SubmitDeadline: time.Now().Add(time.Minute)}, path
}

func TestResourceOwnerAuthorizationBindingAndDeleteObservation(t *testing.T) {
	r, spec, _ := resourceOwnerFixture(t)
	ctx := context.Background()
	reserved, err := r.Reserve(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "file", "file-1", 1); !errors.Is(err, ErrResourceOwnerNotFound) {
		t.Fatalf("reservation authorized: %v", err)
	}
	owner, err := r.Bind(ctx, reserved.ID, 1, "file-1")
	if err != nil {
		t.Fatal(err)
	}
	if owner.ChannelID != 10 || owner.TokenID != 1 || owner.Phase != ResourceOwnerBound {
		t.Fatalf("incorrect owner: %+v", owner)
	}
	if _, err := r.Bind(ctx, reserved.ID, 1, "file-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Bind(ctx, reserved.ID, 1, "file-2"); !errors.Is(err, ErrResourceOwnerConflict) {
		t.Fatalf("rebound identity: %v", err)
	}
	if _, err := r.Get(ctx, "file", "file-1", 2); !errors.Is(err, ErrResourceOwnerNotFound) {
		t.Fatalf("other user authorized: %v", err)
	}
	if _, err := r.Get(ctx, "upload", "file-1", 1); !errors.Is(err, ErrResourceOwnerNotFound) {
		t.Fatalf("other kind authorized: %v", err)
	}
	if err := r.ObserveDelete(ctx, "file", "file-1", 1); err != nil {
		t.Fatal(err)
	}
	deleted, err := r.Get(ctx, "file", "file-1", 1)
	if err != nil || deleted.DeleteObservedAt == nil {
		t.Fatalf("deletion revoked authorization: %+v %v", deleted, err)
	}
	if err := r.Release(ctx, reserved.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "file", "file-1", 1); err != nil {
		t.Fatal("release deleted bound owner", err)
	}
	var count int64
	count, err = resourceOwnerCount(r.DB, 1, time.Now())
	if err != nil || count != 0 {
		t.Fatalf("tombstone consumes active capacity: %d %v", count, err)
	}
	if n, err := r.Cleanup(ctx, time.Now().Add(ResourceOwnerTombstoneRetention+time.Minute)); err != nil || n != 1 {
		t.Fatalf("cleanup: %d %v", n, err)
	}
	if _, err := r.Get(ctx, "file", "file-1", 1); !errors.Is(err, ErrResourceOwnerNotFound) {
		t.Fatalf("missing proof authorized: %v", err)
	}
}

func TestResourceOwnerIdentityCollisionsAndParentScope(t *testing.T) {
	r, spec, _ := resourceOwnerFixture(t)
	ctx := context.Background()
	p, _ := r.Reserve(ctx, spec)
	if _, err := r.Bind(ctx, p.ID, 1, "file-shared"); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ResourceOwnerReservation){func(s *ResourceOwnerReservation) { s.ChannelID = 11 }, func(s *ResourceOwnerReservation) { s.UserID = 2 }} {
		other := spec
		change(&other)
		o, err := r.Reserve(ctx, other)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Bind(ctx, o.ID, other.UserID, "file-shared"); !errors.Is(err, ErrResourceOwnerConflict) {
			t.Fatalf("ambiguous ID rebound: %v", err)
		}
	}
	child := spec
	child.ParentID = &p.ID
	child.ChannelID = 11
	if _, err := r.Reserve(ctx, child); !errors.Is(err, ErrResourceOwnerNotFound) {
		t.Fatalf("cross-channel parent accepted: %v", err)
	}
	child.ChannelID = 10
	child.UserID = 2
	if _, err := r.Reserve(ctx, child); !errors.Is(err, ErrResourceOwnerNotFound) {
		t.Fatalf("other user's parent accepted: %v", err)
	}
	child.UserID = 1
	if _, err := r.Reserve(ctx, child); err != nil {
		t.Fatal(err)
	}
}

func TestResourceOwnerTaskTransferCleanupAndRestart(t *testing.T) {
	r, spec, path := resourceOwnerFixture(t)
	ctx := context.Background()
	spec.TaskOwnerID = "task-owned"
	spec.Slot = "output"
	o, err := r.Reserve(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DB.Transaction(func(tx *gorm.DB) error { return NewResourceOwnerRepository(tx).TransferToTask(ctx, 1, "task-owned") }); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Cleanup(ctx, time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("cleaner reclaimed task obligation: %d %v", n, err)
	}
	reopened, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := reopened.DB()
	defer sqlDB.Close()
	r = NewResourceOwnerRepository(reopened)
	slots, err := r.TaskSlots(ctx, 1, "task-owned")
	if err != nil || len(slots) != 1 || slots[0].ReservationKind != ResourceReservationTaskDerived {
		t.Fatalf("restart lost ownership: %+v %v", slots, err)
	}
	if _, err := r.Bind(ctx, o.ID, 1, "file-output"); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseTaskSlots(ctx, 1, "task-owned"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "file", "file-output", 1); err != nil {
		t.Fatalf("task release removed bound output: %v", err)
	}
	spec.Slot = "error"
	unused, err := r.Reserve(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.TransferToTask(ctx, 1, "task-owned"); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseTaskSlots(ctx, 1, "task-owned"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Bind(ctx, unused.ID, 1, "late-file"); !errors.Is(err, ErrResourceOwnerNotFound) {
		t.Fatalf("late result reclaimed: %v", err)
	}
}

func TestResourceOwnerExpiredReservationAndAtomicAcceptanceRollback(t *testing.T) {
	r, spec, _ := resourceOwnerFixture(t)
	ctx := context.Background()
	spec.TaskOwnerID = "task-rollback"
	spec.Slot = "output"
	o, err := r.Reserve(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("acceptance failed")
	err = r.DB.Transaction(func(tx *gorm.DB) error {
		if err := NewResourceOwnerRepository(tx).TransferToTask(ctx, 1, "task-rollback"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	var got ResourceOwner
	if err := r.DB.First(&got, o.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ReservationKind != ResourceReservationSubmit {
		t.Fatal("transfer escaped acceptance rollback")
	}
	expired := time.Now().Add(-time.Second)
	if err := r.DB.Model(&ResourceOwner{}).Where("id = ?", o.ID).Update("reservation_expires_at", expired).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := r.Bind(ctx, o.ID, 1, "late-file"); !errors.Is(err, ErrResourceReservationExpired) {
		t.Fatalf("expired reserve revived: %v", err)
	}
	if err := r.TransferToTask(ctx, 1, "task-rollback"); !errors.Is(err, ErrResourceReservationExpired) {
		t.Fatalf("expired reserve transferred: %v", err)
	}
	if n, err := r.Cleanup(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("expired cleanup: %d %v", n, err)
	}
}

func TestResourceOwnerCapacityAndConcurrentBind(t *testing.T) {
	r, spec, _ := resourceOwnerFixture(t)
	ctx := context.Background()
	rows := make([]ResourceOwner, ResourceOwnerLimit-1)
	for i := range rows {
		id := fmt.Sprintf("file-capacity-%d", i)
		rows[i] = ResourceOwner{Kind: "file", UpstreamID: &id, UserID: 1, ChannelID: 10, ProviderNamespace: "openai", ProviderScope: "provider-wide", Phase: ResourceOwnerBound, ReservationKind: ResourceReservationSubmit}
	}
	if err := r.DB.CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	owners := make(chan *ResourceOwner, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := r.Reserve(ctx, spec)
			if err == nil {
				owners <- o
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	close(owners)
	success, capacity := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrResourceOwnerCapacity) {
			capacity++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || capacity != 1 {
		t.Fatalf("quota race: success=%d capacity=%d", success, capacity)
	}
	o := <-owners
	results = make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := r.Bind(ctx, o.ID, 1, fmt.Sprintf("file-winner-%d", i))
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrResourceOwnerConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("bind race: success=%d conflict=%d", success, conflict)
	}
}

func TestResourceOwnerUploadLocalRetention(t *testing.T) {
	r, spec, _ := resourceOwnerFixture(t)
	ctx := context.Background()
	spec.Kind = "upload"
	o, err := r.Reserve(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	o, err = r.Bind(ctx, o.ID, 1, "upload-1")
	if err != nil {
		t.Fatal(err)
	}
	if o.RetainUntil == nil || time.Until(*o.RetainUntil) < ResourceUploadRetention-time.Minute {
		t.Fatal("missing local upload retention")
	}
	upstreamExpired := time.Now().Add(-time.Hour)
	if err := r.DB.Model(o).Update("upstream_expires_at", upstreamExpired).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "upload", "upload-1", 1); err != nil {
		t.Fatalf("upstream expiry changed local authorization: %v", err)
	}
}
