package model

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/credentials"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestChannelBusinessDataOfflineMigration(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		type oldChannel struct {
			ID                         int `gorm:"primaryKey"`
			Key                        string
			CredentialRevision         uint64
			CredentialRefreshFence     *string
			CredentialRefreshStartedAt *int64
			DeletedAt                  gorm.DeletedAt
		}
		if err := db.Table("channels").AutoMigrate(&oldChannel{}); err != nil {
			t.Fatal(err)
		}
		fence, started := "Unresolved-Attempt", time.Now().Add(-time.Hour).Unix()
		rows := []oldChannel{{ID: 1, Key: "old", CredentialRevision: 7, CredentialRefreshFence: &fence, CredentialRefreshStartedAt: &started}, {ID: 2, Key: "deleted", CredentialRevision: 9, CredentialRefreshFence: &fence, DeletedAt: gorm.DeletedAt{Time: time.Now(), Valid: true}}, {ID: 3, Key: "static"}}
		if err := db.Table("channels").Create(&rows).Error; err != nil {
			t.Fatal(err)
		}
		migration := migrateChannelBusinessData()
		if err := migration.Migrate(db); err != nil {
			t.Fatal(err)
		}
		if err := migration.Migrate(db); err != nil {
			t.Fatalf("restart: %v", err)
		}
		names, err := databaseColumnNames(db, "channels")
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"credential_revision", "credential_refresh_fence", "credential_refresh_started_at"} {
			if names[name] {
				t.Fatalf("old column retained: %s", name)
			}
		}
		var migrated []Channel
		if err := db.Unscoped().Order("id").Find(&migrated).Error; err != nil {
			t.Fatal(err)
		}
		for i, row := range migrated {
			if row.Key != rows[i].Key || row.Version != rows[i].CredentialRevision || row.DeletedAt.Valid != rows[i].DeletedAt.Valid {
				t.Fatal("migration changed credential or lifecycle")
			}
			refresh, err := credentials.ReadRefresh(row.BizData)
			if err != nil {
				t.Fatal(err)
			}
			if i < 2 && (refresh == nil || refresh.AttemptID != fence) {
				t.Fatal("unresolved operation lost")
			}
			if i == 0 && refresh.StartedAt != started {
				t.Fatal("timestamp lost")
			}
			if i == 2 && refresh != nil {
				t.Fatal("static key got refresh state")
			}
		}
	})
}

func TestChannelEditVersionProtectsSnapshotAndExcludesTelemetry(t *testing.T) {
	useTestChannelDB(t)
	row := Channel{Id: 34001, Type: config.ChannelTypeOpenAI, Key: "secret", Models: "old", BizData: []byte(`{"vendor":{"kept":true}}`)}
	if err := DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	edit := func(raw string) error {
		var request ChannelEditRequest
		if err := json.Unmarshal([]byte(raw), &request); err != nil {
			return err
		}
		return request.Update()
	}
	if err := edit(`{"id":34001,"name":"first","expected_version":0}`); err != nil {
		t.Fatal(err)
	}
	if err := edit(`{"id":34001,"name":"stale","expected_version":0}`); !errors.Is(err, ErrChannelVersionConflict) {
		t.Fatal("stale edit accepted", err)
	}
	if err := edit(`{"id":34001,"name":"no-version"}`); err == nil {
		t.Fatal("missing version accepted")
	}
	if err := edit(`{"id":34001,"expected_version":1,"bizdata":{}}`); err == nil {
		t.Fatal("internal state injection accepted")
	}
	if err := UpdateChannelUsedQuotaWithContext(t.Context(), row.Id, 17); err != nil {
		t.Fatal(err)
	}
	if err := edit(`{"id":34001,"name":"second","expected_version":1}`); err != nil {
		t.Fatal(err)
	}
	saved, _ := GetChannelById(row.Id)
	if saved.Version != 2 || saved.UsedQuota != 17 || string(saved.BizData) != string(row.BizData) {
		t.Fatal("edit mixed telemetry/internal state")
	}
	encoded, _ := json.Marshal(saved)
	var public map[string]any
	_ = json.Unmarshal(encoded, &public)
	if _, ok := public["bizdata"]; ok {
		t.Fatal("internal state exposed")
	}
}

func TestTagEditVersionsAreAtomicAndPreserveInternalState(t *testing.T) {
	useTestChannelDB(t)
	for _, id := range []int{34002, 34003} {
		if err := DB.Create(&Channel{Id: id, Type: config.ChannelTypeOpenAI, Key: "key", Tag: "versions", Models: "old", BizData: []byte(`{"vendor":{}}`)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	fields := ChannelTagSubmittedFields{"models": {}}
	update := Channel{Models: "new"}
	if err := UpdateChannelsTagWithSubmittedFields("versions", &update, fields, ChannelUpdateOptions{ExpectedVersions: map[int]uint64{34002: 0, 34003: 1}}); !errors.Is(err, ErrChannelVersionConflict) {
		t.Fatal(err)
	}
	rows, _ := GetChannelsByTag("versions")
	for _, row := range rows {
		if row.Models != "old" || row.Version != 0 {
			t.Fatal("partial update")
		}
	}
	if err := UpdateChannelsTagWithSubmittedFields("versions", &update, fields, ChannelUpdateOptions{ExpectedVersions: map[int]uint64{34002: 0, 34003: 0}}); err != nil {
		t.Fatal(err)
	}
	rows, _ = GetChannelsByTag("versions")
	for _, row := range rows {
		if row.Models != "new" || row.Version != 1 || string(row.BizData) != `{"vendor":{}}` {
			t.Fatal("invalid batch update")
		}
	}
}

func TestChannelBusinessDataMigrationResumesInterruptedDDL(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		type partialChannel struct {
			ID                     int            `gorm:"primaryKey"`
			Version                uint64         `gorm:"not null;default:0"`
			BizData                datatypes.JSON `gorm:"column:bizdata;type:json"`
			CredentialRevision     uint64
			CredentialRefreshFence *string
		}
		if err := db.Table("channels").AutoMigrate(&partialChannel{}); err != nil {
			t.Fatal(err)
		}
		attempt := "persisted-attempt"
		row := partialChannel{ID: 1, Version: 8, CredentialRevision: 7, CredentialRefreshFence: &attempt, BizData: datatypes.JSON(`{"vendor":{"preserved":true},"credentials":{"refresh":{"attempt_id":"persisted-attempt","started_at":123}}}`)}
		if err := db.Table("channels").Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		// The backfill committed and the timestamp column was dropped before restart.
		if err := migrateChannelBusinessData().Migrate(db); err != nil {
			t.Fatal(err)
		}
		var after Channel
		if err := db.Unscoped().Table("channels").Select("id", "version", "bizdata").First(&after).Error; err != nil {
			t.Fatal(err)
		}
		refresh, err := credentials.ReadRefresh(after.BizData)
		if err != nil || refresh == nil || refresh.StartedAt != 123 || after.Version != 8 {
			t.Fatal("restart lost committed state")
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(after.BizData, &data); err != nil || string(data["vendor"]) != `{"preserved": true}` && string(data["vendor"]) != `{"preserved":true}` {
			t.Fatal("restart lost namespace")
		}
	})
}

func TestChannelBusinessDataMigrationFailsWithoutDroppingConflictingState(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		type conflictingChannel struct {
			ID                     int            `gorm:"primaryKey"`
			Version                uint64         `gorm:"not null;default:0"`
			BizData                datatypes.JSON `gorm:"column:bizdata;type:json"`
			CredentialRefreshFence string
		}
		if err := db.Table("channels").AutoMigrate(&conflictingChannel{}); err != nil {
			t.Fatal(err)
		}
		row := conflictingChannel{ID: 1, CredentialRefreshFence: "old-owner", BizData: datatypes.JSON(`{"credentials":{"refresh":{"attempt_id":"different-owner"}}}`)}
		if err := db.Table("channels").Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		if err := migrateChannelBusinessData().Migrate(db); err == nil {
			t.Fatal("conflicting operation silently replaced")
		}
		names, err := databaseColumnNames(db, "channels")
		if err != nil || !names["credential_refresh_fence"] {
			t.Fatal("old state dropped on failed backfill")
		}
	})
}

func TestChannelEditRetainsCommittedSnapshotWhenReloadFails(t *testing.T) {
	useTestChannelDB(t)
	row := Channel{Id: 34004, Type: config.ChannelTypeOpenAI, Key: "secret", Name: "before", Models: "gpt-5", Other: "legacy-value", Version: 7}
	insertTestChannel(t, &row)
	var request ChannelEditRequest
	if err := json.Unmarshal([]byte(`{"id":34004,"name":"after","expected_version":7,"version":999}`), &request); err != nil {
		t.Fatal(err)
	}
	failChannelQueriesAfter(t, errors.New("post-commit read unavailable"), 1, 100)
	if err := request.Update(); err != nil {
		t.Fatalf("committed edit reported as failure: %v", err)
	}
	if request.Version != 8 || request.Name != "after" || request.Models != row.Models || request.Key != row.Key || request.Other != row.Other {
		t.Fatal("successful response lost committed snapshot or echoed client-owned version")
	}
}
