package config

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

func (s *RuntimeOptionsSnapshot) String(key, fallback string) string {
	value, ok := s.Get(key)
	if !ok {
		return fallback
	}
	return value.Effective
}

func (s *RuntimeOptionsSnapshot) Bool(key string, fallback bool) bool {
	value, ok := s.Get(key)
	if !ok {
		return fallback
	}
	parsed, err := parseStrictBoolOptionValue(value.Effective)
	if err != nil {
		return fallback
	}
	return parsed
}

func (s *RuntimeOptionsSnapshot) Int(key string, fallback int) int {
	value, ok := s.Get(key)
	if !ok {
		return fallback
	}
	parsed, err := strconv.Atoi(value.Effective)
	if err != nil {
		return fallback
	}
	return parsed
}

func (s *RuntimeOptionsSnapshot) Float64(key string, fallback float64) float64 {
	value, ok := s.Get(key)
	if !ok {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value.Effective, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return fallback
	}
	return parsed
}

func (s *RuntimeOptionsSnapshot) Strings(key string, fallback []string, separator string) []string {
	value, ok := s.Get(key)
	if !ok {
		return append([]string(nil), fallback...)
	}
	parts := strings.Split(value.Effective, separator)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

type RuntimeOptionSource string

const (
	RuntimeOptionSourceDefault  RuntimeOptionSource = "default"
	RuntimeOptionSourceOverride RuntimeOptionSource = "override"
)

type RuntimeOptionValue struct {
	Effective  string
	Override   *string
	Source     RuntimeOptionSource
	Visibility OptionVisibility
}

type RuntimeOptionsSnapshot struct {
	version int64
	values  map[string]RuntimeOptionValue
}

func (s *RuntimeOptionsSnapshot) Version() int64 {
	if s == nil {
		return 0
	}
	return s.version
}

func (s *RuntimeOptionsSnapshot) Get(key string) (RuntimeOptionValue, bool) {
	if s == nil {
		return RuntimeOptionValue{}, false
	}
	value, ok := s.values[strings.TrimSpace(key)]
	if !ok {
		return RuntimeOptionValue{}, false
	}
	return cloneRuntimeOptionValue(value), true
}

func (s *RuntimeOptionsSnapshot) EffectiveValues() map[string]string {
	values := make(map[string]string, len(s.values))
	if s == nil {
		return values
	}
	for key, value := range s.values {
		values[key] = value.Effective
	}
	return values
}

func (s *RuntimeOptionsSnapshot) PublicEffectiveValues() map[string]string {
	values := make(map[string]string)
	if s == nil {
		return values
	}
	for key, value := range s.values {
		if value.Visibility == OptionVisibilityPublic {
			values[key] = value.Effective
		}
	}
	return values
}

func (s *RuntimeOptionsSnapshot) SensitiveStatuses() map[string]SensitiveOptionStatus {
	statuses := make(map[string]SensitiveOptionStatus)
	if s == nil {
		return statuses
	}
	for key, value := range s.values {
		if value.Visibility == OptionVisibilitySensitive {
			statuses[key] = SensitiveOptionStatus{Configured: strings.TrimSpace(value.Effective) != ""}
		}
	}
	return statuses
}

func cloneRuntimeOptionValue(value RuntimeOptionValue) RuntimeOptionValue {
	cloned := value
	if value.Override != nil {
		override := *value.Override
		cloned.Override = &override
	}
	return cloned
}

func (cm *OptionManager) RuntimeSnapshot() *RuntimeOptionsSnapshot {
	if cm == nil {
		return nil
	}
	return cm.runtime.Load()
}

func (cm *OptionManager) ValidateRuntimeOverrides(overrides map[string]string) (*RuntimeOptionsSnapshot, error) {
	return cm.buildRuntimeSnapshot(0, overrides)
}

func (cm *OptionManager) PublishRuntimeOverrides(version int64, overrides map[string]string) (bool, error) {
	if cm == nil || version < 1 {
		return false, fmt.Errorf("runtime option version must be positive")
	}
	cm.publishMu.Lock()
	defer cm.publishMu.Unlock()
	if current := cm.runtime.Load(); current != nil && current.version >= version {
		return false, nil
	}
	snapshot, err := cm.buildRuntimeSnapshot(version, overrides)
	if err != nil {
		return false, err
	}
	cm.runtime.Store(snapshot)
	return true, nil
}

func (cm *OptionManager) buildRuntimeSnapshot(version int64, overrides map[string]string) (*RuntimeOptionsSnapshot, error) {
	if cm == nil {
		return nil, fmt.Errorf("option registry is required")
	}
	cm.mutex.RLock()
	values := make(map[string]RuntimeOptionValue, len(cm.entries))
	for key, entry := range cm.entries {
		values[key] = RuntimeOptionValue{
			Effective:  entry.defaultValue,
			Source:     RuntimeOptionSourceDefault,
			Visibility: entry.metadata.Visibility,
		}
	}
	cm.mutex.RUnlock()

	for rawKey, overrideValue := range overrides {
		key := cm.NormalizeKey(rawKey)
		entry, _, exists := cm.getEntry(key)
		if !exists {
			return nil, &OptionValidationError{Key: key, Message: "未知的配置项：" + key}
		}
		if err := validateOptionValueWithManager(cm, key, overrideValue); err != nil {
			return nil, err
		}
		override := overrideValue
		values[key] = RuntimeOptionValue{
			Effective:  overrideValue,
			Override:   &override,
			Source:     RuntimeOptionSourceOverride,
			Visibility: entry.metadata.Visibility,
		}
	}
	effective := make(map[string]string, len(values))
	for key, value := range values {
		effective[key] = value.Effective
	}
	groups := make([]string, 0, len(optionGroupRules))
	for group := range optionGroupRules {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		if violations := optionGroupRules[group].Violations(effective); len(violations) > 0 {
			return nil, optionGroupRules[group].validationError()
		}
	}
	return &RuntimeOptionsSnapshot{version: version, values: values}, nil
}
