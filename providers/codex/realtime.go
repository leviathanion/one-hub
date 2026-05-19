package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/common/responsesws"
	"one-api/types"

	"github.com/gorilla/websocket"
)

const codexResponsesWebsocketBetaHeaderValue = "responses_websockets=2026-02-06"
const codexRealtimeMalformedPayloadLogLimit = 4096

type codexRealtimeConnPlan struct {
	wsURL   string
	headers map[string]string
}

func (p *CodexProvider) createChatRealtimeConn(modelName, sessionID string) (*websocket.Conn, *types.OpenAIErrorWithStatusCode) {
	return p.createChatRealtimeConnWithContext(context.Background(), modelName, sessionID)
}

func (p *CodexProvider) createChatRealtimeConnWithContext(ctx context.Context, modelName, sessionID string) (*websocket.Conn, *types.OpenAIErrorWithStatusCode) {
	plan, errWithCode := p.prepareChatRealtimeConn(modelName, sessionID)
	if errWithCode != nil {
		return nil, errWithCode
	}
	return p.dialChatRealtimeConnWithContext(ctx, plan)
}

func (p *CodexProvider) prepareChatRealtimeConn(modelName, sessionID string) (*codexRealtimeConnPlan, *types.OpenAIErrorWithStatusCode) {
	urlPath, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatRealtime)
	if errWithCode != nil {
		return nil, errWithCode
	}

	httpURL := p.GetFullRequestURL(urlPath, modelName)
	proxyAddr := channelProxyValue(p.Channel)
	wsURL, err := buildCodexRealtimeURLWithPolicy(httpURL, p.codexRealtimeSelfHosted(), proxyAddr == "")
	if err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "ws_request_failed", requester.UpstreamRealtimeURLStatusCode(err))
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
	return p.dialChatRealtimeConnWithContext(context.Background(), plan)
}

func (p *CodexProvider) dialChatRealtimeConnWithContext(ctx context.Context, plan *codexRealtimeConnPlan) (*websocket.Conn, *types.OpenAIErrorWithStatusCode) {
	if plan == nil {
		return nil, common.StringErrorWrapperLocal("realtime websocket plan is required", "ws_request_failed", http.StatusInternalServerError)
	}

	wsRequester := requester.NewWSRequester(channelProxyValue(p.Channel))
	wsConn, err := wsRequester.NewRequestContext(ctx, plan.wsURL, wsRequester.WithHeader(plan.headers))
	if err != nil {
		return nil, mapCodexRealtimeWSDialError(err)
	}

	return wsConn, nil
}

func mapCodexRealtimeWSDialError(err error) *types.OpenAIErrorWithStatusCode {
	var handshake *requester.WSDialHandshakeError
	if errors.As(err, &handshake) && handshake != nil {
		switch handshake.StatusCode {
		case http.StatusNotFound, http.StatusUpgradeRequired:
			return common.StringErrorWrapperLocal("channel does not support Responses websocket transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
		case http.StatusUnauthorized, http.StatusForbidden:
			return common.StringErrorWrapperLocal("provider authentication failed", "provider_authentication_failed", handshake.StatusCode)
		case http.StatusTooManyRequests:
			return common.StringErrorWrapperLocal("provider rate limit exceeded", "provider_rate_limit_exceeded", http.StatusTooManyRequests)
		default:
			if handshake.StatusCode >= 500 {
				return common.StringErrorWrapperLocal("provider websocket request failed", "provider_ws_request_failed", handshake.StatusCode)
			}
		}
	}
	logger.LogError(context.Background(), "codex realtime websocket dial failed: "+err.Error())
	return common.StringErrorWrapperLocal("websocket request failed", "ws_request_failed", http.StatusInternalServerError)
}

func buildCodexRealtimeURL(httpURL string) (string, error) {
	return buildCodexRealtimeURLWithPolicy(httpURL, false, false)
}

func buildCodexRealtimeURLWithPolicy(httpURL string, allowSelfHosted bool, resolveHost bool) (string, error) {
	return requester.ValidateUpstreamRealtimeURL(httpURL, requester.UpstreamRealtimeURLPolicy{
		AllowSelfHosted: allowSelfHosted,
		ResolveHost:     resolveHost,
	})
}

func (p *CodexProvider) codexRealtimeSelfHosted() bool {
	if p == nil {
		return false
	}
	if p.Context != nil && p.Context.GetBool("responses_ws_self_hosted") {
		return true
	}
	if p.Channel == nil {
		return false
	}
	other, err := p.Channel.GetOtherMap()
	if err != nil {
		return false
	}
	for _, key := range []string{"responses_ws_self_hosted", "self_hosted"} {
		if raw, ok := other[key]; ok && strings.EqualFold(strings.TrimSpace(string(raw)), "true") {
			return true
		}
	}
	return false
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
	classified := responsesws.ClassifyResponsesWSTerminalStatus(types.EventTypeResponseDone, status, false, false, "")
	return classified.Kind != responsesws.ResponsesNonTerminal
}

func isCodexRealtimeTerminalEvent(event *types.OpenAIResponsesStreamResponses) bool {
	if event == nil {
		return false
	}
	if responsesws.ClassifyResponsesWSTerminal(event.Type, event.Response, event.Type == "error").Kind != responsesws.ResponsesNonTerminal {
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
		logger.LogError(context.Background(), "codex realtime supplier message unmarshal failed: "+err.Error()+" payload="+codexRealtimePayloadSnippet(message))
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

func codexRealtimePayloadSnippet(payload []byte) string {
	if len(payload) <= codexRealtimeMalformedPayloadLogLimit {
		return string(payload)
	}
	return string(payload[:codexRealtimeMalformedPayloadLogLimit]) + "...(truncated)"
}

func (p *CodexProvider) getPassthroughRealtimeHeader(key string) string {
	if p == nil || p.Context == nil || p.Context.Request == nil {
		return ""
	}
	return strings.TrimSpace(p.Context.Request.Header.Get(key))
}
