package relay

import (
	"context"
	"encoding/json"
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
	"one-api/middleware"
	"one-api/model"
	providersBase "one-api/providers/base"
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
	return openAndPrimeResponsesWSSessionWithContextAndFrameAndAdmission(openCtx, c, firstFrame, request, nil)
}

func openAndPrimeResponsesWSSessionWithContextAndFrameAndAdmission(openCtx context.Context, c *gin.Context, firstFrame *responsesws.RawResponsesCreateFrame, request *types.OpenAIResponsesRequest, admit responsesWSOpenAdmission) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openAndPrimeResponsesWSSessionWithContextAndFrameAdmissionAndBudget(openCtx, c, firstFrame, request, admit, realtimeOpenRetryBudget())
}

func openAndPrimeResponsesWSSessionWithContextAndFrameAdmissionAndBudget(openCtx context.Context, c *gin.Context, firstFrame *responsesws.RawResponsesCreateFrame, request *types.OpenAIResponsesRequest, admit responsesWSOpenAdmission, attemptsRemaining int) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	if c == nil || request == nil {
		return nil, common.StringErrorWrapperLocal("request is required", "invalid_request_error", http.StatusBadRequest)
	}
	markResponsesWSStreamRequest(c)
	if openCtx == nil {
		openCtx = context.Background()
	}
	rawFields := map[string]json.RawMessage(nil)
	if firstFrame != nil {
		rawFields = firstFrame.Object
	}
	if err := validateResponsesSupportedSurface(request, rawFields, responsesOperationCreate); err != nil {
		return nil, capabilityGateAPIError(err)
	}
	if err := validateResponsesWSClientEnvelope(rawFields); err != nil {
		return nil, capabilityGateAPIError(err)
	}
	requireStored := request.Store == nil || *request.Store
	wsCapability := requireResponsesWSAdapterSupport(requireStored, rawFields, request.Model)
	setRequestChannelCapability(c, wsCapability)
	candidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{Context: c, Request: request})
	if err != nil {
		logger.LogError(responsesWSGinLogContext(c), "responses websocket affinity preparation failed: "+err.Error())
		if ownershipErr := responsesOwnershipAPIError(err); ownershipErr != nil {
			return nil, ownershipErr
		}
		return nil, common.StringErrorWrapperLocal(responsesWSStaticErrorMessage("responses_affinity_conflict"), "responses_affinity_conflict", http.StatusConflict)
	}
	relay := &relayBase{c: c}
	relay.setOriginalModel(request.Model)
	var lastErr *types.OpenAIErrorWithStatusCode
	var lastNonUnsupportedErr *types.OpenAIErrorWithStatusCode
	if attemptsRemaining <= 0 {
		attemptsRemaining = 1
	}

	if candidate != nil && candidate.ExplicitPinID > 0 {
		return openResponsesWSSpecificChannelWithContextAndAdmission(openCtx, c, firstFrame, request.Model, candidate, candidate.ExplicitPinID, admit)
	}
	if preferred := currentPreferredChannelID(c); preferred > 0 {
		openResult, openErr := openResponsesWSPreferredChannelWithContextAndAdmission(openCtx, c, firstFrame, request.Model, candidate, preferred, admit)
		if openErr == nil {
			return openResult, nil
		}
		preferredChannel := model.ChannelGroup.GetChannel(preferred)
		observeRelayProviderFailure(c, preferredChannel, openErr)
		attemptsRemaining--
		if currentChannelAffinityStrict(c) || candidate != nil && candidate.OwnershipChannelID > 0 {
			return nil, openErr
		}
		if responsesWSUnsupportedError(openErr) {
			lastErr = openErr
		} else {
			if preferredChannel == nil || !providerOpenCanRetry(openErr) || !shouldRetry(c, openErr, preferredChannel.Type) {
				return nil, openErr
			}
			lastNonUnsupportedErr = openErr
		}
		relay.skipChannelID(preferred)
	}

	providerAttempted := false
	unsupportedScans := 0
	unsupportedScanLimit, unsupportedScanLimited := responsesWSUnsupportedScanPolicy()
	for attemptsRemaining > 0 {
		if err := relay.setProvider(request.Model); err != nil {
			if !providerAttempted && lastErr == nil {
				logger.LogError(responsesWSGinLogContext(c), "responses websocket channel selection failed: "+err.Error())
				lastErr = responsesWSCapabilityAPIError(err)
				if lastErr == nil {
					lastErr = common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
				}
			}
			break
		}
		providerAttempted = true
		provider := relay.getProvider()
		channel := provider.GetChannel()
		if apiErr := middleware.AdmitAuthenticatedChannelWork(c, request.Model, channel.Id); apiErr != nil {
			return nil, apiErr
		}
		if strings.TrimSpace(relay.modelName) != strings.TrimSpace(request.Model) {
			relay.skipChannelID(channel.Id)
			lastErr = common.StringErrorWrapperLocal("native Responses WebSocket does not allow model mapping", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
			unsupportedScans++
			if unsupportedScanLimited && unsupportedScans >= unsupportedScanLimit {
				break
			}
			continue
		}
		activeLease, apiErr := admitResponsesWSOpen(c, admit)
		if apiErr != nil {
			return nil, apiErr
		}
		session, apiErr := openResponsesWSUpstreamWithFrame(openCtx, c, provider, relay.modelName, responsesWSOpenParams(c), firstFrame)
		if apiErr == nil {
			metrics.RecordProvider(c, 200)
			return &responsesWSOpenResult{
				Session:       session,
				ActiveLease:   activeLease,
				Provider:      provider,
				ProviderModel: relay.modelName,
				BillingModel:  relay.getModelName(),
				Channel:       channel,
				Candidate:     candidate,
			}, nil
		}
		if activeLease != nil {
			activeLease.Release()
		}
		lastErr = apiErr
		observeRelayProviderFailure(c, channel, apiErr)
		attemptsRemaining--
		if responsesWSUnsupportedError(apiErr) {
			relay.skipChannelID(channel.Id)
			unsupportedScans++
			if unsupportedScanLimited && unsupportedScans >= unsupportedScanLimit {
				lastNonUnsupportedErr = common.StringErrorWrapperLocal("responses websocket unsupported scan limit reached before exhausting candidates", "responses_ws_unsupported_scan_limited", http.StatusServiceUnavailable)
				break
			}
			continue
		}
		lastNonUnsupportedErr = apiErr
		if !providerOpenCanRetry(apiErr) || !shouldRetry(c, apiErr, channel.Type) {
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

func providerOpenCanRetry(apiErr *types.OpenAIErrorWithStatusCode) bool {
	return apiErr != nil && apiErr.ProviderOpenRetrySafe && apiErr.UpstreamNotAttempted && !apiErr.UpstreamAccepted && !apiErr.UpstreamAmbiguous
}

func responsesWSUnsupportedError(apiErr *types.OpenAIErrorWithStatusCode) bool {
	if apiErr == nil {
		return false
	}
	return openAIErrorCodeString(apiErr.Code, "") == "responses_ws_unsupported_for_channel"
}

func responsesWSCapabilityAPIError(err error) *types.OpenAIErrorWithStatusCode {
	apiErr := capabilityGateAPIError(err)
	if apiErr != nil && apiErr.StatusCode == http.StatusUpgradeRequired && strings.TrimSpace(apiErr.Param) == "" {
		apiErr.Code = "responses_ws_unsupported_for_channel"
	}
	return apiErr
}

func responsesWSUnsupportedScanLimit() int {
	limit, _ := responsesWSUnsupportedScanPolicy()
	return limit
}

func responsesWSUnsupportedScanPolicy() (int, bool) {
	options := config.GlobalOption.RuntimeSnapshot()
	return responsesWSUnsupportedScanPolicyForConfigured(options.Int("RetryTimes", config.RetryTimes))
}

func responsesWSUnsupportedScanPolicyForConfigured(configured int) (int, bool) {
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

func openResponsesWSSpecificChannelWithContext(openCtx context.Context, c *gin.Context, firstFrame *responsesws.RawResponsesCreateFrame, modelName string, candidate *ResponsesTurnAffinity, channelID int) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openResponsesWSSpecificChannelWithContextAndAdmission(openCtx, c, firstFrame, modelName, candidate, channelID, nil)
}

func openResponsesWSSpecificChannelWithContextAndAdmission(openCtx context.Context, c *gin.Context, firstFrame *responsesws.RawResponsesCreateFrame, modelName string, candidate *ResponsesTurnAffinity, channelID int, admit responsesWSOpenAdmission) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	markResponsesWSStreamRequest(c)
	var channel *model.Channel
	var err error
	if candidate != nil && candidate.StrictOwnerRoute && candidate.OwnershipChannelID == channelID {
		channel, err = fetchOwnerChannelById(openCtx, channelID)
	} else {
		channel, err = fetchChannelById(channelID)
	}
	if err != nil {
		logger.LogError(responsesWSGinLogContext(c), "responses websocket pinned channel fetch failed: "+err.Error())
		if wrapped := invalidChannelRuntimeConfigAPIError(err); wrapped != nil {
			return nil, wrapped
		}
		return nil, common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
	}
	return openResponsesWSSelectedChannelWithContextAndAdmission(openCtx, c, firstFrame, modelName, candidate, channel, true, admit)
}

func openResponsesWSPreferredChannel(c *gin.Context, modelName string, candidate *ResponsesTurnAffinity, channelID int) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openResponsesWSPreferredChannelWithContext(context.Background(), c, nil, modelName, candidate, channelID)
}

func openResponsesWSPreferredChannelWithContext(openCtx context.Context, c *gin.Context, firstFrame *responsesws.RawResponsesCreateFrame, modelName string, candidate *ResponsesTurnAffinity, channelID int) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openResponsesWSPreferredChannelWithContextAndAdmission(openCtx, c, firstFrame, modelName, candidate, channelID, nil)
}

func openResponsesWSPreferredChannelWithContextAndAdmission(openCtx context.Context, c *gin.Context, firstFrame *responsesws.RawResponsesCreateFrame, modelName string, candidate *ResponsesTurnAffinity, channelID int, admit responsesWSOpenAdmission) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	markResponsesWSStreamRequest(c)
	channel, err := fetchPreferredRealtimeChannel(c, modelName, channelID)
	if err != nil {
		logger.LogError(responsesWSGinLogContext(c), "responses websocket preferred channel fetch failed: "+err.Error())
		if wrapped := invalidChannelRuntimeConfigAPIError(err); wrapped != nil {
			return nil, wrapped
		}
		return nil, common.StringErrorWrapperLocal("channel selection failed", "channel_error", http.StatusServiceUnavailable)
	}
	return openResponsesWSSelectedChannelWithContextAndAdmission(openCtx, c, firstFrame, modelName, candidate, channel, false, admit)
}

func openResponsesWSSelectedChannelWithContext(openCtx context.Context, c *gin.Context, firstFrame *responsesws.RawResponsesCreateFrame, modelName string, candidate *ResponsesTurnAffinity, channel *model.Channel, requireExactChannelModel bool) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	return openResponsesWSSelectedChannelWithContextAndAdmission(openCtx, c, firstFrame, modelName, candidate, channel, requireExactChannelModel, nil)
}

func openResponsesWSSelectedChannelWithContextAndAdmission(openCtx context.Context, c *gin.Context, firstFrame *responsesws.RawResponsesCreateFrame, modelName string, candidate *ResponsesTurnAffinity, channel *model.Channel, requireExactChannelModel bool, admit responsesWSOpenAdmission) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
	if channel == nil {
		return nil, common.StringErrorWrapperLocal("channel not found", "channel_error", http.StatusServiceUnavailable)
	}
	if apiErr := middleware.AdmitAuthenticatedChannelWork(c, modelName, channel.Id); apiErr != nil {
		return nil, apiErr
	}
	markResponsesWSStreamRequest(c)
	if capability := currentRequestChannelCapability(c); capability != nil {
		if err := capability(channel); err != nil {
			if wrapped := responsesWSCapabilityAPIError(err); wrapped != nil {
				return nil, wrapped
			}
		}
	}
	if apiErr := responsesWSSelectedChannelModelAdmissionError(c, channel, modelName, requireExactChannelModel); apiErr != nil {
		return nil, apiErr
	}
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
	if strings.TrimSpace(mappedModel) != strings.TrimSpace(modelName) {
		return nil, common.StringErrorWrapperLocal("native Responses WebSocket does not allow model mapping", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
	}
	if candidate != nil {
		candidate.SelectedChannelID = channel.Id
	}
	activeLease, apiErr := admitResponsesWSOpen(c, admit)
	if apiErr != nil {
		return nil, apiErr
	}
	session, apiErr := openResponsesWSUpstreamWithFrame(openCtx, c, provider, mappedModel, responsesWSOpenParams(c), firstFrame)
	if apiErr != nil {
		if activeLease != nil {
			activeLease.Release()
		}
		return nil, apiErr
	}
	metrics.RecordProvider(c, 200)
	return &responsesWSOpenResult{
		Session:       session,
		ActiveLease:   activeLease,
		Provider:      provider,
		ProviderModel: mappedModel,
		BillingModel:  responsesWSBillingModel(c, modelName, mappedModel),
		Channel:       channel,
		Candidate:     candidate,
	}, nil
}

func admitResponsesWSOpen(c *gin.Context, admit responsesWSOpenAdmission) (middleware.ResponsesWSLease, *types.OpenAIErrorWithStatusCode) {
	if admit == nil {
		return nil, nil
	}
	return admit(c)
}
func markResponsesWSStreamRequest(c *gin.Context) {
	if c != nil {
		c.Set("is_stream", true)
	}
}

func attachResponsesWSSelectedChannelFacts(snapshot *ResponsesWSRequestSnapshot, channel *model.Channel, providerModel string) {
	if snapshot == nil || channel == nil {
		return
	}
	snapshot.Set("responses_ws_selected_channel", channel)
	snapshot.Set("channel_id", channel.Id)
	snapshot.Set("channel_type", channel.Type)
	snapshot.Set("new_model", strings.TrimSpace(providerModel))
}

func clearResponsesWSSelectedChannelFacts(snapshot *ResponsesWSRequestSnapshot) {
	if snapshot == nil {
		return
	}
	snapshot.Delete(
		"responses_ws_selected_channel",
		"channel_id",
		"channel_type",
		"new_model",
		"billing_original_model",
	)
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
	if frame == nil || request == nil {
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
	if strings.TrimSpace(request.Model) != providerModel || strings.TrimSpace(frame.Projection.Model) != providerModel {
		return nil, errors.New("native responses websocket does not allow model rewriting")
	}
	return append([]byte(nil), frame.Raw...), nil
}

// Raw first-frame read errors can include private socket addresses. Keep code
// stable for clients, but use a precise client-safe message for diagnosis.
func responsesWSFirstFrameReadErrorMessage(err error) string {
	if errors.Is(err, wsconn.ErrFirstFrameByteBudget) {
		return "responses websocket pending byte capacity is exhausted"
	}
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

type responsesWSOpenPermitKey struct{}

type responsesWSUpstreamOpenParams struct {
	upstreamSessionID string
	channelID         int
	diagnostics       responsesws.DiagnosticHook
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
	if err := openCtx.Err(); err != nil {
		return nil, common.ErrorWrapperLocal(err, "responses_ws_closing", http.StatusServiceUnavailable)
	}
	if allow, ok := openCtx.Value(responsesWSOpenPermitKey{}).(func() bool); ok && !allow() {
		return nil, common.ErrorWrapperLocal(context.Canceled, "responses_ws_closing", http.StatusServiceUnavailable)
	}
	return responsesProvider.OpenResponsesWS(openCtx, &responsesws.OpenRequest{
		InboundHeaders:    headers,
		FirstFrame:        firstFrame,
		Principal:         principal,
		SelectedModel:     modelName,
		UpstreamSessionID: options.upstreamSessionID,
		ChannelID:         options.channelID,
		Diagnostics:       options.diagnostics,
	})
}

// responsesWSOpenParams builds connection-local upstream options. The internal
// upstream session id is not derived from request x-session-id; client identity
// remains available to routing and prompt-cache code without sharing live WS
// connections across downstream clients.
func responsesWSOpenParams(c *gin.Context) responsesWSUpstreamOpenParams {
	channelID := 0
	if c != nil {
		channelID = c.GetInt("channel_id")
	}
	return responsesWSUpstreamOpenParams{
		upstreamSessionID: ensureResponsesWSConnectionSessionID(c),
		channelID:         channelID,
		diagnostics:       responsesWSDiagnosticHook(c),
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
