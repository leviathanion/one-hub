package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"one-api/common"
	"one-api/common/logger"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/internal/requesthints"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// CreateChatCompletion builds a non-streamed response via streaming.
func (p *CodexProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	// Convert to Responses format.
	responsesRequest := p.chatToResponsesRequest(request)

	// Use Responses aggregation first, then convert to Chat.
	rawReq, errWithCode := p.chatResponsesRequestFromTyped(responsesRequest)
	if errWithCode != nil {
		return nil, errWithCode
	}
	responsesResponse, errWithCode := p.CreateResponses(p.codexProviderContext(), rawReq)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// Guard nil usage to avoid conversion panic on r.Usage.ToOpenAIUsage().
	if responsesResponse.Usage == nil {
		responsesResponse.Usage = p.Usage.ToResponsesUsage()
	}

	return responsesResponse.ToChat(), nil
}

// CreateChatCompletionStream streams chat completion.
func (p *CodexProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	// Convert to Responses format.
	responsesRequest := p.chatToResponsesRequest(request)

	// Convert Responses stream events to Chat stream chunks.
	responsesRequest.Stream = true
	responsesRequest.ConvertChat = true
	rawReq, errWithCode := p.chatResponsesRequestFromTyped(responsesRequest)
	if errWithCode != nil {
		return nil, errWithCode
	}
	return p.CreateResponsesStream(p.codexProviderContext(), rawReq)
}

// chatToResponsesRequest converts ChatCompletionRequest to OpenAIResponsesRequest.
func (p *CodexProvider) chatToResponsesRequest(request *types.ChatCompletionRequest) *types.OpenAIResponsesRequest {
	converted := request.ToResponsesRequest()
	// The adapter authors this synthesized body, so parameter conflicts are
	// resolved here instead of surfacing the Codex planner's 400 for a body
	// the client never wrote: keep temperature, drop top_p.
	if converted.Temperature != nil && converted.TopP != nil {
		converted.TopP = nil
		logger.LogDebug(p.codexProviderContext(), `[Codex] chat adapter decision {"dialect":"codex_official","field":"top_p","action":"drop","source":"chat_adapter","reason":"temperature-and-top_p-both-present"}`)
	}
	return converted
}

// chatResponsesRequestFromTyped is only the /v1/chat/completions adapter.
// Chat ingress has no raw Responses envelope to preserve, so the adapter creates
// one at the boundary and then hands the request to the same raw Responses
// planner used by /v1/responses. This must not become a fallback for Responses
// ingress, where the downstream raw body is the protocol truth.
func (p *CodexProvider) chatResponsesRequestFromTyped(request *types.OpenAIResponsesRequest) (*commonresponses.Request, *types.OpenAIErrorWithStatusCode) {
	if request == nil {
		return nil, common.StringErrorWrapperLocal("request is required", "invalid_request_error", http.StatusBadRequest)
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
	if p != nil {
		if p.Context != nil && p.Context.Request != nil {
			headers = requestctx.NewHeaderSnapshot(p.Context.Request.Header)
			principal = requestctx.PrincipalFromGin(p.Context)
		}
		if channel := p.codexChannel(); channel != nil {
			channelID = channel.Id
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
		Policy:    codexResponsesPolicyFromProjection(request, p.Context),
		Principal: principal,
		ChannelID: channelID,
		Model:     request.Model,
	}, nil
}

func codexResponsesPolicyFromProjection(request *types.OpenAIResponsesRequest, ctx *gin.Context) commonresponses.PolicyInput {
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
	if key := requesthints.Get(ctx, requesthints.ResponsesPromptCacheKey); key != "" {
		policy.PromptCache = &commonresponses.PromptCacheDecision{
			Key:    key,
			Source: commonresponses.PromptCacheRouteHint,
		}
	}
	return policy
}

func (p *CodexProvider) codexProviderContext() context.Context {
	if p != nil && p.Context != nil && p.Context.Request != nil {
		return p.Context.Request.Context()
	}
	return context.Background()
}

// applyDefaultHeaders adds Codex defaults without overriding existing values.
func (p *CodexProvider) applyDefaultHeaders(headers *codexHeaderBag) {
	if headers == nil {
		return
	}

	// Set Host if missing.
	if !headers.Has("Host") {
		if host := p.defaultCodexHostHeader(); host != "" {
			headers.Set("Host", host)
		}
	}

	// Set User-Agent if missing.
	if !headers.Has("User-Agent") {
		headers.Set("User-Agent", defaultUserAgent)
	}

	// Match Codex CLI behavior when the caller does not pin a session.
	if !headers.Has("session_id") && !headers.Has("x-session-id") {
		headers.Set("session_id", uuid.NewString())
	}

	// Smart originator: pass through client value, otherwise auto-select
	// codex-tui for official clients or pi for non-official. Resolve from the
	// effective upstream UA after channel/default overrides; using the incoming
	// UA can produce inconsistent upstream identity when ModelHeaders replace it.
	if !headers.Has("originator") {
		headers.Set("originator", resolveSmartOriginatorForEffectiveUserAgent(headers.Get("User-Agent")))
	}

	// Keep the connection alive for SSE/non-SSE Codex responses.
	if !headers.Has("Connection") {
		headers.Set("Connection", "Keep-Alive")
	}

	// Set Accept if missing.
	if !headers.Has("Accept") {
		headers.Set("Accept", "application/json")
	}
}

func (p *CodexProvider) defaultCodexHostHeader() string {
	baseURL := strings.TrimSpace(p.GetBaseURL())
	if baseURL == "" {
		return ""
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(parsed.Host)
}

func hasHeader(headers map[string]string, key string) bool {
	return newCodexHeaderBagFromMap(headers).Has(key)
}

func getHeaderValue(headers map[string]string, key string) string {
	return newCodexHeaderBagFromMap(headers).Get(key)
}

func replaceHeader(headers map[string]string, key, value string) {
	if headers == nil {
		return
	}
	bag := newCodexHeaderBagFromMap(headers)
	bag.Set(key, value)
	clear(headers)
	for headerKey, headerValue := range bag.Map() {
		headers[headerKey] = headerValue
	}
}
