package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"one-api/common/logger"
	"one-api/common/responsesws"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
)

func responsesWSSendOutcomeFromError(err error) ResponsesWSSendOutcome {
	if err == nil {
		return SendOutcomeLocalWriteOK
	}
	if errors.Is(err, runtimesession.ErrSessionClosed) || errors.Is(err, runtimesession.ErrStaleResponsesWSContinuation) {
		return SendOutcomeNotSent
	}
	var event *types.Event
	if errors.As(err, &event) && event != nil && event.IsError() {
		code := openAIErrorCodeString(event.ErrorDetail.Code, "")
		switch code {
		case "previous_response_not_found", "invalid_event", "responses_ws_unsupported_for_channel":
			return SendOutcomeNotSent
		default:
			return SendOutcomeAmbiguous
		}
	}
	return SendOutcomeAmbiguous
}

func responsesWSErrorPayload(status int, code string, message string) []byte {
	return responsesWSErrorPayloadWithParam(status, code, message, "")
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
	encoded, _ := json.Marshal(payload)
	return encoded
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
	encoded, _ := json.Marshal(payload)
	return encoded
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
	if payload := runtimesession.ClientPayloadFromError(err); len(payload) > 0 {
		return payload
	}
	if errors.Is(err, runtimesession.ErrStaleResponsesWSContinuation) {
		return responsesWSPreviousResponseNotFoundPayload()
	}
	var event *types.Event
	if errors.As(err, &event) && event != nil && event.IsError() {
		code := openAIErrorCodeString(event.ErrorDetail.Code, "upstream_error")
		message := responsesWSStaticErrorMessage(code)
		return responsesWSErrorPayload(http.StatusBadGateway, code, message)
	}
	logger.LogError(context.Background(), "responses websocket upstream error: "+err.Error())
	return responsesWSErrorPayload(http.StatusBadGateway, "upstream_error", responsesWSStaticErrorMessage("upstream_error"))
}

func responsesWSStaticErrorMessage(code string) string {
	switch strings.TrimSpace(code) {
	case "invalid_event":
		return "invalid response.create event"
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

func isResponsesContinuationMissError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, runtimesession.ErrStaleResponsesWSContinuation) {
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
