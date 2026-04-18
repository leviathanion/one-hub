package config

import (
	"strconv"
	"strings"
)

type OptionUpdate struct {
	Key   string
	Value string
}

type PreparedOptionUpdates struct {
	Updates     []OptionUpdate
	UpdatedKeys []string
}

type OptionGroupValidationMode int

const (
	OptionGroupValidationStrict OptionGroupValidationMode = iota
	OptionGroupValidationAllowIncrementalRepair
)

type OptionValidationError struct {
	Key     string
	Message string
}

func (e *OptionValidationError) Error() string {
	return e.Message
}

type optionGroupRule struct {
	ErrorKey     string
	ErrorMessage string
	Violations   func(values map[string]string) []string
}

var optionGroupRules = map[string]optionGroupRule{
	OptionGroupGitHubOAuth: {
		ErrorKey:     "GitHubOAuthEnabled",
		ErrorMessage: "无法启用 GitHub OAuth，请先填入 GitHub Client Id 以及 GitHub Client Secret！",
		Violations: func(values map[string]string) []string {
			if values["GitHubOAuthEnabled"] != "true" {
				return nil
			}
			return missingRequiredOptionViolations(values, "GitHubClientId", "GitHubClientSecret")
		},
	},
	OptionGroupLarkOAuth: {
		ErrorKey:     "LarkAuthEnabled",
		ErrorMessage: "无法启用飞书登录，请先填入飞书 App Id 以及 App Secret！",
		Violations: func(values map[string]string) []string {
			if values["LarkAuthEnabled"] != "true" {
				return nil
			}
			return missingRequiredOptionViolations(values, "LarkClientId", "LarkClientSecret")
		},
	},
	OptionGroupOIDCAuth: {
		ErrorKey:     "OIDCAuthEnabled",
		ErrorMessage: "无法启用 OIDC，请先填入OIDC信息！",
		Violations: func(values map[string]string) []string {
			if values["OIDCAuthEnabled"] != "true" {
				return nil
			}
			return missingRequiredOptionViolations(values, "OIDCClientId", "OIDCClientSecret", "OIDCIssuer", "OIDCScopes", "OIDCUsernameClaims")
		},
	},
	OptionGroupWeChatAuth: {
		ErrorKey:     "WeChatAuthEnabled",
		ErrorMessage: "无法启用微信登录，请先填入微信登录相关配置信息！",
		Violations: func(values map[string]string) []string {
			if values["WeChatAuthEnabled"] != "true" {
				return nil
			}
			return missingRequiredOptionViolations(values, "WeChatServerAddress", "WeChatServerToken")
		},
	},
	OptionGroupTurnstile: {
		ErrorKey:     "TurnstileCheckEnabled",
		ErrorMessage: "无法启用 Turnstile 校验，请先填入 Turnstile 校验相关配置信息！",
		Violations: func(values map[string]string) []string {
			if values["TurnstileCheckEnabled"] != "true" {
				return nil
			}
			return missingRequiredOptionViolations(values, "TurnstileSiteKey", "TurnstileSecretKey")
		},
	},
	OptionGroupEmailDomainRestriction: {
		ErrorKey:     "EmailDomainRestrictionEnabled",
		ErrorMessage: "无法启用邮箱域名限制，请先填入限制的邮箱域名！",
		Violations: func(values map[string]string) []string {
			if values["EmailDomainRestrictionEnabled"] != "true" {
				return nil
			}
			if len(splitNonEmptyValues(values["EmailDomainWhitelist"], ",")) == 0 {
				return []string{"EmailDomainWhitelist"}
			}
			return nil
		},
	},
}

func PrepareOptionUpdates(requests []OptionUpdate, validationMode OptionGroupValidationMode) (*PreparedOptionUpdates, error) {
	initialValues := GlobalOption.GetAll()
	stagedValues := cloneOptionValues(initialValues)
	updates := make([]OptionUpdate, 0, len(requests))
	updatedKeys := make([]string, 0, len(requests))
	seenKeys := make(map[string]struct{}, len(requests))
	affectedGroups := make(map[string]struct{}, len(requests))

	for _, request := range requests {
		key := GlobalOption.NormalizeKey(request.Key)
		if key == "" {
			return nil, &OptionValidationError{Message: "配置项 key 不能为空"}
		}

		if _, exists := seenKeys[key]; exists {
			return nil, &OptionValidationError{
				Key:     key,
				Message: "请求中包含重复的配置项",
			}
		}

		if err := validateOptionValue(key, request.Value); err != nil {
			return nil, err
		}

		seenKeys[key] = struct{}{}
		if metadata, exists := GlobalOption.GetMetadata(key); exists && metadata.Group != "" {
			affectedGroups[metadata.Group] = struct{}{}
		}
		if currentValue, exists := stagedValues[key]; exists && currentValue == request.Value {
			continue
		}

		stagedValues[key] = request.Value
		updates = append(updates, OptionUpdate{
			Key:   key,
			Value: request.Value,
		})
		updatedKeys = append(updatedKeys, key)
	}

	if err := validateAffectedOptionGroups(initialValues, stagedValues, affectedGroups, validationMode); err != nil {
		return nil, err
	}

	return &PreparedOptionUpdates{
		Updates:     updates,
		UpdatedKeys: updatedKeys,
	}, nil
}

func validateOptionValue(key, value string) error {
	switch key {
	case "PreferredChannelWaitMilliseconds":
		wait, err := strconv.Atoi(value)
		if err != nil {
			return &OptionValidationError{
				Key:     key,
				Message: "Codex 首选渠道等待时间必须是整数毫秒！",
			}
		}
		if wait < 0 {
			return &OptionValidationError{
				Key:     key,
				Message: "Codex 首选渠道等待时间不能为负数！",
			}
		}
		return nil
	case "PreferredChannelWaitPollMilliseconds":
		wait, err := strconv.Atoi(value)
		if err != nil {
			return &OptionValidationError{
				Key:     key,
				Message: "Codex 首选渠道轮询等待时间必须是整数毫秒！",
			}
		}
		if wait < 0 {
			return &OptionValidationError{
				Key:     key,
				Message: "Codex 首选渠道轮询等待时间不能为负数！",
			}
		}
		return nil
	}

	if err := GlobalOption.Validate(key, value); err != nil {
		return &OptionValidationError{
			Key:     key,
			Message: err.Error(),
		}
	}
	return nil
}

func validateAffectedOptionGroups(initialValues, stagedValues map[string]string, groups map[string]struct{}, validationMode OptionGroupValidationMode) error {
	for group := range groups {
		rule, exists := optionGroupRules[group]
		if !exists {
			continue
		}
		if err := validateOptionGroup(initialValues, stagedValues, rule, validationMode); err != nil {
			return err
		}
	}
	return nil
}

func validateOptionGroup(initialValues, stagedValues map[string]string, rule optionGroupRule, validationMode OptionGroupValidationMode) error {
	stagedViolations := normalizeViolations(rule.Violations(stagedValues))
	if len(stagedViolations) == 0 {
		return nil
	}
	if validationMode == OptionGroupValidationStrict {
		return rule.validationError()
	}

	initialViolations := normalizeViolations(rule.Violations(initialValues))
	if len(initialViolations) == 0 {
		return rule.validationError()
	}

	// Keep the legacy single-key API repairable, but only while each accepted
	// write strictly shrinks the invalid state. No-op edits and destructive
	// edits stay blocked; grouped changes should use the atomic batch API.
	if allowsIncrementalRepair(initialViolations, stagedViolations) {
		return nil
	}
	return rule.validationError()
}

func (r optionGroupRule) validationError() error {
	return &OptionValidationError{
		Key:     r.ErrorKey,
		Message: r.ErrorMessage,
	}
}

func cloneOptionValues(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func missingRequiredOptionViolations(values map[string]string, keys ...string) []string {
	violations := make([]string, 0, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(values[key]) == "" {
			violations = append(violations, key)
		}
	}
	return violations
}

func splitNonEmptyValues(value, sep string) []string {
	parts := strings.Split(value, sep)
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		filtered = append(filtered, trimmed)
	}
	return filtered
}

func normalizeViolations(violations []string) []string {
	if len(violations) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(violations))
	normalized := make([]string, 0, len(violations))
	for _, violation := range violations {
		if violation == "" {
			continue
		}
		if _, exists := seen[violation]; exists {
			continue
		}
		seen[violation] = struct{}{}
		normalized = append(normalized, violation)
	}
	return normalized
}

func allowsIncrementalRepair(initialViolations, stagedViolations []string) bool {
	if len(initialViolations) == 0 || len(stagedViolations) >= len(initialViolations) {
		return false
	}
	initialSet := make(map[string]struct{}, len(initialViolations))
	for _, violation := range initialViolations {
		initialSet[violation] = struct{}{}
	}
	for _, violation := range stagedViolations {
		if _, exists := initialSet[violation]; !exists {
			return false
		}
	}
	return true
}
