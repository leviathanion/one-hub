package config

import (
	"errors"
	"testing"
)

func TestOptionManagerRejectsUnknownKeys(t *testing.T) {
	manager := NewOptionManager()

	if err := manager.Validate("MissingOption", "value"); err == nil {
		t.Fatal("expected unknown option validation to fail")
	}
	if err := manager.Set("MissingOption", "value"); err == nil {
		t.Fatal("expected unknown option set to fail")
	}
}

func TestBoolOptionHandlerRequiresCanonicalValues(t *testing.T) {
	manager := NewOptionManager()
	flag := false
	manager.RegisterBool("FeatureEnabled", &flag)

	if err := manager.Set("FeatureEnabled", "true"); err != nil {
		t.Fatalf("expected canonical true to succeed, got %v", err)
	}
	if !flag {
		t.Fatal("expected canonical true to update bool")
	}
	if err := manager.Validate("FeatureEnabled", "false"); err != nil {
		t.Fatalf("expected canonical false to validate, got %v", err)
	}

	invalidValues := []string{"TRUE", "1", " true "}
	for _, value := range invalidValues {
		if err := manager.Validate("FeatureEnabled", value); err == nil {
			t.Fatalf("expected %q validation to fail", value)
		}
		if err := manager.Set("FeatureEnabled", value); err == nil {
			t.Fatalf("expected %q set to fail", value)
		}
	}
	if !flag {
		t.Fatal("expected invalid bool values not to mutate the existing value")
	}
}

func TestOptionManagerRejectsUnspecifiedVisibilityMetadata(t *testing.T) {
	manager := NewOptionManager()
	value := ""
	manager.RegisterString("LooseOption", &value)

	if err := manager.Configure("LooseOption", OptionMetadata{}); err == nil {
		t.Fatal("expected unspecified visibility metadata to fail")
	}
}

func TestOptionManagerNormalizesAliasesAndFiltersSensitiveValues(t *testing.T) {
	manager := NewOptionManager()

	waitMilliseconds := 10
	manager.RegisterIntOption("PreferredChannelWaitMilliseconds", &waitMilliseconds, OptionMetadata{
		Visibility: OptionVisibilityPublic,
		Aliases:    []string{"LegacyPreferredChannelWaitMilliseconds"},
	})

	secret := ""
	manager.RegisterStringOption("GitHubClientSecret", &secret, OptionMetadata{
		Visibility: OptionVisibilitySensitive,
		Group:      OptionGroupGitHubOAuth,
	})

	if normalized := manager.NormalizeKey(" LegacyPreferredChannelWaitMilliseconds "); normalized != "PreferredChannelWaitMilliseconds" {
		t.Fatalf("expected alias normalization to map legacy key, got %q", normalized)
	}
	if err := manager.Set("LegacyPreferredChannelWaitMilliseconds", "27"); err != nil {
		t.Fatalf("expected alias set to succeed, got %v", err)
	}
	if waitMilliseconds != 27 {
		t.Fatalf("expected alias set to update target value, got %d", waitMilliseconds)
	}

	if err := manager.Set("GitHubClientSecret", "super-secret"); err != nil {
		t.Fatalf("expected secret set to succeed, got %v", err)
	}

	publicOptions := manager.GetPublic()
	if publicOptions["PreferredChannelWaitMilliseconds"] != "27" {
		t.Fatalf("expected public options to include interval, got %#v", publicOptions)
	}
	if _, exists := publicOptions["GitHubClientSecret"]; exists {
		t.Fatalf("expected sensitive options to be hidden from public options, got %#v", publicOptions)
	}

	metadata, exists := manager.GetMetadata("LegacyPreferredChannelWaitMilliseconds")
	if !exists {
		t.Fatal("expected alias metadata lookup to succeed")
	}
	if metadata.Group != "" {
		t.Fatalf("expected alias metadata group to stay empty, got %q", metadata.Group)
	}

	statuses := manager.GetSensitiveStatuses()
	githubStatus, exists := statuses["GitHubClientSecret"]
	if !exists {
		t.Fatalf("expected sensitive status for GitHubClientSecret, got %#v", statuses)
	}
	if !githubStatus.Configured {
		t.Fatalf("expected sensitive status to report configured secret, got %#v", githubStatus)
	}
}

func TestPrepareOptionUpdatesRejectsNoProgressGroupedRepair(t *testing.T) {
	originalOptionManager := GlobalOption
	originalGitHubOAuthEnabled := GitHubOAuthEnabled
	originalGitHubClientID := GitHubClientId
	originalGitHubClientSecret := GitHubClientSecret
	t.Cleanup(func() {
		GlobalOption = originalOptionManager
		GitHubOAuthEnabled = originalGitHubOAuthEnabled
		GitHubClientId = originalGitHubClientID
		GitHubClientSecret = originalGitHubClientSecret
	})

	GlobalOption = NewOptionManager()
	GitHubOAuthEnabled = false
	GitHubClientId = ""
	GitHubClientSecret = ""
	GlobalOption.RegisterBoolOption("GitHubOAuthEnabled", &GitHubOAuthEnabled, OptionMetadata{
		Visibility: OptionVisibilityPublic,
		Group:      OptionGroupGitHubOAuth,
	})
	GlobalOption.RegisterStringOption("GitHubClientId", &GitHubClientId, OptionMetadata{
		Visibility: OptionVisibilityPublic,
		Group:      OptionGroupGitHubOAuth,
	})
	GlobalOption.RegisterStringOption("GitHubClientSecret", &GitHubClientSecret, OptionMetadata{
		Visibility: OptionVisibilitySensitive,
		Group:      OptionGroupGitHubOAuth,
	})
	if err := GlobalOption.Set("GitHubOAuthEnabled", "true"); err != nil {
		t.Fatalf("expected invalid github enablement seed to succeed, got %v", err)
	}

	_, err := PrepareOptionUpdates([]OptionUpdate{{
		Key:   "GitHubClientId",
		Value: "",
	}}, OptionGroupValidationAllowIncrementalRepair)
	var validationErr *OptionValidationError
	if err == nil || !errors.As(err, &validationErr) {
		t.Fatalf("expected no-progress grouped repair to fail, got %v", err)
	}
	if validationErr.Key != "GitHubOAuthEnabled" {
		t.Fatalf("expected failed_key GitHubOAuthEnabled, got %#v", validationErr)
	}
}

func TestPrepareOptionUpdatesRejectsWorseningInvalidGroup(t *testing.T) {
	originalOptionManager := GlobalOption
	originalGitHubOAuthEnabled := GitHubOAuthEnabled
	originalGitHubClientID := GitHubClientId
	originalGitHubClientSecret := GitHubClientSecret
	t.Cleanup(func() {
		GlobalOption = originalOptionManager
		GitHubOAuthEnabled = originalGitHubOAuthEnabled
		GitHubClientId = originalGitHubClientID
		GitHubClientSecret = originalGitHubClientSecret
	})

	GlobalOption = NewOptionManager()
	GitHubOAuthEnabled = false
	GitHubClientId = "cli_seed"
	GitHubClientSecret = ""
	GlobalOption.RegisterBoolOption("GitHubOAuthEnabled", &GitHubOAuthEnabled, OptionMetadata{
		Visibility: OptionVisibilityPublic,
		Group:      OptionGroupGitHubOAuth,
	})
	GlobalOption.RegisterStringOption("GitHubClientId", &GitHubClientId, OptionMetadata{
		Visibility: OptionVisibilityPublic,
		Group:      OptionGroupGitHubOAuth,
	})
	GlobalOption.RegisterStringOption("GitHubClientSecret", &GitHubClientSecret, OptionMetadata{
		Visibility: OptionVisibilitySensitive,
		Group:      OptionGroupGitHubOAuth,
	})
	if err := GlobalOption.Set("GitHubOAuthEnabled", "true"); err != nil {
		t.Fatalf("expected invalid github enablement seed to succeed, got %v", err)
	}

	_, err := PrepareOptionUpdates([]OptionUpdate{{
		Key:   "GitHubClientId",
		Value: "",
	}}, OptionGroupValidationAllowIncrementalRepair)
	var validationErr *OptionValidationError
	if err == nil || !errors.As(err, &validationErr) {
		t.Fatalf("expected worsening grouped repair to fail, got %v", err)
	}
	if validationErr.Key != "GitHubOAuthEnabled" {
		t.Fatalf("expected failed_key GitHubOAuthEnabled, got %#v", validationErr)
	}
}
