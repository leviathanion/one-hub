package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/providerendpoint"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
	"strings"

	"one-api/providers/base"
)

type OpenAIProviderFactory struct{}

func (OpenAIProviderFactory) AssessChatRemoteMedia(_ *model.Channel, _ *types.ChatCompletionRequest, _ base.ChatRemoteMediaSummary) (base.RemoteMediaMode, error) {
	return base.RemoteMediaPassURL, nil
}

type UsageHandler func(usage *types.Usage) (ForcedFormatting bool)
type RequestHandleBefore func(request *types.ChatCompletionRequest) (errWithCode *types.OpenAIErrorWithStatusCode)

type openAIRequestAuthMode int

const (
	openAIRequestAuthDefault openAIRequestAuthMode = iota
	openAIRequestAuthBearer
	openAIRequestAuthAzureAPIKey
)

type OpenAIProvider struct {
	base.BaseProvider
	IsAzure              bool
	BalanceAction        bool
	SupportStreamOptions bool
	StreamEscapeJSON     bool
	ReasoningHandler     bool
	// RequireOpenAIStreamTerminal is enabled only for adapters whose Chat and
	// legacy Completions SSE contract requires an explicit [DONE] marker.
	// Compatible endpoints may use transport EOF as their successful terminal.
	RequireOpenAIStreamTerminal bool
	// ProviderRawJSONReplay 仅由确认响应无需 adapter 规范化的 exact-wire provider 开启。
	ProviderRawJSONReplay bool
	UsageHandler          UsageHandler
	RequestHandleBefore   RequestHandleBefore
}

var _ base.RawRelayURLBuilder = (*OpenAIProvider)(nil)

// 创建 OpenAIProvider
func (f OpenAIProviderFactory) Create(channel *model.Channel) base.ProviderInterface {
	openAIProvider := CreateOpenAIProvider(channel, "https://api.openai.com")
	openAIProvider.BalanceAction = true
	return openAIProvider
}

// 创建 OpenAIProvider
// https://platform.openai.com/docs/api-reference/introduction
func CreateOpenAIProvider(channel *model.Channel, baseURL string) *OpenAIProvider {
	openaiConfig := getOpenAIConfig(baseURL, channel)

	openAIProvider := &OpenAIProvider{
		BaseProvider: base.BaseProvider{
			Config:    openaiConfig,
			Channel:   channel,
			Requester: requester.NewHTTPRequester(*channel.Proxy, RequestErrorHandle),
		},
		IsAzure:       false,
		BalanceAction: true,
	}
	trustedOpenAIWire := isOfficialOpenAIWireChannel(channel, baseURL)
	openAIProvider.RequireOpenAIStreamTerminal = trustedOpenAIWire || channel.Type == config.ChannelTypeAzure || channel.Type == config.ChannelTypeAzureV1
	openAIProvider.SetProviderRawJSONReplay(trustedOpenAIWire)
	openAIProvider.SetOpenAIErrorEnvelopeReplay(trustedOpenAIWire)

	if channel.Type == config.ChannelTypeOpenAI {
		openAIProvider.SupportStreamOptions = true
	}

	return openAIProvider
}

func (p *OpenAIProvider) SetProviderRawJSONReplay(enabled bool) {
	if p == nil {
		return
	}
	p.ProviderRawJSONReplay = enabled
}

func (p *OpenAIProvider) SetOpenAIErrorEnvelopeReplay(enabled bool) {
	if p == nil {
		return
	}
	if p.Requester != nil {
		p.Requester.ReplayOpenAIErrorEnvelopes = enabled
	}
}

func isOfficialOpenAIWireChannel(channel *model.Channel, defaultBaseURL string) bool {
	if channel == nil || channel.Type != config.ChannelTypeOpenAI {
		return false
	}
	baseURL := strings.TrimSpace(channel.GetBaseURL())
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return model.IsOfficialOpenAIBaseURL(baseURL)
}

func getOpenAIConfig(baseURL string, channel *model.Channel) base.ProviderConfig {
	providerConfig := base.ProviderConfig{
		BaseURL:      baseURL,
		ModelList:    "/v1/models",
		ChatRealtime: "/v1/realtime",
	}
	relayModeMap := map[int]*string{
		config.RelayModeChatCompletions:    &providerConfig.ChatCompletions,
		config.RelayModeCompletions:        &providerConfig.Completions,
		config.RelayModeEmbeddings:         &providerConfig.Embeddings,
		config.RelayModeAudioSpeech:        &providerConfig.AudioSpeech,
		config.RelayModeAudioTranscription: &providerConfig.AudioTranscriptions,
		config.RelayModeAudioTranslation:   &providerConfig.AudioTranslations,
		config.RelayModeModerations:        &providerConfig.Moderation,
		config.RelayModeImagesGenerations:  &providerConfig.ImagesGenerations,
		config.RelayModeImagesEdits:        &providerConfig.ImagesEdit,
		config.RelayModeImagesVariations:   &providerConfig.ImagesVariations,
		config.RelayModeResponses:          &providerConfig.Responses,
	}

	for _, definition := range providerendpoint.Definitions() {
		target, managed := relayModeMap[definition.RelayMode]
		if !managed {
			continue
		}
		*target = definition.DefaultPath
		if channel.Type == config.ChannelTypeCustom {
			uri, err := channel.ResolveEndpoint(definition.ID)
			*target = uri
			if err != nil && providerConfig.EndpointError == nil {
				providerConfig.EndpointError = err
			}
		}
	}

	return providerConfig
}

// 请求错误处理
func RequestErrorHandle(resp *http.Response) *types.OpenAIError {
	errorResponse := &types.OpenAIErrorResponse{}
	err := json.NewDecoder(resp.Body).Decode(errorResponse)
	if err != nil {
		return nil
	}

	return ErrorHandle(errorResponse)
}

// 错误处理
func ErrorHandle(openaiError *types.OpenAIErrorResponse) *types.OpenAIError {
	if openaiError.Error.Message == "" {
		return nil
	}
	return &openaiError.Error
}

// 获取完整请求 URL
func (p *OpenAIProvider) GetFullRequestURL(requestURL string, modelName string) string {
	baseURL := strings.TrimSuffix(p.GetBaseURL(), "/")
	azureAPIVersion := p.azureClassicAPIVersionOrEmpty()

	if strings.Contains(modelName, "-realtime") {
		if !p.IsAzure {
			return providerendpoint.NonAzureRealtimeRequestURL(p.GetBaseURL(), requestURL, modelName)
		}
		if strings.HasPrefix(baseURL, "https://") {
			baseURL = strings.Replace(baseURL, "https://", "wss://", 1)
		} else {
			baseURL = strings.Replace(baseURL, "http://", "ws://", 1)
		}

		// wss://my-eastus2-openai-resource.openai.azure.com/openai/realtime?api-version=2024-10-01-preview&deployment=gpt-4o-realtime-preview-1001
		requestURL = fmt.Sprintf("/openai/%s?api-version=%s&deployment=%s", requestURL, azureAPIVersion, modelName)

		return fmt.Sprintf("%s%s", baseURL, requestURL)
	}

	if p.Channel != nil && p.Channel.Type == config.ChannelTypeCustom {
		return providerendpoint.CustomRequestURL(p.GetBaseURL(), requestURL)
	}

	if p.IsAzure && p.Channel != nil && p.Channel.Type == config.ChannelTypeAzureV1 {
		return azureV1FullRequestURL(baseURL, requestURL)
	}

	if p.IsAzure {
		apiVersion := azureAPIVersion
		if isAzureClassicResponsesHTTPPath(requestURL) {
			requestURL = azureClassicHTTPRequestURL(requestURL, apiVersion)
		} else if modelName != "" {
			// 检测模型是是否包含 . 如果有则直接去掉
			// modelName = strings.Replace(modelName, ".", "", -1)

			if modelName == "dall-e-2" {
				// 因为dall-e-3需要api-version=2023-12-01-preview，但是该版本
				// 已经没有dall-e-2了，所以暂时写死
				requestURL = fmt.Sprintf("/openai/%s:submit?api-version=2023-09-01-preview", requestURL)
			} else {
				if strings.HasPrefix(requestURL, "/v1") {
					requestURL = fmt.Sprintf("/openai/%s?api-version=%s", requestURL, apiVersion)
				} else {
					requestURL = fmt.Sprintf("/openai/deployments/%s%s?api-version=%s", modelName, requestURL, apiVersion)
				}
			}
		} else {
			if strings.Contains(requestURL, "isGetAzureModelList") {
				//专门生成用于azure获取模型部署列表的URL，因为azure只有2023-03-15-preview版本等特定版本支持通过api-key获取models 所以本url固定写死
				requestURL = "/openai/deployments?api-version=2023-03-15-preview"
			} else {
				requestURL = azureClassicHTTPRequestURL(requestURL, apiVersion)
			}
		}
	}

	if strings.HasPrefix(baseURL, "https://gateway.ai.cloudflare.com") {
		if p.IsAzure {
			requestURL = strings.TrimPrefix(requestURL, "/openai")
			requestURL = strings.TrimPrefix(requestURL, "/deployments")
		} else {
			requestURL = strings.TrimPrefix(requestURL, "/v1")
		}
	}

	return fmt.Sprintf("%s%s", baseURL, requestURL)
}

func (p *OpenAIProvider) BuildRawRelayURL(escapedPath, rawQuery string) (string, error) {
	if p == nil || p.Channel == nil {
		return "", fmt.Errorf("raw relay provider is not initialized")
	}
	switch p.Channel.Type {
	case config.ChannelTypeOpenAI, config.ChannelTypeAzure, config.ChannelTypeAzureV1:
	default:
		return "", fmt.Errorf("channel type does not support raw resource relay")
	}
	escapedPath = strings.TrimSpace(escapedPath)
	if escapedPath == "" || !strings.HasPrefix(escapedPath, "/") || strings.ContainsAny(escapedPath, "?#") {
		return "", fmt.Errorf("raw relay path must be an absolute-path reference")
	}
	requestURL := escapedPath
	if rawQuery != "" {
		requestURL += "?" + rawQuery
	}
	fullURL := p.GetFullRequestURL(requestURL, "")
	parsed, err := url.Parse(fullURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("raw relay upstream URL is invalid")
	}
	return parsed.String(), nil
}

func isAzureClassicResponsesHTTPPath(requestURL string) bool {
	path := azureClassicHTTPPathCandidate(requestURL)
	return path == "/responses" || strings.HasPrefix(path, "/responses/")
}

func azureClassicHTTPRequestURL(requestURL string, apiVersion string) string {
	parsed, err := url.Parse(strings.TrimSpace(requestURL))
	if err != nil {
		return fmt.Sprintf("/openai%s?api-version=%s", azureClassicHTTPPathCandidate(requestURL), apiVersion)
	}
	parsed.Path = "/openai" + azureClassicHTTPPath(parsed.Path)
	parsed.RawPath = ""
	parsed.Fragment = ""
	query := parsed.Query()
	query.Set("api-version", apiVersion)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func azureClassicHTTPPathCandidate(requestURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(requestURL))
	if err == nil {
		return azureClassicHTTPPath(parsed.Path)
	}
	path := strings.TrimSpace(requestURL)
	if index := strings.IndexAny(path, "?#"); index >= 0 {
		path = path[:index]
	}
	return azureClassicHTTPPath(path)
}

func azureClassicHTTPPath(path string) string {
	path = "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	if path == "/v1" {
		return ""
	}
	if strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

func azureV1FullRequestURL(baseURL string, requestURL string) string {
	request, requestErr := url.Parse(strings.TrimSpace(requestURL))
	requestPath := strings.TrimSpace(requestURL)
	requestQuery := ""
	requestRawPath := ""
	if requestErr == nil {
		requestPath = request.Path
		requestRawPath = request.EscapedPath()
		requestQuery = request.RawQuery
	}
	normalizedPath := "/" + strings.TrimLeft(strings.TrimSpace(requestPath), "/")
	if normalizedPath == "/1" {
		normalizedPath = "/v1/models"
	}
	normalizedPath = strings.TrimPrefix(normalizedPath, "/openai")
	if !strings.HasPrefix(normalizedPath, "/v1") {
		normalizedPath = "/v1" + normalizedPath
	}
	normalizedRawPath := ""
	if requestRawPath != "" && requestRawPath != requestPath {
		normalizedRawPath = "/" + strings.TrimLeft(strings.TrimSpace(requestRawPath), "/")
		normalizedRawPath = strings.TrimPrefix(normalizedRawPath, "/openai")
		if !strings.HasPrefix(normalizedRawPath, "/v1") {
			normalizedRawPath = "/v1" + normalizedRawPath
		}
	}
	trimmedBaseURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(trimmedBaseURL)
	if err == nil && parsed.Scheme != "" && parsed.Host != "" {
		basePath := parsed.Path
		baseEscapedPath := parsed.EscapedPath()
		baseQuery := parsed.RawQuery
		if requestErr == nil {
			if _, selectorPresent := request.Query()["api-version"]; selectorPresent {
				baseValues := parsed.Query()
				baseValues.Del("api-version")
				baseQuery = baseValues.Encode()
			}
		}
		parsed.Path = azureV1ResourceEndpointPath(basePath, normalizedPath)

		escapedEndpointPath := normalizedRawPath
		if escapedEndpointPath == "" && baseEscapedPath != basePath {
			escapedEndpointPath = normalizedPath
		}
		parsed.RawPath = ""
		if escapedEndpointPath != "" {
			candidateRawPath := azureV1ResourceEndpointPath(baseEscapedPath, escapedEndpointPath)
			if decodedPath, decodeErr := url.PathUnescape(candidateRawPath); decodeErr == nil && decodedPath == parsed.Path && candidateRawPath != parsed.Path {
				parsed.RawPath = candidateRawPath
			}
		}
		parsed.RawQuery = baseQuery
		if requestQuery != "" {
			if parsed.RawQuery != "" {
				parsed.RawQuery += "&"
			}
			parsed.RawQuery += requestQuery
		}
		parsed.Fragment = ""
		return parsed.String()
	}
	result := trimmedBaseURL + azureV1ResourceEndpointPath("", normalizedPath)
	if requestQuery != "" {
		result += "?" + requestQuery
	}
	return result
}

func azureV1ResourceEndpointPath(basePath string, endpointPath string) string {
	endpointPath = "/" + strings.TrimLeft(strings.TrimSpace(endpointPath), "/")
	if !strings.HasPrefix(endpointPath, "/v1") {
		endpointPath = "/v1" + endpointPath
	}
	prefix := azureV1ResourcePathPrefix(basePath)
	return prefix + "/openai" + endpointPath
}

func azureV1ResourcePathPrefix(basePath string) string {
	path := "/" + strings.Trim(strings.TrimSpace(basePath), "/")
	if path == "/" || path == "/openai" || path == "/openai/v1" {
		return ""
	}
	switch {
	case strings.HasSuffix(path, "/openai/v1"):
		path = strings.TrimSuffix(path, "/openai/v1")
	case strings.HasSuffix(path, "/openai"):
		path = strings.TrimSuffix(path, "/openai")
	}
	if path == "/" {
		return ""
	}
	return strings.TrimRight(path, "/")
}

func (p *OpenAIProvider) azureClassicAPIVersionOrEmpty() string {
	if p == nil || p.Channel == nil {
		return ""
	}
	apiVersion, err := p.Channel.GetAzureAPIVersion()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(apiVersion)
}

func (p *OpenAIProvider) validateAzureClassicAPIVersionForRequest() *types.OpenAIErrorWithStatusCode {
	if p == nil || !p.IsAzure {
		return nil
	}
	if p.Channel != nil && p.Channel.Type == config.ChannelTypeAzureV1 {
		return nil
	}
	_, errWithCode := p.azureClassicAPIVersion()
	return errWithCode
}

// 获取请求头
func (p *OpenAIProvider) GetRequestHeaders() (headers map[string]string) {
	return p.requestHeaders(openAIRequestAuthDefault)
}

func (p *OpenAIProvider) requestHeaders(mode openAIRequestAuthMode) (headers map[string]string) {
	headers = make(map[string]string)
	p.CommonRequestHeaders(headers)

	if p.openAIRequestUsesAzureAPIKey(mode) {
		headers["api-key"] = p.Channel.Key
	} else {
		headers["Authorization"] = fmt.Sprintf("Bearer %s", p.Channel.Key)
	}

	return headers
}

func (p *OpenAIProvider) openAIRequestUsesAzureAPIKey(mode openAIRequestAuthMode) bool {
	switch mode {
	case openAIRequestAuthAzureAPIKey:
		return true
	case openAIRequestAuthBearer:
		return false
	default:
		return p != nil && p.IsAzure && p.Channel != nil && p.Channel.Type != config.ChannelTypeAzureV1
	}
}

// GetRequestTextBody builds the request using the caller's wire contract.
func (p *OpenAIProvider) GetRequestTextBody(relayMode int, ModelName string, request any) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	return p.getRequestTextBody(relayMode, ModelName, request, false)
}

// getRequestTextBody is also used by compatible Chat/Completion streaming
// adapters after they have decided that provider-side usage evidence is
// required. The decision is explicit so raw JSON planning has the same final
// writer as the typed builder, while all other operations retain the public
// pass-through behavior above.
func (p *OpenAIProvider) getRequestTextBody(relayMode int, ModelName string, request any, forceIncludeUsage bool) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(relayMode)
	if errWithCode != nil {
		return nil, errWithCode
	}
	if errWithCode := p.validateAzureClassicAPIVersionForRequest(); errWithCode != nil {
		return nil, errWithCode
	}
	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url, ModelName)

	// 获取请求头
	headers := p.GetRequestHeaders()

	var httpReq *http.Request
	nativeBodyUsed := false
	if p.usesNativeOpenAIWire() && nativeOpenAIJSONRelayMode(relayMode) {
		body, exists, err := p.planNativeJSONBody(ModelName)
		if err != nil {
			return nil, common.ErrorWrapperLocal(err, "build_native_request_failed", http.StatusInternalServerError)
		}
		if exists {
			httpReq, err = p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(body), p.Requester.WithHeader(headers))
			if err != nil {
				return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
			}
			nativeBodyUsed = true
		}
	}
	if httpReq == nil {
		httpReq, errWithCode = p.BuildRequestWithMerge(request, fullRequestURL, headers, ModelName)
		if errWithCode != nil {
			return nil, errWithCode
		}
	}
	if forceIncludeUsage {
		body, err := requestBodyCopy(httpReq)
		if err != nil {
			return nil, common.ErrorWrapperLocal(err, "read_request_body_failed", http.StatusInternalServerError)
		}
		body, err = mergeProviderStreamUsage(body)
		if err != nil {
			return nil, common.ErrorWrapperLocal(err, "build_stream_options_request_failed", http.StatusInternalServerError)
		}
		replaceRequestBody(httpReq, body)
	}
	if relayMode == config.RelayModeImagesGenerations {
		if errWithCode := rejectUnsupportedImageStreamRequest(httpReq); errWithCode != nil {
			return nil, errWithCode
		}
	}
	if nativeBodyUsed && p.Context != nil && p.Context.Request != nil {
		if err := p.applyOpenAIHTTPHeaders(httpReq.Header, requestctx.NewHeaderSnapshot(p.Context.Request.Header)); err != nil {
			return nil, common.ErrorWrapperLocal(err, "invalid_request_header", http.StatusBadRequest)
		}
	}
	return httpReq, nil
}

func (p *OpenAIProvider) usesNativeOpenAIWire() bool {
	if p == nil || p.Channel == nil {
		return false
	}
	switch p.Channel.Type {
	case config.ChannelTypeOpenAI, config.ChannelTypeAzure, config.ChannelTypeAzureV1, config.ChannelTypeCustom:
		return true
	default:
		return false
	}
}
