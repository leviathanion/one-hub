package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"one-api/common/logger"
	"one-api/types"
)

const codexSupplierMalformedPayloadLogLimit = 4096

func isCodexSupplierBootstrapPayload(payload []byte) bool {
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return false
	}
	return strings.TrimSpace(event.Type) == types.EventTypeSessionCreated
}

func isCodexSupplierTerminalEvent(event *types.OpenAIResponsesStreamResponses) bool {
	if event == nil {
		return false
	}
	return classifyCodexSupplierTerminal(event.Type, event.Response, event.Type == types.EventTypeError).isTerminal()
}

func codexSupplierUsageEvent(response *types.OpenAIResponsesResponses, accumulator *codexTurnUsageAccumulator) *types.UsageEvent {
	if response == nil && accumulator == nil {
		return nil
	}
	if response != nil && response.Usage == nil && strings.TrimSpace(response.Status) == types.ResponseStatusCancelled {
		return nil
	}
	if accumulator == nil {
		accumulator = newCodexTurnUsageAccumulator()
	}
	return accumulator.ResolveUsageEvent(response)
}

func (p *CodexProvider) handleCodexSupplierPayload(message []byte, accumulator *codexTurnUsageAccumulator) (bool, *types.UsageEvent, []byte, error) {
	var event types.OpenAIResponsesStreamResponses
	if err := json.Unmarshal(message, &event); err != nil {
		logger.LogError(context.Background(), "codex supplier message unmarshal failed: "+err.Error()+" payload="+codexSupplierPayloadSnippet(message))
		return true, nil, nil, nil
	}

	if event.Type == types.EventTypeError {
		detail := codexSupplierErrorDetailFromPayload(&event, message)
		logger.SysDebug(codexSupplierErrorLogMessage(detail, message))
		return true, nil, nil, nil
	}

	if accumulator != nil {
		if err := accumulator.ObserveEvent(&event); err != nil {
			return false, accumulator.BillingUsageEvent(), nil, err
		}
		// A completed search is billable as soon as its output item is accepted;
		// do not wait for the response token terminal, which may be absent when
		// the supplier closes, reports an error, or is cancelled. The accumulator
		// returns only the newly observed component delta, so a later terminal
		// snapshot cannot charge the same search again.
		if !isCodexSupplierTerminalEvent(&event) {
			return true, accumulator.BillingUsageEvent(), nil, nil
		}
	}

	if isCodexSupplierTerminalEvent(&event) {
		return true, codexSupplierUsageEvent(event.Response, accumulator), nil, nil
	}

	return true, nil, nil, nil
}

func inspectCodexSupplierPayload(payload []byte) (bool, string, string) {
	var event types.OpenAIResponsesStreamResponses
	if err := json.Unmarshal(payload, &event); err != nil {
		return false, "", ""
	}
	return inspectCodexSupplierEvent(&event)
}

func inspectCodexSupplierEvent(event *types.OpenAIResponsesStreamResponses) (bool, string, string) {
	responseID := ""
	if event != nil && event.Response != nil {
		responseID = strings.TrimSpace(event.Response.ID)
	}
	if event == nil {
		return false, responseID, ""
	}
	if strings.TrimSpace(event.Type) == types.EventTypeError {
		if responseID == "" {
			return false, "", ""
		}
		return true, responseID, types.EventTypeError
	}
	classified := classifyCodexSupplierTerminal(event.Type, event.Response, event.Type == types.EventTypeError)
	if !classified.isTerminal() {
		return false, responseID, ""
	}
	return true, responseID, codexSupplierTerminationReason(event.Type, event.Response)
}

func codexSupplierTerminationReason(eventType string, response *types.OpenAIResponsesResponses) string {
	if response != nil {
		if status := strings.TrimSpace(response.Status); status != "" {
			return "response." + status
		}
	}
	if trimmed := strings.TrimSpace(eventType); trimmed != "" {
		return trimmed
	}
	return "response.completed"
}

type codexSupplierErrorDetail struct {
	Type       string
	Code       string
	Message    string
	Param      string
	Status     int
	ResponseID string
}

type codexSupplierErrorPayload struct {
	Type       *string                         `json:"type,omitempty"`
	Status     int                             `json:"status,omitempty"`
	StatusCode int                             `json:"status_code,omitempty"`
	Code       *string                         `json:"code,omitempty"`
	Message    *string                         `json:"message,omitempty"`
	Param      any                             `json:"param,omitempty"`
	Error      *types.OpenAIError              `json:"error,omitempty"`
	Response   *types.OpenAIResponsesResponses `json:"response,omitempty"`
}

func codexSupplierErrorDetailFromPayload(event *types.OpenAIResponsesStreamResponses, payload []byte) codexSupplierErrorDetail {
	detail := codexSupplierErrorDetail{Type: "provider_error", Message: "provider websocket error"}
	if event != nil {
		if event.Response != nil {
			detail.ResponseID = strings.TrimSpace(event.Response.ID)
			applyCodexSupplierOpenAIErrorDetail(&detail, event.Response.Error)
		}
		if event.Code != nil {
			if code := strings.TrimSpace(*event.Code); code != "" {
				detail.Code = code
			}
		}
		if event.Message != nil {
			if message := strings.TrimSpace(*event.Message); message != "" {
				detail.Message = message
			}
		}
		if event.Param != nil {
			if param := codexSupplierAnyString(*event.Param); param != "" {
				detail.Param = param
			}
		}
	}

	var wire codexSupplierErrorPayload
	if len(payload) > 0 && json.Unmarshal(payload, &wire) == nil {
		if wire.Status > 0 {
			detail.Status = wire.Status
		} else if wire.StatusCode > 0 {
			detail.Status = wire.StatusCode
		}
		if wire.Type != nil {
			if errType := strings.TrimSpace(*wire.Type); errType != "" && errType != types.EventTypeError {
				detail.Type = errType
			}
		}
		applyCodexSupplierOpenAIErrorDetail(&detail, wire.Error)
		if wire.Response != nil {
			if responseID := strings.TrimSpace(wire.Response.ID); responseID != "" {
				detail.ResponseID = responseID
			}
			applyCodexSupplierOpenAIErrorDetail(&detail, wire.Response.Error)
		}
		if wire.Code != nil {
			if code := strings.TrimSpace(*wire.Code); code != "" {
				detail.Code = code
			}
		}
		if wire.Message != nil {
			if message := strings.TrimSpace(*wire.Message); message != "" {
				detail.Message = message
			}
		}
		if param := codexSupplierAnyString(wire.Param); param != "" {
			detail.Param = param
		}
	}

	if strings.TrimSpace(detail.Type) == "" {
		detail.Type = "provider_error"
	}
	if strings.TrimSpace(detail.Code) == "" || (detail.Code == "provider_error" && detail.Type != "provider_error") {
		detail.Code = detail.Type
	}
	if strings.TrimSpace(detail.Code) == "" {
		detail.Code = "provider_error"
	}
	if strings.TrimSpace(detail.Message) == "" {
		detail.Message = "provider websocket error"
	}
	return detail
}

func applyCodexSupplierOpenAIErrorDetail(detail *codexSupplierErrorDetail, openAIError *types.OpenAIError) {
	if detail == nil || openAIError == nil {
		return
	}
	if errType := strings.TrimSpace(openAIError.Type); errType != "" {
		detail.Type = errType
	}
	if code := codexRealtimeErrorCodeString(openAIError.Code, ""); code != "" {
		detail.Code = code
	}
	if message := strings.TrimSpace(openAIError.Message); message != "" {
		detail.Message = message
	}
	if param := strings.TrimSpace(openAIError.Param); param != "" {
		detail.Param = param
	}
}

func codexSupplierAnyString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func codexSupplierErrorLogMessage(detail codexSupplierErrorDetail, payload []byte) string {
	return fmt.Sprintf(
		"codex supplier error: type=%s code=%s status=%d message=%s param=%s response_id=%s payload=%s",
		codexRealtimeLogValue(detail.Type),
		codexRealtimeLogValue(detail.Code),
		detail.Status,
		codexRealtimeLogValue(detail.Message),
		codexRealtimeLogValue(detail.Param),
		codexRealtimeLogValue(detail.ResponseID),
		codexRealtimeLogValue(codexSupplierPayloadSnippet(payload)),
	)
}

func codexSupplierPayloadSnippet(payload []byte) string {
	if len(payload) <= codexSupplierMalformedPayloadLogLimit {
		return string(payload)
	}
	return string(payload[:codexSupplierMalformedPayloadLogLimit]) + "...(truncated)"
}
