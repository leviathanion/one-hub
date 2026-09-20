package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResourceOwnerReplayPreservesOwnerAndReleasesReservation(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%t", deleted), func(t *testing.T) {
			repo, spec, _ := resourceOwnerFixture(t)
			ctx := context.Background()
			spec.Kind = "upload"
			reserved, err := repo.Reserve(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			original, err := repo.Bind(ctx, reserved.ID, spec.UserID, " upload-replayed ")
			if err != nil {
				t.Fatal(err)
			}
			if deleted {
				if err := repo.ObserveDelete(ctx, spec.Kind, *original.UpstreamID, spec.UserID); err != nil {
					t.Fatal(err)
				}
			}
			original, err = repo.Get(ctx, spec.Kind, *original.UpstreamID, spec.UserID)
			if err != nil {
				t.Fatal(err)
			}
			spec.TokenID++
			replay, err := repo.Reserve(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			got, err := repo.Bind(ctx, replay.ID, spec.UserID, *original.UpstreamID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != original.ID || got.TokenID != original.TokenID || !got.CreatedAt.Equal(original.CreatedAt) || !got.UpdatedAt.Equal(original.UpdatedAt) || !got.RetainUntil.Equal(*original.RetainUntil) || (got.DeleteObservedAt != nil) != deleted {
				t.Fatalf("replay changed durable owner: before=%+v after=%+v", original, got)
			}
			if deleted && !got.DeleteObservedAt.Equal(*original.DeleteObservedAt) {
				t.Fatal("replay changed delete observation")
			}
			var count int64
			if err := repo.DB.Model(&ResourceOwner{}).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("replay leaked reservation: count=%d err=%v", count, err)
			}
			if err := repo.Release(ctx, replay.ID, spec.UserID); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.Get(ctx, spec.Kind, *original.UpstreamID, spec.UserID); err != nil {
				t.Fatalf("request cleanup removed reused owner: %v", err)
			}
		})
	}
}

func TestResourceOwnerReplayRejectsDifferentBindings(t *testing.T) {
	for _, field := range []string{"user", "channel", "namespace", "scope", "parent", "task", "expired"} {
		t.Run(field, func(t *testing.T) {
			repo, spec, _ := resourceOwnerFixture(t)
			ctx := context.Background()
			reserved, err := repo.Reserve(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			original, err := repo.Bind(ctx, reserved.ID, spec.UserID, "file-replayed")
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "user":
				spec.UserID++
			case "channel":
				spec.ChannelID++
			case "namespace":
				spec.ProviderNamespace = "another-provider"
			case "scope":
				spec.ProviderScope = "another-scope"
			case "parent":
				spec.ParentID = &original.ID
			case "task":
				spec.TaskOwnerID, spec.Slot = "another-task", "output"
			case "expired":
				if err := repo.DB.Model(original).Update("retain_until", time.Now().Add(-time.Minute)).Error; err != nil {
					t.Fatal(err)
				}
			}
			replay, err := repo.Reserve(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repo.Bind(ctx, replay.ID, spec.UserID, "file-replayed"); !errors.Is(err, ErrResourceOwnerConflict) {
				t.Fatalf("different %s binding accepted: %v", field, err)
			}
			var pending ResourceOwner
			if err := repo.DB.First(&pending, replay.ID).Error; err != nil || pending.Phase != ResourceOwnerReserved {
				t.Fatalf("conflict partially changed reservation: %+v %v", pending, err)
			}
		})
	}
}

func TestResourceOwnerConcurrentReplayUsesSingleOwner(t *testing.T) {
	repo, spec, _ := resourceOwnerFixture(t)
	ctx := context.Background()
	reservations, err := repo.ReserveMany(ctx, []ResourceOwnerReservation{spec, spec})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	owners := make([]*ResourceOwner, len(reservations))
	for i, reservation := range reservations {
		wg.Add(1)
		go func(i int, id uint64) {
			defer wg.Done()
			owner, err := repo.Bind(ctx, id, spec.UserID, "file-concurrent")
			if err != nil {
				t.Error(err)
				return
			}
			owners[i] = owner
		}(i, reservation.ID)
	}
	wg.Wait()
	if owners[0] == nil || owners[1] == nil || owners[0].ID != owners[1].ID {
		t.Fatalf("concurrent replay did not converge: %+v", owners)
	}
	var count int64
	if err := repo.DB.Model(&ResourceOwner{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("concurrent replay leaked reservation: count=%d err=%v", count, err)
	}
}

func TestResourceOwnerReplayRejectsOpaqueIDCollationAliases(t *testing.T) {
	for _, test := range []struct{ collation, alias string }{{"RTRIM", "file-a "}, {"NOCASE", "FILE-A"}} {
		t.Run(test.collation, func(t *testing.T) {
			repo, spec, _ := resourceOwnerFixture(t)
			var schema string
			if err := repo.DB.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'resource_owners'").Scan(&schema).Error; err != nil {
				t.Fatal(err)
			}
			collated := strings.Replace(schema, "`upstream_id` text", "`upstream_id` text COLLATE "+test.collation, 1)
			if collated == schema {
				t.Fatalf("cannot apply test collation: %s", schema)
			}
			if err := repo.DB.Migrator().DropTable(&ResourceOwner{}); err != nil {
				t.Fatal(err)
			}
			if err := repo.DB.Exec(collated).Error; err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			original, err := repo.Reserve(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repo.Bind(ctx, original.ID, spec.UserID, "file-a"); err != nil {
				t.Fatal(err)
			}
			replay, err := repo.Reserve(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repo.Bind(ctx, replay.ID, spec.UserID, test.alias); !errors.Is(err, ErrResourceOwnerConflict) {
				t.Fatalf("collation alias reused opaque identity: %v", err)
			}
		})
	}
}
