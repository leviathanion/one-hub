package codex

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/types"

	"github.com/gorilla/websocket"
)

const codexResponsesWebsocketBetaHeaderValue = "responses_websockets=2026-02-06"

type codexRealtimeConnPlan struct {
	wsURL   string
	headers map[string]string
}

func (p *CodexProvider) createChatRealtimeConn(modelName, sessionID string) (*websocket.Conn, *types.OpenAIErrorWithStatusCode) {
	plan, errWithCode := p.prepareChatRealtimeConn(modelName, sessionID)
	if errWithCode != nil {
		return nil, errWithCode
	}
	return p.dialChatRealtimeConn(plan)
}

func (p *CodexProvider) prepareChatRealtimeConn(modelName, sessionID string) (*codexRealtimeConnPlan, *types.OpenAIErrorWithStatusCode) {
	urlPath, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatRealtime)
	if errWithCode != nil {
		return nil, errWithCode
	}

	httpURL := p.GetFullRequestURL(urlPath, modelName)
	wsURL, err := buildCodexRealtimeURL(httpURL)
	if err != nil {
		return nil, common.ErrorWrapper(err, "ws_request_failed", http.StatusInternalServerError)
	}

	headers, err := p.getRealtimeHeaders(sessionID)
	if err != nil {
		return nil, p.handleTokenError(err)
	}

	return &codexRealtimeConnPlan{
		wsURL:   wsURL,
		headers: headers,
	}, nil
}

func (p *CodexProvider) dialChatRealtimeConn(plan *codexRealtimeConnPlan) (*websocket.Conn, *types.OpenAIErrorWithStatusCode) {
	if plan == nil {
		return nil, common.StringErrorWrapperLocal("realtime websocket plan is required", "ws_request_failed", http.StatusInternalServerError)
	}

	wsRequester := requester.NewWSRequester(channelProxyValue(p.Channel))
	wsConn, err := wsRequester.NewRequest(plan.wsURL, wsRequester.WithHeader(plan.headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "ws_request_failed", http.StatusInternalServerError)
	}

	return wsConn, nil
}

func buildCodexRealtimeURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	}
	return parsed.String(), nil
}

var codexRealtimeCompatibilityHeaderKeys = []string{
	"version",
	"originator",
	"x-codex-turn-state",
	"x-responsesapi-include-timing-metrics",
	"x-codex-beta-features",
}

var codexRealtimeRequestOverrideHeaderKeys = []string{
	"x-codex-beta-features",
	"x-codex-turn-state",
	"x-responsesapi-include-timing-metrics",
}

func (p *CodexProvider) getRealtimeHeaders(sessionID string) (map[string]string, error) {
	headers, err := p.getRequestHeaderBag()
	if err != nil {
		return nil, err
	}

	applyCodexExecutionSessionHeader(headers, resolveCodexExecutionSessionID(headers, sessionID))
	p.applyDefaultHeaders(headers)
	headers.Delete("Connection")
	headers.Delete("Accept")
	p.applyRealtimeRequestHeaderOverrides(headers)
	headers.Set("OpenAI-Beta", codexResponsesWebsocketBetaHeaderValue)
	return headers.Map(), nil
}

func (p *CodexProvider) buildRealtimeRequestCompatibilityHeaders() map[string]string {
	headers := make(map[string]string)
	if p == nil || p.Context == nil || p.Context.Request == nil {
		return headers
	}

	for _, key := range codexRealtimeCompatibilityHeaderKeys {
		if value := p.getPassthroughRealtimeHeader(key); value != "" {
			headers[strings.ToLower(strings.TrimSpace(key))] = value
		}
	}

	return headers
}

func (p *CodexProvider) applyRealtimeRequestHeaderOverrides(headers *codexHeaderBag) {
	for _, key := range codexRealtimeRequestOverrideHeaderKeys {
		if value := p.getPassthroughRealtimeHeader(key); value != "" {
			headers.Set(key, value)
		}
	}
}

func resolveCodexExecutionSessionID(headers *codexHeaderBag, sessionID string) string {
	if trimmed := strings.TrimSpace(sessionID); trimmed != "" {
		return trimmed
	}
	if value := headers.Get("x-session-id"); value != "" {
		return value
	}
	return headers.Get("session_id")
}

func applyCodexExecutionSessionHeader(headers *codexHeaderBag, sessionID string) {
	if headers == nil {
		return
	}

	trimmed := strings.TrimSpace(sessionID)
	if trimmed == "" {
		return
	}

	if !headers.Has("session_id") {
		headers.Set("session_id", trimmed)
	}
	if !headers.Has("x-session-id") {
		headers.Set("x-session-id", trimmed)
	}
}

func isCodexRealtimeTerminalStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case types.ResponseStatusCompleted, types.ResponseStatusFailed, types.ResponseStatusIncomplete, types.ResponseStatusCancelled:
		return true
	default:
		return false
	}
}

func isCodexRealtimeTerminalEvent(event *types.OpenAIResponsesStreamResponses) bool {
	if event == nil {
		return false
	}

	switch strings.TrimSpace(event.Type) {
	case "response.completed", "response.failed", "response.incomplete", types.EventTypeResponseDone:
		return true
	}

	return event.Response != nil && isCodexRealtimeTerminalStatus(event.Response.Status)
}

func codexRealtimeUsageEvent(response *types.OpenAIResponsesResponses, accumulator *codexTurnUsageAccumulator, modelName string) *types.UsageEvent {
	if response == nil && accumulator == nil {
		return nil
	}
	if response != nil && response.Usage == nil && strings.TrimSpace(response.Status) == types.ResponseStatusCancelled {
		return nil
	}
	if accumulator == nil {
		accumulator = newCodexTurnUsageAccumulator()
	}
	return accumulator.ResolveUsageEvent(response, modelName, true)
}

func (p *CodexProvider) handleRealtimeSupplierMessage(messageType int, message []byte, accumulator *codexTurnUsageAccumulator, modelName string) (bool, *types.UsageEvent, []byte, error) {
	if messageType != websocket.TextMessage {
		return true, nil, nil, nil
	}

	var event types.OpenAIResponsesStreamResponses
	if err := json.Unmarshal(message, &event); err != nil {
		return true, nil, nil, nil
	}

	if event.Type == "error" {
		code := "provider_error"
		if event.Code != nil && strings.TrimSpace(*event.Code) != "" {
			code = strings.TrimSpace(*event.Code)
		}
		messageText := "provider websocket error"
		if event.Message != nil && strings.TrimSpace(*event.Message) != "" {
			messageText = strings.TrimSpace(*event.Message)
		}
		logger.SysError("codex realtime error: " + messageText)
		return false, nil, nil, types.NewErrorEvent("", "provider_error", code, messageText)
	}

	if accumulator != nil {
		accumulator.ObserveEvent(&event)
	}

	if isCodexRealtimeTerminalEvent(&event) {
		return true, codexRealtimeUsageEvent(event.Response, accumulator, modelName), nil, nil
	}

	return true, nil, nil, nil
}

func (p *CodexProvider) getPassthroughRealtimeHeader(key string) string {
	if p == nil || p.Context == nil || p.Context.Request == nil {
		return ""
	}
	return strings.TrimSpace(p.Context.Request.Header.Get(key))
}
