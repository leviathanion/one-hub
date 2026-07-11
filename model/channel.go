package model

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

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
	BaseURL            *string `json:"base_url" gorm:"column:base_url;default:''" tag_config:"sync"`
	Other              string  `json:"other" form:"other" tag_config:"sync"`
	Balance            float64 `json:"balance"` // in USD
	BalanceUpdatedTime int64   `json:"balance_updated_time" gorm:"bigint"`
	Models             string  `json:"models" form:"models" tag_config:"sync"`
	Group              string  `json:"group" form:"group" gorm:"type:varchar(32);default:'default'" tag_config:"sync"`
	Tag                string  `json:"tag" form:"tag" gorm:"type:varchar(32);default:''" tag_config:"sync"`
	UsedQuota          int64   `json:"used_quota" gorm:"bigint;default:0"`
	ModelMapping       *string `json:"model_mapping" gorm:"type:text" tag_config:"sync"`
	ModelHeaders       *string `json:"model_headers" gorm:"type:varchar(1024);default:''" tag_config:"sync"`
	CustomParameter    *string `json:"custom_parameter" gorm:"type:varchar(1024);default:''" tag_config:"sync"`
	Priority           *int64  `json:"priority" gorm:"bigint;default:0"`
	Proxy              *string `json:"proxy" gorm:"type:varchar(255);default:''" tag_config:"sync"`
	TestModel          string  `json:"test_model" form:"test_model" gorm:"type:varchar(50);default:''" tag_config:"sync"`
	OnlyChat           bool    `json:"only_chat" form:"only_chat" gorm:"default:false" tag_config:"sync"`
	PreCost            int     `json:"pre_cost" form:"pre_cost" gorm:"default:1" tag_config:"sync"`
	CompatibleResponse bool    `json:"compatible_response" gorm:"default:false" tag_config:"sync"`
	AllowExtraBody     bool    `json:"allow_extra_body" form:"allow_extra_body" gorm:"default:false" tag_config:"sync"`

	DisabledStream *datatypes.JSONSlice[string] `json:"disabled_stream,omitempty" gorm:"type:json" tag_config:"sync"`

	Plugin    *datatypes.JSONType[PluginType] `json:"plugin" form:"plugin" gorm:"type:json" tag_config:"sync"`
	DeletedAt gorm.DeletedAt                  `json:"-" gorm:"index"`

	// CredentialRevision is the lifecycle identity of Key/Type/DeletedAt.  The
	// refresh fence deliberately lives beside the credential authority so claim,
	// commit and lifecycle supersession are single-row atomic operations.
	CredentialRevision         uint64  `json:"-" gorm:"column:credential_revision;not null;default:0"`
	CredentialRefreshFence     *string `json:"-" gorm:"column:credential_refresh_fence;type:varchar(36)"`
	CredentialRefreshStartedAt *int64  `json:"-" gorm:"column:credential_refresh_started_at"`
	CredentialRefreshState     string  `json:"credential_refresh_state,omitempty" gorm:"-"`

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

func (c *Channel) AfterFind(_ *gorm.DB) error {
	if c.CredentialRefreshFence == nil {
		c.CredentialRefreshState = "ready"
		return nil
	}
	c.CredentialRefreshState = "unresolved"
	if c.CredentialRefreshStartedAt != nil && time.Since(time.Unix(*c.CredentialRefreshStartedAt, 0)) <= 5*time.Second {
		c.CredentialRefreshState = "in_progress"
	}
	return nil
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

var azureAPIVersionSearchCandidateWarnThreshold = 1000

const (
	codexTokenCacheKeyPrefix           = "api_token:codex"
	codexUsagePreviewCacheKeyPrefix    = "codex:usage:preview"
	codexUsageDetailCacheKeyPrefix     = "codex:usage:detail"
	codexUsageGenerationCacheKeyPrefix = cache.CodexUsageGenerationKeyPrefix
)

type SearchChannelsParams struct {
	Channel
	PaginationParams
	FilterTag       int    `json:"filter_tag" form:"filter_tag"`
	AzureAPIVersion string `json:"azure_api_version" form:"azure_api_version"`
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

	if strings.TrimSpace(params.AzureAPIVersion) != "" {
		db = db.Where("type = ?", config.ChannelTypeAzure)
		tagDB = tagDB.Where("type = ?", config.ChannelTypeAzure)
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

	if strings.TrimSpace(params.AzureAPIVersion) != "" {
		return paginateAndOrderAzureAPIVersion(db, &params.PaginationParams, &channels, params.AzureAPIVersion)
	}
	return PaginateAndOrder(db, &params.PaginationParams, &channels, allowedChannelOrderFields)
}

func paginateAndOrderAzureAPIVersion(db *gorm.DB, params *PaginationParams, result *[]*Channel, apiVersion string) (*DataResult[Channel], error) {
	needle := strings.TrimSpace(apiVersion)
	if params.Page < 1 {
		params.Page = 1
	}
	if params.Size < 1 {
		params.Size = config.ItemsPerPage
	}
	if params.Size > config.MaxRecentItems {
		return nil, fmt.Errorf("size 参数不能超过 %d", config.MaxRecentItems)
	}
	db, err := applyChannelListOrder(db, params.Order)
	if err != nil {
		return nil, err
	}

	type azureAPIVersionCandidate struct {
		Id    int
		Other string
	}
	var candidates []azureAPIVersionCandidate
	if err := db.Model(&Channel{}).Select("id", "other").Find(&candidates).Error; err != nil {
		return nil, err
	}
	if len(candidates) > azureAPIVersionSearchCandidateWarnThreshold {
		logger.LogWarn(context.Background(), fmt.Sprintf("Azure api_version search scanning %d candidate row(s) for api_version=%q page=%d size=%d", len(candidates), needle, params.Page, params.Size))
	}
	filteredIDs := make([]int, 0, len(candidates))
	invalidSkipped := 0
	for _, channel := range candidates {
		options, err := ParseAzureChannelOther(channel.Other)
		if err != nil {
			invalidSkipped++
			continue
		}
		if strings.Contains(options.APIVersion, needle) {
			filteredIDs = append(filteredIDs, channel.Id)
		}
	}
	if invalidSkipped > 0 {
		logger.LogWarn(context.Background(), fmt.Sprintf("Azure api_version search skipped %d invalid Azure channel Other config(s) while filtering %d candidate(s) for %q", invalidSkipped, len(candidates), needle))
	}

	totalCount := int64(len(filteredIDs))
	offset := (params.Page - 1) * params.Size
	if offset >= len(filteredIDs) {
		*result = []*Channel{}
	} else {
		end := offset + params.Size
		if end > len(filteredIDs) {
			end = len(filteredIDs)
		}
		pageIDs := filteredIDs[offset:end]
		pageChannels := make([]*Channel, 0, len(pageIDs))
		if err := DB.Omit("key").Where("id IN ?", pageIDs).Find(&pageChannels).Error; err != nil {
			return nil, err
		}
		byID := make(map[int]*Channel, len(pageChannels))
		for _, channel := range pageChannels {
			if channel != nil {
				byID[channel.Id] = channel
			}
		}
		ordered := make([]*Channel, 0, len(pageIDs))
		for _, id := range pageIDs {
			if channel := byID[id]; channel != nil {
				ordered = append(ordered, channel)
			}
		}
		*result = ordered
	}
	return &DataResult[Channel]{
		Data:       result,
		Page:       params.Page,
		Size:       params.Size,
		TotalCount: totalCount,
	}, nil
}

func applyChannelListOrder(db *gorm.DB, order string) (*gorm.DB, error) {
	if order == "" {
		return db.Order("id DESC"), nil
	}
	orderFields := strings.Split(order, ",")
	for _, field := range orderFields {
		field = strings.TrimSpace(field)
		desc := strings.HasPrefix(field, "-")
		if desc {
			field = field[1:]
		}
		if !allowedChannelOrderFields[field] {
			return nil, fmt.Errorf("不允许对字段 '%s' 进行排序", field)
		}
		if desc {
			field = field + " DESC"
		}
		db = db.Order(field)
	}
	return db, nil
}

func GetAllChannels() ([]*Channel, error) {
	var channels []*Channel
	err := DB.Order("id desc").Find(&channels).Error
	return channels, err
}

func GetChannelsByTypeAndStatus(channelType int, status int) ([]*Channel, error) {
	return GetChannelsByTypeAndStatusWithContext(context.Background(), channelType, status)
}

func GetChannelsByTypeAndStatusWithContext(ctx context.Context, channelType int, status int) ([]*Channel, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var channels []*Channel
	err := DB.WithContext(ctx).Where("type = ? AND status = ?", channelType, status).Order("id desc").Find(&channels).Error
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
	return GetChannelByIdWithContext(context.Background(), id)
}

func GetChannelByIdWithContext(ctx context.Context, id int) (*Channel, error) {
	channel := Channel{Id: id}
	err := DB.WithContext(ctx).First(&channel, "id = ?", id).Error

	return &channel, err
}

func GetChannelsByTag(tag string) ([]*Channel, error) {
	var channels []*Channel
	err := DB.Where("tag = ?", tag).Find(&channels).Error
	return channels, err
}

func DeleteChannelTag(channelId int) error {
	result := DB.Model(&Channel{}).Where("id = ?", channelId).Update("tag", "")
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		refreshChannelGroupAfterMutation("delete channel tag", []int{channelId})
	}
	return nil
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
	// Route mutations fail closed in this process before any subsequent operation
	// can block. This trades brief unavailability for the stronger invariant that a
	// committed config change is never served from the old chooser snapshot.
	ChannelGroup.failClosedChannels(failClosedChannelIDs)
	loadChannelGroupAfterMutation(reason)
}

func loadChannelGroupAfterMutation(reason string) {
	if err := ChannelGroup.Load(); err != nil {
		logger.SysError(fmt.Sprintf("failed to refresh channel group after %s; routing snapshot marked dirty: %s", reason, err.Error()))
	}
}

var invalidateChannelCodexDerivedCaches = ClearChannelCodexDerivedCaches

// finishChannelRouteMutation has exactly one routing publication path:
// quarantine, then replace the complete chooser snapshot from durable state.
// Cache cleanup runs afterward as garbage collection. Content-addressed v2 keys
// provide isolation, so slow or failed cache I/O has no routing authority.
//
// The deliberate cost is one full DB snapshot build per admin mutation and an
// admin response that may still wait for best-effort cleanup after routing is
// already correct. Keeping cache I/O out of the publication condition is more
// important than minimizing mutation latency; never move cleanup before Load or
// restore a per-channel refresh as an optimization.
func finishChannelRouteMutation(reason string, affectedChannelIDs, codexChannelIDs []int) {
	refreshChannelGroupAfterMutation(reason, affectedChannelIDs)
	invalidateChannelCodexDerivedCaches(codexChannelIDs)
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

	// A soft delete is a new lifecycle incarnation.  Advance the generation in
	// the same transaction, but retain a non-empty fence: deleting a row cannot
	// make its possibly-consumed refresh token safe again.
	if err := scope(tx.Model(&Channel{})).UpdateColumn("credential_revision", gorm.Expr("credential_revision + 1")).Error; err != nil {
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

	finishChannelRouteMutation("delete channels", channelIDsFromRows(channels), codexChannelIDsFromRows(channels))

	return result.RowsAffected, nil
}

func BatchDeleteChannel(ids []int) (int64, error) {
	return deleteChannelsMatching(func(db *gorm.DB) *gorm.DB {
		return db.Where("id IN ?", ids)
	})
}

func BatchInsertChannels(channels []Channel) error {
	for i := range channels {
		if err := channels[i].CanonicalizeRuntimeConfigJSON(); err != nil {
			return err
		}
		if err := channels[i].ValidateRuntimeConfigJSON(); err != nil {
			return err
		}
	}

	if len(channels) == 0 {
		return nil
	}

	err := DB.Transaction(func(tx *gorm.DB) error {
		return tx.Omit("UsedQuota").CreateInBatches(&channels, 200).Error
	})
	if err != nil {
		return err
	}

	refreshChannelGroupAfterMutation("insert channels", nil)
	return nil
}

type BatchChannelsParams struct {
	APIVersion string `json:"api_version" form:"api_version"`
	Ids        []int  `json:"ids" form:"ids" binding:"required"`
}

type BatchDelModelChannelsParams struct {
	Value string `json:"value" form:"value" binding:"required"`
	Ids   []int  `json:"ids" form:"ids" binding:"required"`
}

func BatchUpdateChannelsAzureApi(params *BatchChannelsParams) (int64, error) {
	if params == nil {
		return 0, fmt.Errorf("params is required")
	}
	apiVersion := strings.TrimSpace(params.APIVersion)
	if apiVersion == "" {
		return 0, fmt.Errorf("api_version is required")
	}
	var channels []Channel
	if err := DB.Select("id, type, other").Find(&channels, "id IN ?", params.Ids).Error; err != nil {
		return 0, err
	}
	for i := range channels {
		if channels[i].Type != config.ChannelTypeAzure {
			return 0, fmt.Errorf("batch Azure api_version update only supports Azure classic channels")
		}
	}

	var rowsAffected int64
	var affectedIDs []int
	if err := DB.Transaction(func(tx *gorm.DB) error {
		for i := range channels {
			_, other, err := parseAzureChannelOtherObject(channels[i].Other)
			if err != nil {
				return err
			}
			encodedAPIVersion, err := json.Marshal(apiVersion)
			if err != nil {
				return err
			}
			other["api_version"] = encodedAPIVersion
			updated, err := json.Marshal(other)
			if err != nil {
				return err
			}
			result := tx.Model(&Channel{}).Where("id = ?", channels[i].Id).Update("other", string(updated))
			if result.Error != nil {
				return result.Error
			}
			rowsAffected += result.RowsAffected
			if result.RowsAffected > 0 {
				affectedIDs = append(affectedIDs, channels[i].Id)
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}

	if rowsAffected > 0 {
		refreshChannelGroupAfterMutation("batch update channel runtime config", affectedIDs)
	}
	return rowsAffected, nil
}

var errBatchDelModelChannelsConflict = errors.New("batch delete channel model concurrent update conflict")

func BatchDelModelChannels(params *BatchDelModelChannelsParams) (int64, error) {
	return BatchDelModelChannelsWithContext(context.Background(), params)
}

func BatchDelModelChannelsWithContext(ctx context.Context, params *BatchDelModelChannelsParams) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if params == nil {
		return 0, fmt.Errorf("params is required")
	}
	targetModel := strings.TrimSpace(params.Value)
	if targetModel == "" {
		return 0, fmt.Errorf("value is required")
	}
	var count int64
	var affectedIDs []int
	var affectedCodexIDs []int
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var channels []*Channel
		if err := tx.Select("id, type, models, "+quotePostgresField("group")).Find(&channels, "id IN ?", params.Ids).Error; err != nil {
			return err
		}

		for _, channel := range channels {
			modelsSlice := strings.Split(channel.Models, ",")
			remainingModels := modelsSlice[:0]
			removed := false
			for _, model := range modelsSlice {
				if strings.TrimSpace(model) == targetModel {
					removed = true
					continue
				}
				remainingModels = append(remainingModels, model)
			}
			if !removed {
				continue
			}

			oldModels := channel.Models
			channel.Models = strings.Join(remainingModels, ",")
			result := tx.Model(&Channel{}).
				Where("id = ? AND models = ?", channel.Id, oldModels).
				Update("models", channel.Models)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return fmt.Errorf("%w: channel %d models changed", errBatchDelModelChannelsConflict, channel.Id)
			}
			count++
			affectedIDs = append(affectedIDs, channel.Id)
			if channel.Type == config.ChannelTypeCodex {
				affectedCodexIDs = append(affectedCodexIDs, channel.Id)
			}
		}
		// A cancellation observed before commit must turn the callback into an
		// error so gorm rolls the transaction back and post-commit effects remain
		// strictly coupled to durable changes.
		return ctx.Err()
	})
	if err != nil {
		return 0, err
	}

	if count > 0 {
		finishChannelRouteMutation("batch delete channel model", affectedIDs, affectedCodexIDs)
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
	if err := channel.CanonicalizeRuntimeConfigJSON(); err != nil {
		return err
	}
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
	return channel.UpdateWithOptions(overwrite, ChannelUpdateOptions{
		OtherSubmitted:   overwrite,
		BaseURLSubmitted: overwrite,
	})
}

type ChannelUpdateOptions struct {
	OtherSubmitted   bool
	BaseURLSubmitted bool
}

func (channel *Channel) UpdateWithOptions(overwrite bool, options ChannelUpdateOptions) error {
	invalidateCodex, updated, err := channel.updateRawWithOptions(overwrite, options)
	if err != nil {
		return err
	}
	if updated {
		codexIDs := []int(nil)
		if invalidateCodex {
			codexIDs = []int{channel.Id}
		}
		finishChannelRouteMutation("update channel", []int{channel.Id}, codexIDs)
	}
	channel.reloadAfterSuccessfulUpdate("update channel")
	return nil
}

func (channel *Channel) UpdateRaw(overwrite bool) error {
	return channel.UpdateRawWithOptions(overwrite, ChannelUpdateOptions{
		OtherSubmitted:   overwrite,
		BaseURLSubmitted: overwrite,
	})
}

func (channel *Channel) UpdateRawWithOptions(overwrite bool, options ChannelUpdateOptions) error {
	invalidateCodex, updated, err := channel.updateRawWithOptions(overwrite, options)
	if err != nil {
		return err
	}
	if updated {
		codexIDs := []int(nil)
		if invalidateCodex {
			codexIDs = []int{channel.Id}
		}
		finishChannelRouteMutation("raw update channel", []int{channel.Id}, codexIDs)
	}
	channel.reloadAfterSuccessfulUpdate("raw update channel")
	return nil
}

// reloadAfterSuccessfulUpdate restores the historical mutation contract: the
// receiver reflects the complete persisted row, including fields omitted by a
// partial update. Reload happens after all post-commit routing/cache effects.
// A reload failure is diagnostic only; the durable update has already succeeded
// and must not be reported to callers as failed.
func (channel *Channel) reloadAfterSuccessfulUpdate(reason string) {
	persisted, err := GetChannelById(channel.Id)
	if err != nil {
		logger.SysError(fmt.Sprintf("failed to reload channel %d after %s: %s", channel.Id, reason, err.Error()))
		return
	}
	*channel = *persisted
}

func (channel *Channel) updateRawWithOptions(overwrite bool, options ChannelUpdateOptions) (bool, bool, error) {
	persisted, err := GetChannelById(channel.Id)
	if err != nil {
		return false, false, err
	}
	oldType := persisted.Type
	if channel.Type == config.ChannelTypeUnknown {
		channel.Type = oldType
	}
	if err = channel.hydratePersistedOtherForUpdate(options.OtherSubmitted); err != nil {
		return false, false, err
	}
	if err = channel.hydratePersistedBaseURLForUpdate(options.BaseURLSubmitted); err != nil {
		return false, false, err
	}
	if err = channel.CanonicalizeRuntimeConfigJSON(); err != nil {
		return false, false, err
	}
	if err = channel.ValidateRuntimeConfigJSON(); err != nil {
		return false, false, err
	}

	var rowsAffected int64
	err = DB.Transaction(func(tx *gorm.DB) error {
		credentialChanged := channel.Key != persisted.Key
		if !overwrite && channel.Key == "" {
			// GORM omits an empty string in partial struct updates; mirror the
			// effective mutation here so an unrelated edit cannot supersede a fence.
			credentialChanged = false
		}
		typeChanged := channel.Type != persisted.Type
		if persisted.CredentialRefreshFence != nil && !credentialChanged && !typeChanged {
			// Re-submitting the same credential is not proof of reauthorization.
			// Ordinary config edits are allowed and leave the fence untouched.
		}
		if persisted.Type != config.ChannelTypeCodex && channel.Type == config.ChannelTypeCodex && !credentialChanged && persisted.CredentialRefreshFence != nil {
			return fmt.Errorf("entering Codex requires a new credential")
		}
		protocolFields := []string{"CredentialRevision", "CredentialRefreshFence", "CredentialRefreshStartedAt"}
		if overwrite {
			result := tx.Model(channel).Select("*").Omit(append([]string{"UsedQuota"}, protocolFields...)...).Updates(channel)
			rowsAffected += result.RowsAffected
			if result.Error != nil {
				return result.Error
			}
		} else {
			result := tx.Model(channel).Omit(append([]string{"UsedQuota"}, protocolFields...)...).Updates(channel)
			rowsAffected += result.RowsAffected
			if result.Error != nil {
				return result.Error
			}
			zeroRows, updateErr := channel.updateSubmittedZeroValueRuntimeConfig(tx, options)
			rowsAffected += zeroRows
			if updateErr != nil {
				return updateErr
			}
		}

		if credentialChanged || typeChanged {
			updates := map[string]any{
				"credential_revision": gorm.Expr("credential_revision + 1"),
			}
			// A genuinely new credential explicitly supersedes every older attempt.
			// A type-only change preserves the fence, preventing ABA reuse of the old
			// bytes if the row is later changed back to Codex.
			if credentialChanged {
				updates["credential_refresh_fence"] = nil
				updates["credential_refresh_started_at"] = nil
			}
			result := tx.Model(&Channel{}).Where("id = ? AND credential_revision = ?", channel.Id, persisted.CredentialRevision).Updates(updates)
			rowsAffected += result.RowsAffected
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return fmt.Errorf("channel credential lifecycle changed concurrently")
			}
		}
		return nil
	})
	if err != nil {
		return false, false, err
	}
	invalidateCodex := oldType == config.ChannelTypeCodex || channel.Type == config.ChannelTypeCodex
	if !overwrite && channel.Status == 0 {
		channel.Status = persisted.Status
	}
	return invalidateCodex, rowsAffected > 0, nil
}

func (channel *Channel) updateSubmittedZeroValueRuntimeConfig(tx *gorm.DB, options ChannelUpdateOptions) (int64, error) {
	if channel == nil || channel.Id <= 0 {
		return 0, nil
	}
	updates := make(map[string]any)
	if options.OtherSubmitted {
		updates["other"] = channel.Other
	}
	if options.BaseURLSubmitted {
		if channel.BaseURL == nil {
			updates["base_url"] = nil
		} else {
			updates["base_url"] = *channel.BaseURL
		}
	}
	if len(updates) == 0 {
		return 0, nil
	}
	result := tx.Model(&Channel{}).Where("id = ?", channel.Id).UpdateColumns(updates)
	return result.RowsAffected, result.Error
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

func (channel *Channel) hydratePersistedRequiredOtherForPartialUpdate() error {
	return channel.hydratePersistedOtherForUpdate(false)
}

func (channel *Channel) hydratePersistedOtherForUpdate(otherSubmitted bool) error {
	if channel == nil || channel.Id <= 0 || otherSubmitted || strings.TrimSpace(channel.Other) != "" {
		return nil
	}

	persisted, err := GetChannelById(channel.Id)
	if err != nil {
		return err
	}
	if channel.Type != config.ChannelTypeUnknown && channel.Type != persisted.Type {
		return fmt.Errorf("other must be submitted when changing channel type")
	}
	channel.Other = persisted.Other
	return nil
}

func (channel *Channel) hydratePersistedBaseURLForUpdate(baseURLSubmitted bool) error {
	if channel == nil || channel.Id <= 0 || baseURLSubmitted || channel.BaseURL != nil {
		return nil
	}

	persisted, err := GetChannelById(channel.Id)
	if err != nil {
		return err
	}
	if channel.Type != config.ChannelTypeUnknown && channel.Type != persisted.Type {
		return fmt.Errorf("base_url must be submitted when changing channel type")
	}
	channel.BaseURL = persisted.BaseURL
	return nil
}

func channelTypeRequiresOtherForPartialUpdate(channelType int) bool {
	switch channelType {
	case config.ChannelTypeAzure, config.ChannelTypeAzureSpeech, config.ChannelTypeVertexAI:
		return true
	default:
		return false
	}
}

func channelTypeRequiresBaseURLForPartialUpdate(channelType int) bool {
	switch channelType {
	case config.ChannelTypeAzureSpeech, config.ChannelTypeAzureV1:
		return true
	default:
		return false
	}
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

	updated := result.RowsAffected > 0
	channelType := config.ChannelTypeUnknown
	if updated {
		if err := tx.Model(&Channel{}).Select("type").Where("id = ?", id).Scan(&channelType).Error; err != nil {
			tx.Rollback()
			logger.SysError("failed to confirm channel type after status update: " + err.Error())
			return false, err
		}
	}
	if err := tx.Commit().Error; err != nil {
		logger.SysError("failed to commit channel status update: " + err.Error())
		return false, err
	}

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
		codexIDs := []int(nil)
		if channelType == config.ChannelTypeCodex {
			codexIDs = []int{id}
		}
		finishChannelRouteMutation("update channel status", []int{id}, codexIDs)
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
	if err := clearChannelCacheKeysWithContext(context.Background(), cacheKeys); err != nil {
		logger.SysError(fmt.Sprintf("failed to clear %d cache key(s): %v", len(cacheKeys), err))
	}
}

func clearChannelCacheKeysWithContext(ctx context.Context, cacheKeys []string) error {
	return cache.DeleteCacheManyContext(ctx, cacheKeys)
}

func ClearChannelTokenCache(channelId int) {
	clearChannelCacheKeys([]string{fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelId)})
}

func ClearChannelCodexUsageCache(channelId int) {
	clearChannelCacheKeys([]string{
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelId),
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelId),
	})
}

func ClearChannelCodexDerivedCaches(channelIds []int) {
	if len(channelIds) == 0 {
		return
	}
	unique := make([]int, 0, len(channelIds))
	seen := make(map[int]struct{}, len(channelIds))
	for _, channelId := range channelIds {
		if channelId <= 0 {
			continue
		}
		if _, ok := seen[channelId]; ok {
			continue
		}
		seen[channelId] = struct{}{}
		unique = append(unique, channelId)
	}
	if err := cache.RotateCodexUsageGenerations(unique); err != nil {
		logger.SysError(fmt.Sprintf("failed to rotate %d Codex usage generation(s): %v", len(unique), err))
	}
	legacyKeys := make([]string, 0, len(unique)*3)
	for _, channelId := range unique {
		legacyKeys = append(legacyKeys,
			fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelId),
			fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelId),
			fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelId),
		)
	}
	clearChannelCacheKeys(legacyKeys)
}

func ClearChannelCodexDerivedCache(channelId int) {
	ClearChannelCodexDerivedCaches([]int{channelId})
}

func UpdateChannelKey(id int, key string) error {
	return UpdateChannelKeyWithContext(context.Background(), id, key)
}

// CompareAndSetChannelKeyWithContext updates credentials only while the database
// still contains the key from which an OAuth refresh was derived.
func CompareAndSetChannelKeyWithContext(ctx context.Context, id int, expectedKey, key string) (bool, error) {
	// Legacy callers may still use key CAS for non-refresh replacement, but it
	// must never act as a generic fence unlock. Persisted OAuth rotation uses the
	// attempt-scoped CommitCredentialRotation protocol instead.
	result := DB.WithContext(ctx).Model(&Channel{}).Where("id = ? AND key = ? AND credential_refresh_fence IS NULL", id, expectedKey).Updates(map[string]any{
		"key": key, "credential_revision": gorm.Expr("credential_revision + 1"),
		"credential_refresh_fence": nil, "credential_refresh_started_at": nil,
	})
	if result.Error != nil {
		logger.SysError("failed to compare-and-set channel key: " + result.Error.Error())
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	// OAuth persistence changes credentials, not routing topology. Quarantine the
	// old credential-bearing snapshot and let the next routing read publish one
	// complete DB snapshot. The provider that performed the CAS adopts the durable
	// key directly, so persistence never waits on cache or chooser I/O.
	ChannelGroup.failClosedChannels([]int{id})
	return true, nil
}

func UpdateChannelKeyWithContext(ctx context.Context, id int, key string) error {
	updated, err := ReplaceChannelCredentialWithContext(ctx, id, key)
	if err != nil {
		logger.SysError("failed to update channel key: " + err.Error())
		return err
	}
	if !updated {
		return nil
	}

	finishChannelRouteMutation("update channel key", []int{id}, []int{id})

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
