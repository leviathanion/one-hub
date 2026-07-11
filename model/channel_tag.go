package model

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"one-api/common/config"
	"reflect"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// Tag-level config sync is declared on Channel fields with tag_config:"sync"
// instead of in a second update whitelist. The trade-off is that adding or
// removing a tag-level setting still requires touching the model field, but the
// data model stays the single source of truth for what belongs to the tag. The
// controller passes the JSON fields that were actually submitted so omitted
// fields are preserved while submitted zero values still propagate.
const channelTagConfigSyncTag = "sync"

type ChannelTagSubmittedFields map[string]struct{}

type channelTagMutationLockEntry struct {
	semaphore chan struct{}
	refs      int
}

var channelTagMutationLocks = struct {
	sync.Mutex
	entries map[string]*channelTagMutationLockEntry
}{entries: make(map[string]*channelTagMutationLockEntry)}

// lockChannelTagMutation serializes a tag's snapshot, commit, and in-process
// side effects. It deliberately does not pretend to coordinate multiple masters:
// that would require a database/advisory lock. The supported master/slave
// deployment has one mutation master, so adding schema solely for that case is
// not justified.
func lockChannelTagMutation(ctx context.Context, tag string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	channelTagMutationLocks.Lock()
	entry := channelTagMutationLocks.entries[tag]
	if entry == nil {
		entry = &channelTagMutationLockEntry{semaphore: make(chan struct{}, 1)}
		entry.semaphore <- struct{}{}
		channelTagMutationLocks.entries[tag] = entry
	}
	entry.refs++
	channelTagMutationLocks.Unlock()

	select {
	case <-ctx.Done():
		channelTagMutationLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(channelTagMutationLocks.entries, tag)
		}
		channelTagMutationLocks.Unlock()
		return nil, ctx.Err()
	case <-entry.semaphore:
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.semaphore <- struct{}{}
			channelTagMutationLocks.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(channelTagMutationLocks.entries, tag)
			}
			channelTagMutationLocks.Unlock()
		})
	}, nil
}

type SearchChannelsTagParams struct {
	Tag string `json:"tag" form:"tag"`
	PaginationParams
}

type ChannelTag struct {
	ID  int    `json:"id"`
	Tag string `json:"tag"`
}

func GetChannelsTagList(tag string) ([]*Channel, error) {
	var channels []*Channel
	err := DB.Model(&Channel{}).Where("tag = ?", tag).Find(&channels).Error
	return channels, err
}

func GetChannelsTagAllList() ([]*ChannelTag, error) {
	var channelTags []*ChannelTag
	err := DB.Model(&Channel{}).
		Select("tag").
		Where("tag != ''").
		Group("tag").
		Find(&channelTags).Error

	return channelTags, err
}

func ChannelTagExists(tag string) (bool, error) {
	if tag == "" {
		return false, nil
	}

	var count int64
	err := DB.Model(&Channel{}).Where("tag = ?", tag).Count(&count).Error
	return count > 0, err
}

type ChannelTagCollection struct {
	Channel
	KeyMap map[string]int
}

type channelTagConfigField struct {
	index    int
	jsonName string
}

func ParseChannelTagSubmittedFields(rawBody []byte) (ChannelTagSubmittedFields, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return nil, err
	}

	fields := make(ChannelTagSubmittedFields, len(payload))
	for field := range payload {
		fields[field] = struct{}{}
	}
	return fields, nil
}

func channelTagConfigFields(submittedFields ChannelTagSubmittedFields) []channelTagConfigField {
	channelType := reflect.TypeOf(Channel{})
	fields := make([]channelTagConfigField, 0, channelType.NumField())
	for i := 0; i < channelType.NumField(); i++ {
		field := channelType.Field(i)
		if field.Tag.Get("tag_config") != channelTagConfigSyncTag {
			continue
		}

		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		if jsonName == "" || jsonName == "-" {
			continue
		}
		if submittedFields != nil {
			if _, ok := submittedFields[jsonName]; !ok {
				continue
			}
		}
		fields = append(fields, channelTagConfigField{
			index:    i,
			jsonName: jsonName,
		})
	}
	return fields
}

func channelTagConfigUpdateValues(channel *Channel, fields []channelTagConfigField) map[string]interface{} {
	channelValue := reflect.ValueOf(channel).Elem()
	values := make(map[string]interface{}, len(fields))
	for _, field := range fields {
		value := channelValue.Field(field.index)
		if value.Kind() == reflect.Ptr && value.IsNil() {
			values[field.jsonName] = nil
			continue
		}
		values[field.jsonName] = value.Interface()
	}
	return values
}

func applyChannelTagConfigFields(dst *Channel, src *Channel, fields []channelTagConfigField) {
	dstValue := reflect.ValueOf(dst).Elem()
	srcValue := reflect.ValueOf(src).Elem()
	for _, field := range fields {
		dstValue.Field(field.index).Set(srcValue.Field(field.index))
	}
}

func normalizeChannelTagKey(key string, channelType int) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil
	}

	if channelType != config.ChannelTypeCodex {
		return key, nil
	}

	if !strings.HasPrefix(key, "{") {
		if strings.Contains(key, "\n") {
			return "", errors.New("Codex key must be a JSON object or single-line token")
		}
		return key, nil
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(key), &parsed); err != nil {
		return "", fmt.Errorf("Codex key must be a valid JSON object or single-line token: %w", err)
	}
	if parsed == nil {
		return "", errors.New("Codex key must be a JSON object")
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(key)); err != nil {
		return "", err
	}
	return compact.String(), nil
}

func splitChannelTagKeys(keyText string, channelType int) ([]string, error) {
	keyText = strings.TrimSpace(keyText)
	if keyText == "" {
		return nil, nil
	}

	if channelType == config.ChannelTypeCodex {
		if key, err := normalizeChannelTagKey(keyText, channelType); err == nil && key != "" {
			return []string{key}, nil
		}
	}

	lines := strings.Split(keyText, "\n")
	keys := make([]string, 0, len(lines))
	for _, line := range lines {
		key, err := normalizeChannelTagKey(line, channelType)
		if err != nil {
			return nil, err
		}
		if key == "" {
			continue
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func channelTagKeyDigest(key string) string {
	keyMd5 := md5.Sum([]byte(key))
	return hex.EncodeToString(keyMd5[:])
}

func normalizeExistingChannelTagKey(key string, channelType int) string {
	normalized, err := normalizeChannelTagKey(key, channelType)
	if err != nil || normalized == "" {
		return strings.TrimSpace(key)
	}
	return normalized
}

func buildChannelTagMember(channelTag *ChannelTagCollection, channel *Channel, fields []channelTagConfigField, key string, name string, memberIndex int) Channel {
	addChannel := channelTag.Channel
	if channel != nil && len(fields) > 0 {
		applyChannelTagConfigFields(&addChannel, channel, fields)
	}

	if name == "" {
		baseName := ""
		if channel != nil {
			baseName = strings.TrimSpace(channel.Name)
		}
		if baseName == "" {
			baseName = channelTag.Name
		}
		name = fmt.Sprintf("%s_%d", baseName, memberIndex)
	}

	addChannel.Id = 0
	addChannel.Name = name
	addChannel.Key = key
	addChannel.Balance = 0
	addChannel.BalanceUpdatedTime = 0
	addChannel.UsedQuota = 0
	addChannel.ResponseTime = 0
	addChannel.CreatedTime = time.Now().Unix()
	addChannel.TestTime = 0
	addChannel.DeletedAt = gorm.DeletedAt{}
	return addChannel
}

func GetChannelsTag(tag string) (*ChannelTagCollection, error) {
	var channelTag ChannelTagCollection

	var channels []Channel
	err := DB.Where("tag = ?", tag).Find(&channels).Error
	if err != nil {
		return nil, err
	}

	if len(channels) == 0 {
		return nil, errors.New("tag不存在")
	}

	channelTag.Channel = channels[0]
	channelTag.Key = ""

	channelTag.KeyMap = make(map[string]int)
	for _, c := range channels {
		key := normalizeExistingChannelTagKey(c.Key, channelTag.Type)
		channelTag.KeyMap[channelTagKeyDigest(key)] = c.Id
		channelTag.Key += key + "\n"
	}

	channelTag.Key = strings.TrimRight(channelTag.Key, "\n")
	return &channelTag, nil
}

func UpdateChannelsTag(tag string, channel *Channel) error {
	return UpdateChannelsTagWithSubmittedFields(tag, channel, nil)
}

func UpdateChannelsTagWithSubmittedFields(tag string, channel *Channel, submittedFields ChannelTagSubmittedFields) error {
	unlock, err := lockChannelTagMutation(context.Background(), tag)
	if err != nil {
		return err
	}
	defer unlock()
	return updateChannelsTagWithSubmittedFieldsLocked(tag, channel, submittedFields)
}

func updateChannelsTagWithSubmittedFieldsLocked(tag string, channel *Channel, submittedFields ChannelTagSubmittedFields) error {
	channelTag, err := GetChannelsTag(tag)
	if err != nil {
		return err
	}
	existingMemberIDs := make([]int, 0, len(channelTag.KeyMap))
	for _, id := range channelTag.KeyMap {
		existingMemberIDs = append(existingMemberIDs, id)
	}
	channel.Type = channelTag.Type
	configFields := channelTagConfigFields(submittedFields)
	if channelTypeRequiresOtherForPartialUpdate(channel.Type) && strings.TrimSpace(channel.Other) == "" && !channelTagConfigFieldSubmitted(configFields, "other") {
		channel.Other = channelTag.Other
	}
	if channelTypeRequiresBaseURLForPartialUpdate(channel.Type) && channel.BaseURL == nil && !channelTagConfigFieldSubmitted(configFields, "base_url") {
		channel.BaseURL = channelTag.BaseURL
	}
	if err := channel.CanonicalizeRuntimeConfigJSONWithType(channelTag.Type); err != nil {
		return err
	}
	if err := channel.ValidateRuntimeConfigJSONWithType(channelTag.Type); err != nil {
		return err
	}

	if channel.Key == "" {
		return errors.New("key不能为空")
	}

	addKeys := []string{}
	delIds := []int{}

	newKeysMap := make(map[string]bool)

	keys, err := splitChannelTagKeys(channel.Key, channelTag.Type)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("key不能为空")
	}
	for _, key := range keys {
		keyMd5Str := channelTagKeyDigest(key)
		if newKeysMap[keyMd5Str] {
			continue
		}
		newKeysMap[keyMd5Str] = true

		// 如果key不在现有的KeyMap中，则添加到addKeys
		if _, ok := channelTag.KeyMap[keyMd5Str]; !ok {
			addKeys = append(addKeys, key)
		}
	}

	// 检查现有的keys，如果不在新的keys中，则需要删除
	for keyMd5Str, id := range channelTag.KeyMap {
		if _, ok := newKeysMap[keyMd5Str]; !ok {
			delIds = append(delIds, id)
		}
	}

	addChannels := make([]Channel, 0, len(addKeys))
	if len(addKeys) > 0 {
		maxKey := len(channelTag.KeyMap)
		baseName := channel.Name
		if baseName == "" {
			baseName = channelTag.Name
		}
		for _, key := range addKeys {
			// Partial tag updates only carry changed config fields. New members
			// start from the existing tag representative, then receive the
			// submitted tag-level overlay; runtime counters are reset below.
			addChannel := buildChannelTagMember(channelTag, channel, configFields, key, fmt.Sprintf("%s_%d", baseName, maxKey), maxKey)
			if err := addChannel.CanonicalizeRuntimeConfigJSONWithType(channelTag.Type); err != nil {
				return err
			}
			if err := addChannel.ValidateRuntimeConfigJSONWithType(channelTag.Type); err != nil {
				return err
			}
			addChannels = append(addChannels, addChannel)
			maxKey++
		}
	}

	tx := DB.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	// Membership is a fixed snapshot for this operation. Every destructive or
	// config statement also checks the current tag, so a snapshotted member moved
	// out concurrently is untouched, while a row moved in is never adopted by this
	// batch. New members receive config only through buildChannelTagMember above.
	var deletedCandidates []Channel
	if len(delIds) > 0 {
		if err = tx.Model(&Channel{}).Select("id", "type").Where("id IN (?) AND tag = ?", delIds, tag).Find(&deletedCandidates).Error; err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Model(&Channel{}).Where("id IN (?) AND tag = ?", delIds, tag).UpdateColumn("credential_revision", gorm.Expr("credential_revision + 1")).Error; err != nil {
			tx.Rollback()
			return err
		}
		result := tx.Where("id IN (?) AND tag = ?", delIds, tag).Delete(&Channel{})
		if err = result.Error; err != nil {
			tx.Rollback()
			return err
		}
		candidateIDs := channelIDsFromRows(deletedCandidates)
		deletedCandidates = nil
		if len(candidateIDs) > 0 {
			if err = tx.Unscoped().Model(&Channel{}).Select("id", "type").Where("id IN (?) AND deleted_at IS NOT NULL", candidateIDs).Find(&deletedCandidates).Error; err != nil {
				tx.Rollback()
				return err
			}
		}
	}

	// 处理要添加的数据
	if len(addChannels) > 0 {
		err = BatchInsertStrict(tx, addChannels)
		if err != nil {
			tx.Rollback()
			return err
		}
	}

	updateValues := channelTagConfigUpdateValues(channel, configFields)
	var configCandidates []Channel
	if len(updateValues) > 0 && len(existingMemberIDs) > 0 {
		candidateScope := tx.Model(&Channel{}).Where("id IN (?) AND tag = ?", existingMemberIDs, tag)
		if err = candidateScope.Select("id", "type").Find(&configCandidates).Error; err != nil {
			tx.Rollback()
			return err
		}
		candidateIDs := channelIDsFromRows(configCandidates)
		if len(candidateIDs) > 0 {
			result := tx.Model(Channel{}).Where("id IN (?) AND tag = ?", candidateIDs, tag).Updates(updateValues)
			if err = result.Error; err != nil {
				tx.Rollback()
				return err
			}
			// Keep the pre-update candidates for post-commit invalidation. The UPDATE
			// itself may rename the tag, so filtering by the old tag afterward would
			// drop every durably changed row. Conservatively invalidating a candidate
			// skipped by a concurrent move costs one cache clear and immediate reload;
			// omitting an updated row can leave the old route live indefinitely.
		}
	}

	if err = tx.Commit().Error; err != nil {
		return err
	}

	if len(deletedCandidates) == 0 && len(configCandidates) == 0 && len(addChannels) == 0 {
		return nil
	}
	affectedIDs := append(channelIDsFromRows(deletedCandidates), channelIDsFromRows(configCandidates)...)
	codexIDs := append(codexChannelIDsFromRows(deletedCandidates), codexChannelIDsFromRows(configCandidates)...)
	for i := range addChannels {
		if addChannels[i].Type == config.ChannelTypeCodex {
			codexIDs = append(codexIDs, addChannels[i].Id)
		}
	}
	finishChannelRouteMutation("update channel tag", affectedIDs, codexIDs)
	return nil
}

func channelTagConfigFieldSubmitted(fields []channelTagConfigField, jsonName string) bool {
	for _, field := range fields {
		if field.jsonName == jsonName {
			return true
		}
	}
	return false
}

func AddChannelToTag(tag string, channel *Channel) (*Channel, error) {
	if strings.TrimSpace(tag) == "" {
		return nil, errors.New("tag is required")
	}
	unlock, err := lockChannelTagMutation(context.Background(), tag)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return addChannelToTagLocked(tag, channel)
}

func addChannelToTagLocked(tag string, channel *Channel) (*Channel, error) {
	if channel == nil {
		return nil, errors.New("channel is required")
	}

	channelTag, err := GetChannelsTag(tag)
	if err != nil {
		return nil, err
	}

	key, err := normalizeChannelTagKey(channel.Key, channelTag.Type)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, errors.New("key不能为空")
	}
	if strings.Contains(key, "\n") {
		return nil, errors.New("key只能包含一个渠道")
	}
	if _, ok := channelTag.KeyMap[channelTagKeyDigest(key)]; ok {
		return nil, errors.New("key已存在")
	}

	newChannel := buildChannelTagMember(channelTag, nil, nil, key, strings.TrimSpace(channel.Name), len(channelTag.KeyMap))
	if err := newChannel.CanonicalizeRuntimeConfigJSONWithType(channelTag.Type); err != nil {
		return nil, err
	}
	if err := newChannel.ValidateRuntimeConfigJSONWithType(channelTag.Type); err != nil {
		return nil, err
	}
	if err := DB.Omit("UsedQuota").Create(&newChannel).Error; err != nil {
		return nil, err
	}

	codexIDs := []int(nil)
	if newChannel.Type == config.ChannelTypeCodex {
		codexIDs = []int{newChannel.Id}
	}
	finishChannelRouteMutation("add channel to tag", nil, codexIDs)
	return &newChannel, nil
}

func DeleteChannelsTag(tag string, delDisabled bool) error {
	if tag == "" {
		return nil
	}
	unlock, err := lockChannelTagMutation(context.Background(), tag)
	if err != nil {
		return err
	}
	defer unlock()

	_, err = deleteChannelsMatching(func(db *gorm.DB) *gorm.DB {
		if delDisabled {
			db = db.Where("(status = ? or status = ?)", config.ChannelStatusAutoDisabled, config.ChannelStatusManuallyDisabled)
		}
		return db.Where("tag = ?", tag)
	})
	return err
}

func ChangeChannelsTagStatus(tag string, status int) error {
	return ChangeChannelsTagStatusWithContext(context.Background(), tag, status)
}

func ChangeChannelsTagStatusWithContext(ctx context.Context, tag string, status int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if tag == "" {
		return nil
	}
	unlock, err := lockChannelTagMutation(ctx, tag)
	if err != nil {
		return err
	}
	defer unlock()

	// Snapshot candidate IDs and their old statuses, then express all per-row
	// compare-and-swaps as one UPDATE. The tag predicate protects rows moved out
	// of the tag; pairing IDs with their observed status protects a concurrent
	// status change. Rows added to the tag after the snapshot are not candidates.
	var changed []Channel
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidates []Channel
		if err := tx.Select("id", "type", "status").Where("tag = ? AND status <> ?", tag, status).Find(&candidates).Error; err != nil {
			return err
		}
		if len(candidates) == 0 {
			return ctx.Err()
		}

		candidateIDs := make([]int, 0, len(candidates))
		idsByStatus := make(map[int][]int)
		for _, channel := range candidates {
			candidateIDs = append(candidateIDs, channel.Id)
			idsByStatus[channel.Status] = append(idsByStatus[channel.Status], channel.Id)
		}

		var statusGuard *gorm.DB
		for oldStatus, ids := range idsByStatus {
			condition := tx.Where("id IN ? AND status = ?", ids, oldStatus)
			if statusGuard == nil {
				statusGuard = condition
			} else {
				statusGuard = statusGuard.Or(condition)
			}
		}
		result := tx.Model(&Channel{}).
			Where("tag = ? AND status <> ?", tag, status).
			Where(statusGuard).
			Update("status", status)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ctx.Err()
		}

		// This post-update set is exact for our serialized transaction and safely
		// conservative if another writer independently reached the same target.
		// It drives side effects only after the transaction commits.
		if err := tx.Select("id", "type", "status").
			Where("id IN ? AND tag = ? AND status = ?", candidateIDs, tag, status).
			Find(&changed).Error; err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		return nil
	}

	changedIDs := channelIDsFromRows(changed)
	codexIDs := codexChannelIDsFromRows(changed)
	// Enabled rows are normally absent, but fail-close is harmless and preserves
	// one ordering invariant for every durable status transition.
	finishChannelRouteMutation("change channel tag status", changedIDs, codexIDs)
	return nil
}

func UpdateChannelsTagPriority(tag string, value int) error {
	return UpdateChannelsTagPriorityWithContext(context.Background(), tag, value)
}

func UpdateChannelsTagPriorityWithContext(ctx context.Context, tag string, value int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if tag == "" {
		return nil
	}
	unlock, err := lockChannelTagMutation(ctx, tag)
	if err != nil {
		return err
	}
	defer unlock()

	// Membership is snapshotted inside the transaction. Keeping both the ID set
	// and current tag in the UPDATE means a row concurrently moved out is not
	// modified, while a row moved into the tag is deferred to a later request.
	var changed []Channel
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidates []Channel
		if err := tx.Select("id").Where("tag = ? AND (priority IS NULL OR priority <> ?)", tag, value).Find(&candidates).Error; err != nil {
			return err
		}
		if len(candidates) == 0 {
			return ctx.Err()
		}

		candidateIDs := channelIDsFromRows(candidates)
		result := tx.Model(&Channel{}).
			Where("id IN ? AND tag = ? AND (priority IS NULL OR priority <> ?)", candidateIDs, tag, value).
			Update("priority", value)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ctx.Err()
		}

		// Re-reading the candidate set confirms rows still in the tag. This is
		// exact for serialized writes and conservatively includes a candidate if
		// another writer independently reached the same priority.
		if err := tx.Select("id").
			Where("id IN ? AND tag = ? AND priority = ?", candidateIDs, tag, value).
			Find(&changed).Error; err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		return nil
	}

	refreshChannelGroupAfterMutation("update channel tag priority", channelIDsFromRows(changed))
	return nil
}
