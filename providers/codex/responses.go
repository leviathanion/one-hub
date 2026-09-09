package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/jsonobject"
	"one-api/common/logger"
	"one-api/common/providerresponse"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/providers/codex/wire"
	"one-api/providers/openai"
	"one-api/types"
)

// CodexResponsesStreamHandler handles Codex Responses streaming.
type CodexResponsesStreamHandler struct {
	Usage              *types.Usage
	accumulator        *codexTurnUsageAccumulator
	ProviderCredential string
}

const codexResponsesStreamMaxLineBytes = 16 << 20

var codexResponsesStreamMaxEventBytes = 16 << 20

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

func applyResolvedCodexUsage(target *types.Usage, resolved *types.Usage) {
	if target == nil || resolved == nil {
		return
	}
	*target = *resolved
}

func resolveCodexResponsesUsage(seed *types.Usage, accumulator *codexTurnUsageAccumulator, response *types.OpenAIResponsesResponses) *types.Usage {
	if response == nil {
		return nil
	}
	if accumulator == nil {
		accumulator = newCodexTurnUsageAccumulator()
	}
	accumulator.SeedFromUsage(seed)
	return accumulator.ResolveUsage(response)
}

func finalizeCodexResponsesUsage(usage *types.Usage, response *types.OpenAIResponsesResponses) error {
	if response == nil {
		return nil
	}
	accumulator := newCodexTurnUsageAccumulator()
	accumulator.SeedFromUsage(usage)
	if err := accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{Type: codexTerminalEventType(response), Response: response}); err != nil {
		mergeCodexAccumulatorBilling(usage, accumulator)
		return err
	}
	resolved := resolveCodexResponsesUsage(usage, accumulator, response)
	if usage == nil || resolved == nil {
		return nil
	}
	applyResolvedCodexUsage(usage, resolved)
	return nil
}

func codexTerminalEventType(response *types.OpenAIResponsesResponses) string {
	if response != nil {
		switch strings.ToLower(strings.TrimSpace(response.Status)) {
		case types.ResponseStatusFailed:
			return "response.failed"
		case types.ResponseStatusIncomplete, types.ResponseStatusCancelled:
			return "response.incomplete"
		}
	}
	return "response.completed"
}

func (h *CodexResponsesStreamHandler) observeUsageEvent(dataLine string) error {
	if h == nil {
		return nil
	}

	event, ok := commonresponses.ParseStreamUsageEvent([]byte(dataLine))
	if !ok {
		return nil
	}

	if h.accumulator != nil {
		if err := h.accumulator.ObserveEvent(&types.OpenAIResponsesStreamResponses{
			Type:              event.Type,
			Item:              event.Item,
			ItemID:            event.ItemID,
			OutputIndex:       event.OutputIndex,
			PartialImageIndex: event.PartialImageIndex,
			Response:          event.Response,
		}); err != nil {
			mergeCodexAccumulatorBilling(h.Usage, h.accumulator)
			return err
		}
	}

	switch event.Type {
	case "response.output_item.done":
		mergeCodexAccumulatorBilling(h.Usage, h.accumulator)
	case "response.completed", "response.failed", "response.incomplete":
		if resolved := resolveCodexResponsesUsage(h.Usage, h.accumulator, event.Response); resolved != nil {
			applyResolvedCodexUsage(h.Usage, resolved)
		}
	}
	return nil
}

func (h *CodexResponsesStreamHandler) ObserveAcceptedResponsesEvent(rawEvent string) error {
	payload, ok := commonresponses.SSEDataPayload(rawEvent)
	if !ok {
		return nil
	}
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	return h.observeUsageEvent(payload)
}

func mergeCodexAccumulatorBilling(usage *types.Usage, accumulator *codexTurnUsageAccumulator) {
	if usage == nil || accumulator == nil {
		return
	}
	commonresponses.MergeResponsesExtraBillingMax(usage, accumulator.toolUsage.ExtraBilling)
	usage.MergeBillingDiagnostics(accumulator.toolUsage.BillingDiagnostics)
	for key, billing := range accumulator.toolUsage.ExtraBilling {
		usage.MarkProviderExtraBilling(key, billing)
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
	baseRequester := p.codexRequester()
	if baseRequester == nil {
		return nil, common.StringErrorWrapperLocal("requester is not configured", "channel_error", http.StatusServiceUnavailable)
	}
	httpRequester := baseRequester.ForHTTPProfile(requester.HTTPProfileLongStream)
	resp, errWithCode := httpRequester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// Create stream handler.
	handler := newCodexResponsesStreamHandler(p.Usage)
	handler.ProviderCredential = p.Channel.Key

	// Get stream response.
	rawStream, errWithCode := requester.RequestNoTrimStreamWithEmitterOptions(httpRequester, resp, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{
		MaxLineBytes: codexResponsesStreamMaxLineBytes,
	})
	if errWithCode != nil {
		return nil, errWithCode
	}
	stream := commonresponses.NewEventStream(rawStream, handler.ObserveAcceptedResponsesEvent)

	// Aggregate full response.
	response, errWithCode := p.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		return nil, errWithCode
	}

	if p.Usage == nil {
		p.Usage = &types.Usage{}
	}
	if resolved := resolveCodexResponsesUsage(p.Usage, handler.accumulator, response); resolved != nil {
		applyResolvedCodexUsage(p.Usage, resolved)
	}
	backfillCodexResponsePromptCacheKey(response, request)
	return response, nil
}

// CreateResponsesStream streams Responses.
func (p *CodexProvider) CreateResponsesStream(ctx context.Context, rawReq *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.prepareResponsesCreateRequest(ctx, rawReq)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()
	request := codexResponsesProjection(rawReq)
	request.Stream = true
	baseRequester := p.codexRequester()
	if baseRequester == nil {
		return nil, common.StringErrorWrapperLocal("requester is not configured", "channel_error", http.StatusServiceUnavailable)
	}
	httpRequester := baseRequester.ForHTTPProfile(requester.HTTPProfileLongStream)

	// Send request.
	resp, errWithCode := httpRequester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// Create stream handler.
	handler := newCodexResponsesStreamHandler(p.Usage)
	handler.ProviderCredential = p.Channel.Key

	// Convert Responses SSE to ChatCompletion stream when requested.
	if request.ConvertChat {
		chatHandler := openai.OpenAIResponsesStreamHandler{
			Usage:  &types.Usage{},
			Prefix: "data: ",
			Model:  request.Model,
		}

		stream, apiErr := requester.RequestNoTrimStreamWithOptions(httpRequester, resp, chatHandler.ChatSSEHandler(handler.ObserveAcceptedResponsesEvent), requester.StreamReadOptions{
			MaxLineBytes:            codexResponsesStreamMaxLineBytes,
			RequireProtocolTerminal: true,
		})
		return commonresponses.NewEventStream(stream, commonresponses.IgnoreAcceptedResponsesEvent), apiErr
	}

	// Use RequestNoTrimStream to preserve event lines.
	stream, apiErr := requester.RequestNoTrimStreamWithEmitterOptions(httpRequester, resp, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{
		MaxLineBytes: codexResponsesStreamMaxLineBytes,
	})
	return commonresponses.NewEventStream(stream, handler.ObserveAcceptedResponsesEvent), apiErr
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

	if err := finalizeCodexResponsesUsage(p.Usage, response); err != nil {
		var apiErr *types.OpenAIErrorWithStatusCode
		if !errors.As(err, &apiErr) || apiErr == nil {
			apiErr = common.ErrorWrapperLocal(err, commonresponses.ResponsesStreamTrackingFailureCode(err), http.StatusBadGateway)
		}
		apiErr.UpstreamAccepted = true
		return nil, apiErr
	}
	backfillCodexResponsePromptCacheKey(response, request)
	return response, nil
}

// codexResponsesProjection keeps raw body planning separate from the
// local typed projection used for downstream accounting and response shaping.
func codexResponsesProjection(req *commonresponses.Request) *types.OpenAIResponsesRequest {
	return commonresponses.ProjectRequest(req, strings.TrimSpace)
}

func (p *CodexProvider) prepareResponsesCreateRequest(ctx context.Context, req *commonresponses.Request) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	if req == nil || req.Body == nil || req.Body.Object == nil {
		return nil, common.StringErrorWrapperLocal("request body is required", "invalid_request_error", http.StatusBadRequest)
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(req.Body.Projection.Model)
	}
	policy := responsesPolicyInput(req)
	bodyObject := req.Body.Object
	effectiveReq := req
	if req.Control.Purpose == commonresponses.RequestPurposeChannelProbe {
		var errWithCode *types.OpenAIErrorWithStatusCode
		bodyObject, errWithCode = p.withCodexOfficialProbeMetadata(req, bodyObject)
		if errWithCode != nil {
			return nil, errWithCode
		}
		effectiveReq = requestWithResponsesBodyObject(req, bodyObject)
	}
	body, err := wire.PlanResponsesCreateBody(bodyObject, wire.CreateBodyInput{
		Model:       model,
		Stream:      true,
		PromptCache: policy.PromptCache,
	})
	if err != nil {
		return nil, codexWireError(err)
	}
	return p.prepareResponsesOfficialHTTPRequest(ctx, effectiveReq, wire.OpResponsesCreate, "", model, body)
}

func (p *CodexProvider) prepareResponsesCompactRequest(ctx context.Context, req *commonresponses.Request) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	if req == nil || req.Body == nil || req.Body.Object == nil {
		return nil, common.StringErrorWrapperLocal("request body is required", "invalid_request_error", http.StatusBadRequest)
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(req.Body.Projection.Model)
	}
	policy := responsesPolicyInput(req)
	body, err := wire.PlanResponsesCompactBody(req.Body.Object, model, policy.PromptCache)
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

func requestWithResponsesBodyObject(req *commonresponses.Request, object *jsonobject.Object) *commonresponses.Request {
	if req == nil || req.Body == nil {
		return req
	}
	clonedReq := *req
	clonedBody := *req.Body
	clonedBody.Object = object
	clonedReq.Body = &clonedBody
	return &clonedReq
}

func (p *CodexProvider) withCodexOfficialProbeMetadata(req *commonresponses.Request, object *jsonobject.Object) (*jsonobject.Object, *types.OpenAIErrorWithStatusCode) {
	if object == nil {
		return nil, common.StringErrorWrapperLocal("request body is required", "invalid_request_error", http.StatusBadRequest)
	}
	policy, err := p.codexOfficialChannelPolicy()
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "channel_config_error", http.StatusServiceUnavailable)
	}

	input := wire.ProbeMetadataInput{
		ChannelID: req.ChannelID,
		Clock:     wire.RealClock{},
	}
	if policy.AutoGenerate.InstallationID {
		principal, err := p.codexPrincipalFingerprint(req.Principal)
		if err != nil {
			return nil, common.ErrorWrapperLocal(err, "channel_config_error", http.StatusServiceUnavailable)
		}
		input.Principal = principal
		input.AutoGenerateInstallationID = true
	}

	updated, err := wire.WithOfficialProbeMetadata(object, input)
	if err != nil {
		return nil, codexWireError(err)
	}
	return updated, nil
}

func (p *CodexProvider) prepareResponsesOfficialHTTPRequest(ctx context.Context, req *commonresponses.Request, operation wire.Operation, pathSuffix, model string, body []byte) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	metadata, err := wire.MetadataFromResponsesBody(req.Body.Object)
	if err != nil {
		return nil, codexWireError(err)
	}
	multiAgentEnabled := false
	if operation == wire.OpResponsesCreate {
		// This projection only selects a protocol header. The raw request remains
		// authoritative, including provider-side validation of malformed values.
		multiAgentEnabled, _ = commonresponses.ProjectMultiAgentEnabled(req.Body.Object.Fields["multi_agent"])
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
		Policy:            policy,
		Identity:          identity,
		MultiAgentEnabled: multiAgentEnabled,
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
func (p *CodexProvider) collectResponsesStreamResponse(stream commonresponses.EventStream) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	if stream == nil {
		return nil, common.StringErrorWrapperLocal("response stream is required", "stream_read_failed", http.StatusInternalServerError)
	}
	defer requester.CloseAndDrainStream(stream)
	observer := commonresponses.NewStreamObserver()
	framer := commonresponses.NewSSEChunkFramer(codexResponsesStreamMaxEventBytes)
	var response *types.OpenAIResponsesResponses
	var eventError *types.OpenAIErrorWithStatusCode
	handleEvent := func(event string) (bool, error) {
		payload, hasData := commonresponses.SSEDataPayload(event)
		if !hasData || strings.TrimSpace(payload) == "" || strings.TrimSpace(payload) == "[DONE]" {
			return false, nil
		}
		var decoded types.OpenAIResponsesStreamResponses
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			eventError = codexAcceptedStreamError(err, "stream_decode_failed")
			return true, nil
		}
		if err := observer.AcceptRawEvent(event, func() error {
			return stream.ObserveAcceptedResponsesEvent(event)
		}); err != nil {
			eventError = codexAcceptedStreamError(err, "provider_protocol_error")
			return true, nil
		}
		if observer.TerminalKind() == commonresponses.StreamTerminalError {
			eventError = codexResponsesStreamProviderError(&decoded, []byte(payload), !observer.ProviderRejected())
			return true, nil
		}
		if observer.TerminalSeen() {
			response = decoded.Response
			if response != nil && strings.TrimSpace(response.ID) == "" {
				response.ID = observer.ObservedResponseID()
			}
			return true, nil
		}
		return false, nil
	}
	dataChan, errChan := stream.Recv()
	for dataChan != nil || errChan != nil {
		select {
		case data, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}
			stop, err := framer.PushChunk(data, handleEvent)
			if err != nil {
				return nil, codexAcceptedStreamError(err, commonresponses.ResponsesStreamTrackingFailureCode(err))
			}
			if eventError != nil {
				return nil, eventError
			}
			if stop {
				return response, nil
			}
		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err == nil {
				continue
			}
			if !errors.Is(err, io.EOF) {
				if errors.Is(err, requester.ErrSSEEventTooLarge) || errors.Is(err, requester.ErrStreamLineTooLarge) {
					return nil, codexAcceptedStreamError(err, "provider_usage_state_limit")
				}
				return nil, codexAcceptedStreamError(err, "stream_read_failed")
			}
			return nil, codexAcceptedStreamError(errors.New("no complete terminal response received"), "no_response")
		}
	}
	return nil, codexAcceptedStreamError(errors.New("no complete terminal response received"), "no_response")
}

func codexResponsesStreamProviderError(event *types.OpenAIResponsesStreamResponses, payload []byte, providerAccepted bool) *types.OpenAIErrorWithStatusCode {
	safePayload, _ := common.RedactSensitiveJSON(payload)
	detail := codexSupplierErrorDetailFromPayload(event, safePayload)
	status := detail.Status
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusBadGateway
	}
	message := common.RedactSensitiveText(detail.Message)
	if message == "" || message == "provider websocket error" {
		message = "provider rejected request"
	}
	errType := strings.TrimSpace(detail.Type)
	if errType == "" {
		errType = "provider_error"
	}
	code := strings.TrimSpace(detail.Code)
	if code == "" {
		code = errType
	}
	providerError := types.OpenAIError{
		Message: message,
		Type:    errType,
		Code:    code,
		Param:   strings.TrimSpace(detail.Param),
	}
	quotaExhausted := status == http.StatusPaymentRequired || common.ProviderErrorIsQuotaExhausted(providerError)
	rateLimited := status == http.StatusTooManyRequests || common.ProviderErrorIsRateLimited(providerError)
	authRejected := status == http.StatusUnauthorized || common.ProviderErrorIsAuthRejected(providerError)
	if detail.Status == 0 {
		switch {
		case quotaExhausted, rateLimited:
			status = http.StatusTooManyRequests
		case authRejected:
			status = http.StatusUnauthorized
		}
	}
	apiErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError:            providerError,
		StatusCode:             status,
		UpstreamAccepted:       providerAccepted || strings.TrimSpace(detail.ResponseID) != "",
		ProviderQuotaExhausted: quotaExhausted,
		ProviderRateLimited:    rateLimited,
		ProviderAuthRejected:   authRejected,
	}
	return providerresponse.SanitizeAPIError(apiErr)
}

func codexAcceptedStreamError(err error, code string) *types.OpenAIErrorWithStatusCode {
	var apiErr *types.OpenAIErrorWithStatusCode
	if errors.As(err, &apiErr) && apiErr != nil {
		cloned := *apiErr
		cloned.UpstreamAccepted = true
		return &cloned
	}
	errWithCode := common.ErrorWrapper(err, code, http.StatusBadGateway)
	errWithCode.UpstreamAccepted = true
	return errWithCode
}

func (h *CodexResponsesStreamHandler) HandlerResponsesStreamWithEmitter(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
	safe, _ := common.RedactCredentialValuesText(string(*rawLine), h.ProviderCredential)
	emitter.SendData(safe)
}
