package config

import (
	"sync"
	"testing"
)

func TestRuntimeOptionsSnapshotPreservesOverrideSourceAndExplicitEmpty(t *testing.T) {
	manager := NewOptionManager()
	wait := 10
	secret := "default-secret"
	manager.RegisterIntOption("Wait", &wait, OptionMetadata{Visibility: OptionVisibilityPublic})
	manager.RegisterStringOption("Secret", &secret, OptionMetadata{Visibility: OptionVisibilitySensitive})
	if published, err := manager.PublishRuntimeOverrides(1, map[string]string{}); err != nil || !published {
		t.Fatalf("publish defaults: published=%v err=%v", published, err)
	}
	defaults := manager.RuntimeSnapshot()
	if value, ok := defaults.Get("Wait"); !ok || value.Effective != "10" || value.Source != RuntimeOptionSourceDefault || value.Override != nil {
		t.Fatalf("unexpected default option: %+v ok=%v", value, ok)
	}
	if published, err := manager.PublishRuntimeOverrides(2, map[string]string{"Secret": ""}); err != nil || !published {
		t.Fatalf("publish explicit empty override: published=%v err=%v", published, err)
	}
	override, ok := manager.RuntimeSnapshot().Get("Secret")
	if !ok || override.Source != RuntimeOptionSourceOverride || override.Override == nil || *override.Override != "" || override.Effective != "" {
		t.Fatalf("explicit empty override lost its source: %+v", override)
	}
	if status := manager.RuntimeSnapshot().SensitiveStatuses()["Secret"]; status.Configured {
		t.Fatalf("empty sensitive override must not expose a configured value: %+v", status)
	}
}

func TestRuntimeOptionsPublicationIsMonotonicAndValidationFailureKeepsLastGoodSnapshot(t *testing.T) {
	manager := NewOptionManager()
	wait := 10
	manager.RegisterIntOption("Wait", &wait, OptionMetadata{Visibility: OptionVisibilityPublic})
	if _, err := manager.PublishRuntimeOverrides(3, map[string]string{"Wait": "30"}); err != nil {
		t.Fatal(err)
	}
	if published, err := manager.PublishRuntimeOverrides(2, map[string]string{"Wait": "20"}); err != nil || published {
		t.Fatalf("older version publication result: published=%v err=%v", published, err)
	}
	if _, err := manager.PublishRuntimeOverrides(4, map[string]string{"Wait": "invalid"}); err == nil {
		t.Fatal("invalid snapshot was published")
	}
	if snapshot := manager.RuntimeSnapshot(); snapshot.Version() != 3 || snapshot.EffectiveValues()["Wait"] != "30" {
		t.Fatalf("last-good snapshot was replaced: version=%d values=%v", snapshot.Version(), snapshot.EffectiveValues())
	}
}

func TestRuntimeOptionsSnapshotRejectsNonFiniteFloat(t *testing.T) {
	manager := NewOptionManager()
	value := 1.0
	manager.RegisterFloatOption("Ratio", &value, OptionMetadata{Visibility: OptionVisibilityPublic})
	for _, invalid := range []string{"NaN", "+Inf", "-Inf"} {
		if _, err := manager.ValidateRuntimeOverrides(map[string]string{"Ratio": invalid}); err == nil {
			t.Fatalf("non-finite float %q was accepted", invalid)
		}
	}
}

func TestRuntimeOptionsReadersObserveOnlyCompleteSnapshots(t *testing.T) {
	manager := NewOptionManager()
	left, right := "a", "a"
	manager.RegisterStringOption("Left", &left, OptionMetadata{Visibility: OptionVisibilityPublic})
	manager.RegisterStringOption("Right", &right, OptionMetadata{Visibility: OptionVisibilityPublic})
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"Left": "a", "Right": "a"}); err != nil {
		t.Fatal(err)
	}

	var readers sync.WaitGroup
	readerErr := make(chan string, 1)
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for n := 0; n < 2000; n++ {
				values := manager.RuntimeSnapshot().EffectiveValues()
				if values["Left"] != values["Right"] {
					select {
					case readerErr <- values["Left"] + "/" + values["Right"]:
					default:
					}
					return
				}
			}
		}()
	}
	for version := int64(2); version < 30; version++ {
		value := "a"
		if version%2 == 0 {
			value = "b"
		}
		if _, err := manager.PublishRuntimeOverrides(version, map[string]string{"Left": value, "Right": value}); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
	select {
	case observed := <-readerErr:
		t.Fatalf("reader observed a torn snapshot: %s", observed)
	default:
	}
}

func TestRuntimeOptionsPublicationDoesNotMutateLegacyBackingVariables(t *testing.T) {
	manager := NewOptionManager()
	enabled := false
	wait := 10
	manager.RegisterBoolOption("Enabled", &enabled, OptionMetadata{Visibility: OptionVisibilityPublic})
	manager.RegisterIntOption("Wait", &wait, OptionMetadata{Visibility: OptionVisibilityPublic})
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"Enabled": "true", "Wait": "25"}); err != nil {
		t.Fatal(err)
	}
	if enabled || wait != 10 {
		t.Fatalf("atomic publication mutated legacy variables: enabled=%v wait=%d", enabled, wait)
	}
	snapshot := manager.RuntimeSnapshot()
	if !snapshot.Bool("Enabled", false) || snapshot.Int("Wait", 0) != 25 {
		t.Fatalf("published snapshot missing overrides: %+v", snapshot.EffectiveValues())
	}
}
