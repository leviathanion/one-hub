package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/common/utils"
	"one-api/internal/requesthints"
	providersBase "one-api/providers/base"
	"one-api/safty"
	"one-api/types"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type relayChat struct {
	relayBase
	chatRequest types.ChatCompletionRequest
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
	r.chatRequest = types.ChatCompletionRequest{}
	if err := common.UnmarshalBodyReusable(r.c, &r.chatRequest); err != nil {
		return err
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

	if r.chatRequest.Tools != nil {
		r.c.Set("skip_only_chat", true)
	}

	if !r.chatRequest.Stream {
		r.chatRequest.StreamOptions = nil
	}

	r.setOriginalModel(r.chatRequest.Model)

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

var chatModelsRequiringResponses = map[string]bool{
	"o3-pro-2025-06-10":                true,
	"o3-pro":                           true,
	"o1-pro-2025-03-19":                true,
	"o1-pro":                           true,
	"o3-deep-research-2025-06-26":      true,
	"o3-deep-research":                 true,
	"o4-mini-deep-research-2025-06-26": true,
	"o4-mini-deep-research":            true,
	"codex-mini-latest":                true,
}

func (r *relayChat) send() (*types.OpenAIErrorWithStatusCode, bool) {
	r.chatRequest.Model = r.modelName

	if chatModelsRequiringResponses[r.modelName] {
		resProvider, ok := r.provider.(providersBase.ResponsesInterface)
		if ok {
			return r.compatibleSend(resProvider)
		}
	}

	chatProvider, ok := r.provider.(providersBase.ChatInterface)
	if !ok {
		return common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable), true
	}

	// 内容审查
	if config.EnableSafe {
		for _, message := range r.chatRequest.Messages {
			if message.Content != nil {
				CheckResult, _ := safty.CheckContent(message.Content)
				if !CheckResult.IsSafe {
					return common.StringErrorWrapperLocal(CheckResult.Reason, CheckResult.Code, http.StatusBadRequest), true
				}
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
		firstResponseTime, streamErr := responseStreamClient(r.c, response, doneStr)
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
	if r.chatRequest.StreamOptions != nil && r.chatRequest.StreamOptions.IncludeUsage {
		usageResponse := types.ChatCompletionStreamResponse{
			ID:      fmt.Sprintf("chatcmpl-%s", utils.GetUUID()),
			Object:  "chat.completion.chunk",
			Created: utils.GetTimestamp(),
			Model:   r.chatRequest.Model,
			Choices: []types.ChatCompletionStreamChoice{},
			Usage:   r.provider.GetUsage(),
		}

		responseBody, err := json.Marshal(usageResponse)
		if err != nil {
			return ""
		}

		return string(responseBody)
	}

	return ""
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

		firstResponseTime, streamErr := responseStreamClient(r.c, response, doneStr)
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
		if err := responseJsonClient(r.c, response.ToChat()); err != nil {
			return err, true
		}
	}

	return nil, false
}

func (r *relayChat) responsesFallbackRequest(request *types.OpenAIResponsesRequest) (*commonresponses.Request, *types.OpenAIErrorWithStatusCode) {
	// This fallback authors the synthesized Responses body, so dialect
	// conflicts are resolved at the conversion boundary. The Codex planner
	// rejects temperature+top_p; a chat client never wrote this body, so keep
	// temperature and drop top_p instead of surfacing a 400.
	if request.Temperature != nil && request.TopP != nil && r.provider != nil {
		if channel := r.provider.GetChannel(); channel != nil && channel.Type == config.ChannelTypeCodex {
			request.TopP = nil
			logger.LogDebug(r.c.Request.Context(), `[Codex] chat fallback decision {"dialect":"codex_official","field":"top_p","action":"drop","source":"chat_adapter","reason":"temperature-and-top_p-both-present"}`)
		}
	}
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
