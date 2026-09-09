package baidu

import (
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/types"
)

func (p *BaiduProvider) CreateEmbeddings(request *types.EmbeddingRequest) (*types.EmbeddingResponse, *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(config.RelayModeEmbeddings)
	if errWithCode != nil {
		return nil, errWithCode
	}
	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url, request.Model)
	if fullRequestURL == "" {
		return nil, common.ErrorWrapper(nil, "invalid_baidu_config", http.StatusInternalServerError)
	}

	// 获取请求头
	headers := p.GetRequestHeaders()

	aliRequest := convertFromEmbeddingOpenai(request)
	// 创建请求
	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(aliRequest), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	defer req.Body.Close()

	baiduResponse := &BaiduEmbeddingResponse{}

	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, baiduResponse, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	return p.convertToEmbeddingOpenai(baiduResponse, request)
}

func convertFromEmbeddingOpenai(request *types.EmbeddingRequest) *BaiduEmbeddingRequest {
	return &BaiduEmbeddingRequest{
		Input: request.ParseInput(),
	}
}

func (p *BaiduProvider) convertToEmbeddingOpenai(response *BaiduEmbeddingResponse, request *types.EmbeddingRequest) (openaiResponse *types.EmbeddingResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	aiError := errorHandle(&response.BaiduError)
	if aiError != nil {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *aiError,
			StatusCode:  http.StatusBadRequest,
		}
		return
	}

	usage := baiduEmbeddingUsage(&response.Usage)
	openAIEmbeddingResponse := &types.EmbeddingResponse{
		Object: "list",
		Data:   make([]types.Embedding, 0, len(response.Data)),
		Model:  request.Model,
		Usage:  usage,
	}

	for _, item := range response.Data {
		openAIEmbeddingResponse.Data = append(openAIEmbeddingResponse.Data, types.Embedding{
			Object:    item.Object,
			Index:     item.Index,
			Embedding: item.Embedding,
		})
	}

	if p.Usage != nil && usage != nil {
		*p.Usage = *usage
	}

	return openAIEmbeddingResponse, nil
}

// baiduEmbeddingUsage 在公开 usage 前验证百度向量的输入型计量合同。百度
// 可以省略显式的 completion 零值，但 prompt 和 total 必须都存在且相等；
// 否则仍交付响应，但不建立可结算的供应商证据。
func baiduEmbeddingUsage(providerUsage *types.Usage) *types.Usage {
	if providerUsage == nil {
		return nil
	}

	usage := *providerUsage
	// 解码结果通常未授权；清除复用 response 值时可能残留的旧标记。
	usage.ProviderReported = false
	valid := providerUsage.ProviderTokenFields["prompt_tokens"] &&
		providerUsage.ProviderTokenFields["total_tokens"] &&
		providerUsage.PromptTokens >= 0 &&
		providerUsage.TotalTokens >= 0 &&
		providerUsage.PromptTokens == providerUsage.TotalTokens &&
		providerUsage.CompletionTokens == 0
	if !valid {
		if providerUsage.ProviderTokenFields["prompt_tokens"] &&
			providerUsage.ProviderTokenFields["total_tokens"] &&
			(providerUsage.PromptTokens != providerUsage.TotalTokens || providerUsage.CompletionTokens != 0) {
			usage.ProviderTokenConflict = true
		}
		return &usage
	}
	// completion 是该 operation 的结构性零值；这里显式记录它，包括省略字段的
	// wire 形式，避免把任意解码结果自动当成供应商证据。
	usage.CompletionTokens = 0
	usage.MarkProviderTokenField("prompt_tokens")
	usage.MarkProviderTokenField("completion_tokens")
	usage.MarkProviderTokenField("total_tokens")
	usage.MarkProviderReported()
	return &usage
}
