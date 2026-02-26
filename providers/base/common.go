package base

import (
	"encoding/json"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/common/utils"
	"one-api/model"
	"one-api/types"
	"strings"

	"github.com/gin-gonic/gin"
)

type ProviderConfig struct {
	BaseURL             string
	Completions         string
	ChatCompletions     string
	Embeddings          string
	AudioSpeech         string
	Moderation          string
	AudioTranscriptions string
	AudioTranslations   string
	ImagesGenerations   string
	ImagesEdit          string
	ImagesVariations    string
	ModelList           string
	Rerank              string
	ChatRealtime        string
	Responses           string
}

func (pc *ProviderConfig) SetAPIUri(customMapping map[string]interface{}) {
	relayModeMap := map[int]*string{
		config.RelayModeChatCompletions:    &pc.ChatCompletions,
		config.RelayModeCompletions:        &pc.Completions,
		config.RelayModeEmbeddings:         &pc.Embeddings,
		config.RelayModeAudioSpeech:        &pc.AudioSpeech,
		config.RelayModeAudioTranscription: &pc.AudioTranscriptions,
		config.RelayModeAudioTranslation:   &pc.AudioTranslations,
		config.RelayModeModerations:        &pc.Moderation,
		config.RelayModeImagesGenerations:  &pc.ImagesGenerations,
		config.RelayModeImagesEdits:        &pc.ImagesEdit,
		config.RelayModeImagesVariations:   &pc.ImagesVariations,
		config.RelayModeResponses:          &pc.Responses,
	}

	for key, value := range customMapping {
		keyInt := utils.String2Int(key)
		customValue, isString := value.(string)
		if !isString || customValue == "" {
			continue
		}

		if _, exists := relayModeMap[keyInt]; !exists {
			continue
		}

		value := customValue
		if value == "disable" {
			value = ""
		}

		*relayModeMap[keyInt] = value

	}
}

type BaseProvider struct {
	OriginalModel   string
	Usage           *types.Usage
	Config          ProviderConfig
	Context         *gin.Context
	Channel         *model.Channel
	Requester       *requester.HTTPRequester
	SupportResponse bool
}

// 获取基础URL
func (p *BaseProvider) GetBaseURL() string {
	if p.Channel.GetBaseURL() != "" {
		return p.Channel.GetBaseURL()
	}

	return p.Config.BaseURL
}

// 获取完整请求URL
func (p *BaseProvider) GetFullRequestURL(requestURL string, _ string) string {
	baseURL := strings.TrimSuffix(p.GetBaseURL(), "/")

	return fmt.Sprintf("%s%s", baseURL, requestURL)
}

// 获取请求头
func (p *BaseProvider) CommonRequestHeaders(headers map[string]string) {
	if p.Context != nil {
		headers["Content-Type"] = p.Context.Request.Header.Get("Content-Type")
		headers["Accept"] = p.Context.Request.Header.Get("Accept")
	}

	if headers["Content-Type"] == "" {
		headers["Content-Type"] = "application/json"
	}
	// 自定义header
	if p.Channel.ModelHeaders != nil {
		var customHeaders map[string]string
		err := json.Unmarshal([]byte(*p.Channel.ModelHeaders), &customHeaders)
		if err == nil {
			for key, value := range customHeaders {
				headers[key] = value
			}
		}
	}
}

func (p *BaseProvider) GetUsage() *types.Usage {
	return p.Usage
}

func (p *BaseProvider) SetUsage(usage *types.Usage) {
	p.Usage = usage
}

func (p *BaseProvider) SetContext(c *gin.Context) {
	p.Context = c
}

func (p *BaseProvider) SetOriginalModel(ModelName string) {
	p.OriginalModel = ModelName
}

func (p *BaseProvider) GetOriginalModel() string {
	return p.OriginalModel
}

func (p *BaseProvider) GetChannel() *model.Channel {
	return p.Channel
}

func (p *BaseProvider) ModelMappingHandler(modelName string) (string, error) {
	p.OriginalModel = modelName

	modelMapping := p.Channel.GetModelMapping()

	if modelMapping == "" || modelMapping == "{}" {
		return modelName, nil
	}

	modelMap := make(map[string]string)
	err := json.Unmarshal([]byte(modelMapping), &modelMap)
	if err != nil {
		return "", err
	}

	if modelMap[modelName] != "" {
		return modelMap[modelName], nil
	}

	return modelName, nil
}

// CustomParameterHandler processes extra parameters from the channel and returns them as a map
func (p *BaseProvider) CustomParameterHandler() (map[string]interface{}, error) {
	customParameter := p.Channel.GetCustomParameter()
	if customParameter == "" || customParameter == "{}" {
		return nil, nil
	}

	customParams := make(map[string]interface{})
	err := json.Unmarshal([]byte(customParameter), &customParams)
	if err != nil {
		return nil, err
	}

	return customParams, nil
}

func (p *BaseProvider) GetAPIUri(relayMode int) string {
	switch relayMode {
	case config.RelayModeChatCompletions:
		return p.Config.ChatCompletions
	case config.RelayModeCompletions:
		return p.Config.Completions
	case config.RelayModeEmbeddings:
		return p.Config.Embeddings
	case config.RelayModeAudioSpeech:
		return p.Config.AudioSpeech
	case config.RelayModeAudioTranscription:
		return p.Config.AudioTranscriptions
	case config.RelayModeAudioTranslation:
		return p.Config.AudioTranslations
	case config.RelayModeModerations:
		return p.Config.Moderation
	case config.RelayModeImagesGenerations:
		return p.Config.ImagesGenerations
	case config.RelayModeImagesEdits:
		return p.Config.ImagesEdit
	case config.RelayModeImagesVariations:
		return p.Config.ImagesVariations
	case config.RelayModeRerank:
		return p.Config.Rerank
	case config.RelayModeChatRealtime:
		return p.Config.ChatRealtime
	case config.RelayModeResponses:
		return p.Config.Responses
	default:
		return ""
	}
}

func (p *BaseProvider) GetSupportedAPIUri(relayMode int) (url string, err *types.OpenAIErrorWithStatusCode) {
	url = p.GetAPIUri(relayMode)
	if url == "" {
		err = common.StringErrorWrapperLocal("The API interface is not supported", "unsupported_api", http.StatusNotImplemented)
		return
	}

	return
}

func (p *BaseProvider) GetRequester() *requester.HTTPRequester {
	return p.Requester
}

func (p *BaseProvider) GetSupportedResponse() bool {
	return p.SupportResponse
}

func (p *BaseProvider) GetRawBody() ([]byte, bool) {
	if raw, exists := p.Context.Get(config.GinRequestBodyKey); exists {
		if bytes, ok := raw.([]byte); ok {
			return bytes, true
		}
	}
	return nil, false
}

// MergeCustomParams 将自定义参数合并到请求体 map 中
func (p *BaseProvider) MergeCustomParams(requestMap map[string]interface{}, customParams map[string]interface{}, modelName string) map[string]interface{} {
	return ApplyCustomParams(requestMap, customParams, modelName, false)
}

// MergeExtraBodyFromRawRequest 从原始请求中合并额外字段（支持 AllowExtraBody）
// 以用户原始请求为基础，再反序列化处理后的请求字段进行覆盖，
// 这样额外字段自然保留，并减少中间 map 拷贝。
func (p *BaseProvider) MergeExtraBodyFromRawRequest(requestBytes []byte) (map[string]interface{}, error) {
	merged := make(map[string]interface{})

	rawBody, ok := p.GetRawBody()
	if ok && rawBody != nil {
		if err := json.Unmarshal(rawBody, &merged); err != nil {
			merged = make(map[string]interface{})
		}
	}

	if len(requestBytes) == 0 {
		return merged, nil
	}

	if err := json.Unmarshal(requestBytes, &merged); err != nil {
		return nil, err
	}

	return merged, nil
}

// BuildRequestWithMerge 通用的请求体构建方法，支持 CustomParameter 和 AllowExtraBody
// originalBody: 原始请求体（已转换后的供应商特定格式）
// fullRequestURL: 完整的请求 URL
// headers: 请求头
// 返回构建好的 HTTP 请求
func (p *BaseProvider) BuildRequestWithMerge(originalBody interface{}, fullRequestURL string, headers map[string]string, modelName string) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	// 处理额外参数
	customParams, err := p.CustomParameterHandler()
	if err != nil {
		return nil, common.ErrorWrapper(err, "custom_parameter_error", http.StatusInternalServerError)
	}

	// 检查是否需要合并额外字段（来自渠道配置的额外参数或用户请求中的 extra_body）
	needMerge := customParams != nil || p.Channel.AllowExtraBody

	if needMerge {
		var requestMap map[string]interface{}
		if bodyMap, ok := originalBody.(map[string]interface{}); ok {
			if p.Channel.AllowExtraBody {
				requestMap, err = p.MergeExtraBodyFromRawRequest(nil)
				if err != nil {
					return nil, common.ErrorWrapper(err, "unmarshal_request_failed", http.StatusInternalServerError)
				}
			} else {
				requestMap = make(map[string]interface{}, len(bodyMap))
			}
			for key, value := range bodyMap {
				requestMap[key] = value
			}
		} else {
			// 保留 struct -> JSON 这一步，以遵循 json tag/omitempty 语义。
			requestBytes, err := json.Marshal(originalBody)
			if err != nil {
				return nil, common.ErrorWrapper(err, "marshal_request_failed", http.StatusInternalServerError)
			}

			if p.Channel.AllowExtraBody {
				requestMap, err = p.MergeExtraBodyFromRawRequest(requestBytes)
				if err != nil {
					return nil, common.ErrorWrapper(err, "unmarshal_request_failed", http.StatusInternalServerError)
				}
			} else {
				err = json.Unmarshal(requestBytes, &requestMap)
				if err != nil {
					return nil, common.ErrorWrapper(err, "unmarshal_request_failed", http.StatusInternalServerError)
				}
			}
		}

		// 处理自定义额外参数
		if customParams != nil {
			requestMap = p.MergeCustomParams(requestMap, customParams, modelName)
		}

		requestBytes, err := json.Marshal(requestMap)
		if err != nil {
			return nil, common.ErrorWrapper(err, "marshal_request_failed", http.StatusInternalServerError)
		}

		// 使用修改后的请求体创建请求
		req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(requestBytes), p.Requester.WithHeader(headers))
		if err != nil {
			return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
		}

		return req, nil
	}

	// 如果没有额外参数，使用原始请求体创建请求
	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(originalBody), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}

	return req, nil
}

// ApplyCustomParams 核心自定义参数合并逻辑
// 参数说明：
// - requestMap: 请求体 map
// - customParams: 自定义参数
// - skipPreAddCheck: 是否跳过 pre_add 检查（true=不检查 pre_add，直接处理）
func ApplyCustomParams(requestMap map[string]interface{}, customParams map[string]interface{}, modelName string, skipPreAddCheck bool) map[string]interface{} {
	if customParams == nil || len(customParams) == 0 {
		return requestMap
	}

	// 检查是否需要覆盖已有参数
	shouldOverwrite := false
	if overwriteValue, exists := customParams["overwrite"]; exists {
		if boolValue, ok := overwriteValue.(bool); ok {
			shouldOverwrite = boolValue
		}
	}

	// 如果配置是 pre_add，且需要检查 pre_add，则跳过所有处理
	if !skipPreAddCheck {
		if preAdd, exists := customParams["pre_add"]; exists && preAdd == true {
			return requestMap
		}
	}

	// 检查是否按照模型粒度控制
	perModel := false
	if perModelValue, exists := customParams["per_model"]; exists {
		if boolValue, ok := perModelValue.(bool); ok {
			perModel = boolValue
		}
	}

	customParamsModel := customParams
	if perModel {
		if v, exists := customParams[modelName]; exists {
			if modelConfig, ok := v.(map[string]interface{}); ok {
				customParamsModel = modelConfig
			} else {
				customParamsModel = map[string]interface{}{}
			}
		} else {
			customParamsModel = map[string]interface{}{}
		}
	}

	// 处理参数删除
	if removeParams, exists := customParamsModel["remove_params"]; exists {
		if paramsList, ok := removeParams.([]interface{}); ok {
			for _, param := range paramsList {
				if paramName, ok := param.(string); ok {
					removeNestedParam(requestMap, paramName)
				}
			}
		}
	}

	// 添加额外参数
	for key, value := range customParamsModel {
		// 忽略 keys "stream", "overwrite", "per_model", "pre_add"
		if key == "stream" || key == "overwrite" || key == "per_model" || key == "pre_add" {
			continue
		}

		// 根据覆盖的设置决定如何添加参数
		if shouldOverwrite {
			// 覆盖模式：直接添加/覆盖参数
			requestMap[key] = value
		} else {
			// 非覆盖模式：进行深度合并
			if existingValue, exists := requestMap[key]; exists {
				// 如果都是map类型，进行深度合并
				if existingMap, ok := existingValue.(map[string]interface{}); ok {
					if newMap, ok := value.(map[string]interface{}); ok {
						requestMap[key] = DeepMergeMap(existingMap, newMap)
						continue
					}
				}
				// 如果不是map类型或类型不匹配，保持原值（不覆盖）
			} else {
				// 参数不存在时直接添加
				requestMap[key] = value
			}
		}
	}

	return requestMap
}

// removeNestedParam removes a parameter from the map, supporting nested paths like "generationConfig.thinkingConfig"
func removeNestedParam(requestMap map[string]interface{}, paramPath string) {
	// 使用 "." 分割路径
	parts := strings.Split(paramPath, ".")

	// 如果只有一层,直接删除
	if len(parts) == 1 {
		delete(requestMap, paramPath)
		return
	}

	// 处理嵌套路径
	current := requestMap
	for i := 0; i < len(parts)-1; i++ {
		if next, ok := current[parts[i]].(map[string]interface{}); ok {
			current = next
		} else {
			// 如果中间路径不存在或不是 map,则无法继续
			return
		}
	}

	// 删除最后一级的键
	delete(current, parts[len(parts)-1])
}

// DeepMergeMap 深度合并两个map
func DeepMergeMap(existing map[string]interface{}, new map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	// 先复制现有的所有键值
	for k, v := range existing {
		result[k] = v
	}

	// 然后合并新的键值
	for k, newValue := range new {
		if existingValue, exists := result[k]; exists {
			// 如果都是map类型，递归深度合并
			if existingMap, ok := existingValue.(map[string]interface{}); ok {
				if newMap, ok := newValue.(map[string]interface{}); ok {
					result[k] = DeepMergeMap(existingMap, newMap)
					continue
				}
			}
			// 如果不是map类型，新值覆盖旧值
			result[k] = newValue
		} else {
			// 键不存在，直接添加
			result[k] = newValue
		}
	}

	return result
}
