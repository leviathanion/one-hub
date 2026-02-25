package codex

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"one-api/common"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

// CreateChatCompletion builds a non-streamed response via streaming.
func (p *CodexProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	// Codex only supports streaming, so we stream and aggregate.

	// Convert to Responses format.
	responsesRequest := p.chatToResponsesRequest(request)

	// Force stream (Codex requirement).
	responsesRequest.Stream = true

	// Build request.
	req, errWithCode := p.getResponsesRequest(responsesRequest)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// Send streaming request.
	resp, errWithCode := p.Requester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// Create stream handler.
	chatHandler := &CodexStreamHandler{
		Usage:   p.Usage,
		Request: request,
		Context: p.Context,
	}

	// Get stream response.
	stream, errWithCode := requester.RequestStream(p.Requester, resp, chatHandler.HandlerStream)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// Aggregate full response.
	fullResponse, errWithCode := p.collectStreamResponse(stream, request)
	if errWithCode != nil {
		return nil, errWithCode
	}

	return fullResponse, nil
}

// CreateChatCompletionStream streams chat completion.
func (p *CodexProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	// Convert to Responses format.
	responsesRequest := p.chatToResponsesRequest(request)

	// Force stream (Codex requirement).
	responsesRequest.Stream = true

	// Build request.
	req, errWithCode := p.getResponsesRequest(responsesRequest)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// Send request.
	resp, errWithCode := p.Requester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// Use OpenAI stream handler.
	chatHandler := &CodexStreamHandler{
		Usage:   p.Usage,
		Request: request,
		Context: p.Context,
	}

	return requester.RequestStream(p.Requester, resp, chatHandler.HandlerStream)
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

// CodexStreamHandler converts Responses stream to Chat stream.
type CodexStreamHandler struct {
	Usage   *types.Usage
	Request *types.ChatCompletionRequest
	Context *gin.Context
}

// HandlerStream handles streaming responses.
func (h *CodexStreamHandler) HandlerStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	// Skip empty lines.
	if rawLine == nil || len(*rawLine) == 0 {
		return
	}

	line := string(*rawLine)

	// Ignore non-data lines.
	if !strings.HasPrefix(line, "data:") {
		return
	}

	// Trim data prefix.
	data := strings.TrimPrefix(line, "data:")
	data = strings.TrimSpace(data)

	// Skip [DONE].
	if data == "[DONE]" {
		return
	}

	// Parse Responses stream payload.
	var responsesStream types.OpenAIResponsesStreamResponses
	if err := json.Unmarshal([]byte(data), &responsesStream); err != nil {
		logger.SysError("Failed to unmarshal Codex stream response: " + err.Error())
		return
	}

	// Handle response.completed (usage info).
	if responsesStream.Type == "response.completed" && responsesStream.Response != nil {
		if responsesStream.Response.Usage != nil {
			h.Usage.PromptTokens = responsesStream.Response.Usage.InputTokens
			h.Usage.CompletionTokens = responsesStream.Response.Usage.OutputTokens
			h.Usage.TotalTokens = responsesStream.Response.Usage.TotalTokens
		}
		return
	}

	// Handle response.output_text.delta.
	if responsesStream.Type == "response.output_text.delta" {
		delta, ok := responsesStream.Delta.(string)
		if !ok {
			return
		}

		// Convert to Chat stream response.
		chatResponse := h.convertResponsesStreamToChatStream(&responsesStream, delta)
		if chatResponse != nil {
			responseBody, err := json.Marshal(chatResponse)
			if err != nil {
				logger.SysError("Failed to marshal Chat stream response: " + err.Error())
				return
			}
			dataChan <- string(responseBody)
		}
	}
}

// collectStreamResponse aggregates stream to a single response.
func (p *CodexProvider) collectStreamResponse(stream requester.StreamReaderInterface[string], request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	var fullContent strings.Builder
	var responseID string
	model := request.Model
	finishReason := "stop"

	// Get data and error channels.
	dataChan, errChan := stream.Recv()

	// Read stream.
	for {
		select {
		case data, ok := <-dataChan:
			if !ok {
				// Channel closed.
				goto buildResponse
			}

			// Parse stream chunk.
			var chatStream types.ChatCompletionStreamResponse
			if err := json.Unmarshal([]byte(data), &chatStream); err != nil {
				logger.SysError("Failed to unmarshal stream response: " + err.Error())
				continue
			}

			// Extract response ID.
			if responseID == "" && chatStream.ID != "" {
				responseID = chatStream.ID
			}

			// Extract model.
			if chatStream.Model != "" {
				model = chatStream.Model
			}

			// Collect content.
			if len(chatStream.Choices) > 0 {
				choice := chatStream.Choices[0]
				if choice.Delta.Content != "" {
					fullContent.WriteString(choice.Delta.Content)
				}
				if choice.FinishReason != nil {
					if fr, ok := choice.FinishReason.(string); ok && fr != "" {
						finishReason = fr
					}
				}
			}

		case err, ok := <-errChan:
			if !ok {
				// Error channel closed.
				continue
			}
			if err != nil {
				// EOF is normal end-of-stream.
				if err.Error() == "EOF" {
					goto buildResponse
				}
				logger.SysError("Stream error: " + err.Error())
				return nil, common.ErrorWrapper(err, "stream_read_failed", http.StatusInternalServerError)
			}
		}
	}

buildResponse:
	// Build full non-stream response.
	response := &types.ChatCompletionResponse{
		ID:      responseID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []types.ChatCompletionChoice{
			{
				Index: 0,
				Message: types.ChatCompletionMessage{
					Role:    "assistant",
					Content: fullContent.String(),
				},
				FinishReason: finishReason,
			},
		},
		Usage: &types.Usage{
			PromptTokens:     p.Usage.PromptTokens,
			CompletionTokens: p.Usage.CompletionTokens,
			TotalTokens:      p.Usage.TotalTokens,
		},
	}

	return response, nil
}

// convertResponsesStreamToChatStream converts Responses stream to Chat stream.
func (h *CodexStreamHandler) convertResponsesStreamToChatStream(responsesStream *types.OpenAIResponsesStreamResponses, delta string) *types.ChatCompletionStreamResponse {
	// Get response ID.
	responseID := ""
	if responsesStream.Response != nil {
		responseID = responsesStream.Response.ID
	}

	// Resolve model name.
	model := h.Request.Model
	if model == "" {
		model = h.Request.Model
	}

	response := &types.ChatCompletionStreamResponse{
		ID:      responseID,
		Object:  "chat.completion.chunk",
		Created: 0,
		Model:   model,
		Choices: []types.ChatCompletionStreamChoice{
			{
				Index: 0,
				Delta: types.ChatCompletionStreamChoiceDelta{
					Content: delta,
				},
			},
		},
	}

	return response
}

// convertResponsesToChatCompletion converts Responses to Chat response.
func (p *CodexProvider) convertResponsesToChatCompletion(responsesResp *types.OpenAIResponsesResponses, model string) *types.ChatCompletionResponse {
	// Extract text content.
	content := ""
	if len(responsesResp.Output) > 0 {
		for _, output := range responsesResp.Output {
			if output.Type == types.ContentTypeOutputText {
				content += output.StringContent()
			}
		}
	}

	// Build Chat response.
	chatResponse := &types.ChatCompletionResponse{
		ID:      responsesResp.ID,
		Object:  "chat.completion",
		Created: responsesResp.CreatedAt,
		Model:   model,
		Choices: []types.ChatCompletionChoice{
			{
				Index: 0,
				Message: types.ChatCompletionMessage{
					Role:    types.ChatMessageRoleAssistant,
					Content: content,
				},
				FinishReason: types.ConvertResponsesStatusToChat(responsesResp.Status),
			},
		},
		Usage: &types.Usage{
			PromptTokens:     p.Usage.PromptTokens,
			CompletionTokens: p.Usage.CompletionTokens,
			TotalTokens:      p.Usage.TotalTokens,
		},
	}

	return chatResponse
}
