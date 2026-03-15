package model

import (
	"reflect"
	"testing"

	"one-api/common/config"
)

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
		return &Channel{
			Id:       7,
			Key:      "new-key",
			Weight:   &zeroWeight,
			Priority: &priority,
			Proxy:    &proxyTemplate,
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
