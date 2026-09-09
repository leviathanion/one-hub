package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/internal/testutil/sqlitetest"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRemoveLegacyMediaProxyOptionsIsAtomicAndIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	rows := []Option{
		{Key: "ChatImageRequestProxy", Value: "http://proxy.example"},
		{Key: "CFWorkerImageUrl", Value: "https://worker.example"},
		{Key: "CFWorkerImageKey", Value: "secret"},
		{Key: "SystemName", Value: "kept"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	migration := removeLegacyMediaProxyOptions()
	if err := migration.Migrate(db); err != nil {
		t.Fatal(err)
	}
	var remaining []Option
	if err := db.Order("key").Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Key != "SystemName" || remaining[0].Value != "kept" {
		t.Fatalf("migration removed non-target options or retained target options: %#v", remaining)
	}
	version, err := ReadPublicationVersion(context.Background(), db, PublicationOwnerOptions)
	if err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("options version=%d, want 2 after deleting legacy rows", version)
	}

	if err := migration.Migrate(db); err != nil {
		t.Fatalf("idempotent rerun failed: %v", err)
	}
	afterRerun, err := ReadPublicationVersion(context.Background(), db, PublicationOwnerOptions)
	if err != nil {
		t.Fatal(err)
	}
	if afterRerun != version {
		t.Fatalf("idempotent rerun advanced options version from %d to %d", version, afterRerun)
	}
}

func TestRemoveLegacyMediaProxyOptionsDoesNotPublishWithoutDeletion(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Option{Key: "SystemName", Value: "kept"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyMediaProxyOptions().Migrate(db); err != nil {
		t.Fatal(err)
	}
	version, err := ReadPublicationVersion(context.Background(), db, PublicationOwnerOptions)
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("options version=%d, want 1 when no legacy row was deleted", version)
	}
}

func TestRemoveLegacyMediaProxyOptionsRollsBackDeletionWhenRevisionCannotAdvance(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Option{Key: "CFWorkerImageKey", Value: "secret"}).Error; err != nil {
		t.Fatal(err)
	}

	if err := removeLegacyMediaProxyOptions().Migrate(db); err == nil {
		t.Fatal("migration succeeded without the options publication revision row")
	}
	var retained Option
	if err := db.Where("key = ?", "CFWorkerImageKey").Take(&retained).Error; err != nil {
		t.Fatalf("legacy option deletion was not rolled back: %v", err)
	}
}

func TestOtherMigrationPreservesNativeResponsesWSSettings(t *testing.T) {
	useTestChannelDB(t)
	if DB.Migrator().HasColumn(&Channel{}, "responses_ws_native") || DB.Migrator().HasColumn(&Channel{}, "responses_ws_self_hosted") {
		t.Fatal("Responses WS UI switches must remain in channel.other and must not create channel columns")
	}
	insertTestChannel(t, &Channel{Plugin: NewCustomEndpointPlugin(),
		Id:     100,
		Type:   config.ChannelTypeCustom,
		Name:   "custom",
		Key:    "sk",
		Group:  "default",
		Models: "custom-model",
		Other:  `{"responses_ws_native":true,"responses_ws_self_hosted":true,"vendor_extra":{"owner":"ops"}}`,
	})

	legacyMigration := migrateLegacyChannelOtherJSON()
	for iteration := 0; iteration < 2; iteration++ {
		if err := legacyMigration.Migrate(DB); err != nil {
			t.Fatalf("repeat current Other migration: %v", err)
		}
	}
	channel, err := GetChannelById(100)
	if err != nil {
		t.Fatalf("load migrated channel: %v", err)
	}
	assertJSONObjectsEqual(t, channel.Other, `{"responses_ws_native":true,"responses_ws_self_hosted":true,"vendor_extra":{"owner":"ops"}}`)
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("native WS settings and opaque vendor data must remain valid: %v", err)
	}
}

func TestMigrateLegacyChannelOtherJSONConvertsLosslessProviderFormats(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{Id: 1, Type: config.ChannelTypeAzure, Name: "azure", Key: "sk", Group: "default", Models: "gpt-5", Other: "2024-05-01-preview"})
	insertTestChannel(t, &Channel{Id: 2, Type: config.ChannelTypeGemini, Name: "gemini", Key: "sk", Group: "default", Models: "gemini-pro", Other: "v1"})
	insertTestChannel(t, &Channel{Id: 3, Type: config.ChannelTypeXunfei, Name: "xunfei", Key: "sk", Group: "default", Models: "SparkDesk", Other: "v3.5"})
	insertTestChannel(t, &Channel{Id: 4, Type: config.ChannelTypeAzureSpeech, Name: "speech", Key: "sk", Group: "default", Models: "tts-1", Other: "eastus"})
	insertTestChannel(t, &Channel{Id: 5, Type: config.ChannelTypeAli, Name: "ali", Key: "sk", Group: "default", Models: "qwen", Other: "plugin-a"})
	insertTestChannel(t, &Channel{Id: 6, Type: config.ChannelTypeVertexAI, Name: "vertex", Key: "sk", Group: "default", Models: "gemini-pro", Other: "us-central1|project-a"})
	insertTestChannel(t, &Channel{Id: 7, Type: config.ChannelTypeOpenAI, Name: "openai", Key: "sk", Group: "default", Models: "gpt-5", Other: "legacy-openai"})
	insertTestChannel(t, &Channel{Plugin: NewCustomEndpointPlugin(), Id: 8, Type: config.ChannelTypeCustom, Name: "custom", Key: "sk", Group: "default", Models: "gpt-5", Other: "legacy-custom"})

	if err := migrateLegacyChannelOtherJSON().Migrate(DB); err != nil {
		t.Fatalf("expected legacy Other migration to succeed, got %v", err)
	}

	expected := map[int]string{
		1: `{"api_version":"2024-05-01-preview"}`,
		2: `{"api_version":"v1"}`,
		3: `{"api_version":"v3.5"}`,
		4: `{"region":"eastus"}`,
		5: `{"dashscope_plugin":"plugin-a"}`,
		6: `{"region":"us-central1","project_id":"project-a"}`,
		7: `{"vendor_extra":{"legacy_other":"legacy-openai"}}`,
		8: `{"vendor_extra":{"legacy_other":"legacy-custom"}}`,
	}
	for id, want := range expected {
		channel, err := GetChannelById(id)
		if err != nil {
			t.Fatalf("lookup channel %d: %v", id, err)
		}
		assertJSONObjectsEqual(t, channel.Other, want)
		if err := channel.ValidateRuntimeConfigJSON(); err != nil {
			t.Fatalf("expected migrated channel %d to validate, got %v", id, err)
		}
	}
}

func TestMigrateLegacyChannelOtherJSONRollbackIsNoop(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{Id: 1, Type: config.ChannelTypeAzure, Name: "azure", Key: "sk", Group: "default", Models: "gpt-5", Other: "2024-05-01-preview"})

	migration := migrateLegacyChannelOtherJSON()
	if err := migration.Migrate(DB); err != nil {
		t.Fatalf("expected legacy Other migration to succeed, got %v", err)
	}
	if err := migration.Rollback(DB); err != nil {
		t.Fatalf("expected rollback no-op to succeed, got %v", err)
	}

	channel, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("lookup channel: %v", err)
	}
	if channel.Other != `{"api_version":"2024-05-01-preview"}` {
		t.Fatalf("expected rollback to leave migrated JSON unchanged, got %q", channel.Other)
	}
}

func TestMigrateLegacyChannelOtherJSONSkipsAmbiguousVertexValue(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{Id: 1, Type: config.ChannelTypeVertexAI, Name: "vertex", Key: "sk", Group: "default", Models: "gemini-pro", Other: "us-central1"})

	if err := migrateLegacyChannelOtherJSON().Migrate(DB); err != nil {
		t.Fatalf("expected ambiguous legacy Other migration to skip safely, got %v", err)
	}
	channel, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("lookup channel: %v", err)
	}
	if channel.Other != "us-central1" {
		t.Fatalf("expected ambiguous VertexAI Other to remain unchanged, got %q", channel.Other)
	}
}

func TestMigrateLegacyChannelOtherJSONLeavesMalformedJSONLikeValuesFailClosed(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{Id: 1, Type: config.ChannelTypeAzure, Name: "azure", Key: "sk", Group: "default", Models: "gpt-5", Other: `{"api_version":123`})
	insertTestChannel(t, &Channel{Id: 2, Type: config.ChannelTypeGemini, Name: "gemini", Key: "sk", Group: "default", Models: "gemini-pro", Other: `"v1"`})
	insertTestChannel(t, &Channel{Id: 3, Type: config.ChannelTypeAzureSpeech, Name: "speech", Key: "sk", Group: "default", Models: "tts-1", Other: `null`})

	if err := migrateLegacyChannelOtherJSON().Migrate(DB); err != nil {
		t.Fatalf("expected malformed JSON-like Other migration to skip safely, got %v", err)
	}
	expected := map[int]string{
		1: `{"api_version":123`,
		2: `"v1"`,
		3: `null`,
	}
	for id, want := range expected {
		channel, err := GetChannelById(id)
		if err != nil {
			t.Fatalf("lookup channel %d: %v", id, err)
		}
		if channel.Other != want {
			t.Fatalf("channel %d Other mismatch: got %q want %q", id, channel.Other, want)
		}
		if err := channel.ValidateRuntimeConfigJSON(); err == nil {
			t.Fatalf("expected channel %d malformed JSON-like Other to fail runtime validation", id)
		}
	}
}

func TestMigrateLegacyChannelOtherJSONUpdateFailureAbortsWithSafeLog(t *testing.T) {
	useTestChannelDB(t)
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	const rawOther = "2026-secret-api-version"
	insertTestChannel(t, &Channel{Id: 1, Type: config.ChannelTypeAzure, Name: "azure", Key: "sk", Group: "default", Models: "gpt-5", Other: rawOther})

	callbackName := "test:fail_legacy_other_update"
	if err := DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		tx.AddError(errors.New("forced update failure token=secret-token"))
	}); err != nil {
		t.Fatalf("register forced update callback: %v", err)
	}
	t.Cleanup(func() {
		_ = DB.Callback().Update().Remove(callbackName)
	})

	err := migrateLegacyChannelOtherJSON().Migrate(DB)
	if err == nil || !strings.Contains(err.Error(), "legacy channel Other JSON migration update failed") {
		t.Fatalf("expected migration update failure to abort, got %v", err)
	}

	channel, lookupErr := GetChannelById(1)
	if lookupErr != nil {
		t.Fatalf("lookup channel: %v", lookupErr)
	}
	if channel.Other != rawOther {
		t.Fatalf("expected failed migration not to mutate Other, got %q", channel.Other)
	}

	entries, _ := logger.GetLatestLogs(50)
	var failureLog string
	for i := len(entries) - 1; i >= 0; i-- {
		if strings.Contains(entries[i].Message, "legacy channel Other JSON migration failed") {
			failureLog = entries[i].Message
			break
		}
	}
	if failureLog == "" {
		t.Fatal("expected migration failure to be logged")
	}
	for _, want := range []string{"channel_id=1", "channel_type=3", "failed=1"} {
		if !strings.Contains(failureLog, want) {
			t.Fatalf("expected failure log to contain %q, got %q", want, failureLog)
		}
	}
	for _, forbidden := range []string{rawOther, "secret-token"} {
		if strings.Contains(failureLog, forbidden) {
			t.Fatalf("expected migration failure log not to leak %q, got %q", forbidden, failureLog)
		}
	}
}
