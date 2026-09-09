package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"one-api/common/authutil"
	"one-api/common/config"
	"one-api/common/groupctx"
	commonredis "one-api/common/redis"
	"one-api/internal/requesthints"
	"one-api/model"
	runtimeaffinity "one-api/runtime/channelaffinity"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type channelAffinityKind string

const (
	channelAffinityKindChat      channelAffinityKind = "chat"
	channelAffinityKindResponses channelAffinityKind = "responses"
	channelAffinityKindRealtime  channelAffinityKind = "realtime"

	channelAffinityStateContextKey             = "channel_affinity_state"
	channelAffinityPreferredChannelContextKey  = "channel_affinity_preferred_channel_id"
	channelAffinityStrictContextKey            = "channel_affinity_strict"
	channelAffinitySkipRetryContextKey         = "channel_affinity_skip_retry_on_failure"
	channelAffinityIgnoreCooldownContextKey    = "channel_affinity_ignore_preferred_cooldown"
	channelAffinitySelectedPreferredContextKey = "channel_affinity_selected_preferred"
	channelAffinityInputContextKey             = "channel_affinity_input"
	defaultChannelAffinityJanitorInterval      = time.Minute
	defaultChannelAffinityRedisPrefix          = "one-hub:channel-affinity"
)

var channelAffinityManagerPublication struct {
	sync.Mutex
	owner   *config.OptionManager
	version int64
}

type channelAffinityTemplate struct {
	Kind                    channelAffinityKind
	RuleName                string
	Source                  string
	InputSource             string
	TTL                     time.Duration
	Strict                  bool
	SkipRetryOnFailure      bool
	IgnorePreferredCooldown bool
	RecordOnSuccess         bool
	Parts                   []string
}

type channelAffinityBinding struct {
	Template channelAffinityTemplate
	Value    string
	Key      string
}

type channelAffinityState struct {
	Kind               channelAffinityKind
	Lookup             *channelAffinityBinding
	RequestBindings    []*channelAffinityBinding
	DerivedRecorders   map[string][]channelAffinityTemplate
	ResumeFingerprint  string
	PreferredChannelID int
	Hit                bool
}

type channelAffinityInput struct {
	ResponsesRequest  *types.OpenAIResponsesRequest
	PromptCacheKey    string
	RealtimeSessionID string
	ModelName         string
}

var (
	affinityRegexCache sync.Map
)

func rememberChannelAffinityKey(c *gin.Context, kind channelAffinityKind, value string) {
	if c == nil {
		return
	}
	state := currentChannelAffinityState(c)
	if state == nil || state.Kind != kind {
		state = &channelAffinityState{
			Kind:             kind,
			DerivedRecorders: make(map[string][]channelAffinityTemplate),
		}
	}

	binding := defaultChannelAffinityBinding(c, kind, value)
	if binding == nil {
		if len(state.RequestBindings) == 0 {
			c.Set(channelAffinityStateContextKey, state)
		}
		return
	}

	state.Lookup = binding
	state.RequestBindings = appendUniqueChannelAffinityBinding(state.RequestBindings, binding)
	state.DerivedRecorders = appendChannelAffinityRecorder(state.DerivedRecorders, binding.Template.Source, binding.Template)
	c.Set(channelAffinityStateContextKey, state)
}

func currentChannelAffinityKey(c *gin.Context) string {
	state := currentChannelAffinityState(c)
	if state == nil || state.Lookup == nil {
		return ""
	}
	return strings.TrimSpace(state.Lookup.Key)
}

func currentChannelAffinityState(c *gin.Context) *channelAffinityState {
	if c == nil {
		return nil
	}
	value, exists := c.Get(channelAffinityStateContextKey)
	if !exists || value == nil {
		return nil
	}
	state, ok := value.(*channelAffinityState)
	if !ok {
		return nil
	}
	return state
}

func setPreferredChannelFromAffinity(c *gin.Context, channelID int) {
	if c == nil {
		return
	}
	if channelID <= 0 {
		c.Set(channelAffinityPreferredChannelContextKey, 0)
		return
	}
	c.Set(channelAffinityPreferredChannelContextKey, channelID)
}

func currentPreferredChannelID(c *gin.Context) int {
	if c == nil {
		return 0
	}
	return c.GetInt(channelAffinityPreferredChannelContextKey)
}

func currentChannelAffinityStrict(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return c.GetBool(channelAffinityStrictContextKey)
}

func currentChannelAffinitySkipRetry(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return c.GetBool(channelAffinitySkipRetryContextKey)
}

func currentChannelAffinityIgnorePreferredCooldown(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return c.GetBool(channelAffinityIgnoreCooldownContextKey)
}

func currentChannelAffinitySelectedPreferred(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return c.GetBool(channelAffinitySelectedPreferredContextKey)
}

func setChannelAffinitySelectedPreferred(c *gin.Context, selected bool) {
	if c == nil {
		return
	}
	c.Set(channelAffinitySelectedPreferredContextKey, selected)
}

func lookupChannelAffinity(c *gin.Context, kind channelAffinityKind, value string) (int, bool) {
	if state := currentChannelAffinityState(c); state != nil && state.Kind == kind {
		for _, binding := range state.RequestBindings {
			if binding != nil && binding.Value == strings.TrimSpace(value) {
				record, ok := channelAffinityManager().Get(binding.Key)
				if ok && record.ChannelID > 0 && channelAffinityRecordMatchesState(record, state) {
					return record.ChannelID, true
				}
			}
		}
	}
	binding := defaultChannelAffinityBinding(c, kind, value)
	if binding == nil {
		return 0, false
	}
	record, ok := channelAffinityManager().Get(binding.Key)
	if !ok || record.ChannelID <= 0 {
		return 0, false
	}
	if state := currentChannelAffinityState(c); state != nil && state.Kind == kind && !channelAffinityRecordMatchesState(record, state) {
		return 0, false
	}
	return record.ChannelID, true
}

func recordCurrentChannelAffinity(c *gin.Context, kind channelAffinityKind, channelID int) {
	if c == nil || channelID <= 0 {
		return
	}
	state := currentChannelAffinityState(c)
	if explicitChannelPinID(c) > 0 {
		refreshChannelAffinityMeta(c, state, channelID)
		return
	}
	if state == nil || state.Kind != kind {
		key := currentChannelAffinityKey(c)
		if key == "" {
			return
		}
		channelAffinityManager().SetRecord(key, runtimeaffinity.Record{
			ChannelID:         channelID,
			ResumeFingerprint: channelAffinityStateResumeFingerprint(state),
		}, 0)
		return
	}

	recordChannelAffinityStateBindings(state, channelID)
	refreshChannelAffinityMeta(c, state, channelID)
}

func recordResponsesChannelAffinity(c *gin.Context, channelID int, response *types.OpenAIResponsesResponses) {
	if c == nil || channelID <= 0 {
		return
	}
	model.ChannelGroup.RLock()
	choice := model.ChannelGroup.Channels[channelID]
	var channel *model.Channel
	if choice != nil {
		channel = choice.Channel
	}
	model.ChannelGroup.RUnlock()
	requestedModel := ""
	if value, ok := c.Get(channelAffinityInputContextKey); ok {
		if input, ok := value.(channelAffinityInput); ok {
			requestedModel = channelAffinityModelName(channelAffinityKindResponses, input)
		}
	}
	refreshChannelAffinityForSelectedModel(c, channelAffinityKindResponses, channel, requestedModel)
	if explicitChannelPinID(c) > 0 {
		recordCurrentChannelAffinity(c, channelAffinityKindResponses, channelID)
		return
	}

	state := currentChannelAffinityState(c)
	if state == nil || state.Kind != channelAffinityKindResponses {
		recordCurrentChannelAffinity(c, channelAffinityKindResponses, channelID)
		return
	}
	recordResponsesChannelAffinityState(c, state, channelID, response)
}

func recordResponsesChannelAffinityState(c *gin.Context, state *channelAffinityState, channelID int, response *types.OpenAIResponsesResponses) {
	if c == nil || state == nil || state.Kind != channelAffinityKindResponses || channelID <= 0 {
		return
	}
	recordChannelAffinityStateBindings(state, channelID)
	if response == nil {
		refreshChannelAffinityMeta(c, state, channelID)
		return
	}

	for alias, value := range derivedResponseAffinityValues(response) {
		if value == "" {
			continue
		}
		recorders := state.DerivedRecorders[alias]
		for _, recorder := range recorders {
			key := recorder.BuildKey(value)
			if key == "" {
				continue
			}
			channelAffinityManager().SetRecord(key, runtimeaffinity.Record{
				ChannelID:         channelID,
				ResumeFingerprint: channelAffinityStateResumeFingerprint(state),
			}, recorder.TTL)
		}
	}
	refreshChannelAffinityMeta(c, state, channelID)
}

func recordChannelAffinityStateBindings(state *channelAffinityState, channelID int) {
	for _, binding := range state.RequestBindings {
		if binding == nil || !binding.Template.RecordOnSuccess || strings.TrimSpace(binding.Key) == "" {
			continue
		}
		channelAffinityManager().SetRecord(binding.Key, runtimeaffinity.Record{
			ChannelID:         channelID,
			ResumeFingerprint: channelAffinityStateResumeFingerprint(state),
		}, binding.Template.TTL)
	}
}

func clearCurrentChannelAffinity(c *gin.Context) {
	if c == nil {
		return
	}
	key := currentChannelAffinityKey(c)
	if key == "" {
		return
	}
	channelAffinityManager().Delete(key)
}

func clearCurrentChannelAffinityBindings(c *gin.Context) {
	if c == nil {
		return
	}

	state := currentChannelAffinityState(c)
	if state == nil || len(state.RequestBindings) == 0 {
		clearCurrentChannelAffinity(c)
		return
	}

	manager := channelAffinityManager()
	deleted := make(map[string]struct{}, len(state.RequestBindings))
	for _, binding := range state.RequestBindings {
		if binding == nil {
			continue
		}
		key := strings.TrimSpace(binding.Key)
		if key == "" {
			continue
		}
		if _, exists := deleted[key]; exists {
			continue
		}
		manager.Delete(key)
		deleted[key] = struct{}{}
	}

	if key := strings.TrimSpace(currentChannelAffinityKey(c)); key != "" {
		if _, exists := deleted[key]; !exists {
			manager.Delete(key)
		}
	}
}

func channelAffinityLock(c *gin.Context, kind channelAffinityKind, value string) func() {
	key := currentChannelAffinityKey(c)
	if key == "" {
		if binding := defaultChannelAffinityBinding(c, kind, value); binding != nil {
			key = binding.Key
		}
	}
	if key == "" {
		return func() {}
	}
	return channelAffinityManager().Lock(key)
}

func prepareResponsesChannelAffinity(c *gin.Context, request *types.OpenAIResponsesRequest) {
	requesthints.ResolveResponses(c, request)
	resolvedPromptCacheKey := requesthints.Get(c, requesthints.ResponsesPromptCacheKey)
	requestCopy := *request
	input := channelAffinityInput{ResponsesRequest: &requestCopy}
	c.Set(channelAffinityInputContextKey, input)
	state := evaluateChannelAffinityAcrossCanonicalModels(c, channelAffinityKindResponses, input, request.Model)
	applyChannelAffinityState(c, state)
	if strings.TrimSpace(requestPromptCacheKey(request)) == "" && resolvedPromptCacheKey != "" {
		mergeChannelAffinityMeta(c, map[string]any{
			"channel_affinity_prompt_cache_key_derived": true,
			"channel_affinity_prompt_cache_key_source":  "request_hint",
		})
	}
}

func prepareChatChannelAffinity(c *gin.Context, modelName, promptCacheKey string) {
	input := channelAffinityInput{
		ModelName:      modelName,
		PromptCacheKey: promptCacheKey,
	}
	c.Set(channelAffinityInputContextKey, input)
	state := evaluateChannelAffinityAcrossCanonicalModels(c, channelAffinityKindChat, input, modelName)
	applyChannelAffinityState(c, state)
}

func evaluateChannelAffinityAcrossCanonicalModels(c *gin.Context, kind channelAffinityKind, input channelAffinityInput, requestedModel string) *channelAffinityState {
	models := channelAffinityCanonicalModels(requestedModel)
	var fallback *channelAffinityState
	for _, modelName := range models {
		candidate := input
		candidate.ModelName = modelName
		if input.ResponsesRequest != nil {
			requestCopy := *input.ResponsesRequest
			requestCopy.Model = modelName
			candidate.ResponsesRequest = &requestCopy
		}
		state := evaluateChannelAffinity(c, kind, candidate)
		if fallback == nil {
			fallback = state
		}
		if state != nil && state.Hit && channelAffinityHitMatchesCanonicalModel(state, requestedModel, modelName) {
			return state
		}
	}
	return fallback
}

func channelAffinityHitMatchesCanonicalModel(state *channelAffinityState, requestedModel, canonicalModel string) bool {
	if state == nil || !state.Hit || state.PreferredChannelID <= 0 {
		return false
	}
	channel := model.ChannelGroup.GetChannel(state.PreferredChannelID)
	if channel == nil {
		return false
	}
	mapped, err := mappedModelForChannel(channel, requestedModel)
	return err == nil && strings.TrimSpace(mapped) == strings.TrimSpace(canonicalModel)
}

func channelAffinityCanonicalModels(requestedModel string) []string {
	models := []string{strings.TrimSpace(requestedModel)}
	seen := map[string]struct{}{strings.TrimSpace(requestedModel): {}}
	model.ChannelGroup.RLock()
	channels := make([]*model.Channel, 0, len(model.ChannelGroup.Channels))
	for _, choice := range model.ChannelGroup.Channels {
		if choice != nil && choice.Channel != nil {
			channels = append(channels, choice.Channel)
		}
	}
	model.ChannelGroup.RUnlock()
	for _, channel := range channels {
		canonical, err := mappedModelForChannel(channel, requestedModel)
		canonical = strings.TrimSpace(canonical)
		if err != nil || canonical == "" {
			continue
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		models = append(models, canonical)
	}
	return models
}

func refreshChannelAffinityForSelectedModel(c *gin.Context, kind channelAffinityKind, channel *model.Channel, requestedModel string) {
	if c == nil || channel == nil {
		return
	}
	value, ok := c.Get(channelAffinityInputContextKey)
	if !ok {
		return
	}
	input, ok := value.(channelAffinityInput)
	if !ok {
		return
	}
	canonical, err := mappedModelForChannel(channel, requestedModel)
	if err != nil || strings.TrimSpace(canonical) == "" {
		return
	}
	input.ModelName = canonical
	if input.ResponsesRequest != nil {
		requestCopy := *input.ResponsesRequest
		requestCopy.Model = canonical
		input.ResponsesRequest = &requestCopy
	}
	applyChannelAffinityState(c, evaluateChannelAffinity(c, kind, input))
}

func prepareRealtimeChannelAffinity(c *gin.Context, modelName, clientSessionID string) int {
	input := channelAffinityInput{
		RealtimeSessionID: clientSessionID,
		ModelName:         modelName,
	}
	if c != nil {
		c.Set(channelAffinityInputContextKey, input)
	}
	state := evaluateChannelAffinity(c, channelAffinityKindRealtime, input)
	applyChannelAffinityState(c, state)
	if state == nil {
		return 0
	}
	return state.PreferredChannelID
}

func realtimeClientSessionIDFromRequest(req *http.Request) string {
	return runtimesession.ReadClientSessionID(req)
}

func buildChannelAffinityKey(c *gin.Context, kind channelAffinityKind, value string) string {
	binding := defaultChannelAffinityBinding(c, kind, value)
	if binding == nil {
		return ""
	}
	return binding.Key
}

func channelAffinityScope(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if tokenID := c.GetInt("token_id"); tokenID > 0 {
		return fmt.Sprintf("token:%d", tokenID)
	}
	if userID := c.GetInt("id"); userID > 0 {
		return fmt.Sprintf("user:%d", userID)
	}
	if namespace := authutil.StableRequestCredentialNamespace(c.Request); namespace != "" {
		return namespace
	}
	return "anonymous"
}

func shouldSkipRetryAfterAffinityFailure(c *gin.Context) bool {
	return currentChannelAffinitySelectedPreferred(c) && currentChannelAffinitySkipRetry(c)
}

func currentChannelAffinityLogMeta(c *gin.Context) map[string]any {
	if c == nil {
		return nil
	}
	value, exists := c.Get(config.GinChannelAffinityMetaKey)
	if !exists || value == nil {
		return nil
	}
	meta, ok := value.(map[string]any)
	if !ok || len(meta) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(meta))
	for key, item := range meta {
		cloned[key] = item
	}
	mergeRoutingGroupLogMeta(c, cloned)
	return cloned
}

func mergeChannelAffinityMeta(c *gin.Context, extra map[string]any) {
	if c == nil || len(extra) == 0 {
		return
	}

	meta := currentChannelAffinityLogMeta(c)
	if meta == nil {
		meta = map[string]any{}
	}
	for key, value := range extra {
		meta[key] = value
	}
	mergeRoutingGroupLogMeta(c, meta)
	c.Set(config.GinChannelAffinityMetaKey, meta)
}

func mergeRoutingGroupLogMeta(c *gin.Context, meta map[string]any) {
	if len(meta) == 0 {
		return
	}
	for key, value := range groupctx.CurrentRoutingGroupMeta(c) {
		meta[key] = value
	}
}

func ChannelAffinityCacheStats() map[string]any {
	settings := config.RuntimeChannelAffinitySettings(config.GlobalOption.RuntimeSnapshot())
	manager := channelAffinityManager()
	stats := manager.Stats()

	return map[string]any{
		"enabled":             settings.Enabled,
		"default_ttl_seconds": settings.DefaultTTLSeconds,
		"max_entries":         settings.MaxEntries,
		"rules_count":         len(settings.Rules),
		"backend":             stats.Backend,
		"local_entries":       stats.LocalEntries,
		"backend_entries":     stats.BackendEntries,
	}
}

func ClearChannelAffinityCache() int {
	return channelAffinityManager().Clear()
}

func applyChannelAffinityState(c *gin.Context, state *channelAffinityState) {
	if c == nil {
		return
	}
	c.Set(channelAffinityStateContextKey, state)
	setPreferredChannelFromAffinity(c, 0)
	c.Set(channelAffinityStrictContextKey, false)
	c.Set(channelAffinitySkipRetryContextKey, false)
	c.Set(channelAffinityIgnoreCooldownContextKey, false)
	setChannelAffinitySelectedPreferred(c, false)
	refreshChannelAffinityMeta(c, state, 0)

	if state == nil {
		return
	}
	if state.Hit && state.PreferredChannelID > 0 {
		setPreferredChannelFromAffinity(c, state.PreferredChannelID)
		c.Set(channelAffinityStrictContextKey, state.Lookup.Template.Strict)
		c.Set(channelAffinitySkipRetryContextKey, state.Lookup.Template.SkipRetryOnFailure)
		c.Set(channelAffinityIgnoreCooldownContextKey, state.Lookup.Template.IgnorePreferredCooldown)
	}
}

func evaluateChannelAffinity(c *gin.Context, kind channelAffinityKind, input channelAffinityInput) *channelAffinityState {
	settings := config.RuntimeChannelAffinitySettings(config.GlobalOption.RuntimeSnapshot())
	if !settings.Enabled {
		return nil
	}
	manager := channelAffinityManager()

	state := &channelAffinityState{
		Kind:              kind,
		DerivedRecorders:  make(map[string][]channelAffinityTemplate),
		ResumeFingerprint: channelAffinityResumeFingerprint(kind, input),
	}
	modelName := strings.TrimSpace(channelAffinityModelName(kind, input))

	var firstBinding *channelAffinityBinding
	var firstHit *channelAffinityBinding
	explicitPin := explicitChannelPinID(c) > 0

	for _, rule := range settings.Rules {
		if !channelAffinityRuleMatches(c, kind, modelName, rule) {
			continue
		}

		for _, source := range rule.KeySources {
			template := newChannelAffinityTemplate(c, kind, modelName, rule, source.Source, source.Alias, settings.DefaultTTLSeconds)
			if template.Source == "" {
				continue
			}
			if template.RecordOnSuccess {
				state.DerivedRecorders = appendChannelAffinityRecorder(state.DerivedRecorders, template.Source, template)
			}

			value := extractChannelAffinityValue(c, input, source)
			if value == "" {
				continue
			}
			binding := &channelAffinityBinding{
				Template: template,
				Value:    value,
				Key:      template.BuildKey(value),
			}
			if binding.Key == "" {
				continue
			}
			if firstBinding == nil {
				firstBinding = binding
			}
			state.RequestBindings = appendUniqueChannelAffinityBinding(state.RequestBindings, binding)
			if explicitPin {
				continue
			}
			record, ok := manager.Get(binding.Key)
			if !ok || record.ChannelID <= 0 || firstHit != nil || !channelAffinityRecordMatchesState(record, state) {
				continue
			}
			state.Hit = true
			state.PreferredChannelID = record.ChannelID
			firstHit = binding
		}
	}

	if firstHit != nil {
		state.Lookup = firstHit
		return state
	}
	state.Lookup = firstBinding
	if state.Lookup == nil && len(state.DerivedRecorders) == 0 {
		return nil
	}
	return state
}

func defaultChannelAffinityBinding(c *gin.Context, kind channelAffinityKind, value string) *channelAffinityBinding {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}

	settings := config.RuntimeChannelAffinitySettings(config.GlobalOption.RuntimeSnapshot())
	if !settings.Enabled {
		return nil
	}
	var defaultRule config.ChannelAffinityRule
	var found bool
	for _, rule := range settings.Rules {
		if !rule.Enabled || rule.Kind != string(kind) {
			continue
		}
		for _, source := range rule.KeySources {
			if source.Alias == defaultChannelAffinityAlias(kind) {
				defaultRule = rule
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		defaultRule = config.ChannelAffinityRule{
			Name:            string(kind),
			Enabled:         true,
			Kind:            string(kind),
			IncludeRuleName: true,
			RecordOnSuccess: true,
		}
	}

	template := newChannelAffinityTemplate(c, kind, "", defaultRule, "default", defaultChannelAffinityAlias(kind), settings.DefaultTTLSeconds)
	key := template.BuildKey(value)
	if key == "" {
		return nil
	}
	return &channelAffinityBinding{
		Template: template,
		Value:    value,
		Key:      key,
	}
}

func channelAffinityRuleMatches(c *gin.Context, kind channelAffinityKind, modelName string, rule config.ChannelAffinityRule) bool {
	if !rule.Enabled {
		return false
	}
	if rule.Kind != "" && rule.Kind != string(kind) {
		return false
	}
	if rule.ModelRegex != "" && !channelAffinityRegexMatch(rule.ModelRegex, modelName) {
		return false
	}
	path := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	if rule.PathRegex != "" && !channelAffinityRegexMatch(rule.PathRegex, path) {
		return false
	}
	if rule.UserAgentRegex != "" {
		userAgent := ""
		if c != nil && c.Request != nil {
			userAgent = c.Request.UserAgent()
		}
		if !channelAffinityRegexMatch(rule.UserAgentRegex, userAgent) {
			return false
		}
	}
	return true
}

func extractChannelAffinityValue(c *gin.Context, input channelAffinityInput, source config.ChannelAffinityKeySource) string {
	var value string

	switch source.Source {
	case "request_field":
		if strings.EqualFold(strings.TrimSpace(source.Key), "prompt_cache_key") {
			value = strings.TrimSpace(input.PromptCacheKey)
		}
		if value == "" {
			value = extractChannelAffinityRequestField(input.ResponsesRequest, source.Key)
		}
	case "header":
		if c != nil && c.Request != nil {
			value = strings.TrimSpace(c.Request.Header.Get(source.Key))
		}
	case "query":
		if c != nil {
			value = strings.TrimSpace(c.Query(source.Key))
		}
	case "request_hint":
		value = requesthints.Get(c, source.Key)
	}

	value = strings.TrimSpace(value)
	if value == "" && source.Source == "header" && source.Alias == config.ChannelAffinityAliasSessionID {
		value = strings.TrimSpace(input.RealtimeSessionID)
	}
	if value == "" {
		return ""
	}
	if source.ValueRegex != "" && !channelAffinityRegexMatch(source.ValueRegex, value) {
		return ""
	}
	return value
}

func requestPromptCacheKey(request *types.OpenAIResponsesRequest) string {
	if request == nil {
		return ""
	}
	return strings.TrimSpace(request.PromptCacheKey)
}

func extractChannelAffinityRequestField(request *types.OpenAIResponsesRequest, field string) string {
	if request == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "prompt_cache_key":
		return strings.TrimSpace(request.PromptCacheKey)
	case "previous_response_id":
		return strings.TrimSpace(request.PreviousResponseID)
	}

	field = strings.TrimSpace(field)
	if strings.HasPrefix(field, "metadata.") {
		key := strings.TrimSpace(strings.TrimPrefix(field, "metadata."))
		if key == "" {
			return ""
		}
		return strings.TrimSpace(request.Metadata[key])
	}
	return ""
}

func newChannelAffinityTemplate(c *gin.Context, kind channelAffinityKind, modelName string, rule config.ChannelAffinityRule, inputSource, alias string, defaultTTLSeconds int) channelAffinityTemplate {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if alias == "" {
		return channelAffinityTemplate{}
	}

	parts := []string{channelAffinityScope(c), string(kind), alias}
	if rule.IncludeRuleName && rule.Name != "" {
		parts = append(parts, "rule:"+rule.Name)
	}
	if rule.IncludeGroup {
		groupName := groupctx.CurrentRoutingGroup(c)
		if groupName == "" {
			groupName = "default"
		}
		parts = append(parts, "group:"+groupName)
	}
	if rule.IncludeModel && modelName != "" {
		parts = append(parts, "model:"+modelName)
	}
	if rule.IncludePath && c != nil && c.Request != nil && c.Request.URL != nil {
		path := strings.TrimSpace(c.Request.URL.Path)
		if path != "" {
			parts = append(parts, "path:"+path)
		}
	}

	ttl := time.Duration(rule.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = time.Duration(defaultTTLSeconds) * time.Second
	}

	return channelAffinityTemplate{
		Kind:                    kind,
		RuleName:                rule.Name,
		Source:                  alias,
		InputSource:             inputSource,
		TTL:                     ttl,
		Strict:                  rule.Strict,
		SkipRetryOnFailure:      rule.SkipRetryOnFailure,
		IgnorePreferredCooldown: rule.IgnorePreferredCooldown,
		RecordOnSuccess:         rule.RecordOnSuccess,
		Parts:                   parts,
	}
}

func (t channelAffinityTemplate) BuildKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(t.Parts) == 0 {
		return ""
	}

	sum := sha256.Sum256([]byte(value))
	parts := append(append([]string(nil), t.Parts...), hex.EncodeToString(sum[:16]))
	return strings.Join(parts, "/")
}

func derivedResponseAffinityValues(response *types.OpenAIResponsesResponses) map[string]string {
	if response == nil {
		return nil
	}
	values := map[string]string{}
	if responseID := strings.TrimSpace(response.ID); responseID != "" {
		values[config.ChannelAffinityAliasResponseID] = responseID
	}
	if promptCacheKey := strings.TrimSpace(response.PromptCacheKey); promptCacheKey != "" {
		values[config.ChannelAffinityAliasPromptCacheKey] = promptCacheKey
	}
	return values
}

func refreshChannelAffinityMeta(c *gin.Context, state *channelAffinityState, channelID int) {
	if c == nil {
		return
	}
	if state == nil || state.Lookup == nil {
		meta := map[string]any{
			"channel_affinity_enabled": config.RuntimeChannelAffinitySettings(config.GlobalOption.RuntimeSnapshot()).Enabled,
			"channel_affinity_hit":     false,
		}
		mergeRoutingGroupLogMeta(c, meta)
		c.Set(config.GinChannelAffinityMetaKey, meta)
		return
	}

	meta := map[string]any{
		"channel_affinity_enabled":         config.RuntimeChannelAffinitySettings(config.GlobalOption.RuntimeSnapshot()).Enabled,
		"channel_affinity_kind":            string(state.Kind),
		"channel_affinity_rule":            state.Lookup.Template.RuleName,
		"channel_affinity_alias":           state.Lookup.Template.Source,
		"channel_affinity_source":          state.Lookup.Template.InputSource,
		"channel_affinity_hit":             state.Hit,
		"channel_affinity_preferred_id":    state.PreferredChannelID,
		"channel_affinity_selected_id":     channelID,
		"channel_affinity_skip_retry":      state.Lookup.Template.SkipRetryOnFailure,
		"channel_affinity_strict":          state.Lookup.Template.Strict,
		"channel_affinity_ignore_cooldown": state.Lookup.Template.IgnorePreferredCooldown,
		"channel_affinity_record_bindings": len(state.RequestBindings),
		"channel_affinity_fallback":        state.Hit && state.PreferredChannelID > 0 && channelID > 0 && channelID != state.PreferredChannelID,
	}
	if value := strings.TrimSpace(state.Lookup.Value); value != "" {
		sum := sha256.Sum256([]byte(value))
		meta["channel_affinity_value_hash"] = hex.EncodeToString(sum[:8])
	}
	if fingerprint := channelAffinityStateResumeFingerprint(state); fingerprint != "" {
		sum := sha256.Sum256([]byte(fingerprint))
		meta["channel_affinity_resume_fingerprint_hash"] = hex.EncodeToString(sum[:8])
	}
	mergeRoutingGroupLogMeta(c, meta)
	c.Set(config.GinChannelAffinityMetaKey, meta)
}

func channelAffinityModelName(kind channelAffinityKind, input channelAffinityInput) string {
	if modelName := strings.TrimSpace(input.ModelName); modelName != "" {
		return modelName
	}
	switch kind {
	case channelAffinityKindResponses:
		if input.ResponsesRequest != nil {
			return input.ResponsesRequest.Model
		}
	case channelAffinityKindRealtime:
		return input.ModelName
	}
	return ""
}

func channelAffinityManager() *runtimeaffinity.Manager {
	snapshot := config.GlobalOption.RuntimeSnapshot()
	settings := config.RuntimeChannelAffinitySettings(snapshot)
	version := snapshot.Version()
	channelAffinityManagerPublication.Lock()
	defer channelAffinityManagerPublication.Unlock()
	if channelAffinityManagerPublication.owner != config.GlobalOption || version > channelAffinityManagerPublication.version {
		manager := runtimeaffinity.ConfigureDefault(channelAffinityManagerOptions(settings))
		channelAffinityManagerPublication.owner = config.GlobalOption
		channelAffinityManagerPublication.version = version
		return manager
	}
	return runtimeaffinity.DefaultManager()
}

func channelAffinityManagerOptions(settings config.ChannelAffinitySettings) runtimeaffinity.ManagerOptions {
	options := runtimeaffinity.ManagerOptions{
		DefaultTTL:      time.Duration(settings.DefaultTTLSeconds) * time.Second,
		JanitorInterval: defaultChannelAffinityJanitorInterval,
		MaxEntries:      settings.MaxEntries,
		RedisPrefix:     defaultChannelAffinityRedisPrefix,
	}
	if config.RedisEnabled {
		options.RedisClient = commonredis.GetRedisClient()
	}
	return options
}

func channelAffinityRegexMatch(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return true
	}

	compiled, ok := affinityRegexCache.Load(pattern)
	if !ok {
		reg, err := regexp.Compile(pattern)
		if err != nil {
			return false
		}
		compiled, _ = affinityRegexCache.LoadOrStore(pattern, reg)
	}
	reg, ok := compiled.(*regexp.Regexp)
	if !ok {
		return false
	}
	return reg.MatchString(strings.TrimSpace(value))
}

func appendUniqueChannelAffinityBinding(bindings []*channelAffinityBinding, binding *channelAffinityBinding) []*channelAffinityBinding {
	if binding == nil || strings.TrimSpace(binding.Key) == "" {
		return bindings
	}
	for _, existing := range bindings {
		if existing != nil && existing.Key == binding.Key {
			return bindings
		}
	}
	return append(bindings, binding)
}

func appendChannelAffinityRecorder(recorders map[string][]channelAffinityTemplate, alias string, template channelAffinityTemplate) map[string][]channelAffinityTemplate {
	if recorders == nil {
		recorders = make(map[string][]channelAffinityTemplate)
	}
	alias = strings.TrimSpace(alias)
	if alias == "" || !template.RecordOnSuccess {
		return recorders
	}
	for _, existing := range recorders[alias] {
		if existing.RuleName == template.RuleName && existing.Source == template.Source && strings.Join(existing.Parts, "/") == strings.Join(template.Parts, "/") {
			return recorders
		}
	}
	recorders[alias] = append(recorders[alias], template)
	return recorders
}

func channelAffinityResumeFingerprint(kind channelAffinityKind, input channelAffinityInput) string {
	if kind != channelAffinityKindResponses && kind != channelAffinityKindChat {
		return ""
	}
	modelName := strings.TrimSpace(channelAffinityModelName(kind, input))
	if modelName == "" {
		return ""
	}
	return "model:" + modelName
}

func channelAffinityStateResumeFingerprint(state *channelAffinityState) string {
	if state == nil {
		return ""
	}
	return strings.TrimSpace(state.ResumeFingerprint)
}

func channelAffinityRecordMatchesState(record runtimeaffinity.Record, state *channelAffinityState) bool {
	recordFingerprint := strings.TrimSpace(record.ResumeFingerprint)
	stateFingerprint := channelAffinityStateResumeFingerprint(state)
	if recordFingerprint == "" || stateFingerprint == "" {
		return true
	}
	return recordFingerprint == stateFingerprint
}

func defaultChannelAffinityAlias(kind channelAffinityKind) string {
	switch kind {
	case channelAffinityKindRealtime:
		return config.ChannelAffinityAliasSessionID
	default:
		return config.ChannelAffinityAliasPromptCacheKey
	}
}
