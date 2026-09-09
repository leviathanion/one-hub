package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var loggedUnknownOptionKeys sync.Map

const optionsPublicationWatchInterval = 5 * time.Second

var (
	optionsReloadMu       sync.Mutex
	optionsPublicationMu  sync.RWMutex
	optionsPublicationErr string
)

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
	config.GlobalOption.RegisterBoolOption("ApproximateTokenEnabled", &config.ApproximateTokenEnabled, publicOption())
	config.GlobalOption.RegisterBoolOption("LogConsumeEnabled", &config.LogConsumeEnabled, publicOption())
	config.GlobalOption.RegisterBoolOption("DisplayInCurrencyEnabled", &config.DisplayInCurrencyEnabled, publicOption())
	config.GlobalOption.RegisterFloatOption("ChannelDisableThreshold", &config.ChannelDisableThreshold, publicOption())
	config.GlobalOption.RegisterIntOption("ChannelTestConcurrency", &config.ChannelTestConcurrency, publicOption())
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
	config.GlobalOption.RegisterCustomOptionWithValidator("PreConsumedQuota", func() string {
		return strconv.Itoa(config.PreConsumedQuota)
	}, func(value string) error {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		config.PreConsumedQuota = parsed
		return nil
	}, func(value string) error {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		if parsed < 0 {
			return errors.New("PreConsumedQuota must be non-negative")
		}
		return nil
	}, publicOption(), "")
	config.GlobalOption.RegisterStringOption("TopUpLink", &config.TopUpLink, publicOption())
	config.GlobalOption.RegisterStringOption("ChatLink", &config.ChatLink, publicOption())
	config.GlobalOption.RegisterStringOption("ChatLinks", &config.ChatLinks, publicOption())
	config.GlobalOption.RegisterCustomOptionWithValidator("QuotaPerUnit", func() string {
		return strconv.FormatFloat(config.QuotaPerUnit, 'f', -1, 64)
	}, func(value string) error {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return errors.New("QuotaPerUnit must be a finite number")
		}
		config.QuotaPerUnit = parsed
		return nil
	}, func(value string) error {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return errors.New("QuotaPerUnit must be a finite number")
		}
		if parsed <= 0 {
			return errors.New("QuotaPerUnit must be positive")
		}
		return nil
	}, publicOption(), "")
	config.GlobalOption.RegisterIntOption("RetryTimes", &config.RetryTimes, publicOption())
	config.GlobalOption.RegisterCustomOptionWithValidator("RetryStatusCodes", func() string {
		return config.RetryStatusCodes
	}, func(value string) error {
		return config.SetRetryStatusCodes(value)
	}, func(value string) error {
		return config.ValidateRetryStatusCodes(value)
	}, publicOption(), config.DefaultRetryStatusCodes)
	config.GlobalOption.RegisterIntOption("RetryCooldownSeconds", &config.RetryCooldownSeconds, publicOption())
	config.GlobalOption.RegisterIntOption("PreferredChannelWaitMilliseconds", &config.PreferredChannelWaitMilliseconds, publicOption())
	config.GlobalOption.RegisterIntOption("PreferredChannelWaitPollMilliseconds", &config.PreferredChannelWaitPollMilliseconds, publicOption())
	config.GlobalOption.RegisterBoolOption("MjNotifyEnabled", &config.MjNotifyEnabled, publicOption())
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

	config.GlobalOption.RegisterIntOption("OldTokenMaxId", &config.OldTokenMaxId, publicOption())
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

	if err := LoadAndPublishOptions(context.Background()); err != nil {
		if logger.Logger != nil {
			logger.SysError("failed to initialize runtime options: " + err.Error())
		}
		return
	}
	// 将环境继承的单一签发方通过既有发布协议固定，后续登录只核验 SQL 作用域。
	issuer := config.GlobalOption.RuntimeSnapshot().String("OIDCIssuer", config.OIDCIssuer)
	if issuer != "" {
		if _, err := GetOption("OIDCIssuer"); errors.Is(err, gorm.ErrRecordNotFound) {
			if err := UpdateOption("OIDCIssuer", issuer); err != nil {
				setOptionsPublicationError(err)
			}
		}
	}
}

func loadOptionsFromDatabase() {
	_ = LoadAndPublishOptions(context.Background())
}

func LoadAndPublishOptions(ctx context.Context) (err error) {
	if DB == nil {
		return errors.New("database is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := lockPublicationReload(ctx, &optionsReloadMu); err != nil {
		return err
	}
	defer optionsReloadMu.Unlock()
	defer func() { setOptionsPublicationError(err) }()
	for attempt := 0; attempt < 2; attempt++ {
		versionBefore, err := ReadPublicationVersion(ctx, DB, PublicationOwnerOptions)
		if err != nil {
			return err
		}
		overrides, err := loadOptionOverrides(DB.WithContext(ctx))
		if err != nil {
			return err
		}
		versionAfter, err := ReadPublicationVersion(ctx, DB, PublicationOwnerOptions)
		if err != nil {
			return err
		}
		if versionBefore != versionAfter {
			continue
		}
		_, err = config.GlobalOption.PublishRuntimeOverrides(versionBefore, overrides)
		if err != nil {
			return err
		}
		return nil
	}
	return ErrPublicationVersionConflict
}

func SyncOptionsPublication(ctx context.Context) error {
	if DB == nil {
		return errors.New("database is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	head, err := ReadPublicationVersion(ctx, DB, PublicationOwnerOptions)
	if err != nil {
		setOptionsPublicationError(err)
		return err
	}
	snapshot := config.GlobalOption.RuntimeSnapshot()
	if snapshot != nil && snapshot.Version() == head {
		setOptionsPublicationError(nil)
		return nil
	}
	return LoadAndPublishOptions(ctx)
}

func WatchOptionsPublication(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(optionsPublicationWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeCtx, cancelProbe := context.WithTimeout(ctx, publicationCommitProbeDeadline)
			_ = SyncOptionsPublication(probeCtx)
			cancelProbe()
		}
	}
}

func CheckOptionsPublication(ctx context.Context) error {
	if DB == nil {
		return errors.New("options table is unavailable")
	}
	migrator := DB.WithContext(ctx).Migrator()
	if !migrator.HasTable(&Option{}) {
		return errors.New("options table is unavailable")
	}
	if !migrator.HasColumn(&Option{}, "key") || !migrator.HasColumn(&Option{}, "value") {
		return errors.New("options schema is incomplete")
	}
	head, err := ReadPublicationVersion(ctx, DB, PublicationOwnerOptions)
	if err != nil {
		return err
	}
	snapshot := config.GlobalOption.RuntimeSnapshot()
	if snapshot == nil || snapshot.Version() != head {
		return fmt.Errorf("published options version does not match database head %d", head)
	}
	optionsPublicationMu.RLock()
	publicationErr := optionsPublicationErr
	optionsPublicationMu.RUnlock()
	if publicationErr != "" {
		return errors.New(publicationErr)
	}
	return nil
}

func setOptionsPublicationError(err error) {
	optionsPublicationMu.Lock()
	defer optionsPublicationMu.Unlock()
	optionsPublicationErr = errorText(err)
}

type OptionMutation struct {
	Key     string
	Value   *string
	Inherit bool
}

var ErrOptionsCommitOutcomeUnknown = errors.New("options commit outcome is unknown")

func ApplyOptionMutations(ctx context.Context, expectedVersion int64, mutations []OptionMutation) (int64, error) {
	if DB == nil || expectedVersion < 1 {
		return 0, errors.New("database and positive expected version are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	prepared, err := normalizeOptionMutations(mutations)
	if err != nil {
		return 0, err
	}
	var targetOverrides map[string]string
	changed := false
	tx := DB.WithContext(ctx).Begin()
	if tx.Error != nil {
		return 0, tx.Error
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback().Error
		}
	}()
	head, err := ReadPublicationVersion(ctx, tx, PublicationOwnerOptions)
	if err != nil {
		return 0, err
	}
	if head != expectedVersion {
		return 0, ErrPublicationVersionConflict
	}
	current, err := loadOptionOverrides(tx)
	if err != nil {
		return 0, err
	}
	targetOverrides = cloneOptionOverrides(current)
	for _, mutation := range prepared {
		if mutation.Inherit {
			if _, exists := targetOverrides[mutation.Key]; exists {
				delete(targetOverrides, mutation.Key)
				changed = true
			}
			continue
		}
		if currentValue, exists := targetOverrides[mutation.Key]; !exists || currentValue != *mutation.Value {
			targetOverrides[mutation.Key] = *mutation.Value
			changed = true
		}
	}
	candidate, err := config.GlobalOption.ValidateRuntimeOverrides(targetOverrides)
	if err != nil {
		return 0, err
	}
	for _, mutation := range prepared {
		if mutation.Key == "OIDCIssuer" {
			if err := validateOIDCIssuerMutation(tx, candidate.String("OIDCIssuer", config.OIDCIssuer), mutation.Inherit); err != nil {
				return 0, err
			}
		}
	}
	if !changed {
		_ = tx.Rollback().Error
		rollback = false
		loadCtx, cancelLoad := publicationCommitProbeContext(ctx)
		_ = LoadAndPublishOptions(loadCtx)
		cancelLoad()
		return expectedVersion, nil
	}
	for _, mutation := range prepared {
		if mutation.Inherit {
			if _, exists := current[mutation.Key]; !exists {
				continue
			}
			result := tx.Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: mutation.Key}).Delete(&Option{})
			if result.Error != nil {
				return 0, result.Error
			}
			if result.RowsAffected != 1 {
				return 0, ErrPublicationVersionConflict
			}
			continue
		}
		if currentValue, exists := current[mutation.Key]; exists && currentValue == *mutation.Value {
			continue
		}
		option := Option{Key: mutation.Key, Value: *mutation.Value}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value"})}).Create(&option).Error; err != nil {
			return 0, err
		}
	}
	if _, err := BumpPublicationVersionCAS(ctx, tx, PublicationOwnerOptions, expectedVersion); err != nil {
		return 0, err
	}
	rollback = false
	commitErr := tx.Commit().Error
	if commitErr != nil {
		if resolvedErr := resolveOptionsCommitOutcome(ctx, expectedVersion, commitErr, targetOverrides); resolvedErr != nil {
			return 0, resolvedErr
		}
	}
	newVersion := expectedVersion + 1
	loadCtx, cancelLoad := publicationCommitProbeContext(ctx)
	defer cancelLoad()
	if err := LoadAndPublishOptions(loadCtx); err != nil && logger.Logger != nil {
		logger.SysError(fmt.Sprintf("options version %d committed but local publication failed: %v", newVersion, err))
	}
	return newVersion, nil
}

func UpdateOption(key string, value string) error {
	head, err := ReadPublicationVersion(context.Background(), DB, PublicationOwnerOptions)
	if err != nil {
		return err
	}
	_, err = ApplyOptionMutations(context.Background(), head, []OptionMutation{{Key: key, Value: &value}})
	return err
}

func SaveOptionsTx(tx *gorm.DB, options []Option) error {
	for i := range options {
		key := config.GlobalOption.NormalizeKey(options[i].Key)
		if !config.GlobalOption.IsRegistered(key) {
			return fmt.Errorf("未知的配置项：%s", key)
		}
		option := Option{Key: key, Value: options[i].Value}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value"})}).Create(&option).Error; err != nil {
			return err
		}
	}
	return nil
}

func loadOptionOverrides(tx *gorm.DB) (map[string]string, error) {
	var options []Option
	if err := tx.Find(&options).Error; err != nil {
		return nil, err
	}
	overrides := make(map[string]string, len(options))
	for _, option := range options {
		key := config.GlobalOption.NormalizeKey(option.Key)
		if !config.GlobalOption.IsRegistered(key) {
			logUnknownOptionKeyOnce(option.Key)
			return nil, fmt.Errorf("unknown option override %q", option.Key)
		}
		if _, exists := overrides[key]; exists {
			return nil, fmt.Errorf("duplicate option override %q", key)
		}
		overrides[key] = option.Value
	}
	return overrides, nil
}

func normalizeOptionMutations(mutations []OptionMutation) ([]OptionMutation, error) {
	if len(mutations) == 0 {
		return nil, errors.New("option updates are required")
	}
	prepared := make([]OptionMutation, 0, len(mutations))
	seen := make(map[string]struct{}, len(mutations))
	for _, mutation := range mutations {
		key := config.GlobalOption.NormalizeKey(mutation.Key)
		if !config.GlobalOption.IsRegistered(key) {
			return nil, &config.OptionValidationError{Key: key, Message: "未知的配置项：" + key}
		}
		if _, exists := seen[key]; exists {
			return nil, &config.OptionValidationError{Key: key, Message: "请求中包含重复的配置项"}
		}
		seen[key] = struct{}{}
		if mutation.Inherit == (mutation.Value != nil) {
			return nil, &config.OptionValidationError{Key: key, Message: "配置项必须且只能指定 value 或 inherit"}
		}
		mutation.Key = key
		prepared = append(prepared, mutation)
	}
	sort.Slice(prepared, func(i, j int) bool { return prepared[i].Key < prepared[j].Key })
	return prepared, nil
}

func cloneOptionOverrides(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func resolveOptionsCommitOutcome(ctx context.Context, expectedVersion int64, transactionErr error, target map[string]string) error {
	probeCtx, cancelProbe := publicationCommitProbeContext(ctx)
	defer cancelProbe()
	head, err := ReadPublicationVersion(probeCtx, DB, PublicationOwnerOptions)
	if err != nil {
		return fmt.Errorf("%w: transaction error: %v; head read failed: %v", ErrOptionsCommitOutcomeUnknown, transactionErr, err)
	}
	if head == expectedVersion {
		return transactionErr
	}
	if head != expectedVersion+1 || target == nil {
		return fmt.Errorf("%w: transaction error: %v; expected head %d, got %d", ErrOptionsCommitOutcomeUnknown, transactionErr, expectedVersion+1, head)
	}
	actual, err := loadOptionOverrides(DB.WithContext(probeCtx))
	if err != nil {
		return fmt.Errorf("%w: transaction error: %v; target read failed: %v", ErrOptionsCommitOutcomeUnknown, transactionErr, err)
	}
	confirmedHead, err := ReadPublicationVersion(probeCtx, DB, PublicationOwnerOptions)
	if err != nil {
		return fmt.Errorf("%w: transaction error: %v; head confirmation failed: %v", ErrOptionsCommitOutcomeUnknown, transactionErr, err)
	}
	if confirmedHead != head {
		return fmt.Errorf("%w: transaction error: %v; head changed during target verification from %d to %d", ErrOptionsCommitOutcomeUnknown, transactionErr, head, confirmedHead)
	}
	if !reflect.DeepEqual(actual, target) {
		return fmt.Errorf("%w: transaction error: %v; committed head does not match target overrides", ErrOptionsCommitOutcomeUnknown, transactionErr)
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
