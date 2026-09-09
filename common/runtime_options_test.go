package common

import (
	"testing"

	"one-api/common/config"
)

func TestLogQuotaReadsCurrentPublication(t *testing.T) {
	manager := config.NewOptionManager()
	quotaPerUnit := 100.0
	displayInCurrency := false
	manager.RegisterFloatOption("QuotaPerUnit", &quotaPerUnit, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterBoolOption("DisplayInCurrencyEnabled", &displayInCurrency, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	original := config.GlobalOption
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = original })

	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"QuotaPerUnit": "100", "DisplayInCurrencyEnabled": "false"}); err != nil {
		t.Fatal(err)
	}
	if got := LogQuota(100); got != "100 点额度" {
		t.Fatalf("unexpected quota log before update: %q", got)
	}
	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{"QuotaPerUnit": "200", "DisplayInCurrencyEnabled": "true"}); err != nil {
		t.Fatal(err)
	}
	if got := LogQuota(100); got != "＄0.500000 额度" {
		t.Fatalf("quota log did not use current publication: %q", got)
	}
}
