package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"one-api/common"
	"one-api/common/jsonobject"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/common/utils"
	"one-api/internal/requesthints"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/safty"
	"one-api/types"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type relayChat struct {
	relayBase
	chatRequest        types.ChatCompletionRequest
	rawEnvelope        *jsonobject.Object
	streamResponseMeta chatStreamResponseMetadata
}

func chatPromptCacheKey(fields map[string]json.RawMessage) string {
	raw, ok := fields["prompt_cache_key"]
	if !ok {
		return ""
	}
	var key string
	if json.Unmarshal(raw, &key) != nil {
		return ""
	}
	return strings.TrimSpace(key)
}

type chatStreamResponseMetadata struct {
	ID          string
	Object      string
	Created     json.RawMessage
	Model       string
	ServiceTier string
	UsageSeen   bool
}

func NewRelayChat(c *gin.Context) *relayChat {
	relay := &relayChat{
		relayBase: relayBase{
			allowHeartbeat: true,
			c:              c,
		},
	}
	return relay
}

func (r *relayChat) setRequest() error {
	if err := r.decodeCurrentRequestBody(); err != nil {
		return err
	}
	r.setOriginalModel(r.chatRequest.Model)
	r.c.Set("skip_only_chat", len(r.chatRequest.Tools) > 0)
	prepareChatChannelAffinity(r.c, r.chatRequest.Model, chatPromptCacheKey(r.rawEnvelope.Fields))
	setRequestChannelCapability(r.c, requireChatChannelCompatibility(r.chatRequest.Model, &r.chatRequest, r.rawEnvelope.Fields))
	return nil
}

func (r *relayChat) materializeSelectedProviderRequest() error {
	return r.decodeCurrentRequestBody()
}

func (r *relayChat) decodeCurrentRequestBody() error {
	r.chatRequest = types.ChatCompletionRequest{}
	raw, err := common.CacheRequestBody(r.c)
	if err != nil {
		return err
	}
	envelope, err := jsonobject.Parse(raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &r.chatRequest); err != nil {
		return err
	}
	r.rawEnvelope = envelope
	if strings.TrimSpace(r.chatRequest.Model) == "" {
		return errors.New("field Model is required")
	}

	if r.chatRequest.MaxTokens < 0 || r.chatRequest.MaxTokens > math.MaxInt32/2 {
		return errors.New("max_tokens is invalid")
	}

	// 归一化：将 MaxTokens 统一到 MaxCompletionTokens
	if r.chatRequest.MaxTokens > 0 && r.chatRequest.MaxCompletionTokens == 0 {
		r.chatRequest.MaxCompletionTokens = r.chatRequest.MaxTokens
	}
	r.chatRequest.MaxTokens = 0

	// 归一化：统一 ReasoningEffort 和 Reasoning
	r.chatRequest.NormalizeReasoning()
	if err := validateChatSupportedSurface(&r.chatRequest, envelope.Fields); err != nil {
		return err
	}

	if !r.chatRequest.Stream {
		r.chatRequest.StreamOptions = nil
	}
	return nil
}

func (r *relayChat) prepareSelectedProviderRemoteMedia() error {
	if r == nil || r.provider == nil {
		return nil
	}
	r.chatRequest.Model = r.modelName
	fetcher := r.remoteMedia
	if fetcher == nil {
		fetcher = newRequestRemoteMediaFetcher(r.c.Request.Context())
	}
	if err := providers.PrepareChatRemoteMedia(r.provider, &r.chatRequest, fetcher); err != nil {
		return remoteMediaCapabilityGateError(err)
	}
	return nil
}

func (r *relayChat) getRequest() interface{} {
	return &r.chatRequest
}

func (r *relayChat) IsStream() bool {
	return r.chatRequest.Stream
}

func (r *relayChat) getPromptTokens() (int, error) {
	channel := r.provider.GetChannel()
	return common.CountTokenMessages(r.chatRequest.Messages, r.modelName, channel.PreCost), nil
}

var chatModelsRequiringResponses = map[string]struct{}{
	"o3-pro-2025-06-10":                {},
	"o3-pro":                           {},
	"o1-pro-2025-03-19":                {},
	"o1-pro":                           {},
	"o3-deep-research-2025-06-26":      {},
	"o3-deep-research":                 {},
	"o4-mini-deep-research-2025-06-26": {},
	"o4-mini-deep-research":            {},
	"codex-mini-latest":                {},
}

func chatModelRequiresResponses(modelName string) bool {
	_, ok := chatModelsRequiringResponses[strings.ToLower(strings.TrimSpace(modelName))]
	return ok
}

func (r *relayChat) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	err, done = r.sendCurrentProvider()
	if err == nil && r.provider != nil && r.provider.GetChannel() != nil {
		refreshChannelAffinityForSelectedModel(r.c, channelAffinityKindChat, r.provider.GetChannel(), r.getOriginalModel())
		recordCurrentChannelAffinity(r.c, channelAffinityKindChat, r.provider.GetChannel().Id)
	}
	return
}

func (r *relayChat) sendCurrentProvider() (*types.OpenAIErrorWithStatusCode, bool) {
	r.chatRequest.Model = r.modelName
	if chatModelRequiresResponses(r.modelName) {
		responsesProvider, ok := r.provider.(providersBase.ResponsesInterface)
		if !ok {
			return common.StringErrorWrapperLocal("selected channel cannot send this model through the Responses API", unsupportedCapabilityCode, http.StatusServiceUnavailable), true
		}
		return r.compatibleSend(responsesProvider)
	}

	chatProvider, ok := r.provider.(providersBase.ChatInterface)
	if !ok {
		return common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable), true
	}

	// 内容审查
	for _, message := range r.chatRequest.Messages {
		if message.Content != nil {
			CheckResult, _ := safty.CheckContent(message.Content)
			if !CheckResult.IsSafe {
				return common.StringErrorWrapperLocal(CheckResult.Reason, CheckResult.Code, http.StatusBadRequest), true
			}
		}
	}

	if r.chatRequest.Stream {
		var response requester.StreamReaderInterface[string]
		response, err := chatProvider.CreateChatCompletionStream(&r.chatRequest)
		if err != nil {
			return err, false
		}

		if r.heartbeat != nil {
			r.heartbeat.Stop()
		}

		doneStr := func() string {
			return r.getUsageResponse()
		}

		var firstResponseTime time.Time
		firstResponseTime, streamErr := responseStreamClient(r.c, response, doneStr, r.observeStreamResponseMetadata)
		r.SetFirstResponseTime(firstResponseTime)
		if streamErr != nil {
			return streamErr, true
		}
	} else {
		var response *types.ChatCompletionResponse
		response, err := chatProvider.CreateChatCompletion(&r.chatRequest)
		if err != nil {
			return err, false
		}

		if r.heartbeat != nil {
			r.heartbeat.Stop()
		}

		if err := responseJsonClient(r.c, response); err != nil {
			return err, true
		}
	}

	return nil, false
}

func (r *relayChat) getUsageResponse() string {
	if r.chatRequest.StreamOptions == nil || !r.chatRequest.StreamOptions.IncludeUsage || r.streamResponseMeta.UsageSeen || r.provider == nil {
		return ""
	}
	providerUsage := r.provider.GetUsage()
	if providerUsage.HasProviderUsage() {
		id := strings.TrimSpace(r.streamResponseMeta.ID)
		if id == "" {
			id = fmt.Sprintf("chatcmpl-%s", utils.GetUUID())
		}
		created := any(utils.GetTimestamp())
		if len(r.streamResponseMeta.Created) > 0 {
			created = r.streamResponseMeta.Created
		}
		object := strings.TrimSpace(r.streamResponseMeta.Object)
		if object == "" {
			object = "chat.completion.chunk"
		}
		modelName := strings.TrimSpace(r.streamResponseMeta.Model)
		if modelName == "" {
			modelName = r.chatRequest.Model
		}
		usageResponse := types.ChatCompletionStreamResponse{
			ID:          id,
			Object:      object,
			Created:     created,
			Model:       modelName,
			ServiceTier: r.streamResponseMeta.ServiceTier,
			Choices:     []types.ChatCompletionStreamChoice{},
			Usage:       providerUsage,
		}

		responseBody, err := json.Marshal(usageResponse)
		if err != nil {
			return ""
		}

		return string(responseBody)
	}
	return ""
}

func (r *relayChat) observeStreamResponseMetadata(data string) {
	if r == nil || strings.TrimSpace(data) == "" || data == "[DONE]" {
		return
	}
	var envelope struct {
		ID          string          `json:"id"`
		Object      string          `json:"object"`
		Created     json.RawMessage `json:"created"`
		Model       string          `json:"model"`
		ServiceTier string          `json:"service_tier"`
		Usage       json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return
	}
	if r.streamResponseMeta.ID == "" {
		r.streamResponseMeta.ID = envelope.ID
	}
	if r.streamResponseMeta.Object == "" {
		r.streamResponseMeta.Object = envelope.Object
	}
	if len(r.streamResponseMeta.Created) == 0 && len(envelope.Created) > 0 && string(envelope.Created) != "null" {
		r.streamResponseMeta.Created = append(json.RawMessage(nil), envelope.Created...)
	}
	if envelope.Model != "" {
		r.streamResponseMeta.Model = envelope.Model
	}
	if envelope.ServiceTier != "" {
		r.streamResponseMeta.ServiceTier = envelope.ServiceTier
	}
	if len(envelope.Usage) > 0 && string(envelope.Usage) != "null" {
		r.streamResponseMeta.UsageSeen = true
	}
}

func (r *relayChat) compatibleSend(resProvider providersBase.ResponsesInterface) (*types.OpenAIErrorWithStatusCode, bool) {
	resRequest := r.chatRequest.ToResponsesRequest()
	resRequest.ConvertChat = true
	rawReq, buildErr := r.responsesFallbackRequest(resRequest)
	if buildErr != nil {
		return buildErr, true
	}

	if r.chatRequest.Stream {
		response, err := resProvider.CreateResponsesStream(r.c.Request.Context(), rawReq)
		if err != nil {
			return err, false
		}

		if r.heartbeat != nil {
			r.heartbeat.Stop()
		}

		doneStr := func() string {
			return r.getUsageResponse()
		}

		firstResponseTime, streamErr := responseStreamClient(r.c, response, doneStr, r.observeStreamResponseMetadata)
		r.SetFirstResponseTime(firstResponseTime)
		if streamErr != nil {
			return streamErr, true
		}
	} else {
		response, err := resProvider.CreateResponses(r.c.Request.Context(), rawReq)
		if err != nil {
			return err, false
		}

		if r.heartbeat != nil {
			r.heartbeat.Stop()
		}
		if terminalErr := commonresponses.ChatTerminalError(response); terminalErr != nil {
			return terminalErr, true
		}
		if err := responseJsonClient(r.c, response.ToChat()); err != nil {
			return err, true
		}
	}

	return nil, false
}

func (r *relayChat) responsesFallbackRequest(request *types.OpenAIResponsesRequest) (*commonresponses.Request, *types.OpenAIErrorWithStatusCode) {
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "marshal_request_failed", http.StatusInternalServerError)
	}
	envelope, err := commonresponses.ParseRawEnvelope(raw)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_request_error", http.StatusBadRequest)
	}
	downstreamDialect := commonresponses.DownstreamResponses
	if request.ConvertChat {
		downstreamDialect = commonresponses.DownstreamChatCompletions
	}
	headers := requestctx.HeaderSnapshot{}
	principal := requestctx.Principal{}
	channelID := 0
	if r != nil {
		if r.c != nil {
			if r.c.Request != nil {
				headers = requestctx.NewHeaderSnapshot(r.c.Request.Header)
			}
			principal = requestctx.PrincipalFromGin(r.c)
		}
		if r.provider != nil && r.provider.GetChannel() != nil {
			channelID = r.provider.GetChannel().Id
		}
	}
	return &commonresponses.Request{
		Operation: commonresponses.ResponsesCreate,
		Headers:   headers,
		Body:      envelope,
		Control: commonresponses.Control{
			DownstreamDialect: downstreamDialect,
			Stream:            request.Stream,
		},
		Policy:    chatResponsesPolicyInput(request, r.c),
		Principal: principal,
		ChannelID: channelID,
		Model:     request.Model,
	}, nil
}

func chatResponsesPolicyInput(request *types.OpenAIResponsesRequest, c *gin.Context) commonresponses.PolicyInput {
	policy := commonresponses.PolicyInput{}
	if request != nil {
		if key := strings.TrimSpace(request.PromptCacheKey); key != "" {
			policy.PromptCache = &commonresponses.PromptCacheDecision{
				Key:    key,
				Source: commonresponses.PromptCacheClientBody,
			}
			return policy
		}
	}
	if key := requesthints.Get(c, requesthints.ResponsesPromptCacheKey); key != "" {
		policy.PromptCache = &commonresponses.PromptCacheDecision{
			Key:    key,
			Source: commonresponses.PromptCacheRouteHint,
		}
	}
	return policy
}
