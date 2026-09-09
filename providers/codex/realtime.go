package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/common/wsconn"
	runtimesession "one-api/runtime/session"
	"one-api/types"
)

const codexResponsesWebsocketBetaHeaderValue = "responses_websockets=2026-02-06"
const codexRealtimeDiagnosticValueLogLimit = 4096

type codexRealtimeConnPlan struct {
	wsURL           string
	headers         map[string]string
	allowSelfHosted bool
	proxyAddr       string
	safeRouteRetry  bool
}

func (p *CodexProvider) createChatRealtimeConn(modelName, sessionID string) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	ctx, cancel := context.WithTimeout(context.Background(), config.ConnectTimeout())
	defer cancel()
	return p.createChatRealtimeConnWithContext(ctx, modelName, sessionID)
}

func (p *CodexProvider) createChatRealtimeConnWithContext(ctx context.Context, modelName, sessionID string) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	plan, errWithCode := p.prepareChatRealtimeConn(modelName, sessionID)
	if errWithCode != nil {
		return nil, errWithCode
	}
	return p.dialChatRealtimeConnWithContext(ctx, plan)
}

func (p *CodexProvider) prepareChatRealtimeConn(modelName, sessionID string) (*codexRealtimeConnPlan, *types.OpenAIErrorWithStatusCode) {
	return p.prepareChatRealtimeConnWithSelfHosted(modelName, sessionID, p.codexRealtimeSelfHosted())
}

func (p *CodexProvider) prepareChatRealtimeConnWithSelfHosted(modelName, sessionID string, allowSelfHosted bool) (*codexRealtimeConnPlan, *types.OpenAIErrorWithStatusCode) {
	urlPath, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatRealtime)
	if errWithCode != nil {
		return nil, errWithCode
	}

	httpURL := p.GetFullRequestURL(urlPath, modelName)
	proxyAddr := channelProxyValue(p.codexChannel())
	wsURL, err := buildCodexRealtimeURLWithPolicy(httpURL, allowSelfHosted, proxyAddr == "")
	if err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "ws_request_failed", requester.UpstreamRealtimeURLStatusCode(err))
	}

	safeRouteRetry := !p.codexOpenMayMutateCredentials()
	headers, err := p.getRealtimeHeaders(sessionID)
	if err != nil {
		return nil, p.handleTokenError(err)
	}

	return &codexRealtimeConnPlan{
		wsURL:           wsURL,
		headers:         headers,
		allowSelfHosted: allowSelfHosted,
		proxyAddr:       proxyAddr,
		safeRouteRetry:  safeRouteRetry,
	}, nil
}

func (p *CodexProvider) dialChatRealtimeConn(plan *codexRealtimeConnPlan) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	ctx, cancel := context.WithTimeout(context.Background(), config.ConnectTimeout())
	defer cancel()
	return p.dialChatRealtimeConnWithContext(ctx, plan)
}

func (p *CodexProvider) dialChatRealtimeConnWithContext(ctx context.Context, plan *codexRealtimeConnPlan) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	if plan == nil {
		return nil, common.StringErrorWrapperLocal("realtime websocket plan is required", "ws_request_failed", http.StatusInternalServerError)
	}

	dialCtx, cancel := codexRealtimeDialContext(ctx)
	defer cancel()
	wsConn, err := wsconn.DialManaged(dialCtx, plan.wsURL, codexRealtimeHTTPHeader(plan.headers), codexRealtimeWSConfig(), codexRealtimeDialOptions(plan.proxyAddr, plan.allowSelfHosted)...)
	if err != nil {
		apiErr := mapCodexRealtimeWSDialError(err)
		if apiErr != nil {
			apiErr.ProviderOpenRetrySafe = plan.safeRouteRetry
		}
		return nil, apiErr
	}

	return wsConn, nil
}

func (p *CodexProvider) codexOpenMayMutateCredentials() bool {
	if p == nil {
		return false
	}
	p.credentialsMu.Lock()
	refreshPossible := p.Credentials != nil && strings.TrimSpace(p.Credentials.RefreshToken) != ""
	p.credentialsMu.Unlock()
	return refreshPossible || p.hasDirtyCredentials()
}

func codexRealtimeDialContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, config.ConnectTimeout())
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
	inboundActivityTimeout := config.RealtimeWebsocketClientInboundActivityTimeout()
	writeTimeout := config.RealtimeWebsocketWriteTimeout()
	return wsconn.Config{
		Label:           "codex realtime upstream",
		PingInterval:    config.RealtimeWebsocketPingInterval(),
		PongMissTimeout: config.RealtimeWebsocketClientPongMissTimeout(),
		InboundActivityTimeout: func() time.Duration {
			return inboundActivityTimeout
		},
		ReadLimit:    config.RealtimeWebsocketReadLimit(),
		WriteTimeout: func() time.Duration { return writeTimeout },
	}
}

func codexRealtimeDialOptions(proxyAddr string, allowSelfHosted bool) []wsconn.DialOption {
	policy := wsconn.DialSecurityPolicy{
		AllowInsecureWS: allowSelfHosted,
		AllowPrivateIP:  allowSelfHosted,
	}
	options := []wsconn.DialOption{
		wsconn.WithHandshakeTimeout(config.ConnectTimeout()),
		wsconn.WithDialSecurityPolicy(policy),
	}
	if strings.TrimSpace(proxyAddr) != "" {
		options = append(options, wsconn.WithProxyURL(proxyAddr))
	}
	return options
}

func mapCodexRealtimeWSDialError(err error) *types.OpenAIErrorWithStatusCode {
	logCodexRealtimeWSDialFailure(err)

	var dialErr *wsconn.DialError
	if errors.As(err, &dialErr) && dialErr != nil {
		if apiErr := codexRealtimeProviderAPIErrorFromDialError(dialErr); apiErr != nil {
			apiErr.LocalError = false
			apiErr.UpstreamNotAttempted = true
			return apiErr
		}
		apiErr := mapCodexRealtimeWSDialStatus(dialErr.StatusCode)
		apiErr.LocalError = false
		apiErr.UpstreamNotAttempted = true
		return apiErr
	}
	apiErr := common.StringErrorWrapperLocal("websocket request failed", "ws_request_failed", http.StatusInternalServerError)
	apiErr.LocalError = false
	apiErr.UpstreamNotAttempted = true
	return apiErr
}

func codexRealtimeProviderAPIErrorFromDialError(dialErr *wsconn.DialError) *types.OpenAIErrorWithStatusCode {
	if dialErr == nil || len(dialErr.BodySnippet) == 0 {
		return nil
	}
	apiErr := runtimesession.ProviderAPIErrorFromPayload(dialErr.BodySnippet)
	if apiErr == nil {
		return nil
	}
	if dialErr.StatusCode > 0 {
		apiErr.StatusCode = dialErr.StatusCode
	}
	apiErr.LocalError = false
	apiErr.Message = codexRealtimeProviderHandshakeMessage(apiErr.OpenAIError, apiErr.StatusCode)
	return apiErr
}

func mapCodexRealtimeWSDialStatus(statusCode int) *types.OpenAIErrorWithStatusCode {
	switch statusCode {
	case http.StatusNotFound, http.StatusUpgradeRequired:
		return common.StringErrorWrapperLocal("channel does not support Responses websocket transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
	case http.StatusUnauthorized, http.StatusForbidden:
		return codexRealtimeProviderHandshakeError("provider authentication failed", "provider_authentication_failed", "authentication_error", statusCode)
	case http.StatusTooManyRequests:
		return codexRealtimeProviderHandshakeError("provider rate limit exceeded", "provider_rate_limit_exceeded", "rate_limit_error", http.StatusTooManyRequests)
	default:
		if statusCode >= 500 {
			return codexRealtimeProviderHandshakeError("provider websocket request failed", "provider_ws_request_failed", "upstream_error", statusCode)
		}
	}
	return common.StringErrorWrapperLocal("websocket request failed", "ws_request_failed", http.StatusInternalServerError)
}

func codexRealtimeProviderHandshakeError(message, code, errType string, statusCode int) *types.OpenAIErrorWithStatusCode {
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Message: message,
			Type:    errType,
			Code:    code,
		},
		StatusCode: statusCode,
		LocalError: false,
	}
}

func codexRealtimeProviderHandshakeMessage(err types.OpenAIError, statusCode int) string {
	switch {
	case common.ProviderErrorIsQuotaExhausted(err):
		return "provider quota exhausted"
	case common.ProviderErrorIsRateLimited(err) || statusCode == http.StatusTooManyRequests:
		return "provider rate limit exceeded"
	case common.ProviderErrorIsAuthRejected(err) || statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return "provider authentication failed"
	case statusCode >= 500:
		return "provider websocket request failed"
	default:
		return "websocket request failed"
	}
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

	body := codexRealtimeBodyForLog(dialErr.BodySnippet)
	if body == "" && dialErr.BodyReadErr != nil {
		body = "body_read_failed:" + codexRealtimeLogValue(dialErr.BodyReadErr.Error())
	}
	return fmt.Sprintf(
		"codex realtime websocket dial failed: status=%d url=%s server=%s via=%s cf_ray=%s x_request_id=%s openai_request_id=%s retry_after=%s body_truncated=%v body=%s cause=%s",
		dialErr.StatusCode,
		codexRealtimeWSURLForLog(dialErr.SafeURL()),
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

func codexRealtimeBodyForLog(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if redacted, changed := common.RedactSensitiveJSON(body); changed {
		return codexRealtimeLogValue(string(redacted))
	}
	original := string(body)
	redacted := common.RedactSensitiveText(original)
	if redacted == strings.Join(strings.Fields(original), " ") {
		redacted = original
	}
	return codexRealtimeLogValue(redacted)
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
	if p.Context != nil && p.Context.GetBool("self_hosted") {
		return true
	}
	channel := p.codexChannel()
	if channel == nil {
		return false
	}
	other, err := channel.GetOtherMap()
	if err != nil {
		return false
	}
	return codexRawJSONBool(other["self_hosted"])
}

func (p *CodexProvider) codexResponsesWSSelfHosted() bool {
	channel := p.codexChannel()
	if channel == nil {
		return false
	}
	enabled, _ := channel.GetOtherBoolField("responses_ws_self_hosted")
	return enabled
}

func codexRawJSONBool(raw json.RawMessage) bool {
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value
}

var codexRealtimeCompatibilityHeaderKeys = []string{
	"version",
	"originator",
	"user-agent",
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

func (p *CodexProvider) handleCodexSupplierMessage(messageType wsconn.MessageType, message []byte, accumulator *codexTurnUsageAccumulator) (bool, *types.UsageEvent, []byte, error) {
	// wsconn.MessageType belongs at the concrete websocket boundary. Adapters
	// that already own framing should call handleCodexSupplierPayload directly.
	if messageType != wsconn.TextMessage {
		return true, nil, nil, nil
	}
	return p.handleCodexSupplierPayload(message, accumulator)
}

func (p *CodexProvider) getPassthroughRealtimeHeader(key string) string {
	if p == nil || p.Context == nil || p.Context.Request == nil {
		return ""
	}
	return strings.TrimSpace(p.Context.Request.Header.Get(key))
}
