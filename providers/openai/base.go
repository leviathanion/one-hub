package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
	"strings"

	"one-api/providers/base"
)

type OpenAIProviderFactory struct{}

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

	UsageHandler        UsageHandler
	RequestHandleBefore RequestHandleBefore
}

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

	OpenAIProvider := &OpenAIProvider{
		BaseProvider: base.BaseProvider{
			Config:          openaiConfig,
			Channel:         channel,
			Requester:       requester.NewHTTPRequester(*channel.Proxy, RequestErrorHandle),
			SupportResponse: true,
		},
		IsAzure:       false,
		BalanceAction: true,
	}

	if channel.Type == config.ChannelTypeOpenAI {
		OpenAIProvider.SupportStreamOptions = true
	}

	return OpenAIProvider
}

func getOpenAIConfig(baseURL string, channel *model.Channel) base.ProviderConfig {
	providerConfig := base.ProviderConfig{
		BaseURL:             baseURL,
		Completions:         "/v1/completions",
		ChatCompletions:     "/v1/chat/completions",
		Embeddings:          "/v1/embeddings",
		Moderation:          "/v1/moderations",
		AudioSpeech:         "/v1/audio/speech",
		AudioTranscriptions: "/v1/audio/transcriptions",
		AudioTranslations:   "/v1/audio/translations",
		ImagesGenerations:   "/v1/images/generations",
		ImagesEdit:          "/v1/images/edits",
		ImagesVariations:    "/v1/images/variations",
		ModelList:           "/v1/models",
		ChatRealtime:        "/v1/realtime",
		Responses:           "/v1/responses",
	}

	if channel.Type != config.ChannelTypeCustom || channel.Plugin == nil {
		return providerConfig
	}

	customMapping, ok := channel.Plugin.Data()["customize"]
	if !ok {
		return providerConfig
	}

	providerConfig.SetAPIUri(customMapping)

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
		if strings.HasPrefix(baseURL, "https://") {
			baseURL = strings.Replace(baseURL, "https://", "wss://", 1)
		} else {
			baseURL = strings.Replace(baseURL, "http://", "ws://", 1)
		}

		if p.IsAzure {
			// wss://my-eastus2-openai-resource.openai.azure.com/openai/realtime?api-version=2024-10-01-preview&deployment=gpt-4o-realtime-preview-1001
			requestURL = fmt.Sprintf("/openai/%s?api-version=%s&deployment=%s", requestURL, azureAPIVersion, modelName)
		} else {
			requestURL += fmt.Sprintf("?model=%s", modelName)
		}

		return fmt.Sprintf("%s%s", baseURL, requestURL)
	}

	if p.IsAzure && p.Channel != nil && p.Channel.Type == config.ChannelTypeAzureV1 {
		return azureV1FullRequestURL(baseURL, requestURL)
	}

	if p.IsAzure {
		apiVersion := azureAPIVersion
		if isAzureClassicResponsesHTTPPath(requestURL) {
			requestURL = azureClassicResponsesHTTPRequestURL(requestURL, apiVersion)
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
				requestURL = strings.TrimPrefix(requestURL, "/v1")
				requestURL = fmt.Sprintf("/openai%s?api-version=%s", requestURL, apiVersion)
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

func isAzureClassicResponsesHTTPPath(requestURL string) bool {
	path := azureClassicResponsesHTTPPathCandidate(requestURL)
	return path == "/responses" || strings.HasPrefix(path, "/responses/")
}

func azureClassicResponsesHTTPRequestURL(requestURL string, apiVersion string) string {
	parsed, err := url.Parse(strings.TrimSpace(requestURL))
	if err != nil {
		return fmt.Sprintf("/openai%s?api-version=%s", azureClassicResponsesHTTPPathCandidate(requestURL), apiVersion)
	}
	parsed.Path = "/openai" + azureClassicResponsesHTTPPath(parsed.Path)
	parsed.RawPath = ""
	parsed.Fragment = ""
	query := parsed.Query()
	query.Set("api-version", apiVersion)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func azureClassicResponsesHTTPPathCandidate(requestURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(requestURL))
	if err == nil {
		return azureClassicResponsesHTTPPath(parsed.Path)
	}
	path := strings.TrimSpace(requestURL)
	if index := strings.IndexAny(path, "?#"); index >= 0 {
		path = path[:index]
	}
	return azureClassicResponsesHTTPPath(path)
}

func azureClassicResponsesHTTPPath(path string) string {
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
	normalizedPath := "/" + strings.TrimLeft(strings.TrimSpace(requestURL), "/")
	if normalizedPath == "/1" {
		normalizedPath = "/v1/models"
	}
	normalizedPath = strings.TrimPrefix(normalizedPath, "/openai")
	if !strings.HasPrefix(normalizedPath, "/v1") {
		normalizedPath = "/v1" + normalizedPath
	}
	trimmedBaseURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(trimmedBaseURL)
	if err == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.Path = azureV1ResourceEndpointPath(parsed.Path, normalizedPath)
		parsed.RawPath = ""
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	}
	return trimmedBaseURL + azureV1ResourceEndpointPath("", normalizedPath)
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

// 修改 GetRequestTextBody 函数中的对应部分
func (p *OpenAIProvider) GetRequestTextBody(relayMode int, ModelName string, request any) (*http.Request, *types.OpenAIErrorWithStatusCode) {
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

	// 使用通用的 BuildRequestWithMerge 方法构建请求
	return p.BuildRequestWithMerge(request, fullRequestURL, headers, ModelName)
}
