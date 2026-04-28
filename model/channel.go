package model

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/utils"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Channel struct {
	Id                 int     `json:"id"`
	Type               int     `json:"type" form:"type" gorm:"default:0"`
	Key                string  `json:"key" form:"key" gorm:"type:text"`
	Status             int     `json:"status" form:"status" gorm:"default:1"`
	Name               string  `json:"name" form:"name" gorm:"index"`
	Weight             *uint   `json:"weight" gorm:"default:1"`
	CreatedTime        int64   `json:"created_time" gorm:"bigint"`
	TestTime           int64   `json:"test_time" gorm:"bigint"`
	ResponseTime       int     `json:"response_time"` // in milliseconds
	BaseURL            *string `json:"base_url" gorm:"column:base_url;default:''"`
	Other              string  `json:"other" form:"other"`
	Balance            float64 `json:"balance"` // in USD
	BalanceUpdatedTime int64   `json:"balance_updated_time" gorm:"bigint"`
	Models             string  `json:"models" form:"models"`
	Group              string  `json:"group" form:"group" gorm:"type:varchar(32);default:'default'"`
	Tag                string  `json:"tag" form:"tag" gorm:"type:varchar(32);default:''"`
	UsedQuota          int64   `json:"used_quota" gorm:"bigint;default:0"`
	ModelMapping       *string `json:"model_mapping" gorm:"type:text"`
	ModelHeaders       *string `json:"model_headers" gorm:"type:varchar(1024);default:''"`
	CustomParameter    *string `json:"custom_parameter" gorm:"type:varchar(1024);default:''"`
	Priority           *int64  `json:"priority" gorm:"bigint;default:0"`
	Proxy              *string `json:"proxy" gorm:"type:varchar(255);default:''"`
	TestModel          string  `json:"test_model" form:"test_model" gorm:"type:varchar(50);default:''"`
	OnlyChat           bool    `json:"only_chat" form:"only_chat" gorm:"default:false"`
	PreCost            int     `json:"pre_cost" form:"pre_cost" gorm:"default:1"`
	CompatibleResponse bool    `json:"compatible_response" gorm:"default:false"`
	AllowExtraBody     bool    `json:"allow_extra_body" form:"allow_extra_body" gorm:"default:false"`

	DisabledStream *datatypes.JSONSlice[string] `json:"disabled_stream,omitempty" gorm:"type:json"`

	Plugin    *datatypes.JSONType[PluginType] `json:"plugin" form:"plugin" gorm:"type:json"`
	DeletedAt gorm.DeletedAt                  `json:"-" gorm:"index"`

	parsedModelMapping    map[string]string          `json:"-" gorm:"-"`
	parsedModelHeaders    map[string]string          `json:"-" gorm:"-"`
	parsedCustomParameter map[string]interface{}     `json:"-" gorm:"-"`
	parsedOther           map[string]json.RawMessage `json:"-" gorm:"-"`
	modelMappingErr       error                      `json:"-" gorm:"-"`
	modelHeadersErr       error                      `json:"-" gorm:"-"`
	customParameterErr    error                      `json:"-" gorm:"-"`
	otherErr              error                      `json:"-" gorm:"-"`
	runtimeConfigParsed   bool                       `json:"-" gorm:"-"`
	lastModelMapping      string                     `json:"-" gorm:"-"`
	lastModelHeaders      string                     `json:"-" gorm:"-"`
	lastCustomParameter   string                     `json:"-" gorm:"-"`
	lastOther             string                     `json:"-" gorm:"-"`
}

func (c *Channel) AllowStream(modelName string) bool {
	if c.DisabledStream == nil {
		return true
	}

	return !slices.Contains(*c.DisabledStream, modelName)
}

type PluginType map[string]map[string]interface{}

const (
	customClaudePluginKey        = "claude"
	customClaudeEnabledPluginKey = "enabled"
	customClaudeBaseURLPluginKey = "base_url"
)

var allowedChannelOrderFields = map[string]bool{
	"id":            true,
	"name":          true,
	"group":         true,
	"type":          true,
	"status":        true,
	"response_time": true,
	"balance":       true,
	"priority":      true,
	"weight":        true,
}

var loadChannelByIDForChannelGroupRefresh = GetChannelById

const (
	codexTokenCacheKeyPrefix        = "api_token:codex"
	codexUsagePreviewCacheKeyPrefix = "codex:usage:preview"
	codexUsageDetailCacheKeyPrefix  = "codex:usage:detail"
)

type SearchChannelsParams struct {
	Channel
	PaginationParams
	FilterTag int `json:"filter_tag" form:"filter_tag"`
}

func GetChannelsList(params *SearchChannelsParams) (*DataResult[Channel], error) {
	var channels []*Channel

	db := DB.Omit("key")
	tagDB := DB.Model(&Channel{}).Select("Max(id) as id").Where("tag != ''").Group("tag")

	if params.Type != 0 {
		db = db.Where("type = ?", params.Type)
		tagDB = tagDB.Where("type = ?", params.Type)
	}

	if params.Status != 0 {
		db = db.Where("status = ?", params.Status)
		tagDB = tagDB.Where("status = ?", params.Status)
	}

	if params.Name != "" {
		db = db.Where("name LIKE ?", "%"+params.Name+"%")
		tagDB = tagDB.Where("tag LIKE ?", "%"+params.Name+"%")
	}

	if params.Group != "" {
		groupKey := quotePostgresField("group")
		db = db.Where("( "+groupKey+" LIKE ? OR "+groupKey+" LIKE ? OR "+groupKey+" LIKE ? OR "+groupKey+" = ?)",
			"%,"+params.Group+",%", params.Group+",%", "%,"+params.Group, params.Group)
		tagDB = tagDB.Where("( "+groupKey+" LIKE ? OR "+groupKey+" LIKE ? OR "+groupKey+" LIKE ? OR "+groupKey+" = ?)",
			"%,"+params.Group+",%", params.Group+",%", "%,"+params.Group, params.Group)
	}

	if params.Models != "" {
		db = db.Where("models LIKE ?", "%"+params.Models+"%")
		tagDB = tagDB.Where("models LIKE ?", "%"+params.Models+"%")
	}

	if params.Other != "" {
		db = db.Where("other LIKE ?", params.Other+"%")
		tagDB = tagDB.Where("other LIKE ?", params.Other+"%")
	}

	if params.Key != "" {
		db = db.Where(quotePostgresField("key")+" = ?", params.Key)
		tagDB = tagDB.Where(quotePostgresField("key")+" = ?", params.Key)
	}

	if params.TestModel != "" {
		db = db.Where("test_model LIKE ?", params.TestModel+"%")
		tagDB = tagDB.Where("test_model LIKE ?", params.TestModel+"%")
	}

	if params.Tag != "" {
		db = db.Where("tag = ?", params.Tag)
		tagDB = tagDB.Where("tag = ?", params.Tag)
	}

	switch params.FilterTag {
	case 1:
		db = db.Where("tag = ''")
	case 2:
		db = db.Where("id IN (?)", tagDB)
	default:
		db = db.Where("tag = '' OR id IN (?)", tagDB)
	}

	return PaginateAndOrder(db, &params.PaginationParams, &channels, allowedChannelOrderFields)
}

func GetAllChannels() ([]*Channel, error) {
	var channels []*Channel
	err := DB.Order("id desc").Find(&channels).Error
	return channels, err
}

func GetChannelsByTypeAndStatus(channelType int, status int) ([]*Channel, error) {
	var channels []*Channel
	err := DB.Where("type = ? AND status = ?", channelType, status).Order("id desc").Find(&channels).Error
	return channels, err
}

func GetChannelsByStatus(status int) ([]*Channel, error) {
	var channels []*Channel
	err := DB.Where("status = ?", status).Order("id desc").Find(&channels).Error
	return channels, err
}

func GetChannelsByIDs(ids []int) ([]*Channel, error) {
	var channels []*Channel
	if len(ids) == 0 {
		return channels, nil
	}
	err := DB.Where("id IN ?", ids).Find(&channels).Error
	return channels, err
}

func GetChannelById(id int) (*Channel, error) {
	channel := Channel{Id: id}
	err := DB.First(&channel, "id = ?", id).Error

	return &channel, err
}

func GetChannelsByTag(tag string) ([]*Channel, error) {
	var channels []*Channel
	err := DB.Where("tag = ?", tag).Find(&channels).Error
	return channels, err
}

func DeleteChannelTag(channelId int) error {
	err := DB.Model(&Channel{}).Where("id = ?", channelId).Update("tag", "").Error
	return err
}

func codexChannelIDsFromRows(channels []Channel) []int {
	channelIDs := make([]int, 0, len(channels))
	for _, channel := range channels {
		if channel.Type == config.ChannelTypeCodex && channel.Id > 0 {
			channelIDs = append(channelIDs, channel.Id)
		}
	}
	return channelIDs
}

func channelIDsFromRows(channels []Channel) []int {
	channelIDs := make([]int, 0, len(channels))
	for _, channel := range channels {
		if channel.Id > 0 {
			channelIDs = append(channelIDs, channel.Id)
		}
	}
	return channelIDs
}

func refreshChannelGroupAfterMutation(reason string, failClosedChannelIDs []int) {
	// Cache-aside trade-off: a post-commit rebuild keeps routing accurate without
	// coupling API success to an in-memory refresh. Destructive/status mutations
	// fail closed in the old snapshot first. Non-lifecycle route edits remain
	// eventually consistent if the reload fails; keeping that trade-off explicit
	// avoids model/group-scoped tombstones for a low-frequency admin path.
	//
	// What this guarantees: if this process disables/deletes a channel, it stops
	// routing to that channel immediately, even when the following DB reload fails.
	// That preserves sibling-channel fallback in the same tag/group/model.
	//
	// What it does not guarantee: cross-node instant consistency, or instant removal
	// of model/group/tag route edits when the DB read fails. Those stronger
	// guarantees require distributed coordination or route tombstones with expiry and
	// versioning, which are more complex than the original failure justifies.
	ChannelGroup.failClosedChannels(failClosedChannelIDs)
	if err := ChannelGroup.Load(); err != nil {
		logger.SysError(fmt.Sprintf("failed to refresh channel group after %s; routing snapshot marked dirty: %s", reason, err.Error()))
	}
}

func deleteChannelsMatching(scope func(*gorm.DB) *gorm.DB) (int64, error) {
	tx := DB.Begin()
	if tx.Error != nil {
		return 0, tx.Error
	}

	var channels []Channel
	if err := scope(tx.Model(&Channel{})).Select("id", "type").Find(&channels).Error; err != nil {
		tx.Rollback()
		return 0, err
	}

	result := scope(tx).Delete(&Channel{})
	if result.Error != nil {
		tx.Rollback()
		return 0, result.Error
	}

	if err := tx.Commit().Error; err != nil {
		return 0, err
	}

	// Delete-path invalidation is not a trade-off; it is a lifecycle invariant.
	// These derived caches are keyed by channel id, so leaving them behind risks a
	// later channel with the same id inheriting another account's cached Codex data.
	ClearChannelCodexDerivedCaches(codexChannelIDsFromRows(channels))
	refreshChannelGroupAfterMutation("delete channels", channelIDsFromRows(channels))

	return result.RowsAffected, nil
}

func BatchDeleteChannel(ids []int) (int64, error) {
	return deleteChannelsMatching(func(db *gorm.DB) *gorm.DB {
		return db.Where("id IN ?", ids)
	})
}

func BatchInsertChannels(channels []Channel) error {
	for i := range channels {
		if err := channels[i].ValidateRuntimeConfigJSON(); err != nil {
			return err
		}
	}
	err := DB.Omit("UsedQuota").Create(&channels).Error
	if err != nil {
		return err
	}

	refreshChannelGroupAfterMutation("insert channels", nil)
	return nil
}

type BatchChannelsParams struct {
	Value string `json:"value" form:"value" binding:"required"`
	Ids   []int  `json:"ids" form:"ids" binding:"required"`
}

func BatchUpdateChannelsAzureApi(params *BatchChannelsParams) (int64, error) {
	var channels []Channel
	if err := DB.Select("id, type").Find(&channels, "id IN ?", params.Ids).Error; err != nil {
		return 0, err
	}
	codexChannelIDs := make([]int, 0, len(channels))
	for i := range channels {
		channel := Channel{
			Type:  channels[i].Type,
			Other: params.Value,
		}
		if err := channel.ValidateRuntimeConfigJSON(); err != nil {
			return 0, err
		}
		if channels[i].Type == config.ChannelTypeCodex {
			codexChannelIDs = append(codexChannelIDs, channels[i].Id)
		}
	}

	db := DB.Model(&Channel{}).Where("id IN ?", params.Ids).Update("other", params.Value)
	if db.Error != nil {
		return 0, db.Error
	}

	if db.RowsAffected > 0 {
		ClearChannelCodexDerivedCaches(codexChannelIDs)
		refreshChannelGroupAfterMutation("batch update channel runtime config", nil)
	}
	return db.RowsAffected, nil
}

func BatchDelModelChannels(params *BatchChannelsParams) (int64, error) {
	var count int64

	var channels []*Channel
	err := DB.Select("id, models, "+quotePostgresField("group")).Find(&channels, "id IN ?", params.Ids).Error
	if err != nil {
		return 0, err
	}

	for _, channel := range channels {
		modelsSlice := strings.Split(channel.Models, ",")
		for i, m := range modelsSlice {
			if m == params.Value {
				modelsSlice = append(modelsSlice[:i], modelsSlice[i+1:]...)
				break
			}
		}

		channel.Models = strings.Join(modelsSlice, ",")
		channel.UpdateRaw(false)
		count++
	}

	if count > 0 {
		refreshChannelGroupAfterMutation("batch delete channel model", nil)
	}

	return count, nil
}

func (c *Channel) SetProxy() {
	if c.Proxy == nil {
		return
	}

	if strings.Contains(*c.Proxy, "%s") {
		md5Str := md5.Sum([]byte(c.Key))
		idStr := hex.EncodeToString(md5Str[:])
		*c.Proxy = strings.Replace(*c.Proxy, "%s", idStr, 1)
	}

}

func (channel *Channel) GetPriority() int64 {
	if channel.Priority == nil {
		return 0
	}
	return *channel.Priority
}

func (channel *Channel) GetBaseURL() string {
	if channel.BaseURL == nil {
		return ""
	}
	return *channel.BaseURL
}

func (channel *Channel) CustomClaudeRelayEnabled() bool {
	if channel == nil || channel.Type != config.ChannelTypeCustom || channel.Plugin == nil {
		return false
	}

	claudeConfig, ok := channel.Plugin.Data()[customClaudePluginKey]
	if !ok || claudeConfig == nil {
		return false
	}

	enabled, ok := claudeConfig[customClaudeEnabledPluginKey].(bool)
	return ok && enabled
}

func (channel *Channel) ResolveCustomClaudeBaseURL(defaultBaseURL string) (string, error) {
	if channel == nil {
		return "", fmt.Errorf("channel is nil")
	}
	if channel.Type != config.ChannelTypeCustom {
		return "", fmt.Errorf("channel is not a custom channel")
	}
	if !channel.CustomClaudeRelayEnabled() {
		return "", fmt.Errorf("plugin.claude.enabled must be true for Claude relay")
	}

	baseURL := ""
	baseURLField := "plugin.claude.base_url"
	if channel.Plugin != nil {
		if claudeConfig, ok := channel.Plugin.Data()[customClaudePluginKey]; ok && claudeConfig != nil {
			if rawBaseURL, exists := claudeConfig[customClaudeBaseURLPluginKey]; exists && rawBaseURL != nil {
				value, ok := rawBaseURL.(string)
				if !ok {
					return "", fmt.Errorf("plugin.claude.base_url must be a string")
				}
				baseURL = strings.TrimSpace(value)
			}
		}
	}

	if baseURL == "" {
		baseURL = strings.TrimSpace(channel.GetBaseURL())
		baseURLField = "base_url"
	}
	if baseURL == "" {
		baseURL = strings.TrimSpace(defaultBaseURL)
		baseURLField = "plugin.claude.default_base_url"
	}
	if baseURL == "" {
		return "", nil
	}

	return normalizeClaudeBaseURL(baseURLField, baseURL)
}

func normalizeClaudeBaseURL(fieldName, rawBaseURL string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(rawBaseURL), "/")
	if trimmed == "" {
		return "", nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("%s must be an absolute http(s) base URL", fieldName)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%s must use http or https", fieldName)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s must not include query or fragment", fieldName)
	}

	// Keep path suffixes exactly as configured. The Claude provider appends its
	// endpoint path later, so users must supply the upstream base path they mean
	// instead of relying on this layer to correct duplicated endpoint segments.
	return trimmed, nil
}

func (channel *Channel) GetModelMapping() string {
	if channel.ModelMapping == nil {
		return ""
	}
	return *channel.ModelMapping
}

func (channel *Channel) GetModelMappingMap() (map[string]string, error) {
	channel.ensureRuntimeConfigParsed()
	if channel.modelMappingErr != nil {
		return nil, channel.modelMappingErr
	}
	return channel.parsedModelMapping, nil
}

func (channel *Channel) GetCustomParameter() string {
	if channel.CustomParameter == nil {
		return ""
	}
	return *channel.CustomParameter
}

func (channel *Channel) GetCustomParameterMap() (map[string]interface{}, error) {
	channel.ensureRuntimeConfigParsed()
	if channel.customParameterErr != nil {
		return nil, channel.customParameterErr
	}
	return channel.parsedCustomParameter, nil
}

func (channel *Channel) GetOtherMap() (map[string]json.RawMessage, error) {
	channel.ensureRuntimeConfigParsed()
	if channel.otherErr != nil {
		return nil, channel.otherErr
	}
	return channel.parsedOther, nil
}

func (channel *Channel) GetModelHeadersMap() (map[string]string, error) {
	channel.ensureRuntimeConfigParsed()
	if channel.modelHeadersErr != nil {
		return nil, channel.modelHeadersErr
	}
	return channel.parsedModelHeaders, nil
}

func (channel *Channel) ensureRuntimeConfigParsed() {
	// Not goroutine-safe; call this before the channel is shared across request goroutines.
	if channel == nil {
		return
	}

	modelHeaders := ""
	if channel.ModelHeaders != nil {
		modelHeaders = *channel.ModelHeaders
	}

	modelMapping := channel.GetModelMapping()
	customParameter := channel.GetCustomParameter()
	other := strings.TrimSpace(channel.Other)

	if channel.runtimeConfigParsed &&
		channel.lastModelMapping == modelMapping &&
		channel.lastModelHeaders == modelHeaders &&
		channel.lastCustomParameter == customParameter &&
		channel.lastOther == other {
		return
	}

	channel.ParseRuntimeConfig()
}

func (channel *Channel) ParseRuntimeConfig() {
	modelMapping := channel.GetModelMapping()
	modelHeaders := ""
	if channel.ModelHeaders != nil {
		modelHeaders = *channel.ModelHeaders
	}
	customParameter := channel.GetCustomParameter()
	other := strings.TrimSpace(channel.Other)

	channel.parsedModelMapping = nil
	channel.parsedModelHeaders = nil
	channel.parsedCustomParameter = nil
	channel.parsedOther = nil
	channel.modelMappingErr = nil
	channel.modelHeadersErr = nil
	channel.customParameterErr = nil
	channel.otherErr = nil
	channel.runtimeConfigParsed = true
	channel.lastModelMapping = modelMapping
	channel.lastModelHeaders = modelHeaders
	channel.lastCustomParameter = customParameter
	channel.lastOther = other

	if modelMapping != "" && modelMapping != "{}" {
		modelMap := make(map[string]string)
		if err := json.Unmarshal([]byte(modelMapping), &modelMap); err != nil {
			channel.modelMappingErr = err
		} else {
			channel.parsedModelMapping = modelMap
		}
	}

	if modelHeaders != "" && modelHeaders != "{}" {
		headers := make(map[string]string)
		if err := json.Unmarshal([]byte(modelHeaders), &headers); err != nil {
			channel.modelHeadersErr = err
		} else {
			channel.parsedModelHeaders = headers
		}
	}

	if customParameter != "" && customParameter != "{}" {
		customParams := make(map[string]interface{})
		if err := json.Unmarshal([]byte(customParameter), &customParams); err != nil {
			channel.customParameterErr = err
		} else {
			channel.parsedCustomParameter = customParams
		}
	}

	if other != "" && other != "{}" {
		otherMap := make(map[string]json.RawMessage)
		if err := json.Unmarshal([]byte(other), &otherMap); err != nil {
			channel.otherErr = err
		} else {
			channel.parsedOther = otherMap
		}
	}
}

func (channel *Channel) Insert() error {
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		return err
	}
	err := DB.Omit("UsedQuota").Create(channel).Error
	if err == nil {
		refreshChannelGroupAfterMutation("insert channel", nil)
	}

	return err
}

func (channel *Channel) Update(overwrite bool) error {

	err := channel.UpdateRaw(overwrite)

	if err == nil {
		var failClosedChannelIDs []int
		if channel.Status != 0 && channel.Status != config.ChannelStatusEnabled {
			failClosedChannelIDs = []int{channel.Id}
		}
		refreshChannelGroupAfterMutation("update channel", failClosedChannelIDs)
	}

	return err
}

func (channel *Channel) UpdateRaw(overwrite bool) error {
	var err error
	if err = channel.hydratePersistedTypeForUpdate(); err != nil {
		return err
	}
	if err = channel.ValidateRuntimeConfigJSON(); err != nil {
		return err
	}

	if overwrite {
		err = DB.Model(channel).Select("*").Omit("UsedQuota").Updates(channel).Error
	} else {
		err = DB.Model(channel).Omit("UsedQuota").Updates(channel).Error
	}
	if err != nil {
		return err
	}
	ClearChannelCodexDerivedCache(channel.Id)
	DB.Model(channel).First(channel, "id = ?", channel.Id)
	return err
}

func (channel *Channel) hydratePersistedTypeForUpdate() error {
	if channel == nil || channel.Id <= 0 || channel.Type != config.ChannelTypeUnknown {
		return nil
	}

	persisted, err := GetChannelById(channel.Id)
	if err != nil {
		return err
	}

	channel.Type = persisted.Type
	return nil
}

func (channel *Channel) UpdateResponseTime(responseTime int64) {
	err := DB.Model(channel).Select("response_time", "test_time").Updates(Channel{
		TestTime:     utils.GetTimestamp(),
		ResponseTime: int(responseTime),
	}).Error
	if err != nil {
		logger.SysError("failed to update response time: " + err.Error())
	}
}

func (channel *Channel) UpdateBalance(balance float64) {
	err := DB.Model(channel).Select("balance_updated_time", "balance").Updates(Channel{
		BalanceUpdatedTime: utils.GetTimestamp(),
		Balance:            balance,
	}).Error
	if err != nil {
		logger.SysError("failed to update balance: " + err.Error())
	}
}

func (channel *Channel) Delete() error {
	_, err := deleteChannelsMatching(func(db *gorm.DB) *gorm.DB {
		return db.Where("id = ?", channel.Id)
	})
	return err
}

func (channel *Channel) StatusToStr() string {
	switch channel.Status {
	case config.ChannelStatusEnabled:
		return "启用"
	case config.ChannelStatusAutoDisabled:
		return "自动禁用"
	case config.ChannelStatusManuallyDisabled:
		return "手动禁用"
	}

	return "禁用"
}

func updateChannelStatus(id int, targetStatus int, applyScope func(*gorm.DB) *gorm.DB) (bool, error) {
	tx := DB.Begin()
	if tx.Error != nil {
		logger.SysError("failed to begin channel status update transaction: " + tx.Error.Error())
		return false, tx.Error
	}

	query := tx.Model(&Channel{}).Where("id = ?", id)
	if applyScope != nil {
		query = applyScope(query)
	}

	result := query.Update("status", targetStatus)
	if result.Error != nil {
		logger.SysError("failed to update channel status: " + result.Error.Error())
		tx.Rollback()
		return false, result.Error
	}

	if err := tx.Commit().Error; err != nil {
		logger.SysError("failed to commit channel status update: " + err.Error())
		return false, err
	}

	updated := result.RowsAffected > 0
	if updated {
		// Status changes are routing-index changes, not just per-channel flags.
		// A disabled channel may be absent from Channels after a prior Load, and an
		// enabled channel may be the last member of a group/model rule. Full reload is
		// deliberately cheaper and safer than maintaining Rule/Match/ModelGroup
		// incrementally on this low-frequency lifecycle path.
		//
		// Trade-off: disabling fails closed immediately; enabling waits for a
		// successful reload instead of locally patching every routing index. That can
		// delay re-enable during DB read failures, but avoids a second incremental
		// routing implementation that can diverge from Load().
		var failClosedChannelIDs []int
		if targetStatus != config.ChannelStatusEnabled {
			failClosedChannelIDs = []int{id}
		}
		refreshChannelGroupAfterMutation("update channel status", failClosedChannelIDs)
	}

	return updated, nil
}

func UpdateChannelStatusById(id int, status int) {
	if _, err := updateChannelStatus(id, status, nil); err != nil {
		return
	}
}

// Automated probe results must not override an operator's manual state change.
// Compare-and-set keeps that trade-off explicit: we may skip a stale recovery/disable
// result, but we never silently rewrite a newer status chosen by an admin.
func UpdateChannelStatusIfCurrent(id int, currentStatus int, targetStatus int) (bool, error) {
	return updateChannelStatus(id, targetStatus, func(db *gorm.DB) *gorm.DB {
		return db.Where("status = ?", currentStatus)
	})
}

func UpdateChannelUsedQuota(id int, quota int) {
	if config.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeChannelUsedQuota, id, quota)
		return
	}
	updateChannelUsedQuota(id, quota)
}

func updateChannelUsedQuota(id int, quota int) {
	err := DB.Model(&Channel{}).Where("id = ?", id).Update("used_quota", gorm.Expr("used_quota + ?", quota)).Error
	if err != nil {
		logger.SysError("failed to update channel used quota: " + err.Error())
	}
}

func clearChannelCacheKeys(cacheKeys []string) {
	for _, key := range cacheKeys {
		if err := cache.DeleteCache(key); err != nil {
			logger.SysError(fmt.Sprintf("failed to clear cache %s: %v", key, err))
		}
	}
}

func ClearChannelTokenCache(channelId int) {
	cacheKeys := []string{
		fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelId),
	}

	clearChannelCacheKeys(cacheKeys)
}

func ClearChannelCodexUsageCache(channelId int) {
	cacheKeys := []string{
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelId),
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelId),
	}

	clearChannelCacheKeys(cacheKeys)
}

func ClearChannelCodexDerivedCaches(channelIds []int) {
	if len(channelIds) == 0 {
		return
	}

	cacheKeys := make([]string, 0, len(channelIds)*3)
	seen := make(map[int]struct{}, len(channelIds))
	for _, channelId := range channelIds {
		if channelId <= 0 {
			continue
		}
		if _, ok := seen[channelId]; ok {
			continue
		}
		seen[channelId] = struct{}{}
		// We intentionally over-invalidate Codex derived caches here. Usage data depends
		// on runtime credentials plus request-shaping fields such as baseURL, proxy,
		// model headers, and other provider options. A 1-minute cache miss is cheaper
		// than trying to diff those fields precisely and serving stale admin usage data.
		cacheKeys = append(cacheKeys,
			fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelId),
			fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelId),
			fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelId),
		)
	}

	clearChannelCacheKeys(cacheKeys)
}

func ClearChannelCodexDerivedCache(channelId int) {
	ClearChannelCodexDerivedCaches([]int{channelId})
}

func UpdateChannelKey(id int, key string) error {
	err := DB.Model(&Channel{}).Where("id = ?", id).Update("key", key).Error
	if err != nil {
		logger.SysError("failed to update channel key: " + err.Error())
		return err
	}

	ClearChannelCodexDerivedCache(id)
	if err := ChannelGroup.RefreshChannel(id); err != nil {
		ChannelGroup.markDirty()
		logger.SysError("failed to refresh channel state after key update: " + err.Error())
	}

	return nil
}

func DeleteDisabledChannel() (int64, error) {
	return deleteChannelsMatching(func(db *gorm.DB) *gorm.DB {
		return db.Where("status = ? or status = ?", config.ChannelStatusAutoDisabled, config.ChannelStatusManuallyDisabled)
	})
}

type ChannelStatistics struct {
	TotalChannels int `json:"total_channels"`
	Status        int `json:"status"`
}

func GetStatisticsChannel() (statistics []*ChannelStatistics, err error) {
	err = DB.Model(&Channel{}).Select("count(*) as total_channels, status").Group("status").Scan(&statistics).Error
	return statistics, err
}
