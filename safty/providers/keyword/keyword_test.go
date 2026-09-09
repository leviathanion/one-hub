package keyword

import (
	"testing"

	"one-api/common/config"
)

func TestCheckReadsCurrentKeywords(t *testing.T) {
	originalManager := config.GlobalOption
	t.Cleanup(func() { config.GlobalOption = originalManager })

	manager := config.NewOptionManager()
	keywords := "old-word"
	manager.RegisterString("SafeKeyWords", &keywords)
	config.GlobalOption = manager

	checker := NewKeywordChecker()
	publishKeywords(t, manager, 1, "old-word")
	result, err := checker.Check("contains old-word")
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if result.IsSafe {
		t.Fatal("the current keyword should be rejected")
	}

	publishKeywords(t, manager, 2, "new-word")
	result, err = checker.Check("contains old-word")
	if err != nil {
		t.Fatalf("Check returned error after keyword update: %v", err)
	}
	if !result.IsSafe {
		t.Fatal("a removed keyword should no longer be rejected")
	}

	result, err = checker.Check("contains new-word")
	if err != nil {
		t.Fatalf("Check returned error for the new keyword: %v", err)
	}
	if result.IsSafe {
		t.Fatal("Check should observe the newly published keyword")
	}
}

func publishKeywords(t *testing.T, manager *config.OptionManager, version int64, keywords string) {
	t.Helper()
	published, err := manager.PublishRuntimeOverrides(version, map[string]string{"SafeKeyWords": keywords})
	if err != nil {
		t.Fatalf("publish keywords: %v", err)
	}
	if !published {
		t.Fatalf("keyword version %d was not published", version)
	}
}
