package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/logger"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/providers/codex/wire"
	"one-api/providers/openai"
	"one-api/types"
)

// CodexResponsesStreamHandler handles Codex Responses streaming.
type CodexResponsesStreamHandler struct {
	Usage       *types.Usage
	eventBuffer strings.Builder
	eventType   string
	accumulator *codexTurnUsageAccumulator
}

const codexResponsesStreamMaxLineBytes = 16 << 20

var (
	codexResponsesStreamMaxEventBytes = 16 << 20
	errCodexResponsesSSEEventTooLarge = errors.New("codex responses SSE event exceeds configured read limit")
)

func cloneCodexExtraBilling(extraBilling map[string]types.ExtraBilling) map[string]types.ExtraBilling {
	if len(extraBilling) == 0 {
		return nil
	}

	cloned := make(map[string]types.ExtraBilling, len(extraBilling))
	for key, value := range extraBilling {
		cloned[key] = value
	}
	return cloned
}

func safeCountCodexResponseTokens(content string, modelName string) (tokens int) {
	defer func() {
		if recover() != nil {
			tokens = 0
		}
	}()
	return common.CountTokenText(content, modelName)
}

func applyResolvedCodexUsage(target *types.Usage, resolved *types.Usage) {
	if target == nil || resolved == nil {
		return
	}

	existingText := ""
	if target.TextBuilder.Len() > 0 {
		existingText = target.TextBuilder.String()
	}

	*target = *resolved
	if existingText != "" {
		target.TextBuilder.WriteString(existingText)
	}
}

func resolveCodexResponsesUsage(seed *types.Usage, accumulator *codexTurnUsageAccumulator, response *types.OpenAIResponsesResponses, modelName string, allowContentFallback bool) *types.Usage {
	if response == nil {
		return nil
	}
	if accumulator == nil {
		accumulator = newCodexTurnUsageAccumulator()
	}
	accumulator.SeedFromUsage(seed)
	return accumulator.ResolveUsage(response, modelName, allowContentFallback)
}

func finalizeCodexResponsesUsage(usage *types.Usage, response *types.OpenAIResponsesResponses, modelName string, allowContentFallback bool) {
	resolved := resolveCodexResponsesUsage(usage, nil, response, modelName, allowContentFallback)
	if usage == nil || resolved == nil {
		return
	}
	applyResolvedCodexUsage(usage, resolved)
}

func codexResponsesSearchType(response *types.OpenAIResponsesResponses) string {
	return commonresponses.ResponsesSearchType(response)
}

func applyCodexResponsesAddedToolBilling(usage *types.Usage, item *types.ResponsesOutput, searchType string) {
	commonresponses.ApplyResponsesOutputItemBilling(usage, item, searchType)
}

func (h *CodexResponsesStreamHandler) observeUsageEvent(dataLine string) {
	if h == nil {
		return
	}

	event, ok := commonresponses.ParseStreamUsageEvent([]byte(dataLine))
	if !ok {
		return
	}

	if h.accumulator != nil {
		var delta any
		if text, ok := commonresponses.StreamEventDeltaString(event.Delta); ok {
			delta = text
		}
		h.accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
			Type:        event.Type,
			Delta:       delta,
			Item:        event.Item,
			OutputIndex: event.OutputIndex,
			Response:    event.Response,
		})
	}

	switch event.Type {
	case "response.output_text.delta", "response.reasoning_summary_text.delta":
		if h.Usage != nil {
			if delta, ok := commonresponses.StreamEventDeltaString(event.Delta); ok {
				h.Usage.TextBuilder.WriteString(delta)
			}
		}
	case "response.output_item.added":
		if h.Usage != nil {
			searchType := ""
			if h.accumulator != nil {
				searchType = h.accumulator.searchType
			}
			applyCodexResponsesAddedToolBilling(h.Usage, event.Item, searchType)
		}
	case "response.completed", "response.failed", "response.incomplete", "response.done":
		if resolved := resolveCodexResponsesUsage(h.Usage, h.accumulator, event.Response, "", false); resolved != nil {
			applyResolvedCodexUsage(h.Usage, resolved)
		}
	}
}

func newCodexResponsesStreamHandler(usage *types.Usage) *CodexResponsesStreamHandler {
	accumulator := newCodexTurnUsageAccumulator()
	accumulator.SeedFromUsage(usage)
	return &CodexResponsesStreamHandler{
		Usage:       usage,
		accumulator: accumulator,
	}
}

// CreateResponses builds a non-streamed response via Codex Official streaming.
func (p *CodexProvider) CreateResponses(ctx context.Context, rawReq *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.prepareResponsesCreateRequest(ctx, rawReq)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()
	request := codexResponsesProjection(rawReq)
	request.Stream = true

	// Send streaming request.
	httpRequester := p.codexRequester()
	if httpRequester == nil {
		return nil, common.StringErrorWrapperLocal("requester is not configured", "channel_error", http.StatusServiceUnavailable)
	}
	resp, errWithCode := httpRequester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// Create stream handler.
	handler := newCodexResponsesStreamHandler(p.Usage)

	// Get stream response.
	stream, errWithCode := requester.RequestNoTrimStreamWithEmitterOptions(httpRequester, resp, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{
		MaxLineBytes: codexResponsesStreamMaxLineBytes,
	})
	if errWithCode != nil {
		return nil, errWithCode
	}

	// Aggregate full response.
	response, errWithCode := p.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		return nil, errWithCode
	}

	if p.Usage == nil {
		p.Usage = &types.Usage{}
	}
	if resolved := resolveCodexResponsesUsage(p.Usage, handler.accumulator, response, request.Model, true); resolved != nil {
		applyResolvedCodexUsage(p.Usage, resolved)
	}
	backfillCodexResponsePromptCacheKey(response, request)
	return response, nil
}

// CreateResponsesStream streams Responses.
func (p *CodexProvider) CreateResponsesStream(ctx context.Context, rawReq *commonresponses.Request) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.prepareResponsesCreateRequest(ctx, rawReq)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()
	request := codexResponsesProjection(rawReq)
	request.Stream = true
	httpRequester := p.codexRequester()
	if httpRequester == nil {
		return nil, common.StringErrorWrapperLocal("requester is not configured", "channel_error", http.StatusServiceUnavailable)
	}

	// Send request.
	resp, errWithCode := httpRequester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// Create stream handler.
	handler := newCodexResponsesStreamHandler(p.Usage)

	// Convert Responses SSE to ChatCompletion stream when requested.
	if request.ConvertChat {
		chatHandler := openai.OpenAIResponsesStreamHandler{
			Usage:  &types.Usage{},
			Prefix: "data: ",
			Model:  request.Model,
		}

		bridgeHandler := func(rawLine *[]byte, dataChan chan string, errChan chan error) {
			if rawLine == nil || len(*rawLine) == 0 {
				return
			}

			rawStr := strings.TrimSpace(string(*rawLine))
			if !strings.HasPrefix(rawStr, "data:") {
				return
			}

			// Normalize "data:{...}" and "data: {...}" to the expected "data: {...}".
			dataLine := strings.TrimSpace(strings.TrimPrefix(rawStr, "data:"))
			if dataLine == "" || dataLine == "[DONE]" {
				return
			}
			handler.observeUsageEvent(dataLine)

			normalized := []byte("data: " + dataLine)
			chatHandler.HandlerChatStream(&normalized, dataChan, errChan)
		}

		return requester.RequestStreamWithOptions(httpRequester, resp, bridgeHandler, requester.StreamReadOptions{
			MaxLineBytes: codexResponsesStreamMaxLineBytes,
		})
	}

	// Use RequestNoTrimStream to preserve event lines.
	return requester.RequestNoTrimStreamWithEmitterOptions(httpRequester, resp, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{
		MaxLineBytes: codexResponsesStreamMaxLineBytes,
	})
}

func (p *CodexProvider) CompactResponses(ctx context.Context, rawReq *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.prepareResponsesCompactRequest(ctx, rawReq)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()
	request := codexResponsesProjection(rawReq)
	request.Stream = false

	response := &types.OpenAIResponsesResponses{}
	httpRequester := p.codexRequester()
	if httpRequester == nil {
		return nil, common.StringErrorWrapperLocal("requester is not configured", "channel_error", http.StatusServiceUnavailable)
	}
	_, errWithCode = httpRequester.SendRequest(req, response, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	if p.Usage == nil {
		p.Usage = &types.Usage{}
	}

	finalizeCodexResponsesUsage(p.Usage, response, request.Model, false)
	backfillCodexResponsePromptCacheKey(response, request)
	return response, nil
}

// codexResponsesProjection keeps raw body planning separate from the
// local typed projection used for downstream accounting and response shaping.
func codexResponsesProjection(req *commonresponses.Request) *types.OpenAIResponsesRequest {
	return commonresponses.ProjectRequest(req, normalizeCodexModelName)
}

func (p *CodexProvider) prepareResponsesCreateRequest(ctx context.Context, req *commonresponses.Request) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	if req == nil || req.Body == nil || req.Body.Object == nil {
		return nil, common.StringErrorWrapperLocal("request body is required", "invalid_request_error", http.StatusBadRequest)
	}
	model := normalizeCodexModelName(req.Model)
	if model == "" {
		model = normalizeCodexModelName(req.Body.Projection.Model)
	}
	policy := responsesPolicyInput(req)
	body, err := wire.PlanResponsesCreateBody(req.Body.Object, wire.CreateBodyInput{
		Model:       model,
		Stream:      true,
		PromptCache: policy.PromptCache,
	})
	if err != nil {
		return nil, codexWireError(err)
	}
	return p.prepareResponsesOfficialHTTPRequest(ctx, req, wire.OpResponsesCreate, "", model, body)
}

func (p *CodexProvider) prepareResponsesCompactRequest(ctx context.Context, req *commonresponses.Request) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	if req == nil || req.Body == nil || req.Body.Object == nil {
		return nil, common.StringErrorWrapperLocal("request body is required", "invalid_request_error", http.StatusBadRequest)
	}
	model := normalizeCodexModelName(req.Model)
	if model == "" {
		model = normalizeCodexModelName(req.Body.Projection.Model)
	}
	policy := responsesPolicyInput(req)
	body, err := wire.PlanResponsesCompactBody(req.Body.Object, req.Body.Projection, model, policy.PromptCache)
	if err != nil {
		return nil, codexWireError(err)
	}
	return p.prepareResponsesOfficialHTTPRequest(ctx, req, wire.OpResponsesCompact, "compact", model, body)
}

func responsesPolicyInput(req *commonresponses.Request) commonresponses.PolicyInput {
	if req == nil {
		return commonresponses.PolicyInput{}
	}
	policy := req.Policy
	if req.Body != nil {
		if key := strings.TrimSpace(req.Body.Projection.PromptCacheKey); key != "" {
			policy.PromptCache = &commonresponses.PromptCacheDecision{
				Key:    key,
				Source: commonresponses.PromptCacheClientBody,
			}
			return policy
		}
	}
	if policy.PromptCache != nil && strings.TrimSpace(policy.PromptCache.Key) != "" {
		return policy
	}
	return policy
}

func (p *CodexProvider) prepareResponsesOfficialHTTPRequest(ctx context.Context, req *commonresponses.Request, operation wire.Operation, pathSuffix, model string, body []byte) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	metadata, err := wire.MetadataFromResponsesBody(req.Body.Object)
	if err != nil {
		return nil, codexWireError(err)
	}
	policy, err := p.codexOfficialChannelPolicy()
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "channel_config_error", http.StatusServiceUnavailable)
	}
	principal := wire.PrincipalFingerprint{}
	if policy.AutoGenerate.InstallationID {
		principal, err = p.codexPrincipalFingerprint(req.Principal)
		if err != nil {
			return nil, common.ErrorWrapperLocal(err, "channel_config_error", http.StatusServiceUnavailable)
		}
	}
	token, tokenErr := p.GetToken()
	if tokenErr != nil {
		return nil, p.handleTokenError(tokenErr)
	}
	identity, decisions, err := wire.ResolveIdentity(wire.IdentityInput{
		Operation: operation,
		Headers:   req.Headers,
		Metadata:  metadata,
		Policy:    policy,
		Principal: principal,
		ChannelID: req.ChannelID,
		Clock:     wire.RealClock{},
	})
	if err != nil {
		return nil, codexWireError(err)
	}
	plan, err := wire.BuildHeaders(wire.HeaderPlanInput{
		Operation: operation,
		Headers:   req.Headers,
		Credential: wire.Credential{
			AccessToken: token,
			AccountID:   p.codexAccountID(),
		},
		Policy:   policy,
		Identity: identity,
	})
	if err != nil {
		return nil, codexWireError(err)
	}
	plan.Decisions = append(decisions, plan.Decisions...)
	p.auditCodexOfficialHeaderPlan(ctx, operation, req.ChannelID, plan.Decisions)

	requestPath := strings.TrimRight(p.Config.Responses, "/")
	if pathSuffix != "" {
		requestPath += "/" + strings.TrimLeft(pathSuffix, "/")
	}
	fullRequestURL := p.GetFullRequestURL(requestPath, model)
	httpRequester := p.codexRequester()
	if httpRequester == nil {
		return nil, common.StringErrorWrapperLocal("requester is not configured", "channel_error", http.StatusServiceUnavailable)
	}
	httpReq, buildErr := httpRequester.NewRequest(http.MethodPost, fullRequestURL, httpRequester.WithBody(body), httpRequester.WithHeader(plan.Map()), httpRequester.WithContext(ctx))
	if buildErr != nil {
		return nil, common.ErrorWrapper(buildErr, "new_request_failed", http.StatusInternalServerError)
	}
	return httpReq, nil
}

func codexWireError(err error) *types.OpenAIErrorWithStatusCode {
	if err == nil {
		return nil
	}
	if violation, ok := err.(*wire.Violation); ok {
		return &types.OpenAIErrorWithStatusCode{
			OpenAIError: types.OpenAIError{
				Message: publicWireViolationMessage(violation),
				Type:    "invalid_request_error",
				Param:   violation.Param,
				Code:    "invalid_request_error",
			},
			StatusCode: http.StatusBadRequest,
			LocalError: true,
		}
	}
	return common.ErrorWrapperLocal(err, "internal_server_error", http.StatusInternalServerError)
}

func publicWireViolationMessage(violation *wire.Violation) string {
	if violation == nil || strings.TrimSpace(violation.Param) == "" {
		return "invalid request"
	}
	return "invalid request field: " + strings.TrimSpace(violation.Param)
}

func (p *CodexProvider) auditCodexOfficialHeaderPlan(ctx context.Context, operation wire.Operation, channelID int, decisions []wire.Decision) {
	if len(decisions) == 0 {
		return
	}
	if !logger.DebugEnabled() {
		return
	}
	payload, err := json.Marshal(struct {
		Dialect   string          `json:"dialect"`
		Operation wire.Operation  `json:"operation"`
		ChannelID int             `json:"channel_id,omitempty"`
		Decisions []wire.Decision `json:"decisions"`
	}{
		Dialect:   "codex_official",
		Operation: operation,
		ChannelID: channelID,
		Decisions: decisions,
	})
	if err != nil {
		return
	}
	logger.LogDebug(ctx, "[Codex] official upstream header decisions "+string(payload))
}

func (p *CodexProvider) getPromptCacheKeyStrategy() string {
	if options := p.getChannelOptions(); options != nil {
		return normalizePromptCacheStrategy(options.PromptCacheKeyStrategy)
	}
	return codexPromptCacheStrategyOff
}

func backfillCodexResponsePromptCacheKey(response *types.OpenAIResponsesResponses, request *types.OpenAIResponsesRequest) {
	if response == nil || request == nil {
		return
	}
	if strings.TrimSpace(response.PromptCacheKey) != "" {
		return
	}
	if strings.TrimSpace(request.PromptCacheKey) == "" {
		return
	}
	response.PromptCacheKey = request.PromptCacheKey
}

// collectResponsesStreamResponse aggregates stream to a response.
func (p *CodexProvider) collectResponsesStreamResponse(stream requester.StreamReaderInterface[string]) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	if stream == nil {
		return nil, common.StringErrorWrapperLocal("response stream is required", "stream_read_failed", http.StatusInternalServerError)
	}
	defer stream.Close()

	var response *types.OpenAIResponsesResponses

	dataChan, errChan := stream.Recv()

	for dataChan != nil || errChan != nil {
		select {
		case data, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}

			if strings.TrimSpace(data) == "" {
				continue
			}

			// Extract JSON payload from SSE.
			jsonData := extractJSONFromSSE(data)
			if jsonData == "" {
				continue
			}

			// Parse stream payload.
			var streamResp types.OpenAIResponsesStreamResponses
			if err := json.Unmarshal([]byte(jsonData), &streamResp); err != nil {
				continue
			}

			// Capture terminal response event.
			if (streamResp.Type == "response.completed" || streamResp.Type == "response.failed" || streamResp.Type == "response.incomplete" || streamResp.Type == "response.done") && streamResp.Response != nil {
				response = streamResp.Response
			}

		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err != nil {
				// EOF is normal end-of-stream.
				if errors.Is(err, io.EOF) {
					dataChan = nil
					errChan = nil
					continue
				}
				return nil, common.ErrorWrapper(err, "stream_read_failed", http.StatusInternalServerError)
			}
		}
	}

	if response == nil {
		return nil, common.StringErrorWrapperLocal("no response received", "no_response", http.StatusInternalServerError)
	}
	return response, nil
}

// extractJSONFromSSE extracts JSON payload from SSE data.
func extractJSONFromSSE(sseData string) string {
	// SSE format example:
	// event: response.created
	//
	// data: {"type":"response.created",...}
	//
	// Extract JSON after data: prefix.

	var payload strings.Builder
	forEachSSELine(sseData, func(line string) bool {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				return true
			}
			if payload.Len() > 0 {
				payload.WriteByte('\n')
			}
			payload.WriteString(data)
		}
		return true
	})
	return payload.String()
}

func forEachSSELine(sseData string, visit func(string) bool) {
	for {
		idx := strings.IndexByte(sseData, '\n')
		if idx < 0 {
			if sseData != "" {
				visit(sseData)
			}
			return
		}
		if !visit(sseData[:idx]) {
			return
		}
		sseData = sseData[idx+1:]
	}
}

// HandlerResponsesStream handles Responses streaming (passthrough).
func (h *CodexResponsesStreamHandler) HandlerResponsesStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	h.handleResponsesStreamWithError(rawLine, func(data string) bool {
		dataChan <- data
		return true
	}, func(err error) bool {
		select {
		case errChan <- err:
			return true
		default:
			return false
		}
	})
}

func (h *CodexResponsesStreamHandler) HandlerResponsesStreamWithEmitter(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
	h.handleResponsesStreamWithError(rawLine, emitter.SendData, emitter.SendError)
}

func (h *CodexResponsesStreamHandler) handleResponsesStream(rawLine *[]byte, sendData func(string) bool) {
	h.handleResponsesStreamWithError(rawLine, sendData, nil)
}

func (h *CodexResponsesStreamHandler) handleResponsesStreamWithError(rawLine *[]byte, sendData func(string) bool, sendError func(error) bool) {
	if h == nil || rawLine == nil {
		return
	}
	rawStr := string(*rawLine)

	// Handle SSE event lines.
	if strings.HasPrefix(rawStr, "event: ") {
		if h.eventBuffer.Len() > 0 {
			if !sendData(h.eventBuffer.String()) {
				return
			}
			h.eventBuffer.Reset()
		}
		// Start new event and capture event type.
		h.eventType = strings.TrimSpace(strings.TrimPrefix(rawStr, "event: "))
		h.eventBuffer.Reset()
		if err := appendCodexSSELine(&h.eventBuffer, rawStr); err != nil {
			h.failBufferedSSEEvent(sendError, err)
		}
		return
	}

	// Buffer non-data lines when inside an event.
	if !strings.HasPrefix(rawStr, "data:") {
		if h.eventBuffer.Len() > 0 {
			if err := appendCodexSSELine(&h.eventBuffer, rawStr); err != nil {
				h.failBufferedSSEEvent(sendError, err)
				return
			}
			if strings.TrimSpace(rawStr) == "" {
				if !sendData(h.eventBuffer.String()) {
					return
				}
				h.eventBuffer.Reset()
				h.eventType = ""
			}
		} else {
			// No event type: forward as-is.
			sendData(rawStr)
		}
		return
	}

	// Handle data line.
	dataLine := strings.TrimPrefix(rawStr, "data:")
	dataLine = strings.TrimSpace(dataLine)

	// Skip [DONE].
	if dataLine == "[DONE]" {
		// Flush buffered event.
		if h.eventBuffer.Len() > 0 {
			if !sendData(h.eventBuffer.String()) {
				return
			}
			h.eventBuffer.Reset()
			h.eventType = ""
		}
		return
	}

	// Passthrough: buffer or forward raw data.
	if h.eventBuffer.Len() > 0 {
		// Buffer data line within event.
		if err := appendCodexSSELine(&h.eventBuffer, rawStr); err != nil {
			h.failBufferedSSEEvent(sendError, err)
			return
		}
	} else {
		// No event type: forward data line.
		sendData(rawStr)
	}

	h.observeUsageEvent(dataLine)
}

func (h *CodexResponsesStreamHandler) failBufferedSSEEvent(sendError func(error) bool, err error) {
	if h != nil {
		h.eventBuffer.Reset()
		h.eventType = ""
	}
	if sendError != nil {
		sendError(err)
	}
}

func appendCodexSSELine(buffer *strings.Builder, raw string) error {
	if buffer == nil {
		return nil
	}
	extraBytes := len(raw)
	if !strings.HasSuffix(raw, "\n") {
		extraBytes++
	}
	if codexResponsesStreamMaxEventBytes > 0 && buffer.Len()+extraBytes > codexResponsesStreamMaxEventBytes {
		return errCodexResponsesSSEEventTooLarge
	}
	buffer.WriteString(raw)
	if !strings.HasSuffix(raw, "\n") {
		buffer.WriteString("\n")
	}
	return nil
}
