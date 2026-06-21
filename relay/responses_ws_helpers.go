package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"one-api/common/logger"
	"one-api/common/responsesws"
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
	return responsesWSRedactAndLimitDiagnostic(err.Error())
}

func responsesWSRedactAndLimitDiagnostic(message string) string {
	return responsesWSSafeDiagnosticValue(responsesws.RedactSensitiveText(message))
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
	return responsesWSErrorPayloadWithParam(http.StatusConflict, "previous_response_not_found", responsesWSStaticErrorMessage("previous_response_not_found"), "previous_response_id")
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
	if !apiErr.LocalError && strings.TrimSpace(apiErr.Message) != "" && apiErr.StatusCode < http.StatusInternalServerError {
		return apiErr.Message
	}
	return responsesWSStaticErrorMessage(code)
}

func responsesWSClientParamFromOpenAI(apiErr *types.OpenAIErrorWithStatusCode) string {
	if apiErr == nil || apiErr.LocalError {
		return ""
	}
	param := strings.TrimSpace(apiErr.Param)
	if param == "" {
		return ""
	}
	switch param {
	case "model", "input", "instructions", "tools", "tool_choice", "temperature", "top_p", "max_output_tokens", "previous_response_id", "metadata", "stream":
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
	logger.LogError(logCtx, "responses websocket upstream error: "+err.Error())
	return responsesWSErrorPayload(http.StatusBadGateway, "upstream_error", responsesWSStaticErrorMessage("upstream_error"))
}

func isResponsesWSBridgeOpenProviderError(err error) bool {
	var bridgeErr *responsesws.BridgeOpenProviderError
	return errors.As(err, &bridgeErr)
}

func responsesWSBridgeOpenProviderAPIError(err error) *types.OpenAIErrorWithStatusCode {
	var bridgeErr *responsesws.BridgeOpenProviderError
	if !errors.As(err, &bridgeErr) || bridgeErr == nil {
		return nil
	}
	status := bridgeErr.StatusCode
	if status <= 0 {
		status = http.StatusBadGateway
	}
	// Trade-off: carry the typed provider rejection through the actor instead of
	// reparsing the safe client payload. This preserves HTTP status for metrics
	// and auto-disable while keeping the websocket payload redacted.
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Type:    bridgeErr.Type,
			Code:    bridgeErr.Code,
			Message: bridgeErr.Message,
		},
		StatusCode: status,
		LocalError: false,
	}
}

func responsesWSBridgeOpenProviderContinuationMiss(event ResponsesWSEventBridgeOpenProviderError) bool {
	if event.ProviderAPIError != nil {
		code := openAIErrorCodeString(event.ProviderAPIError.Code, "")
		if code == "previous_response_not_found" {
			return true
		}
		message := strings.ToLower(strings.TrimSpace(event.ProviderAPIError.Message))
		if strings.Contains(message, "previous_response_not_found") ||
			(strings.Contains(message, "previous response") && strings.Contains(message, "not found")) {
			return true
		}
	}
	if len(event.Payload) == 0 {
		return false
	}
	classified := responsesws.ClassifyResponsesWSEvent(event.Payload)
	if classified.ContinuationMiss {
		return true
	}
	return strings.Contains(strings.ToLower(string(event.Payload)), "previous_response_not_found")
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
	usage.PromptTokens += event.InputTokens
	usage.CompletionTokens += event.OutputTokens
	usage.TotalTokens += event.TotalTokens
	usage.PromptTokensDetails.Merge(&event.InputTokenDetails)
	usage.CompletionTokensDetails.Merge(&event.OutputTokenDetails)
	usage.ExtraTokens = mergeIntMaps(usage.ExtraTokens, event.ExtraTokens)
	usage.MergeExtraBilling(event.ExtraBilling)
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
		overwritePositiveInt(&usage.PromptTokensDetails.CachedReadTokens, responseUsage.InputTokensDetails.CachedReadTokens)
		overwritePositiveInt(&usage.PromptTokensDetails.CachedWriteTokens, responseUsage.InputTokensDetails.CachedWriteTokens)
		overwritePositiveInt(&usage.PromptTokensDetails.TextTokens, responseUsage.InputTokensDetails.TextTokens)
		overwritePositiveInt(&usage.PromptTokensDetails.ImageTokens, responseUsage.InputTokensDetails.ImageTokens)
	}
	if responseUsage.OutputTokensDetails != nil {
		overwritePositiveInt(&usage.CompletionTokensDetails.ReasoningTokens, responseUsage.OutputTokensDetails.ReasoningTokens)
	}
}

func overwritePositiveInt(dst *int, src int) {
	if dst != nil && src > 0 {
		*dst = src
	}
}

func mergeResponsesWSTerminalResponse(usage *types.Usage, response *types.OpenAIResponsesResponses) {
	if usage == nil || response == nil {
		return
	}
	mergeResponsesWSResponsesUsage(usage, response.Usage)
	// Terminal response output is the fallback source for Responses tool billing.
	// Provider UsageEvents can already contain the same charges, so merge by max
	// count per normalized key rather than adding and risking double billing.
	usage.ExtraBilling = mergeExtraBillingMapsMax(usage.ExtraBilling, types.GetResponsesExtraBilling(response))
}

func responsesWSTerminalUsageSnapshot(response *types.OpenAIResponsesResponses) *types.Usage {
	if response == nil || response.Usage == nil {
		return nil
	}
	// Terminal exact settlement uses the provider terminal usage snapshot as
	// the authority. Accumulated observed usage remains useful for no-terminal
	// settlement and diagnostics, but must not inflate or overwrite exact
	// terminal billing.
	usage := response.Usage.ToOpenAIUsage()
	usage.ExtraBilling = mergeExtraBillingMapsMax(usage.ExtraBilling, types.GetResponsesExtraBilling(response))
	return usage
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
	}
	if len(usage.ExtraTokens) > 0 {
		cloned.ExtraTokens = make(map[string]int, len(usage.ExtraTokens))
		for key, value := range usage.ExtraTokens {
			cloned.ExtraTokens[key] = value
		}
	}
	if len(usage.ExtraBilling) > 0 {
		cloned.ExtraBilling = make(map[string]types.ExtraBilling, len(usage.ExtraBilling))
		for key, value := range usage.ExtraBilling {
			cloned.ExtraBilling[key] = value
		}
	}
	if usage.TextBuilder.Len() > 0 {
		cloned.TextBuilder.WriteString(usage.TextBuilder.String())
	}
	return cloned
}

func responsesWSShouldMergeAttachedFrameUsage(classified responsesws.ResponsesTerminalResult, eventUsage *types.UsageEvent) bool {
	if eventUsage == nil {
		return false
	}
	// Native adapters and the HTTP bridge may attach Usage extracted from the
	// same provider frame that still carries response.usage. response.usage is
	// an absolute snapshot, not a delta, so adding the attached copy would bill
	// the same terminal frame twice.
	return classified.Response == nil || classified.Response.Usage == nil
}

func responsesWSUsageHasBillableEvidence(usage *types.Usage) bool {
	if usage == nil {
		return false
	}
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.TotalTokens > 0 {
		return true
	}
	if len(usage.GetExtraTokens()) > 0 {
		return true
	}
	for _, extra := range usage.ExtraBilling {
		if extra.CallCount > 0 {
			return true
		}
	}
	return false
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
