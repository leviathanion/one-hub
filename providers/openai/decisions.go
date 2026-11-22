package openai

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"net/http"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/providerendpoint"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

var _ base.DecisionsInterface = (*OpenAIProvider)(nil)

func decisionsEndpoint(channel *model.Channel) (string, error) {
	var uri string
	if channel != nil {
		switch channel.Type {
		case config.ChannelTypeOpenAI:
			definition, _ := providerendpoint.Find(providerendpoint.Decisions)
			uri = definition.DefaultPath
		case config.ChannelTypeCustom:
			var err error
			uri, err = channel.ResolveEndpoint(providerendpoint.Decisions)
			if err != nil {
				return "", &base.RequestCapabilityError{Param: "operation", Message: err.Error()}
			}
		}
	}
	if model.IsOpenAIDataResidencyChannel(channel) || model.IsOpenAIDataResidencyBaseURL(uri) {
		return "", &base.RequestCapabilityError{Param: "operation", Message: "OpenAI data-residency endpoints are not supported"}
	}
	return uri, base.RequireOperationEndpoint(uri, "Decisions")
}

func (OpenAIProviderFactory) AssessDecisions(channel *model.Channel) error {
	_, err := decisionsEndpoint(channel)
	return err
}

func (p *OpenAIProvider) CreateDecision(modelName string, raw json.RawMessage) (*types.ProviderRawJSONState, *types.OpenAIErrorWithStatusCode) {
	uri, err := decisionsEndpoint(p.Channel)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "unsupported_capability", http.StatusServiceUnavailable)
	}
	if p.OriginalModel != modelName {
		raw, err = patchNativeJSONModel(raw, modelName)
		if err != nil {
			return nil, common.ErrorWrapperLocal(err, "invalid_request_body", http.StatusBadRequest)
		}
	}
	params, err := p.CustomParameterHandler()
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_channel_config", http.StatusInternalServerError)
	}
	// Decisions 入口直接交付原始正文，同方言单次请求只有一个参数合并阶段。
	preAdd, _ := params["pre_add"].(bool)
	body, err := base.ApplyRawCustomParams(raw, params, modelName, preAdd)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_request_body", http.StatusBadRequest)
	}
	req, err := p.Requester.NewRequest(http.MethodPost, p.GetFullRequestURL(uri, ""), p.Requester.WithBody(body), p.Requester.WithHeader(p.GetRequestHeaders()))
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "create_request_failed", http.StatusInternalServerError)
	}
	defer req.Body.Close()
	if err := p.applyNativeJSONRequestHeaders(req.Header, body, true); err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_request_header", http.StatusBadRequest)
	}

	var response decisionResponse
	response.EnableProviderRawJSONCapture()
	httpResponse, apiErr := p.Requester.SendRequestPreservingNativeDialect(req, &response, false)
	if apiErr != nil {
		return nil, apiErr
	}
	p.captureProviderResponseHeaders(httpResponse, true)
	if p.Usage != nil {
		if usage := decisionTokenUsage(response.Usage); usage != nil {
			*p.Usage = *usage
		}
		p.Usage.MergeProviderAttribution(rawJSONString(response.Model), "")
	}
	response.EnableProviderRawJSONReplay()
	return &response.ProviderRawJSONState, nil
}

func decisionTokenUsage(raw json.RawMessage) *types.Usage {
	fields := decisionUsageFields(raw)
	input := decisionTokenCount(fields["input_tokens"])
	output := decisionTokenCount(fields["output_tokens"])
	if input == nil || output == nil || *input < 0 || *output < 0 {
		return nil
	}
	usage := &types.Usage{
		PromptTokens:        *input,
		CompletionTokens:    *output,
		ProviderTokenFields: map[string]bool{"prompt_tokens": true, "completion_tokens": true},
	}
	if total := decisionTokenCount(fields["total_tokens"]); total != nil {
		usage.TotalTokens = *total
		usage.MarkProviderTokenField("total_tokens")
	}
	inputDetails := decisionUsageFields(fields["input_tokens_details"])
	outputDetails := decisionUsageFields(fields["output_tokens_details"])
	usage.PromptTokensDetails.CachedTokens = decisionTokenDetail(usage, inputDetails, config.UsageExtraCache, *input)
	usage.PromptTokensDetails.CacheWriteTokens = decisionTokenDetail(usage, inputDetails, config.UsageExtraCacheWrite, *input)
	usage.CompletionTokensDetails.ReasoningTokens = decisionTokenDetail(usage, outputDetails, config.UsageExtraReasoning, *output)
	usage.MarkProviderReported()
	return usage
}

func decisionUsageFields(raw json.RawMessage) map[string]json.RawMessage {
	fields := make(map[string]json.RawMessage)
	if visitUniqueJSONObjectMembers(raw, func(key string, _ int, value []byte) {
		fields[key] = value
	}) < 0 {
		return nil
	}
	return fields
}

func decisionTokenCount(raw json.RawMessage) *int {
	var count *int
	if jsonv2.Unmarshal(raw, &count) != nil {
		return nil
	}
	return count
}

func decisionTokenDetail(usage *types.Usage, fields map[string]json.RawMessage, key string, total int) int {
	// 总量为零时，缺少明细也能确定该分区的计价数量。
	if total > 0 {
		usage.RequireTokenExtraEvidence(key)
	}
	count := decisionTokenCount(fields[key])
	if count == nil || *count < 0 || *count > total {
		return 0
	}
	usage.MarkProviderTokenField(key)
	return *count
}

type decisionResponse struct {
	types.ProviderRawJSONState `json:"-"`
	Model                      json.RawMessage `json:"model"`
	Usage                      json.RawMessage `json:"usage"`
}

func (r *decisionResponse) DecodeCapturedProviderJSON(raw []byte) error {
	// 上游拥有答案类型。只读取独立的用量和模型证据，客户端接收原始 JSON。
	var model, usage json.RawMessage
	if visitUniqueJSONObjectMembers(raw, func(key string, _ int, value []byte) {
		switch key {
		case "model":
			model = value
		case "usage":
			usage = value
		}
	}) >= 0 {
		r.Model, r.Usage = model, usage
		return nil
	}
	r.Model, r.Usage = nil, nil
	if json.Valid(raw) {
		return nil
	}
	return jsonv2.Unmarshal(raw, r)
}
