package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/providerresponse"
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
	if err := finalizeSelectedProviderRequest(relay); err != nil {
		relay.HandleJsonError(wrapRelaySetupError(relay, "provider_request", err, "one_hub_error", http.StatusServiceUnavailable))
		return
	}

	heartbeat := relay.SetHeartbeat(relay.IsStream())
	if heartbeat != nil {
		defer heartbeat.Close()
	}

	apiErr := executeRelayAttempts(relay)
	if apiErr != nil {
		if c.GetBool(streamErrorAlreadyRenderedContextKey) {
			return
		}
		if heartbeat != nil && heartbeat.IsSafeWriteStream() {
			relay.HandleStreamError(apiErr)
			return
		}

		relay.HandleJsonError(apiErr)
	}
}

func wrapRelaySetupError(relay RelayBaseInterface, stage string, err error, defaultCode string, statusCode int) *types.OpenAIErrorWithStatusCode {
	if wrapped := capabilityGateAPIError(err); wrapped != nil {
		return wrapped
	}
	if wrapped := invalidChannelRuntimeConfigAPIError(err); wrapped != nil {
		return wrapped
	}
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
	apiErr = sanitizeRelayAttemptError(apiErr)
	if apiErr == nil {
		metrics.RecordProvider(c, 200)
		return nil
	}
	if handledErr, handled := handleResponsesContinuationMiss(relay, apiErr); handled {
		metrics.RecordProvider(c, apiErr.StatusCode)
		return handledErr
	}

	channel := relay.getProvider().GetChannel()
	observeRelayProviderFailure(c, channel, apiErr)

	options := config.GlobalOption.RuntimeSnapshot()
	retryTimes := options.Int("RetryTimes", config.RetryTimes)
	if done || !relayAttemptShouldRetry(relay, apiErr, channel.Type) || relayShouldSkipRetryAfterAffinityFailure(relay) {
		logger.LogError(c.Request.Context(), fmt.Sprintf("relay error happen, status code is %d, won't retry in this case", apiErr.StatusCode))
		retryTimes = 0
	}

	startTime := c.GetTime("requestStartTime")
	timeout := time.Duration(options.Int("RetryTimeOut", config.RetryTimeOut)) * time.Second

	for i := retryTimes; i > 0; i-- {
		shouldCooldownsFunc(c, channel, apiErr)

		if timeout <= 0 || time.Since(startTime) > timeout {
			break
		}

		if err := relay.setProvider(relay.getOriginalModel()); err != nil {
			apiErr = wrapRelaySetupError(relay, "provider", err, "one_hub_error", http.StatusServiceUnavailable)
			break
		}
		if err := finalizeSelectedProviderRequest(relay); err != nil {
			apiErr = wrapRelaySetupError(relay, "provider_request", err, "one_hub_error", http.StatusServiceUnavailable)
			break
		}

		channel = relay.getProvider().GetChannel()
		logger.LogError(c.Request.Context(), fmt.Sprintf("using channel #%d(%s) to retry (remain times %d)", channel.Id, channel.Name, i))
		apiErr, done = relayHandlerFunc(relay)
		apiErr = sanitizeRelayAttemptError(apiErr)
		if apiErr == nil {
			metrics.RecordProvider(c, 200)
			return nil
		}
		if handledErr, handled := handleResponsesContinuationMiss(relay, apiErr); handled {
			metrics.RecordProvider(c, apiErr.StatusCode)
			return handledErr
		}
		observeRelayProviderFailure(c, channel, apiErr)
		if done || !relayAttemptShouldRetry(relay, apiErr, channel.Type) || relayShouldSkipRetryAfterAffinityFailure(relay) {
			break
		}
	}

	return apiErr
}

func sanitizeRelayAttemptError(apiErr *types.OpenAIErrorWithStatusCode) *types.OpenAIErrorWithStatusCode {
	if apiErr == nil || apiErr.LocalError {
		return apiErr
	}
	return providerresponse.SanitizeAPIError(apiErr)
}

func relayAttemptShouldRetry(relay RelayBaseInterface, apiErr *types.OpenAIErrorWithStatusCode, channelType int) bool {
	if apiErr != nil && (apiErr.UpstreamAccepted || apiErr.UpstreamAmbiguous) && !relayAllowsSideEffectFreeObservationRetry(relay) {
		return false
	}
	return shouldRetryFunc(relay.getContext(), apiErr, channelType)
}

func relayAllowsSideEffectFreeObservationRetry(relay RelayBaseInterface) bool {
	policy, ok := relay.(interface{ allowsSideEffectFreeObservationRetry() bool })
	return ok && policy.allowsSideEffectFreeObservationRetry()
}

// observeRelayProviderFailure projects provider health once per attempt. It
// affects future selection without granting the current submission a replay.
func observeRelayProviderFailure(c *gin.Context, channel *model.Channel, apiErr *types.OpenAIErrorWithStatusCode) {
	if c == nil || apiErr == nil {
		return
	}
	if apiErr.LocalError {
		return
	}
	metrics.RecordProvider(c, apiErr.StatusCode)
	if channel == nil {
		return
	}
	if apiErr.StatusCode == http.StatusTooManyRequests {
		model.ChannelGroup.SetCooldowns(channel.Id, c.GetString("new_model"))
	}
	requestCtx := context.Background()
	if c.Request != nil {
		requestCtx = context.WithoutCancel(c.Request.Context())
	}
	healthCtx, cancel := context.WithTimeout(requestCtx, 5*time.Second)
	process := processChannelRelayErrorFunc
	go func() {
		defer cancel()
		process(healthCtx, channel.Id, channel.Name, apiErr, channel.Type)
	}()
}

func relayShouldSkipRetryAfterAffinityFailure(relay RelayBaseInterface) bool {
	if responsesRelay, ok := relay.(*relayResponses); ok && responsesRelay.strictOwnerRoute {
		return true
	}
	return shouldSkipRetryAfterAffinityFailure(relay.getContext())
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
	return apiErr, true
}

func validateSelectedProviderRequest(relay RelayBaseInterface) error {
	validator, ok := relay.(interface{ validateSelectedProviderRequest() error })
	if !ok {
		return nil
	}
	err := validator.validateSelectedProviderRequest()
	if err == nil {
		return nil
	}
	return &selectedProviderRepresentabilityError{err: err}
}

type selectedProviderRepresentabilityError struct {
	err error
}

func (e *selectedProviderRepresentabilityError) Error() string {
	if e == nil || e.err == nil {
		return "selected provider cannot represent the request"
	}
	return e.err.Error()
}

func (e *selectedProviderRepresentabilityError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func RelayHandler(relay RelayBaseInterface) (err *types.OpenAIErrorWithStatusCode, done bool) {
	if noQuota, ok := relay.(interface{ skipQuotaSettlement() bool }); ok && noQuota.skipQuotaSettlement() {
		return relay.send()
	}
	promptTokens, tonkeErr := relay.getPromptTokens()
	if tonkeErr != nil {
		err = common.ErrorWrapperLocal(tonkeErr, "token_error", http.StatusBadRequest)
		done = true
		return
	}

	usage := &types.Usage{PromptTokens: promptTokens}

	relay.getProvider().SetUsage(usage)

	protocol := relay_util.LogProtocolHTTP
	if relay.IsStream() {
		protocol = relay_util.LogProtocolHTTPStream
	}
	quota, billingErr := relay_util.NewAttemptQuota(relay.getContext(), relay.getModelName(), int64(promptTokens), relay_util.BillingAttemptSpec{
		LogProtocol: protocol,
	})
	if billingErr != nil {
		err = common.ErrorWrapperLocal(billingErr, "billing_admission_failed", http.StatusServiceUnavailable)
		done = true
		return
	}
	requestCtx := relay.getContext().Request.Context()
	if billingErr = quota.ApplyReserve(requestCtx); billingErr != nil {
		err = relay_util.BillingAPIError(billingErr, "billing_reserve_failed", http.StatusServiceUnavailable)
		done = true
		return
	}
	if billingErr = quota.ClaimSubmission(); billingErr != nil {
		_, _ = quota.CloseWithoutSubmission(requestCtx)
		err = common.ErrorWrapperLocal(billingErr, "billing_submission_claim_failed", http.StatusInternalServerError)
		done = true
		return
	}
	// A claimed submission is never replayed by the outer channel loop.
	done = true
	defer func() {
		if recovered := recover(); recovered != nil {
			_, _ = quota.CloseFromProviderResult(requestCtx, usage, relay.IsStream())
			panic(recovered)
		}
	}()

	err, _ = relay.send()
	if quota.Quota() != nil {
		quota.Quota().SetFirstResponseTime(relay.GetFirstResponseTime())
	}
	_, billingErr = quota.CloseFromProviderResult(requestCtx, usage, relay.IsStream())
	if billingErr != nil {
		logger.LogError(requestCtx, "billing final balance action failed after provider submission: "+billingErr.Error())
	}

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
	if trimmedTools != "" && trimmedTools != "null" {
		var tools []json.RawMessage
		if err := json.Unmarshal(state.Tools, &tools); err != nil {
			state.SkipOnlyChat = true
		} else {
			state.SkipOnlyChat = len(tools) > 0
		}
	}
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

func materializeSelectedProviderRequest(relay RelayBaseInterface) error {
	c := relay.getContext()
	if !common.GetRequestBodyReparseNeeded(c) {
		return nil
	}

	common.SetRequestBodyReparseNeeded(c, false)
	materializer, ok := relay.(interface{ materializeSelectedProviderRequest() error })
	if !ok {
		return errors.New("selected provider request transform cannot be materialized")
	}
	if err := materializer.materializeSelectedProviderRequest(); err != nil {
		return err
	}
	c.Set("is_stream", relay.IsStream())
	return nil
}

func finalizeSelectedProviderRequest(relay RelayBaseInterface) error {
	if err := materializeSelectedProviderRequest(relay); err != nil {
		return err
	}
	if relay == nil || relay.getProvider() == nil || relay.getProvider().GetChannel() == nil {
		return errors.New("selected provider is unavailable")
	}
	channel := relay.getProvider().GetChannel()
	if relay.getContext().GetBool("skip_only_chat") && channel.OnlyChat {
		return errors.New("selected channel cannot represent requests with tools")
	}
	if relay.IsStream() && !channel.AllowStream(relay.getOriginalModel()) {
		return errors.New("selected channel does not support streaming")
	}
	if capability := currentRequestChannelCapability(relay.getContext()); capability != nil {
		if err := capability(channel); err != nil {
			return err
		}
	}
	if err := validateSelectedProviderRequest(relay); err != nil {
		return err
	}
	return prepareSelectedProviderRemoteMedia(relay)
}
