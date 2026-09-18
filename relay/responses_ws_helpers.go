package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"one-api/common/logger"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
	"one-api/relay/relay_util"
	"one-api/types"
	"strings"
)

// Client-facing error messages must not expose parser/validation internals.
// Detailed errors are logged server-side; clients receive static messages only.
const (
	responsesWSErrorCodeInvalidResponseCreate = "invalid_response_create"
	responsesWSMessageInvalidWebsocketEvent   = "invalid websocket event"
	responsesWSMessageInvalidResponseCreate   = "invalid response.create"
)

var responsesWSSystemErrorPayloadLiteral = []byte(`{"type":"error","status":500,"error":{"type":"invalid_request_error","code":"system_error","message":"system error"}}`)

func responsesWSErrorPayload(status int, code string, message string) []byte {
	return responsesWSErrorPayloadWithParam(status, code, message, "")
}

func responsesWSSafeErrorDiagnostic(err error) string {
	if err == nil {
		return ""
	}
	return responsesWSSafeDiagnosticValue(err.Error())
}

func responsesWSErrorPayloadWithParam(status int, code string, message string, param string) []byte {
	payload := map[string]any{
		"type":   "error",
		"status": status,
		"error": map[string]any{
			"type":    "invalid_request_error",
			"code":    code,
			"message": message,
		},
	}
	if strings.TrimSpace(param) != "" {
		payload["error"].(map[string]any)["param"] = strings.TrimSpace(param)
	}
	return responsesWSMarshalErrorPayload(payload)
}

func responsesWSMarshalErrorPayload(payload any) []byte {
	encoded, err := json.Marshal(payload)
	if err == nil && len(encoded) > 0 {
		return encoded
	}
	if err != nil {
		logger.LogError(context.Background(), "responses websocket error payload marshal failed: "+err.Error())
	} else {
		logger.LogError(context.Background(), "responses websocket error payload marshal returned empty payload")
	}
	return append([]byte(nil), responsesWSSystemErrorPayloadLiteral...)
}

func responsesWSPreviousResponseNotFoundPayload() []byte {
	return responsesWSErrorPayloadWithParam(http.StatusBadRequest, "previous_response_not_found", responsesWSStaticErrorMessage("previous_response_not_found"), "previous_response_id")
}

func responsesWSFallbackPayload() []byte {
	return responsesWSErrorPayload(http.StatusUpgradeRequired, "responses_ws_unsupported_for_channel", "channel does not support Responses websocket transport")
}

func responsesWSErrorFromOpenAI(apiErr *types.OpenAIErrorWithStatusCode) []byte {
	if apiErr == nil {
		return responsesWSErrorPayload(http.StatusInternalServerError, "system_error", "system error")
	}
	errType := strings.TrimSpace(apiErr.Type)
	if errType == "" {
		errType = "one_hub_error"
	}
	code := openAIErrorCodeString(apiErr.Code, "system_error")
	if code == "previous_response_not_found" {
		return responsesWSPreviousResponseNotFoundPayload()
	}
	message := responsesWSClientMessageFromOpenAI(apiErr, code)
	param := responsesWSClientParamFromOpenAI(apiErr)
	payload := map[string]any{
		"type":   "error",
		"status": apiErr.StatusCode,
		"error": map[string]any{
			"type":    errType,
			"code":    code,
			"message": message,
			"param":   param,
		},
	}
	return responsesWSMarshalErrorPayload(payload)
}

func responsesWSClientMessageFromOpenAI(apiErr *types.OpenAIErrorWithStatusCode, code string) string {
	if apiErr == nil {
		return responsesWSStaticErrorMessage("system_error")
	}
	if apiErr.LocalError && code == unsupportedCapabilityCode && strings.TrimSpace(apiErr.Message) != "" {
		return apiErr.Message
	}
	if !apiErr.LocalError && strings.TrimSpace(apiErr.Message) != "" && apiErr.StatusCode < http.StatusInternalServerError {
		return apiErr.Message
	}
	return responsesWSStaticErrorMessage(code)
}

func responsesWSClientParamFromOpenAI(apiErr *types.OpenAIErrorWithStatusCode) string {
	if apiErr == nil {
		return ""
	}
	if apiErr.LocalError && openAIErrorCodeString(apiErr.Code, "") != unsupportedCapabilityCode {
		return ""
	}
	param := strings.TrimSpace(apiErr.Param)
	if param == "" {
		return ""
	}
	switch param {
	case "model", "input", "instructions", "tools", "tool_choice", "temperature", "top_p", "max_output_tokens", "previous_response_id", "metadata", "stream", "truncation", "context_management", "client_metadata", "service_tier", "processing_class":
		return param
	default:
		return ""
	}
}

func responsesWSErrorFromErr(err error) []byte {
	if err == nil {
		return nil
	}
	if payload := responsesws.ClientPayloadFromError(err); len(payload) > 0 {
		return payload
	}
	if errors.Is(err, responsesws.ErrStaleContinuation) {
		return responsesWSPreviousResponseNotFoundPayload()
	}
	var apiErr *types.OpenAIErrorWithStatusCode
	if errors.As(err, &apiErr) && apiErr != nil {
		return responsesWSErrorFromOpenAI(apiErr)
	}
	var event *types.Event
	if errors.As(err, &event) && event != nil && event.IsError() {
		code := openAIErrorCodeString(event.ErrorDetail.Code, "upstream_error")
		message := responsesWSStaticErrorMessage(code)
		return responsesWSErrorPayload(http.StatusBadGateway, code, message)
	}
	logCtx := context.Background()
	logger.LogError(logCtx, "responses websocket upstream error: "+responsesWSSafeErrorDiagnostic(err))
	return responsesWSErrorPayload(http.StatusBadGateway, "upstream_error", responsesWSStaticErrorMessage("upstream_error"))
}

func responsesWSStaticErrorMessage(code string) string {
	switch strings.TrimSpace(code) {
	case "invalid_event":
		return "invalid response.create event"
	case responsesWSErrorCodeInvalidResponseCreate:
		return responsesWSMessageInvalidResponseCreate
	case "responses_affinity_conflict":
		return "responses affinity conflict"
	case "quota_rollback_failed":
		return "quota rollback failed"
	case "responses_ws_attempt_failed":
		return "responses websocket turn attempt failed"
	case "responses_ws_payload_rewrite_failed":
		return "internal payload rewrite failed"
	case "responses_ws_send_queue_full":
		return "responses websocket upstream send queue is full"
	case "previous_response_not_found":
		return "previous response was not found"
	case "provider_connection_closed":
		return "upstream websocket connection closed"
	case "ws_write_failed":
		return "upstream websocket write failed"
	case "upstream_error":
		return "upstream websocket request failed"
	default:
		return "responses websocket request failed"
	}
}

func isProviderReportedContinuationMiss(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, responsesws.ErrStaleContinuation) {
		return false
	}
	payload := responsesWSErrorFromErr(err)
	if len(payload) == 0 {
		return false
	}
	return responsesws.ClassifyResponsesWSEvent(payload).ContinuationMiss || strings.Contains(strings.ToLower(err.Error()), "previous_response_not_found")
}

func mergeResponsesWSUsageEvent(usage *types.Usage, event *types.UsageEvent) {
	if usage == nil || event == nil {
		return
	}
	mergeProjectedResponsesWSUsageEvent(usage, relay_util.ProviderUsageEventForBilling(event))
}

func mergeProjectedResponsesWSUsageEvent(usage *types.Usage, event *types.UsageEvent) {
	if event.ProviderTokenEvidence {
		usage.MarkProviderReported()
		usage.PromptTokens += event.InputTokens
		usage.CompletionTokens += event.OutputTokens
		usage.TotalTokens += event.TotalTokens
		usage.PromptTokensDetails.Merge(&event.InputTokenDetails)
		usage.CompletionTokensDetails.Merge(&event.OutputTokenDetails)
		usage.ExtraTokens = mergeIntMaps(usage.ExtraTokens, event.ExtraTokens)
	}
	if len(event.ExtraUsageUnits) > 0 {
		if usage.ExtraUsageUnits == nil {
			usage.ExtraUsageUnits = make(map[string]float64, len(event.ExtraUsageUnits))
		}
		for key, value := range event.ExtraUsageUnits {
			usage.ExtraUsageUnits[key] += value
		}
	}
	markResponsesWSIndependentUsageEvidence(usage, event.ProviderIndependentUsageUnits)
	usage.MergeExtraBilling(event.ExtraBilling)
	if usage.ProviderExtraBilling == nil && len(event.ProviderExtraBilling) > 0 {
		usage.ProviderExtraBilling = make(map[string]bool)
	}
	for key, present := range event.ProviderExtraBilling {
		if present {
			usage.ProviderExtraBilling[key] = true
		}
	}
	usage.MergeBillingDiagnostics(event.BillingDiagnostics)
	usage.AttributionConflict = usage.AttributionConflict || event.AttributionConflict
	usage.MergeProviderAttribution(event.ResponseModel, event.ServiceTier)
	usage.MergeProviderSpeed(event.Speed, event.SpeedConflict)
}

func markResponsesWSIndependentUsageEvidence(usage *types.Usage, providerEvidence map[string]bool) {
	if usage == nil || len(providerEvidence) == 0 {
		return
	}
	if usage.ProviderIndependentUsageUnits == nil {
		usage.ProviderIndependentUsageUnits = make(map[string]bool, len(providerEvidence))
	}
	for key, present := range providerEvidence {
		if !present {
			continue
		}
		usage.ProviderIndependentUsageUnits[key] = true
	}
}

func mergeResponsesWSIndependentUsageUnits(usage *types.Usage, units map[string]float64, providerEvidence map[string]bool) {
	markResponsesWSIndependentUsageEvidence(usage, providerEvidence)
	for key, present := range providerEvidence {
		value, exists := units[key]
		if !present || !exists {
			continue
		}
		if usage.ExtraUsageUnits == nil {
			usage.ExtraUsageUnits = make(map[string]float64)
		}
		usage.ExtraUsageUnits[key] += value
	}
}

func mergeResponsesWSResponsesUsage(usage *types.Usage, responseUsage *types.ResponsesUsage) {
	if usage == nil || responseUsage == nil {
		return
	}
	if responseUsage.InputTokens > 0 {
		usage.PromptTokens = responseUsage.InputTokens
	}
	if responseUsage.OutputTokens > 0 {
		usage.CompletionTokens = responseUsage.OutputTokens
	}
	if responseUsage.TotalTokens > 0 {
		usage.TotalTokens = responseUsage.TotalTokens
	}
	if responseUsage.InputTokensDetails != nil {
		overwritePositiveInt(&usage.PromptTokensDetails.AudioTokens, responseUsage.InputTokensDetails.AudioTokens)
		overwritePositiveInt(&usage.PromptTokensDetails.CachedTokens, responseUsage.InputTokensDetails.CachedTokens)
		overwritePositiveInt(&usage.PromptTokensDetails.CacheWriteTokens, responseUsage.InputTokensDetails.CacheWriteTokens)
		overwritePositiveInt(&usage.PromptTokensDetails.TextTokens, responseUsage.InputTokensDetails.TextTokens)
		overwritePositiveInt(&usage.PromptTokensDetails.ImageTokens, responseUsage.InputTokensDetails.ImageTokens)
	}
	if responseUsage.OutputTokensDetails != nil {
		overwritePositiveInt(&usage.CompletionTokensDetails.ReasoningTokens, responseUsage.OutputTokensDetails.ReasoningTokens)
	}
	if responseUsage.ProviderReported {
		usage.MarkProviderReported()
	}
}

func overwritePositiveInt(dst *int, src int) {
	if dst != nil && src > 0 {
		*dst = src
	}
}

func mergeResponsesWSTerminalResponse(usage *types.Usage, response *types.OpenAIResponsesResponses, imageTrackers ...*commonresponses.ImageGenerationStreamTracker) {
	if usage == nil || response == nil {
		return
	}
	mergeResponsesWSResponsesUsage(usage, response.Usage)
	usage.MergeProviderAttribution(response.Model, response.ServiceTier)
	response.ApplyUsageAttribution(usage)
	// Terminal response output is the fallback source for Responses tool billing.
	// Provider UsageEvents can already contain the same charges, so merge by max
	// count per normalized key rather than adding and risking double billing.
	extraBilling, diagnostics := responsesWSTerminalExtraBilling(response, firstResponsesWSImageTracker(imageTrackers))
	usage.ExtraBilling = mergeExtraBillingMapsMax(usage.ExtraBilling, extraBilling)
	for key, billing := range extraBilling {
		usage.MarkProviderExtraBilling(key, billing)
	}
	usage.MergeBillingDiagnostics(diagnostics)
}

func responsesWSTerminalUsageSnapshot(response *types.OpenAIResponsesResponses, imageTrackers ...*commonresponses.ImageGenerationStreamTracker) *types.Usage {
	if response == nil || response.Usage == nil {
		return nil
	}
	// Terminal exact settlement uses the provider terminal usage snapshot as
	// the authority. Accumulated observed usage remains useful for no-terminal
	// settlement and diagnostics, but must not inflate or overwrite exact
	// terminal billing.
	usage := response.Usage.ToOpenAIUsage()
	response.ApplyUsageAttribution(usage)
	extraBilling, diagnostics := responsesWSTerminalExtraBilling(response, firstResponsesWSImageTracker(imageTrackers))
	usage.ExtraBilling = mergeExtraBillingMapsMax(usage.ExtraBilling, extraBilling)
	for key, billing := range extraBilling {
		usage.MarkProviderExtraBilling(key, billing)
	}
	usage.MergeBillingDiagnostics(diagnostics)
	return usage
}

func firstResponsesWSImageTracker(trackers []*commonresponses.ImageGenerationStreamTracker) *commonresponses.ImageGenerationStreamTracker {
	for _, tracker := range trackers {
		if tracker != nil {
			return tracker
		}
	}
	return nil
}

func responsesWSTerminalExtraBilling(response *types.OpenAIResponsesResponses, imageTracker *commonresponses.ImageGenerationStreamTracker) (map[string]types.ExtraBilling, map[string]bool) {
	if imageTracker == nil {
		return types.GetResponsesExtraBilling(response), types.GetResponsesBillingDiagnostics(response)
	}
	usage := &types.Usage{}
	imageTracker.ApplyExtraBilling(response, usage)
	return usage.ExtraBilling, usage.BillingDiagnostics
}

func cloneResponsesWSUsage(usage *types.Usage) *types.Usage {
	if usage == nil {
		return nil
	}
	cloned := &types.Usage{
		PromptTokens:            usage.PromptTokens,
		CompletionTokens:        usage.CompletionTokens,
		TotalTokens:             usage.TotalTokens,
		PromptTokensDetails:     usage.PromptTokensDetails,
		CompletionTokensDetails: usage.CompletionTokensDetails,
		ResponseModel:           usage.ResponseModel,
		ServiceTier:             usage.ServiceTier,
		Speed:                   usage.Speed,
		SpeedConflict:           usage.SpeedConflict,
		ProviderReported:        usage.ProviderReported,
		AttributionConflict:     usage.AttributionConflict,
		ProviderTokenConflict:   usage.ProviderTokenConflict,
		BillingDiagnostics:      make(map[string]bool, len(usage.BillingDiagnostics)),
	}
	if usage.ProviderOperationUnits != nil {
		cloned.ProviderOperationUnits = new(int)
		*cloned.ProviderOperationUnits = *usage.ProviderOperationUnits
	}
	if len(usage.ProviderTokenFields) > 0 {
		cloned.ProviderTokenFields = make(map[string]bool, len(usage.ProviderTokenFields))
		for key, value := range usage.ProviderTokenFields {
			cloned.ProviderTokenFields[key] = value
		}
	}
	cloned.RequiredTokenExtraKeys = append([]string(nil), usage.RequiredTokenExtraKeys...)
	if len(usage.ProviderExtraBilling) > 0 {
		cloned.ProviderExtraBilling = make(map[string]bool, len(usage.ProviderExtraBilling))
		for key, value := range usage.ProviderExtraBilling {
			cloned.ProviderExtraBilling[key] = value
		}
	}
	if len(usage.ProviderIndependentUsageUnits) > 0 {
		cloned.ProviderIndependentUsageUnits = make(map[string]bool, len(usage.ProviderIndependentUsageUnits))
		for key, value := range usage.ProviderIndependentUsageUnits {
			cloned.ProviderIndependentUsageUnits[key] = value
		}
	}
	for key, value := range usage.BillingDiagnostics {
		cloned.BillingDiagnostics[key] = value
	}
	if len(cloned.BillingDiagnostics) == 0 {
		cloned.BillingDiagnostics = nil
	}
	if len(usage.ExtraTokens) > 0 {
		cloned.ExtraTokens = make(map[string]int, len(usage.ExtraTokens))
		for key, value := range usage.ExtraTokens {
			cloned.ExtraTokens[key] = value
		}
	}
	if len(usage.ExtraUsageUnits) > 0 {
		cloned.ExtraUsageUnits = make(map[string]float64, len(usage.ExtraUsageUnits))
		for key, value := range usage.ExtraUsageUnits {
			cloned.ExtraUsageUnits[key] = value
		}
	}
	if len(usage.ExtraBilling) > 0 {
		cloned.ExtraBilling = make(map[string]types.ExtraBilling, len(usage.ExtraBilling))
		for key, value := range usage.ExtraBilling {
			cloned.ExtraBilling[key] = value
		}
	}
	return cloned
}

func mergeResponsesWSAttachedFrameUsage(usage *types.Usage, classified responsesws.ResponsesTerminalResult, eventUsage *types.UsageEvent) {
	if usage == nil || eventUsage == nil {
		return
	}
	eventUsage = relay_util.ProviderUsageEventForBilling(eventUsage)
	// response.usage is an absolute token snapshot. Attached provider usage may
	// carry the same token values, so only merge token deltas when the frame has
	// no authoritative response usage. Provider adapters expose ExtraBilling as
	// increments, so distinct tool calls must be added rather than max-merged.
	if classified.Response == nil || classified.Response.Usage == nil {
		tokens := *eventUsage
		tokens.ExtraBilling = nil
		tokens.BillingDiagnostics = nil
		mergeProjectedResponsesWSUsageEvent(usage, &tokens)
	} else {
		mergeResponsesWSIndependentUsageUnits(usage, eventUsage.ExtraUsageUnits, eventUsage.ProviderIndependentUsageUnits)
	}
	usage.MergeExtraBilling(eventUsage.ExtraBilling)
	if usage.ProviderExtraBilling == nil && len(eventUsage.ProviderExtraBilling) > 0 {
		usage.ProviderExtraBilling = make(map[string]bool)
	}
	for key, present := range eventUsage.ProviderExtraBilling {
		if present {
			usage.ProviderExtraBilling[key] = true
		}
	}
	usage.MergeBillingDiagnostics(eventUsage.BillingDiagnostics)
	usage.AttributionConflict = usage.AttributionConflict || eventUsage.AttributionConflict
	usage.MergeProviderAttribution(eventUsage.ResponseModel, eventUsage.ServiceTier)
	usage.MergeProviderSpeed(eventUsage.Speed, eventUsage.SpeedConflict)
}

func mergeIntMaps(dst map[string]int, src map[string]int) map[string]int {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]int, len(src))
	}
	for key, value := range src {
		dst[key] += value
	}
	return dst
}

func mergeExtraBillingMapsMax(dst map[string]types.ExtraBilling, src map[string]types.ExtraBilling) map[string]types.ExtraBilling {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]types.ExtraBilling, len(src))
	}
	for key, value := range src {
		serviceType := types.ResolveExtraBillingServiceType(key, value)
		bType := types.ResolveExtraBillingType(key, value)
		normalizedKey := types.BuildExtraBillingKey(serviceType, bType)
		if normalizedKey == "" {
			continue
		}
		value.ServiceType = serviceType
		value.Type = bType
		if existing, ok := dst[normalizedKey]; ok && existing.CallCount >= value.CallCount {
			continue
		}
		dst[normalizedKey] = value
	}
	return dst
}

// 明确的 create 续接拒绝发生在绑定 Response 之前；泛化 error 不提供该事实。
func responsesWSExplicitCreateRejection(event responsesws.ResponsesTerminalResult, attempt *ResponsesWSTurnAttempt) bool {
	return event.RequestError && event.ErrorCode == "previous_response_not_found" && attempt != nil && attempt.SeenProviderResponseID == "" && attempt.AttemptedPreviousResponseID != ""
}

func responsesWSResourceLifecycleEvent(eventType string) bool {
	return commonresponses.IsResponseLifecycleEvent(eventType)
}
