package model

import (
	"bytes"
	"encoding/json"

	"one-api/common/config"
)

// 本文件只服务历史价格配置的一次性迁移：把旧内部证据键改写为 provider 原始字段名。
// 迁移之外不再识别旧键，运行时其他路径不读取旧键。

var legacyExtraRatioKeyRenames = map[string]string{
	"cached_read_tokens":           config.UsageExtraCacheReadInputTokens,
	"cached_write_tokens":          config.UsageExtraCacheCreationInputTokens,
	"claude_cache_write_5m_tokens": config.UsageExtraEphemeral5mInputTokens,
	"claude_cache_write_1h_tokens": config.UsageExtraEphemeral1hInputTokens,
}

// normalizeLegacyExtraRatioKeys 就地把旧证据键改写为 provider 原始字段名，
// 返回是否发生改写；新旧键同时存在时保留显式的新键。
func normalizeLegacyExtraRatioKeys(ratios map[string]float64) bool {
	changed := false
	for legacy, canonical := range legacyExtraRatioKeyRenames {
		value, ok := ratios[legacy]
		if !ok {
			continue
		}
		if _, explicit := ratios[canonical]; !explicit {
			ratios[canonical] = value
		}
		delete(ratios, legacy)
		changed = true
	}
	return changed
}

func normalizeLegacyExtraRatioValues(ratios map[string]any) bool {
	changed := false
	for legacy, canonical := range legacyExtraRatioKeyRenames {
		value, ok := ratios[legacy]
		if !ok {
			continue
		}
		if _, explicit := ratios[canonical]; !explicit {
			ratios[canonical] = value
		}
		delete(ratios, legacy)
		changed = true
	}
	return changed
}

// normalizeLegacyRateRuleJSON 解析原始 rate_rules JSON，把 extra_multipliers 中的
// 旧证据键改写为 provider 原始字段名，其余字段和数字文本原样保留。
func normalizeLegacyRateRuleJSON(raw []byte) ([]byte, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, false, err
	}
	if !normalizeLegacyRateRuleNode(root) {
		return raw, false, nil
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return nil, false, err
	}
	return encoded, true, nil
}

func normalizeLegacyRateRuleNode(node any) bool {
	changed := false
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "extra_multipliers" {
				if extras, ok := child.(map[string]any); ok {
					changed = normalizeLegacyExtraRatioValues(extras) || changed
				}
				continue
			}
			changed = normalizeLegacyRateRuleNode(child) || changed
		}
	case []any:
		for _, child := range value {
			changed = normalizeLegacyRateRuleNode(child) || changed
		}
	}
	return changed
}
