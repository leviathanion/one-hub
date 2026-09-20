package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestResourceOwnerBindRetainsExactOpaqueID(t *testing.T) {
	repo, spec, _ := resourceOwnerFixture(t)
	reserved, err := repo.Reserve(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := repo.Bind(context.Background(), reserved.ID, spec.UserID, " file-a ")
	if err != nil || owner.UpstreamID == nil || *owner.UpstreamID != " file-a " {
		t.Fatalf("binding normalized ID: %+v %v", owner, err)
	}
	if _, err := repo.Get(context.Background(), "file", "file-a", spec.UserID); !errors.Is(err, ErrResourceOwnerNotFound) {
		t.Fatalf("different ID acquired ownership: %v", err)
	}
}

func TestResourceOwnerGetRejectsDatabaseCollationAliases(t *testing.T) {
	for _, test := range []struct{ collation, alias string }{{"RTRIM", "file-a "}, {"NOCASE", "FILE-A"}} {
		t.Run(test.collation, func(t *testing.T) {
			repo, _, _ := resourceOwnerFixture(t)
			if err := repo.DB.Migrator().DropTable(&ResourceOwner{}); err != nil {
				t.Fatal(err)
			}
			query := fmt.Sprintf("CREATE TABLE resource_owners (id INTEGER PRIMARY KEY, user_id INTEGER, kind TEXT, upstream_id TEXT COLLATE %s, phase TEXT, retain_until DATETIME)", test.collation)
			if err := repo.DB.Exec(query).Error; err != nil {
				t.Fatal(err)
			}
			if err := repo.DB.Exec("INSERT INTO resource_owners(id,user_id,kind,upstream_id,phase) VALUES(1,1,'file','file-a','bound')").Error; err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := repo.DB.Model(&ResourceOwner{}).Where("upstream_id = ?", test.alias).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("fixture did not reproduce collation alias: %d %v", count, err)
			}
			if _, err := repo.Get(context.Background(), "file", test.alias, 1); !errors.Is(err, ErrResourceOwnerNotFound) {
				t.Fatalf("database collation authorized a different wire ID: %v", err)
			}
			if _, err := repo.Get(context.Background(), "file", "file-a", 1); err != nil {
				t.Fatal(err)
			}
		})
	}
}
