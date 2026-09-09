package ali

import (
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/types"
)

func (p *AliProvider) CreateEmbeddings(request *types.EmbeddingRequest) (*types.EmbeddingResponse, *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(config.RelayModeEmbeddings)
	if errWithCode != nil {
		return nil, errWithCode
	}
	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url, request.Model)

	// 获取请求头
	headers := p.GetRequestHeaders()

	aliRequest := convertFromEmbeddingOpenai(request)
	// 创建请求
	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(aliRequest), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	defer req.Body.Close()

	aliResponse := &AliEmbeddingResponse{}

	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, aliResponse, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	return p.convertToEmbeddingOpenai(aliResponse, request)
}

func convertFromEmbeddingOpenai(request *types.EmbeddingRequest) *AliEmbeddingRequest {
	return &AliEmbeddingRequest{
		Model: "text-embedding-v1",
		Input: struct {
			Texts []string `json:"texts"`
		}{
			Texts: request.ParseInput(),
		},
	}
}

func (p *AliProvider) convertToEmbeddingOpenai(response *AliEmbeddingResponse, request *types.EmbeddingRequest) (openaiResponse *types.EmbeddingResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	aiError := errorHandle(&response.AliError)
	if aiError != nil {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *aiError,
			StatusCode:  http.StatusBadRequest,
		}
		return
	}

	openaiResponse = &types.EmbeddingResponse{
		Object: "list",
		Data:   make([]types.Embedding, 0, len(response.Output.Embeddings)),
		Model:  request.Model,
		Usage:  aliEmbeddingUsageToOpenAI(&response.Usage),
	}

	for _, item := range response.Output.Embeddings {
		openaiResponse.Data = append(openaiResponse.Data, types.Embedding{
			Object:    `embedding`,
			Index:     item.TextIndex,
			Embedding: item.Embedding,
		})
	}

	if openaiResponse.Usage != nil && p.Usage != nil {
		*p.Usage = *openaiResponse.Usage
	}

	return
}

// Ali 文本向量接口只把 total_tokens 定义为输入 token 数。该语义不能
// 复用聊天接口的 input/output/total 三字段证据要求。
func aliEmbeddingUsageToOpenAI(providerUsage *AliUsage) *types.Usage {
	if providerUsage == nil || !providerUsage.present || !providerUsage.totalTokensPresent || providerUsage.TotalTokens < 0 {
		return nil
	}

	usage := &types.Usage{
		PromptTokens:     providerUsage.TotalTokens,
		CompletionTokens: 0,
		TotalTokens:      providerUsage.TotalTokens,
		ProviderTokenFields: map[string]bool{
			"prompt_tokens":     true,
			"completion_tokens": true,
			"total_tokens":      true,
		},
	}
	usage.MarkProviderReported()
	return usage
}
