package model

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"

	"github.com/go-gormigrate/gormigrate/v2"
	"go.uber.org/zap"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useDashboardCacheTokenTestDB(t *testing.T) {
	t.Helper()

	logger.Logger = zap.NewNop()

	originalDB := DB
	originalUsingSQLite := common.UsingSQLite
	originalUsingPostgreSQL := common.UsingPostgreSQL

	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Log{}, &Statistics{}); err != nil {
		t.Fatalf("expected cache token schema migration to succeed, got %v", err)
	}

	DB = testDB
	common.UsingSQLite = true
	common.UsingPostgreSQL = false

	t.Cleanup(func() {
		DB = originalDB
		common.UsingSQLite = originalUsingSQLite
		common.UsingPostgreSQL = originalUsingPostgreSQL
	})
}

func useDashboardMigrationTestDB(t *testing.T) {
	t.Helper()

	useDashboardCacheTokenTestDB(t)

	if err := DB.AutoMigrate(&Channel{}, &Token{}, &Option{}, &Price{}, &UserGroup{}); err != nil {
		t.Fatalf("expected migration schema to succeed, got %v", err)
	}
}

func TestBuildUserDashboardSeparatesCacheTokens(t *testing.T) {
	dashboard := buildUserDashboard([]*LogStatisticGroupModel{
		{
			LogStatistic: LogStatistic{
				Date:             "2026-04-06",
				RequestCount:     2,
				PromptTokens:     120,
				CompletionTokens: 30,
				CacheTokens:      0,
				CacheReadTokens:  40,
				CacheWriteTokens: 10,
				CacheHitCount:    1,
			},
			ModelName: "gpt-4.1",
		},
		{
			LogStatistic: LogStatistic{
				Date:             "2026-04-06",
				RequestCount:     1,
				PromptTokens:     5,
				CompletionTokens: 2,
				CacheTokens:      3,
				CacheReadTokens:  0,
				CacheWriteTokens: 1,
				CacheHitCount:    1,
			},
			ModelName: "gpt-4o",
		},
		{
			LogStatistic: LogStatistic{
				Date:             "2026-04-05",
				RequestCount:     9,
				PromptTokens:     999,
				CompletionTokens: 888,
				CacheTokens:      555,
				CacheReadTokens:  777,
				CacheWriteTokens: 666,
				CacheHitCount:    9,
			},
			ModelName: "ignored-model",
		},
	}, "2026-04-06")

	if dashboard.TodayTokenBreakdown.RequestCount != 3 {
		t.Fatalf("expected today request count to be 3, got %d", dashboard.TodayTokenBreakdown.RequestCount)
	}
	if dashboard.TodayTokenBreakdown.InputTokens != 71 {
		t.Fatalf("expected today input tokens to be 71, got %d", dashboard.TodayTokenBreakdown.InputTokens)
	}
	if dashboard.TodayTokenBreakdown.OutputTokens != 32 {
		t.Fatalf("expected today output tokens to be 32, got %d", dashboard.TodayTokenBreakdown.OutputTokens)
	}
	if dashboard.TodayTokenBreakdown.CacheTokens != 3 {
		t.Fatalf("expected today cache tokens to be 3, got %d", dashboard.TodayTokenBreakdown.CacheTokens)
	}
	if dashboard.TodayTokenBreakdown.CacheReadTokens != 40 {
		t.Fatalf("expected today cache read tokens to be 40, got %d", dashboard.TodayTokenBreakdown.CacheReadTokens)
	}
	if dashboard.TodayTokenBreakdown.CacheWriteTokens != 11 {
		t.Fatalf("expected today cache write tokens to be 11, got %d", dashboard.TodayTokenBreakdown.CacheWriteTokens)
	}
	if dashboard.TodayTokenBreakdown.TotalTokens != 157 {
		t.Fatalf("expected today total tokens to be 157, got %d", dashboard.TodayTokenBreakdown.TotalTokens)
	}

	if math.Abs(dashboard.TodayCacheHitRate.HitRate-(2.0/3.0)) > 1e-9 {
		t.Fatalf("expected overall cache hit rate to be 2/3, got %.12f", dashboard.TodayCacheHitRate.HitRate)
	}
	if len(dashboard.TodayCacheHitRate.Models) != 2 {
		t.Fatalf("expected 2 model cache hit rate rows, got %d", len(dashboard.TodayCacheHitRate.Models))
	}
	if dashboard.TodayCacheHitRate.Models[0].ModelName != "gpt-4.1" {
		t.Fatalf("expected model cache hit rate rows to be sorted by model name, got %+v", dashboard.TodayCacheHitRate.Models)
	}
	if dashboard.TodayCacheHitRate.Models[1].RequestCount != 1 || dashboard.TodayCacheHitRate.Models[1].CacheHitCount != 1 {
		t.Fatalf("expected per-model cache hit stats to be aggregated, got %+v", dashboard.TodayCacheHitRate.Models[1])
	}
}

func TestGetUserDashboardCacheOverviewAppliesFiltersAndKeepsChannelNamesPrivate(t *testing.T) {
	useDashboardMigrationTestDB(t)

	if err := DB.Create([]*Channel{
		{Id: 1, Name: "Alpha"},
		{Id: 2, Name: "Beta"},
		{Id: 3, Name: "Gamma"},
	}).Error; err != nil {
		t.Fatalf("expected channel fixtures to persist, got %v", err)
	}

	insertSQL := `
		INSERT INTO statistics
		(date, user_id, channel_id, model_name, request_count, quota, prompt_tokens, completion_tokens, cache_tokens, cache_read_tokens, cache_write_tokens, cache_hit_count, request_time)
		VALUES
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	if err := DB.Exec(insertSQL,
		"2026-04-06", 1, 1, "gpt-4.1", 2, 20, 120, 30, 0, 40, 10, 1, 50,
		"2026-04-06", 1, 2, "gpt-4.1", 1, 10, 60, 15, 5, 0, 0, 1, 25,
		"2026-04-06", 1, 2, "gpt-4o", 4, 40, 40, 8, 0, 0, 0, 0, 70,
		"2026-04-05", 1, 1, "gpt-4.1", 9, 90, 900, 90, 9, 9, 9, 9, 90,
		"2026-04-05", 1, 3, "gpt-4.1-mini", 7, 70, 700, 70, 7, 7, 7, 7, 70,
		"2026-04-06", 2, 1, "gpt-4.1", 99, 99, 99, 99, 99, 99, 99, 99, 99,
	).Error; err != nil {
		t.Fatalf("expected statistics fixtures to persist, got %v", err)
	}

	dateRange := DashboardDateRange{
		Start: "2026-03-31",
		End:   "2026-04-06",
		Today: "2026-04-06",
	}

	overview, err := GetUserDashboardCacheOverview(1, dateRange, DashboardCacheOverviewFilters{
		ModelName: "gpt-4.1",
	})
	if err != nil {
		t.Fatalf("expected cache overview lookup to succeed, got %v", err)
	}

	if len(overview.AvailableDates) != 7 || overview.AvailableDates[0] != "2026-03-31" || overview.AvailableDates[6] != "2026-04-06" {
		t.Fatalf("expected seven dashboard dates, got %+v", overview.AvailableDates)
	}
	if len(overview.TokenBreakdownByDay) != 7 {
		t.Fatalf("expected seven token breakdown rows, got %+v", overview.TokenBreakdownByDay)
	}
	if overview.TokenBreakdownByDay[5].Date != "2026-04-05" || overview.TokenBreakdownByDay[5].RequestCount != 9 {
		t.Fatalf("expected historical rows to stay available in the seven-day snapshot, got %+v", overview.TokenBreakdownByDay[5])
	}
	todayBreakdown := overview.TokenBreakdownByDay[6]
	if todayBreakdown.RequestCount != 3 {
		t.Fatalf("expected filtered today request count to be 3, got %+v", todayBreakdown)
	}
	if todayBreakdown.InputTokens != 125 {
		t.Fatalf("expected filtered today input tokens to be 125, got %+v", todayBreakdown)
	}
	if todayBreakdown.CacheTokens != 5 || todayBreakdown.CacheReadTokens != 40 || todayBreakdown.CacheWriteTokens != 10 {
		t.Fatalf("expected filtered today cache token totals to match model slice, got %+v", todayBreakdown)
	}
	todayHitRate := overview.CacheHitRateByDay[6]
	if math.Abs(todayHitRate.HitRate-(2.0/3.0)) > 1e-9 {
		t.Fatalf("expected filtered today hit rate to be 2/3, got %+v", todayHitRate)
	}
	if len(overview.FilterOptions.Models) != 3 || overview.FilterOptions.Models[0] != "gpt-4.1" || overview.FilterOptions.Models[1] != "gpt-4.1-mini" || overview.FilterOptions.Models[2] != "gpt-4o" {
		t.Fatalf("expected filter models to stay unfiltered and sorted, got %+v", overview.FilterOptions.Models)
	}
	if len(overview.FilterOptions.Channels) != 3 || overview.FilterOptions.Channels[0].Id != 1 || overview.FilterOptions.Channels[1].Id != 2 || overview.FilterOptions.Channels[2].Id != 3 {
		t.Fatalf("expected filter channels to expose only stable ids, got %+v", overview.FilterOptions.Channels)
	}
	if overview.FilterOptions.Channels[0].Name != "" || overview.FilterOptions.Channels[1].Name != "" {
		t.Fatalf("expected filter channels not to leak internal names, got %+v", overview.FilterOptions.Channels)
	}
	if overview.DateRange != dateRange {
		t.Fatalf("expected cache overview to echo the snapshot date range, got %+v", overview.DateRange)
	}

	channelOverview, err := GetUserDashboardCacheOverview(1, dateRange, DashboardCacheOverviewFilters{
		ChannelId: 2,
	})
	if err != nil {
		t.Fatalf("expected channel-filtered cache overview lookup to succeed, got %v", err)
	}

	channelTodayBreakdown := channelOverview.TokenBreakdownByDay[6]
	if channelTodayBreakdown.RequestCount != 5 {
		t.Fatalf("expected channel filter request count to be 5, got %+v", channelTodayBreakdown)
	}
	channelTodayHitRate := channelOverview.CacheHitRateByDay[6]
	if math.Abs(channelTodayHitRate.HitRate-0.2) > 1e-9 {
		t.Fatalf("expected channel filter hit rate to be 0.2, got %+v", channelTodayHitRate)
	}

	intersectionOverview, err := GetUserDashboardCacheOverview(1, dateRange, DashboardCacheOverviewFilters{
		ModelName: "gpt-4.1",
		ChannelId: 2,
	})
	if err != nil {
		t.Fatalf("expected model+channel filtered cache overview lookup to succeed, got %v", err)
	}

	intersectionTodayBreakdown := intersectionOverview.TokenBreakdownByDay[6]
	if intersectionTodayBreakdown.RequestCount != 1 || intersectionTodayBreakdown.TotalTokens != 75 {
		t.Fatalf("expected intersection filter to isolate a single statistics row, got %+v", intersectionTodayBreakdown)
	}
	intersectionTodayHitRate := intersectionOverview.CacheHitRateByDay[6]
	if math.Abs(intersectionTodayHitRate.HitRate-1.0) > 1e-9 {
		t.Fatalf("expected intersection hit rate to be 1, got %+v", intersectionTodayHitRate)
	}
}

func TestGetUserDashboardCacheOverviewWithChannelNamesIncludesAdminVisibleNamesAndMissingChannels(t *testing.T) {
	useDashboardMigrationTestDB(t)

	if err := DB.Create([]*Channel{
		{Id: 1, Name: "Alpha"},
		{Id: 2, Name: "Beta"},
	}).Error; err != nil {
		t.Fatalf("expected channel fixtures to persist, got %v", err)
	}

	if err := DB.Exec(`
		INSERT INTO statistics
		(date, user_id, channel_id, model_name, request_count, quota, prompt_tokens, completion_tokens, cache_tokens, cache_read_tokens, cache_write_tokens, cache_hit_count, request_time)
		VALUES
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		"2026-04-06", 1, 1, "gpt-4.1", 2, 20, 20, 2, 0, 0, 0, 1, 20,
		"2026-04-06", 1, 2, "gpt-4o", 3, 30, 30, 3, 0, 0, 0, 0, 30,
		"2026-04-05", 1, 9, "historical-only", 4, 40, 40, 4, 0, 0, 0, 0, 40,
		"2026-04-06", 2, 1, "other-user", 5, 50, 50, 5, 0, 0, 0, 0, 50,
	).Error; err != nil {
		t.Fatalf("expected statistics fixtures to persist, got %v", err)
	}

	overview, err := GetUserDashboardCacheOverviewWithChannelNames(1, DashboardDateRange{
		Start: "2026-03-31",
		End:   "2026-04-06",
		Today: "2026-04-06",
	}, DashboardCacheOverviewFilters{})
	if err != nil {
		t.Fatalf("expected cache overview lookup to succeed, got %v", err)
	}

	if len(overview.FilterOptions.Channels) != 3 {
		t.Fatalf("expected 3 channel options, got %+v", overview.FilterOptions.Channels)
	}
	if overview.FilterOptions.Channels[0].Id != 1 || overview.FilterOptions.Channels[0].Name != "Alpha" {
		t.Fatalf("expected channel 1 to expose its admin-visible name, got %+v", overview.FilterOptions.Channels[0])
	}
	if overview.FilterOptions.Channels[1].Id != 2 || overview.FilterOptions.Channels[1].Name != "Beta" {
		t.Fatalf("expected channel 2 to expose its admin-visible name, got %+v", overview.FilterOptions.Channels[1])
	}
	if overview.FilterOptions.Channels[2].Id != 9 || overview.FilterOptions.Channels[2].Name != "" {
		t.Fatalf("expected missing channel rows to fall back to id-only options, got %+v", overview.FilterOptions.Channels[2])
	}
}

func TestGetUserDashboardCacheOverviewReturnsEmptyDayRowsWhenSomeDaysHaveNoData(t *testing.T) {
	useDashboardMigrationTestDB(t)

	if err := DB.Exec(`
		INSERT INTO statistics
		(date, user_id, channel_id, model_name, request_count, quota, prompt_tokens, completion_tokens, cache_tokens, cache_read_tokens, cache_write_tokens, cache_hit_count, request_time)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		"2026-04-05", 1, 2, "gpt-4.1", 3, 30, 300, 30, 3, 3, 3, 1, 30,
	).Error; err != nil {
		t.Fatalf("expected historical statistics fixture to persist, got %v", err)
	}

	overview, err := GetUserDashboardCacheOverview(1, DashboardDateRange{
		Start: "2026-03-31",
		End:   "2026-04-06",
		Today: "2026-04-06",
	}, DashboardCacheOverviewFilters{})
	if err != nil {
		t.Fatalf("expected cache overview lookup to succeed, got %v", err)
	}

	if len(overview.TokenBreakdownByDay) != 7 || len(overview.CacheHitRateByDay) != 7 {
		t.Fatalf("expected each day in the snapshot to be represented, got token=%+v cache=%+v", overview.TokenBreakdownByDay, overview.CacheHitRateByDay)
	}
	if overview.TokenBreakdownByDay[5].RequestCount != 3 || overview.TokenBreakdownByDay[5].Date != "2026-04-05" {
		t.Fatalf("expected populated historical day to be preserved, got %+v", overview.TokenBreakdownByDay[5])
	}
	emptyTodayBreakdown := overview.TokenBreakdownByDay[6]
	if emptyTodayBreakdown.Date != "2026-04-06" || emptyTodayBreakdown.DashboardTokenBreakdown != (DashboardTokenBreakdown{}) {
		t.Fatalf("expected today token breakdown to stay empty when today has no data, got %+v", emptyTodayBreakdown)
	}
	emptyTodayHitRate := overview.CacheHitRateByDay[6]
	if emptyTodayHitRate.Date != "2026-04-06" || emptyTodayHitRate.RequestCount != 0 || emptyTodayHitRate.CacheHitCount != 0 || emptyTodayHitRate.HitRate != 0 {
		t.Fatalf("expected today cache hit rate to stay empty when today has no data, got %+v", emptyTodayHitRate)
	}
	if overview.FilterOptions.Models == nil || len(overview.FilterOptions.Models) != 1 || overview.FilterOptions.Models[0] != "gpt-4.1" {
		t.Fatalf("expected seven-day model options to include historical data, got %+v", overview.FilterOptions.Models)
	}
	if overview.FilterOptions.Channels == nil || len(overview.FilterOptions.Channels) != 1 || overview.FilterOptions.Channels[0].Id != 2 {
		t.Fatalf("expected seven-day channel options to include historical data, got %+v", overview.FilterOptions.Channels)
	}
}

func TestGetUserDashboardCacheFilterOptionsKeepsEmptyChannelSliceSerializable(t *testing.T) {
	useDashboardMigrationTestDB(t)

	if err := DB.Exec(`
		INSERT INTO statistics
		(date, user_id, channel_id, model_name, request_count, quota, prompt_tokens, completion_tokens, cache_tokens, cache_read_tokens, cache_write_tokens, cache_hit_count, request_time)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		"2026-04-06", 1, 0, "gpt-4.1", 1, 10, 20, 5, 0, 0, 0, 0, 10,
	).Error; err != nil {
		t.Fatalf("expected statistics fixture to persist, got %v", err)
	}

	filterOptions, err := GetUserDashboardCacheFilterOptions(1, DashboardDateRange{
		Start: "2026-03-31",
		End:   "2026-04-06",
		Today: "2026-04-06",
	})
	if err != nil {
		t.Fatalf("expected cache filter options lookup to succeed, got %v", err)
	}

	if filterOptions.Models == nil || len(filterOptions.Models) != 1 || filterOptions.Models[0] != "gpt-4.1" {
		t.Fatalf("expected model options to include the persisted model, got %+v", filterOptions.Models)
	}
	if filterOptions.Channels == nil {
		t.Fatalf("expected channels slice to be initialized to an empty slice, got nil")
	}
	if len(filterOptions.Channels) != 0 {
		t.Fatalf("expected no channel options for channel_id=0 only data, got %+v", filterOptions.Channels)
	}

	payload, err := json.Marshal(filterOptions)
	if err != nil {
		t.Fatalf("expected cache filter options to marshal, got %v", err)
	}
	if string(payload) != `{"models":["gpt-4.1"],"channels":[]}` {
		t.Fatalf("expected channels to serialize as an empty array, got %s", payload)
	}
}

func TestGetUserDashboardStatisticsByPeriodIncludesCacheOverviewFilterOptions(t *testing.T) {
	useDashboardMigrationTestDB(t)

	if err := DB.Exec(`
		INSERT INTO statistics
		(date, user_id, channel_id, model_name, request_count, quota, prompt_tokens, completion_tokens, cache_tokens, cache_read_tokens, cache_write_tokens, cache_hit_count, request_time)
		VALUES
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		"2026-04-06", 1, 1, "gpt-4.1", 2, 20, 20, 2, 0, 0, 0, 1, 20,
		"2026-04-06", 1, 2, "gpt-4o", 3, 30, 30, 3, 0, 0, 0, 0, 30,
		"2026-04-05", 1, 3, "historical-only", 4, 40, 40, 4, 0, 0, 0, 0, 40,
		"2026-04-06", 2, 9, "other-user", 5, 50, 50, 5, 0, 0, 0, 0, 50,
	).Error; err != nil {
		t.Fatalf("expected statistics fixtures to persist, got %v", err)
	}

	dashboard, err := GetUserDashboardStatisticsByPeriod(1, DashboardDateRange{
		Start: "2026-03-31",
		End:   "2026-04-06",
		Today: "2026-04-06",
	})
	if err != nil {
		t.Fatalf("expected dashboard lookup to succeed, got %v", err)
	}

	if len(dashboard.CacheOverviewFilterOptions.Models) != 3 || dashboard.CacheOverviewFilterOptions.Models[0] != "gpt-4.1" || dashboard.CacheOverviewFilterOptions.Models[1] != "gpt-4o" || dashboard.CacheOverviewFilterOptions.Models[2] != "historical-only" {
		t.Fatalf("expected seven-day model options in dashboard payload, got %+v", dashboard.CacheOverviewFilterOptions.Models)
	}
	if len(dashboard.CacheOverviewFilterOptions.Channels) != 3 || dashboard.CacheOverviewFilterOptions.Channels[0].Id != 1 || dashboard.CacheOverviewFilterOptions.Channels[1].Id != 2 || dashboard.CacheOverviewFilterOptions.Channels[2].Id != 3 {
		t.Fatalf("expected seven-day channel ids in dashboard payload, got %+v", dashboard.CacheOverviewFilterOptions.Channels)
	}
	for _, channel := range dashboard.CacheOverviewFilterOptions.Channels {
		if channel.Name != "" {
			t.Fatalf("expected dashboard filter options to keep channel names private, got %+v", dashboard.CacheOverviewFilterOptions.Channels)
		}
	}
}

func TestGetUserDashboardStatisticsByPeriodWithChannelNamesIncludesCacheOverviewFilterOptions(t *testing.T) {
	useDashboardMigrationTestDB(t)

	if err := DB.Create([]*Channel{
		{Id: 1, Name: "Alpha"},
		{Id: 2, Name: "Beta"},
	}).Error; err != nil {
		t.Fatalf("expected channel fixtures to persist, got %v", err)
	}

	if err := DB.Exec(`
		INSERT INTO statistics
		(date, user_id, channel_id, model_name, request_count, quota, prompt_tokens, completion_tokens, cache_tokens, cache_read_tokens, cache_write_tokens, cache_hit_count, request_time)
		VALUES
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		"2026-04-06", 1, 1, "gpt-4.1", 2, 20, 20, 2, 0, 0, 0, 1, 20,
		"2026-04-06", 1, 2, "gpt-4o", 3, 30, 30, 3, 0, 0, 0, 0, 30,
		"2026-04-05", 1, 9, "historical-only", 4, 40, 40, 4, 0, 0, 0, 0, 40,
		"2026-04-06", 2, 1, "other-user", 5, 50, 50, 5, 0, 0, 0, 0, 50,
	).Error; err != nil {
		t.Fatalf("expected statistics fixtures to persist, got %v", err)
	}

	dashboard, err := GetUserDashboardStatisticsByPeriodWithChannelNames(1, DashboardDateRange{
		Start: "2026-03-31",
		End:   "2026-04-06",
		Today: "2026-04-06",
	})
	if err != nil {
		t.Fatalf("expected dashboard lookup to succeed, got %v", err)
	}

	if len(dashboard.CacheOverviewFilterOptions.Channels) != 3 {
		t.Fatalf("expected three channel options in dashboard payload, got %+v", dashboard.CacheOverviewFilterOptions.Channels)
	}
	if dashboard.CacheOverviewFilterOptions.Channels[0].Id != 1 || dashboard.CacheOverviewFilterOptions.Channels[0].Name != "Alpha" {
		t.Fatalf("expected channel 1 to include its admin-visible name, got %+v", dashboard.CacheOverviewFilterOptions.Channels[0])
	}
	if dashboard.CacheOverviewFilterOptions.Channels[1].Id != 2 || dashboard.CacheOverviewFilterOptions.Channels[1].Name != "Beta" {
		t.Fatalf("expected channel 2 to include its admin-visible name, got %+v", dashboard.CacheOverviewFilterOptions.Channels[1])
	}
	if dashboard.CacheOverviewFilterOptions.Channels[2].Id != 9 || dashboard.CacheOverviewFilterOptions.Channels[2].Name != "" {
		t.Fatalf("expected missing channels to stay selectable by id, got %+v", dashboard.CacheOverviewFilterOptions.Channels[2])
	}
}

func TestBackfillLogCacheTokensFromMetadataRebuildsStatistics(t *testing.T) {
	useDashboardCacheTokenTestDB(t)

	createdAt := time.Date(2026, 4, 6, 4, 0, 0, 0, time.UTC).Unix()
	logs := []*Log{
		{
			UserId:           1,
			CreatedAt:        createdAt,
			Type:             LogTypeConsume,
			ChannelId:        2,
			ModelName:        "gpt-4.1",
			PromptTokens:     30,
			CompletionTokens: 7,
			Quota:            123,
			RequestTime:      456,
			Metadata: datatypes.NewJSONType(map[string]any{
				config.UsageExtraCachedRead:  12,
				config.UsageExtraCachedWrite: 5,
			}),
		},
		{
			UserId:           1,
			CreatedAt:        createdAt + 1,
			Type:             LogTypeConsume,
			ChannelId:        2,
			ModelName:        "gpt-4.1",
			PromptTokens:     20,
			CompletionTokens: 1,
			Quota:            77,
			RequestTime:      100,
			Metadata: datatypes.NewJSONType(map[string]any{
				"extra_tokens": map[string]any{
					config.UsageExtraCachedWrite: "3",
				},
			}),
		},
		{
			UserId:           1,
			CreatedAt:        createdAt + 2,
			Type:             LogTypeConsume,
			ChannelId:        2,
			ModelName:        "gpt-4.1",
			PromptTokens:     9,
			CompletionTokens: 2,
			Quota:            50,
			RequestTime:      50,
			Metadata: datatypes.NewJSONType(map[string]any{
				config.UsageExtraCache: "9",
			}),
		},
		{
			UserId:           1,
			CreatedAt:        createdAt + 3,
			Type:             LogTypeConsume,
			ChannelId:        2,
			ModelName:        "gpt-4.1",
			PromptTokens:     4,
			CompletionTokens: 0,
			Quota:            10,
			RequestTime:      20,
			Metadata: datatypes.NewJSONType(map[string]any{
				"extra_tokens": map[string]any{
					config.UsageExtraCache: 4,
				},
			}),
		},
	}

	if err := DB.Create(logs).Error; err != nil {
		t.Fatalf("expected logs fixture to persist, got %v", err)
	}

	startTimestamp := createdAt - 60
	endTimestamp := createdAt + 60
	if err := backfillLogCacheTokensFromMetadata(DB, startTimestamp, endTimestamp); err != nil {
		t.Fatalf("expected log cache token backfill to succeed, got %v", err)
	}
	if err := rebuildStatisticsByCreatedAtRange(DB, startTimestamp, endTimestamp); err != nil {
		t.Fatalf("expected statistics rebuild to succeed, got %v", err)
	}

	var persistedLogs []Log
	if err := DB.Order("created_at asc").Find(&persistedLogs).Error; err != nil {
		t.Fatalf("expected persisted logs lookup to succeed, got %v", err)
	}
	if persistedLogs[0].CacheTokens != 0 || persistedLogs[0].CacheReadTokens != 12 || persistedLogs[0].CacheWriteTokens != 5 {
		t.Fatalf("expected direct metadata cache tokens to be backfilled, got %+v", persistedLogs[0])
	}
	if persistedLogs[1].CacheTokens != 0 || persistedLogs[1].CacheReadTokens != 0 || persistedLogs[1].CacheWriteTokens != 3 {
		t.Fatalf("expected nested metadata cache tokens to be backfilled, got %+v", persistedLogs[1])
	}
	if persistedLogs[2].CacheTokens != 9 || persistedLogs[2].CacheReadTokens != 0 || persistedLogs[2].CacheWriteTokens != 0 {
		t.Fatalf("expected direct metadata generic cache tokens to be backfilled, got %+v", persistedLogs[2])
	}
	if persistedLogs[3].CacheTokens != 4 || persistedLogs[3].CacheReadTokens != 0 || persistedLogs[3].CacheWriteTokens != 0 {
		t.Fatalf("expected nested metadata generic cache tokens to be backfilled, got %+v", persistedLogs[3])
	}

	var statistic Statistics
	if err := DB.First(&statistic).Error; err != nil {
		t.Fatalf("expected statistics lookup to succeed, got %v", err)
	}
	if statistic.RequestCount != 4 || statistic.Quota != 260 {
		t.Fatalf("expected statistics request/quota aggregation to be rebuilt, got %+v", statistic)
	}
	if statistic.CacheTokens != 13 || statistic.CacheReadTokens != 12 || statistic.CacheWriteTokens != 8 {
		t.Fatalf("expected statistics cache token aggregation to be rebuilt, got %+v", statistic)
	}
	if statistic.CacheHitCount != 3 {
		t.Fatalf("expected statistics cache hit count to be rebuilt, got %+v", statistic)
	}
}

func TestRebuildStatisticsByCreatedAtRangeRollsBackOnInsertFailure(t *testing.T) {
	useDashboardCacheTokenTestDB(t)

	createdAt := time.Date(2026, 4, 6, 4, 0, 0, 0, time.UTC).Unix()
	date := time.Unix(createdAt, 0).In(time.Local)
	existing := &Statistics{
		Date:             time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.Local),
		UserId:           1,
		ChannelId:        2,
		ModelName:        "gpt-4.1",
		RequestCount:     7,
		Quota:            70,
		PromptTokens:     30,
		CompletionTokens: 11,
		CacheTokens:      4,
		CacheReadTokens:  3,
		CacheWriteTokens: 2,
		CacheHitCount:    1,
		RequestTime:      90,
	}
	if err := DB.Create(existing).Error; err != nil {
		t.Fatalf("expected statistics fixture to persist, got %v", err)
	}
	if err := DB.Exec("DROP TABLE logs").Error; err != nil {
		t.Fatalf("expected logs table drop to succeed, got %v", err)
	}

	err := rebuildStatisticsByCreatedAtRange(nil, createdAt-60, createdAt+60)
	if err == nil {
		t.Fatal("expected statistics rebuild to fail when logs table is unavailable")
	}

	var count int64
	if err := DB.Model(&Statistics{}).Count(&count).Error; err != nil {
		t.Fatalf("expected statistics count lookup to succeed, got %v", err)
	}
	if count != 1 {
		t.Fatalf("expected failed rebuild to leave original statistic in place, got %d rows", count)
	}

	var persisted Statistics
	if err := DB.First(&persisted).Error; err != nil {
		t.Fatalf("expected original statistic to remain after rollback, got %v", err)
	}
	if persisted.RequestCount != existing.RequestCount || persisted.CacheReadTokens != existing.CacheReadTokens || persisted.CacheHitCount != existing.CacheHitCount {
		t.Fatalf("expected failed rebuild to preserve original statistic, got %+v", persisted)
	}
}

func TestAfterAutoMigrateMigrationsRunOnlyOnce(t *testing.T) {
	useDashboardMigrationTestDB(t)

	log := &Log{
		UserId:           1,
		CreatedAt:        time.Now().Add(-time.Hour).Unix(),
		Type:             LogTypeConsume,
		ChannelId:        2,
		ModelName:        "gpt-4.1",
		PromptTokens:     30,
		CompletionTokens: 7,
		Quota:            123,
		RequestTime:      456,
		Metadata: datatypes.NewJSONType(map[string]any{
			config.UsageExtraCachedRead: 9,
		}),
	}
	if err := DB.Create(log).Error; err != nil {
		t.Fatalf("expected logs fixture to persist, got %v", err)
	}

	if err := gormigrate.New(DB, gormigrate.DefaultOptions, afterAutoMigrateMigrations()).Migrate(); err != nil {
		t.Fatalf("expected first migration run to succeed, got %v", err)
	}

	var migratedLog Log
	if err := DB.First(&migratedLog, log.Id).Error; err != nil {
		t.Fatalf("expected migrated log lookup to succeed, got %v", err)
	}
	if migratedLog.CacheReadTokens != 9 {
		t.Fatalf("expected first migration run to backfill cache read tokens, got %+v", migratedLog)
	}

	var statistic Statistics
	if err := DB.Where("user_id = ? AND channel_id = ? AND model_name = ?", log.UserId, log.ChannelId, log.ModelName).First(&statistic).Error; err != nil {
		t.Fatalf("expected migrated statistics lookup to succeed, got %v", err)
	}
	if statistic.CacheReadTokens != 9 || statistic.CacheHitCount != 1 {
		t.Fatalf("expected first migration run to rebuild statistics, got %+v", statistic)
	}

	if err := DB.Model(&Log{}).Where("id = ?", log.Id).Updates(map[string]any{
		"cache_tokens":       0,
		"cache_read_tokens":  0,
		"cache_write_tokens": 0,
	}).Error; err != nil {
		t.Fatalf("expected log corruption setup to succeed, got %v", err)
	}
	if err := DB.Model(&Statistics{}).Where("user_id = ? AND channel_id = ? AND model_name = ?", log.UserId, log.ChannelId, log.ModelName).Updates(map[string]any{
		"cache_tokens":       0,
		"cache_read_tokens":  0,
		"cache_write_tokens": 0,
		"cache_hit_count":    0,
	}).Error; err != nil {
		t.Fatalf("expected statistics corruption setup to succeed, got %v", err)
	}

	if err := gormigrate.New(DB, gormigrate.DefaultOptions, afterAutoMigrateMigrations()).Migrate(); err != nil {
		t.Fatalf("expected second migration run to succeed, got %v", err)
	}

	if err := DB.First(&migratedLog, log.Id).Error; err != nil {
		t.Fatalf("expected log lookup after second migration run to succeed, got %v", err)
	}
	if migratedLog.CacheReadTokens != 0 {
		t.Fatalf("expected second migration run to skip already-applied migrations, got %+v", migratedLog)
	}

	var rerunStatistic Statistics
	if err := DB.Where("user_id = ? AND channel_id = ? AND model_name = ?", log.UserId, log.ChannelId, log.ModelName).First(&rerunStatistic).Error; err != nil {
		t.Fatalf("expected statistics lookup after second migration run to succeed, got %v", err)
	}
	if rerunStatistic.CacheReadTokens != 0 || rerunStatistic.CacheHitCount != 0 {
		t.Fatalf("expected second migration run to skip already-applied migrations, got %+v", rerunStatistic)
	}
}
