package codex

import (
	"encoding/json"

	"one-api/common/requester"
	"one-api/types"
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
	// Use standard conversion.
	responsesRequest := request.ToResponsesRequest()

	// Normalize gpt-5-* model names.
	if len(responsesRequest.Model) > 6 && responsesRequest.Model[:6] == "gpt-5-" && responsesRequest.Model != "gpt-5-codex" {
		responsesRequest.Model = "gpt-5"
	}

	// Codex requires store=false.
	storeFalse := false
	responsesRequest.Store = &storeFalse

	// Prefer temperature over top_p when both set.
	if responsesRequest.Temperature != nil && responsesRequest.TopP != nil {
		responsesRequest.TopP = nil
	}

	// Adapt to Codex CLI format.
	p.adaptCodexCLI(responsesRequest)

	return responsesRequest
}

// applyDefaultHeaders adds Codex defaults without overriding existing values.
func (p *CodexProvider) applyDefaultHeaders(headers map[string]string) {
	// Set Host if missing.
	if _, exists := headers["Host"]; !exists {
		headers["Host"] = "chatgpt.com"
	}

	// Set User-Agent if missing.
	if _, exists := headers["User-Agent"]; !exists {
		// Try custom UA from channel.Other.
		if p.Channel.Other != "" {
			var config map[string]string
			if err := json.Unmarshal([]byte(p.Channel.Other), &config); err == nil {
				if userAgent, exists := config["user_agent"]; exists && userAgent != "" {
					headers["User-Agent"] = userAgent
					return
				}
			}
		}
		// Default UA.
		headers["User-Agent"] = "codex_cli_rs/0.38.0 (Ubuntu 22.4.0; x86_64) WindowsTerminal"
	}

	// Set Accept if missing.
	if _, exists := headers["Accept"]; !exists {
		headers["Accept"] = "application/json"
	}
}
