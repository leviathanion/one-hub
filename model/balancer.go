package model

import (
	"errors"
	"fmt"
	"math/rand"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/utils"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ChannelChoice struct {
	Channel       *Channel
	CooldownsTime int64
	Disable       bool
}

type ChannelsChooser struct {
	sync.RWMutex
	// Serialize DB snapshot/build/publish so an older reload cannot overwrite a
	// newer snapshot. Reads keep using the previous routing while reload builds.
	//
	// Trade-off: this protects process-local ordering without blocking routing
	// reads on DB I/O. During reload, readers may briefly see the previous
	// snapshot, which is acceptable for admin/config changes. Lifecycle changes
	// that must fail closed are handled separately by publishGeneration.
	reloadMu sync.Mutex
	// Dirty state is versioned instead of boolean. A successful load only marks the
	// generation it observed as clean, so a concurrent mutation cannot be erased by
	// an older in-flight reload.
	//
	// Trade-off: while dirty and DB is unhealthy, reads may synchronously retry a
	// serialized reload. This keeps recovery simple and immediate for the original
	// stale-cache problem. If DB outages make routing latency unacceptable, add a
	// small retry backoff here rather than expanding route tombstones.
	dirtyGeneration atomic.Uint64
	cleanGeneration atomic.Uint64
	// publishGeneration is bumped before fail-closed lifecycle mutations publish to
	// the current snapshot. A Load that started earlier must not overwrite that local
	// disable with stale DB rows. This is deliberately process-local; cross-node
	// consistency still converges through DB reloads instead of a distributed lock.
	//
	// Trade-off: we guarantee "this process will not keep routing to a channel it
	// just disabled/deleted" without taking a distributed lock. Other nodes may lag
	// until they observe the DB change, which matches the existing cache-aside model.
	publishGeneration atomic.Uint64
	Channels          map[int]*ChannelChoice
	Rule              map[string]map[string][][]int // group -> model -> priority -> channelIds
	Match             []string
	Cooldowns         sync.Map

	ModelGroup map[string]map[string]bool
}

type ChannelsFilterFunc func(channelId int, choice *ChannelChoice) bool

func FilterFunc(fn func(channelId int, choice *ChannelChoice) bool) ChannelsFilterFunc {
	return func(channelId int, choice *ChannelChoice) bool {
		if fn == nil {
			return false
		}
		return fn(channelId, choice)
	}
}

func FilterChannelId(skipChannelIds []int) ChannelsFilterFunc {
	return func(channelId int, _ *ChannelChoice) bool {
		return utils.Contains(channelId, skipChannelIds)
	}
}

func FilterChannelTypes(channelTypes []int) ChannelsFilterFunc {
	return func(_ int, choice *ChannelChoice) bool {
		return !utils.Contains(choice.Channel.Type, channelTypes)
	}
}

func FilterOnlyChat() ChannelsFilterFunc {
	return func(channelId int, choice *ChannelChoice) bool {
		return choice.Channel.OnlyChat
	}
}

func FilterDisabledStream(modelName string) ChannelsFilterFunc {
	return func(_ int, choice *ChannelChoice) bool {
		return !choice.Channel.AllowStream(modelName)
	}
}

func init() {
	// 每小时清理一次过期的冷却时间
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		for range ticker.C {
			ChannelGroup.CleanupExpiredCooldowns()
		}
	}()
}

func (cc *ChannelsChooser) SetCooldowns(channelId int, modelName string) bool {
	if channelId == 0 || modelName == "" || config.RetryCooldownSeconds == 0 {
		return false
	}

	key := fmt.Sprintf("%d:%s", channelId, modelName)
	nowTime := time.Now().Unix()

	cooldownTime, exists := cc.Cooldowns.Load(key)
	if exists && nowTime < cooldownTime.(int64) {
		return true
	}

	cc.Cooldowns.LoadOrStore(key, nowTime+int64(config.RetryCooldownSeconds))
	return true
}

func (cc *ChannelsChooser) IsInCooldown(channelId int, modelName string) bool {
	key := fmt.Sprintf("%d:%s", channelId, modelName)

	cooldownTime, exists := cc.Cooldowns.Load(key)
	if !exists {
		return false
	}

	return time.Now().Unix() < cooldownTime.(int64)
}

func (cc *ChannelsChooser) CleanupExpiredCooldowns() {
	now := time.Now().Unix()
	cc.Cooldowns.Range(func(key, value interface{}) bool {
		if now >= value.(int64) {
			cc.Cooldowns.Delete(key)
		}
		return true
	})
}

func (cc *ChannelsChooser) Disable(channelId int) {
	cc.Lock()
	defer cc.Unlock()
	if _, ok := cc.Channels[channelId]; !ok {
		return
	}

	cc.Channels[channelId].Disable = true
}

func (cc *ChannelsChooser) Enable(channelId int) {
	cc.Lock()
	defer cc.Unlock()
	if _, ok := cc.Channels[channelId]; !ok {
		return
	}

	cc.Channels[channelId].Disable = false
}

func (cc *ChannelsChooser) failClosedChannels(channelIds []int) {
	if len(channelIds) == 0 {
		return
	}

	// Lifecycle mutations fail closed before the DB snapshot reload. This fixes the
	// important failure mode where an auto-disabled channel stayed routable until a
	// later tag edit forced a successful rebuild.
	//
	// Trade-off: this only marks known channel IDs in the current process snapshot.
	// We intentionally do not add model/group scoped tombstones because route edits
	// are low-frequency admin operations and tombstones would need expiry/versioning
	// to avoid keeping channels dark after a cross-node re-enable.
	cc.publishGeneration.Add(1)
	cc.Lock()
	defer cc.Unlock()
	for _, channelId := range channelIds {
		if channelId <= 0 {
			continue
		}
		if choice, ok := cc.Channels[channelId]; ok && choice != nil {
			choice.Disable = true
		}
	}
	cc.markDirty()
}

func (cc *ChannelsChooser) ChangeStatus(channelId int, status bool) {
	if status {
		cc.Enable(channelId)
	} else {
		cc.Disable(channelId)
	}
}

func (cc *ChannelsChooser) balancer(channelIds []int, filters []ChannelsFilterFunc, modelName string) *Channel {
	totalWeight := 0

	validChannels := make([]*ChannelChoice, 0, len(channelIds))
	for _, channelId := range channelIds {
		choice, ok := cc.Channels[channelId]
		if !ok || choice.Disable {
			continue
		}

		if cc.IsInCooldown(channelId, modelName) {
			continue
		}

		isSkip := false
		for _, filter := range filters {
			if filter(channelId, choice) {
				isSkip = true
				break
			}
		}
		if isSkip {
			continue
		}

		weight := int(*choice.Channel.Weight)
		totalWeight += weight
		validChannels = append(validChannels, choice)
	}

	if len(validChannels) == 0 {
		return nil
	}

	if len(validChannels) == 1 {
		return validChannels[0].Channel
	}

	choiceWeight := rand.Intn(totalWeight)
	for _, choice := range validChannels {
		weight := int(*choice.Channel.Weight)
		choiceWeight -= weight
		if choiceWeight < 0 {
			return choice.Channel
		}
	}

	return nil
}

func (cc *ChannelsChooser) preferredChannel(channelIds []int, preferredChannelID int, ignoreCooldown bool, filters []ChannelsFilterFunc, modelName string) *Channel {
	if preferredChannelID <= 0 || !utils.Contains(preferredChannelID, channelIds) {
		return nil
	}

	choice, ok := cc.Channels[preferredChannelID]
	if !ok || choice.Disable {
		return nil
	}
	if !ignoreCooldown && cc.IsInCooldown(preferredChannelID, modelName) {
		return nil
	}

	for _, filter := range filters {
		if filter(preferredChannelID, choice) {
			return nil
		}
	}

	return choice.Channel
}

func (cc *ChannelsChooser) channelsPriority(group, modelName string) ([][]int, error) {
	if _, ok := cc.Rule[group]; !ok {
		return nil, errors.New("group not found")
	}

	channelsPriority, ok := cc.Rule[group][modelName]
	if !ok {
		matchModel := utils.GetModelsWithMatch(&cc.Match, modelName)
		channelsPriority, ok = cc.Rule[group][matchModel]
		if !ok {
			return nil, errors.New("model not found")
		}
	}

	if len(channelsPriority) == 0 {
		return nil, errors.New("channel not found")
	}

	return channelsPriority, nil
}

func (cc *ChannelsChooser) NextWithPreferred(group, modelName string, preferredChannelID int, ignorePreferredCooldown bool, filters ...ChannelsFilterFunc) (*Channel, error) {
	cc.reloadIfDirty()

	cc.RLock()
	defer cc.RUnlock()

	channelsPriority, err := cc.channelsPriority(group, modelName)
	if err != nil {
		return nil, err
	}

	for _, priority := range channelsPriority {
		if channel := cc.preferredChannel(priority, preferredChannelID, ignorePreferredCooldown, filters, modelName); channel != nil {
			return channel, nil
		}
	}

	for _, priority := range channelsPriority {
		channel := cc.balancer(priority, filters, modelName)
		if channel != nil {
			return channel, nil
		}
	}

	return nil, errors.New("channel not found")
}

func (cc *ChannelsChooser) PreferredChannelEligible(group, modelName string, preferredChannelID int, filters ...ChannelsFilterFunc) (bool, error) {
	if preferredChannelID <= 0 {
		return false, nil
	}

	cc.reloadIfDirty()

	cc.RLock()
	defer cc.RUnlock()

	channelsPriority, err := cc.channelsPriority(group, modelName)
	if err != nil {
		return false, err
	}

	for _, priority := range channelsPriority {
		if !utils.Contains(preferredChannelID, priority) {
			continue
		}

		choice, ok := cc.Channels[preferredChannelID]
		if !ok || choice == nil || choice.Channel == nil || choice.Disable {
			return false, nil
		}

		for _, filter := range filters {
			if filter(preferredChannelID, choice) {
				return false, nil
			}
		}

		return true, nil
	}

	return false, nil
}

func (cc *ChannelsChooser) Next(group, modelName string, filters ...ChannelsFilterFunc) (*Channel, error) {
	return cc.NextWithPreferred(group, modelName, 0, false, filters...)
}

func (cc *ChannelsChooser) GetGroupModels(group string) ([]string, error) {
	cc.reloadIfDirty()

	cc.RLock()
	defer cc.RUnlock()

	if _, ok := cc.Rule[group]; !ok {
		return nil, errors.New("group not found")
	}

	models := make([]string, 0, len(cc.Rule[group]))
	for model := range cc.Rule[group] {
		models = append(models, model)
	}

	return models, nil
}

func (cc *ChannelsChooser) ModelHasChannel(group string, modelName string, filters ...ChannelsFilterFunc) bool {
	cc.reloadIfDirty()

	cc.RLock()
	defer cc.RUnlock()

	channelsPriority, err := cc.channelsPriority(group, modelName)
	if err != nil {
		return false
	}

	for _, priority := range channelsPriority {
		for _, channelId := range priority {
			choice, ok := cc.Channels[channelId]
			if !ok || choice == nil || choice.Channel == nil || choice.Disable {
				continue
			}

			isSkip := false
			for _, filter := range filters {
				if filter(channelId, choice) {
					isSkip = true
					break
				}
			}
			if !isSkip {
				return true
			}
		}
	}

	return false
}

func (cc *ChannelsChooser) GetModelsGroups() map[string]map[string]bool {
	cc.reloadIfDirty()

	cc.RLock()
	defer cc.RUnlock()

	return cc.ModelGroup
}

func (cc *ChannelsChooser) GetChannel(channelId int) *Channel {
	cc.reloadIfDirty()

	cc.RLock()
	defer cc.RUnlock()

	if choice, ok := cc.Channels[channelId]; ok {
		return choice.Channel
	}

	return nil
}

var ChannelGroup = ChannelsChooser{}

func normalizeChannelForChooser(channel *Channel) {
	if channel == nil {
		return
	}

	channel.SetProxy()
	channel.ParseRuntimeConfig()
	if channel.Weight == nil || *channel.Weight == 0 {
		channel.Weight = &config.DefaultChannelWeight
	}
}

func (cc *ChannelsChooser) RefreshChannel(channelID int) error {
	if channelID <= 0 {
		return nil
	}

	channel, err := loadChannelByIDForChannelGroupRefresh(channelID)
	if err != nil {
		return err
	}
	normalizeChannelForChooser(channel)

	cc.Lock()
	defer cc.Unlock()

	if cc.Channels == nil {
		return nil
	}
	if choice, ok := cc.Channels[channelID]; ok && choice != nil {
		choice.Channel = channel
	}
	return nil
}

func (cc *ChannelsChooser) markDirty() {
	cc.dirtyGeneration.Add(1)
}

func (cc *ChannelsChooser) isDirty() bool {
	return cc.dirtyGeneration.Load() != cc.cleanGeneration.Load()
}

func (cc *ChannelsChooser) markCleanIfUnchanged(dirtyGeneration uint64) {
	if cc.dirtyGeneration.Load() == dirtyGeneration {
		cc.cleanGeneration.Store(dirtyGeneration)
	}
}

func (cc *ChannelsChooser) reloadIfDirty() {
	if !cc.isDirty() {
		return
	}

	// Dirty reloads are retried by the next routing read so a failed post-mutation
	// refresh can self-heal without requiring another admin save. reloadMu keeps the
	// retry serialized.
	//
	// Trade-off: there is no backoff here. That keeps recovery immediate and the
	// code small, but a long DB outage can add latency to reads while dirty. Prefer
	// adding bounded backoff here if this becomes operationally visible; do not move
	// route correctness into more cache-side tombstone state unless there is a real
	// product requirement for stronger consistency.
	cc.reloadMu.Lock()
	defer cc.reloadMu.Unlock()
	if !cc.isDirty() {
		return
	}

	_ = cc.loadLocked()
}

func (cc *ChannelsChooser) Load() error {
	cc.reloadMu.Lock()
	defer cc.reloadMu.Unlock()

	return cc.loadLocked()
}

func (cc *ChannelsChooser) loadLocked() error {
	loadDirtyGeneration := cc.dirtyGeneration.Load()
	loadPublishGeneration := cc.publishGeneration.Load()

	var channels []*Channel
	if err := DB.Where("status = ?", config.ChannelStatusEnabled).Find(&channels).Error; err != nil {
		// Never publish a partial/empty routing table after a DB read failure. The
		// previous snapshot is safer, especially after lifecycle fail-close already
		// removed the just-disabled channel locally.
		cc.markDirty()
		logger.SysError("failed to load channels: " + err.Error())
		return err
	}

	newGroup := make(map[string]map[string][][]int)
	newChannels := make(map[int]*ChannelChoice)
	newMatch := make(map[string]bool)
	newModelGroup := make(map[string]map[string]bool)

	type groupModelKey struct {
		group string
		model string
	}
	channelGroups := make(map[groupModelKey]map[int64][]int)

	// 处理每个channel
	for _, channel := range channels {
		normalizeChannelForChooser(channel)
		newChannels[channel.Id] = &ChannelChoice{
			Channel:       channel,
			CooldownsTime: 0,
			Disable:       false,
		}

		// 处理groups和models
		groups := strings.Split(channel.Group, ",")
		models := strings.Split(channel.Models, ",")

		for _, group := range groups {
			group = strings.TrimSpace(group)
			if group == "" {
				continue
			}

			for _, model := range models {
				model = strings.TrimSpace(model)
				if model == "" {
					continue
				}

				key := groupModelKey{group: group, model: model}
				if _, ok := channelGroups[key]; !ok {
					channelGroups[key] = make(map[int64][]int)
				}

				// 按priority分组存储channelId
				priority := *channel.Priority
				channelGroups[key][priority] = append(channelGroups[key][priority], channel.Id)

				// 处理通配符模型
				if strings.HasSuffix(model, "*") {
					newMatch[model] = true
				}

				// 初始化ModelGroup
				if _, ok := newModelGroup[model]; !ok {
					newModelGroup[model] = make(map[string]bool)
				}
				newModelGroup[model][group] = true
			}
		}
	}

	// 构建最终的newGroup结构
	for key, priorityMap := range channelGroups {
		// 初始化group和model的map
		if _, ok := newGroup[key.group]; !ok {
			newGroup[key.group] = make(map[string][][]int)
		}

		// 获取所有优先级并排序（从大到小）
		var priorities []int64
		for priority := range priorityMap {
			priorities = append(priorities, priority)
		}
		sort.Slice(priorities, func(i, j int) bool {
			return priorities[i] > priorities[j]
		})

		// 按优先级顺序构建[][]int
		var channelsList [][]int
		for _, priority := range priorities {
			channelsList = append(channelsList, priorityMap[priority])
		}

		newGroup[key.group][key.model] = channelsList
	}

	// 构建newMatchList
	newMatchList := make([]string, 0, len(newMatch))
	for match := range newMatch {
		newMatchList = append(newMatchList, match)
	}

	// 更新ChannelsChooser
	cc.Lock()
	if cc.publishGeneration.Load() != loadPublishGeneration {
		// A lifecycle mutation fail-closed a channel after this load started. Even
		// if this DB snapshot is internally valid, publishing it could re-enable the
		// locally disabled/deleted channel for a short window. Keep the current
		// snapshot and let the dirty retry perform a fresh load.
		cc.Unlock()
		cc.markDirty()
		logger.SysLog("channels Load skipped stale snapshot")
		return nil
	}
	cc.Rule = newGroup
	cc.Channels = newChannels
	cc.Match = newMatchList
	cc.ModelGroup = newModelGroup
	cc.Unlock()
	cc.markCleanIfUnchanged(loadDirtyGeneration)
	logger.SysLog("channels Load success")
	return nil
}
