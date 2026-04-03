package model

import (
	"reflect"
	"testing"

	"one-api/common/config"
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

func TestChannelValidateRuntimeConfigJSONRejectsInvalidCodexOther(t *testing.T) {
	channel := &Channel{
		Type:  config.ChannelTypeCodex,
		Other: `{"prompt_cache_key_strategy":`,
	}
	if err := channel.ValidateRuntimeConfigJSON(); err == nil {
		t.Fatal("expected invalid Codex other JSON to be rejected")
	}
}

func TestChannelValidateRuntimeConfigJSONAllowsPlainOtherForNonCodexChannels(t *testing.T) {
	channel := &Channel{
		Type:  3,
		Other: "2024-05-01-preview",
	}
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		t.Fatalf("expected non-Codex plain other field to remain valid, got %v", err)
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
