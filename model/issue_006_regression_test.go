package model

import (
	"math"
	"testing"

	"gorm.io/gorm"
)

func TestIssue006GroupCreateAndUpdatePreserveRatioAcrossDatabases(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		if err := db.AutoMigrate(&UserGroup{}, &PublicationVersion{}); err != nil {
			t.Fatal(err)
		}
		if err := EnsurePublicationVersionRows(db); err != nil {
			t.Fatal(err)
		}
		oldDB, oldGroups := DB, GlobalUserGroupRatio
		DB, GlobalUserGroupRatio = db, &UserGroupRatio{}
		t.Cleanup(func() {
			stopUserGroupAPILimiters(GlobalUserGroupRatio.APILimiter)
			DB, GlobalUserGroupRatio = oldDB, oldGroups
		})
		group := &UserGroup{Symbol: "free", Name: "免费组", Ratio: 0, Public: true}
		if err := group.Create(); err != nil {
			t.Fatal(err)
		}
		check := func(want float64) {
			t.Helper()
			var stored UserGroup
			if err := db.First(&stored, group.Id).Error; err != nil {
				t.Fatal(err)
			}
			published := GlobalUserGroupRatio.GetBySymbol(group.Symbol)
			if group.Id == 0 || stored.Ratio != want || published == nil || published.Ratio != want {
				t.Fatalf("分组持久化或发布错误: id=%d stored=%v published=%+v want=%v", group.Id, stored.Ratio, published, want)
			}
			if !stored.Public || stored.Enable == nil || !*stored.Enable || stored.APIRate != 600 {
				t.Fatalf("创建丢失其他字段或默认值: %+v", stored)
			}
		}
		check(0)
		for _, ratio := range []float64{2.5, 0} {
			group.Ratio = ratio
			if err := group.Update(); err != nil {
				t.Fatal(err)
			}
			check(ratio)
		}
	})
}

func TestIssue006InvalidGroupRatioDoesNotPublish(t *testing.T) {
	db := setupUserGroupPublicationTest(t)
	group := createPublicationTestGroup(t, "valid", 600)
	version := GlobalUserGroupRatio.PublicationStatus().PublishedVersion
	for _, ratio := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		invalid := &UserGroup{Symbol: "invalid", Ratio: ratio}
		if err := invalid.Create(); err == nil {
			t.Fatalf("创建接受了非法倍率 %v", ratio)
		}
		group.Ratio = ratio
		if err := group.Update(); err == nil {
			t.Fatalf("更新接受了非法倍率 %v", ratio)
		}
	}
	var count int64
	if err := db.Model(&UserGroup{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	var stored UserGroup
	if err := db.First(&stored, group.Id).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 || stored.Ratio != 1 || GlobalUserGroupRatio.PublicationStatus().PublishedVersion != version {
		t.Fatal("非法倍率改变了分组或发布版本")
	}
}
