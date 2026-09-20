package model

import (
	"one-api/common/config"
	"testing"
)

func TestTokenUserEditPreservesLatestResourceChannel(t *testing.T) {
	useTokenSettlementTestDB(t)
	insertTokenSettlementFixtures(t)
	oldEnabled := config.RedisEnabled
	config.RedisEnabled = false
	t.Cleanup(func() { config.RedisEnabled = oldEnabled })
	snapshot, err := GetTokenById(1)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := GetTokenById(1)
	if err != nil {
		t.Fatal(err)
	}
	setting := admin.Setting.Data()
	setting.ResourceChannelID = 7
	admin.Setting.Set(setting)
	quota := admin.RemainQuota
	if err := admin.UpdateMutableFields(&quota); err != nil {
		t.Fatal(err)
	}
	snapshot.Name = "user edited after admin"
	if err := snapshot.UpdateMutableFieldsPreservingResourceChannel(&quota); err != nil {
		t.Fatal(err)
	}
	persisted, err := GetTokenById(1)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Setting.Data().ResourceChannelID != 7 || persisted.Name != snapshot.Name {
		t.Fatalf("stale user settings overwrote the admin route: %+v", persisted)
	}
}
