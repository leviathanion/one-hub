package model

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"one-api/common/config"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

func TestFixI018_OptionUpgradeAndInheritAcrossDatabases(t *testing.T) {
	for _, scenario := range []string{"empty_options", "legacy_migration_pending", "legacy_migration_completed"} {
		t.Run(scenario, func(t *testing.T) {
			forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
				setupI018Options(t, db)
				rows := map[string]string{}
				want := map[string]string{}
				wantVersion := int64(1)
				if scenario != "empty_options" {
					rows["QuotaRemindThreshold"] = "1000"
					rows["PasswordRegisterEnabled"] = "false"
					want["PasswordRegisterEnabled"] = "false"
					wantVersion++
				}
				if scenario == "legacy_migration_pending" {
					rows["ChatImageRequestProxy"] = "http://127.0.0.1/test-proxy"
					rows["CFWorkerImageUrl"] = "http://127.0.0.1/test-worker"
					rows["CFWorkerImageKey"] = "onehub-test-key"
					wantVersion++
				}
				for key, value := range rows {
					if err := db.Create(&Option{Key: key, Value: value}).Error; err != nil {
						t.Fatal(err)
					}
				}
				// 其余历史迁移已有独立覆盖；这里通过真实迁移记录选择两项 options 清理。
				if err := db.Exec("CREATE TABLE migrations (id varchar(255) PRIMARY KEY)").Error; err != nil {
					t.Fatal(err)
				}
				for _, migration := range afterAutoMigrateMigrations() {
					if migration.ID < "202608310001" || (migration.ID == "202608310001" && scenario == "legacy_migration_completed") {
						if err := db.Exec("INSERT INTO migrations (id) VALUES (?)", migration.ID).Error; err != nil {
							t.Fatal(err)
						}
					}
				}
				for restart := 0; restart < 2; restart++ {
					if err := gormigrate.New(db, gormigrate.DefaultOptions, afterAutoMigrateMigrations()).Migrate(); err != nil {
						t.Fatal(err)
					}
					assertI018RowsAndVersion(t, db, want, wantVersion)
				}
				InitOptionMap()
				if err := CheckOptionsPublication(context.Background()); err != nil {
					t.Fatalf("升级后配置不可发布: %v", err)
				}
				defaultName, _ := config.GlobalOption.RuntimeSnapshot().Get("SystemName")
				for _, value := range []string{"I-018 保存的名称", ""} {
					if err := UpdateOption("SystemName", value); err != nil {
						t.Fatalf("保存配置失败: %v", err)
					}
					want["SystemName"] = value
					wantVersion++
					assertI018RowsAndVersion(t, db, want, wantVersion)
					stored, err := GetOption("SystemName")
					if err != nil || stored.Value != value {
						t.Fatalf("保存的配置不可读取: %+v, %v", stored, err)
					}
					snapshot := config.GlobalOption.RuntimeSnapshot()
					published, _ := snapshot.Get("SystemName")
					if snapshot.Version() != wantVersion || published.Source != config.RuntimeOptionSourceOverride || published.Override == nil || *published.Override != value || published.Effective != value {
						t.Fatalf("保存值未发布: version=%d value=%+v", snapshot.Version(), published)
					}
				}
				version, err := ApplyOptionMutations(context.Background(), wantVersion, []OptionMutation{{Key: "SystemName", Inherit: true}})
				if err != nil {
					t.Fatalf("继承默认值失败: %v", err)
				}
				wantVersion++
				if version != wantVersion {
					t.Fatalf("继承发布版本 = %d, want %d", version, wantVersion)
				}
				delete(want, "SystemName")
				assertI018RowsAndVersion(t, db, want, wantVersion)
				published, _ := config.GlobalOption.RuntimeSnapshot().Get("SystemName")
				if config.GlobalOption.RuntimeSnapshot().Version() != wantVersion || published.Source != config.RuntimeOptionSourceDefault || published.Override != nil || published.Effective != defaultName.Effective {
					t.Fatalf("继承默认值未发布: %+v", published)
				}
				// 重建进程内配置，确认 inherit 在重启后仍使用登记默认值。
				config.GlobalOption = config.NewOptionManager()
				InitOptionMap()
				if err := CheckOptionsPublication(context.Background()); err != nil {
					t.Fatal(err)
				}
				got, _ := config.GlobalOption.RuntimeSnapshot().Get("SystemName")
				if got.Source != config.RuntimeOptionSourceDefault || got.Override != nil || got.Effective != defaultName.Effective {
					t.Fatalf("重启后默认值不一致: %+v, want %+v", got, defaultName)
				}
				if version, err := ApplyOptionMutations(context.Background(), wantVersion, []OptionMutation{{Key: "SystemName", Inherit: true}}); err != nil || version != wantVersion {
					t.Fatalf("重复 inherit 应保持版本: version=%d err=%v", version, err)
				}
				assertI018RowsAndVersion(t, db, want, wantVersion)
			})
		})
	}
}

func TestFixI018_DeleteAndPublicationVersionAreAtomic(t *testing.T) {
	for _, operation := range []string{"migration", "inherit"} {
		for _, failAt := range []string{"after_delete", "after_version_update"} {
			t.Run(operation+"/"+failAt, func(t *testing.T) {
				forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
					setupI018Options(t, db)
					key := "QuotaRemindThreshold"
					if operation == "inherit" {
						key = "SystemName"
					}
					before := map[string]string{key: "42", "PasswordRegisterEnabled": "false"}
					for key, value := range before {
						if err := db.Create(&Option{Key: key, Value: value}).Error; err != nil {
							t.Fatal(err)
						}
					}
					if operation == "inherit" {
						InitOptionMap()
						if err := CheckOptionsPublication(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					injected := errors.New("I-018 测试事务错误")
					callback := func(tx *gorm.DB) {
						if tx.Error == nil && tx.RowsAffected > 0 {
							tx.AddError(injected)
						}
					}
					const callbackName = "test:i018:transaction_failure"
					if failAt == "after_delete" {
						if err := db.Callback().Delete().After("gorm:delete").Register(callbackName, callback); err != nil {
							t.Fatal(err)
						}
					} else if err := db.Callback().Update().After("gorm:update").Register(callbackName, callback); err != nil {
						t.Fatal(err)
					}
					var err error
					if operation == "migration" {
						err = removeLegacyQuotaRemindOption().Migrate(db)
					} else {
						value := "true"
						_, err = ApplyOptionMutations(context.Background(), 1, []OptionMutation{
							{Key: "PasswordRegisterEnabled", Value: &value},
							{Key: "SystemName", Inherit: true},
						})
					}
					if !errors.Is(err, injected) {
						t.Fatalf("预期事务错误，得到 %v", err)
					}
					assertI018RowsAndVersion(t, db, before, 1)
					if operation == "inherit" {
						snapshot := config.GlobalOption.RuntimeSnapshot()
						if snapshot.Version() != 1 || snapshot.EffectiveValues()["SystemName"] != "42" || snapshot.EffectiveValues()["PasswordRegisterEnabled"] != "false" {
							t.Fatalf("失败事务影响了配置快照: version=%d", snapshot.Version())
						}
					}
				})
			})
		}
	}
}

func setupI018Options(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	oldDB, oldOptions := DB, config.GlobalOption
	DB, config.GlobalOption = db, config.NewOptionManager()
	t.Cleanup(func() {
		DB, config.GlobalOption = oldDB, oldOptions
		setOptionsPublicationError(nil)
	})
}

func assertI018RowsAndVersion(t *testing.T, db *gorm.DB, want map[string]string, wantVersion int64) {
	t.Helper()
	var rows []Option
	if err := db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, row := range rows {
		got[row.Key] = row.Value
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("数据库配置 = %#v, want %#v", got, want)
	}
	for _, owner := range []string{PublicationOwnerOptions, PublicationOwnerPrice, PublicationOwnerUserGroup} {
		version, err := ReadPublicationVersion(context.Background(), db, owner)
		if err != nil {
			t.Fatal(err)
		}
		expected := int64(1)
		if owner == PublicationOwnerOptions {
			expected = wantVersion
		}
		if version != expected {
			t.Fatalf("%s 版本 = %d, want %d", owner, version, expected)
		}
	}
}
