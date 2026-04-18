package config

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// OptionHandler 定义配置项处理器接口
type OptionHandler interface {
	// SetValue 设置配置值
	SetValue(value string) error
	// GetValue 获取配置字符串值
	GetValue() string
}

type OptionVisibility uint8

const (
	OptionVisibilityUnspecified OptionVisibility = iota
	OptionVisibilityPublic
	OptionVisibilitySensitive
)

type OptionMetadata struct {
	// Visibility is fail-closed: unconfigured options stay hidden from public
	// APIs until callers explicitly mark them public or sensitive.
	Visibility OptionVisibility
	Aliases    []string
	Group      string
}

type SensitiveOptionStatus struct {
	Configured bool `json:"configured"`
}

const (
	OptionGroupGitHubOAuth            = "github_oauth"
	OptionGroupLarkOAuth              = "lark_oauth"
	OptionGroupOIDCAuth               = "oidc_auth"
	OptionGroupWeChatAuth             = "wechat_auth"
	OptionGroupTurnstile              = "turnstile"
	OptionGroupEmailDomainRestriction = "email_domain_restriction"
)

type optionEntry struct {
	handler  OptionHandler
	metadata OptionMetadata
}

// OptionManager 配置管理器
type OptionManager struct {
	entries map[string]*optionEntry
	aliases map[string]string
	mutex   *sync.RWMutex
}

var GlobalOption = NewOptionManager()

// NewOptionManager 创建配置管理器实例
func NewOptionManager() *OptionManager {
	return &OptionManager{
		entries: make(map[string]*optionEntry),
		aliases: make(map[string]string),
		mutex:   &sync.RWMutex{},
	}
}

// Register 注册配置项
func (cm *OptionManager) Register(key string, handler OptionHandler, defaultValue string) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	trimmedKey := strings.TrimSpace(key)
	cm.entries[trimmedKey] = &optionEntry{
		handler: handler,
	}
	cm.rebuildAliasesLocked()

	// 设置默认值
	if defaultValue != "" {
		handler.SetValue(defaultValue)
	}
}

func (cm *OptionManager) RegisterOption(key string, handler OptionHandler, metadata OptionMetadata, defaultValue string) {
	cm.Register(key, handler, defaultValue)
	if err := cm.Configure(key, metadata); err != nil {
		panic(err)
	}
}

func (cm *OptionManager) Configure(key string, metadata OptionMetadata) error {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	entry, exists := cm.entries[strings.TrimSpace(key)]
	if !exists {
		return fmt.Errorf("未知的配置项：%s", key)
	}
	sanitized, err := sanitizeOptionMetadata(metadata)
	if err != nil {
		return fmt.Errorf("配置项 %s %w", strings.TrimSpace(key), err)
	}
	entry.metadata = sanitized
	cm.rebuildAliasesLocked()
	return nil
}

// RegisterString 快速注册字符串配置
func (cm *OptionManager) RegisterString(key string, value *string) {
	cm.Register(key, &StringOptionHandler{
		value: value,
	}, "")
}

func (cm *OptionManager) RegisterStringOption(key string, value *string, metadata OptionMetadata) {
	cm.RegisterOption(key, &StringOptionHandler{
		value: value,
	}, metadata, "")
}

// RegisterBool 快速注册布尔配置
func (cm *OptionManager) RegisterBool(key string, value *bool) {
	cm.Register(key, &BoolOptionHandler{
		value: value,
	}, "")
}

func (cm *OptionManager) RegisterBoolOption(key string, value *bool, metadata OptionMetadata) {
	cm.RegisterOption(key, &BoolOptionHandler{
		value: value,
	}, metadata, "")
}

// RegisterInt 快速注册整数配置
func (cm *OptionManager) RegisterInt(key string, value *int) {
	cm.Register(key, &IntOptionHandler{
		value: value,
	}, "")
}

func (cm *OptionManager) RegisterIntOption(key string, value *int, metadata OptionMetadata) {
	cm.RegisterOption(key, &IntOptionHandler{
		value: value,
	}, metadata, "")
}

// RegisterFloat 快速注册浮点数配置
func (cm *OptionManager) RegisterFloat(key string, value *float64) {
	cm.Register(key, &FloatOptionHandler{
		value: value,
	}, "")
}

func (cm *OptionManager) RegisterFloatOption(key string, value *float64, metadata OptionMetadata) {
	cm.RegisterOption(key, &FloatOptionHandler{
		value: value,
	}, metadata, "")
}

// RegisterCustom 注册自定义处理函数的配置
func (cm *OptionManager) RegisterCustom(key string, getter func() string, setter func(string) error, defaultValue string) {
	cm.RegisterCustomOptionWithValidator(key, getter, setter, nil, OptionMetadata{}, defaultValue)
}

// RegisterCustomWithValidator 注册带校验函数的自定义配置
func (cm *OptionManager) RegisterCustomWithValidator(key string, getter func() string, setter func(string) error, validator func(string) error, defaultValue string) {
	cm.RegisterCustomOptionWithValidator(key, getter, setter, validator, OptionMetadata{}, defaultValue)
}

func (cm *OptionManager) RegisterCustomOption(key string, getter func() string, setter func(string) error, metadata OptionMetadata, defaultValue string) {
	cm.RegisterCustomOptionWithValidator(key, getter, setter, nil, metadata, defaultValue)
}

func (cm *OptionManager) RegisterCustomOptionWithValidator(key string, getter func() string, setter func(string) error, validator func(string) error, metadata OptionMetadata, defaultValue string) {
	cm.RegisterOption(key, &CustomOptionHandler{
		getter:    getter,
		setter:    setter,
		validator: validator,
	}, metadata, defaultValue)
}

// RegisterValue 注册一个值类型的配置项
func (cm *OptionManager) RegisterValue(key string) {
	cm.Register(key, &ValueOptionHandler{
		value: "",
	}, "")
}

func (cm *OptionManager) RegisterValueOption(key string, metadata OptionMetadata) {
	cm.RegisterOption(key, &ValueOptionHandler{
		value: "",
	}, metadata, "")
}

// Get 获取配置值(字符串)
func (cm *OptionManager) Get(key string) string {
	entry, _, exists := cm.getEntry(key)
	if !exists {
		return ""
	}
	return entry.handler.GetValue()
}

// Set 设置配置值
func (cm *OptionManager) Set(key string, value string) error {
	entry, normalizedKey, exists := cm.getEntry(key)
	if !exists {
		return fmt.Errorf("未知的配置项：%s", normalizedKey)
	}
	if err := validateOptionHandlerValue(entry.handler, value); err != nil {
		return err
	}

	return entry.handler.SetValue(value)
}

// Validate 校验配置值，但不实际写入
func (cm *OptionManager) Validate(key string, value string) error {
	entry, normalizedKey, exists := cm.getEntry(key)
	if !exists {
		return fmt.Errorf("未知的配置项：%s", normalizedKey)
	}
	return validateOptionHandlerValue(entry.handler, value)
}

func (cm *OptionManager) NormalizeKey(key string) string {
	normalized, _ := cm.resolveKey(key)
	return normalized
}

func (cm *OptionManager) GetAll() map[string]string {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	all := make(map[string]string)
	for key, entry := range cm.entries {
		all[key] = entry.handler.GetValue()
	}
	return all
}

func (cm *OptionManager) GetPublic() map[string]string {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	all := make(map[string]string)
	for key, entry := range cm.entries {
		if entry.metadata.Visibility != OptionVisibilityPublic {
			continue
		}
		all[key] = entry.handler.GetValue()
	}
	return all
}

func (cm *OptionManager) GetSensitiveStatuses() map[string]SensitiveOptionStatus {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	statuses := make(map[string]SensitiveOptionStatus)
	for key, entry := range cm.entries {
		if entry.metadata.Visibility != OptionVisibilitySensitive {
			continue
		}
		statuses[key] = SensitiveOptionStatus{
			Configured: strings.TrimSpace(entry.handler.GetValue()) != "",
		}
	}
	return statuses
}

func (cm *OptionManager) GetMetadata(key string) (OptionMetadata, bool) {
	entry, _, exists := cm.getEntry(key)
	if !exists {
		return OptionMetadata{}, false
	}
	return entry.metadata, true
}

func (cm *OptionManager) IsRegistered(key string) bool {
	_, _, exists := cm.getEntry(key)
	return exists
}

func (cm *OptionManager) getEntry(key string) (*optionEntry, string, bool) {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	normalizedKey, exists := cm.resolveKeyLocked(key)
	if !exists {
		return nil, normalizedKey, false
	}
	entry, exists := cm.entries[normalizedKey]
	return entry, normalizedKey, exists
}

func (cm *OptionManager) resolveKey(key string) (string, bool) {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()
	return cm.resolveKeyLocked(key)
}

func (cm *OptionManager) resolveKeyLocked(key string) (string, bool) {
	normalizedKey := strings.TrimSpace(key)
	if normalizedKey == "" {
		return "", false
	}
	canonicalKey, exists := cm.aliases[normalizedKey]
	if exists {
		return canonicalKey, true
	}
	return normalizedKey, false
}

func (cm *OptionManager) rebuildAliasesLocked() {
	aliases := make(map[string]string, len(cm.entries))
	for key, entry := range cm.entries {
		aliases[key] = key
		for _, alias := range entry.metadata.Aliases {
			trimmedAlias := strings.TrimSpace(alias)
			if trimmedAlias == "" {
				continue
			}
			aliases[trimmedAlias] = key
		}
	}
	cm.aliases = aliases
}

func sanitizeOptionMetadata(metadata OptionMetadata) (OptionMetadata, error) {
	if metadata.Visibility == OptionVisibilityUnspecified {
		return OptionMetadata{}, fmt.Errorf("必须显式声明可见性")
	}
	sanitized := OptionMetadata{
		Visibility: metadata.Visibility,
		Group:      strings.TrimSpace(metadata.Group),
	}
	if len(metadata.Aliases) == 0 {
		return sanitized, nil
	}
	sanitized.Aliases = make([]string, 0, len(metadata.Aliases))
	for _, alias := range metadata.Aliases {
		trimmedAlias := strings.TrimSpace(alias)
		if trimmedAlias == "" {
			continue
		}
		sanitized.Aliases = append(sanitized.Aliases, trimmedAlias)
	}
	return sanitized, nil
}

func validateOptionHandlerValue(handler OptionHandler, value string) error {
	if validator, ok := handler.(interface{ ValidateValue(string) error }); ok {
		return validator.ValidateValue(value)
	}
	return nil
}

// 以下是各种配置处理器的实现
type StringOptionHandler struct {
	value *string
}

func (h *StringOptionHandler) SetValue(value string) error {
	*h.value = value
	return nil
}

func (h *StringOptionHandler) GetValue() string {
	return *h.value
}

func (h *StringOptionHandler) ValidateValue(value string) error {
	return nil
}

type BoolOptionHandler struct {
	value *bool
}

func (h *BoolOptionHandler) SetValue(value string) error {
	parsed, err := parseStrictBoolOptionValue(value)
	if err != nil {
		return err
	}
	*h.value = parsed
	return nil
}

func (h *BoolOptionHandler) GetValue() string {
	if *h.value {
		return "true"
	}
	return "false"
}

func (h *BoolOptionHandler) ValidateValue(value string) error {
	_, err := parseStrictBoolOptionValue(value)
	return err
}

type IntOptionHandler struct {
	value *int
}

func (h *IntOptionHandler) SetValue(value string) error {
	val, err := strconv.Atoi(value)
	if err != nil {
		return err
	}
	*h.value = val
	return nil
}

func (h *IntOptionHandler) GetValue() string {
	return strconv.Itoa(*h.value)
}

func (h *IntOptionHandler) ValidateValue(value string) error {
	_, err := strconv.Atoi(value)
	return err
}

type FloatOptionHandler struct {
	value *float64
}

func (h *FloatOptionHandler) SetValue(value string) error {
	val, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return err
	}
	*h.value = val
	return nil
}

func (h *FloatOptionHandler) GetValue() string {
	return strconv.FormatFloat(*h.value, 'f', -1, 64)
}

func (h *FloatOptionHandler) ValidateValue(value string) error {
	_, err := strconv.ParseFloat(value, 64)
	return err
}

type CustomOptionHandler struct {
	getter    func() string
	setter    func(string) error
	validator func(string) error
}

func (h *CustomOptionHandler) SetValue(value string) error {
	return h.setter(value)
}

func (h *CustomOptionHandler) GetValue() string {
	return h.getter()
}

func (h *CustomOptionHandler) ValidateValue(value string) error {
	if h.validator == nil {
		return nil
	}
	return h.validator(value)
}

// ValueOptionHandler 用于存储非全局变量的字符串值
type ValueOptionHandler struct {
	value string
}

func (h *ValueOptionHandler) SetValue(value string) error {
	h.value = value
	return nil
}

func (h *ValueOptionHandler) GetValue() string {
	return h.value
}

func (h *ValueOptionHandler) ValidateValue(value string) error {
	return nil
}

func parseStrictBoolOptionValue(value string) (bool, error) {
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("布尔值必须是 true 或 false")
	}
}
