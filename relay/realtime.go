package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/utils"
	"one-api/common/wsconn"
	"one-api/metrics"
	"one-api/middleware"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/relay/relay_util"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
)

type RelayModeChatRealtime struct {
	relayBase
	userConn   *wsconn.ManagedConn
	session    runtimerealtime.RealtimeSession
	beforeOpen func() *types.OpenAIErrorWithStatusCode
	models     runtimesession.ModelBinding
	workPolicy runtimesession.RealtimeWorkPolicy
}

func realtimeWebSocketOriginAllowed(r *http.Request) bool {
	return validateRealtimeWebSocketOrigin(r) == nil
}

func validateRealtimeWebSocketOrigin(r *http.Request) *types.OpenAIErrorWithStatusCode {
	if r == nil {
		return common.StringErrorWrapperLocal("websocket request is required", "invalid_request", http.StatusBadRequest)
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	hasCredentialSubprotocol := requestHasOpenAIInsecureAPIKeySubprotocol(r)
	if origin == "" {
		if hasCredentialSubprotocol {
			return common.StringErrorWrapperLocal("origin is required when using insecure websocket API key subprotocol", "realtime_origin_required", http.StatusForbidden)
		}
		return nil
	}
	allowed := configuredRealtimeAllowedOrigins()
	if len(allowed) == 0 {
		if hasCredentialSubprotocol && !viper.GetBool("realtime.unsafe_allow_credential_subprotocol_any_origin") {
			return common.StringErrorWrapperLocal("explicit websocket origin allowlist is required when using insecure websocket API key subprotocol", "realtime_origin_allowlist_required", http.StatusForbidden)
		}
		return nil
	}
	parsedOrigin, err := url.Parse(origin)
	if err != nil || parsedOrigin.Scheme == "" || parsedOrigin.Host == "" {
		return common.StringErrorWrapperLocal("invalid websocket origin", "invalid_origin", http.StatusForbidden)
	}
	if parsedOrigin.Scheme != "http" && parsedOrigin.Scheme != "https" {
		return common.StringErrorWrapperLocal("invalid websocket origin", "invalid_origin", http.StatusForbidden)
	}
	for _, candidate := range allowed {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			if hasCredentialSubprotocol && !viper.GetBool("realtime.unsafe_allow_credential_subprotocol_any_origin") {
				return common.StringErrorWrapperLocal("wildcard origin is not allowed with insecure websocket API key subprotocol", "realtime_origin_not_allowed", http.StatusForbidden)
			}
			return nil
		}
		if strings.EqualFold(candidate, origin) {
			return nil
		}
	}
	return common.StringErrorWrapperLocal("websocket origin is not allowed", "realtime_origin_not_allowed", http.StatusForbidden)
}

func configuredRealtimeAllowedOrigins() []string {
	raw := viper.GetStringSlice("realtime.allowed_origins")
	if len(raw) == 0 {
		if single := strings.TrimSpace(viper.GetString("realtime.allowed_origins")); single != "" {
			raw = strings.Split(single, ",")
		}
	}
	origins := make([]string, 0, len(raw))
	for _, origin := range raw {
		trimmed := strings.TrimSpace(origin)
		if trimmed != "" {
			origins = append(origins, trimmed)
		}
	}
	if len(origins) > 0 {
		return origins
	}
	return middleware.ConfiguredCORSAllowOrigins()
}

func websocketUpgradeResponseHeader(r *http.Request) http.Header {
	selected := selectWebSocketSubprotocol(r)
	if selected == "" {
		return nil
	}
	header := make(http.Header)
	header.Set("Sec-Websocket-Protocol", selected)
	return header
}

func echoableClientWebSocketSubprotocols(r *http.Request) []string {
	selected := selectWebSocketSubprotocol(r)
	if selected == "" {
		return nil
	}
	return []string{selected}
}

func selectWebSocketSubprotocol(r *http.Request) string {
	protocols := allowedClientWebSocketSubprotocols(r)
	if len(protocols) == 0 {
		return ""
	}
	for _, protocol := range protocols {
		if protocol == "realtime" {
			return protocol
		}
	}
	for _, protocol := range protocols {
		if isEchoableWebSocketSubprotocol(protocol) {
			return protocol
		}
	}
	return ""
}

func isEchoableWebSocketSubprotocol(protocol string) bool {
	protocol = strings.TrimSpace(protocol)
	if protocol == "" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(protocol), "openai-insecure-api-key.") {
		return false
	}
	return true
}

func allowedClientWebSocketSubprotocols(r *http.Request) []string {
	if r == nil {
		return nil
	}
	clientProtocols := wsconn.Subprotocols(r)
	allowed := make([]string, 0, len(clientProtocols))
	for _, protocol := range clientProtocols {
		protocol = strings.TrimSpace(protocol)
		lower := strings.ToLower(protocol)
		switch {
		case protocol == "realtime":
			allowed = append(allowed, protocol)
		case protocol == "openai-beta.realtime-v1":
			allowed = append(allowed, protocol)
		case strings.HasPrefix(lower, "openai-insecure-api-key."):
			allowed = append(allowed, protocol)
		}
	}
	return allowed
}

func requestHasOpenAIInsecureAPIKeySubprotocol(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, protocol := range wsconn.Subprotocols(r) {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(protocol)), "openai-insecure-api-key.") {
			return true
		}
	}
	return false
}

func ChatRealtime(c *gin.Context) {
	modelName := c.Query("model")
	if modelName == "" {
		common.AbortWithMessage(c, http.StatusBadRequest, "model_name_required")
		return
	}
	if apiErr := validateRealtimeWebSocketOrigin(c.Request); apiErr != nil {
		common.AbortWithErr(c, apiErr.StatusCode, apiErr)
		return
	}

	relay := &RelayModeChatRealtime{relayBase: relayBase{c: c}}
	relay.setOriginalModel(modelName)
	relay.beforeOpen = relay.acceptClientConnection

	if !relay.getProvider() {
		return
	}
	if relay.session != nil {
		relay.session.SetTurnObserverFactory(relay_util.NewRealtimeTurnObserverFactory(relay.getContext(), relay.models, relay.workPolicy))
	}

	bridge := newRealtimeRelayActorWithContext(relay.realtimeOpenContext(), relay.userConn, relay.session, time.Minute*2)
	bridge.providerPayloadObserver = func(_ wsconn.MessageType, payload []byte) {
		processProviderPayloadAPIError(relay.c, relay.provider.GetChannel(), payload, "chat_realtime_provider_frame")
	}

	bridge.Start()
	var closedBy string
	select {
	case <-bridge.UserClosed():
		closedBy = "user"
	case <-bridge.SupplierClosed():
		closedBy = "provider"
	}
	bridge.Wait()
	bridge.Close()
	logger.LogInfo(c.Request.Context(), fmt.Sprintf("连接由%s关闭", closedBy))
}

// 渠道准备完成后才申请建连许可和升级；后续安全的上游重试沿用同一连接。
func (r *RelayModeChatRealtime) acceptClientConnection() *types.OpenAIErrorWithStatusCode {
	if err := r.workPolicy.CheckFutureWork(r.models, false); err != nil {
		var apiErr *types.OpenAIErrorWithStatusCode
		if errors.As(err, &apiErr) {
			return apiErr
		}
		return common.ErrorWrapperLocal(err, "permission_denied", http.StatusForbidden)
	}
	if r.userConn != nil {
		return nil
	}
	if apiErr := middleware.AllowRealtimeConnectionAttempt(r.c); apiErr != nil {
		return apiErr
	}
	inboundActivityTimeout := config.RealtimeWebsocketClientInboundActivityTimeout()
	writeTimeout := config.RealtimeWebsocketWriteTimeout()
	userConn, err := wsconn.AcceptManaged(r.c.Writer, r.c.Request, wsconn.Config{
		Label:           "client-realtime",
		PingInterval:    config.RealtimeWebsocketClientPingInterval(),
		PongMissTimeout: config.RealtimeWebsocketClientPongMissTimeout(),
		InboundActivityTimeout: func() time.Duration {
			return inboundActivityTimeout
		},
		ReadLimit:    config.RealtimeWebsocketReadLimit(),
		WriteTimeout: func() time.Duration { return writeTimeout },
	}, wsconn.AcceptOptions{
		CheckOrigin:       realtimeWebSocketOriginAllowed,
		ResponseHeader:    websocketUpgradeResponseHeader(r.c.Request),
		Subprotocols:      echoableClientWebSocketSubprotocols(r.c.Request),
		EnableCompression: false,
	})
	if err != nil {
		logger.SysError("realtime websocket upgrade failed: " + err.Error())
		return common.ErrorWrapperLocal(err, "upgrade_failed", http.StatusBadRequest)
	}
	r.userConn = userConn
	return nil
}

func (r *RelayModeChatRealtime) openRealtimeSession(provider providersBase.ProviderInterface, modelName string, options runtimerealtime.RealtimeOpenOptions) (runtimerealtime.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	models := runtimesession.ModelBinding{
		RequestedModel:  r.getOriginalModel(),
		ProviderModel:   modelName,
		BillingModel:    modelName,
		BillingOriginal: r.c.GetBool("billing_original_model"),
	}
	if r.c.GetBool("billing_original_model") {
		models.BillingModel = models.RequestedModel
	}
	mapping, err := provider.GetChannel().GetModelMappingMap()
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_model_mapping", http.StatusServiceUnavailable)
	}
	policy := relay_util.NewRealtimeWorkPolicy(r.c, models, func(requested string) (runtimesession.ModelBinding, error) {
		if requested == models.RequestedModel {
			return models, nil
		}
		providerModel := requested
		if mapped := mapping[requested]; mapped != "" {
			providerModel = mapped
		}
		billingModel := providerModel
		billingOriginal := strings.HasPrefix(providerModel, "+")
		if billingOriginal {
			providerModel = strings.TrimPrefix(providerModel, "+")
			billingModel = requested
		}
		return runtimesession.ModelBinding{RequestedModel: requested, ProviderModel: providerModel, BillingModel: billingModel, BillingOriginal: billingOriginal}, nil
	})
	r.models, r.workPolicy = models, policy
	options.Models, options.WorkPolicy = models, policy
	if r.beforeOpen != nil {
		if apiErr := r.beforeOpen(); apiErr != nil {
			return nil, apiErr
		}
	}
	return openRealtimeSessionWithFreshFallback(provider, modelName, options)
}

func (r *RelayModeChatRealtime) abortWithMessage(message string) {
	if r != nil && r.userConn == nil && r.c != nil {
		common.AbortWithMessage(r.c, http.StatusServiceUnavailable, message)
		return
	}
	r.writeAbortPayload(buildRealtimeMessageErrorPayload(message), "system_error")
}

func (r *RelayModeChatRealtime) abortWithError(apiErr *types.OpenAIErrorWithStatusCode) {
	if r != nil && r.userConn == nil && r.c != nil && apiErr != nil {
		common.AbortWithErr(r.c, apiErr.StatusCode, apiErr)
		return
	}
	code := "system_error"
	if apiErr != nil {
		code = openAIErrorCodeString(apiErr.Code, code)
	}
	r.writeAbortPayload(buildRealtimeErrorPayload(apiErr), code)
}

func buildRealtimeMessageErrorPayload(message string) []byte {
	return []byte(types.NewErrorEvent("", "system_error", "system_error", message).Error())
}

func buildRealtimeErrorPayload(apiErr *types.OpenAIErrorWithStatusCode) []byte {
	if apiErr == nil {
		return buildRealtimeMessageErrorPayload("system_error")
	}

	errType := strings.TrimSpace(apiErr.Type)
	if errType == "" {
		errType = "system_error"
	}

	code := openAIErrorCodeString(apiErr.Code, "system_error")
	message := apiErr.Message
	if apiErr.LocalError || strings.TrimSpace(message) == "" || apiErr.StatusCode >= http.StatusInternalServerError {
		message = realtimeStaticErrorMessage(code)
	}
	return []byte(types.NewErrorEvent("", errType, code, message).Error())
}

func realtimeStaticErrorMessage(code string) string {
	switch strings.TrimSpace(code) {
	case "invalid_session_id":
		return "invalid realtime session id"
	case "provider_authentication_failed":
		return "provider authentication failed"
	case "provider_rate_limit_exceeded":
		return "provider rate limit exceeded"
	case "provider_ws_request_failed", "ws_request_failed":
		return "websocket request failed"
	case "session_binding_mismatch", "session_closed", "session_model_mismatch":
		return "realtime session is unavailable"
	default:
		return "realtime request failed"
	}
}

func (r *RelayModeChatRealtime) writeAbortPayload(payload []byte, code string) {
	if r == nil || r.userConn == nil {
		return
	}

	ctx := context.Background()
	if r.c != nil && r.c.Request != nil {
		ctx = r.c.Request.Context()
	}

	if err := r.userConn.WriteMessage(wsconn.TextMessage, payload); err != nil {
		logger.LogError(ctx, fmt.Sprintf("write realtime abort payload failed (code=%s): %v", strings.TrimSpace(code), err))
	}
	r.userConn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.CloseNormalClosure, Reason: strings.TrimSpace(code)})
}

func openAIErrorCodeString(code any, fallback string) string {
	switch typed := code.(type) {
	case string:
		if trimmed := strings.TrimSpace(typed); trimmed != "" {
			return trimmed
		}
	case nil:
	default:
		if trimmed := strings.TrimSpace(fmt.Sprint(typed)); trimmed != "" && trimmed != "<nil>" {
			return trimmed
		}
	}
	return fallback
}

func realtimeOpenRetryBudget() int {
	options := config.GlobalOption.RuntimeSnapshot()
	retryTimes := options.Int("RetryTimes", config.RetryTimes)
	if retryTimes <= 0 {
		return 1
	}
	return retryTimes
}

func (r *RelayModeChatRealtime) getProvider() bool {
	clientSessionID := realtimeClientSessionIDFromRequest(r.c.Request)
	preferredChannelID := prepareRealtimeChannelAffinity(r.c, r.getOriginalModel(), clientSessionID)
	openBudget := realtimeOpenRetryBudget()

	unlockAffinity := channelAffinityLock(r.c, channelAffinityKindRealtime, clientSessionID)
	defer unlockAffinity()

	if pinnedChannelID := explicitChannelPinID(r.c); pinnedChannelID > 0 {
		if clientSessionID != "" {
			return r.tryPinnedRealtimeSession(clientSessionID, pinnedChannelID)
		}
		return r.openFreshRealtimeSession(clientSessionID, false, openBudget)
	}

	if explicitChannelPinID(r.c) == 0 && preferredChannelID > 0 {
		ok, apiErr := r.tryAffinityRealtimeSession(clientSessionID, preferredChannelID)
		if ok {
			return true
		}
		if apiErr != nil {
			preferredChannel := model.ChannelGroup.GetChannel(preferredChannelID)
			observeRelayProviderFailure(r.c, preferredChannel, apiErr)
		}
		if currentChannelAffinityStrict(r.c) {
			if apiErr != nil {
				r.abortWithError(apiErr)
			} else {
				r.abortWithMessage("preferred realtime channel is unavailable")
			}
			return false
		}
		if apiErr != nil {
			preferredChannel := model.ChannelGroup.GetChannel(preferredChannelID)
			if !providerOpenCanRetry(apiErr) || preferredChannel == nil || !shouldRetry(r.c, apiErr, preferredChannel.Type) {
				r.abortWithError(apiErr)
				return false
			}
			r.excludeRealtimePreferredChannelForCurrentRequest(preferredChannelID, apiErr)
			openBudget--
			if openBudget <= 0 {
				r.abortWithError(apiErr)
				return false
			}
		}
	}

	return r.openFreshRealtimeSession(clientSessionID, clientSessionID != "", openBudget)
}

func (r *RelayModeChatRealtime) realtimeOpenContext() context.Context {
	if r == nil || r.c == nil || r.c.Request == nil {
		return context.Background()
	}
	return r.c.Request.Context()
}

func (r *RelayModeChatRealtime) tryPinnedRealtimeSession(clientSessionID string, channelID int) bool {
	channel, err := fetchChannelById(channelID)
	if err != nil {
		r.abortWithMessage(err.Error())
		return false
	}
	provider, modelName, err := prepareProviderForChannel(r.c, r.getOriginalModel(), channel)
	if err != nil {
		r.abortWithMessage(err.Error())
		return false
	}

	if !providerSupportsRealtime(provider) {
		r.abortWithMessage("channel not implemented")
		return false
	}

	realtimeSession, apiErr := r.openRealtimeSession(provider, modelName, runtimerealtime.RealtimeOpenOptions{
		Context:         r.realtimeOpenContext(),
		ClientSessionID: clientSessionID,
	})
	if apiErr == nil {
		r.activateRealtimeSession(provider, modelName, realtimeSession, channel.Id)
		return true
	}

	observeRelayProviderFailure(r.c, channel, apiErr)
	r.abortWithError(apiErr)
	return false
}

func (r *RelayModeChatRealtime) tryAffinityRealtimeSession(clientSessionID string, preferredChannelID int) (bool, *types.OpenAIErrorWithStatusCode) {
	channel, err := fetchPreferredRealtimeChannel(r.c, r.getOriginalModel(), preferredChannelID)
	if err != nil {
		r.clearUnavailableRealtimeAffinityIfSoft()
		return false, nil
	}
	provider, modelName, err := prepareProviderForChannel(r.c, r.getOriginalModel(), channel)
	if err != nil {
		r.clearUnavailableRealtimeAffinityIfSoft()
		return false, nil
	}

	if !providerSupportsRealtime(provider) {
		r.clearUnavailableRealtimeAffinityIfSoft()
		return false, nil
	}

	realtimeSession, apiErr := r.openRealtimeSession(provider, modelName, runtimerealtime.RealtimeOpenOptions{
		Context:         r.realtimeOpenContext(),
		ClientSessionID: clientSessionID,
	})
	if apiErr == nil {
		r.activateRealtimeSession(provider, modelName, realtimeSession, channel.Id)
		return true, nil
	}

	logger.LogError(
		r.c.Request.Context(),
		fmt.Sprintf("same-channel realtime open failed on channel #%d(%s): %s", channel.Id, channel.Name, apiErr.Error()),
	)
	return false, apiErr
}

func (r *RelayModeChatRealtime) clearUnavailableRealtimeAffinityIfSoft() {
	if r != nil && !currentChannelAffinityStrict(r.c) {
		clearCurrentChannelAffinity(r.c)
	}
}

func fetchPreferredRealtimeChannel(c *gin.Context, modelName string, preferredChannelID int) (*model.Channel, error) {
	if c == nil || preferredChannelID <= 0 {
		return nil, errors.New("preferred realtime channel is required")
	}
	selection := currentRealtimeChannelSelection(c)
	selection.preferredChannelID = preferredChannelID
	selection.strictPreferredChannel = true
	channel, err := fetchChannelByModelWithSelection(c, modelName, selection)
	if err != nil {
		return nil, err
	}
	if channel == nil || channel.Id != preferredChannelID {
		return nil, errors.New("preferred realtime channel is unavailable")
	}
	return channel, nil
}

func providerSupportsRealtime(provider providersBase.ProviderInterface) bool {
	if provider == nil {
		return false
	}
	_, ok := provider.(providersBase.RealtimeSessionProvider)
	return ok
}

func openRealtimeSessionWithOptions(provider providersBase.ProviderInterface, modelName string, options runtimerealtime.RealtimeOpenOptions) (runtimerealtime.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	if providerWithOptions, ok := provider.(providersBase.RealtimeSessionProviderWithOptions); ok {
		return providerWithOptions.OpenRealtimeSessionWithOptions(modelName, options)
	}
	realtimeProvider, ok := provider.(providersBase.RealtimeSessionProvider)
	if !ok {
		return nil, common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
	}
	return realtimeProvider.OpenRealtimeSession(modelName)
}

func openRealtimeSessionWithFreshFallback(provider providersBase.ProviderInterface, modelName string, options runtimerealtime.RealtimeOpenOptions) (runtimerealtime.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	realtimeSession, apiErr := openRealtimeSessionWithOptions(provider, modelName, options)
	if apiErr == nil || options.ForceFresh || !shouldForceFreshRealtimeSession(apiErr) {
		return realtimeSession, apiErr
	}

	options.ForceFresh = true
	return openRealtimeSessionWithOptions(provider, modelName, options)
}

func shouldForceFreshRealtimeSession(apiErr *types.OpenAIErrorWithStatusCode) bool {
	if apiErr == nil || !apiErr.LocalError {
		return false
	}
	switch openAIErrorCodeString(apiErr.Code, "") {
	case "session_binding_mismatch", "session_closed", "session_model_mismatch":
		return true
	default:
		return false
	}
}

func (r *RelayModeChatRealtime) activateRealtimeSession(provider providersBase.ProviderInterface, modelName string, realtimeSession runtimerealtime.RealtimeSession, channelID int) {
	if r == nil {
		return
	}

	r.provider = provider
	r.modelName = modelName
	r.session = realtimeSession
	metrics.RecordProvider(r.c, 200)
	refreshChannelAffinityForSelectedModel(r.c, channelAffinityKindRealtime, provider.GetChannel(), r.getOriginalModel())
	recordCurrentChannelAffinity(r.c, channelAffinityKindRealtime, channelID)
}

func (r *RelayModeChatRealtime) openFreshRealtimeSession(clientSessionID string, forceFresh bool, retryTimes int) bool {
	if retryTimes <= 0 {
		r.abortWithMessage("get provider failed")
		return false
	}

	for i := retryTimes; i > 0; i-- {
		if err := r.setProvider(r.getOriginalModel()); err != nil {
			r.abortWithMessage(err.Error())
			return false
		}

		channel := r.provider.GetChannel()
		if !providerSupportsRealtime(r.provider) {
			if explicitChannelPinID(r.c) > 0 {
				r.abortWithMessage("channel not implemented")
				return false
			}
			r.skipChannelIds(channel.Id)
			continue
		}

		realtimeSession, apiErr := r.openRealtimeSession(r.provider, r.modelName, runtimerealtime.RealtimeOpenOptions{
			Context:         r.realtimeOpenContext(),
			ClientSessionID: clientSessionID,
			ForceFresh:      forceFresh,
		})
		if apiErr == nil {
			r.activateRealtimeSession(r.provider, r.modelName, realtimeSession, channel.Id)
			return true
		}

		observeRelayProviderFailure(r.c, channel, apiErr)
		if !providerOpenCanRetry(apiErr) || !shouldRetry(r.c, apiErr, channel.Type) {
			logger.LogError(r.c.Request.Context(), fmt.Sprintf("using channel #%d(%s) Error: %s without retry", channel.Id, channel.Name, apiErr.Error()))
			r.abortWithError(apiErr)
			return false
		}

		r.skipChannelIds(channel.Id)
		forceFresh = true
		logger.LogError(r.c.Request.Context(), fmt.Sprintf("using channel #%d(%s) Error: %s to retry (remain times %d)", channel.Id, channel.Name, apiErr.Error(), i))
	}

	r.abortWithMessage("get provider failed")
	return false
}

func (r *RelayModeChatRealtime) excludeRealtimePreferredChannelForCurrentRequest(channelID int, apiErr *types.OpenAIErrorWithStatusCode) {
	if r == nil || r.c == nil || channelID <= 0 || explicitChannelPinID(r.c) > 0 {
		return
	}

	r.skipChannelIds(channelID)
	mergeChannelAffinityMeta(r.c, map[string]any{
		"channel_affinity_preferred_open_failed":          true,
		"channel_affinity_preferred_open_failed_id":       channelID,
		"channel_affinity_preferred_open_failed_excluded": true,
		"channel_affinity_preferred_open_failed_reason":   openAIErrorCodeString(apiErr.Code, strings.TrimSpace(apiErr.Message)),
	})
}

func (r *RelayModeChatRealtime) skipChannelIds(channelId int) {
	skipChannelIds, ok := utils.GetGinValue[[]int](r.c, "skip_channel_ids")
	if !ok {
		skipChannelIds = make([]int, 0)
	}

	skipChannelIds = append(skipChannelIds, channelId)

	r.c.Set("skip_channel_ids", skipChannelIds)
}
