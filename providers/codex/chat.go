package codex

import (
	"net/url"
	"strings"

	"one-api/common/requester"
	"one-api/types"

	"github.com/google/uuid"
)

// CreateChatCompletion builds a non-streamed response via streaming.
func (p *CodexProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	// Convert to Responses format.
	responsesRequest := p.chatToResponsesRequest(request)

	// Use Responses aggregation first, then convert to Chat.
	responsesResponse, errWithCode := p.CreateResponses(responsesRequest)
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
	return p.CreateResponsesStream(responsesRequest)
}

// chatToResponsesRequest converts ChatCompletionRequest to OpenAIResponsesRequest.
func (p *CodexProvider) chatToResponsesRequest(request *types.ChatCompletionRequest) *types.OpenAIResponsesRequest {
	return request.ToResponsesRequest()
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
		if userAgent := p.getLegacyUserAgentOverride(); userAgent != "" {
			headers.Set("User-Agent", userAgent)
		} else {
			headers.Set("User-Agent", defaultUserAgent)
		}
	}

	// Match Codex CLI behavior when the caller does not pin a session.
	if !headers.Has("session_id") && !headers.Has("x-session-id") {
		headers.Set("session_id", uuid.NewString())
	}

	// Set Originator when missing to mimic Codex CLI requests.
	if !headers.Has("Originator") {
		headers.Set("Originator", defaultOriginator)
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
