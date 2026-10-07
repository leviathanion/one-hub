package model

import (
	"one-api/common"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestModelOwnershipStatisticsPreserveUncategorizedUsage(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		useModelCatalogTestDB(t, db)
		oldSQLite, oldPostgres := common.UsingSQLite, common.UsingPostgreSQL
		common.UsingSQLite = db.Dialector.Name() == "sqlite"
		common.UsingPostgreSQL = db.Dialector.Name() == "postgres"
		t.Cleanup(func() { common.UsingSQLite, common.UsingPostgreSQL = oldSQLite, oldPostgres })
		if err := db.AutoMigrate(&Statistics{}); err != nil {
			t.Fatal(err)
		}
		owner, unknownOwner := 1001, 1002
		for _, item := range []ModelOwnedBy{{Id: owner, Name: "模型分类"}, {Id: unknownOwner, Name: UnknownOwnedBy}} {
			if err := CreateModelOwnedBy(&item); err != nil {
				t.Fatal(err)
			}
		}
		for _, info := range []ModelInfo{
			{Model: "known-one", OwnedByID: &owner},
			{Model: "known-two", OwnedByID: &owner},
			{Model: "no-owner"},
			{Model: "unknown-label", OwnedByID: &unknownOwner},
		} {
			if err := CreateModelInfo(&info); err != nil {
				t.Fatal(err)
			}
		}
		day := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
		for i, name := range []string{"known-one", "known-two", "no-owner", "no-directory", "unknown-label"} {
			row := Statistics{Date: day, UserId: 1, ChannelId: 1, ModelName: name, Quota: (i + 1) * 10, RequestCount: 1, CacheReadTokens: 2}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
		}
		for _, row := range []Statistics{
			{Date: day, UserId: 2, ChannelId: 1, ModelName: "known-one", Quota: 900, RequestCount: 1},
			{Date: day.AddDate(0, 0, -1), UserId: 1, ChannelId: 1, ModelName: "known-one", Quota: 900, RequestCount: 1},
		} {
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
		}
		rows, err := GetChannelExpensesStatisticsByPeriod("2026-10-07", "2026-10-08", "model_type", 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("未知分类应合并且不丢数据: %+v", rows)
		}
		var totalQuota, totalRequests, cacheReadTokens int64
		for _, row := range rows {
			totalQuota += row.Quota
			totalRequests += row.RequestCount
			cacheReadTokens += row.CacheReadTokens
			switch row.Channel {
			case "模型分类":
				if row.Quota != 30 || row.RequestCount != 2 {
					t.Fatalf("分类聚合不正确: %+v", row)
				}
			case UnknownOwnedBy:
				if row.Quota != 120 || row.RequestCount != 3 {
					t.Fatalf("缺目录/缺归属用量不应丢失: %+v", row)
				}
			default:
				t.Fatalf("未知结果分类: %q", row.Channel)
			}
		}
		if totalQuota != 150 || totalRequests != 5 || cacheReadTokens != 10 {
			t.Fatalf("统计合计或用户/日期过滤错误: quota=%d requests=%d cache=%d", totalQuota, totalRequests, cacheReadTokens)
		}
	})
}
