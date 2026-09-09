package model

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"one-api/common/config"
	"one-api/common/providerendpoint"
)

// 旧格式只在此一次性数据库迁移中解释；服务请求不调用迁移函数。
func migrateCustomChannelEndpoints() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: "202609080001",
		Migrate: func(db *gorm.DB) error {
			if !db.Migrator().HasTable(&Channel{}) {
				return nil
			}
			return db.Transaction(func(tx *gorm.DB) error {
				lastID := 0
				for {
					var channels []Channel
					if err := tx.Unscoped().Select("id", "type", "plugin", "base_url").Where("type = ? AND id > ?", config.ChannelTypeCustom, lastID).Order("id").Limit(200).Find(&channels).Error; err != nil {
						return err
					}
					if len(channels) == 0 {
						return nil
					}
					for i := range channels {
						channel := &channels[i]
						lastID = channel.Id
						changed, err := migrateCustomEndpointConfig(channel)
						if err != nil {
							return fmt.Errorf("渠道 %d 接口迁移失败: %w", channel.Id, err)
						}
						if !changed {
							continue
						}
						// 禁止失败 SQL 把包含账号信息的配置写进日志。
						if err := tx.Session(&gorm.Session{Logger: tx.Logger.LogMode(logger.Silent)}).Unscoped().Model(&Channel{}).Where("id = ?", channel.Id).UpdateColumn("plugin", channel.Plugin).Error; err != nil {
							return fmt.Errorf("渠道 %d 接口迁移写入失败", channel.Id)
						}
					}
				}
			})
		},
		Rollback: func(*gorm.DB) error {
			return fmt.Errorf("接口配置不能无损降级；请停机并恢复升级前的数据库备份及对应程序")
		},
	}
}

func migrateCustomEndpointConfig(channel *Channel) (bool, error) {
	plugin := PluginType{}
	if channel.Plugin != nil {
		for key, value := range channel.Plugin.Data() {
			plugin[key] = value
		}
	}
	if _, exists := plugin["endpoints"]; exists {
		return false, channel.validateEndpointConfig()
	}
	endpoints := make(map[string]any)
	knownModes := make(map[int]bool)
	// 冻结升级前的支持面；以后新增接口不能通过重跑历史迁移被自动开启。
	for _, mode := range []int{
		config.RelayModeChatCompletions, config.RelayModeCompletions, config.RelayModeEmbeddings,
		config.RelayModeModerations, config.RelayModeImagesGenerations, config.RelayModeImagesEdits,
		config.RelayModeImagesVariations, config.RelayModeAudioSpeech, config.RelayModeAudioTranscription,
		config.RelayModeAudioTranslation, config.RelayModeResponses,
	} {
		definition, ok := providerendpoint.ForRelayMode(mode)
		if !ok {
			return false, fmt.Errorf("旧接口 %d 的迁移目标不存在", mode)
		}
		knownModes[definition.RelayMode] = true
		uri, present, err := resolveLegacyCustomAPIURI(plugin["customize"], definition.RelayMode)
		if err != nil {
			return false, err
		}
		setting := providerendpoint.Setting{Enabled: true}
		if present {
			setting.Enabled, setting.UpstreamURL = uri != "", uri
		}
		endpoints[definition.ID] = setting.Data()
	}
	for key := range plugin["customize"] {
		mode, err := strconv.Atoi(key)
		if err != nil || !knownModes[mode] {
			return false, fmt.Errorf("customize 包含无法归属的接口键 %q，请在升级前清理", key)
		}
	}
	claude := plugin["claude"]
	setting := providerendpoint.Setting{}
	if rawEnabled, exists := claude["enabled"]; exists {
		var ok bool
		setting.Enabled, ok = rawEnabled.(bool)
		if !ok {
			return false, fmt.Errorf("claude.enabled 必须是布尔值")
		}
	}
	baseURL := ""
	if rawURL, exists := claude["base_url"]; exists && rawURL != nil {
		var ok bool
		baseURL, ok = rawURL.(string)
		if !ok {
			return false, fmt.Errorf("claude.base_url 必须是字符串")
		}
	}
	for key := range claude {
		if key != "enabled" && key != "base_url" {
			return false, fmt.Errorf("claude 包含无法迁移的配置字段 %q", key)
		}
	}
	if strings.TrimSpace(baseURL) != "" || setting.Enabled {
		explicitBase := strings.TrimSpace(baseURL) != ""
		if !explicitBase {
			baseURL = channel.GetBaseURL()
		}
		if strings.TrimSpace(baseURL) == "" {
			baseURL = providerendpoint.DefaultClaudeBaseURL
		}
		normalized, err := normalizeClaudeBaseURL("claude.base_url", baseURL)
		if err != nil {
			return false, fmt.Errorf("claude.base_url 无法迁移为有效上游地址")
		}
		oldURL := providerendpoint.CustomRequestURL(normalized, providerendpoint.DefaultMessagesURI)
		newBase := strings.TrimSpace(channel.GetBaseURL())
		if newBase == "" {
			newBase = providerendpoint.DefaultClaudeBaseURL
		}
		// 显式地址保持独立；继承渠道地址时，仅在原规范化会改变 wire 时固化完整地址。
		if explicitBase || oldURL != providerendpoint.CustomRequestURL(newBase, providerendpoint.DefaultMessagesURI) {
			setting.UpstreamURL = oldURL
		}
	}
	endpoints[providerendpoint.Messages] = setting.Data()
	delete(plugin, "customize")
	delete(plugin, "claude")
	plugin["endpoints"] = endpoints
	converted := datatypes.NewJSONType(plugin)
	candidate := *channel
	candidate.Plugin = &converted
	if err := candidate.validateEndpointConfig(); err != nil {
		return false, err
	}
	channel.Plugin = &converted
	return true, nil
}

// resolveLegacyCustomAPIURI 统一解释 customize 的数字键、默认值及 disable。
// 数字别名只有在解释结果一致时才可共存，避免 map 遍历顺序改变执行地址。
func resolveLegacyCustomAPIURI(mapping map[string]interface{}, relayMode int) (value string, present bool, err error) {
	found := false
	for key, raw := range mapping {
		mode, parseErr := strconv.Atoi(key)
		if parseErr != nil || mode != relayMode {
			continue
		}
		candidate, ok := raw.(string)
		candidatePresent := ok && candidate != ""
		if !candidatePresent || candidate == "disable" {
			candidate = ""
		}
		if found && (candidate != value || candidatePresent != present) {
			return "", true, fmt.Errorf("plugin.customize 中接口 %d 的数字键别名存在冲突", relayMode)
		}
		value, present, found = candidate, candidatePresent, true
	}
	return value, present, nil
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
