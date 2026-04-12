package model

import (
	"errors"
	"fmt"
	"one-api/common"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

type Statistics struct {
	Date             time.Time `gorm:"primary_key;type:date" json:"date"`
	UserId           int       `json:"user_id" gorm:"primary_key"`
	ChannelId        int       `json:"channel_id" gorm:"primary_key"`
	ModelName        string    `json:"model_name" gorm:"primary_key;type:varchar(255)"`
	RequestCount     int       `json:"request_count"`
	Quota            int       `json:"quota"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CacheTokens      int       `json:"cache_tokens"`
	CacheReadTokens  int       `json:"cache_read_tokens"`
	CacheWriteTokens int       `json:"cache_write_tokens"`
	CacheHitCount    int       `json:"cache_hit_count"`
	RequestTime      int       `json:"request_time"`
}

type DashboardDateRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Today string `json:"today"`
}

type UserDashboard struct {
	DateRange                  DashboardDateRange          `json:"dateRange"`
	Series                     []*LogStatisticGroupModel   `json:"series"`
	TodayTokenBreakdown        DashboardTokenBreakdown     `json:"todayTokenBreakdown"`
	TodayCacheHitRate          DashboardCacheHitRate       `json:"todayCacheHitRate"`
	CacheOverviewFilterOptions DashboardCacheFilterOptions `json:"cacheOverviewFilterOptions"`
}

type DashboardTokenBreakdown struct {
	RequestCount     int64 `json:"requestCount"`
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheTokens      int64 `json:"cacheTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	TotalTokens      int64 `json:"totalTokens"`
}

type DashboardCacheHitRate struct {
	RequestCount  int64                        `json:"requestCount"`
	CacheHitCount int64                        `json:"cacheHitCount"`
	HitRate       float64                      `json:"hitRate"`
	Models        []DashboardCacheHitRateModel `json:"models"`
}

type DashboardCacheHitRateModel struct {
	ModelName     string  `json:"modelName"`
	RequestCount  int64   `json:"requestCount"`
	CacheHitCount int64   `json:"cacheHitCount"`
	HitRate       float64 `json:"hitRate"`
}

type DashboardCacheOverviewFilters struct {
	ModelName string `json:"model_name"`
	ChannelId int    `json:"channel_id"`
}

type DashboardChannelOption struct {
	Id   int    `json:"id" gorm:"column:id"`
	Name string `json:"name" gorm:"column:name"`
}

type DashboardCacheFilterOptions struct {
	Models   []string                 `json:"models"`
	Channels []DashboardChannelOption `json:"channels"`
}

type DashboardTokenBreakdownDay struct {
	Date string `json:"date"`
	DashboardTokenBreakdown
}

type DashboardCacheHitRateDay struct {
	Date string `json:"date"`
	DashboardCacheHitRate
}

type DashboardCacheOverview struct {
	DateRange           DashboardDateRange           `json:"dateRange"`
	AvailableDates      []string                     `json:"availableDates"`
	TokenBreakdownByDay []DashboardTokenBreakdownDay `json:"tokenBreakdownByDay"`
	CacheHitRateByDay   []DashboardCacheHitRateDay   `json:"cacheHitRateByDay"`
	FilterOptions       DashboardCacheFilterOptions  `json:"filterOptions"`
}

const dashboardSnapshotLookbackDays = 6
const DashboardSnapshotMismatchCode = "DASHBOARD_SNAPSHOT_MISMATCH"

type DashboardSnapshotMismatchError struct {
	Expected DashboardDateRange
	Actual   DashboardDateRange
}

func (e *DashboardSnapshotMismatchError) Error() string {
	return fmt.Sprintf(
		"dateRange must match the current dashboard snapshot (expected start=%s end=%s today=%s, got start=%s end=%s today=%s)",
		e.Expected.Start,
		e.Expected.End,
		e.Expected.Today,
		e.Actual.Start,
		e.Actual.End,
		e.Actual.Today,
	)
}

func IsDashboardSnapshotMismatchError(err error) bool {
	var mismatchErr *DashboardSnapshotMismatchError
	return errors.As(err, &mismatchErr)
}

func BuildDashboardDateRange(now time.Time) DashboardDateRange {
	toDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	today := toDay.Format("2006-01-02")

	// The dashboard renders today plus the previous six calendar days, and the
	// statistics table stores dates without a time component, so the query
	// window should stay inclusive on these YYYY-MM-DD boundaries.
	return DashboardDateRange{
		Start: toDay.AddDate(0, 0, -dashboardSnapshotLookbackDays).Format("2006-01-02"),
		End:   today,
		Today: today,
	}
}

func NormalizeDashboardDateRange(dateRange DashboardDateRange) (DashboardDateRange, error) {
	if dateRange.Start == "" || dateRange.End == "" || dateRange.Today == "" {
		return DashboardDateRange{}, fmt.Errorf("dateRange.start, dateRange.end, and dateRange.today are required")
	}

	start, err := time.ParseInLocation("2006-01-02", dateRange.Start, time.Local)
	if err != nil {
		return DashboardDateRange{}, fmt.Errorf("invalid dateRange.start: %w", err)
	}
	end, err := time.ParseInLocation("2006-01-02", dateRange.End, time.Local)
	if err != nil {
		return DashboardDateRange{}, fmt.Errorf("invalid dateRange.end: %w", err)
	}
	today, err := time.ParseInLocation("2006-01-02", dateRange.Today, time.Local)
	if err != nil {
		return DashboardDateRange{}, fmt.Errorf("invalid dateRange.today: %w", err)
	}
	if end.Before(start) {
		return DashboardDateRange{}, fmt.Errorf("dateRange.end must not be before dateRange.start")
	}
	if !end.Equal(today) {
		return DashboardDateRange{}, fmt.Errorf("dateRange.end must equal dateRange.today")
	}

	return DashboardDateRange{
		Start: start.Format("2006-01-02"),
		End:   end.Format("2006-01-02"),
		Today: today.Format("2006-01-02"),
	}, nil
}

func ValidateDashboardDateRangeMatchesSnapshot(dateRange DashboardDateRange, now time.Time) error {
	expected := BuildDashboardDateRange(now)
	if dateRange == expected {
		return nil
	}

	return &DashboardSnapshotMismatchError{
		Expected: expected,
		Actual:   dateRange,
	}
}

func GetUserModelStatisticsByPeriod(userId int, startTime, endTime string) (LogStatistic []*LogStatisticGroupModel, err error) {
	dateStr := "date"
	if common.UsingPostgreSQL {
		dateStr = "TO_CHAR(date, 'YYYY-MM-DD') as date"
	} else if common.UsingSQLite {
		dateStr = "strftime('%Y-%m-%d', date) as date"
	}

	err = DB.Raw(`
		SELECT `+dateStr+`,
		model_name, 
		sum(request_count) as request_count,
		sum(quota) as quota,
		sum(prompt_tokens) as prompt_tokens,
		sum(completion_tokens) as completion_tokens,
		sum(cache_tokens) as cache_tokens,
		sum(cache_read_tokens) as cache_read_tokens,
		sum(cache_write_tokens) as cache_write_tokens,
		sum(cache_hit_count) as cache_hit_count,
		sum(request_time) as request_time
		FROM statistics
		WHERE user_id= ?
		AND date BETWEEN ? AND ?
		GROUP BY date, model_name
		ORDER BY date, model_name
	`, userId, startTime, endTime).Scan(&LogStatistic).Error
	return
}

func GetUserDashboardStatisticsByPeriod(userId int, dateRange DashboardDateRange) (*UserDashboard, error) {
	series, err := GetUserModelStatisticsByPeriod(userId, dateRange.Start, dateRange.End)
	if err != nil {
		return nil, err
	}
	filterOptions, err := GetUserDashboardCacheFilterOptions(userId, dateRange)
	if err != nil {
		return nil, err
	}
	dashboard := buildUserDashboard(series, dateRange.Today)
	dashboard.DateRange = dateRange
	dashboard.CacheOverviewFilterOptions = *filterOptions
	return dashboard, nil
}

// GetUserDashboardCacheOverview serves the cache overview module as the
// dashboard's full seven-day snapshot. The UI defaults to today but can switch
// to any day in the snapshot without another round trip, so the backend returns
// one row per day and keeps filter options scoped to the same seven-day window.
// Trade-off: the filter dropdowns may include options that are empty on the
// currently selected day, but they stay expressive for historical inspection.
func GetUserDashboardCacheOverview(userId int, dateRange DashboardDateRange, filters DashboardCacheOverviewFilters) (*DashboardCacheOverview, error) {
	series, err := getUserModelStatisticsByPeriodWithFilters(userId, dateRange.Start, dateRange.End, filters)
	if err != nil {
		return nil, err
	}

	filterOptions, err := GetUserDashboardCacheFilterOptions(userId, dateRange)
	if err != nil {
		return nil, err
	}

	return &DashboardCacheOverview{
		DateRange:           dateRange,
		AvailableDates:      buildDashboardDateList(dateRange),
		TokenBreakdownByDay: buildDashboardTokenBreakdownByDay(series, dateRange),
		CacheHitRateByDay:   buildDashboardCacheHitRateByDay(series, dateRange),
		FilterOptions:       *filterOptions,
	}, nil
}

func getUserModelStatisticsByPeriodWithFilters(userId int, startTime, endTime string, filters DashboardCacheOverviewFilters) (logStatistic []*LogStatisticGroupModel, err error) {
	dateStr := "date"
	if common.UsingPostgreSQL {
		dateStr = "TO_CHAR(date, 'YYYY-MM-DD') as date"
	} else if common.UsingSQLite {
		dateStr = "strftime('%Y-%m-%d', date) as date"
	}

	var whereClause strings.Builder
	whereClause.WriteString("WHERE user_id = ? AND date BETWEEN ? AND ?")
	args := []interface{}{userId, startTime, endTime}
	if filters.ModelName != "" {
		whereClause.WriteString(" AND model_name = ?")
		args = append(args, filters.ModelName)
	}
	if filters.ChannelId != 0 {
		whereClause.WriteString(" AND channel_id = ?")
		args = append(args, filters.ChannelId)
	}

	query := `
		SELECT ` + dateStr + `,
		model_name,
		sum(request_count) as request_count,
		sum(quota) as quota,
		sum(prompt_tokens) as prompt_tokens,
		sum(completion_tokens) as completion_tokens,
		sum(cache_tokens) as cache_tokens,
		sum(cache_read_tokens) as cache_read_tokens,
		sum(cache_write_tokens) as cache_write_tokens,
		sum(cache_hit_count) as cache_hit_count,
		sum(request_time) as request_time
		FROM statistics
		` + whereClause.String() + `
		GROUP BY date, model_name
		ORDER BY date, model_name
	`

	err = DB.Raw(query, args...).Scan(&logStatistic).Error
	return
}

func GetUserDashboardCacheFilterOptions(userId int, dateRange DashboardDateRange) (*DashboardCacheFilterOptions, error) {
	filterOptions := &DashboardCacheFilterOptions{
		Models:   make([]string, 0),
		Channels: make([]DashboardChannelOption, 0),
	}

	if err := DB.Model(&Statistics{}).
		Where("user_id = ? AND date BETWEEN ? AND ? AND model_name <> ''", userId, dateRange.Start, dateRange.End).
		Distinct().
		Order("model_name").
		Pluck("model_name", &filterOptions.Models).Error; err != nil {
		return nil, err
	}

	// User dashboard filters are visible to every authenticated user, so keep
	// internal channel metadata admin-only and expose only stable channel ids.
	err := DB.Table("statistics").
		Select("statistics.channel_id as id").
		Where("statistics.user_id = ? AND statistics.date BETWEEN ? AND ? AND statistics.channel_id <> 0", userId, dateRange.Start, dateRange.End).
		Group("statistics.channel_id").
		Order("statistics.channel_id").
		Scan(&filterOptions.Channels).Error
	if err != nil {
		return nil, err
	}

	if filterOptions.Models == nil {
		filterOptions.Models = make([]string, 0)
	}
	if filterOptions.Channels == nil {
		filterOptions.Channels = make([]DashboardChannelOption, 0)
	}

	return filterOptions, nil
}

func buildUserDashboard(series []*LogStatisticGroupModel, today string) *UserDashboard {
	dashboard := &UserDashboard{
		Series: series,
		TodayCacheHitRate: DashboardCacheHitRate{
			Models: make([]DashboardCacheHitRateModel, 0),
		},
	}
	if today == "" {
		today = time.Now().Format("2006-01-02")
	}

	modelRates := make(map[string]*DashboardCacheHitRateModel)
	for _, item := range series {
		if item == nil || item.Date != today {
			continue
		}

		inputTokens := normalizeInputTokens(item.PromptTokens, item.CacheTokens, item.CacheReadTokens, item.CacheWriteTokens)
		dashboard.TodayTokenBreakdown.RequestCount += item.RequestCount
		dashboard.TodayTokenBreakdown.InputTokens += inputTokens
		dashboard.TodayTokenBreakdown.OutputTokens += item.CompletionTokens
		dashboard.TodayTokenBreakdown.CacheTokens += item.CacheTokens
		dashboard.TodayTokenBreakdown.CacheReadTokens += item.CacheReadTokens
		dashboard.TodayTokenBreakdown.CacheWriteTokens += item.CacheWriteTokens

		dashboard.TodayCacheHitRate.RequestCount += item.RequestCount
		dashboard.TodayCacheHitRate.CacheHitCount += item.CacheHitCount

		modelRate, ok := modelRates[item.ModelName]
		if !ok {
			modelRate = &DashboardCacheHitRateModel{
				ModelName: item.ModelName,
			}
			modelRates[item.ModelName] = modelRate
		}
		modelRate.RequestCount += item.RequestCount
		modelRate.CacheHitCount += item.CacheHitCount
	}

	dashboard.TodayTokenBreakdown.TotalTokens = dashboard.TodayTokenBreakdown.InputTokens +
		dashboard.TodayTokenBreakdown.OutputTokens +
		dashboard.TodayTokenBreakdown.CacheTokens +
		dashboard.TodayTokenBreakdown.CacheReadTokens +
		dashboard.TodayTokenBreakdown.CacheWriteTokens
	dashboard.TodayCacheHitRate.HitRate = calculateCacheHitRate(
		dashboard.TodayCacheHitRate.CacheHitCount,
		dashboard.TodayCacheHitRate.RequestCount,
	)

	if len(modelRates) > 0 {
		dashboard.TodayCacheHitRate.Models = make([]DashboardCacheHitRateModel, 0, len(modelRates))
		for _, modelRate := range modelRates {
			modelRate.HitRate = calculateCacheHitRate(modelRate.CacheHitCount, modelRate.RequestCount)
			dashboard.TodayCacheHitRate.Models = append(dashboard.TodayCacheHitRate.Models, *modelRate)
		}
		sort.Slice(dashboard.TodayCacheHitRate.Models, func(i, j int) bool {
			return dashboard.TodayCacheHitRate.Models[i].ModelName < dashboard.TodayCacheHitRate.Models[j].ModelName
		})
	}

	return dashboard
}

func buildDashboardDateList(dateRange DashboardDateRange) []string {
	start, err := time.ParseInLocation("2006-01-02", dateRange.Start, time.Local)
	if err != nil {
		return []string{}
	}
	end, err := time.ParseInLocation("2006-01-02", dateRange.End, time.Local)
	if err != nil || end.Before(start) {
		return []string{}
	}

	dates := make([]string, 0, int(end.Sub(start).Hours()/24)+1)
	for current := start; !current.After(end); current = current.AddDate(0, 0, 1) {
		dates = append(dates, current.Format("2006-01-02"))
	}
	return dates
}

func buildDashboardTokenBreakdownByDay(series []*LogStatisticGroupModel, dateRange DashboardDateRange) []DashboardTokenBreakdownDay {
	dates := buildDashboardDateList(dateRange)
	byDate := make(map[string]*DashboardTokenBreakdownDay, len(dates))
	rows := make([]DashboardTokenBreakdownDay, 0, len(dates))
	for _, date := range dates {
		row := DashboardTokenBreakdownDay{Date: date}
		rows = append(rows, row)
		byDate[date] = &rows[len(rows)-1]
	}

	for _, item := range series {
		if item == nil {
			continue
		}
		row, ok := byDate[item.Date]
		if !ok {
			continue
		}

		inputTokens := normalizeInputTokens(item.PromptTokens, item.CacheTokens, item.CacheReadTokens, item.CacheWriteTokens)
		row.RequestCount += item.RequestCount
		row.InputTokens += inputTokens
		row.OutputTokens += item.CompletionTokens
		row.CacheTokens += item.CacheTokens
		row.CacheReadTokens += item.CacheReadTokens
		row.CacheWriteTokens += item.CacheWriteTokens
	}

	for i := range rows {
		rows[i].TotalTokens = rows[i].InputTokens + rows[i].OutputTokens + rows[i].CacheTokens + rows[i].CacheReadTokens + rows[i].CacheWriteTokens
	}

	return rows
}

func buildDashboardCacheHitRateByDay(series []*LogStatisticGroupModel, dateRange DashboardDateRange) []DashboardCacheHitRateDay {
	dates := buildDashboardDateList(dateRange)
	byDate := make(map[string]*DashboardCacheHitRateDay, len(dates))
	rows := make([]DashboardCacheHitRateDay, 0, len(dates))
	for _, date := range dates {
		row := DashboardCacheHitRateDay{
			Date: date,
			DashboardCacheHitRate: DashboardCacheHitRate{
				Models: make([]DashboardCacheHitRateModel, 0),
			},
		}
		rows = append(rows, row)
		byDate[date] = &rows[len(rows)-1]
	}

	for _, item := range series {
		if item == nil {
			continue
		}
		row, ok := byDate[item.Date]
		if !ok {
			continue
		}

		row.RequestCount += item.RequestCount
		row.CacheHitCount += item.CacheHitCount
	}

	for i := range rows {
		rows[i].HitRate = calculateCacheHitRate(rows[i].CacheHitCount, rows[i].RequestCount)
	}

	return rows
}

func normalizeInputTokens(promptTokens, cacheTokens, cacheReadTokens, cacheWriteTokens int64) int64 {
	inputTokens := promptTokens - cacheTokens - cacheReadTokens - cacheWriteTokens
	if inputTokens < 0 {
		return 0
	}
	return inputTokens
}

func calculateCacheHitRate(cacheHitCount, requestCount int64) float64 {
	if requestCount <= 0 {
		return 0
	}
	return float64(cacheHitCount) / float64(requestCount)
}

type MultiUserStatistic struct {
	Username         string `gorm:"column:username" json:"username"`
	ModelName        string `gorm:"column:model_name" json:"model_name"`
	RequestCount     int64  `gorm:"column:request_count" json:"request_count"`
	Quota            int64  `gorm:"column:quota" json:"quota"`
	PromptTokens     int64  `gorm:"column:prompt_tokens" json:"prompt_tokens"`
	CompletionTokens int64  `gorm:"column:completion_tokens" json:"completion_tokens"`
	RequestTime      int64  `gorm:"column:request_time" json:"request_time"`
}

// GetMultiUserStatisticsByPeriod 获取多个用户在指定时间段内的统计数据
func GetMultiUserStatisticsByPeriod(usernames []string, startTime, endTime string) ([]*MultiUserStatistic, error) {
	if len(usernames) == 0 {
		return nil, fmt.Errorf("usernames cannot be empty")
	}

	var statistics []*MultiUserStatistic

	// Build SQL query
	query := `
		SELECT 
			users.username,
			statistics.model_name,
			SUM(statistics.request_count) as request_count,
			SUM(statistics.quota) as quota,
			SUM(statistics.prompt_tokens) as prompt_tokens,
			SUM(statistics.completion_tokens) as completion_tokens,
			SUM(statistics.request_time) as request_time
		FROM statistics
		INNER JOIN users ON statistics.user_id = users.id
		WHERE users.username IN (?)
		AND statistics.date BETWEEN ? AND ?
		GROUP BY users.username, statistics.model_name
		ORDER BY users.username, statistics.model_name
	`

	err := DB.Raw(query, usernames, startTime, endTime).Scan(&statistics).Error
	if err != nil {
		return nil, err
	}

	return statistics, nil
}

// UserGroupedStatistic 按用户分组的统计数据(不按模型分组)
type UserGroupedStatistic struct {
	Username         string `gorm:"column:username" json:"username"`
	RequestCount     int64  `gorm:"column:request_count" json:"request_count"`
	Quota            int64  `gorm:"column:quota" json:"quota"`
	PromptTokens     int64  `gorm:"column:prompt_tokens" json:"prompt_tokens"`
	CompletionTokens int64  `gorm:"column:completion_tokens" json:"completion_tokens"`
	RequestTime      int64  `gorm:"column:request_time" json:"request_time"`
}

// ModelUsageByUser 按用户和模型分组的使用统计
type ModelUsageByUser struct {
	Username     string `gorm:"column:username" json:"username"`
	ModelName    string `gorm:"column:model_name" json:"model_name"`
	RequestCount int64  `gorm:"column:request_count" json:"request_count"`
}

// GetUserGroupedStatisticsByPeriod 获取按用户分组的统计数据(不按模型分组)
func GetUserGroupedStatisticsByPeriod(usernames []string, startTime, endTime string) ([]*UserGroupedStatistic, error) {
	if len(usernames) == 0 {
		return nil, fmt.Errorf("usernames cannot be empty")
	}

	var statistics []*UserGroupedStatistic

	// Build SQL query - group by username only
	query := `
		SELECT 
			users.username,
			SUM(statistics.request_count) as request_count,
			SUM(statistics.quota) as quota,
			SUM(statistics.prompt_tokens) as prompt_tokens,
			SUM(statistics.completion_tokens) as completion_tokens,
			SUM(statistics.request_time) as request_time
		FROM statistics
		INNER JOIN users ON statistics.user_id = users.id
		WHERE users.username IN (?)
		AND statistics.date BETWEEN ? AND ?
		GROUP BY users.username
		ORDER BY users.username
	`

	err := DB.Raw(query, usernames, startTime, endTime).Scan(&statistics).Error
	if err != nil {
		return nil, err
	}

	return statistics, nil
}

// GetModelUsageByUser 获取每个用户使用不同模型的调用次数
func GetModelUsageByUser(usernames []string, startTime, endTime string) ([]*ModelUsageByUser, error) {
	if len(usernames) == 0 {
		return nil, fmt.Errorf("usernames cannot be empty")
	}

	var usage []*ModelUsageByUser

	query := `
		SELECT 
			users.username,
			statistics.model_name,
			SUM(statistics.request_count) as request_count
		FROM statistics
		INNER JOIN users ON statistics.user_id = users.id
		WHERE users.username IN (?)
		AND statistics.date BETWEEN ? AND ?
		GROUP BY users.username, statistics.model_name
		ORDER BY users.username, request_count DESC
	`

	err := DB.Raw(query, usernames, startTime, endTime).Scan(&usage).Error
	if err != nil {
		return nil, err
	}

	return usage, nil
}

func GetChannelExpensesStatisticsByPeriod(startTime, endTime, groupType string, userID int) (LogStatistics []*LogStatisticGroupChannel, err error) {

	var whereClause strings.Builder
	whereClause.WriteString("WHERE date BETWEEN ? AND ?")
	args := []interface{}{startTime, endTime}

	if userID > 0 {
		whereClause.WriteString(" AND user_id = ?")
		args = append(args, userID)
	}

	dateStr := "date"
	if common.UsingPostgreSQL {
		dateStr = "TO_CHAR(date, 'YYYY-MM-DD') as date"
	} else if common.UsingSQLite {
		dateStr = "strftime('%%Y-%%m-%%d', date) as date"
	}

	baseSelect := `
        SELECT ` + dateStr + `,
        sum(request_count) as request_count,
        sum(quota) as quota,
        sum(prompt_tokens) as prompt_tokens,
        sum(completion_tokens) as completion_tokens,
        sum(cache_tokens) as cache_tokens,
        sum(cache_read_tokens) as cache_read_tokens,
        sum(cache_write_tokens) as cache_write_tokens,
        sum(cache_hit_count) as cache_hit_count,
        sum(request_time) as request_time,`

	var sql string
	if groupType == "model" {
		sql = baseSelect + `
            model_name as channel
            FROM statistics
            %s
            GROUP BY date, model_name
            ORDER BY date, model_name`
	} else if groupType == "model_type" {
		sql = baseSelect + `
            model_owned_by.name as channel
            FROM statistics
            JOIN prices ON statistics.model_name = prices.model
			JOIN model_owned_by ON prices.channel_type = model_owned_by.id
            %s
            GROUP BY date, model_owned_by.name
            ORDER BY date, model_owned_by.name`

	} else {
		sql = baseSelect + `
            MAX(channels.name) as channel
            FROM statistics
            JOIN channels ON statistics.channel_id = channels.id
            %s
            GROUP BY date, channel_id
            ORDER BY date, channel_id`
	}

	sql = fmt.Sprintf(sql, whereClause.String())
	err = DB.Raw(sql, args...).Scan(&LogStatistics).Error
	if err != nil {
		return nil, err
	}

	return LogStatistics, nil
}

type StatisticsUpdateType int

const (
	StatisticsUpdateTypeToDay     StatisticsUpdateType = 1
	StatisticsUpdateTypeYesterday StatisticsUpdateType = 2
	StatisticsUpdateTypeALL       StatisticsUpdateType = 3
)

func statisticsInsertPrefix() string {
	if common.UsingSQLite {
		return "INSERT OR REPLACE INTO"
	}
	return "INSERT INTO"
}

func statisticsDateExpression() string {
	if common.UsingSQLite {
		return "strftime('%Y-%m-%d', datetime(created_at, 'unixepoch', '+8 hours'))"
	}
	if common.UsingPostgreSQL {
		return "DATE_TRUNC('day', TO_TIMESTAMP(created_at))::DATE"
	}
	return "DATE_FORMAT(FROM_UNIXTIME(created_at), '%Y-%m-%d')"
}

func withStatisticsRebuildTransaction(tx *gorm.DB, rebuild func(*gorm.DB) error) error {
	if tx == nil {
		tx = DB
	}
	// Trade-off: readers may wait briefly on the rebuild transaction, but they never observe an empty replacement window.
	return tx.Transaction(func(innerTx *gorm.DB) error {
		return rebuild(innerTx)
	})
}

func rebuildAllStatistics(tx *gorm.DB) error {
	return withStatisticsRebuildTransaction(tx, func(innerTx *gorm.DB) error {
		if err := innerTx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&Statistics{}).Error; err != nil {
			return err
		}
		return insertStatisticsRange(innerTx, 0, 0)
	})
}

func rebuildStatisticsByCreatedAtRange(tx *gorm.DB, startTimestamp, endTimestamp int64) error {
	startDate := time.Unix(startTimestamp, 0).In(time.Local).Format("2006-01-02")
	endDate := startDate
	if endTimestamp > startTimestamp {
		endDate = time.Unix(endTimestamp-1, 0).In(time.Local).Format("2006-01-02")
	}
	return withStatisticsRebuildTransaction(tx, func(innerTx *gorm.DB) error {
		if err := innerTx.Where("date BETWEEN ? AND ?", startDate, endDate).Delete(&Statistics{}).Error; err != nil {
			return err
		}
		return insertStatisticsRange(innerTx, startTimestamp, endTimestamp)
	})
}

func insertStatisticsRange(tx *gorm.DB, startTimestamp, endTimestamp int64) error {
	if tx == nil {
		tx = DB
	}
	whereParts := []string{"type = ?"}
	args := []interface{}{LogTypeConsume}
	if startTimestamp > 0 {
		whereParts = append(whereParts, "created_at >= ?")
		args = append(args, startTimestamp)
	}
	if endTimestamp > 0 {
		whereParts = append(whereParts, "created_at < ?")
		args = append(args, endTimestamp)
	}

	sql := fmt.Sprintf(`
		%s statistics (
			date,
			user_id,
			channel_id,
			model_name,
			request_count,
			quota,
			prompt_tokens,
			completion_tokens,
			cache_tokens,
			cache_read_tokens,
			cache_write_tokens,
			cache_hit_count,
			request_time
		)
		SELECT
			%s as date,
			user_id,
			channel_id,
			model_name,
			count(1) as request_count,
			sum(quota) as quota,
			sum(prompt_tokens) as prompt_tokens,
			sum(completion_tokens) as completion_tokens,
			sum(cache_tokens) as cache_tokens,
			sum(cache_read_tokens) as cache_read_tokens,
			sum(cache_write_tokens) as cache_write_tokens,
			sum(case when cache_tokens > 0 or cache_read_tokens > 0 then 1 else 0 end) as cache_hit_count,
			sum(request_time) as request_time
		FROM logs
		WHERE %s
		GROUP BY date, channel_id, user_id, model_name
		ORDER BY date, model_name
	`, statisticsInsertPrefix(), statisticsDateExpression(), strings.Join(whereParts, " AND "))

	return tx.Exec(sql, args...).Error
}

func UpdateStatistics(updateType StatisticsUpdateType) error {
	now := time.Now()
	todayTimestamp := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()

	switch updateType {
	case StatisticsUpdateTypeToDay:
		return rebuildStatisticsByCreatedAtRange(nil, todayTimestamp, todayTimestamp+86400)
	case StatisticsUpdateTypeYesterday:
		yesterdayTimestamp := todayTimestamp - 86400
		return rebuildStatisticsByCreatedAtRange(nil, yesterdayTimestamp, todayTimestamp)
	case StatisticsUpdateTypeALL:
		return rebuildAllStatistics(nil)
	default:
		return rebuildAllStatistics(nil)
	}
}
