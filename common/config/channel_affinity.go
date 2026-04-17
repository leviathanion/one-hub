package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	ChannelAffinityAliasPromptCacheKey = "prompt_cache_key"
	ChannelAffinityAliasResponseID     = "response_id"
	ChannelAffinityAliasSessionID      = "session_id"
)

type ChannelAffinityKeySource struct {
	Source     string `json:"source"`
	Key        string `json:"key"`
	Alias      string `json:"alias,omitempty"`
	ValueRegex string `json:"value_regex,omitempty"`
}

type ChannelAffinityRule struct {
	Name                    string                     `json:"name"`
	Enabled                 bool                       `json:"enabled"`
	Kind                    string                     `json:"kind"`
	ModelRegex              string                     `json:"model_regex,omitempty"`
	PathRegex               string                     `json:"path_regex,omitempty"`
	UserAgentRegex          string                     `json:"user_agent_regex,omitempty"`
	IncludeGroup            bool                       `json:"include_group"`
	IncludeModel            bool                       `json:"include_model"`
	IncludePath             bool                       `json:"include_path"`
	IncludeRuleName         bool                       `json:"include_rule_name"`
	IgnorePreferredCooldown bool                       `json:"ignore_preferred_cooldown"`
	Strict                  bool                       `json:"strict"`
	SkipRetryOnFailure      bool                       `json:"skip_retry_on_failure"`
	RecordOnSuccess         bool                       `json:"record_on_success"`
	TTLSeconds              int                        `json:"ttl_seconds"`
	KeySources              []ChannelAffinityKeySource `json:"key_sources,omitempty"`
}

type ChannelAffinitySettings struct {
	Enabled           bool                  `json:"enabled"`
	DefaultTTLSeconds int                   `json:"default_ttl_seconds"`
	MaxEntries        int                   `json:"max_entries"`
	Rules             []ChannelAffinityRule `json:"rules,omitempty"`
}

var ChannelAffinitySettingsInstance = DefaultChannelAffinitySettings()

func init() {
	GlobalOption.RegisterCustomOptionWithValidator("ChannelAffinitySetting", func() string {
		return ChannelAffinitySettingsInstance.JSONString()
	}, func(value string) error {
		return ChannelAffinitySettingsInstance.SetFromJSON(value)
	}, func(value string) error {
		settings := DefaultChannelAffinitySettings()
		return settings.SetFromJSON(value)
	}, OptionMetadata{
		Visibility: OptionVisibilityPublic,
	}, ChannelAffinitySettingsInstance.JSONString())
}

func DefaultChannelAffinitySettings() ChannelAffinitySettings {
	settings := ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 3600,
		MaxEntries:        50000,
		Rules: []ChannelAffinityRule{
			{
				Name:               "responses-continuation",
				Enabled:            true,
				Kind:               "responses",
				PathRegex:          "^/v1/responses(?:/compact)?$",
				IncludeGroup:       true,
				IncludeRuleName:    true,
				Strict:             true,
				SkipRetryOnFailure: true,
				RecordOnSuccess:    true,
				KeySources: []ChannelAffinityKeySource{
					{
						Source: "request_field",
						Key:    "previous_response_id",
						Alias:  ChannelAffinityAliasResponseID,
					},
				},
			},
			{
				Name:            "responses-prompt-cache-key",
				Enabled:         true,
				Kind:            "responses",
				PathRegex:       "^/v1/responses(?:/compact)?$",
				IncludeGroup:    true,
				IncludeModel:    true,
				IncludeRuleName: true,
				RecordOnSuccess: true,
				KeySources: []ChannelAffinityKeySource{
					{
						Source: "request_field",
						Key:    "prompt_cache_key",
						Alias:  ChannelAffinityAliasPromptCacheKey,
					},
					{
						Source: "request_hint",
						Key:    "responses.prompt_cache_key",
						Alias:  ChannelAffinityAliasPromptCacheKey,
					},
				},
			},
			{
				Name:            "realtime-session",
				Enabled:         true,
				Kind:            "realtime",
				PathRegex:       "^/v1/realtime$",
				IncludeGroup:    true,
				IncludeRuleName: true,
				RecordOnSuccess: true,
				KeySources: []ChannelAffinityKeySource{
					{
						Source: "header",
						Key:    "x-session-id",
						Alias:  ChannelAffinityAliasSessionID,
					},
					{
						Source: "header",
						Key:    "session_id",
						Alias:  ChannelAffinityAliasSessionID,
					},
				},
			},
		},
	}
	settings.Normalize()
	return settings
}

func (s *ChannelAffinitySettings) SetFromJSON(data string) error {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		*s = DefaultChannelAffinitySettings()
		return nil
	}

	settings := DefaultChannelAffinitySettings()
	if err := json.Unmarshal([]byte(trimmed), &settings); err != nil {
		return err
	}
	settings.Normalize()
	if err := settings.Validate(); err != nil {
		return err
	}
	*s = settings
	return nil
}

func (s ChannelAffinitySettings) Clone() ChannelAffinitySettings {
	cloned := s
	if len(s.Rules) == 0 {
		return cloned
	}

	cloned.Rules = make([]ChannelAffinityRule, 0, len(s.Rules))
	for _, rule := range s.Rules {
		clonedRule := rule
		if len(rule.KeySources) > 0 {
			clonedRule.KeySources = append([]ChannelAffinityKeySource(nil), rule.KeySources...)
		}
		cloned.Rules = append(cloned.Rules, clonedRule)
	}
	return cloned
}

func (s ChannelAffinitySettings) JSONString() string {
	normalized := s.Clone()
	normalized.Normalize()

	data, err := json.Marshal(normalized)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func (s *ChannelAffinitySettings) Normalize() {
	if s.DefaultTTLSeconds <= 0 {
		s.DefaultTTLSeconds = 3600
	}
	if s.MaxEntries <= 0 {
		s.MaxEntries = 50000
	}
	for index := range s.Rules {
		s.Rules[index].Normalize()
	}
}

func (r *ChannelAffinityRule) Normalize() {
	r.Name = strings.TrimSpace(r.Name)
	r.Kind = strings.ToLower(strings.TrimSpace(r.Kind))
	r.ModelRegex = strings.TrimSpace(r.ModelRegex)
	r.PathRegex = strings.TrimSpace(r.PathRegex)
	r.UserAgentRegex = strings.TrimSpace(r.UserAgentRegex)
	if !r.Enabled {
		return
	}
	for index := range r.KeySources {
		r.KeySources[index].Normalize()
	}
}

func (s *ChannelAffinityKeySource) Normalize() {
	s.Source = strings.ToLower(strings.TrimSpace(s.Source))
	s.Key = strings.TrimSpace(s.Key)
	s.Alias = normalizeChannelAffinityAlias(s.Alias, s.Key)
	s.ValueRegex = strings.TrimSpace(s.ValueRegex)
}

func normalizeChannelAffinityAlias(alias, fallback string) string {
	normalized := strings.ToLower(strings.TrimSpace(alias))
	if normalized != "" {
		return normalized
	}
	return strings.ToLower(strings.TrimSpace(fallback))
}

func (s ChannelAffinitySettings) Validate() error {
	for ruleIndex, rule := range s.Rules {
		if err := validateChannelAffinityRegex(fmt.Sprintf("rules[%d].model_regex", ruleIndex), rule.ModelRegex); err != nil {
			return err
		}
		if err := validateChannelAffinityRegex(fmt.Sprintf("rules[%d].path_regex", ruleIndex), rule.PathRegex); err != nil {
			return err
		}
		if err := validateChannelAffinityRegex(fmt.Sprintf("rules[%d].user_agent_regex", ruleIndex), rule.UserAgentRegex); err != nil {
			return err
		}
		for sourceIndex, source := range rule.KeySources {
			if err := validateChannelAffinityRegex(fmt.Sprintf("rules[%d].key_sources[%d].value_regex", ruleIndex, sourceIndex), source.ValueRegex); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateChannelAffinityRegex(fieldName, pattern string) error {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Errorf("%s 必须是合法的正则表达式: %w", fieldName, err)
	}
	return nil
}
