package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/model"
	"time"

	"github.com/gin-gonic/gin"
)

const dashboardModuleCacheOverview = "cache_overview"

var dashboardModuleNow = time.Now

type DashboardModuleQueryRequest struct {
	DateRange *model.DashboardDateRange `json:"dateRange,omitempty"`
	Modules   []DashboardModuleQuery    `json:"modules" binding:"required"`
}

type DashboardModuleQuery struct {
	Name    string          `json:"name" binding:"required"`
	Filters json.RawMessage `json:"filters,omitempty"`
}

func validateDashboardModuleQueries(moduleQueries []DashboardModuleQuery) error {
	seen := make(map[string]struct{}, len(moduleQueries))
	for _, moduleQuery := range moduleQueries {
		if _, exists := seen[moduleQuery.Name]; exists {
			// The response payload is keyed by module name, so accepting repeated
			// names would silently drop earlier results after still doing the work.
			return fmt.Errorf("duplicate dashboard module: %s", moduleQuery.Name)
		}
		seen[moduleQuery.Name] = struct{}{}
	}
	return nil
}

func resolveDashboardDateRange(requested *model.DashboardDateRange, now time.Time) (model.DashboardDateRange, error) {
	if requested == nil {
		return model.BuildDashboardDateRange(now), nil
	}
	dateRange, err := model.NormalizeDashboardDateRange(*requested)
	if err != nil {
		return model.DashboardDateRange{}, err
	}
	if err := model.ValidateDashboardDateRangeMatchesSnapshot(dateRange, now); err != nil {
		return model.DashboardDateRange{}, err
	}
	return dateRange, nil
}

func respondDashboardModuleError(c *gin.Context, err error) {
	if model.IsDashboardSnapshotMismatchError(err) {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
			"code":    model.DashboardSnapshotMismatchCode,
		})
		return
	}

	common.APIRespondWithError(c, http.StatusOK, err)
}

func QueryUserDashboardModules(c *gin.Context) {
	// The web API client expects the project's standard success envelope even
	// for validation failures; returning non-2xx here breaks dashboard errors.
	var req DashboardModuleQueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	if len(req.Modules) == 0 {
		common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("modules is required"))
		return
	}
	if err := validateDashboardModuleQueries(req.Modules); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	userId := c.GetInt("id")
	dateRange, err := resolveDashboardDateRange(req.DateRange, dashboardModuleNow())
	if err != nil {
		respondDashboardModuleError(c, err)
		return
	}

	modules := make(map[string]any, len(req.Modules))
	for _, moduleQuery := range req.Modules {
		switch moduleQuery.Name {
		case dashboardModuleCacheOverview:
			var filters model.DashboardCacheOverviewFilters
			if len(moduleQuery.Filters) > 0 {
				if err := json.Unmarshal(moduleQuery.Filters, &filters); err != nil {
					common.APIRespondWithError(c, http.StatusOK, err)
					return
				}
			}

			cacheOverview, err := model.GetUserDashboardCacheOverview(userId, dateRange, filters)
			if err != nil {
				common.APIRespondWithError(c, http.StatusOK, err)
				return
			}
			modules[moduleQuery.Name] = cacheOverview
		default:
			common.APIRespondWithError(c, http.StatusOK, fmt.Errorf("unknown dashboard module: %s", moduleQuery.Name))
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"modules": modules,
		},
	})
}
