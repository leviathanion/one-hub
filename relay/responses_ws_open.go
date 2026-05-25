package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/metrics"
	"one-api/model"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
)

func openAndPrimeResponsesWSSession(c *gin.Context, request *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openAndPrimeResponsesWSSessionWithContext(context.Background(), c, request)
}

func openAndPrimeResponsesWSSessionWithContext(openCtx context.Context, c *gin.Context, request *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	if c == nil || request == nil {
		return nil, common.StringErrorWrapperLocal("request is required", "invalid_request_error", http.StatusBadRequest)
	}
	markResponsesWSStreamRequest(c)
	setResponsesWSOpenPreviousResponseID(c, request.PreviousResponseID)
	if openCtx == nil {
		openCtx = context.Background()
	}
	candidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{Context: c, Request: request})
	if err != nil {
		logger.LogError(context.Background(), "responses websocket affinity preparation failed: "+err.Error())
		return nil, common.StringErrorWrapperLocal(responsesWSStaticErrorMessage("responses_affinity_conflict"), "responses_affinity_conflict", http.StatusConflict)
	}
	relay := &relayBase{c: c}
	relay.setOriginalModel(request.Model)

	if candidate != nil && candidate.ExplicitPinID > 0 {
		return openResponsesWSSpecificChannelWithContext(openCtx, c, request.Model, candidate, candidate.ExplicitPinID)
	}
	if preferred := currentPreferredChannelID(c); preferred > 0 {
		openResult, openErr := openResponsesWSPreferredChannelWithContext(openCtx, c, request.Model, candidate, preferred)
		if openErr == nil {
			return openResult, nil
		}
		if currentChannelAffinityStrict(c) {
			return nil, openErr
		}
		relay.skipChannelID(preferred)
	}

	retryTimes := realtimeOpenRetryBudget()
	var lastErr *types.OpenAIErrorWithStatusCode
	var lastNonUnsupportedErr *types.OpenAIErrorWithStatusCode
	providerAttempted := false
	unsupportedScans := 0
	unsupportedScanLimit := responsesWSUnsupportedScanLimit()
	for i := retryTimes; i > 0; i-- {
		if err := relay.setProvider(request.Model); err != nil {
			if !providerAttempted && lastErr == nil {
				logger.LogError(context.Background(), "responses websocket channel selection failed: "+err.Error())
				lastErr = common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
			}
			break
		}
		providerAttempted = true
		provider := relay.getProvider()
		channel := provider.GetChannel()
		session, apiErr := openRealtimeSessionWithOptions(provider, relay.modelName, runtimesession.RealtimeOpenOptions{
			Context:                       openCtx,
			PreferredTransport:            runtimesession.TransportModeResponsesWS,
			RequireWS:                     true,
			ResponsesWSPreviousResponseID: responsesWSOpenPreviousResponseID(c),
		})
		if apiErr == nil {
			metrics.RecordProvider(c, 200)
			return &responsesWSOpenResult{
				Session:       session,
				Provider:      provider,
				ProviderModel: relay.modelName,
				BillingModel:  relay.getModelName(),
				Channel:       channel,
				Candidate:     candidate,
			}, nil
		}
		lastErr = apiErr
		if openAIErrorCodeString(apiErr.Code, "") == "responses_ws_unsupported_for_channel" {
			relay.skipChannelID(channel.Id)
			unsupportedScans++
			if unsupportedScans < unsupportedScanLimit {
				i++
			}
			continue
		}
		lastNonUnsupportedErr = apiErr
		if !shouldRetry(c, apiErr, channel.Type) {
			break
		}
		relay.skipChannelID(channel.Id)
	}
	if lastNonUnsupportedErr != nil {
		return nil, lastNonUnsupportedErr
	}
	if lastErr == nil {
		lastErr = common.StringErrorWrapperLocal("channel does not support Responses websocket transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
	}
	return nil, lastErr
}

func responsesWSUnsupportedScanLimit() int {
	configured := config.RetryTimes
	if configured <= 0 {
		configured = 1
	}
	if viper.IsSet("responses_ws.unsupported_scan_limit") {
		if value := viper.GetInt("responses_ws.unsupported_scan_limit"); value > 0 {
			configured = value
		}
	}
	model.ChannelGroup.RLock()
	channelCount := len(model.ChannelGroup.Channels)
	model.ChannelGroup.RUnlock()
	if channelCount > 0 && channelCount < configured {
		return channelCount
	}
	return configured
}

func openResponsesWSSpecificChannelWithContext(openCtx context.Context, c *gin.Context, modelName string, candidate *ResponsesTurnAffinity, channelID int) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	markResponsesWSStreamRequest(c)
	channel, err := fetchChannelById(channelID)
	if err != nil {
		logger.LogError(context.Background(), "responses websocket pinned channel fetch failed: "+err.Error())
		return nil, common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
	}
	return openResponsesWSSelectedChannelWithContext(openCtx, c, modelName, candidate, channel)
}

func openResponsesWSPreferredChannel(c *gin.Context, modelName string, candidate *ResponsesTurnAffinity, channelID int) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openResponsesWSPreferredChannelWithContext(context.Background(), c, modelName, candidate, channelID)
}

func openResponsesWSPreferredChannelWithContext(openCtx context.Context, c *gin.Context, modelName string, candidate *ResponsesTurnAffinity, channelID int) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	markResponsesWSStreamRequest(c)
	channel, err := fetchPreferredRealtimeChannel(c, modelName, channelID)
	if err != nil {
		logger.LogError(context.Background(), "responses websocket preferred channel fetch failed: "+err.Error())
		return nil, common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
	}
	return openResponsesWSSelectedChannelWithContext(openCtx, c, modelName, candidate, channel)
}

func openResponsesWSSelectedChannelWithContext(openCtx context.Context, c *gin.Context, modelName string, candidate *ResponsesTurnAffinity, channel *model.Channel) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	if channel == nil {
		return nil, common.StringErrorWrapperLocal("channel not found", "channel_error", http.StatusServiceUnavailable)
	}
	markResponsesWSStreamRequest(c)
	if openCtx == nil {
		openCtx = context.Background()
	}
	provider, mappedModel, err := prepareProviderForChannel(c, modelName, channel)
	if err != nil {
		logger.LogError(context.Background(), "responses websocket provider preparation failed: "+err.Error())
		return nil, common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
	}
	if candidate != nil {
		candidate.SelectedChannelID = channel.Id
	}
	session, apiErr := openRealtimeSessionWithOptions(provider, mappedModel, runtimesession.RealtimeOpenOptions{
		Context:                       openCtx,
		PreferredTransport:            runtimesession.TransportModeResponsesWS,
		RequireWS:                     true,
		ResponsesWSPreviousResponseID: responsesWSOpenPreviousResponseID(c),
	})
	if apiErr != nil {
		return nil, apiErr
	}
	metrics.RecordProvider(c, 200)
	return &responsesWSOpenResult{
		Session:       session,
		Provider:      provider,
		ProviderModel: mappedModel,
		BillingModel:  responsesWSBillingModel(c, modelName, mappedModel),
		Channel:       channel,
		Candidate:     candidate,
	}, nil
}

func markResponsesWSStreamRequest(c *gin.Context) {
	if c != nil {
		c.Set("is_stream", true)
	}
}

func setResponsesWSOpenPreviousResponseID(c *gin.Context, previousResponseID string) {
	if c != nil {
		c.Set(responsesWSPreviousResponseIDContextKey, strings.TrimSpace(previousResponseID))
	}
}

func responsesWSOpenPreviousResponseID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.GetString(responsesWSPreviousResponseIDContextKey))
}

func attachResponsesWSSelectedChannelSnapshot(snapshot *ResponsesWSRequestSnapshot, channel *model.Channel, providerModel string, billingModel string) {
	if snapshot == nil || channel == nil {
		return
	}
	selected := &SelectedChannelSnapshot{
		ChannelID:            channel.Id,
		ChannelType:          channel.Type,
		PreCost:              channel.PreCost,
		ProviderModel:        strings.TrimSpace(providerModel),
		BillingModel:         strings.TrimSpace(billingModel),
		OriginalModel:        strings.TrimSpace(snapshot.GetString("original_model")),
		BillingOriginalModel: snapshotBool(snapshot, "billing_original_model"),
		Channel:              channel,
	}
	snapshot.Set("responses_ws_selected_channel_snapshot", selected)
	snapshot.Set("responses_ws_selected_channel", channel)
	snapshot.Set("channel_id", selected.ChannelID)
	snapshot.Set("channel_type", selected.ChannelType)
	snapshot.Set("new_model", selected.ProviderModel)
	snapshot.Set("billing_original_model", selected.BillingOriginalModel)
}

func clearResponsesWSSelectedChannelSnapshot(snapshot *ResponsesWSRequestSnapshot) {
	if snapshot == nil {
		return
	}
	snapshot.Delete(
		"responses_ws_selected_channel_snapshot",
		"responses_ws_selected_channel",
		"channel_id",
		"channel_type",
		"new_model",
		"billing_original_model",
	)
}

func snapshotBool(snapshot *ResponsesWSRequestSnapshot, key string) bool {
	value, ok := snapshot.Get(key)
	if !ok {
		return false
	}
	typed, _ := value.(bool)
	return typed
}

func (r *relayBase) skipChannelID(channelID int) {
	if r == nil || r.c == nil || channelID <= 0 {
		return
	}
	skipChannelIds, ok := r.c.Get("skip_channel_ids")
	if !ok {
		r.c.Set("skip_channel_ids", []int{channelID})
		return
	}
	typed, ok := skipChannelIds.([]int)
	if !ok {
		r.c.Set("skip_channel_ids", []int{channelID})
		return
	}
	r.c.Set("skip_channel_ids", append(typed, channelID))
}

func responsesWSProviderPayload(c *gin.Context, frame *responsesws.RawResponsesCreateFrame, request *types.OpenAIResponsesRequest, mappedModel string) ([]byte, error) {
	if request == nil {
		return nil, errors.New("responses websocket request is required")
	}
	// Raw frame remains the serialization source so unknown fields and exact JSON
	// shapes survive provider rewrite; the corrected typed request owns the model
	// validation shared by affinity, quota estimate, and payload rewrite.
	if strings.TrimSpace(request.Model) == "" {
		return nil, errors.New("response.create model is required")
	}
	providerModel := strings.TrimSpace(mappedModel)
	if providerModel == "" {
		return nil, errors.New("mapped responses websocket model is required")
	}
	return frame.CloneForModel(providerModel)
}

// Raw first-frame read errors can include private socket addresses. Keep code
// stable for clients, but use a precise client-safe message for diagnosis.
func responsesWSFirstFrameReadErrorMessage(err error) string {
	if errors.Is(err, wsconn.ErrFirstFrameTooLarge) {
		return "frame is too large or invalid; send smaller audio chunks"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout waiting for first websocket frame"
	}
	return "websocket read failed before first frame"
}

func responsesWSCurrentModelNames(c *gin.Context) (providerModel string, billingModel string) {
	if c == nil {
		return "", ""
	}
	providerModel = strings.TrimSpace(c.GetString("new_model"))
	originalModel := strings.TrimSpace(c.GetString("original_model"))
	billingModel = responsesWSBillingModel(c, originalModel, providerModel)
	return providerModel, billingModel
}

func responsesWSBillingModel(c *gin.Context, originalModel string, providerModel string) string {
	if c != nil && c.GetBool("billing_original_model") && strings.TrimSpace(originalModel) != "" {
		return strings.TrimSpace(originalModel)
	}
	return strings.TrimSpace(providerModel)
}

func responsesWSSubsequentModelMismatch(requestModel string, lockedSessionModel string) string {
	requestModel = strings.TrimSpace(requestModel)
	lockedSessionModel = strings.TrimSpace(lockedSessionModel)
	if requestModel == "" || lockedSessionModel == "" || requestModel == lockedSessionModel {
		return ""
	}
	return fmt.Sprintf("responses websocket session is locked to model %q", lockedSessionModel)
}
