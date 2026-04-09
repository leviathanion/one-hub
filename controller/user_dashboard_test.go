package controller

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	commonTest "one-api/common/test"

	"github.com/gin-gonic/gin"
)

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
