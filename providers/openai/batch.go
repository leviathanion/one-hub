package openai

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"one-api/common/config"
	commonresponses "one-api/common/responses"
	"one-api/providers/base"
	"one-api/types"
)

var _ base.BatchRelayInterface = (*OpenAIProvider)(nil)

func (p *OpenAIProvider) BuildBatchRelayURL(escapedPath, rawQuery string) (string, error) {
	if p == nil || p.Channel == nil {
		return "", fmt.Errorf("batch provider is not initialized")
	}
	switch p.Channel.Type {
	case config.ChannelTypeOpenAI, config.ChannelTypeAzureV1, config.ChannelTypeCustom:
	default:
		return "", fmt.Errorf("channel does not implement native OpenAI batches")
	}
	if escapedPath != "/v1/batches" {
		if !strings.HasPrefix(escapedPath, "/v1/batches/") {
			return "", fmt.Errorf("batch operation is not implemented")
		}
		parts := strings.Split(strings.TrimPrefix(escapedPath, "/v1/batches/"), "/")
		if len(parts) > 2 || (len(parts) == 2 && parts[1] != "cancel") {
			return "", fmt.Errorf("batch operation is not implemented")
		}
		id, err := url.PathUnescape(parts[0])
		if err != nil || id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00\r\n") {
			return "", fmt.Errorf("batch resource path is invalid")
		}
	}
	return p.BuildRawRelayURL(escapedPath, rawQuery)
}

// ExtractBatchUsage 不修改 body，也不从 HTTP 成功或请求数量推导用量。
// endpoint 来自已关联的准入项，body 是 Batch 结果中的真实响应正文。
func (p *OpenAIProvider) ExtractBatchUsage(endpoint string, body []byte) *types.Usage {
	usage := &types.Usage{}
	switch endpoint {
	case "/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/moderations":
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil {
			return usage
		}
		var reported *types.Usage
		if json.Unmarshal(fields["usage"], &reported) != nil || reported == nil {
			return usage
		}
		actualModel := rawJSONString(fields["model"])
		if endpoint == "/v1/embeddings" {
			applyOpenAIEmbeddingUsage(usage, reported, actualModel)
			return usage
		}
		reported.MarkProviderReported()
		reported.MergeProviderAttribution(actualModel, rawJSONString(fields["service_tier"]))
		return reported
	case "/v1/responses":
		var response types.OpenAIResponsesResponses
		if response.DecodeCapturedProviderJSON(body) != nil {
			return usage
		}
		if response.Usage != nil {
			response.Usage.MarkProviderReported()
		}
		commonresponses.ApplyResponsesUsage(usage, &response)
	case "/v1/images/generations", "/v1/images/edits":
		var response OpenAIProviderImageResponse
		if response.DecodeCapturedProviderJSON(body) != nil {
			return usage
		}
		applyImageEvidence(usage, &response.ImageResponse, response.providerDataCount)
	}
	return usage
}
