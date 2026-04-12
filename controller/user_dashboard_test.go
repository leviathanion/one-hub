package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	commonTest "one-api/common/test"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useControllerDashboardTestDB(t *testing.T) {
	t.Helper()

	logger.Logger = zap.NewNop()

	originalDB := model.DB
	originalUsingSQLite := common.UsingSQLite
	originalUsingPostgreSQL := common.UsingPostgreSQL

	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.Log{}, &model.Statistics{}, &model.Channel{}, &model.Token{}, &model.Option{}, &model.Price{}, &model.UserGroup{}); err != nil {
		t.Fatalf("expected dashboard test schema migration to succeed, got %v", err)
	}

	model.DB = testDB
	common.UsingSQLite = true
	common.UsingPostgreSQL = false

	t.Cleanup(func() {
		model.DB = originalDB
		common.UsingSQLite = originalUsingSQLite
		common.UsingPostgreSQL = originalUsingPostgreSQL
	})
}

func seedControllerDashboardFixture(t *testing.T) {
	t.Helper()

	if err := model.DB.Create([]*model.Channel{
		{Id: 1, Name: "Alpha"},
		{Id: 2, Name: "Beta"},
	}).Error; err != nil {
		t.Fatalf("expected channel fixtures to persist, got %v", err)
	}

	if err := model.DB.Exec(`
		INSERT INTO statistics
		(date, user_id, channel_id, model_name, request_count, quota, prompt_tokens, completion_tokens, cache_tokens, cache_read_tokens, cache_write_tokens, cache_hit_count, request_time)
		VALUES
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		"2026-04-06", 1, 1, "gpt-4.1", 2, 20, 20, 2, 0, 0, 0, 1, 20,
		"2026-04-06", 1, 2, "gpt-4o", 3, 30, 30, 3, 0, 0, 0, 0, 30,
		"2026-04-05", 1, 9, "historical-only", 4, 40, 40, 4, 0, 0, 0, 0, 40,
	).Error; err != nil {
		t.Fatalf("expected statistics fixtures to persist, got %v", err)
	}
}

func TestQueryUserDashboardModulesValidationErrorsUseLegacySuccessEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalNow := dashboardModuleNow
	dashboardModuleNow = func() time.Time {
		return time.Date(2026, 4, 6, 12, 0, 0, 0, time.Local)
	}
	t.Cleanup(func() {
		dashboardModuleNow = originalNow
	})

	testCases := []struct {
		name          string
		body          string
		expectedInMsg string
		expectedCode  string
	}{
		{
			name:          "bad json",
			body:          `{"modules":[`,
			expectedInMsg: "unexpected EOF",
		},
		{
			name:          "invalid date range",
			body:          `{"dateRange":{"start":"2026-04-06","end":"2026-04-05","today":"2026-04-06"},"modules":[{"name":"cache_overview"}]}`,
			expectedInMsg: "dateRange.end must not be before dateRange.start",
		},
		{
			name:          "date range must match current snapshot",
			body:          `{"dateRange":{"start":"2026-01-01","end":"2026-01-07","today":"2026-01-07"},"modules":[{"name":"cache_overview"}]}`,
			expectedInMsg: "dateRange must match the current dashboard snapshot (expected start=2026-03-31 end=2026-04-06 today=2026-04-06, got start=2026-01-01 end=2026-01-07 today=2026-01-07)",
			expectedCode:  "DASHBOARD_SNAPSHOT_MISMATCH",
		},
		{
			name:          "duplicate modules",
			body:          `{"modules":[{"name":"cache_overview"},{"name":"cache_overview"}]}`,
			expectedInMsg: "duplicate dashboard module: cache_overview",
		},
		{
			name:          "unknown module",
			body:          `{"modules":[{"name":"unknown"}]}`,
			expectedInMsg: "unknown dashboard module: unknown",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, recorder := commonTest.GetContext(
				http.MethodPost,
				"/api/user/dashboard/modules/query",
				commonTest.RequestJSONConfig(),
				strings.NewReader(tc.body),
			)
			ctx.Set("id", 1)

			QueryUserDashboardModules(ctx)

			if recorder.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d", recorder.Code)
			}

			var resp struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
				Code    string `json:"code"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			if resp.Success {
				t.Fatalf("expected failure envelope, got body %s", recorder.Body.String())
			}
			if !strings.Contains(resp.Message, tc.expectedInMsg) {
				t.Fatalf("expected message to contain %q, got %q", tc.expectedInMsg, resp.Message)
			}
			if resp.Code != tc.expectedCode {
				t.Fatalf("expected code %q, got %q", tc.expectedCode, resp.Code)
			}
		})
	}
}

func TestGetUserDashboardShowsChannelNamesOnlyToAdmins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerDashboardTestDB(t)
	seedControllerDashboardFixture(t)

	originalNow := userDashboardNow
	userDashboardNow = func() time.Time {
		return time.Date(2026, 4, 6, 12, 0, 0, 0, time.Local)
	}
	t.Cleanup(func() {
		userDashboardNow = originalNow
	})

	testCases := []struct {
		name         string
		role         int
		expectedName string
	}{
		{
			name:         "non-admin keeps channel names private",
			role:         0,
			expectedName: "",
		},
		{
			name:         "admin sees channel names",
			role:         config.RoleAdminUser,
			expectedName: "Alpha",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, recorder := commonTest.GetContext(http.MethodGet, "/api/user/dashboard", nil, nil)
			ctx.Set("id", 1)
			ctx.Set("role", tc.role)

			GetUserDashboard(ctx)

			if recorder.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d", recorder.Code)
			}

			var resp struct {
				Success bool `json:"success"`
				Data    struct {
					CacheOverviewFilterOptions struct {
						Channels []model.DashboardChannelOption `json:"channels"`
					} `json:"cacheOverviewFilterOptions"`
				} `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			if !resp.Success {
				t.Fatalf("expected success response, got %s", recorder.Body.String())
			}
			if len(resp.Data.CacheOverviewFilterOptions.Channels) != 3 {
				t.Fatalf("expected three channel options, got %+v", resp.Data.CacheOverviewFilterOptions.Channels)
			}
			firstChannel := resp.Data.CacheOverviewFilterOptions.Channels[0]
			if firstChannel.Id != 1 || firstChannel.Name != tc.expectedName {
				t.Fatalf("expected first channel to be {id:1 name:%q}, got %+v", tc.expectedName, firstChannel)
			}
			if resp.Data.CacheOverviewFilterOptions.Channels[2].Id != 9 || resp.Data.CacheOverviewFilterOptions.Channels[2].Name != "" {
				t.Fatalf("expected missing channels to stay id-only, got %+v", resp.Data.CacheOverviewFilterOptions.Channels[2])
			}
		})
	}
}

func TestQueryUserDashboardModulesShowsChannelNamesOnlyToAdmins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerDashboardTestDB(t)
	seedControllerDashboardFixture(t)

	originalNow := dashboardModuleNow
	dashboardModuleNow = func() time.Time {
		return time.Date(2026, 4, 6, 12, 0, 0, 0, time.Local)
	}
	t.Cleanup(func() {
		dashboardModuleNow = originalNow
	})

	testCases := []struct {
		name         string
		role         int
		expectedName string
	}{
		{
			name:         "non-admin keeps channel names private",
			role:         0,
			expectedName: "",
		},
		{
			name:         "admin sees channel names",
			role:         config.RoleAdminUser,
			expectedName: "Alpha",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, recorder := commonTest.GetContext(
				http.MethodPost,
				"/api/user/dashboard/modules/query",
				commonTest.RequestJSONConfig(),
				strings.NewReader(`{"modules":[{"name":"cache_overview"}]}`),
			)
			ctx.Set("id", 1)
			ctx.Set("role", tc.role)

			QueryUserDashboardModules(ctx)

			if recorder.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d", recorder.Code)
			}

			var resp struct {
				Success bool `json:"success"`
				Data    struct {
					Modules struct {
						CacheOverview struct {
							FilterOptions struct {
								Channels []model.DashboardChannelOption `json:"channels"`
							} `json:"filterOptions"`
						} `json:"cache_overview"`
					} `json:"modules"`
				} `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			if !resp.Success {
				t.Fatalf("expected success response, got %s", recorder.Body.String())
			}
			channels := resp.Data.Modules.CacheOverview.FilterOptions.Channels
			if len(channels) != 3 {
				t.Fatalf("expected three channel options, got %+v", channels)
			}
			if channels[0].Id != 1 || channels[0].Name != tc.expectedName {
				t.Fatalf("expected first channel to be {id:1 name:%q}, got %+v", tc.expectedName, channels[0])
			}
			if channels[2].Id != 9 || channels[2].Name != "" {
				t.Fatalf("expected missing channels to stay id-only, got %+v", channels[2])
			}
		})
	}
}
