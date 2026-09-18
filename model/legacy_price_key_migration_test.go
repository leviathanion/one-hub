package model

import (
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/internal/testutil/sqlitetest"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestNormalizeLegacyExtraRatioKeysRenamesToProviderNames(t *testing.T) {
	ratios := map[string]float64{
		"cached_read_tokens":           1,
		"cached_write_tokens":          2,
		"claude_cache_write_5m_tokens": 3,
		"claude_cache_write_1h_tokens": 4,
		"cached_tokens":                5,
	}
	if !normalizeLegacyExtraRatioKeys(ratios) {
		t.Fatal("expected legacy keys to report a change")
	}

	want := map[string]float64{
		config.UsageExtraCacheReadInputTokens:     1,
		config.UsageExtraCacheCreationInputTokens: 2,
		config.UsageExtraEphemeral5mInputTokens:   3,
		config.UsageExtraEphemeral1hInputTokens:   4,
		config.UsageExtraCache:                    5,
	}
	if len(ratios) != len(want) {
		t.Fatalf("expected normalized key set %+v, got %+v", want, ratios)
	}
	for key, value := range want {
		if ratios[key] != value {
			t.Fatalf("expected normalized ratio %s=%v, got %+v", key, value, ratios)
		}
	}
}

func TestNormalizeLegacyExtraRatioKeysKeepsExplicitProviderName(t *testing.T) {
	ratios := map[string]float64{
		"cached_write_tokens":                     1,
		config.UsageExtraCacheCreationInputTokens: 2,
	}
	if !normalizeLegacyExtraRatioKeys(ratios) {
		t.Fatal("expected legacy key to report a change")
	}

	if len(ratios) != 1 || ratios[config.UsageExtraCacheCreationInputTokens] != 2 {
		t.Fatalf("expected explicit provider-name key to win, got %+v", ratios)
	}
}

func TestNormalizeLegacyExtraRatioKeysReportsNoChange(t *testing.T) {
	ratios := map[string]float64{
		config.UsageExtraCacheCreationInputTokens: 2,
		config.UsageExtraCache:                    0.5,
	}
	if normalizeLegacyExtraRatioKeys(ratios) {
		t.Fatalf("expected provider-name keys to report no change, got %+v", ratios)
	}
}

func TestNormalizeLegacyRateRuleJSONRenamesExtraMultipliers(t *testing.T) {
	raw := []byte(`{"version":2,"speed":[{"id":"fast","when":{"input_tokens":{"gt":9007199254740993}},"multipliers":{"all":2,"extra_multipliers":{"cached_read_tokens":1,"cached_write_tokens":0.5}}}]}`)
	encoded, changed, err := normalizeLegacyRateRuleJSON(raw)
	if err != nil {
		t.Fatalf("expected legacy rate rules JSON to normalize, got %v", err)
	}
	if !changed {
		t.Fatal("expected legacy rate rule keys to report a change")
	}
	wire := string(encoded)
	for _, legacy := range []string{"cached_read_tokens", "cached_write_tokens"} {
		if strings.Contains(wire, legacy) {
			t.Fatalf("legacy rate rule key %q survived normalization: %s", legacy, wire)
		}
	}
	if !strings.Contains(wire, config.UsageExtraCacheReadInputTokens) || !strings.Contains(wire, config.UsageExtraCacheCreationInputTokens) {
		t.Fatalf("expected provider-name keys in normalized rate rules: %s", wire)
	}
	if !strings.Contains(wire, `"all":2`) || !strings.Contains(wire, `"gt":9007199254740993`) {
		t.Fatalf("expected unrelated fields and number text to survive normalization: %s", wire)
	}
}

func TestNormalizeLegacyRateRuleJSONReportsNoChange(t *testing.T) {
	raw := []byte(`{"version":2,"long_context":[{"id":"long","when":{"input_tokens":{"gt":9007199254740993}},"multipliers":{"input":2,"extra_multipliers":{"cache_read_input_tokens":1}}}]}`)
	encoded, changed, err := normalizeLegacyRateRuleJSON(raw)
	if err != nil {
		t.Fatalf("expected provider-name rate rules JSON to parse, got %v", err)
	}
	if changed {
		t.Fatal("expected provider-name rate rule keys to report no change")
	}
	if string(encoded) != string(raw) {
		t.Fatalf("expected unchanged raw JSON, got %s", encoded)
	}
}

func TestRenameProviderEvidenceKeysMigrationRewritesStoredConfig(t *testing.T) {
	logger.Logger = zap.NewNop()

	originalDB := DB
	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Price{}); err != nil {
		t.Fatalf("expected price schema migration to succeed, got %v", err)
	}
	DB = testDB
	t.Cleanup(func() { DB = originalDB })

	legacyExtra := `{"cached_write_tokens": 2}`
	legacyRules := `{"version":2,"speed":[{"id":"fast","when":{"speed":["fast"]},"multipliers":{"extra_multipliers":{"cached_read_tokens":1}}}]}`
	insert := func(model string, extra, rules any) {
		t.Helper()
		if err := testDB.Exec(`INSERT INTO prices (model, type, extra_ratios, rate_rules) VALUES (?, ?, ?, ?)`, model, TokensPriceType, extra, rules).Error; err != nil {
			t.Fatalf("expected price fixture %q to persist, got %v", model, err)
		}
	}
	insert("legacy-migrated", legacyExtra, legacyRules)
	insert("already-current", `{ "cache_creation_input_tokens" : 3 }`, `{"version":2,"long_context":[{"id":"long","when":{"input_tokens":{"gt":9007199254740993}},"multipliers":{"input":2}}]}`)
	insert("null-config", nil, nil)
	insert("empty-rules", "{}", "{}")

	if err := renameProviderEvidenceKeys().Migrate(testDB); err != nil {
		t.Fatalf("expected evidence key migration to succeed, got %v", err)
	}
	if err := renameProviderEvidenceKeys().Migrate(testDB); err != nil {
		t.Fatalf("expected repeated evidence key migration to succeed, got %v", err)
	}

	rawByModel := func(model string) (string, string, bool) {
		t.Helper()
		var raw struct {
			ExtraRatios *string
			RateRules   *string
		}
		if err := testDB.Table("prices").Select("extra_ratios", "rate_rules").Where("model = ?", model).Scan(&raw).Error; err != nil {
			t.Fatalf("expected raw config lookup for %q to succeed, got %v", model, err)
		}
		if raw.ExtraRatios == nil || raw.RateRules == nil {
			return "", "", false
		}
		return *raw.ExtraRatios, *raw.RateRules, true
	}

	extra, rules, present := rawByModel("legacy-migrated")
	if !present {
		t.Fatal("expected migrated price config to be present")
	}
	if strings.Contains(extra, "cached_write_tokens") {
		t.Fatalf("legacy extra ratio key survived in storage: %s", extra)
	}
	if !strings.Contains(extra, config.UsageExtraCacheCreationInputTokens) {
		t.Fatalf("expected provider-name key in stored extra ratios: %s", extra)
	}
	if strings.Contains(rules, "cached_read_tokens") {
		t.Fatalf("legacy rate rule key survived in storage: %s", rules)
	}
	if !strings.Contains(rules, config.UsageExtraCacheReadInputTokens) {
		t.Fatalf("expected provider-name key in stored rate rules: %s", rules)
	}

	// 已使用新键的行不应被迁移改写格式或数字文本。
	extra, rules, _ = rawByModel("already-current")
	if extra != `{ "cache_creation_input_tokens" : 3 }` {
		t.Fatalf("expected current extra ratios to stay untouched, got %s", extra)
	}
	if !strings.Contains(rules, `"gt":9007199254740993`) {
		t.Fatalf("expected current rate rules number text to stay untouched, got %s", rules)
	}

	// NULL 与空对象配置不应让迁移失败。
	if _, _, present := rawByModel("null-config"); present {
		t.Fatal("expected NULL config to stay NULL")
	}
	if extra, rules, _ = rawByModel("empty-rules"); extra != "{}" || rules != "{}" {
		t.Fatalf("expected empty config to stay untouched, got extra=%q rules=%q", extra, rules)
	}

	var stored Price
	if err := testDB.Where("model = ?", "legacy-migrated").First(&stored).Error; err != nil {
		t.Fatalf("expected migrated price to decode, got %v", err)
	}
	if stored.ExtraRatios.Data()[config.UsageExtraCacheCreationInputTokens] != 2 {
		t.Fatalf("expected migrated cache_creation_input_tokens=2, got %+v", stored.ExtraRatios.Data())
	}
}
