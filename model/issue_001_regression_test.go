package model

import (
	"context"
	"testing"

	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestFixI001_UpgradePreservesPublishedOptions(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		t.Run(map[bool]string{true: "old_quota_override", false: "no_legacy_override"}[legacy], func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
				t.Fatal(err)
			}
			if err := EnsurePublicationVersionRows(db); err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&Option{Key: "PasswordRegisterEnabled", Value: "false"}).Error; err != nil {
				t.Fatal(err)
			}
			if legacy {
				if err := db.Create(&Option{Key: "QuotaRemindThreshold", Value: "1000"}).Error; err != nil {
					t.Fatal(err)
				}
			}
			// 模拟旧迁移已完成；新增清理必须有独立的迁移编号。
			if err := db.Exec("CREATE TABLE migrations (id varchar(255) PRIMARY KEY)").Error; err != nil {
				t.Fatal(err)
			}
			for _, migration := range afterAutoMigrateMigrations() {
				if migration.ID <= "202608310001" {
					if err := db.Exec("INSERT INTO migrations (id) VALUES (?)", migration.ID).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			oldDB, oldOptions := DB, config.GlobalOption
			DB = db
			t.Cleanup(func() { DB = oldDB; config.GlobalOption = oldOptions; setOptionsPublicationError(nil) })
			for restart := 0; restart < 2; restart++ {
				if err := gormigrate.New(db, gormigrate.DefaultOptions, afterAutoMigrateMigrations()).Migrate(); err != nil {
					t.Fatal(err)
				}
				config.GlobalOption = config.NewOptionManager()
				InitOptionMap()
				if err := CheckOptionsPublication(context.Background()); err != nil {
					t.Fatalf("restart %d: %v", restart, err)
				}
				if got := config.GlobalOption.RuntimeSnapshot().EffectiveValues()["PasswordRegisterEnabled"]; got != "false" {
					t.Fatalf("saved registration switch = %s", got)
				}
				version, err := ReadPublicationVersion(context.Background(), db, PublicationOwnerOptions)
				if err != nil {
					t.Fatal(err)
				}
				want := int64(1 + restart)
				if legacy {
					want++
				}
				if version != want {
					t.Fatalf("version = %d, want %d", version, want)
				}
				if restart == 0 {
					name := "修复后保存"
					if _, err := ApplyOptionMutations(context.Background(), version, []OptionMutation{{Key: "SystemName", Value: &name}}); err != nil {
						t.Fatal(err)
					}
				} else if got := config.GlobalOption.RuntimeSnapshot().EffectiveValues()["SystemName"]; got != "修复后保存" {
					t.Fatalf("saved system name = %s", got)
				}
			}
			if err := db.Create(&Option{Key: "ReallyUnknownSetting", Value: "x"}).Error; err != nil {
				t.Fatal(err)
			}
			if err := LoadAndPublishOptions(context.Background()); err == nil {
				t.Fatal("unknown option must still reject publication")
			}
			if err := CheckOptionsPublication(context.Background()); err == nil {
				t.Fatal("invalid options must remain unready")
			}
		})
	}
}
