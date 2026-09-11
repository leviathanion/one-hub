package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/providerendpoint"
	"one-api/common/providerresponse"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/common/utils"
	"one-api/model"
	providersBase "one-api/providers/base"
	runtimesession "one-api/runtime/session"
	"one-api/types"
)

type OpenAIResponsesStreamHandler struct {
	Usage              *types.Usage
	Prefix             string
	Model              string
	ProviderCredential string
	ServiceTier        string
	MessageID          string

	searchType         string
	searchServiceType  string
	imageTracker       commonresponses.ImageGenerationStreamTracker
	toolBillingTracker commonresponses.ToolBillingStreamTracker
	toolIndex          int
	hasToolCall        bool
}

var (
	responsesDataPrefix  = []byte("data:")
	responsesDonePayload = []byte("[DONE]")
	// These fields determine proxy-owned lifecycle admission, ownership, and
	// routing before the provider request is built. Channel transforms may tune
	// provider parameters, but they cannot change those already-frozen facts.
	responsesLifecycleFieldsOwnedByRelay = []string{
		"background",
		"conversation",
		"previous_response_id",
		"prompt",
		"store",
	}
)

type responsesLifecycleFieldState struct {
	present bool
	value   json.RawMessage
}

func appendURLPathSegment(rawURL string, segment string) (string, error) {
	parsed, err := providerendpoint.ParseResponsesURI(rawURL)
	if err != nil {
		return "", err
	}
	if segment == "" {
		return parsed.String(), nil
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	baseRawPath := strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.Path = basePath + "/" + segment
	candidateRawPath := baseRawPath + "/" + url.PathEscape(segment)
	parsed.RawPath = ""
	if decoded, decodeErr := url.PathUnescape(candidateRawPath); decodeErr == nil && decoded == parsed.Path && candidateRawPath != parsed.Path {
		parsed.RawPath = candidateRawPath
	}
	return parsed.String(), nil
}

func mergeURLRawQuery(rawURL string, additionalRawQuery string) (string, error) {
	parsed, err := providerendpoint.ParseResponsesURI(rawURL)
	if err != nil {
		return "", err
	}
	additionalRawQuery = strings.TrimPrefix(strings.TrimSpace(additionalRawQuery), "?")
	if additionalRawQuery == "" {
		return parsed.String(), nil
	}
	if parsed.RawQuery == "" {
		parsed.RawQuery = additionalRawQuery
	} else {
		parsed.RawQuery += "&" + additionalRawQuery
	}
	return parsed.String(), nil
}

func (p *OpenAIProvider) responsesRequestPath(rawReq *commonresponses.Request, basePath string, segments ...string) (string, error) {
	requestPath := basePath
	var err error
	for _, segment := range segments {
		requestPath, err = appendURLPathSegment(requestPath, segment)
		if err != nil {
			return "", err
		}
	}
	if rawReq == nil {
		return requestPath, nil
	}
	return mergeURLRawQuery(requestPath, rawReq.RawQuery)
}

func (p *OpenAIProvider) CreateResponses(ctx context.Context, rawReq *commonresponses.Request) (openaiResponse *types.OpenAIResponsesResponses, errWithCode *types.OpenAIErrorWithStatusCode) {
	request := responsesRequestProjection(rawReq)
	httpReq, errWithCode := p.buildResponsesCreateRequest(rawReq, request, false)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if ctx != nil {
		httpReq = p.Requester.WithRequestContext(httpReq, ctx)
	}
	defer httpReq.Body.Close()

	response := &types.OpenAIResponsesResponses{}
	if p.ProviderRawJSONReplay {
		response.EnableProviderRawJSONCapture()
	}
	// 发送请求
	var httpResponse *http.Response
	if p.preserveResponsesRedirect(rawReq) {
		httpResponse, errWithCode = p.Requester.SendRequestPreservingRedirect(httpReq, response, false)
	} else {
		httpResponse, errWithCode = p.Requester.SendRequest(httpReq, response, false)
	}
	if errWithCode != nil {
		return nil, errWithCode
	}
	// Responses create is typed for compatible channels and may also cross into
	// Chat; preserve representation headers only for exact raw replay.
	p.captureProviderResponseHeaders(httpResponse, p.ProviderRawJSONReplay)

	if response.Usage != nil {
		response.Usage.MarkProviderReported()
		*p.Usage = *response.Usage.ToOpenAIUsage()
	}
	response.ApplyUsageAttribution(p.Usage)

	getResponsesExtraBilling(response, p.Usage)
	if p.ProviderRawJSONReplay {
		response.EnableProviderRawJSONReplay()
	}

	return response, nil
}

func (p *OpenAIProvider) CreateResponsesStream(ctx context.Context, rawReq *commonresponses.Request) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	request := responsesRequestProjection(rawReq)
	req, errWithCode := p.buildResponsesCreateRequest(rawReq, request, true)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if ctx != nil {
		req = p.Requester.WithRequestContext(req, ctx)
	}
	defer req.Body.Close()

	return p.createResponsesStreamFromRequest(req, request)
}

func rawMessageBodyToInterfaceMap(body map[string]json.RawMessage) (map[string]interface{}, error) {
	if body == nil {
		return nil, nil
	}
	converted := make(map[string]interface{}, len(body))
	for key, value := range body {
		var decoded interface{}
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return nil, err
		}
		converted[key] = decoded
	}
	return converted, nil
}

func (p *OpenAIProvider) createResponsesStreamFromRequest(req *http.Request, request *types.OpenAIResponsesRequest) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	return p.createResponsesStreamFromRequestWithOptions(req, request, requester.StreamReadOptions{})
}

func (p *OpenAIProvider) createResponsesStreamFromRequestWithOptions(req *http.Request, request *types.OpenAIResponsesRequest, options requester.StreamReadOptions) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	// 发送请求
	streamRequester := p.Requester.ForHTTPProfile(requester.HTTPProfileLongStream)
	resp, errWithCode := streamRequester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}
	p.captureProviderResponseHeaders(resp)

	chatHandler := OpenAIResponsesStreamHandler{
		Usage:              p.Usage,
		Prefix:             `data: `,
		Model:              request.Model,
		ProviderCredential: p.Channel.Key,
	}

	if request.ConvertChat {
		options.RequireProtocolTerminal = true
		stream, apiErr := requester.RequestNoTrimStreamWithOptions(streamRequester, resp, chatHandler.ChatSSEHandler(chatHandler.ObserveResponsesEvent), options)
		return commonresponses.NewEventStream(stream, commonresponses.IgnoreResponsesEvent), apiErr
	}

	options.SSELines = true
	stream, apiErr := requester.RequestNoTrimStreamWithEmitterOptions(streamRequester, resp, chatHandler.HandlerResponsesStreamWithEmitter, options)
	return commonresponses.NewEventStream(stream, chatHandler.ObserveResponsesEvent), apiErr
}

func (p *OpenAIProvider) CompactResponses(ctx context.Context, rawReq *commonresponses.Request) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	request := responsesRequestProjection(rawReq)
	req, errWithCode := p.buildCompactResponsesRequest(rawReq, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if ctx != nil {
		req = p.Requester.WithRequestContext(req, ctx)
	}
	defer req.Body.Close()

	response := &types.OpenAIResponsesResponses{}
	if p.ProviderRawJSONReplay {
		response.EnableProviderRawJSONCapture()
	}
	var httpResponse *http.Response
	if p.preserveResponsesRedirect(rawReq) {
		httpResponse, errWithCode = p.Requester.SendRequestPreservingRedirect(req, response, false)
	} else {
		httpResponse, errWithCode = p.Requester.SendRequest(req, response, false)
	}
	if errWithCode != nil {
		return nil, errWithCode
	}
	// Compact follows the same typed/rewritten response path as create.
	p.captureProviderResponseHeaders(httpResponse, p.ProviderRawJSONReplay)

	if response.Usage != nil {
		response.Usage.MarkProviderReported()
		*p.Usage = *response.Usage.ToOpenAIUsage()
	}
	response.ApplyUsageAttribution(p.Usage)
	getResponsesExtraBilling(response, p.Usage)
	if p.ProviderRawJSONReplay {
		response.EnableProviderRawJSONReplay()
	}

	return response, nil
}

func (p *OpenAIProvider) CountResponsesInputTokens(ctx context.Context, rawReq *commonresponses.Request) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	request := responsesRequestProjection(rawReq)
	basePath, errWithCode := p.GetSupportedAPIUri(config.RelayModeResponses)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if errWithCode := p.validateAzureClassicAPIVersionForRequest(); errWithCode != nil {
		return nil, errWithCode
	}
	body, errWithCode := p.buildResponsesCreateBody(rawReq, request, false)
	if errWithCode != nil {
		return nil, errWithCode
	}
	requestPath, err := p.responsesRequestPath(rawReq, basePath, "input_tokens")
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_channel_config", http.StatusInternalServerError)
	}
	fullRequestURL := p.GetFullRequestURL(requestPath, request.Model)
	headers := p.GetRequestHeaders()
	httpReq, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(body), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	if rawReq != nil {
		if err := p.applyOpenAIHTTPHeaders(httpReq.Header, rawReq.Headers); err != nil {
			return nil, common.ErrorWrapperLocal(err, "invalid_request_header", http.StatusBadRequest)
		}
	}
	if ctx != nil {
		httpReq = p.Requester.WithRequestContext(httpReq, ctx)
	}
	if httpReq.Body != nil {
		defer httpReq.Body.Close()
	}
	var response *http.Response
	if p.ProviderRawJSONReplay {
		response, errWithCode = p.Requester.SendRequestRawCheckedPreservingRedirect(httpReq, providerresponse.OperationResponsesInputTokens)
	} else {
		response, errWithCode = p.Requester.SendRequestRaw(httpReq)
	}
	if errWithCode == nil {
		p.captureProviderResponseHeaders(response)
	}
	return response, errWithCode
}

func (p *OpenAIProvider) RelayStoredResponse(ctx context.Context, input providersBase.StoredResponsesRequest) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	responseID := strings.TrimSpace(input.ResponseID)
	if responseID == "" {
		return nil, common.StringErrorWrapperLocal("response id is required", "invalid_request_error", http.StatusBadRequest)
	}
	basePath, errWithCode := p.GetSupportedAPIUri(config.RelayModeResponses)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if errWithCode := p.validateAzureClassicAPIVersionForRequest(); errWithCode != nil {
		return nil, errWithCode
	}

	method := ""
	requestPath, err := appendURLPathSegment(basePath, responseID)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_channel_config", http.StatusInternalServerError)
	}
	switch input.Operation {
	case providersBase.OperationResponsesRetrieve:
		method = http.MethodGet
	case providersBase.OperationResponsesDelete:
		method = http.MethodDelete
	case providersBase.OperationResponsesInputItems:
		method = http.MethodGet
		requestPath, err = appendURLPathSegment(requestPath, "input_items")
		if err != nil {
			return nil, common.ErrorWrapperLocal(err, "invalid_channel_config", http.StatusInternalServerError)
		}
	default:
		return nil, common.StringErrorWrapperLocal("stored responses operation is unsupported", "invalid_request_error", http.StatusBadRequest)
	}
	requestPath, err = mergeURLRawQuery(requestPath, input.RawQuery)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_channel_config", http.StatusInternalServerError)
	}

	headers := p.GetRequestHeaders()
	requestURL := p.GetFullRequestURL(requestPath, "")
	httpReq, err := p.Requester.NewRequest(method, requestURL, p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	if err := p.applyOpenAIHTTPHeaders(httpReq.Header, input.Headers); err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_request_header", http.StatusBadRequest)
	}
	if input.Operation == providersBase.OperationResponsesRetrieve || input.Operation == providersBase.OperationResponsesInputItems {
		if err := applyResponsesConditionalReadHeaders(httpReq.Header, input.Headers); err != nil {
			return nil, common.ErrorWrapperLocal(err, "invalid_request_header", http.StatusBadRequest)
		}
	}
	if ctx != nil {
		httpReq = p.Requester.WithRequestContext(httpReq, ctx)
	}
	var response *http.Response
	if p.ProviderRawJSONReplay {
		response, errWithCode = p.Requester.SendRequestRawCheckedPreservingRedirect(httpReq, input.Operation)
	} else {
		response, errWithCode = p.Requester.SendRequestRawCheckedNoRedirect(httpReq)
	}
	if response != nil {
		p.captureProviderResponseHeaders(response)
	}
	return response, errWithCode
}

func (p *OpenAIProvider) preserveResponsesRedirect(req *commonresponses.Request) bool {
	return p != nil && p.ProviderRawJSONReplay && req != nil && req.Control.DownstreamDialect == commonresponses.DownstreamResponses
}

func responsesRequestProjection(req *commonresponses.Request) *types.OpenAIResponsesRequest {
	return commonresponses.ProjectRequest(req, nil)
}

func (p *OpenAIProvider) buildResponsesCreateRequest(rawReq *commonresponses.Request, request *types.OpenAIResponsesRequest, stream bool) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	if request == nil {
		request = &types.OpenAIResponsesRequest{}
	}
	basePath, errWithCode := p.GetSupportedAPIUri(config.RelayModeResponses)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if errWithCode := p.validateAzureClassicAPIVersionForRequest(); errWithCode != nil {
		return nil, errWithCode
	}

	requestPath, err := p.responsesRequestPath(rawReq, basePath)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_channel_config", http.StatusInternalServerError)
	}
	fullRequestURL := p.GetFullRequestURL(requestPath, request.Model)
	headers := p.GetRequestHeaders()

	bodyMap, errWithCode := p.buildResponsesCreateBody(rawReq, request, stream)
	if errWithCode != nil {
		return nil, errWithCode
	}

	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(bodyMap), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	if rawReq != nil {
		if err := p.applyOpenAIHTTPHeaders(req.Header, rawReq.Headers); err != nil {
			return nil, common.ErrorWrapperLocal(err, "invalid_request_header", http.StatusBadRequest)
		}
	}
	return req, nil
}

func (p *OpenAIProvider) buildResponsesCreateBody(rawReq *commonresponses.Request, request *types.OpenAIResponsesRequest, stream bool) (map[string]interface{}, *types.OpenAIErrorWithStatusCode) {
	bodyMap, err := rawResponsesBodyMap(rawReq)
	if err != nil {
		return nil, common.ErrorWrapper(err, "decode_request_failed", http.StatusInternalServerError)
	}
	if bodyMap == nil {
		bodyMap = make(map[string]interface{})
	}

	if _, exists := bodyMap["prompt_cache_key"]; !exists && rawReq != nil && rawReq.Policy.PromptCache != nil {
		if key := strings.TrimSpace(rawReq.Policy.PromptCache.Key); key != "" {
			bodyMap["prompt_cache_key"] = key
		}
	}

	customParams, err := p.CustomParameterHandler()
	if err != nil {
		return nil, common.ErrorWrapper(err, "custom_parameter_error", http.StatusInternalServerError)
	}
	if customParams != nil {
		var field string
		var changed bool
		inputTokens := rawReq != nil && rawReq.Operation == commonresponses.ResponsesInputTokens
		applyPreAdd := rawReq == nil || rawReq.Control.DownstreamDialect != commonresponses.DownstreamChatCompletions
		bodyMap, field, changed, err = applyResponsesCustomParameters(bodyMap, customParams, request.Model, inputTokens, applyPreAdd)
		if err != nil {
			return nil, common.ErrorWrapper(err, "custom_parameter_error", http.StatusInternalServerError)
		}
		if changed {
			return nil, responsesLifecycleCustomParameterError(field)
		}
	}
	if model := strings.TrimSpace(request.Model); model != "" {
		bodyMap["model"] = model
	}
	if stream {
		bodyMap["stream"] = true
	}
	if err := commonresponses.ValidateNoAccountScopedResources(bodyMap); err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "unsupported_resource_reference", http.StatusBadRequest)
	}
	return bodyMap, nil
}

func applyResponsesCustomParameters(bodyMap map[string]interface{}, customParams map[string]interface{}, modelName string, inputTokens, applyPreAdd bool) (map[string]interface{}, string, bool, error) {
	lifecycleFields := responsesLifecycleFieldsOwnedByRelay
	if inputTokens {
		lifecycleFields = []string{"previous_response_id"}
	}
	lifecycleBefore, err := snapshotResponsesLifecycleFields(bodyMap, lifecycleFields)
	if err != nil {
		return nil, "", false, err
	}
	bodyMap = providersBase.ApplyCustomParams(bodyMap, customParams, modelName, applyPreAdd)
	field, changed, err := changedResponsesLifecycleField(lifecycleBefore, bodyMap, lifecycleFields)
	return bodyMap, field, changed, err
}

// ValidateResponsesCustomParameterCompatibility performs the same lifecycle
// freeze used by request construction, but on a disposable body projection so
// routing can skip an incompatible candidate before provider work.
func ValidateResponsesCustomParameterCompatibility(channel *model.Channel, fields map[string]json.RawMessage, modelName string, operation providerresponse.Operation, applyPreAdd bool) error {
	if channel == nil {
		return nil
	}
	customParams, err := channel.GetCustomParameterMap()
	if err != nil {
		return &providersBase.RequestCapabilityError{Param: "custom_parameter", Message: "channel custom parameters are invalid"}
	}
	if len(customParams) == 0 {
		return nil
	}
	bodyMap, err := rawMessageBodyToInterfaceMap(fields)
	if err != nil {
		return &providersBase.RequestCapabilityError{Param: "request", Message: "request cannot be checked against channel custom parameters"}
	}
	if bodyMap == nil {
		bodyMap = make(map[string]interface{})
	}
	var field string
	var changed bool
	bodyMap, field, changed, err = applyResponsesCustomParameters(bodyMap, customParams, modelName, operation == providerresponse.OperationResponsesInputTokens, applyPreAdd)
	if err != nil {
		return &providersBase.RequestCapabilityError{Param: "custom_parameter", Message: "channel custom parameters cannot be evaluated"}
	}
	if changed {
		return &providersBase.RequestCapabilityError{Param: field, Message: "channel custom parameters cannot modify Responses lifecycle fields"}
	}
	if err := commonresponses.ValidateNoAccountScopedResources(bodyMap); err != nil {
		return &providersBase.RequestCapabilityError{Param: "custom_parameter", Message: err.Error()}
	}
	return nil
}

func snapshotResponsesLifecycleFields(body map[string]interface{}, lifecycleFields []string) (map[string]responsesLifecycleFieldState, error) {
	snapshot := make(map[string]responsesLifecycleFieldState, len(lifecycleFields))
	for _, field := range lifecycleFields {
		value, present := body[field]
		state := responsesLifecycleFieldState{present: present}
		if present {
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			state.value = encoded
		}
		snapshot[field] = state
	}
	return snapshot, nil
}

func changedResponsesLifecycleField(before map[string]responsesLifecycleFieldState, body map[string]interface{}, lifecycleFields []string) (string, bool, error) {
	for _, field := range lifecycleFields {
		prior := before[field]
		value, present := body[field]
		if prior.present != present {
			return field, true, nil
		}
		if !present {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", false, err
		}
		if !bytes.Equal(prior.value, encoded) {
			return field, true, nil
		}
	}
	return "", false, nil
}

func responsesLifecycleCustomParameterError(field string) *types.OpenAIErrorWithStatusCode {
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Message: "channel custom parameters cannot modify Responses lifecycle fields",
			Type:    "channel_error",
			Param:   field,
			Code:    "responses_lifecycle_custom_parameter_conflict",
		},
		StatusCode: http.StatusServiceUnavailable,
		LocalError: true,
	}
}

func rawResponsesBodyMap(req *commonresponses.Request) (map[string]interface{}, error) {
	if req == nil || req.Body == nil || req.Body.Object == nil {
		return nil, nil
	}
	return rawMessageBodyToInterfaceMap(req.Body.Object.Fields)
}

func (p *OpenAIProvider) buildCompactResponsesRequest(rawReq *commonresponses.Request, request *types.OpenAIResponsesRequest) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	basePath, errWithCode := p.GetSupportedAPIUri(config.RelayModeResponses)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if errWithCode := p.validateAzureClassicAPIVersionForRequest(); errWithCode != nil {
		return nil, errWithCode
	}

	requestPath, err := p.responsesRequestPath(rawReq, basePath, "compact")
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_channel_config", http.StatusInternalServerError)
	}
	fullRequestURL := p.GetFullRequestURL(requestPath, request.Model)
	headers := p.GetRequestHeaders()

	bodyMap, errWithCode := p.buildResponsesCreateBody(rawReq, request, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(bodyMap), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	if rawReq != nil {
		if err := p.applyOpenAIHTTPHeaders(req.Header, rawReq.Headers); err != nil {
			return nil, common.ErrorWrapperLocal(err, "invalid_request_header", http.StatusBadRequest)
		}
	}

	return req, nil
}

func (h *OpenAIResponsesStreamHandler) HandlerResponsesStreamWithEmitter(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
	emitter.SendData(h.safeProviderEvent(string(*rawLine)))
}

func (h *OpenAIResponsesStreamHandler) ObserveResponsesEvent(rawEvent string) error {
	if err := h.observeResponsesEvent(rawEvent); err != nil {
		return responsesUsageTrackingError(err)
	}
	return nil
}

func (h *OpenAIResponsesStreamHandler) observeResponsesEvent(rawEvent string) error {
	payload, ok := commonresponses.SSEDataPayload(rawEvent)
	if !ok {
		return nil
	}
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == string(responsesDonePayload) {
		return nil
	}

	openaiResponse, ok := commonresponses.ParseStreamUsageEvent([]byte(payload))
	if !ok {
		return nil
	}
	serviceType, searchType := commonresponses.ResponsesSearchBilling(openaiResponse.Response)
	if openaiResponse.Type == "response.created" && serviceType != "" {
		if err := commonresponses.ValidateResponsesStreamToolBillingDimensions(serviceType, searchType); err != nil {
			return err
		}
	}
	candidateImage := h.imageTracker
	imageErr := candidateImage.ObserveUsageEvent(openaiResponse)
	trackingErr := commonresponses.ObserveBillingFailure(h.Usage, types.APIToolTypeImageGeneration, imageErr)
	switch openaiResponse.Type {
	case "response.created":
		if h.Usage != nil && openaiResponse.Response != nil {
			openaiResponse.Response.ApplyUsageAttribution(h.Usage)
		}
		if serviceType != "" {
			h.searchServiceType = strings.Clone(serviceType)
			h.searchType = strings.Clone(searchType)
		}
	case "response.output_item.added", "response.output_item.done":
		if err := commonresponses.ApplyResponsesStreamOutputItemBillingWithToolTracker(h.Usage, openaiResponse.Type, openaiResponse.Item, openaiResponse.ItemID, openaiResponse.OutputIndex, h.searchServiceType, h.searchType, &h.toolBillingTracker); err != nil {
			service := h.searchServiceType
			if service == "" {
				service = types.APIToolTypeWebSearchPreview
			}
			trackingErr = errors.Join(trackingErr, commonresponses.ObserveBillingFailure(h.Usage, service, err))
		}
	default:
		// This observer is the provider-owned acceptance boundary for Responses
		// stream usage. Decode presence alone is not billing authority; only usage
		// carried by an accepted terminal response is marked before the shared
		// projection copies its fields into the attempt-local Usage. Partial image
		// events may carry a response snapshot for tracker context, but they are
		// not a token terminal and cannot authorize token billing.
		terminalUsageEvent := openaiResponse.Type == "response.completed" || openaiResponse.Type == "response.failed" || openaiResponse.Type == "response.incomplete"
		if terminalUsageEvent && openaiResponse.Response != nil && openaiResponse.Response.Usage != nil {
			openaiResponse.Response.Usage.MarkProviderReported()
		}
		commonresponses.ApplyResponsesUsageWithImageTracker(h.Usage, openaiResponse.Response, &candidateImage)
	}
	h.imageTracker = candidateImage
	return trackingErr
}

func responsesUsageTrackingError(err error) error {
	return common.ErrorWrapperLocal(err, commonresponses.ResponsesStreamTrackingFailureCode(err), http.StatusBadGateway)
}

func (h *OpenAIResponsesStreamHandler) safeProviderEvent(event string) string {
	safe, _ := common.RedactCredentialValuesText(event, h.ProviderCredential)
	return safe
}

func (h *OpenAIResponsesStreamHandler) sseDataPayload(rawLine []byte) ([]byte, bool) {
	prefix := []byte(strings.TrimSpace(h.Prefix))
	if len(prefix) == 0 {
		prefix = responsesDataPrefix
	}
	line := bytes.TrimSpace(rawLine)
	if !bytes.HasPrefix(line, prefix) {
		return nil, false
	}
	return bytes.TrimSpace(line[len(prefix):]), true
}

func openAIStreamDeltaString(delta any) (string, bool) {
	text, ok := delta.(string)
	return text, ok
}

func (h *OpenAIResponsesStreamHandler) ChatSSEHandler(observe func(string) error) requester.HandlerPrefix[string] {
	framer := commonresponses.NewSSEChunkFramer(16 << 20)
	observer := commonresponses.NewStreamObserver()
	return func(rawLine *[]byte, dataChan chan string, errChan chan error) {
		stop, err := framer.PushChunk(string(*rawLine), func(event string) (bool, error) {
			payload, ok := commonresponses.SSEDataPayload(event)
			if !ok || strings.TrimSpace(payload) == "" || strings.TrimSpace(payload) == "[DONE]" {
				return false, nil
			}
			line := []byte("data: " + payload)
			h.handleChatStream(&line, dataChan, errChan, func() (bool, error) {
				err := observer.ObserveEvent(event)
				if err == nil {
					err = observe(event)
				}
				return !observer.ProviderRejected(), err
			})
			return bytes.Equal(line, requester.StreamClosed), nil
		})
		if err != nil {
			errChan <- responsesUsageTrackingError(err)
		}
		if stop || err != nil {
			*rawLine = requester.StreamClosed
		}
	}
}

func (h *OpenAIResponsesStreamHandler) handleChatStream(rawLine *[]byte, dataChan chan string, errChan chan error, accept func() (bool, error)) {
	payload, ok := h.sseDataPayload(*rawLine)
	if !ok || len(payload) == 0 || bytes.Equal(payload, responsesDonePayload) {
		*rawLine = nil
		return
	}
	if safe, changed := common.RedactCredentialValuesText(string(payload), h.ProviderCredential); changed {
		payload = []byte(safe)
	}
	*rawLine = payload

	var openaiResponse types.OpenAIResponsesStreamResponses
	err := json.Unmarshal(*rawLine, &openaiResponse)
	if err != nil {
		*rawLine = requester.StreamClosed
		errChan <- common.ErrorToOpenAIError(err)
		return
	}
	providerAccepted, err := accept()
	if err != nil {
		*rawLine = requester.StreamClosed
		var apiErr *types.OpenAIErrorWithStatusCode
		if errors.As(err, &apiErr) && apiErr != nil {
			failure := *apiErr
			failure.UpstreamAccepted = true
			errChan <- &failure
		} else {
			failure := common.ErrorWrapperLocal(err, "invalid_provider_response", http.StatusBadGateway)
			failure.UpstreamAccepted = true
			errChan <- failure
		}
		return
	}
	if openaiResponse.Response != nil {
		if modelName := strings.TrimSpace(openaiResponse.Response.Model); modelName != "" {
			h.Model = modelName
		}
		if serviceTier := strings.TrimSpace(openaiResponse.Response.ServiceTier); serviceTier != "" {
			h.ServiceTier = serviceTier
		}
	}

	chatRes := types.ChatCompletionStreamResponse{
		ID:          h.MessageID,
		Object:      "chat.completion.chunk",
		Created:     utils.GetTimestamp(),
		Model:       h.Model,
		ServiceTier: h.ServiceTier,
		Choices:     make([]types.ChatCompletionStreamChoice, 0),
	}
	needOutput := false
	terminal := false

	switch openaiResponse.Type {
	case types.EventTypeError:
		apiErr := runtimesession.ProviderAPIErrorFromPayload(*rawLine)
		safeErr := providerresponse.SanitizeAPIError(apiErr)
		if safeErr != nil {
			safeErr.UpstreamAccepted = safeErr.UpstreamAccepted || providerAccepted
		}
		*rawLine = requester.StreamClosed
		errChan <- safeErr
		return
	case "response.created":
		h.hasToolCall = false
		h.toolIndex = 0
		if openaiResponse.Response != nil {
			if h.MessageID == "" {
				h.MessageID = openaiResponse.Response.ID
				chatRes.ID = h.MessageID
			}
		}
		chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
			Index: 0,
			Delta: types.ChatCompletionStreamChoiceDelta{},
		})
		needOutput = true
	case "response.output_text.delta": // 处理文本输出的增量
		delta, _ := openAIStreamDeltaString(openaiResponse.Delta)
		chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
			Index: 0,
			Delta: types.ChatCompletionStreamChoiceDelta{
				Content: delta,
			},
		})
		needOutput = true
	case "response.refusal.delta":
		delta, _ := openAIStreamDeltaString(openaiResponse.Delta)
		chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
			Index: 0,
			Delta: types.ChatCompletionStreamChoiceDelta{Refusal: delta},
		})
		needOutput = true
	case "response.reasoning_summary_text.delta": // 处理文本输出的增量
		delta, _ := openAIStreamDeltaString(openaiResponse.Delta)
		chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
			Index: 0,
			Delta: types.ChatCompletionStreamChoiceDelta{
				ReasoningContent: delta,
			},
		})
		needOutput = true
	case "response.function_call_arguments.delta": // 处理函数调用参数的增量
		h.hasToolCall = true
		delta, _ := openAIStreamDeltaString(openaiResponse.Delta)
		chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
			Index: 0,
			Delta: types.ChatCompletionStreamChoiceDelta{
				Role: types.ChatMessageRoleAssistant,
				ToolCalls: []*types.ChatCompletionToolCalls{
					{
						Index: h.toolIndex,
						Function: &types.ChatCompletionToolCallsFunction{
							Arguments: delta,
						},
					},
				},
			},
		})
		needOutput = true
	case "response.function_call_arguments.done":
		h.hasToolCall = true
		h.toolIndex++
	case "response.custom_tool_call_input.delta":
		h.hasToolCall = true
		delta, _ := openAIStreamDeltaString(openaiResponse.Delta)
		chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
			Index: 0,
			Delta: types.ChatCompletionStreamChoiceDelta{
				Role: types.ChatMessageRoleAssistant,
				ToolCalls: []*types.ChatCompletionToolCalls{
					{
						Index: h.toolIndex,
						Custom: &types.ChatCompletionToolCallsCustom{
							Input: delta,
						},
					},
				},
			},
		})
		needOutput = true
	case "response.custom_tool_call_input.done":
		// done 携带完整输入，重复输出会使 Chat 客户端再次拼接；这里只关闭当前调用槽位。
		h.hasToolCall = true
		h.toolIndex++
	case "response.output_item.added":
		if openaiResponse.Item != nil {
			switch openaiResponse.Item.Type {
			case types.InputTypeMessage, types.InputTypeReasoning:
				chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
					Index: 0,
					Delta: types.ChatCompletionStreamChoiceDelta{
						Role:    types.ChatMessageRoleAssistant,
						Content: "",
					},
				})
				needOutput = true
			case types.InputTypeFunctionCall:
				h.hasToolCall = true
				arguments := ""
				if openaiResponse.Item.Arguments != nil {
					arguments = *openaiResponse.Item.Arguments
				}

				chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
					Index: 0,
					Delta: types.ChatCompletionStreamChoiceDelta{
						Role: types.ChatMessageRoleAssistant,
						ToolCalls: []*types.ChatCompletionToolCalls{
							{
								Index: h.toolIndex,
								Id:    openaiResponse.Item.CallID,
								Type:  "function",
								Function: &types.ChatCompletionToolCallsFunction{
									Name:      openaiResponse.Item.Name,
									Arguments: arguments,
								},
							},
						},
					},
				})
				needOutput = true
			case types.InputTypeCustomToolCall:
				h.hasToolCall = true
				chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
					Index: 0,
					Delta: types.ChatCompletionStreamChoiceDelta{
						Role: types.ChatMessageRoleAssistant,
						ToolCalls: []*types.ChatCompletionToolCalls{
							{
								Index: h.toolIndex,
								Id:    openaiResponse.Item.CallID,
								Type:  types.ToolChoiceTypeCustom,
								Custom: &types.ChatCompletionToolCallsCustom{
									Name:  openaiResponse.Item.Name,
									Input: openaiResponse.Item.Input,
								},
							},
						},
					},
				})
				needOutput = true
			}
		}
	case "response.output_item.done":
		if openaiResponse.Item != nil {
			switch openaiResponse.Item.Type {
			case types.InputTypeFunctionCall, types.InputTypeCustomToolCall:
				h.hasToolCall = true
			}
		}
	case "response.completed", "response.failed", "response.incomplete":
		terminal = true
		if openaiResponse.Response != nil {
			if terminalErr := commonresponses.ChatTerminalError(openaiResponse.Response); terminalErr != nil {
				*rawLine = requester.StreamClosed
				errChan <- terminalErr
				return
			}
			finishReason := types.ConvertResponsesStatusToChat(openaiResponse.Response.Status)
			if finishReason == types.FinishReasonStop && shouldUseToolCallsFinishReason(openaiResponse.Response, h.hasToolCall) {
				finishReason = types.FinishReasonToolCalls
			}
			chatRes.Choices = append(chatRes.Choices, types.ChatCompletionStreamChoice{
				Index:        0,
				Delta:        types.ChatCompletionStreamChoiceDelta{},
				FinishReason: finishReason,
			})
			needOutput = true
		}
	}

	if needOutput {
		jsonData, err := json.Marshal(chatRes)
		if err != nil {
			errChan <- common.ErrorToOpenAIError(err)
			return
		}
		dataChan <- string(jsonData)
	}
	if terminal {
		*rawLine = requester.StreamClosed
		errChan <- io.EOF
		return
	}
	if needOutput {
		return
	}

	*rawLine = nil
}

func shouldUseToolCallsFinishReason(response *types.OpenAIResponsesResponses, hasToolCall bool) bool {
	if hasToolCall {
		return true
	}

	if response == nil {
		return false
	}

	for _, output := range response.Output {
		if output.Type == types.InputTypeFunctionCall || output.Type == types.InputTypeCustomToolCall {
			return true
		}
	}

	return false
}

func getResponsesExtraBilling(response *types.OpenAIResponsesResponses, usage *types.Usage) {
	if usage == nil {
		return
	}
	types.ApplyResponsesExtraBilling(response, usage)
}
