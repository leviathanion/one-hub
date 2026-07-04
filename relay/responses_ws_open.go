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
	"one-api/common/requestctx"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/metrics"
	"one-api/model"
	providersBase "one-api/providers/base"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
)

const responsesWSUnsupportedScanWarnChannelThreshold = 16

var responsesWSUnsupportedScanWarnOnce sync.Once

func responsesWSGinLogContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}

func openAndPrimeResponsesWSSession(c *gin.Context, request *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openAndPrimeResponsesWSSessionWithContext(context.Background(), c, request)
}

func openAndPrimeResponsesWSSessionWithContext(openCtx context.Context, c *gin.Context, request *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openAndPrimeResponsesWSSessionWithContextAndFrame(openCtx, c, nil, request)
}

func openAndPrimeResponsesWSSessionWithContextAndFrame(openCtx context.Context, c *gin.Context, firstFrame *responsesws.RawResponsesCreateFrame, request *types.OpenAIResponsesRequest) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	if c == nil || request == nil {
		return nil, common.StringErrorWrapperLocal("request is required", "invalid_request_error", http.StatusBadRequest)
	}
	markResponsesWSStreamRequest(c)
	if openCtx == nil {
		openCtx = context.Background()
	}
	candidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{Context: c, Request: request})
	if err != nil {
		logger.LogError(responsesWSGinLogContext(c), "responses websocket affinity preparation failed: "+err.Error())
		return nil, common.StringErrorWrapperLocal(responsesWSStaticErrorMessage("responses_affinity_conflict"), "responses_affinity_conflict", http.StatusConflict)
	}
	relay := &relayBase{c: c}
	relay.setOriginalModel(request.Model)
	var lastErr *types.OpenAIErrorWithStatusCode
	var lastNonUnsupportedErr *types.OpenAIErrorWithStatusCode

	if candidate != nil && candidate.ExplicitPinID > 0 {
		return openResponsesWSSpecificChannelWithContext(openCtx, c, firstFrame, request.Model, candidate, candidate.ExplicitPinID, request.PreviousResponseID)
	}
	if preferred := currentPreferredChannelID(c); preferred > 0 {
		openResult, openErr := openResponsesWSPreferredChannelWithContext(openCtx, c, firstFrame, request.Model, candidate, preferred, request.PreviousResponseID)
		if openErr == nil {
			return openResult, nil
		}
		if currentChannelAffinityStrict(c) {
			return nil, openErr
		}
		if !responsesWSUnsupportedError(openErr) {
			lastNonUnsupportedErr = openErr
		}
		relay.skipChannelID(preferred)
	}

	attemptsRemaining := realtimeOpenRetryBudget()
	providerAttempted := false
	unsupportedScans := 0
	unsupportedScanLimit, unsupportedScanLimited := responsesWSUnsupportedScanPolicy()
	for attemptsRemaining > 0 {
		if err := relay.setProvider(request.Model); err != nil {
			if !providerAttempted && lastErr == nil {
				logger.LogError(responsesWSGinLogContext(c), "responses websocket channel selection failed: "+err.Error())
				lastErr = common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
			}
			break
		}
		providerAttempted = true
		provider := relay.getProvider()
		channel := provider.GetChannel()
		transport, apiErr := parseResponsesWSTransportMode(channel)
		if apiErr != nil {
			return nil, apiErr
		}
		session, apiErr := openResponsesWSUpstreamWithFrame(openCtx, c, provider, relay.modelName, responsesWSOpenParamsWithPreviousResponseID(c, request.PreviousResponseID, transport), firstFrame)
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
		if responsesWSUnsupportedError(apiErr) {
			relay.skipChannelID(channel.Id)
			unsupportedScans++
			if unsupportedScanLimited && unsupportedScans >= unsupportedScanLimit {
				lastNonUnsupportedErr = common.StringErrorWrapperLocal("responses websocket unsupported scan limit reached before exhausting candidates", "responses_ws_unsupported_scan_limited", http.StatusServiceUnavailable)
				break
			}
			continue
		}
		attemptsRemaining--
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

func responsesWSUnsupportedError(apiErr *types.OpenAIErrorWithStatusCode) bool {
	if apiErr == nil {
		return false
	}
	return openAIErrorCodeString(apiErr.Code, "") == "responses_ws_unsupported_for_channel"
}

func responsesWSUnsupportedScanLimit() int {
	limit, _ := responsesWSUnsupportedScanPolicy()
	return limit
}

func responsesWSUnsupportedScanPolicy() (int, bool) {
	configured := config.RetryTimes
	explicit := false
	if viper.IsSet("responses_ws.unsupported_scan_limit") {
		if value := viper.GetInt("responses_ws.unsupported_scan_limit"); value > 0 {
			configured = value
			explicit = true
		}
	}
	if configured <= 0 {
		configured = 1
	}
	model.ChannelGroup.RLock()
	channelCount := len(model.ChannelGroup.Channels)
	model.ChannelGroup.RUnlock()
	if channelCount <= 0 {
		return configured, explicit
	}
	if !explicit && channelCount > responsesWSUnsupportedScanWarnChannelThreshold {
		responsesWSUnsupportedScanWarnOnce.Do(func() {
			logCtx := context.Background()
			logger.LogWarn(logCtx, fmt.Sprintf(
				"responses_ws.unsupported_scan_limit is not set; ResponsesWS unsupported fallback may scan up to %d loaded channels before proving exhaustion. Configure responses_ws.unsupported_scan_limit to cap tail latency.",
				channelCount,
			))
		})
	}
	if explicit && configured < channelCount {
		return configured, true
	}
	return channelCount, false
}

func openResponsesWSSpecificChannelWithContext(openCtx context.Context, c *gin.Context, firstFrame *responsesws.RawResponsesCreateFrame, modelName string, candidate *ResponsesTurnAffinity, channelID int, previousResponseID string) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	markResponsesWSStreamRequest(c)
	channel, err := fetchChannelById(channelID)
	if err != nil {
		logger.LogError(responsesWSGinLogContext(c), "responses websocket pinned channel fetch failed: "+err.Error())
		if wrapped := invalidChannelRuntimeConfigAPIError(err); wrapped != nil {
			return nil, wrapped
		}
		return nil, common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
	}
	return openResponsesWSSelectedChannelWithContext(openCtx, c, firstFrame, modelName, candidate, channel, previousResponseID)
}

func openResponsesWSPreferredChannel(c *gin.Context, modelName string, candidate *ResponsesTurnAffinity, channelID int) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openResponsesWSPreferredChannelWithContext(context.Background(), c, nil, modelName, candidate, channelID, "")
}

func openResponsesWSPreferredChannelWithContext(openCtx context.Context, c *gin.Context, firstFrame *responsesws.RawResponsesCreateFrame, modelName string, candidate *ResponsesTurnAffinity, channelID int, previousResponseID string) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	markResponsesWSStreamRequest(c)
	channel, err := fetchPreferredRealtimeChannel(c, modelName, channelID)
	if err != nil {
		logger.LogError(responsesWSGinLogContext(c), "responses websocket preferred channel fetch failed: "+err.Error())
		if wrapped := invalidChannelRuntimeConfigAPIError(err); wrapped != nil {
			return nil, wrapped
		}
		return nil, common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
	}
	return openResponsesWSSelectedChannelWithContext(openCtx, c, firstFrame, modelName, candidate, channel, previousResponseID)
}

func openResponsesWSSelectedChannelWithContext(openCtx context.Context, c *gin.Context, firstFrame *responsesws.RawResponsesCreateFrame, modelName string, candidate *ResponsesTurnAffinity, channel *model.Channel, previousResponseID string) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	if channel == nil {
		return nil, common.StringErrorWrapperLocal("channel not found", "channel_error", http.StatusServiceUnavailable)
	}
	markResponsesWSStreamRequest(c)
	if openCtx == nil {
		openCtx = context.Background()
	}
	provider, mappedModel, err := prepareProviderForChannel(c, modelName, channel)
	if err != nil {
		logger.LogError(responsesWSGinLogContext(c), "responses websocket provider preparation failed: "+err.Error())
		if wrapped := invalidChannelRuntimeConfigAPIError(err); wrapped != nil {
			return nil, wrapped
		}
		return nil, common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
	}
	if candidate != nil {
		candidate.SelectedChannelID = channel.Id
	}
	transport, apiErr := parseResponsesWSTransportMode(channel)
	if apiErr != nil {
		return nil, apiErr
	}
	session, apiErr := openResponsesWSUpstreamWithFrame(openCtx, c, provider, mappedModel, responsesWSOpenParamsWithPreviousResponseID(c, previousResponseID, transport), firstFrame)
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

type responsesWSUpstreamOpenParams struct {
	upstreamSessionID  string
	previousResponseID string
	transport          runtimesession.TransportMode
	channelID          int
	diagnostics        responsesws.DiagnosticHook
}

func openResponsesWSUpstreamWithFrame(openCtx context.Context, c *gin.Context, provider providersBase.ProviderInterface, modelName string, options responsesWSUpstreamOpenParams, firstFrame *responsesws.RawResponsesCreateFrame) (responsesws.Upstream, *types.OpenAIErrorWithStatusCode) {
	responsesProvider, ok := provider.(providersBase.ResponsesWSProvider)
	if !ok {
		return nil, common.StringErrorWrapperLocal("channel does not support Responses websocket transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
	}
	if openCtx == nil {
		openCtx = context.Background()
	}
	headers := requestctx.HeaderSnapshot{}
	principal := requestctx.Principal{}
	if c != nil {
		if c.Request != nil {
			headers = requestctx.NewHeaderSnapshot(c.Request.Header)
		}
		principal = requestctx.PrincipalFromGin(c)
	}
	return responsesProvider.OpenResponsesWS(openCtx, &responsesws.OpenRequest{
		InboundHeaders:     headers,
		FirstFrame:         firstFrame,
		Principal:          principal,
		SelectedModel:      modelName,
		UpstreamSessionID:  options.upstreamSessionID,
		PreviousResponseID: options.previousResponseID,
		Transport:          options.transport,
		ChannelID:          options.channelID,
		Diagnostics:        options.diagnostics,
	})
}

// responsesWSOpenParams builds connection-local upstream options. The internal
// upstream session id is not derived from request x-session-id; client identity
// remains available to routing and prompt-cache code without sharing live WS
// connections across downstream clients.
func responsesWSOpenParams(c *gin.Context, transport ...runtimesession.TransportMode) responsesWSUpstreamOpenParams {
	return responsesWSOpenParamsWithPreviousResponseID(c, "", transport...)
}

func responsesWSOpenParamsWithPreviousResponseID(c *gin.Context, previousResponseID string, transport ...runtimesession.TransportMode) responsesWSUpstreamOpenParams {
	selectedTransport := runtimesession.TransportModeResponsesWS
	if len(transport) > 0 && transport[0] != "" {
		selectedTransport = transport[0]
	}
	channelID := 0
	if c != nil {
		channelID = c.GetInt("channel_id")
	}
	return responsesWSUpstreamOpenParams{
		upstreamSessionID:  ensureResponsesWSConnectionSessionID(c),
		previousResponseID: strings.TrimSpace(previousResponseID),
		transport:          selectedTransport,
		channelID:          channelID,
		diagnostics:        responsesWSDiagnosticHook(c),
	}
}

func responsesWSDiagnosticHook(c *gin.Context) responsesws.DiagnosticHook {
	requestID := ""
	connectionSessionID := ""
	userID := 0
	tokenID := 0
	if c != nil {
		requestID = c.GetString(logger.RequestIdKey)
		connectionSessionID = c.GetString(responsesWSConnectionSessionIDKey)
		userID = c.GetInt("id")
		tokenID = c.GetInt("token_id")
		if requestID == "" && c.Request != nil && c.Request.Context() != nil {
			if value, ok := c.Request.Context().Value(logger.RequestIdKey).(string); ok {
				requestID = value
			}
		}
	}
	logCtx := context.Background()
	if requestID != "" {
		logCtx = context.WithValue(logCtx, logger.RequestIdKey, requestID)
	}
	return func(diag responsesws.Diagnostic) {
		logger.LogError(logCtx, fmt.Sprintf(
			"responses websocket diagnostic: request_id=%s connection_session_id=%s user_id=%d token_id=%d code=%s provider=%s channel_id=%d transport=%s phase=%s panic_class=%s stack_hash=%s detail=%s",
			responsesWSSafeDiagnosticValue(requestID),
			responsesWSSafeDiagnosticValue(connectionSessionID),
			userID,
			tokenID,
			responsesWSSafeDiagnosticValue(diag.Code),
			responsesWSSafeDiagnosticValue(diag.Provider),
			diag.ChannelID,
			responsesWSSafeDiagnosticValue(diag.Transport),
			responsesWSSafeDiagnosticValue(string(diag.Phase)),
			responsesWSSafeDiagnosticValue(diag.PanicClass),
			responsesWSSafeDiagnosticValue(diag.StackHash),
			responsesWSSafeDiagnosticValue(diag.DetailError),
		))
	}
}

func parseResponsesWSTransportMode(channel *model.Channel) (runtimesession.TransportMode, *types.OpenAIErrorWithStatusCode) {
	if channel == nil {
		return runtimesession.TransportModeResponsesWS, nil
	}
	other, err := channel.GetOtherMap()
	if err != nil {
		return "", common.StringErrorWrapperLocal("invalid responses websocket transport configuration", "invalid_responses_ws_transport", http.StatusBadRequest)
	}
	raw, ok := other["responses_ws_transport"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return runtimesession.TransportModeResponsesWS, nil
	}
	mode, err := runtimesession.ParseResponsesWSTransportField(raw)
	if err != nil {
		return "", common.StringErrorWrapperLocal("invalid responses websocket transport configuration", "invalid_responses_ws_transport", http.StatusBadRequest)
	}
	return mode, nil
}
