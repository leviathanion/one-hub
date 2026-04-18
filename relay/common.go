package relay

import (
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
	"one-api/common/requester"
	"one-api/common/surface"
	"one-api/common/utils"
	"one-api/controller"
	"one-api/metrics"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/relay/relay_util"
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
	// 判断modelName是否在token的setting.limits.LimitModelSetting.models[]范围内

	// 从context中获取token设置
	tokenSetting, exists := c.Get("token_setting")
	if !exists {
		// 如果没有token设置，则不进行限制
		return nil
	}

	// 类型断言为TokenSetting指针
	setting, ok := tokenSetting.(*model.TokenSetting)
	if !ok || setting == nil {
		// 类型断言失败或为空，不进行限制
		return nil
	}

	// 检查是否启用了模型限制
	if !setting.Limits.LimitModelSetting.Enabled {
		// 未启用模型限制，允许所有模型
		return nil
	}

	// 检查模型列表是否为空
	if len(setting.Limits.LimitModelSetting.Models) == 0 {
		// Empty model list means no models are allowed
		return errors.New("No available models configured for current token")
	}

	// Check if modelName is in the allowed models list
	for _, allowedModel := range setting.Limits.LimitModelSetting.Models {
		if allowedModel == modelName {
			// Found matching model, allow usage
			return nil
		}
	}

	// modelName is not in the allowed models list
	return fmt.Errorf("Model %s is not supported for current token", modelName)
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

func prepareProviderForChannel(c *gin.Context, modelName string, channel *model.Channel) (provider providersBase.ProviderInterface, newModelName string, fail error) {
	if channel == nil {
		fail = errors.New("channel not found")
		return
	}

	c.Set("channel_id", channel.Id)
	c.Set("channel_type", channel.Type)

	provider = providers.GetProvider(channel, c)
	if provider == nil {
		fail = errors.New("channel not found")
		return
	}
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

type cachedProviderSelection struct {
	provider        providersBase.ProviderInterface
	originalModel   string
	newModelName    string
	channelID       int
	channelType     int
	billingOriginal bool
	skipOnlyChat    bool
	isStream        bool
}

func cacheProviderSelection(c *gin.Context, originalModel string, provider providersBase.ProviderInterface, newModelName string) {
	billingOriginalModel := c.GetBool("billing_original_model")
	selection := &cachedProviderSelection{
		provider:        provider,
		originalModel:   originalModel,
		newModelName:    newModelName,
		channelID:       c.GetInt("channel_id"),
		channelType:     c.GetInt("channel_type"),
		billingOriginal: billingOriginalModel,
		skipOnlyChat:    c.GetBool("skip_only_chat"),
		isStream:        c.GetBool("is_stream"),
	}
	c.Set(config.GinProviderCacheKey, selection)
}

func consumeCachedProviderSelection(c *gin.Context, originalModel string) (providersBase.ProviderInterface, string, bool) {
	cached, exists := c.Get(config.GinProviderCacheKey)
	if !exists || cached == nil {
		return nil, "", false
	}

	selection, ok := cached.(*cachedProviderSelection)
	if !ok || selection == nil || selection.provider == nil || selection.originalModel != originalModel {
		c.Set(config.GinProviderCacheKey, nil)
		return nil, "", false
	}

	if selection.skipOnlyChat != c.GetBool("skip_only_chat") || selection.isStream != c.GetBool("is_stream") {
		c.Set(config.GinProviderCacheKey, nil)
		return nil, "", false
	}

	// Keep this restore list in sync with every provider-selection context write in GetProvider.
	// Cache hits must restore the full selection context: channel_id, channel_type,
	// original_model, new_model, and billing_original_model.
	c.Set(config.GinProviderCacheKey, nil)
	c.Set("channel_id", selection.channelID)
	c.Set("channel_type", selection.channelType)
	c.Set("original_model", selection.originalModel)
	c.Set("new_model", selection.newModelName)
	c.Set("billing_original_model", selection.billingOriginal)
	return selection.provider, selection.newModelName, true
}

func fetchChannel(c *gin.Context, modelName string) (channel *model.Channel, fail error) {
	channelId := explicitChannelPinID(c)
	if channelId > 0 {
		channel, err := fetchChannelById(channelId)
		if err != nil {
			return nil, err
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

func preferredChannelWaitBudget() time.Duration {
	if config.PreferredChannelWaitMilliseconds <= 0 {
		return 0
	}
	return time.Duration(config.PreferredChannelWaitMilliseconds) * time.Millisecond
}

func preferredChannelWaitPollInterval() time.Duration {
	if config.PreferredChannelWaitPollMilliseconds <= 0 {
		return 50 * time.Millisecond
	}
	return time.Duration(config.PreferredChannelWaitPollMilliseconds) * time.Millisecond
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

	budget := preferredChannelWaitBudget()
	if budget <= 0 {
		return nil
	}

	eligible, err := model.ChannelGroup.PreferredChannelEligible(group, modelName, selection.preferredChannelID, filters...)
	if err != nil || !eligible || !model.ChannelGroup.IsInCooldown(selection.preferredChannelID, modelName) {
		return nil
	}

	pollInterval := preferredChannelWaitPollInterval()
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

	var filters []model.ChannelsFilterFunc
	if skipOnlyChat {
		filters = append(filters, model.FilterOnlyChat())
	}

	if len(selection.skipChannelIDs) > 0 {
		filters = append(filters, model.FilterChannelId(selection.skipChannelIDs))
	}

	if len(selection.allowChannelTypes) > 0 {
		filters = append(filters, model.FilterChannelTypes(selection.allowChannelTypes))
	}

	if isStream {
		filters = append(filters, model.FilterDisabledStream(modelName))
	}

	// 使用统一的分组管理器
	groupManager := NewGroupManager(c)
	return groupManager.TryWithGroups(modelName, filters, func(group string) (*model.Channel, error) {
		if err := waitForPreferredChannelCooldown(c, group, modelName, selection, filters); err != nil {
			return nil, err
		}
		channel, err := model.ChannelGroup.NextWithPreferred(group, modelName, selection.preferredChannelID, selection.ignorePreferredCooldown, filters...)
		if err != nil {
			return nil, err
		}
		if selection.preferredChannelID > 0 && (channel == nil || channel.Id != selection.preferredChannelID) {
			clearCurrentChannelAffinity(c)
			if selection.strictPreferredChannel {
				return nil, errors.New("preferred affinity channel is unavailable")
			}
		}
		setChannelAffinitySelectedPreferred(c, channel != nil && selection.preferredChannelID > 0 && channel.Id == selection.preferredChannelID)
		return channel, nil
	})

}

func responseJsonClient(c *gin.Context, data interface{}) *types.OpenAIErrorWithStatusCode {
	// 将data转换为 JSON
	responseBody, err := json.Marshal(data)
	if err != nil {
		logger.LogError(c.Request.Context(), "marshal_response_body_failed:"+err.Error())
		return nil
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	_, err = c.Writer.Write(responseBody)
	if err != nil {
		logger.LogError(c.Request.Context(), "write_response_body_failed:"+err.Error())
	}

	return nil
}

type StreamEndHandler func() string

func responseStreamClient(c *gin.Context, stream requester.StreamReaderInterface[string], endHandler StreamEndHandler) (firstResponseTime time.Time, errWithOP *types.OpenAIErrorWithStatusCode) {
	requester.SetEventStreamHeaders(c)
	dataChan, errChan := stream.Recv()
	var finalErr *types.OpenAIErrorWithStatusCode

	defer stream.Close()
	streamWriter := relay_util.NewBufferedStreamWriter(c.Writer, 0)
	defer streamWriter.Close()

	var isFirstResponse bool

	for {
		select {
		case data, ok := <-dataChan:
			if !ok {
				return firstResponseTime, nil
			}

			streamData := "data: " + data + "\n\n"
			if !isFirstResponse {
				firstResponseTime = time.Now()
				isFirstResponse = true
			}

			select {
			case <-c.Request.Context().Done():
			default:
				_, _ = streamWriter.WriteString(streamData)
			}
		case err := <-errChan:
			if !errors.Is(err, io.EOF) {
				errPayload := map[string]any{
					"error": map[string]any{
						"message": err.Error(),
						"type":    "stream_error",
						"code":    "stream_error",
					},
				}
				errJSON, _ := json.Marshal(errPayload)
				errMsg := "data: " + string(errJSON) + "\n\n"
				select {
				case <-c.Request.Context().Done():
				default:
					_, _ = streamWriter.WriteString(errMsg)
				}

				finalErr = common.StringErrorWrapper(err.Error(), "stream_error", 900)
				logger.LogError(c.Request.Context(), "Stream err:"+err.Error())
			} else {
				if finalErr == nil && endHandler != nil {
					streamData := endHandler()
					if streamData != "" {
						select {
						case <-c.Request.Context().Done():
						default:
							_, _ = streamWriter.WriteString("data: " + streamData + "\n\n")
						}
					}
				}

				select {
				case <-c.Request.Context().Done():
				default:
					_, _ = streamWriter.WriteString("data: [DONE]\n\n")
				}
			}
			return firstResponseTime, nil
		}
	}
}

func responseGeneralStreamClient(c *gin.Context, stream requester.StreamReaderInterface[string], endHandler StreamEndHandler) (firstResponseTime time.Time) {
	return responseGeneralStreamClientWithObserver(c, stream, endHandler, nil)
}

func responseGeneralStreamClientWithObserver(c *gin.Context, stream requester.StreamReaderInterface[string], endHandler StreamEndHandler, observer func(string)) (firstResponseTime time.Time) {
	requester.SetEventStreamHeaders(c)
	dataChan, errChan := stream.Recv()

	defer stream.Close()
	streamWriter := relay_util.NewBufferedStreamWriter(c.Writer, 0)
	defer streamWriter.Close()
	var isFirstResponse bool

	for {
		select {
		case data, ok := <-dataChan:
			if !ok {
				return firstResponseTime
			}
			if !isFirstResponse {
				firstResponseTime = time.Now()
				isFirstResponse = true
			}
			if observer != nil {
				observer(data)
			}
			select {
			case <-c.Request.Context().Done():
			default:
				_, _ = streamWriter.WriteString(data)
			}
			continue
		default:
		}

		select {
		case data, ok := <-dataChan:
			if !ok {
				return firstResponseTime
			}
			if !isFirstResponse {
				firstResponseTime = time.Now()
				isFirstResponse = true
			}
			if observer != nil {
				observer(data)
			}
			select {
			case <-c.Request.Context().Done():
			default:
				_, _ = streamWriter.WriteString(data)
			}
		case err := <-errChan:
			if !errors.Is(err, io.EOF) {
				errPayload := map[string]any{
					"type":    "error",
					"code":    "stream_error",
					"message": err.Error(),
				}
				errJSON, _ := json.Marshal(errPayload)
				errEvent := "event: error\ndata: " + string(errJSON) + "\n\n"
				select {
				case <-c.Request.Context().Done():
				default:
					_, _ = streamWriter.WriteString(errEvent)
				}

				logger.LogError(c.Request.Context(), "Stream err:"+err.Error())
			} else if endHandler != nil {
				streamData := endHandler()
				if streamData != "" {
					if observer != nil {
						observer(streamData)
					}
					select {
					case <-c.Request.Context().Done():
					default:
						_, _ = streamWriter.WriteString(streamData)
					}
				}
			}
			return firstResponseTime
		}
	}
}

func responseMultipart(c *gin.Context, resp *http.Response) *types.OpenAIErrorWithStatusCode {
	defer resp.Body.Close()

	for k, v := range resp.Header {
		c.Writer.Header().Set(k, v[0])
	}

	c.Writer.WriteHeader(resp.StatusCode)

	_, err := io.Copy(c.Writer, resp.Body)
	if err != nil {
		return common.ErrorWrapper(err, "write_response_body_failed", http.StatusInternalServerError)
	}

	return nil
}

func responseCustom(c *gin.Context, response *types.AudioResponseWrapper) *types.OpenAIErrorWithStatusCode {
	for k, v := range response.Headers {
		c.Writer.Header().Set(k, v)
	}
	c.Writer.WriteHeader(http.StatusOK)

	_, err := c.Writer.Write(response.Body)
	if err != nil {
		return common.ErrorWrapper(err, "write_response_body_failed", http.StatusInternalServerError)
	}

	return nil
}

func responseCache(c *gin.Context, response string, isStream bool) {
	if isStream {
		requester.SetEventStreamHeaders(c)
		c.Stream(func(w io.Writer) bool {
			fmt.Fprint(w, response)
			return false
		})
	} else {
		c.Data(http.StatusOK, "application/json", []byte(response))
	}

}

func shouldRetry(c *gin.Context, apiErr *types.OpenAIErrorWithStatusCode, channelType int) bool {
	if apiErr == nil {
		return false
	}

	metrics.RecordProvider(c, apiErr.StatusCode)

	if apiErr.LocalError || explicitChannelPinID(c) > 0 {
		return false
	}

	switch apiErr.StatusCode {
	case http.StatusTooManyRequests, http.StatusTemporaryRedirect:
		return true
	case http.StatusRequestTimeout, http.StatusGatewayTimeout, 524:
		return false
	case http.StatusBadRequest:
		return shouldRetryBadRequest(channelType, apiErr)
	}

	if apiErr.StatusCode/100 == 5 {
		return true
	}

	if apiErr.StatusCode/100 == 2 {
		return false
	}
	return true
}

func shouldRetryBadRequest(channelType int, apiErr *types.OpenAIErrorWithStatusCode) bool {
	switch channelType {
	case config.ChannelTypeAnthropic:
		return strings.Contains(apiErr.OpenAIError.Message, "Your credit balance is too low")
	case config.ChannelTypeBedrock:
		return strings.Contains(apiErr.OpenAIError.Message, "Operation not allowed")
	default:
		// gemini
		if apiErr.OpenAIError.Param == "INVALID_ARGUMENT" && strings.Contains(apiErr.OpenAIError.Message, "API key not valid") {
			return true
		}
		return false
	}
}

func processChannelRelayError(ctx context.Context, channelId int, channelName string, err *types.OpenAIErrorWithStatusCode, channelType int) {
	logger.LogError(ctx, fmt.Sprintf("relay error (channel #%d(%s)): %s", channelId, channelName, err.Message))
	if controller.ShouldDisableChannel(channelType, err) {
		if _, disableErr := controller.AutoDisableChannel(channelId, channelName, err.Message, true); disableErr != nil {
			logger.LogError(ctx, fmt.Sprintf("failed to auto disable channel #%d(%s): %s", channelId, channelName, disableErr.Error()))
		}
	}
}

func FilterOpenAIErr(c *gin.Context, err *types.OpenAIErrorWithStatusCode) (errWithStatusCode types.OpenAIErrorWithStatusCode) {
	return surface.NormalizeOpenAIError(c, err)
}

func relayResponseWithOpenAIErr(c *gin.Context, err *types.OpenAIErrorWithStatusCode) {
	surfaceErr := surface.FromOpenAIError(err)
	surface.LogLocalError(c, surfaceErr)
	surface.OpenAIContract().RenderJSONError(c, surfaceErr)
}

func relayRerankResponseWithErr(c *gin.Context, err *types.OpenAIErrorWithStatusCode) {
	surfaceErr := surface.FromOpenAIError(err)
	surface.LogLocalError(c, surfaceErr)
	surface.RerankContract().RenderJSONError(c, surfaceErr)
}

// mergeCustomParamsForPreMapping applies custom parameter logic similar to OpenAI provider
// 专门用于 pre-mapping 阶段，跳过 pre_add 检查
func mergeCustomParamsForPreMapping(requestMap map[string]interface{}, customParams map[string]interface{}, modelName string) map[string]interface{} {
	return providersBase.ApplyCustomParams(requestMap, customParams, modelName, true)
}
