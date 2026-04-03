package relay

import (
	"sync"

	"one-api/model"
)

type channelGroupSnapshot struct {
	channels   map[int]*model.ChannelChoice
	rule       map[string]map[string][][]int
	match      []string
	modelGroup map[string]map[string]bool
	cooldowns  map[any]any
}

func snapshotChannelGroup() channelGroupSnapshot {
	model.ChannelGroup.RLock()
	defer model.ChannelGroup.RUnlock()

	snapshot := channelGroupSnapshot{
		channels:   model.ChannelGroup.Channels,
		rule:       model.ChannelGroup.Rule,
		match:      append([]string(nil), model.ChannelGroup.Match...),
		modelGroup: model.ChannelGroup.ModelGroup,
		cooldowns:  make(map[any]any),
	}
	model.ChannelGroup.Cooldowns.Range(func(key, value any) bool {
		snapshot.cooldowns[key] = value
		return true
	})
	return snapshot
}

func restoreChannelGroup(snapshot channelGroupSnapshot) {
	model.ChannelGroup.Lock()
	defer model.ChannelGroup.Unlock()

	model.ChannelGroup.Channels = snapshot.channels
	model.ChannelGroup.Rule = snapshot.rule
	model.ChannelGroup.Match = append([]string(nil), snapshot.match...)
	model.ChannelGroup.ModelGroup = snapshot.modelGroup
	model.ChannelGroup.Cooldowns = sync.Map{}
	for key, value := range snapshot.cooldowns {
		model.ChannelGroup.Cooldowns.Store(key, value)
	}
}
