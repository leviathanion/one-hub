package model

import (
	"encoding/json"
	"fmt"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

const (
	automaticRecoverIntervalLegacyOptionKey = "AutomaticEnableChannelRecoverFrequency"
	automaticRecoverEnabledOptionKey        = "AutomaticRecoverChannelsEnabled"
	automaticRecoverIntervalOptionKey       = "AutomaticRecoverChannelsIntervalMinutes"
)

var loggedUnknownOptionKeys sync.Map
var loggedInvalidOptionLoadErrors sync.Map

type Option struct {
	Key   string `json:"key" gorm:"primaryKey"`
	Value string `json:"value"`
}

func AllOption() ([]*Option, error) {
	var options []*Option
	err := DB.Find(&options).Error
	return options, err
}

func GetOption(key string) (option Option, err error) {
	err = DB.First(&option, Option{Key: key}).Error
	return
}

func InitOptionMap() {
	publicOption := func() config.OptionMetadata {
		return config.OptionMetadata{Visibility: config.OptionVisibilityPublic}
	}
	publicGroupedOption := func(group string) config.OptionMetadata {
		metadata := publicOption()
		metadata.Group = group
		return metadata
	}
	sensitiveOption := func() config.OptionMetadata {
		return config.OptionMetadata{Visibility: config.OptionVisibilitySensitive}
	}
	sensitiveGroupedOption := func(group string) config.OptionMetadata {
		metadata := sensitiveOption()
		metadata.Group = group
		return metadata
	}

	config.GlobalOption.RegisterBoolOption("PasswordLoginEnabled", &config.PasswordLoginEnabled, publicOption())
	config.GlobalOption.RegisterBoolOption("PasswordRegisterEnabled", &config.PasswordRegisterEnabled, publicOption())
	config.GlobalOption.RegisterBoolOption("EmailVerificationEnabled", &config.EmailVerificationEnabled, publicOption())
	config.GlobalOption.RegisterBoolOption("GitHubOAuthEnabled", &config.GitHubOAuthEnabled, publicGroupedOption(config.OptionGroupGitHubOAuth))
	config.GlobalOption.RegisterBoolOption("WeChatAuthEnabled", &config.WeChatAuthEnabled, publicGroupedOption(config.OptionGroupWeChatAuth))
	config.GlobalOption.RegisterBoolOption("LarkAuthEnabled", &config.LarkAuthEnabled, publicGroupedOption(config.OptionGroupLarkOAuth))
	config.GlobalOption.RegisterBoolOption("OIDCAuthEnabled", &config.OIDCAuthEnabled, publicGroupedOption(config.OptionGroupOIDCAuth))
	config.GlobalOption.RegisterBoolOption("TurnstileCheckEnabled", &config.TurnstileCheckEnabled, publicGroupedOption(config.OptionGroupTurnstile))
	config.GlobalOption.RegisterBoolOption("RegisterEnabled", &config.RegisterEnabled, publicOption())
	config.GlobalOption.RegisterBoolOption("AutomaticDisableChannelEnabled", &config.AutomaticDisableChannelEnabled, publicOption())
	config.GlobalOption.RegisterBoolOption("AutomaticEnableChannelEnabled", &config.AutomaticEnableChannelEnabled, publicOption())
	config.GlobalOption.RegisterBoolOption(automaticRecoverEnabledOptionKey, &config.AutomaticRecoverChannelsEnabled, publicGroupedOption(config.OptionGroupAutomaticRecoverChannel))
	config.GlobalOption.RegisterIntOption(automaticRecoverIntervalOptionKey, &config.AutomaticRecoverChannelsIntervalMinutes, config.OptionMetadata{
		Visibility: config.OptionVisibilityPublic,
		Aliases:    []string{automaticRecoverIntervalLegacyOptionKey},
		Group:      config.OptionGroupAutomaticRecoverChannel,
	})
	config.GlobalOption.RegisterBoolOption("ApproximateTokenEnabled", &config.ApproximateTokenEnabled, publicOption())
	config.GlobalOption.RegisterBoolOption("LogConsumeEnabled", &config.LogConsumeEnabled, publicOption())
	config.GlobalOption.RegisterBoolOption("DisplayInCurrencyEnabled", &config.DisplayInCurrencyEnabled, publicOption())
	config.GlobalOption.RegisterFloatOption("ChannelDisableThreshold", &config.ChannelDisableThreshold, publicOption())
	config.GlobalOption.RegisterBoolOption("EmailDomainRestrictionEnabled", &config.EmailDomainRestrictionEnabled, publicGroupedOption(config.OptionGroupEmailDomainRestriction))

	config.GlobalOption.RegisterCustomOption("EmailDomainWhitelist", func() string {
		return strings.Join(config.EmailDomainWhitelist, ",")
	}, func(value string) error {
		config.EmailDomainWhitelist = strings.Split(value, ",")
		return nil
	}, publicGroupedOption(config.OptionGroupEmailDomainRestriction), "")

	config.GlobalOption.RegisterStringOption("SMTPServer", &config.SMTPServer, publicOption())
	config.GlobalOption.RegisterStringOption("SMTPFrom", &config.SMTPFrom, publicOption())
	config.GlobalOption.RegisterIntOption("SMTPPort", &config.SMTPPort, publicOption())
	config.GlobalOption.RegisterStringOption("SMTPAccount", &config.SMTPAccount, publicOption())
	config.GlobalOption.RegisterStringOption("SMTPToken", &config.SMTPToken, sensitiveOption())
	config.GlobalOption.RegisterValueOption("Notice", publicOption())
	config.GlobalOption.RegisterValueOption("About", publicOption())
	config.GlobalOption.RegisterValueOption("HomePageContent", publicOption())
	config.GlobalOption.RegisterStringOption("Footer", &config.Footer, publicOption())
	config.GlobalOption.RegisterStringOption("SystemName", &config.SystemName, publicOption())
	config.GlobalOption.RegisterStringOption("Logo", &config.Logo, publicOption())
	config.GlobalOption.RegisterStringOption("AnalyticsCode", &config.AnalyticsCode, publicOption())
	config.GlobalOption.RegisterStringOption("ServerAddress", &config.ServerAddress, publicOption())
	config.GlobalOption.RegisterStringOption("GitHubClientId", &config.GitHubClientId, publicGroupedOption(config.OptionGroupGitHubOAuth))
	config.GlobalOption.RegisterStringOption("GitHubClientSecret", &config.GitHubClientSecret, sensitiveGroupedOption(config.OptionGroupGitHubOAuth))
	config.GlobalOption.RegisterStringOption("LarkClientId", &config.LarkClientId, publicGroupedOption(config.OptionGroupLarkOAuth))
	config.GlobalOption.RegisterStringOption("LarkClientSecret", &config.LarkClientSecret, sensitiveGroupedOption(config.OptionGroupLarkOAuth))
	config.GlobalOption.RegisterStringOption("OIDCClientId", &config.OIDCClientId, publicGroupedOption(config.OptionGroupOIDCAuth))
	config.GlobalOption.RegisterStringOption("OIDCClientSecret", &config.OIDCClientSecret, sensitiveGroupedOption(config.OptionGroupOIDCAuth))
	config.GlobalOption.RegisterStringOption("OIDCIssuer", &config.OIDCIssuer, publicGroupedOption(config.OptionGroupOIDCAuth))
	config.GlobalOption.RegisterStringOption("OIDCScopes", &config.OIDCScopes, publicGroupedOption(config.OptionGroupOIDCAuth))
	config.GlobalOption.RegisterStringOption("OIDCUsernameClaims", &config.OIDCUsernameClaims, publicGroupedOption(config.OptionGroupOIDCAuth))
	config.GlobalOption.RegisterStringOption("WeChatServerAddress", &config.WeChatServerAddress, publicGroupedOption(config.OptionGroupWeChatAuth))
	config.GlobalOption.RegisterStringOption("WeChatServerToken", &config.WeChatServerToken, sensitiveGroupedOption(config.OptionGroupWeChatAuth))
	config.GlobalOption.RegisterStringOption("WeChatAccountQRCodeImageURL", &config.WeChatAccountQRCodeImageURL, publicOption())
	config.GlobalOption.RegisterStringOption("TurnstileSiteKey", &config.TurnstileSiteKey, publicGroupedOption(config.OptionGroupTurnstile))
	config.GlobalOption.RegisterStringOption("TurnstileSecretKey", &config.TurnstileSecretKey, sensitiveGroupedOption(config.OptionGroupTurnstile))
	config.GlobalOption.RegisterIntOption("QuotaForNewUser", &config.QuotaForNewUser, publicOption())
	config.GlobalOption.RegisterIntOption("QuotaForInviter", &config.QuotaForInviter, publicOption())
	config.GlobalOption.RegisterIntOption("QuotaForInvitee", &config.QuotaForInvitee, publicOption())
	config.GlobalOption.RegisterIntOption("QuotaRemindThreshold", &config.QuotaRemindThreshold, publicOption())
	config.GlobalOption.RegisterIntOption("PreConsumedQuota", &config.PreConsumedQuota, publicOption())
	config.GlobalOption.RegisterStringOption("TopUpLink", &config.TopUpLink, publicOption())
	config.GlobalOption.RegisterStringOption("ChatLink", &config.ChatLink, publicOption())
	config.GlobalOption.RegisterStringOption("ChatLinks", &config.ChatLinks, publicOption())
	config.GlobalOption.RegisterFloatOption("QuotaPerUnit", &config.QuotaPerUnit, publicOption())
	config.GlobalOption.RegisterIntOption("RetryTimes", &config.RetryTimes, publicOption())
	config.GlobalOption.RegisterIntOption("RetryCooldownSeconds", &config.RetryCooldownSeconds, publicOption())
	config.GlobalOption.RegisterIntOption("PreferredChannelWaitMilliseconds", &config.PreferredChannelWaitMilliseconds, publicOption())
	config.GlobalOption.RegisterIntOption("PreferredChannelWaitPollMilliseconds", &config.PreferredChannelWaitPollMilliseconds, publicOption())
	config.GlobalOption.RegisterBoolOption("MjNotifyEnabled", &config.MjNotifyEnabled, publicOption())
	config.GlobalOption.RegisterStringOption("ChatImageRequestProxy", &config.ChatImageRequestProxy, publicOption())
	config.GlobalOption.RegisterFloatOption("PaymentUSDRate", &config.PaymentUSDRate, publicOption())
	config.GlobalOption.RegisterIntOption("PaymentMinAmount", &config.PaymentMinAmount, publicOption())

	config.GlobalOption.RegisterCustomOptionWithValidator("RechargeDiscount", func() string {
		return common.RechargeDiscount2JSONString()
	}, func(value string) error {
		if err := common.UpdateRechargeDiscountByJSONString(value); err != nil {
			return err
		}
		config.RechargeDiscount = value
		return nil
	}, func(value string) error {
		preview := make(map[string]float64)
		return json.Unmarshal([]byte(value), &preview)
	}, publicOption(), "")

	config.GlobalOption.RegisterStringOption("CFWorkerImageUrl", &config.CFWorkerImageUrl, publicOption())
	config.GlobalOption.RegisterStringOption("CFWorkerImageKey", &config.CFWorkerImageKey, sensitiveOption())
	config.GlobalOption.RegisterIntOption("OldTokenMaxId", &config.OldTokenMaxId, publicOption())
	config.GlobalOption.RegisterBoolOption("GitHubOldIdCloseEnabled", &config.GitHubOldIdCloseEnabled, publicOption())
	config.GlobalOption.RegisterBoolOption("GeminiAPIEnabled", &config.GeminiAPIEnabled, publicOption())
	config.GlobalOption.RegisterBoolOption("ClaudeAPIEnabled", &config.ClaudeAPIEnabled, publicOption())

	config.GlobalOption.RegisterCustomOption("DisableChannelKeywords", func() string {
		return common.DisableChannelKeywordsInstance.GetKeywords()
	}, func(value string) error {
		common.DisableChannelKeywordsInstance.Load(value)
		return nil
	}, publicOption(), common.GetDefaultDisableChannelKeywords())

	config.GlobalOption.RegisterIntOption("RetryTimeOut", &config.RetryTimeOut, publicOption())

	config.GlobalOption.RegisterBoolOption("EnableSafe", &config.EnableSafe, publicOption())
	config.GlobalOption.RegisterStringOption("SafeToolName", &config.SafeToolName, publicOption())
	config.GlobalOption.RegisterCustomOption("SafeKeyWords", func() string {
		return strings.Join(config.SafeKeyWords, "\n")
	}, func(value string) error {
		config.SafeKeyWords = strings.Split(value, "\n")
		return nil
	}, publicOption(), "")

	loadOptionsFromDatabase()
}

func loadOptionsFromDatabase() {
	options, _ := AllOption()
	options = migrateAutomaticRecoverOptions(options)
	loadedOptions := make(map[string]string, len(options))
	for _, option := range options {
		normalizedKey := config.GlobalOption.NormalizeKey(option.Key)
		if !config.GlobalOption.IsRegistered(normalizedKey) {
			logUnknownOptionKeyOnce(option.Key)
			continue
		}
		loadedOptions[normalizedKey] = option.Value
	}
	for key, value := range loadedOptions {
		err := config.GlobalOption.Set(key, value)
		if err != nil {
			if shouldLogInvalidOptionLoadError(key, err) && logger.Logger != nil {
				logger.SysError("failed to update option map for key " + key + ": " + err.Error())
			}
			continue
		}
		clearLoggedInvalidOptionLoadError(key)
	}
	clearStaleLoggedInvalidOptionLoadErrors(loadedOptions)
}

func migrateAutomaticRecoverOptions(options []*Option) []*Option {
	optionByKey := make(map[string]*Option, len(options))
	for _, option := range options {
		optionByKey[option.Key] = option
	}

	legacyOption, exists := optionByKey[automaticRecoverIntervalLegacyOptionKey]
	if !exists {
		return options
	}

	canonicalOption, hasCanonical := optionByKey[automaticRecoverIntervalOptionKey]
	if !hasCanonical && strings.TrimSpace(legacyOption.Value) != "" {
		migratedOption := &Option{
			Key:   automaticRecoverIntervalOptionKey,
			Value: legacyOption.Value,
		}
		if err := DB.Save(migratedOption).Error; err != nil {
			if logger.Logger != nil {
				logger.SysError("failed to migrate automatic recover interval option: " + err.Error())
			}
		} else {
			canonicalOption = migratedOption
			hasCanonical = true
			if logger.Logger != nil {
				logger.SysLog("migrated legacy automatic recover interval option to AutomaticRecoverChannelsIntervalMinutes")
			}
		}
	}

	if hasCanonical {
		if err := DB.Delete(&Option{}, "key = ?", automaticRecoverIntervalLegacyOptionKey).Error; err != nil {
			if logger.Logger != nil {
				logger.SysError("failed to delete legacy automatic recover interval option: " + err.Error())
			}
		}
	}

	migratedOptions := make([]*Option, 0, len(options)+1)
	hasCanonicalInSlice := false
	for _, option := range options {
		switch option.Key {
		case automaticRecoverIntervalLegacyOptionKey:
			if hasCanonical {
				continue
			}
			migratedOptions = append(migratedOptions, option)
		case automaticRecoverIntervalOptionKey:
			hasCanonicalInSlice = true
			migratedOptions = append(migratedOptions, option)
		default:
			migratedOptions = append(migratedOptions, option)
		}
	}

	if hasCanonical && !hasCanonicalInSlice && canonicalOption != nil {
		migratedOptions = append(migratedOptions, canonicalOption)
	}
	return migratedOptions
}

func SyncOptions(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		logger.SysLog("syncing options from database")
		loadOptionsFromDatabase()
	}
}

func UpdateOption(key string, value string) error {
	normalizedKey := config.GlobalOption.NormalizeKey(key)
	prepared, err := config.PrepareOptionUpdates([]config.OptionUpdate{{
		Key:   key,
		Value: value,
	}}, config.OptionGroupValidationStrict)
	if err != nil {
		return err
	}
	if len(prepared.Updates) == 0 {
		return SaveOptionsTx(DB, []Option{{
			Key:   normalizedKey,
			Value: value,
		}})
	}
	if err := SaveOptionsTx(DB, []Option{{
		Key:   prepared.Updates[0].Key,
		Value: prepared.Updates[0].Value,
	}}); err != nil {
		return err
	}
	return config.GlobalOption.Set(prepared.Updates[0].Key, prepared.Updates[0].Value)
}

func SaveOptionsTx(tx *gorm.DB, options []Option) error {
	for i := range options {
		key := config.GlobalOption.NormalizeKey(options[i].Key)
		if !config.GlobalOption.IsRegistered(key) {
			return fmt.Errorf("未知的配置项：%s", key)
		}
		option := Option{
			Key: key,
		}
		if err := tx.FirstOrCreate(&option, Option{Key: key}).Error; err != nil {
			return err
		}
		option.Value = options[i].Value
		if err := tx.Save(&option).Error; err != nil {
			return err
		}
	}
	return nil
}

func logUnknownOptionKeyOnce(key string) {
	if _, loaded := loggedUnknownOptionKeys.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	if logger.Logger != nil {
		logger.SysLog("skipping unknown option key during option sync: " + key)
	}
}

func shouldLogInvalidOptionLoadError(key string, err error) bool {
	message := err.Error()
	previous, exists := loggedInvalidOptionLoadErrors.Load(key)
	if exists && previous == message {
		return false
	}
	loggedInvalidOptionLoadErrors.Store(key, message)
	return true
}

func clearLoggedInvalidOptionLoadError(key string) {
	loggedInvalidOptionLoadErrors.Delete(key)
}

func clearStaleLoggedInvalidOptionLoadErrors(loadedOptions map[string]string) {
	loggedInvalidOptionLoadErrors.Range(func(rawKey, _ any) bool {
		key, ok := rawKey.(string)
		if !ok {
			loggedInvalidOptionLoadErrors.Delete(rawKey)
			return true
		}
		if _, exists := loadedOptions[key]; !exists {
			loggedInvalidOptionLoadErrors.Delete(key)
		}
		return true
	})
}
