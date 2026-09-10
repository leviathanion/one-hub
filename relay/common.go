package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/common/surface"
	"one-api/common/utils"
	"one-api/controller"
	"one-api/middleware"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/providers/claude"
	"one-api/relay/relay_util"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type realtimeChannelSelection struct {
	preferredChannelID      int
	ignorePreferredCooldown bool
	strictPreferredChannel  bool
	allowChannelTypes       []int
	skipChannelIDs          []int
}

const requestChannelCapabilityContextKey = "request_channel_capability"

type requestChannelCapability func(channel *model.Channel) error

func setRequestChannelCapability(c *gin.Context, capability requestChannelCapability) {
	if c == nil {
		return
	}
	if capability == nil {
		c.Set(requestChannelCapabilityContextKey, nil)
		return
	}
	c.Set(requestChannelCapabilityContextKey, capability)
}

func currentRequestChannelCapability(c *gin.Context) requestChannelCapability {
	if c == nil {
		return nil
	}
	value, ok := c.Get(requestChannelCapabilityContextKey)
	if !ok || value == nil {
		return nil
	}
	capability, _ := value.(requestChannelCapability)
	return capability
}

func invalidChannelRuntimeConfigAPIError(err error) *types.OpenAIErrorWithStatusCode {
	var invalidConfig *model.InvalidChannelRuntimeConfigError
	if !errors.As(err, &invalidConfig) {
		return nil
	}
	return common.StringErrorWrapperLocal("invalid channel runtime config", model.InvalidChannelRuntimeConfigCode, http.StatusServiceUnavailable)
}

func Path2Relay(c *gin.Context, path string) RelayBaseInterface {
	var relay RelayBaseInterface
	if strings.HasPrefix(path, "/v1/chat/completions") {
		relay = NewRelayChat(c)
	} else if strings.HasPrefix(path, "/v1/completions") {
		relay = NewRelayCompletions(c)
	} else if strings.HasPrefix(path, "/v1/embeddings") {
		relay = NewRelayEmbeddings(c)
	} else if strings.HasPrefix(path, "/v1/moderations") {
		relay = NewRelayModerations(c)
	} else if strings.HasPrefix(path, "/v1/images/generations") || strings.HasPrefix(path, "/recraftAI/v1/images/generations") {
		relay = NewRelayImageGenerations(c)
	} else if strings.HasPrefix(path, "/v1/images/edits") {
		relay = NewRelayImageEdits(c)
	} else if strings.HasPrefix(path, "/v1/images/variations") {
		relay = NewRelayImageVariations(c)
	} else if strings.HasPrefix(path, "/v1/audio/speech") {
		relay = NewRelaySpeech(c)
	} else if strings.HasPrefix(path, "/v1/audio/transcriptions") {
		relay = NewRelayTranscriptions(c)
	} else if strings.HasPrefix(path, "/v1/audio/translations") {
		relay = NewRelayTranslations(c)
	} else if strings.HasPrefix(path, "/claude") {
		relay = NewRelayClaudeOnly(c)
	} else if strings.HasPrefix(path, "/gemini") {
		relay = NewRelayGeminiOnly(c)
	} else if strings.HasPrefix(path, "/v1/responses") {
		relay = NewRelayResponses(c)
	} else if IsRecraftNativePath(path) {
		relay = NewRelayRecraftNative(c)
	}

	return relay
}

func checkLimitModel(c *gin.Context, modelName string) (error error) {
	return middleware.EnsureTokenModelAllowed(c, modelName)
}

func GetProvider(c *gin.Context, modelName string) (provider providersBase.ProviderInterface, newModelName string, fail error) {
	// 检查模型限制
	if modelName != "" {
		if err := checkLimitModel(c, modelName); err != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return nil, "", err
		}
	}
	channel, fail := fetchChannel(c, modelName)
	if fail != nil {
		return
	}

	return prepareProviderForChannel(c, modelName, channel)
}

// GetProviderForOwnerChannel resolves an exact channel incarnation after the
// caller has located it through a durable owner. New child work still requires
// current principal, group and model permission on that exact channel.
func GetProviderForOwnerChannel(c *gin.Context, modelName string, channelID int) (provider providersBase.ProviderInterface, newModelName string, fail error) {
	if c == nil || channelID <= 0 {
		return nil, "", errors.New("durable owner channel is invalid")
	}
	if apiErr := middleware.RefreshAuthenticatedLongLivedPrincipal(c); apiErr != nil {
		return nil, "", apiErr
	}
	if modelName != "" {
		if err := checkLimitModel(c, modelName); err != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return nil, "", err
		}
	}
	channel, err := fetchOwnerChannelById(c.Request.Context(), channelID)
	if err != nil {
		return nil, "", err
	}
	if apiErr := middleware.AdmitAuthenticatedChannelWork(c, modelName, channelID); apiErr != nil {
		return nil, "", apiErr
	}
	if capability := currentRequestChannelCapability(c); capability != nil {
		if err := capability(channel); err != nil {
			return nil, "", err
		}
	}
	return prepareProviderForChannel(c, modelName, channel)
}

func prepareProviderForChannel(c *gin.Context, modelName string, channel *model.Channel) (provider providersBase.ProviderInterface, newModelName string, fail error) {
	if channel == nil {
		fail = errors.New("channel not found")
		return
	}
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		fail = model.NewInvalidChannelRuntimeConfigError(channel.Id, err)
		return
	}
	channel.ParseRuntimeConfig()

	c.Set("channel_id", channel.Id)
	c.Set("channel_type", channel.Type)

	if strings.HasPrefix(c.Request.URL.Path, "/claude") && channel.Type == config.ChannelTypeCustom {
		if !channel.CustomClaudeRelayEnabled() {
			fail = errors.New("selected channel does not enable native Claude Messages")
			return
		}
		provider = claude.CreateClaudeProvider(channel, "")
	} else {
		provider = providers.GetProvider(channel, c)
	}
	if provider == nil {
		fail = errors.New("channel not found")
		return
	}
	provider.SetContext(c)
	provider.SetOriginalModel(modelName)
	c.Set("original_model", modelName)

	newModelName, fail = provider.ModelMappingHandler(modelName)
	if fail != nil {
		return
	}

	BillingOriginalModel := false

	if strings.HasPrefix(newModelName, "+") {
		newModelName = newModelName[1:]
		BillingOriginalModel = true
	}

	c.Set("new_model", newModelName)
	c.Set("billing_original_model", BillingOriginalModel)

	return
}

func fetchChannel(c *gin.Context, modelName string) (channel *model.Channel, fail error) {
	channelId := explicitChannelPinID(c)
	if channelId > 0 {
		if responseOwnerChannelID(c) == channelId {
			channel, fail = fetchOwnerChannelById(c.Request.Context(), channelId)
		} else {
			channel, fail = fetchChannelById(channelId)
		}
		if fail != nil {
			return nil, fail
		}
		if capability := currentRequestChannelCapability(c); capability != nil {
			if err := capability(channel); err != nil {
				return nil, err
			}
		}
		return channel, nil
	}

	return fetchChannelByModel(c, modelName)
}

func explicitChannelPinID(c *gin.Context) int {
	if c == nil || c.GetBool("specific_channel_id_ignore") {
		return 0
	}
	return c.GetInt("specific_channel_id")
}

func fetchChannelById(channelId int) (*model.Channel, error) {
	channel, err := model.GetChannelById(channelId)
	if err != nil {
		return nil, errors.New("无效的渠道 Id")
	}
	if channel.Status != config.ChannelStatusEnabled {
		return nil, errors.New("该渠道已被禁用")
	}
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		return nil, model.NewInvalidChannelRuntimeConfigError(channel.Id, err)
	}

	return channel, nil
}

func fetchOwnerChannelById(ctx context.Context, channelID int) (*model.Channel, error) {
	channel, err := model.GetChannelIncarnationByID(ctx, channelID)
	if err != nil {
		return nil, errors.New("无效的 owner 渠道 Id")
	}
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		return nil, model.NewInvalidChannelRuntimeConfigError(channel.Id, err)
	}
	return channel, nil
}

// GroupManager 统一管理分组逻辑
type GroupManager struct {
	primaryGroup string
	backupGroup  string
	context      *gin.Context
}

// NewGroupManager 创建分组管理器
func NewGroupManager(c *gin.Context) *GroupManager {
	return &GroupManager{
		primaryGroup: groupctx.CurrentRoutingGroup(c),
		backupGroup:  groupctx.BackupGroup(c),
		context:      c,
	}
}

// TryWithGroups 尝试使用主分组和备用分组
func (gm *GroupManager) TryWithGroups(modelName string, filters []model.ChannelsFilterFunc, operation func(group string) (*model.Channel, error)) (*model.Channel, error) {
	// 首先尝试主分组
	if gm.primaryGroup != "" {
		channel, err := gm.tryGroup(gm.primaryGroup, modelName, filters, operation)
		if err == nil {
			gm.context.Set("is_backupGroup", false)
			return channel, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		logger.LogError(gm.context.Request.Context(), fmt.Sprintf("主分组 %s 失败: %v", gm.primaryGroup, err))
	}

	// 如果主分组失败，尝试备用分组
	if gm.backupGroup != "" && gm.backupGroup != gm.primaryGroup {
		logger.LogInfo(gm.context.Request.Context(), fmt.Sprintf("尝试使用备用分组: %s", gm.backupGroup))
		channel, err := gm.tryGroup(gm.backupGroup, modelName, filters, operation)
		if err == nil {
			groupctx.SetRoutingGroup(gm.context, gm.backupGroup, groupctx.RoutingGroupSourceBackupGroup)
			gm.context.Set("is_backupGroup", true)
			if err := gm.setGroupRatio(gm.backupGroup); err != nil {
				return nil, fmt.Errorf("设置备用分组倍率失败: %v", err)
			}
			return channel, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		logger.LogError(gm.context.Request.Context(), fmt.Sprintf("备用分组 %s 也失败: %v", gm.backupGroup, err))
		return nil, gm.createGroupError(gm.backupGroup, modelName, channel)
	}
	return nil, gm.createGroupError(gm.primaryGroup, modelName, nil)
}

// tryGroup 尝试使用指定分组
func (gm *GroupManager) tryGroup(group string, modelName string, filters []model.ChannelsFilterFunc, operation func(group string) (*model.Channel, error)) (*model.Channel, error) {
	if group == "" {
		return nil, errors.New("分组为空")
	}
	return operation(group)
}

// setGroupRatio 设置分组比例
func (gm *GroupManager) setGroupRatio(group string) error {
	groupRatio := model.GlobalUserGroupRatio.GetBySymbol(group)
	if groupRatio == nil {
		return fmt.Errorf("分组 %s 不存在", group)
	}
	gm.context.Set("group_ratio", groupRatio.Ratio)
	return nil
}

// createGroupError 创建统一的分组错误信息
func (gm *GroupManager) createGroupError(group string, modelName string, channel *model.Channel) error {
	if channel != nil {
		logger.SysError(fmt.Sprintf("渠道不存在：%d", channel.Id))
		return errors.New("数据库一致性已被破坏，请联系管理员")
	}
	return fmt.Errorf("当前分组 %s 下对于模型 %s 无可用渠道", group, modelName)
}

func fetchChannelByModel(c *gin.Context, modelName string) (*model.Channel, error) {
	return fetchChannelByModelWithSelection(c, modelName, currentRealtimeChannelSelection(c))
}

func isClaudeRouteEligibleChannel(channel *model.Channel) bool {
	if channel == nil {
		return false
	}

	switch channel.Type {
	case config.ChannelTypeAnthropic, config.ChannelTypeVertexAI, config.ChannelTypeBedrock:
		return true
	case config.ChannelTypeCustom:
		return channel.CustomClaudeRelayEnabled()
	default:
		return false
	}
}

func filterNonClaudeRouteEligibleChannel(_ int, choice *model.ChannelChoice) bool {
	if choice == nil || choice.Channel == nil {
		return true
	}
	return !isClaudeRouteEligibleChannel(choice.Channel)
}

func currentRealtimeChannelSelection(c *gin.Context) realtimeChannelSelection {
	selection := realtimeChannelSelection{
		preferredChannelID:      currentPreferredChannelID(c),
		ignorePreferredCooldown: currentChannelAffinityIgnorePreferredCooldown(c),
		strictPreferredChannel:  currentChannelAffinityStrict(c),
	}

	if skipChannelIds, ok := utils.GetGinValue[[]int](c, "skip_channel_ids"); ok && len(skipChannelIds) > 0 {
		selection.skipChannelIDs = append(selection.skipChannelIDs, skipChannelIds...)
	}

	if types, exists := c.Get("allow_channel_type"); exists {
		if allowTypes, ok := types.([]int); ok && len(allowTypes) > 0 {
			selection.allowChannelTypes = append(selection.allowChannelTypes, allowTypes...)
		}
	}

	return selection
}

func channelIDInList(ids []int, target int) bool {
	if target <= 0 {
		return false
	}
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func preferredChannelWaitBudget(c *gin.Context) time.Duration {
	waitMilliseconds := 0
	if snapshot := config.GlobalOption.RuntimeSnapshot(); snapshot != nil {
		waitMilliseconds = snapshot.Int("PreferredChannelWaitMilliseconds", 0)
	} else {
		waitMilliseconds = config.PreferredChannelWaitMilliseconds
	}
	if waitMilliseconds <= 0 {
		return 0
	}
	return time.Duration(waitMilliseconds) * time.Millisecond
}

func preferredChannelWaitPollInterval(c *gin.Context) time.Duration {
	pollMilliseconds := 50
	if snapshot := config.GlobalOption.RuntimeSnapshot(); snapshot != nil {
		pollMilliseconds = snapshot.Int("PreferredChannelWaitPollMilliseconds", 50)
	} else {
		pollMilliseconds = config.PreferredChannelWaitPollMilliseconds
	}
	if pollMilliseconds <= 0 {
		return 50 * time.Millisecond
	}
	return time.Duration(pollMilliseconds) * time.Millisecond
}

func requestContextErr(c *gin.Context) error {
	if c == nil || c.Request == nil {
		return nil
	}
	return c.Request.Context().Err()
}

func recordPreferredChannelWaitMeta(c *gin.Context, budget, waited time.Duration, exhausted, canceled bool) {
	mergeChannelAffinityMeta(c, map[string]any{
		"channel_affinity_wait_triggered": true,
		"channel_affinity_wait_budget_ms": budget.Milliseconds(),
		"channel_affinity_waited_ms":      waited.Milliseconds(),
		"channel_affinity_wait_exhausted": exhausted,
		"channel_affinity_wait_canceled":  canceled,
	})
}

func waitForPreferredChannelCooldown(c *gin.Context, group, modelName string, selection realtimeChannelSelection, filters []model.ChannelsFilterFunc) error {
	if selection.preferredChannelID <= 0 || selection.ignorePreferredCooldown {
		return nil
	}

	budget := preferredChannelWaitBudget(c)
	if budget <= 0 {
		return nil
	}

	eligible, err := model.ChannelGroup.PreferredChannelEligible(group, modelName, selection.preferredChannelID, filters...)
	if err != nil || !eligible || !model.ChannelGroup.IsInCooldown(selection.preferredChannelID, modelName) {
		return nil
	}

	pollInterval := preferredChannelWaitPollInterval(c)
	if pollInterval <= 0 {
		pollInterval = 50 * time.Millisecond
	}

	waitCtx := context.Background()
	if c != nil && c.Request != nil {
		waitCtx = c.Request.Context()
	}
	start := time.Now()
	deadline := start.Add(budget)
	if requestDeadline, ok := waitCtx.Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	waitExhausted := false

	for {
		if err := waitCtx.Err(); err != nil {
			recordPreferredChannelWaitMeta(c, budget, time.Since(start), waitExhausted, true)
			return err
		}
		eligible, err := model.ChannelGroup.PreferredChannelEligible(group, modelName, selection.preferredChannelID, filters...)
		if err != nil || !eligible || !model.ChannelGroup.IsInCooldown(selection.preferredChannelID, modelName) {
			break
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			if err := waitCtx.Err(); err != nil {
				recordPreferredChannelWaitMeta(c, budget, time.Since(start), waitExhausted, true)
				return err
			}
			waitExhausted = true
			break
		}

		sleepFor := pollInterval
		if sleepFor > remaining {
			sleepFor = remaining
		}
		timer := time.NewTimer(sleepFor)
		select {
		case <-waitCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			recordPreferredChannelWaitMeta(c, budget, time.Since(start), waitExhausted, true)
			return waitCtx.Err()
		case <-timer.C:
		}
	}

	recordPreferredChannelWaitMeta(c, budget, time.Since(start), waitExhausted, false)
	return nil
}

func fetchChannelByModelWithSelection(c *gin.Context, modelName string, selection realtimeChannelSelection) (*model.Channel, error) {
	if err := requestContextErr(c); err != nil {
		return nil, err
	}

	skipOnlyChat := c.GetBool("skip_only_chat")
	isStream := c.GetBool("is_stream")
	setChannelAffinitySelectedPreferred(c, false)

	var baseFilters []model.ChannelsFilterFunc
	var rejectedCapabilityErr error
	if skipOnlyChat {
		baseFilters = append(baseFilters, model.FilterOnlyChat())
	}

	if len(selection.skipChannelIDs) > 0 {
		baseFilters = append(baseFilters, model.FilterChannelId(selection.skipChannelIDs))
	}

	if len(selection.allowChannelTypes) > 0 {
		baseFilters = append(baseFilters, model.FilterChannelTypes(selection.allowChannelTypes))
	}
	if strings.HasPrefix(c.Request.URL.Path, "/claude") {
		baseFilters = append(baseFilters, model.FilterFunc(filterNonClaudeRouteEligibleChannel))
	}

	if isStream {
		baseFilters = append(baseFilters, model.FilterDisabledStream(modelName))
	}
	filters := append([]model.ChannelsFilterFunc(nil), baseFilters...)
	var capabilityFilter model.ChannelsFilterFunc
	if capability := currentRequestChannelCapability(c); capability != nil {
		capabilityFilter = model.FilterFunc(func(_ int, choice *model.ChannelChoice) bool {
			if choice == nil || choice.Channel == nil {
				return true
			}
			if err := capability(choice.Channel); err != nil {
				if rejectedCapabilityErr == nil {
					rejectedCapabilityErr = err
				}
				return true
			}
			return false
		})
		filters = append(filters, capabilityFilter)
	}

	// 使用统一的分组管理器
	groupManager := NewGroupManager(c)
	channel, err := groupManager.TryWithGroups(modelName, filters, func(group string) (*model.Channel, error) {
		if err := waitForPreferredChannelCooldown(c, group, modelName, selection, filters); err != nil {
			return nil, err
		}
		channel, err := model.ChannelGroup.NextWithPreferred(group, modelName, selection.preferredChannelID, selection.ignorePreferredCooldown, filters...)
		if err != nil {
			return nil, err
		}
		if selection.preferredChannelID > 0 && (channel == nil || channel.Id != selection.preferredChannelID) {
			preferredSkippedForRequest := channelIDInList(selection.skipChannelIDs, selection.preferredChannelID)
			// A skip-list miss means this request already tried the preferred
			// channel and is intentionally avoiding it for retry. Do not erase
			// durable affinity on that transient signal; only clear records when
			// the preferred channel is genuinely unavailable to normal selection.
			if !preferredSkippedForRequest && !selection.strictPreferredChannel {
				clearCurrentChannelAffinity(c)
			}
			if selection.strictPreferredChannel {
				return nil, errors.New("preferred affinity channel is unavailable")
			}
		}
		setChannelAffinitySelectedPreferred(c, channel != nil && selection.preferredChannelID > 0 && channel.Id == selection.preferredChannelID)
		return channel, nil
	})
	if err != nil {
		if contextErr := requestContextErr(c); contextErr != nil {
			return nil, contextErr
		}
		if rejectedCapabilityErr != nil && capabilityFilter != nil && allSelectionCandidatesRejectCapability(groupManager, modelName, baseFilters, capabilityFilter) {
			return nil, rejectedCapabilityErr
		}
	}
	return channel, err
}

func allSelectionCandidatesRejectCapability(groupManager *GroupManager, modelName string, baseFilters []model.ChannelsFilterFunc, capabilityFilter model.ChannelsFilterFunc) bool {
	if groupManager == nil || capabilityFilter == nil {
		return false
	}
	groups := []string{groupManager.primaryGroup, groupManager.backupGroup}
	seen := make(map[string]struct{}, len(groups))
	foundBaseCandidate := false
	capabilityFilters := make([]model.ChannelsFilterFunc, 0, len(baseFilters)+1)
	capabilityFilters = append(capabilityFilters, baseFilters...)
	capabilityFilters = append(capabilityFilters, capabilityFilter)
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if _, exists := seen[group]; exists {
			continue
		}
		seen[group] = struct{}{}
		if !model.ChannelGroup.ModelHasCandidate(group, modelName, baseFilters...) {
			continue
		}
		foundBaseCandidate = true
		if model.ChannelGroup.ModelHasCandidate(group, modelName, capabilityFilters...) {
			return false
		}
	}
	return foundBaseCandidate
}

func responseJsonClient(c *gin.Context, data interface{}) *types.OpenAIErrorWithStatusCode {
	var responseBody []byte
	if rawResponse, ok := data.(requester.ProviderRawJSONReplayer); ok {
		responseBody = rawResponse.ReplayProviderRawJSON()
	}
	if len(responseBody) == 0 {
		var err error
		responseBody, err = json.Marshal(data)
		if err != nil {
			logger.LogError(c.Request.Context(), "marshal_response_body_failed:"+err.Error())
			return nil
		}
		// JSON marshaling is a new representation even when the dialect is the
		// same. Do this before copying provider headers so Content-Length,
		// Content-Encoding, validators, and ranges cannot describe the old body.
		invalidateProviderRepresentationHeaders(c)
	}
	if safeBody, changed := common.RedactProviderMetadataJSON(responseBody); changed {
		responseBody = safeBody
		invalidateProviderRepresentationHeaders(c)
	}

	applyProviderResponseHeaders(c)
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(providerResponseStatus(c, http.StatusOK))
	_, err := c.Writer.Write(responseBody)
	if err != nil {
		logger.LogError(c.Request.Context(), "write_response_body_failed:"+err.Error())
	}

	return nil
}

func invalidateProviderRepresentationHeaders(c *gin.Context) {
	if c == nil {
		return
	}
	value, ok := c.Get(requestctx.ProviderResponseHeadersContextKey)
	if !ok || value == nil {
		return
	}
	headers, ok := value.(http.Header)
	if !ok {
		return
	}
	headers = headers.Clone()
	for _, name := range []string{
		"Content-Encoding",
		"Content-Length",
		"Content-Range",
		"Digest",
		"Etag",
	} {
		headers.Del(name)
	}
	c.Set(requestctx.ProviderResponseHeadersContextKey, headers)
}

func providerResponseStatus(c *gin.Context, fallback int) int {
	if c == nil {
		return fallback
	}
	status := c.GetInt(requestctx.ProviderResponseStatusContextKey)
	if status < 100 || status > 599 {
		return fallback
	}
	return status
}

func applyProviderResponseHeaders(c *gin.Context) {
	if c == nil {
		return
	}
	value, ok := c.Get(requestctx.ProviderResponseHeadersContextKey)
	if !ok || value == nil {
		return
	}
	headers, ok := value.(http.Header)
	if !ok {
		return
	}
	for name, values := range headers {
		c.Writer.Header()[name] = append([]string(nil), values...)
	}
}

type StreamEndHandler func() string

const streamErrorClientMessage = "stream interrupted"

type sseBoundaryTracker struct {
	tail string
}

func (t *sseBoundaryTracker) Observe(data string) {
	if t == nil || data == "" {
		return
	}
	t.tail += data
	if len(t.tail) > 4 {
		t.tail = t.tail[len(t.tail)-4:]
	}
}

func (t *sseBoundaryTracker) CompleteEvent(write func(string) error) error {
	if t == nil || write == nil {
		return nil
	}
	suffix := "\n\n"
	switch {
	case strings.HasSuffix(t.tail, "\r\n\r\n"), strings.HasSuffix(t.tail, "\n\n"), strings.HasSuffix(t.tail, "\r\r"):
		return nil
	case strings.HasSuffix(t.tail, "\r\n"):
		suffix = "\r\n"
	case strings.HasSuffix(t.tail, "\n"):
		suffix = "\n"
	case strings.HasSuffix(t.tail, "\r"):
		suffix = "\r"
	}
	if err := write(suffix); err != nil {
		return err
	}
	t.Observe(suffix)
	return nil
}

func isStreamTerminalEOF(err error) bool {
	return err == nil || errors.Is(err, io.EOF)
}

func responseStreamClient(c *gin.Context, stream requester.StreamReaderInterface[string], endHandler StreamEndHandler, observers ...func(string)) (firstResponseTime time.Time, errWithOP *types.OpenAIErrorWithStatusCode) {
	applyProviderResponseHeaders(c)
	requester.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(providerResponseStatus(c, http.StatusOK))
	dataChan, errChan := stream.Recv()
	rawSSEEvents := requester.IsRawSSEEventStream(stream)

	defer requester.CloseAndDrainStream(stream)
	streamWriter := relay_util.NewBufferedStreamWriter(c.Writer, 0)
	defer streamWriter.Close()

	var isFirstResponse bool
	var sawProviderInBandError bool
	var providerInBandError *types.OpenAIErrorWithStatusCode
	var sawProviderData bool
	dataOpen := dataChan != nil
	errOpen := errChan != nil

	handleData := func(data string) error {
		payload := data
		hasPayload := true
		if rawSSEEvents {
			payload, hasPayload = commonresponses.SSEDataPayload(data)
			if hasPayload && strings.TrimSpace(payload) == "[DONE]" {
				hasPayload = false
			}
		}
		if hasPayload {
			apiErr := runtimesession.ProviderAPIErrorFromPayload([]byte(payload))
			if rawSSEEvents {
				apiErr = runtimesession.OpenAIErrorEnvelopeFromPayload([]byte(payload))
			}
			if apiErr != nil {
				sawProviderInBandError = true
				if providerInBandError == nil {
					providerInBandError = providerresponse.SanitizeAPIError(apiErr)
				}
			} else {
				sawProviderData = true
			}
			for _, observe := range observers {
				if observe != nil {
					observe(payload)
				}
			}
		}
		streamData := ""
		if rawSSEEvents {
			streamData = sanitizeOpenAIChatCompletionSSEEvent(data)
		} else {
			payload = string(sanitizeProviderJSONPayload([]byte(payload)))
			streamData = "data: " + payload + "\n\n"
		}
		if !isFirstResponse {
			firstResponseTime = time.Now()
			isFirstResponse = true
		}

		select {
		case <-c.Request.Context().Done():
			return c.Request.Context().Err()
		default:
			_, err := streamWriter.WriteString(streamData)
			return err
		}
	}

	handleEOF := func() error {
		if rawSSEEvents {
			return nil
		}
		if sawProviderInBandError {
			return nil
		}
		if endHandler != nil {
			streamData := endHandler()
			if streamData != "" {
				select {
				case <-c.Request.Context().Done():
					return c.Request.Context().Err()
				default:
					if _, err := streamWriter.WriteString("data: " + streamData + "\n\n"); err != nil {
						return err
					}
				}
			}
		}

		select {
		case <-c.Request.Context().Done():
			return c.Request.Context().Err()
		default:
			_, err := streamWriter.WriteString("data: [DONE]\n\n")
			return err
		}
	}
	renderedProviderInBandError := func() *types.OpenAIErrorWithStatusCode {
		if providerInBandError == nil {
			return nil
		}
		failure := *providerInBandError
		failure.UpstreamAccepted = failure.UpstreamAccepted || sawProviderData
		c.Set(streamErrorAlreadyRenderedContextKey, true)
		return &failure
	}

	handleError := func(err error) error {
		if isStreamTerminalEOF(err) {
			return handleEOF()
		}
		if sawProviderInBandError {
			c.Set(streamErrorAlreadyRenderedContextKey, true)
			return nil
		}
		errPayload := map[string]any{
			"error": map[string]any{
				"message": streamErrorClientMessage,
				"type":    "stream_error",
				"code":    "stream_error",
			},
		}
		var providerErr *types.OpenAIErrorWithStatusCode
		if errors.As(err, &providerErr) && providerErr != nil && !providerErr.LocalError {
			safeErr := providerresponse.SanitizeAPIError(providerErr)
			errPayload["error"] = safeErr.OpenAIError
		}
		errJSON, _ := json.Marshal(errPayload)
		errMsg := "data: " + string(errJSON) + "\n\n"
		select {
		case <-c.Request.Context().Done():
			return c.Request.Context().Err()
		default:
			if _, writeErr := streamWriter.WriteString(errMsg); writeErr != nil {
				return writeErr
			}
		}
		c.Set(streamErrorAlreadyRenderedContextKey, true)

		logger.LogError(c.Request.Context(), "Stream err:"+common.RedactSensitiveText(err.Error()))
		return nil
	}

	writeFailure := func(err error) *types.OpenAIErrorWithStatusCode {
		c.Set(streamErrorAlreadyRenderedContextKey, true)
		apiErr := common.ErrorWrapper(err, "stream_write_failed", http.StatusInternalServerError)
		apiErr.UpstreamAccepted = true
		return apiErr
	}
	streamFailure := func(err error) *types.OpenAIErrorWithStatusCode {
		var providerErr *types.OpenAIErrorWithStatusCode
		if errors.As(err, &providerErr) && providerErr != nil {
			safeErr := providerresponse.SanitizeAPIError(providerErr)
			failure := *safeErr
			// A stream has already opened. Local parsing/framing failures cannot
			// establish provider rejection, even before the first converted chunk.
			failure.UpstreamAccepted = failure.UpstreamAccepted || sawProviderData || failure.LocalError
			return &failure
		}
		code := "stream_read_failed"
		if errors.Is(err, requester.ErrStreamLineTooLarge) || errors.Is(err, requester.ErrSSEEventTooLarge) {
			code = "provider_usage_state_limit"
		}
		apiErr := common.ErrorWrapper(err, code, http.StatusBadGateway)
		apiErr.LocalError = code == "provider_usage_state_limit"
		apiErr.UpstreamAccepted = true
		return apiErr
	}

	for dataOpen || errOpen {
		if dataOpen {
			select {
			case data, ok := <-dataChan:
				if !ok {
					dataOpen = false
					dataChan = nil
					continue
				}
				if err := handleData(data); err != nil {
					return firstResponseTime, writeFailure(err)
				}
				if inBandErr := renderedProviderInBandError(); inBandErr != nil {
					return firstResponseTime, inBandErr
				}
				continue
			default:
			}
		}

		select {
		case data, ok := <-dataChan:
			if !ok {
				dataOpen = false
				dataChan = nil
				continue
			}
			if err := handleData(data); err != nil {
				return firstResponseTime, writeFailure(err)
			}
			if inBandErr := renderedProviderInBandError(); inBandErr != nil {
				return firstResponseTime, inBandErr
			}
		case err, ok := <-errChan:
			if !ok {
				errOpen = false
				errChan = nil
				continue
			}
			if writeErr := handleError(err); writeErr != nil {
				return firstResponseTime, writeFailure(writeErr)
			}
			if inBandErr := renderedProviderInBandError(); inBandErr != nil {
				return firstResponseTime, inBandErr
			}
			if !isStreamTerminalEOF(err) {
				return firstResponseTime, streamFailure(err)
			}
			return firstResponseTime, nil
		}
	}

	if err := handleEOF(); err != nil {
		return firstResponseTime, writeFailure(err)
	}
	if inBandErr := renderedProviderInBandError(); inBandErr != nil {
		return firstResponseTime, inBandErr
	}
	return firstResponseTime, nil
}

func responseGeneralStreamClient(c *gin.Context, stream requester.StreamReaderInterface[string], endHandler StreamEndHandler) (firstResponseTime time.Time) {
	return responseGeneralStreamClientWithObserver(c, stream, endHandler, nil)
}

func responseGeneralStreamClientWithObserver(c *gin.Context, stream requester.StreamReaderInterface[string], endHandler StreamEndHandler, observer func(string)) (firstResponseTime time.Time) {
	firstResponseTime, _ = responseGeneralStreamClientWithObserverResult(c, stream, endHandler, observer, nil, true)
	return firstResponseTime
}

func responseGeneralStreamClientWithObserverResult(c *gin.Context, stream requester.StreamReaderInterface[string], endHandler StreamEndHandler, observer func(string), transform func(string) string, renderStreamError bool) (firstResponseTime time.Time, streamErr error) {
	applyProviderResponseHeaders(c)
	requester.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(providerResponseStatus(c, http.StatusOK))
	dataChan, errChan := stream.Recv()

	defer requester.CloseAndDrainStream(stream)
	streamWriter := relay_util.NewBufferedStreamWriter(c.Writer, 0)
	defer streamWriter.Close()
	var framing sseBoundaryTracker
	ensureEventBoundary := func() error {
		return framing.CompleteEvent(func(data string) error {
			_, writeErr := streamWriter.WriteString(data)
			return writeErr
		})
	}
	var isFirstResponse bool
	dataOpen := dataChan != nil
	errOpen := errChan != nil

	handleData := func(data string) error {
		if !isFirstResponse {
			firstResponseTime = time.Now()
			isFirstResponse = true
		}
		if observer != nil {
			observer(data)
		}
		if transform != nil {
			data = transform(data)
		}
		select {
		case <-c.Request.Context().Done():
			return c.Request.Context().Err()
		default:
			if _, err := streamWriter.WriteString(data); err != nil {
				return err
			}
			framing.Observe(data)
		}
		return nil
	}

	handleEOF := func() error {
		if endHandler == nil {
			return nil
		}
		streamData := endHandler()
		if streamData == "" {
			return nil
		}
		if observer != nil {
			observer(streamData)
		}
		select {
		case <-c.Request.Context().Done():
			return c.Request.Context().Err()
		default:
			_, err := streamWriter.WriteString(streamData)
			return err
		}
	}

	handleError := func(err error) error {
		if isStreamTerminalEOF(err) {
			return handleEOF()
		}
		errPayload := map[string]any{
			"type":    "error",
			"code":    "stream_error",
			"message": streamErrorClientMessage,
		}
		errJSON, _ := json.Marshal(errPayload)
		errEvent := "event: error\ndata: " + string(errJSON) + "\n\n"
		if boundaryErr := ensureEventBoundary(); boundaryErr != nil {
			return boundaryErr
		}
		select {
		case <-c.Request.Context().Done():
			return c.Request.Context().Err()
		default:
			if _, writeErr := streamWriter.WriteString(errEvent); writeErr != nil {
				return writeErr
			}
		}
		framing.Observe(errEvent)
		c.Set(streamErrorAlreadyRenderedContextKey, true)

		logger.LogError(c.Request.Context(), "Stream err:"+common.RedactSensitiveText(err.Error()))
		return nil
	}

	for dataOpen || errOpen {
		if dataOpen {
			select {
			case data, ok := <-dataChan:
				if !ok {
					dataOpen = false
					dataChan = nil
					continue
				}
				if err := handleData(data); err != nil {
					return firstResponseTime, err
				}
				continue
			default:
			}
		}

		select {
		case <-c.Request.Context().Done():
			return firstResponseTime, c.Request.Context().Err()
		case data, ok := <-dataChan:
			if !ok {
				dataOpen = false
				dataChan = nil
				continue
			}
			if err := handleData(data); err != nil {
				return firstResponseTime, err
			}
		case err, ok := <-errChan:
			if !ok {
				errOpen = false
				errChan = nil
				continue
			}
			if renderStreamError {
				if writeErr := handleError(err); writeErr != nil {
					return firstResponseTime, writeErr
				}
			} else if !isStreamTerminalEOF(err) {
				if writeErr := ensureEventBoundary(); writeErr != nil {
					return firstResponseTime, writeErr
				}
			}
			if !isStreamTerminalEOF(err) {
				return firstResponseTime, err
			}
			return firstResponseTime, nil
		}
	}

	if err := handleEOF(); err != nil {
		return firstResponseTime, err
	}
	return firstResponseTime, nil
}

func sanitizeProviderJSONPayload(payload []byte) []byte {
	return sanitizeProviderJSONPayloadWithError(payload, runtimesession.ProviderAPIErrorFromPayload(payload))
}

func sanitizeProviderJSONPayloadWithError(payload []byte, apiErr *types.OpenAIErrorWithStatusCode) []byte {
	safe := providerresponse.SanitizeErrorPayload(payload, apiErr)
	if redacted, changed := common.RedactProviderMetadataJSON(safe); changed {
		return redacted
	}
	return safe
}

func sanitizeProviderSSEEvent(data string) string {
	return sanitizeProviderSSEEventWithErrorDetector(data, runtimesession.ProviderAPIErrorFromPayload)
}

func sanitizeOpenAIChatCompletionSSEEvent(data string) string {
	return sanitizeProviderSSEEventWithErrorDetector(data, runtimesession.OpenAIErrorEnvelopeFromPayload)
}

func sanitizeProviderSSEEventWithErrorDetector(data string, detectError func([]byte) *types.OpenAIErrorWithStatusCode) string {
	payload, hasData := commonresponses.SSEDataPayload(data)
	if !hasData {
		return data
	}
	var apiErr *types.OpenAIErrorWithStatusCode
	if detectError != nil {
		apiErr = detectError([]byte(payload))
	}
	safe := string(sanitizeProviderJSONPayloadWithError([]byte(payload), apiErr))
	if safe == payload {
		return data
	}
	// Only a security rewrite replaces the logical data payload. Preserve all
	// event/id/retry/comment fields and the original event-ending bytes.
	var out strings.Builder
	wroteData := false
	remaining := data
	for len(remaining) > 0 {
		lineEnd := strings.IndexByte(remaining, '\n')
		line := remaining
		if lineEnd >= 0 {
			line = remaining[:lineEnd+1]
		}
		remaining = remaining[len(line):]
		content := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if content != "data" && !strings.HasPrefix(content, "data:") {
			out.WriteString(line)
			continue
		}
		if !wroteData {
			out.WriteString("data: ")
			out.WriteString(safe)
			out.WriteString(line[len(content):])
			wroteData = true
		}
	}
	return out.String()
}

func responseMultipart(c *gin.Context, resp *http.Response, policy providerresponse.Policy) *types.OpenAIErrorWithStatusCode {
	defer resp.Body.Close()

	for k, v := range providerresponse.Filter(resp.Header, policy) {
		c.Writer.Header()[k] = append([]string(nil), v...)
	}

	c.Writer.WriteHeader(resp.StatusCode)
	c.Writer.WriteHeaderNow()

	_, err := io.Copy(c.Writer, resp.Body)
	if err != nil {
		// Status and possibly a body prefix are already committed. Abort the
		// transport so HTTP/1.x clients do not receive a clean chunk terminator and
		// HTTP/2 clients receive a reset for the truncated provider body.
		logger.LogError(c.Request.Context(), "failed to deliver provider response body: "+common.RedactSensitiveText(err.Error()))
		panic(http.ErrAbortHandler)
	}

	return nil
}

func replayProviderRawResponse(c *gin.Context, apiErr *types.OpenAIErrorWithStatusCode, policy providerresponse.Policy) bool {
	if c == nil || apiErr == nil || !apiErr.ReplayRawResponse {
		return false
	}
	response := &http.Response{
		StatusCode: apiErr.StatusCode,
		Header:     apiErr.ResponseHeaders.Clone(),
		Body:       io.NopCloser(bytes.NewReader(apiErr.RawBody)),
	}
	_ = responseMultipart(c, response, policy)
	c.Abort()
	return true
}

func providerResponsePolicyForChannel(channel *model.Channel, operation providersBase.Operation, bodyUnmodified bool) providerresponse.Policy {
	dataPath := providersBase.DataPathCrossProtocol
	if resolved, ok := providers.ResolveAdapterSupport(channel).DataPath(operation); ok {
		dataPath = resolved
	}
	return providerresponse.Policy{
		Operation:      operation,
		DataPath:       dataPath,
		BodyUnmodified: bodyUnmodified,
	}
}

func responseCustom(c *gin.Context, response *types.AudioResponseWrapper, operation providerresponse.Operation) *types.OpenAIErrorWithStatusCode {
	headers := make(http.Header, len(response.Headers))
	for name, value := range response.Headers {
		headers.Set(name, value)
	}
	for name, values := range providerresponse.Filter(headers, providerresponse.Policy{
		Operation:      operation,
		DataPath:       providerresponse.DataPathCrossProtocol,
		BodyUnmodified: true,
	}) {
		c.Writer.Header()[name] = append([]string(nil), values...)
	}
	c.Writer.WriteHeader(http.StatusOK)

	_, err := c.Writer.Write(response.Body)
	if err != nil {
		return common.ErrorWrapper(err, "write_response_body_failed", http.StatusInternalServerError)
	}

	return nil
}

func shouldRetry(c *gin.Context, apiErr *types.OpenAIErrorWithStatusCode, channelType int) bool {
	if apiErr == nil {
		return false
	}
	if common.ProviderErrorStopsWorkflow(apiErr.OpenAIError) {
		return false
	}
	if requestContextErr(c) != nil {
		return false
	}

	if explicitChannelPinID(c) > 0 {
		return false
	}
	if apiErr.UpstreamNotAttempted {
		return true
	}
	if apiErr.LocalError {
		return false
	}

	// TODO(retry-policy): this status-code gate is a pragmatic interim policy, not
	// the ideal architecture. The better design is a structured provider failure
	// classifier whose disposition drives retry, cooldown, disable, and error
	// presentation decisions; message matching should stay only as a scoped fallback.
	if config.RuntimeRetryStatusCodeIsRetryable(config.GlobalOption.RuntimeSnapshot(), apiErr.StatusCode) {
		return true
	}

	switch apiErr.StatusCode {
	case http.StatusBadRequest:
		return shouldRetryBadRequest(channelType, apiErr)
	}
	return false
}

func shouldRetryBadRequest(channelType int, apiErr *types.OpenAIErrorWithStatusCode) bool {
	if apiErr.ProviderRateLimited {
		return true
	}
	switch channelType {
	case config.ChannelTypeAnthropic:
		return apiErr.ProviderQuotaExhausted || strings.Contains(apiErr.OpenAIError.Message, "Your credit balance is too low")
	case config.ChannelTypeBedrock:
		return strings.Contains(apiErr.OpenAIError.Message, "Operation not allowed")
	case config.ChannelTypeGemini:
		return apiErr.ProviderAuthRejected ||
			(apiErr.OpenAIError.Param == "INVALID_ARGUMENT" && strings.Contains(apiErr.OpenAIError.Message, "API key not valid"))
	default:
		return false
	}
}

func processChannelRelayError(ctx context.Context, channelId int, channelName string, err *types.OpenAIErrorWithStatusCode, channelType int) {
	if controller.ShouldDisableChannel(channelType, err) {
		disabled, disableErr := controller.AutoDisableChannel(channelId, channelName, err.Message, true, ctx)
		if disableErr != nil {
			logger.LogError(ctx, fmt.Sprintf("failed to auto disable channel #%d(%s): %s", channelId, channelName, disableErr.Error()))
			return
		}
		if disabled {
			logger.LogError(ctx, fmt.Sprintf("auto disabled channel #%d(%s): %s", channelId, channelName, err.Message))
		}
	}
}

func processProviderPayloadAPIError(c *gin.Context, channel *model.Channel, payload []byte, source string) {
	if c == nil || len(payload) == 0 {
		return
	}
	apiErr := runtimesession.ProviderAPIErrorFromPayload(payload)
	if apiErr == nil {
		return
	}
	processProviderAPIError(c, channel, apiErr, source)
}

func processProviderAPIError(c *gin.Context, channel *model.Channel, apiErr *types.OpenAIErrorWithStatusCode, source string) {
	if c == nil || apiErr == nil {
		return
	}
	apiErr = providerresponse.SanitizeAPIError(apiErr)
	ctx := context.Background()
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	source = strings.TrimSpace(source)
	if source != "" {
		channelID := 0
		channelName := ""
		if channel != nil {
			channelID = channel.Id
			channelName = channel.Name
		}
		logger.LogError(ctx, fmt.Sprintf("provider api error source=%s channel #%d(%s): status=%d code=%v message=%s", source, channelID, channelName, apiErr.StatusCode, apiErr.Code, apiErr.Message))
	}
	observeRelayProviderFailure(c, channel, apiErr)
}

func FilterOpenAIErr(c *gin.Context, err *types.OpenAIErrorWithStatusCode) (errWithStatusCode types.OpenAIErrorWithStatusCode) {
	return surface.NormalizeOpenAIError(c, err)
}

func relayResponseWithOpenAIErr(c *gin.Context, err *types.OpenAIErrorWithStatusCode) {
	surfaceErr := surface.FromOpenAIError(err)
	surface.LogLocalError(c, surfaceErr)
	surface.OpenAIContract().RenderJSONError(c, surfaceErr)
}

// mergeCustomParamsForPreMapping applies custom parameter logic similar to OpenAI provider
// 专门用于 pre-mapping 阶段，跳过 pre_add 检查
func mergeCustomParamsForPreMapping(requestMap map[string]interface{}, customParams map[string]interface{}, modelName string) map[string]interface{} {
	return providersBase.ApplyCustomParams(requestMap, customParams, modelName, true)
}
