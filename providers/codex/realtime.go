package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/types"
)

const codexResponsesWebsocketBetaHeaderValue = "responses_websockets=2026-02-06"
const codexRealtimeMalformedPayloadLogLimit = 4096
const codexRealtimeDiagnosticValueLogLimit = 4096

type codexRealtimeConnPlan struct {
	wsURL   string
	headers map[string]string
}

func (p *CodexProvider) createChatRealtimeConn(modelName, sessionID string) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	return p.createChatRealtimeConnWithContext(context.Background(), modelName, sessionID)
}

func (p *CodexProvider) createChatRealtimeConnWithContext(ctx context.Context, modelName, sessionID string) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
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

func (p *CodexProvider) dialChatRealtimeConn(plan *codexRealtimeConnPlan) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	return p.dialChatRealtimeConnWithContext(context.Background(), plan)
}

func (p *CodexProvider) dialChatRealtimeConnWithContext(ctx context.Context, plan *codexRealtimeConnPlan) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	if plan == nil {
		return nil, common.StringErrorWrapperLocal("realtime websocket plan is required", "ws_request_failed", http.StatusInternalServerError)
	}

	wsConn, err := wsconn.DialManaged(ctx, plan.wsURL, codexRealtimeHTTPHeader(plan.headers), codexRealtimeWSConfig(), codexRealtimeDialOptions(channelProxyValue(p.Channel), p.codexRealtimeSelfHosted())...)
	if err != nil {
		return nil, mapCodexRealtimeWSDialError(err)
	}

	return wsConn, nil
}

func codexRealtimeHTTPHeader(headers map[string]string) http.Header {
	httpHeaders := make(http.Header, len(headers))
	for key, value := range headers {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		httpHeaders.Set(key, value)
	}
	return httpHeaders
}

func codexRealtimeWSConfig() wsconn.Config {
	return wsconn.Config{
		Label:           "codex realtime upstream",
		PingInterval:    config.RealtimeWebsocketPingInterval(),
		PongMissTimeout: config.RealtimeWebsocketClientPongMissTimeout(),
		InboundActivityTimeout: func() time.Duration {
			return config.RealtimeWebsocketClientInboundActivityTimeout()
		},
		ReadLimit:    config.RealtimeWebsocketReadLimit(),
		WriteTimeout: config.RealtimeWebsocketWriteTimeout,
	}
}

func codexRealtimeDialOptions(proxyAddr string, allowSelfHosted bool) []wsconn.DialOption {
	policy := wsconn.DialSecurityPolicy{
		AllowInsecureWS: allowSelfHosted,
		AllowPrivateIP:  allowSelfHosted,
	}
	if allowSelfHosted {
		policy.HostFilter = func(host string, ips []net.IP) bool {
			return true
		}
	}
	options := []wsconn.DialOption{wsconn.WithDialSecurityPolicy(policy)}
	if strings.TrimSpace(proxyAddr) != "" {
		options = append(options, wsconn.WithProxyURL(proxyAddr))
	}
	return options
}

func mapCodexRealtimeWSDialError(err error) *types.OpenAIErrorWithStatusCode {
	logCodexRealtimeWSDialFailure(err)

	var dialErr *wsconn.DialError
	if errors.As(err, &dialErr) && dialErr != nil {
		return mapCodexRealtimeWSDialStatus(dialErr.StatusCode)
	}
	return common.StringErrorWrapperLocal("websocket request failed", "ws_request_failed", http.StatusInternalServerError)
}

func mapCodexRealtimeWSDialStatus(statusCode int) *types.OpenAIErrorWithStatusCode {
	switch statusCode {
	case http.StatusNotFound, http.StatusUpgradeRequired:
		return common.StringErrorWrapperLocal("channel does not support Responses websocket transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
	case http.StatusUnauthorized, http.StatusForbidden:
		return common.StringErrorWrapperLocal("provider authentication failed", "provider_authentication_failed", statusCode)
	case http.StatusTooManyRequests:
		return common.StringErrorWrapperLocal("provider rate limit exceeded", "provider_rate_limit_exceeded", http.StatusTooManyRequests)
	default:
		if statusCode >= 500 {
			return common.StringErrorWrapperLocal("provider websocket request failed", "provider_ws_request_failed", statusCode)
		}
	}
	return common.StringErrorWrapperLocal("websocket request failed", "ws_request_failed", http.StatusInternalServerError)
}

func logCodexRealtimeWSDialFailure(err error) {
	if err == nil {
		return
	}
	message := codexRealtimeWSDialFailureLogMessage(err)
	if logger.Logger != nil {
		logger.LogError(context.Background(), message)
		return
	}
	log.Printf("%s", message)
}

func codexRealtimeWSDialFailureLogMessage(err error) string {
	var dialErr *wsconn.DialError
	if !errors.As(err, &dialErr) || dialErr == nil {
		return "codex realtime websocket dial failed: cause=" + codexRealtimeLogValue(err.Error())
	}

	body := codexRealtimeLogValue(string(dialErr.BodySnippet))
	if body == "" && dialErr.BodyReadErr != nil {
		body = "body_read_failed:" + codexRealtimeLogValue(dialErr.BodyReadErr.Error())
	}
	return fmt.Sprintf(
		"codex realtime websocket dial failed: status=%d url=%s server=%s via=%s cf_ray=%s x_request_id=%s openai_request_id=%s retry_after=%s body_truncated=%v body=%s cause=%s",
		dialErr.StatusCode,
		codexRealtimeWSURLForLog(dialErr.URL),
		codexRealtimeHeaderForLog(dialErr.Header, "server"),
		codexRealtimeHeaderForLog(dialErr.Header, "via"),
		codexRealtimeHeaderForLog(dialErr.Header, "cf-ray"),
		codexRealtimeHeaderForLog(dialErr.Header, "x-request-id"),
		codexRealtimeHeaderForLog(dialErr.Header, "x-openai-request-id"),
		codexRealtimeHeaderForLog(dialErr.Header, "retry-after"),
		dialErr.BodyTruncated,
		body,
		codexRealtimeLogValue(fmt.Sprint(dialErr.Err)),
	)
}

func codexRealtimeWSURLForLog(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return codexRealtimeLogValue(trimmed)
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return codexRealtimeLogValue(parsed.String())
}

func codexRealtimeHeaderForLog(header http.Header, key string) string {
	if header == nil {
		return ""
	}
	return codexRealtimeLogValue(header.Get(key))
}

func codexRealtimeLogValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.NewReplacer("\r", "\\r", "\n", "\\n", "\t", "\\t").Replace(trimmed)
	if len(trimmed) <= codexRealtimeDiagnosticValueLogLimit {
		return trimmed
	}
	return trimmed[:codexRealtimeDiagnosticValueLogLimit] + "...(truncated)"
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

func (p *CodexProvider) handleRealtimeSupplierMessage(messageType wsconn.MessageType, message []byte, accumulator *codexTurnUsageAccumulator, modelName string) (bool, *types.UsageEvent, []byte, error) {
	if messageType != wsconn.TextMessage {
		return true, nil, nil, nil
	}

	var event types.OpenAIResponsesStreamResponses
	if err := json.Unmarshal(message, &event); err != nil {
		logger.LogError(context.Background(), "codex realtime supplier message unmarshal failed: "+err.Error()+" payload="+codexRealtimePayloadSnippet(message))
		return true, nil, nil, nil
	}

	if event.Type == "error" {
		detail := codexRealtimeProviderErrorDetailFromPayload(&event, message)
		logger.SysDebug(codexRealtimeProviderErrorLogMessage(detail, message))
		return true, nil, nil, nil
	}

	if accumulator != nil {
		accumulator.ObserveEvent(&event)
	}

	if isCodexRealtimeTerminalEvent(&event) {
		return true, codexRealtimeUsageEvent(event.Response, accumulator, modelName), nil, nil
	}

	return true, nil, nil, nil
}

type codexRealtimeProviderErrorDetail struct {
	Type       string
	Code       string
	Message    string
	Param      string
	Status     int
	ResponseID string
}

type codexRealtimeProviderErrorPayload struct {
	Type       *string                         `json:"type,omitempty"`
	Status     int                             `json:"status,omitempty"`
	StatusCode int                             `json:"status_code,omitempty"`
	Code       *string                         `json:"code,omitempty"`
	Message    *string                         `json:"message,omitempty"`
	Param      any                             `json:"param,omitempty"`
	Error      *types.OpenAIError              `json:"error,omitempty"`
	Response   *types.OpenAIResponsesResponses `json:"response,omitempty"`
}

func codexRealtimeProviderErrorDetailFromPayload(event *types.OpenAIResponsesStreamResponses, payload []byte) codexRealtimeProviderErrorDetail {
	detail := codexRealtimeProviderErrorDetail{
		Type:    "provider_error",
		Code:    "",
		Message: "provider websocket error",
	}
	if event != nil {
		if event.Response != nil {
			detail.ResponseID = strings.TrimSpace(event.Response.ID)
			applyCodexRealtimeOpenAIErrorDetail(&detail, event.Response.Error)
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
			if param := codexRealtimeAnyString(*event.Param); param != "" {
				detail.Param = param
			}
		}
	}

	var wire codexRealtimeProviderErrorPayload
	if len(payload) > 0 && json.Unmarshal(payload, &wire) == nil {
		if wire.Status > 0 {
			detail.Status = wire.Status
		} else if wire.StatusCode > 0 {
			detail.Status = wire.StatusCode
		}
		if wire.Type != nil {
			if errType := strings.TrimSpace(*wire.Type); errType != "" && errType != "error" {
				detail.Type = errType
			}
		}
		applyCodexRealtimeOpenAIErrorDetail(&detail, wire.Error)
		if wire.Response != nil {
			if responseID := strings.TrimSpace(wire.Response.ID); responseID != "" {
				detail.ResponseID = responseID
			}
			applyCodexRealtimeOpenAIErrorDetail(&detail, wire.Response.Error)
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
		if param := codexRealtimeAnyString(wire.Param); param != "" {
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

func applyCodexRealtimeOpenAIErrorDetail(detail *codexRealtimeProviderErrorDetail, openAIError *types.OpenAIError) {
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

func codexRealtimeAnyString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func codexRealtimeProviderErrorLogMessage(detail codexRealtimeProviderErrorDetail, payload []byte) string {
	return fmt.Sprintf(
		"codex realtime error: type=%s code=%s status=%d message=%s param=%s response_id=%s payload=%s",
		codexRealtimeLogValue(detail.Type),
		codexRealtimeLogValue(detail.Code),
		detail.Status,
		codexRealtimeLogValue(detail.Message),
		codexRealtimeLogValue(detail.Param),
		codexRealtimeLogValue(detail.ResponseID),
		codexRealtimeLogValue(codexRealtimePayloadSnippet(payload)),
	)
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
