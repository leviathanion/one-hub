package model

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"one-api/common/config"

	"gorm.io/datatypes"
)

func TestChannelRuntimeConfigParsesOnFirstGetterAccess(t *testing.T) {
	modelMapping := `{"codex-mini":"codex-mini-latest"}`
	modelHeaders := `{"x-test":"header"}`
	customParameter := `{"temperature":0.2,"stream":true}`

	channel := &Channel{
		ModelMapping:    &modelMapping,
		ModelHeaders:    &modelHeaders,
		CustomParameter: &customParameter,
	}

	mapping, err := channel.GetModelMappingMap()
	if err != nil {
		t.Fatalf("expected model mapping to parse on demand, got %v", err)
	}
	if mapping["codex-mini"] != "codex-mini-latest" {
		t.Fatalf("unexpected model mapping: %#v", mapping)
	}

	headers, err := channel.GetModelHeadersMap()
	if err != nil {
		t.Fatalf("expected model headers to parse on demand, got %v", err)
	}
	if headers["x-test"] != "header" {
		t.Fatalf("unexpected model headers: %#v", headers)
	}

	params, err := channel.GetCustomParameterMap()
	if err != nil {
		t.Fatalf("expected custom parameters to parse on demand, got %v", err)
	}
	if params["temperature"] != 0.2 {
		t.Fatalf("unexpected custom parameters: %#v", params)
	}
}

func TestChannelsChooserRefreshChannelUpdatesTrackedKeyAndProxy(t *testing.T) {
	originalLoader := loadChannelByIDForChannelGroupRefresh
	t.Cleanup(func() {
		loadChannelByIDForChannelGroupRefresh = originalLoader
	})

	weight := uint(2)
	priority := int64(1)
	initialProxy := "http://proxy.example/old"
	originalRule := map[string]map[string][][]int{
		"default": {
			"codex-mini": {{7}},
		},
	}
	originalMatch := []string{"codex-*"}
	originalModelGroup := map[string]map[string]bool{
		"codex-mini": {
			"default": true,
		},
	}

	chooser := ChannelsChooser{
		Channels: map[int]*ChannelChoice{
			7: {
				Channel: &Channel{
					Id:       7,
					Key:      "old-key",
					Weight:   &weight,
					Priority: &priority,
					Proxy:    &initialProxy,
				},
				CooldownsTime: 42,
				Disable:       true,
			},
		},
		Rule:       originalRule,
		Match:      append([]string(nil), originalMatch...),
		ModelGroup: originalModelGroup,
	}

	zeroWeight := uint(0)
	loadChannelByIDForChannelGroupRefresh = func(id int) (*Channel, error) {
		if id != 7 {
			t.Fatalf("unexpected channel refresh id: got %d want 7", id)
		}
		proxyTemplate := "http://proxy.example/%s"
		modelMapping := `{"codex-mini":"codex-mini-latest"}`
		modelHeaders := `{"x-test":"header"}`
		customParameter := `{"pre_add":true,"temperature":0.2}`
		return &Channel{
			Id:              7,
			Key:             "new-key",
			Weight:          &zeroWeight,
			Priority:        &priority,
			Proxy:           &proxyTemplate,
			ModelMapping:    &modelMapping,
			ModelHeaders:    &modelHeaders,
			CustomParameter: &customParameter,
		}, nil
	}

	if err := chooser.RefreshChannel(7); err != nil {
		t.Fatalf("expected tracked channel refresh to succeed, got %v", err)
	}

	choice := chooser.Channels[7]
	if choice == nil || choice.Channel == nil {
		t.Fatalf("expected tracked channel choice to remain available")
	}
	if choice.Channel.Key != "new-key" {
		t.Fatalf("expected channel key to be replaced, got %q", choice.Channel.Key)
	}
	if choice.CooldownsTime != 42 {
		t.Fatalf("expected cooldown state to be preserved, got %d", choice.CooldownsTime)
	}
	if !choice.Disable {
		t.Fatalf("expected disable state to be preserved")
	}
	if choice.Channel.Weight == nil || *choice.Channel.Weight != config.DefaultChannelWeight {
		t.Fatalf("expected zero weight to normalize to default, got %#v", choice.Channel.Weight)
	}

	expectedProxy := "http://proxy.example/%s"
	expectedChannel := &Channel{Key: "new-key", Proxy: &expectedProxy}
	expectedChannel.SetProxy()
	if choice.Channel.Proxy == nil || *choice.Channel.Proxy != *expectedChannel.Proxy {
		t.Fatalf("expected proxy to be recomputed from refreshed key, got %v", choice.Channel.Proxy)
	}
	modelMapping, err := choice.Channel.GetModelMappingMap()
	if err != nil || modelMapping["codex-mini"] != "codex-mini-latest" {
		t.Fatalf("expected parsed model mapping to be available, got %v, %v", modelMapping, err)
	}
	modelHeaders, err := choice.Channel.GetModelHeadersMap()
	if err != nil || modelHeaders["x-test"] != "header" {
		t.Fatalf("expected parsed model headers to be available, got %v, %v", modelHeaders, err)
	}
	customParameter, err := choice.Channel.GetCustomParameterMap()
	if err != nil || customParameter["temperature"] != 0.2 {
		t.Fatalf("expected parsed custom parameters to be available, got %v, %v", customParameter, err)
	}

	if !reflect.DeepEqual(chooser.Rule, originalRule) {
		t.Fatalf("expected routing rules to remain unchanged")
	}
	if !reflect.DeepEqual(chooser.Match, originalMatch) {
		t.Fatalf("expected wildcard match state to remain unchanged")
	}
	if !reflect.DeepEqual(chooser.ModelGroup, originalModelGroup) {
		t.Fatalf("expected model group state to remain unchanged")
	}
}

func TestChannelsChooserRefreshChannelInvalidRuntimeConfigFailsClosed(t *testing.T) {
	originalLoader := loadChannelByIDForChannelGroupRefresh
	t.Cleanup(func() {
		loadChannelByIDForChannelGroupRefresh = originalLoader
	})

	weight := uint(1)
	chooser := ChannelsChooser{
		Channels: map[int]*ChannelChoice{
			7: {
				Channel: &Channel{
					Id:     7,
					Type:   config.ChannelTypeOpenAI,
					Weight: &weight,
				},
			},
		},
	}
	loadChannelByIDForChannelGroupRefresh = func(id int) (*Channel, error) {
		if id != 7 {
			t.Fatalf("unexpected channel refresh id: got %d want 7", id)
		}
		return &Channel{
			Id:    7,
			Type:  config.ChannelTypeOpenAI,
			Other: "2024-05-01-preview",
		}, nil
	}

	err := chooser.RefreshChannel(7)
	var invalid *InvalidChannelRuntimeConfigError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected invalid runtime config error, got %v", err)
	}
	if choice := chooser.Channels[7]; choice == nil || !choice.Disable {
		t.Fatalf("expected invalid refresh to fail-close existing channel, got %+v", choice)
	}
	if !chooser.isDirty() {
		t.Fatal("expected fail-closed refresh to mark chooser dirty")
	}
}

func TestChannelValidateRuntimeConfigJSONRejectsInvalidCodexOther(t *testing.T) {
	channel := &Channel{
		Type:  config.ChannelTypeCodex,
		Other: `{"prompt_cache_key_strategy":`,
	}
	if err := channel.ValidateRuntimeConfigJSON(); err == nil {
		t.Fatal("expected invalid Codex other JSON to be rejected")
	}
}

func TestChannelValidateRuntimeConfigJSONRejectsPlainOtherForAllChannels(t *testing.T) {
	channel := &Channel{
		Type:  config.ChannelTypeOpenAI,
		Other: "2024-05-01-preview",
	}
	if err := channel.ValidateRuntimeConfigJSON(); err == nil {
		t.Fatal("expected plain other field to be rejected")
	}

	channel.Other = `{"vendor_extra":{"api_version":"2024-05-01-preview"}}`
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected JSON object other field to remain valid, got %v", err)
	}
}

func TestCustomClaudeRelayConfigHelpers(t *testing.T) {
	plugin := datatypes.NewJSONType(PluginType{
		"claude": {
			"enabled":  true,
			"base_url": "https://proxy.example.com/root/",
		},
	})
	channel := &Channel{
		Type:   config.ChannelTypeCustom,
		Plugin: &plugin,
	}

	if !channel.CustomClaudeRelayEnabled() {
		t.Fatal("expected custom Claude relay to be enabled")
	}

	baseURL, err := channel.ResolveCustomClaudeBaseURL("https://api.anthropic.com")
	if err != nil {
		t.Fatalf("expected plugin Claude base_url to resolve, got %v", err)
	}
	if baseURL != "https://proxy.example.com/root" {
		t.Fatalf("unexpected resolved plugin Claude base_url: %q", baseURL)
	}

	channel.Plugin = func() *datatypes.JSONType[PluginType] {
		fallbackPlugin := datatypes.NewJSONType(PluginType{
			"claude": {
				"enabled": true,
			},
		})
		return &fallbackPlugin
	}()
	channel.BaseURL = testStringPtr("https://channel-base.example.com/api/")

	baseURL, err = channel.ResolveCustomClaudeBaseURL("https://api.anthropic.com")
	if err != nil {
		t.Fatalf("expected channel base_url fallback to resolve, got %v", err)
	}
	if baseURL != "https://channel-base.example.com/api" {
		t.Fatalf("unexpected channel base_url fallback: %q", baseURL)
	}

	channel.BaseURL = nil
	baseURL, err = channel.ResolveCustomClaudeBaseURL("https://api.anthropic.com")
	if err != nil {
		t.Fatalf("expected default Claude base_url fallback to resolve, got %v", err)
	}
	if baseURL != "https://api.anthropic.com" {
		t.Fatalf("unexpected default Claude base_url fallback: %q", baseURL)
	}

	channel.Plugin = func() *datatypes.JSONType[PluginType] {
		disabledPlugin := datatypes.NewJSONType(PluginType{
			"claude": {
				"enabled": false,
			},
		})
		return &disabledPlugin
	}()
	if channel.CustomClaudeRelayEnabled() {
		t.Fatal("expected disabled custom Claude relay to be reported as disabled")
	}
	if _, err := channel.ResolveCustomClaudeBaseURL("https://api.anthropic.com"); err == nil {
		t.Fatal("expected disabled custom Claude relay to reject resolution")
	}
}

func TestChannelInsertAndHydrateValidationBranches(t *testing.T) {
	useTestChannelDB(t)

	if err := BatchInsertChannels([]Channel{
		{
			Type:   config.ChannelTypeCodex,
			Name:   "bad-batch",
			Key:    "sk-batch",
			Group:  "default",
			Models: "gpt-5",
			Other:  `{"prompt_cache_key_strategy":`,
		},
	}); err == nil {
		t.Fatal("expected batch inserts to reject invalid Codex runtime config")
	}

	if err := (&Channel{
		Type:   config.ChannelTypeCodex,
		Name:   "bad-insert",
		Key:    "sk-insert",
		Group:  "default",
		Models: "gpt-5",
		Other:  `{"prompt_cache_key_strategy":`,
	}).Insert(); err == nil {
		t.Fatal("expected insert to reject invalid Codex runtime config")
	}

	if err := (&Channel{}).hydratePersistedTypeForUpdate(); err != nil {
		t.Fatalf("expected hydratePersistedTypeForUpdate to ignore zero-value channels, got %v", err)
	}

	if err := (&Channel{
		Id:     9999,
		Name:   "missing",
		Key:    "sk-missing",
		Group:  "default",
		Models: "gpt-5",
	}).UpdateRaw(false); err == nil {
		t.Fatal("expected updates for missing channels to fail while hydrating the persisted type")
	}
}

func TestChannelPersistenceCanonicalizesLegacyOther(t *testing.T) {
	useTestChannelDB(t)

	inserted := &Channel{
		Type:   config.ChannelTypeOpenAI,
		Name:   "openai-insert",
		Key:    "sk-insert",
		Group:  "default",
		Models: "gpt-5",
		Other:  "legacy-openai",
	}
	if err := inserted.Insert(); err != nil {
		t.Fatalf("expected insert to canonicalize OpenAI legacy other, got %v", err)
	}
	persisted, err := GetChannelById(inserted.Id)
	if err != nil {
		t.Fatalf("expected inserted OpenAI channel lookup to succeed, got %v", err)
	}
	assertJSONObjectsEqual(t, persisted.Other, `{"vendor_extra":{"legacy_other":"legacy-openai"}}`)

	if err := BatchInsertChannels([]Channel{
		{
			Type:   config.ChannelTypeCodex,
			Name:   "codex-batch",
			Key:    "sk-batch",
			Group:  "default",
			Models: "gpt-5",
			Other:  `{"websocket_mode":"required"}`,
		},
	}); err != nil {
		t.Fatalf("expected batch insert to canonicalize Codex legacy websocket_mode, got %v", err)
	}
	var batch Channel
	if err := DB.Where("name = ?", "codex-batch").First(&batch).Error; err != nil {
		t.Fatalf("expected batch-inserted Codex channel lookup to succeed, got %v", err)
	}
	assertJSONObjectsEqual(t, batch.Other, `{"websocket_mode":"force"}`)

	update := &Channel{
		Id:     inserted.Id,
		Type:   config.ChannelTypeOpenAI,
		Name:   "openai-update",
		Key:    "sk-update",
		Group:  "default",
		Models: "gpt-5",
		Other:  "legacy-update",
	}
	if err := update.UpdateRawWithOptions(false, ChannelUpdateOptions{OtherSubmitted: true}); err != nil {
		t.Fatalf("expected update to canonicalize OpenAI legacy other, got %v", err)
	}
	persisted, err = GetChannelById(inserted.Id)
	if err != nil {
		t.Fatalf("expected updated OpenAI channel lookup to succeed, got %v", err)
	}
	assertJSONObjectsEqual(t, persisted.Other, `{"vendor_extra":{"legacy_other":"legacy-update"}}`)
}

func TestBatchInsertChannelsCreatesLargeBatches(t *testing.T) {
	useTestChannelDB(t)

	channels := make([]Channel, 0, 1200)
	for i := 0; i < 1200; i++ {
		channels = append(channels, Channel{
			Type:   config.ChannelTypeCodex,
			Name:   fmt.Sprintf("codex-batch-%04d", i),
			Key:    fmt.Sprintf("sk-batch-%04d", i),
			Group:  "default",
			Models: "gpt-5",
		})
	}

	if err := BatchInsertChannels(channels); err != nil {
		t.Fatalf("expected 1200 channel batch insert to succeed, got %v", err)
	}

	var count int64
	if err := DB.Model(&Channel{}).Count(&count).Error; err != nil {
		t.Fatalf("expected channel count query to succeed, got %v", err)
	}
	if count != 1200 {
		t.Fatalf("expected 1200 persisted channels, got %d", count)
	}
}

func TestBatchInsertChannelsValidationFailureDoesNotWrite(t *testing.T) {
	useTestChannelDB(t)

	channels := make([]Channel, 0, 3)
	for i := 0; i < 3; i++ {
		channels = append(channels, Channel{
			Type:   config.ChannelTypeCodex,
			Name:   fmt.Sprintf("codex-validation-%d", i),
			Key:    fmt.Sprintf("sk-validation-%d", i),
			Group:  "default",
			Models: "gpt-5",
		})
	}
	channels[1].Other = `{"prompt_cache_key_strategy":`

	if err := BatchInsertChannels(channels); err == nil {
		t.Fatal("expected invalid runtime config to fail the whole batch")
	}

	var count int64
	if err := DB.Model(&Channel{}).Count(&count).Error; err != nil {
		t.Fatalf("expected channel count query to succeed, got %v", err)
	}
	if count != 0 {
		t.Fatalf("expected validation failure to write no channels, got %d", count)
	}
}

func TestBatchInsertChannelsRollsBackWhenLaterBatchFails(t *testing.T) {
	useTestChannelDB(t)

	channels := make([]Channel, 0, 250)
	for i := 0; i < 250; i++ {
		channels = append(channels, Channel{
			Type:   config.ChannelTypeCodex,
			Name:   fmt.Sprintf("codex-rollback-%03d", i),
			Key:    fmt.Sprintf("sk-rollback-%03d", i),
			Group:  "default",
			Models: "gpt-5",
		})
	}
	channels[0].Id = 9001
	channels[220].Id = 9001

	if err := BatchInsertChannels(channels); err == nil {
		t.Fatal("expected duplicate primary key in a later batch to fail")
	}

	var count int64
	if err := DB.Model(&Channel{}).Count(&count).Error; err != nil {
		t.Fatalf("expected channel count query to succeed, got %v", err)
	}
	if count != 0 {
		t.Fatalf("expected transaction rollback to write no channels, got %d", count)
	}
}

func TestChannelGetOtherMapParsesAndReparsesOtherJSON(t *testing.T) {
	channel := &Channel{
		Other: `{"prompt_cache_key_strategy":"token_id"}`,
	}

	other, err := channel.GetOtherMap()
	if err != nil {
		t.Fatalf("expected valid other json to parse, got %v", err)
	}
	if got := string(other["prompt_cache_key_strategy"]); got != `"token_id"` {
		t.Fatalf("expected parsed prompt cache strategy, got %s", got)
	}

	channel.Other = `{"websocket_mode":"force"}`
	other, err = channel.GetOtherMap()
	if err != nil {
		t.Fatalf("expected runtime config reparse after other change, got %v", err)
	}
	if got := string(other["websocket_mode"]); got != `"force"` {
		t.Fatalf("expected reparsed websocket mode, got %s", got)
	}
}

func TestChannelGetOtherMapReturnsParseErrors(t *testing.T) {
	channel := &Channel{
		Other: `{"prompt_cache_key_strategy":`,
	}

	other, err := channel.GetOtherMap()
	if err == nil {
		t.Fatal("expected invalid other json to return a parse error")
	}
	if other != nil {
		t.Fatalf("expected invalid other json not to return parsed data, got %+v", other)
	}
}
