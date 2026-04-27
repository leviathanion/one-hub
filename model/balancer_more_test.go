package model

import (
	"testing"
	"time"

	"one-api/common/config"
)

func testWeightedChannel(id, channelType int) *Channel {
	weight := uint(1)
	return &Channel{
		Id:     id,
		Type:   channelType,
		Weight: &weight,
	}
}

func TestChannelsChooserFilterHelpersAndCooldownLifecycle(t *testing.T) {
	choice := &ChannelChoice{Channel: &Channel{Type: config.ChannelTypeCodex, OnlyChat: true}}

	if FilterFunc(nil)(1, choice) {
		t.Fatal("expected FilterFunc(nil) to return false")
	}
	if !FilterChannelId([]int{1, 2})(2, choice) {
		t.Fatal("expected FilterChannelId to skip listed channel")
	}
	if FilterChannelId([]int{1, 2})(3, choice) {
		t.Fatal("expected FilterChannelId to keep unlisted channel")
	}
	if FilterChannelTypes([]int{config.ChannelTypeCodex})(1, choice) {
		t.Fatal("expected FilterChannelTypes to keep allowed type")
	}
	if !FilterChannelTypes([]int{config.ChannelTypeOpenAI})(1, choice) {
		t.Fatal("expected FilterChannelTypes to skip disallowed type")
	}
	if !FilterOnlyChat()(1, choice) {
		t.Fatal("expected FilterOnlyChat to skip chat-only channels")
	}

	chooser := &ChannelsChooser{
		Channels: map[int]*ChannelChoice{
			1: {Channel: testWeightedChannel(1, config.ChannelTypeCodex)},
		},
	}

	originalCooldown := config.RetryCooldownSeconds
	config.RetryCooldownSeconds = 1
	t.Cleanup(func() {
		config.RetryCooldownSeconds = originalCooldown
	})

	if chooser.SetCooldowns(0, "gpt-5") {
		t.Fatal("expected zero channel cooldown to be ignored")
	}
	if !chooser.SetCooldowns(1, "gpt-5") {
		t.Fatal("expected cooldown to be recorded")
	}
	if !chooser.IsInCooldown(1, "gpt-5") {
		t.Fatal("expected channel to be in cooldown")
	}
	chooser.Cooldowns.Store("1:gpt-5", time.Now().Unix()-1)
	chooser.CleanupExpiredCooldowns()
	if chooser.IsInCooldown(1, "gpt-5") {
		t.Fatal("expected expired cooldown to be removed")
	}

	chooser.Disable(1)
	if !chooser.Channels[1].Disable {
		t.Fatal("expected Disable to mark channel disabled")
	}
	chooser.Enable(1)
	if chooser.Channels[1].Disable {
		t.Fatal("expected Enable to re-enable channel")
	}
	chooser.ChangeStatus(1, false)
	if !chooser.Channels[1].Disable {
		t.Fatal("expected ChangeStatus(false) to disable")
	}
	chooser.ChangeStatus(1, true)
	if chooser.Channels[1].Disable {
		t.Fatal("expected ChangeStatus(true) to enable")
	}
}

func TestChannelsChooserModelHasChannel(t *testing.T) {
	chooser := &ChannelsChooser{
		Channels: map[int]*ChannelChoice{
			1: {Channel: testWeightedChannel(1, config.ChannelTypeCustom)},
			2: {Channel: testWeightedChannel(2, config.ChannelTypeOpenAI), Disable: true},
		},
		Rule: map[string]map[string][][]int{
			"default": {
				"custom-model": {{1, 2}},
			},
		},
	}

	if !chooser.ModelHasChannel("default", "custom-model") {
		t.Fatal("expected model with available channel to be reported")
	}
	if chooser.ModelHasChannel("default", "missing-model") {
		t.Fatal("expected missing model to have no channel")
	}
	if chooser.ModelHasChannel("default", "custom-model", FilterChannelTypes([]int{config.ChannelTypeOpenAI})) {
		t.Fatal("expected filters to exclude the only enabled matching channel")
	}
}

func TestChannelsChooserPreferredSelectionAndPriorityRouting(t *testing.T) {
	chooser := &ChannelsChooser{
		Channels: map[int]*ChannelChoice{
			1: {Channel: testWeightedChannel(1, config.ChannelTypeCodex)},
			2: {Channel: testWeightedChannel(2, config.ChannelTypeCodex)},
		},
		Rule: map[string]map[string][][]int{
			"default": {
				"gpt-5":   {{1, 2}},
				"codex-*": {{2}},
			},
		},
		Match: []string{"codex-*"},
	}

	if channel := chooser.preferredChannel([]int{1, 2}, 0, false, nil, "gpt-5"); channel != nil {
		t.Fatalf("expected no preferred channel when id is zero, got %#v", channel)
	}
	chooser.Channels[2].Disable = true
	if channel := chooser.preferredChannel([]int{1, 2}, 2, false, nil, "gpt-5"); channel != nil {
		t.Fatalf("expected disabled preferred channel to be rejected, got %#v", channel)
	}
	chooser.Channels[2].Disable = false
	chooser.Cooldowns.Store("2:gpt-5", time.Now().Unix()+60)
	if channel := chooser.preferredChannel([]int{1, 2}, 2, false, nil, "gpt-5"); channel != nil {
		t.Fatalf("expected preferred channel in cooldown to be rejected, got %#v", channel)
	}
	if channel := chooser.preferredChannel([]int{1, 2}, 2, true, nil, "gpt-5"); channel == nil || channel.Id != 2 {
		t.Fatalf("expected ignoreCooldown preferred selection to choose channel 2, got %#v", channel)
	}
	if channel := chooser.preferredChannel([]int{1, 2}, 2, true, []ChannelsFilterFunc{FilterChannelId([]int{2})}, "gpt-5"); channel != nil {
		t.Fatalf("expected preferred channel to honor filters, got %#v", channel)
	}
	chooser.Cooldowns.Delete("2:gpt-5")

	if _, err := chooser.channelsPriority("missing", "gpt-5"); err == nil {
		t.Fatal("expected missing group to fail")
	}
	if _, err := chooser.channelsPriority("default", "missing-model"); err == nil {
		t.Fatal("expected missing model to fail")
	}
	if priorities, err := chooser.channelsPriority("default", "codex-mini"); err != nil || len(priorities) != 1 || len(priorities[0]) != 1 || priorities[0][0] != 2 {
		t.Fatalf("expected wildcard model match to resolve channel 2, got priorities=%v err=%v", priorities, err)
	}

	channel, err := chooser.NextWithPreferred("default", "gpt-5", 2, false)
	if err != nil || channel == nil || channel.Id != 2 {
		t.Fatalf("expected preferred channel routing to pick channel 2, got channel=%#v err=%v", channel, err)
	}
	channel, err = chooser.NextWithPreferred("default", "gpt-5", 2, false, FilterChannelId([]int{2}))
	if err != nil || channel == nil || channel.Id != 1 {
		t.Fatalf("expected fallback routing to pick remaining channel 1, got channel=%#v err=%v", channel, err)
	}
	if channel := chooser.balancer([]int{2}, []ChannelsFilterFunc{FilterChannelId([]int{2})}, "gpt-5"); channel != nil {
		t.Fatalf("expected balancer to return nil when filters reject all channels, got %#v", channel)
	}
	if channel := chooser.balancer([]int{1}, nil, "gpt-5"); channel == nil || channel.Id != 1 {
		t.Fatalf("expected single-channel balancer to return channel 1, got %#v", channel)
	}
}

func TestChannelsChooserPreferredEligibilityAndNextWrapperBranches(t *testing.T) {
	choice := &ChannelChoice{Channel: testWeightedChannel(3, config.ChannelTypeCodex)}
	invoked := false
	if !FilterFunc(func(channelId int, current *ChannelChoice) bool {
		invoked = true
		return channelId == 3 && current == choice
	})(3, choice) || !invoked {
		t.Fatal("expected FilterFunc to delegate to the provided predicate")
	}

	chooser := &ChannelsChooser{
		Channels: map[int]*ChannelChoice{
			1: {Channel: testWeightedChannel(1, config.ChannelTypeCodex)},
		},
		Rule: map[string]map[string][][]int{
			"default": {
				"gpt-5": {{1}},
			},
		},
	}

	if ok, err := chooser.PreferredChannelEligible("default", "gpt-5", 0); err != nil || ok {
		t.Fatalf("expected zero preferred channel ids to be ignored, ok=%v err=%v", ok, err)
	}
	if ok, err := chooser.PreferredChannelEligible("missing", "gpt-5", 1); err == nil || ok {
		t.Fatalf("expected missing groups to surface an error, ok=%v err=%v", ok, err)
	}
	if ok, err := chooser.PreferredChannelEligible("default", "gpt-5", 2); err != nil || ok {
		t.Fatalf("expected non-priority preferred channels to be ineligible, ok=%v err=%v", ok, err)
	}

	chooser.Channels[1] = nil
	if ok, err := chooser.PreferredChannelEligible("default", "gpt-5", 1); err != nil || ok {
		t.Fatalf("expected nil tracked choices to be ineligible, ok=%v err=%v", ok, err)
	}

	chooser.Channels[1] = &ChannelChoice{Channel: testWeightedChannel(1, config.ChannelTypeCodex)}
	if ok, err := chooser.PreferredChannelEligible("default", "gpt-5", 1, FilterChannelId([]int{1})); err != nil || ok {
		t.Fatalf("expected preferred channel filters to be honored, ok=%v err=%v", ok, err)
	}
	if channel, err := chooser.Next("default", "gpt-5"); err != nil || channel == nil || channel.Id != 1 {
		t.Fatalf("expected Next wrapper to delegate to preferred routing, channel=%#v err=%v", channel, err)
	}
}
