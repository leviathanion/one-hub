package azuredatabricks

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

type AzureDatabricksProviderFactory struct{}

// Create returns an AzureDatabricksProvider
func (f AzureDatabricksProviderFactory) Create(channel *model.Channel) base.ProviderInterface {
	return &AzureDatabricksProvider{
		BaseProvider: base.BaseProvider{
			Channel:   channel,
			Requester: requester.NewHTTPRequester(*channel.Proxy, requestErrorHandle),
		},
	}
}

type AzureDatabricksProvider struct {
	base.BaseProvider
}

func (p *AzureDatabricksProvider) GetRequestHeaders() (headers map[string]string) {
	headers = make(map[string]string)
	// https://learn.microsoft.com/en-us/azure/databricks/dev-tools/api/latest/authentication
	auth := base64.StdEncoding.EncodeToString([]byte("token:" + p.Channel.Key))
	headers["Authorization"] = fmt.Sprintf("Basic %s", auth)
	return headers
}

func (p *AzureDatabricksProvider) GetFullRequestURL(modelName string) string {
	baseURL := strings.TrimSuffix(p.GetBaseURL(), "/")
	return fmt.Sprintf("%s/serving-endpoints/%s/invocations", baseURL, modelName)
}

func (p *AzureDatabricksProvider) prepareRequest(httpRequester *requester.HTTPRequester, request *types.ChatCompletionRequest) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.convertRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}

	requestURL := p.GetFullRequestURL(request.Model)
	httpResponse, errWithCode := p.doRequest(httpRequester, req, requestURL)
	if errWithCode != nil {
		return nil, errWithCode
	}

	return httpResponse, nil
}

func (p *AzureDatabricksProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	httpResponse, errWithCode := p.prepareRequest(p.Requester, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer httpResponse.Body.Close()

	response, err := p.convertResponse(httpResponse)
	if err != nil {
		return nil, common.ErrorWrapper(err, "convert_response_failed", http.StatusInternalServerError)
	}

	return response, nil
}

func (p *AzureDatabricksProvider) doRequest(httpRequester *requester.HTTPRequester, request any, requestURL string) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	req, err := httpRequester.NewRequest(http.MethodPost, requestURL,
		httpRequester.WithBody(request),
		httpRequester.WithHeader(p.GetRequestHeaders()),
	)
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	return httpRequester.SendRequestRaw(req)
}

func (p *AzureDatabricksProvider) convertRequest(request *types.ChatCompletionRequest) (*databricksChatRequest, *types.OpenAIErrorWithStatusCode) {
	messages := make([]types.ChatCompletionMessage, 0)
	for _, msg := range request.Messages {
		messages = append(messages, types.ChatCompletionMessage{
			Role:    msg.Role,
			Content: msg.StringContent(),
		})
	}

	databricksRequest := &databricksChatRequest{
		Messages: messages,
		Stream:   request.Stream,
	}

	if request.MaxCompletionTokens != 0 {
		databricksRequest.MaxTokens = request.MaxCompletionTokens
	}
	if request.Temperature != nil {
		databricksRequest.Temperature = float64(*request.Temperature)
	}
	if request.TopP != nil {
		databricksRequest.TopP = float64(*request.TopP)
	}
	if request.N != nil {
		databricksRequest.N = *request.N
	}
	if request.Stop != nil {
		if stop, ok := request.Stop.([]string); ok {
			databricksRequest.Stop = stop
		}
	}
	if request.PresencePenalty != nil {
		databricksRequest.PresencePenalty = float64(*request.PresencePenalty)
	}
	if request.FrequencyPenalty != nil {
		databricksRequest.FrequencyPenalty = float64(*request.FrequencyPenalty)
	}

	if reasoning := request.EffectiveReasoning(); reasoning != nil {
		var opErr *types.OpenAIErrorWithStatusCode
		databricksRequest.MaxTokens, databricksRequest.Thinking, opErr = getThinking(databricksRequest.MaxTokens, reasoning)
		if opErr != nil {
			return nil, opErr
		}
	}

	return databricksRequest, nil
}

func getThinking(maxTokens int, reasoning *types.ChatReasoning) (newMaxTokens int, thinking *Thinking, err *types.OpenAIErrorWithStatusCode) {
	newMaxTokens = maxTokens
	thinking = &Thinking{
		Type: "enabled",
	}
	if reasoning == nil || (reasoning.MaxTokens == 0 && reasoning.Effort == "") {
		thinking.BudgetTokens = int(float64(maxTokens) * 0.8)
	} else if reasoning.MaxTokens > 0 {
		if reasoning.MaxTokens > maxTokens {
			err = common.StringErrorWrapper(fmt.Sprintf("budget_token cannot be greater than the max_token, max_token: %d, budget_token: %d", maxTokens, reasoning.MaxTokens), "budget_tokens_too_large", http.StatusBadRequest)
			return
		}
		thinking.BudgetTokens = reasoning.MaxTokens
	} else {
		switch reasoning.Effort {
		case "low":
			thinking.BudgetTokens = int(float64(maxTokens) * 0.2)
		case "medium":
			thinking.BudgetTokens = int(float64(maxTokens) * 0.5)
		default:
			thinking.BudgetTokens = int(float64(maxTokens) * 0.8)
		}
	}
	if thinking.BudgetTokens < 128 {
		thinking.BudgetTokens = 128
	}
	if newMaxTokens <= thinking.BudgetTokens {
		newMaxTokens = thinking.BudgetTokens + 128
	}

	return
}

func (p *AzureDatabricksProvider) convertResponse(resp *http.Response) (response *types.ChatCompletionResponse, err error) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var databricksResponse databricksChatResponse
	err = json.Unmarshal(responseBody, &databricksResponse)
	if err != nil {
		return nil, err
	}

	response = &types.ChatCompletionResponse{
		ID:      databricksResponse.ID,
		Object:  "chat.completion",
		Created: databricksResponse.Created,
		Model:   databricksResponse.Model,
		Choices: []types.ChatCompletionChoice{},
	}

	for _, choice := range databricksResponse.Choices {
		response.Choices = append(response.Choices, types.ChatCompletionChoice{
			Index: choice.Index,
			Message: types.ChatCompletionMessage{
				Role:    "assistant",
				Content: choice.Message.Content,
			},
			FinishReason: choice.FinishReason,
		})
	}

	if databricksResponse.Usage != nil {
		response.Usage = databricksResponse.Usage
		publishDatabricksUsage(p.Usage, response.Usage, databricksResponse.Model, databricksResponse.ServiceTier)
	}
	response.ServiceTier = databricksResponse.ServiceTier

	return response, nil
}

func (p *AzureDatabricksProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	streamRequester := p.Requester.ForHTTPProfile(requester.HTTPProfileLongStream)
	httpResponse, errWithCode := p.prepareRequest(streamRequester, request)
	if errWithCode != nil {
		return nil, errWithCode
	}

	stream, err := requester.RequestStream(streamRequester, httpResponse, p.streamHandler)
	if err != nil {
		return nil, common.ErrorWrapper(err, "request_stream_failed", http.StatusInternalServerError)
	}

	return stream, nil
}

func (p *AzureDatabricksProvider) streamHandler(rawLine *[]byte, dataChan chan string, errChan chan error) {
	if !strings.HasPrefix(string(*rawLine), "data:") {
		*rawLine = nil
		return
	}

	*rawLine = (*rawLine)[5:]
	*rawLine = []byte(strings.TrimSpace(string(*rawLine)))

	if string(*rawLine) == "[DONE]" {
		errChan <- io.EOF
		*rawLine = requester.StreamClosed
		return
	}

	observeDatabricksStreamUsage(p, *rawLine)
	dataChan <- string(*rawLine)
}

type databricksStreamEnvelope struct {
	Model       string          `json:"model"`
	ServiceTier string          `json:"service_tier"`
	Usage       json.RawMessage `json:"usage"`
}

func observeDatabricksStreamUsage(provider *AzureDatabricksProvider, payload []byte) {
	if provider == nil || provider.Usage == nil {
		return
	}

	var envelope databricksStreamEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return
	}

	var usage types.Usage
	var usagePtr *types.Usage
	if len(envelope.Usage) > 0 && !strings.EqualFold(strings.TrimSpace(string(envelope.Usage)), "null") {
		if err := json.Unmarshal(envelope.Usage, &usage); err == nil {
			usagePtr = &usage
		}
	}
	publishDatabricksUsage(provider.Usage, usagePtr, envelope.Model, envelope.ServiceTier)
}

func publishDatabricksUsage(target, candidate *types.Usage, model, serviceTier string) bool {
	if candidate == nil {
		if target != nil {
			target.MergeProviderAttribution(model, serviceTier)
		}
		return false
	}
	if !validDatabricksUsage(candidate) {
		if target != nil {
			target.MergeProviderAttribution(model, serviceTier)
		}
		return false
	}

	previousModel := ""
	previousTier := ""
	previousConflict := false
	if target != nil {
		previousModel = target.ResponseModel
		previousTier = target.ServiceTier
		previousConflict = target.AttributionConflict
	}

	candidate.MarkProviderReported()
	candidate.MergeProviderAttribution(model, serviceTier)
	if target == nil {
		return true
	}
	*target = *candidate
	target.MergeProviderAttribution(previousModel, previousTier)
	target.AttributionConflict = target.AttributionConflict || previousConflict
	return true
}

func validDatabricksUsage(usage *types.Usage) bool {
	if usage == nil || usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		return false
	}
	for _, value := range []int{
		usage.PromptTokensDetails.AudioTokens,
		usage.PromptTokensDetails.CachedTokens,
		usage.PromptTokensDetails.TextTokens,
		usage.PromptTokensDetails.ImageTokens,
		usage.PromptTokensDetails.CachedTokensInternal,
		usage.PromptTokensDetails.CacheWriteTokens,
		usage.PromptTokensDetails.CachedWriteTokens,
		usage.PromptTokensDetails.CachedReadTokens,
		usage.CompletionTokensDetails.AudioTokens,
		usage.CompletionTokensDetails.TextTokens,
		usage.CompletionTokensDetails.ReasoningTokens,
		usage.CompletionTokensDetails.AcceptedPredictionTokens,
		usage.CompletionTokensDetails.RejectedPredictionTokens,
		usage.CompletionTokensDetails.ImageTokens,
	} {
		if value < 0 {
			return false
		}
	}
	for _, value := range usage.ExtraTokens {
		if value < 0 {
			return false
		}
	}
	for _, value := range usage.ExtraUsageUnits {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	for _, value := range usage.ExtraBilling {
		if value.CallCount < 0 {
			return false
		}
	}
	if !usage.ProviderTokenFields["prompt_tokens"] ||
		!usage.ProviderTokenFields["completion_tokens"] ||
		!usage.ProviderTokenFields["total_tokens"] {
		return false
	}
	if usage.PromptTokens > usage.TotalTokens || usage.TotalTokens-usage.PromptTokens != usage.CompletionTokens {
		return false
	}
	return true
}

func requestErrorHandle(resp *http.Response) *types.OpenAIError {
	var errorResponse types.OpenAIErrorResponse
	err := json.NewDecoder(resp.Body).Decode(&errorResponse)
	if err != nil {
		return nil
	}
	return &errorResponse.Error
}

// databricksChatRequest is the request body for Azure Databricks
type databricksChatRequest struct {
	Messages         []types.ChatCompletionMessage `json:"messages"`
	Stream           bool                          `json:"stream,omitempty"`
	MaxTokens        int                           `json:"max_tokens,omitempty"`
	Temperature      float64                       `json:"temperature,omitempty"`
	TopP             float64                       `json:"top_p,omitempty"`
	N                int                           `json:"n,omitempty"`
	Stop             []string                      `json:"stop,omitempty"`
	PresencePenalty  float64                       `json:"presence_penalty,omitempty"`
	FrequencyPenalty float64                       `json:"frequency_penalty,omitempty"`
	Thinking         *Thinking                     `json:"thinking,omitempty"`
}

type Thinking struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// databricksChatResponse is the response body for Azure Databricks
type databricksChatResponse struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Created     int64  `json:"created"`
	Model       string `json:"model"`
	ServiceTier string `json:"service_tier,omitempty"`
	Choices     []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *types.Usage `json:"usage,omitempty"`
}
