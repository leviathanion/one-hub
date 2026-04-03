package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/common/utils"
	"one-api/metrics"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/relay/relay_util"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type RelayModeChatRealtime struct {
	relayBase
	userConn *websocket.Conn
	session  runtimesession.RealtimeSession
	quota    *relay_util.Quota
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	Subprotocols: []string{"realtime"},
}

func ChatRealtime(c *gin.Context) {
	modelName := c.Query("model")
	if modelName == "" {
		common.AbortWithMessage(c, http.StatusBadRequest, "model_name_required")
		return
	}

	userConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		fmt.Println("upgrade failed", err)
		common.AbortWithMessage(c, http.StatusInternalServerError, "upgrade_failed")
		return
	}

	relay := &RelayModeChatRealtime{
		relayBase: relayBase{
			c: c,
		},
		userConn: userConn,
	}
	relay.setOriginalModel(modelName)

	if !relay.getProvider() {
		return
	}

	relay.quota = relay_util.NewQuota(relay.getContext(), relay.getModelName(), 0)
	if relay.session != nil {
		// Realtime quota observation lives in the provider session turn observer.
		relay.session.SetTurnObserverFactory(relay_util.NewRealtimeTurnObserverFactory(relay.quota))
	}

	type realtimeProxy interface {
		Start()
		Wait()
		Close()
		UserClosed() <-chan struct{}
		SupplierClosed() <-chan struct{}
	}

	var proxy realtimeProxy
	proxy = requester.NewRealtimeSessionProxy(relay.userConn, relay.session, time.Minute*2)

	proxy.Start()
	go func() {
		var closedBy string
		select {
		case <-proxy.UserClosed():
			closedBy = "user"
		case <-proxy.SupplierClosed():
			closedBy = "provider"
		}

		logger.LogInfo(relay.c.Request.Context(), fmt.Sprintf("连接由%s关闭", closedBy))
	}()

	proxy.Wait()
	proxy.Close()
}

func (r *RelayModeChatRealtime) abortWithMessage(message string) {
	r.writeAbortPayload(buildRealtimeMessageErrorPayload(message), "system_error")
}

func (r *RelayModeChatRealtime) abortWithError(apiErr *types.OpenAIErrorWithStatusCode) {
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

	return []byte(types.NewErrorEvent("", errType, openAIErrorCodeString(apiErr.Code, "system_error"), apiErr.Message).Error())
}

func (r *RelayModeChatRealtime) writeAbortPayload(payload []byte, code string) {
	if r == nil || r.userConn == nil {
		return
	}

	ctx := context.Background()
	if r.c != nil && r.c.Request != nil {
		ctx = r.c.Request.Context()
	}

	if err := r.userConn.WriteMessage(websocket.TextMessage, payload); err != nil {
		logger.LogError(ctx, fmt.Sprintf("write realtime abort payload failed (code=%s): %v", strings.TrimSpace(code), err))
	}
	if err := r.userConn.Close(); err != nil {
		logger.LogError(ctx, fmt.Sprintf("close realtime websocket failed after abort (code=%s): %v", strings.TrimSpace(code), err))
	}
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
	if config.RetryTimes <= 0 {
		return 1
	}
	return config.RetryTimes
}

func (r *RelayModeChatRealtime) getProvider() bool {
	clientSessionID := realtimeClientSessionIDFromRequest(r.c.Request)
	preferredChannelID := prepareRealtimeChannelAffinity(r.c, r.getOriginalModel(), clientSessionID)

	unlockAffinity := channelAffinityLock(r.c, channelAffinityKindRealtime, clientSessionID)
	defer unlockAffinity()

	if pinnedChannelID := explicitChannelPinID(r.c); pinnedChannelID > 0 {
		if clientSessionID != "" {
			return r.tryPinnedRealtimeSession(clientSessionID, pinnedChannelID)
		}
		return r.openFreshRealtimeSession(clientSessionID, false)
	}

	if explicitChannelPinID(r.c) == 0 && preferredChannelID > 0 {
		ok, apiErr := r.tryAffinityRealtimeSession(clientSessionID, preferredChannelID)
		if ok {
			return true
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
			r.excludeRealtimePreferredChannelForCurrentRequest(preferredChannelID, apiErr)
		}
	}

	return r.openFreshRealtimeSession(clientSessionID, clientSessionID != "")
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

	realtimeSession, apiErr := openRealtimeSessionWithFreshFallback(provider, modelName, runtimesession.RealtimeOpenOptions{
		ClientSessionID: clientSessionID,
	})
	if apiErr == nil {
		r.activateRealtimeSession(provider, modelName, realtimeSession, channel.Id)
		return true
	}

	r.abortWithError(apiErr)
	return false
}

func (r *RelayModeChatRealtime) tryAffinityRealtimeSession(clientSessionID string, preferredChannelID int) (bool, *types.OpenAIErrorWithStatusCode) {
	channel, err := fetchPreferredRealtimeChannel(r.c, r.getOriginalModel(), preferredChannelID)
	if err != nil {
		clearCurrentChannelAffinity(r.c)
		return false, nil
	}

	provider, modelName, err := prepareProviderForChannel(r.c, r.getOriginalModel(), channel)
	if err != nil {
		clearCurrentChannelAffinity(r.c)
		return false, nil
	}

	if !providerSupportsRealtime(provider) {
		clearCurrentChannelAffinity(r.c)
		return false, nil
	}

	realtimeSession, apiErr := openRealtimeSessionWithFreshFallback(provider, modelName, runtimesession.RealtimeOpenOptions{
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

func openRealtimeSessionWithOptions(provider providersBase.ProviderInterface, modelName string, options runtimesession.RealtimeOpenOptions) (runtimesession.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	if providerWithOptions, ok := provider.(providersBase.RealtimeSessionProviderWithOptions); ok {
		return providerWithOptions.OpenRealtimeSessionWithOptions(modelName, options)
	}
	realtimeProvider, ok := provider.(providersBase.RealtimeSessionProvider)
	if !ok {
		return nil, common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
	}
	return realtimeProvider.OpenRealtimeSession(modelName)
}

func openRealtimeSessionWithFreshFallback(provider providersBase.ProviderInterface, modelName string, options runtimesession.RealtimeOpenOptions) (runtimesession.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
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

func (r *RelayModeChatRealtime) activateRealtimeSession(provider providersBase.ProviderInterface, modelName string, realtimeSession runtimesession.RealtimeSession, channelID int) {
	if r == nil {
		return
	}

	r.provider = provider
	r.modelName = modelName
	r.session = realtimeSession
	metrics.RecordProvider(r.c, 200)
	recordCurrentChannelAffinity(r.c, channelAffinityKindRealtime, channelID)
}

func (r *RelayModeChatRealtime) openFreshRealtimeSession(clientSessionID string, forceFresh bool) bool {
	retryTimes := realtimeOpenRetryBudget()

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

		realtimeSession, apiErr := openRealtimeSessionWithFreshFallback(r.provider, r.modelName, runtimesession.RealtimeOpenOptions{
			ClientSessionID: clientSessionID,
			ForceFresh:      forceFresh,
		})
		if apiErr == nil {
			r.activateRealtimeSession(r.provider, r.modelName, realtimeSession, channel.Id)
			return true
		}

		if !shouldRetry(r.c, apiErr, channel.Type) {
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
