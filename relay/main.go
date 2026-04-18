package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/utils"
	"one-api/metrics"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/relay/relay_util"
	"one-api/types"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	relayHandlerFunc             = RelayHandler
	processChannelRelayErrorFunc = processChannelRelayError
	shouldRetryFunc              = shouldRetry
	shouldCooldownsFunc          = shouldCooldowns
)

func Relay(c *gin.Context) {
	relay := Path2Relay(c, c.Request.URL.Path)
	if relay == nil {
		common.AbortWithMessage(c, http.StatusNotFound, "Not Found")
		return
	}

	// Apply pre-mapping before setRequest to ensure request body modifications take effect
	applyPreMappingBeforeRequest(c)

	if err := relay.setRequest(); err != nil {
		openaiErr := wrapRelaySetupError(relay, "request", err, "one_hub_error", http.StatusBadRequest)
		relay.HandleJsonError(openaiErr)
		return
	}

	c.Set("is_stream", relay.IsStream())
	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		openaiErr := wrapRelaySetupError(relay, "provider", err, "one_hub_error", http.StatusServiceUnavailable)
		relay.HandleJsonError(openaiErr)
		return
	}
	if err := reparseRequestAfterProviderSelection(relay); err != nil {
		openaiErr := wrapRelaySetupError(relay, "reparse", err, "one_hub_error", http.StatusBadRequest)
		relay.HandleJsonError(openaiErr)
		return
	}

	heartbeat := relay.SetHeartbeat(relay.IsStream())
	if heartbeat != nil {
		defer heartbeat.Close()
	}

	apiErr := executeRelayAttempts(relay)
	if apiErr != nil {
		if heartbeat != nil && heartbeat.IsSafeWriteStream() {
			relay.HandleStreamError(apiErr)
			return
		}

		relay.HandleJsonError(apiErr)
	}
}

func wrapRelaySetupError(relay RelayBaseInterface, stage string, err error, defaultCode string, statusCode int) *types.OpenAIErrorWithStatusCode {
	if wrapper, ok := relay.(relaySetupErrorWrapper); ok {
		if wrapped := wrapper.WrapSetupError(stage, err); wrapped != nil {
			return wrapped
		}
	}
	return common.StringErrorWrapperLocal(err.Error(), defaultCode, statusCode)
}

func executeRelayAttempts(relay RelayBaseInterface) *types.OpenAIErrorWithStatusCode {
	c := relay.getContext()

	apiErr, done := relayHandlerFunc(relay)
	if apiErr == nil {
		metrics.RecordProvider(c, 200)
		return nil
	}
	if handledErr, handled := handleResponsesContinuationMiss(relay, apiErr); handled {
		metrics.RecordProvider(c, apiErr.StatusCode)
		return handledErr
	}

	channel := relay.getProvider().GetChannel()
	go processChannelRelayErrorFunc(c.Request.Context(), channel.Id, channel.Name, apiErr, channel.Type)

	retryTimes := config.RetryTimes
	if done || !shouldRetryFunc(c, apiErr, channel.Type) || shouldSkipRetryAfterAffinityFailure(c) {
		logger.LogError(c.Request.Context(), fmt.Sprintf("relay error happen, status code is %d, won't retry in this case", apiErr.StatusCode))
		retryTimes = 0
	}

	startTime := c.GetTime("requestStartTime")
	timeout := time.Duration(config.RetryTimeOut) * time.Second

	for i := retryTimes; i > 0; i-- {
		shouldCooldownsFunc(c, channel, apiErr)

		if time.Since(startTime) > timeout {
			apiErr = common.StringErrorWrapperLocal("重试超时，上游负载已饱和，请稍后再试", "system_error", http.StatusTooManyRequests)
			break
		}

		if err := relay.setProvider(relay.getOriginalModel()); err != nil {
			break
		}
		if err := reparseRequestAfterProviderSelection(relay); err != nil {
			apiErr = common.StringErrorWrapperLocal(err.Error(), "one_hub_error", http.StatusBadRequest)
			done = true
			break
		}

		channel = relay.getProvider().GetChannel()
		logger.LogError(c.Request.Context(), fmt.Sprintf("using channel #%d(%s) to retry (remain times %d)", channel.Id, channel.Name, i))
		apiErr, done = relayHandlerFunc(relay)
		if apiErr == nil {
			metrics.RecordProvider(c, 200)
			return nil
		}
		if handledErr, handled := handleResponsesContinuationMiss(relay, apiErr); handled {
			metrics.RecordProvider(c, apiErr.StatusCode)
			return handledErr
		}
		go processChannelRelayErrorFunc(c.Request.Context(), channel.Id, channel.Name, apiErr, channel.Type)
		if done || !shouldRetryFunc(c, apiErr, channel.Type) || shouldSkipRetryAfterAffinityFailure(c) {
			break
		}
	}

	return apiErr
}

func handleResponsesContinuationMiss(relay RelayBaseInterface, apiErr *types.OpenAIErrorWithStatusCode) (*types.OpenAIErrorWithStatusCode, bool) {
	responsesRelay, ok := relay.(*relayResponses)
	if !ok {
		return apiErr, false
	}

	plan := responsesRelay.stalePreviousResponseHandlingPlan(apiErr)
	if plan == nil {
		return apiErr, false
	}

	responsesRelay.clearStalePreviousResponseAffinity()
	mergeChannelAffinityMeta(responsesRelay.getContext(), plan.recoveryCandidateMeta)
	return plan.clientError, true
}

func RelayHandler(relay RelayBaseInterface) (err *types.OpenAIErrorWithStatusCode, done bool) {
	promptTokens, tonkeErr := relay.getPromptTokens()
	if tonkeErr != nil {
		err = common.ErrorWrapperLocal(tonkeErr, "token_error", http.StatusBadRequest)
		done = true
		return
	}

	usage := &types.Usage{
		PromptTokens: promptTokens,
	}

	relay.getProvider().SetUsage(usage)

	quota := relay_util.NewQuota(relay.getContext(), relay.getModelName(), promptTokens)
	if err = quota.PreQuotaConsumption(); err != nil {
		done = true
		return
	}

	err, done = relay.send()
	// 最后处理流式中断时计算tokens
	if usage.CompletionTokens == 0 && usage.TextBuilder.Len() > 0 {
		usage.CompletionTokens = common.CountTokenText(usage.TextBuilder.String(), relay.getModelName())
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	if err != nil {
		quota.Undo(relay.getContext())
		return
	}

	quota.SetFirstResponseTime(relay.GetFirstResponseTime())

	quota.Consume(relay.getContext(), usage, relay.IsStream())

	return
}

func shouldCooldowns(c *gin.Context, channel *model.Channel, apiErr *types.OpenAIErrorWithStatusCode) {
	modelName := c.GetString("new_model")
	channelId := channel.Id

	// 如果是频率限制，冻结通道
	if apiErr.StatusCode == http.StatusTooManyRequests {
		model.ChannelGroup.SetCooldowns(channelId, modelName)
	}

	skipChannelIds, ok := utils.GetGinValue[[]int](c, "skip_channel_ids")
	if !ok {
		skipChannelIds = make([]int, 0)
	}

	skipChannelIds = append(skipChannelIds, channelId)

	c.Set("skip_channel_ids", skipChannelIds)
}

type preMappingRequestState struct {
	Model        string          `json:"model"`
	IsStream     bool            `json:"stream"`
	Tools        json.RawMessage `json:"tools"`
	SkipOnlyChat bool            `json:"-"`
}

func shouldApplyPreMapping(path string) bool {
	return strings.HasPrefix(path, "/v1/chat/completions") || strings.HasPrefix(path, "/v1/completions")
}

func parsePreMappingRequestState(bodyBytes []byte) (preMappingRequestState, error) {
	var state preMappingRequestState
	if err := json.Unmarshal(bodyBytes, &state); err != nil {
		return state, err
	}

	trimmedTools := strings.TrimSpace(string(state.Tools))
	state.SkipOnlyChat = trimmedTools != "" && trimmedTools != "null"
	return state, nil
}

func updatePreMappingSelectionContext(c *gin.Context, bodyBytes []byte) (preMappingRequestState, error) {
	state, err := parsePreMappingRequestState(bodyBytes)
	if err != nil {
		return state, err
	}

	c.Set("is_stream", state.IsStream)
	c.Set("skip_only_chat", state.SkipOnlyChat)
	return state, nil
}

func applyPreMappingForProvider(c *gin.Context, modelName string, provider providersBase.ProviderInterface) (bool, error) {
	if c == nil || provider == nil || !shouldApplyPreMapping(c.Request.URL.Path) {
		return false, nil
	}

	currentBodyBytes, err := common.CacheRequestBody(c)
	if err != nil {
		return false, err
	}

	originalBodyBytes, ok := common.GetOriginalRequestBody(c)
	if !ok {
		originalBodyBytes = currentBodyBytes
	}

	finalBodyBytes := originalBodyBytes
	var finalRequestMap map[string]interface{}

	customParams, err := provider.CustomParameterHandler()
	if err == nil && customParams != nil {
		if preAdd, exists := customParams["pre_add"]; exists && preAdd == true {
			requestMap := make(map[string]interface{})
			if err := json.Unmarshal(originalBodyBytes, &requestMap); err == nil {
				finalRequestMap = mergeCustomParamsForPreMapping(requestMap, customParams, modelName)
				if modifiedBodyBytes, err := json.Marshal(finalRequestMap); err == nil {
					finalBodyBytes = modifiedBodyBytes
				} else {
					finalRequestMap = nil
				}
			}
		}
	}

	if finalRequestMap != nil {
		common.SetReusableRequestBodyMap(c, finalBodyBytes, finalRequestMap)
	} else {
		common.SetReusableRequestBody(c, finalBodyBytes)
	}

	if _, err := updatePreMappingSelectionContext(c, finalBodyBytes); err != nil {
		return false, nil
	}

	bodyChanged := !bytes.Equal(currentBodyBytes, finalBodyBytes)
	common.SetRequestBodyReparseNeeded(c, bodyChanged)
	return bodyChanged, nil
}

func reparseRequestAfterProviderSelection(relay RelayBaseInterface) error {
	c := relay.getContext()
	if !common.GetRequestBodyReparseNeeded(c) {
		return nil
	}

	common.SetRequestBodyReparseNeeded(c, false)
	if err := relay.setRequest(); err != nil {
		return err
	}
	c.Set("is_stream", relay.IsStream())
	return nil
}

// applies pre-mapping before setRequest to ensure modifications take effect
func applyPreMappingBeforeRequest(c *gin.Context) {
	// check if this is a chat completion request that needs pre-mapping
	path := c.Request.URL.Path
	if !shouldApplyPreMapping(path) {
		return
	}

	bodyBytes, err := common.CacheRequestBody(c)
	if err != nil {
		return
	}

	requestState, err := updatePreMappingSelectionContext(c, bodyBytes)
	if err != nil || requestState.Model == "" {
		return
	}

	provider, _, err := GetProvider(c, requestState.Model)
	if err != nil {
		return
	}
	cacheProviderSelection(c, requestState.Model, provider, c.GetString("new_model"))
	_, _ = applyPreMappingForProvider(c, requestState.Model, provider)
	common.SetRequestBodyReparseNeeded(c, false)
}
