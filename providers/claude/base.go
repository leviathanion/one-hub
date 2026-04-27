package claude

import (
	"encoding/json"
	"fmt"
	"net/http"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
	"strings"
)

type ClaudeProviderFactory struct{}

// 创建 ClaudeProvider
func (f ClaudeProviderFactory) Create(channel *model.Channel) base.ProviderInterface {
	return CreateClaudeProvider(channel, "")
}

func CreateClaudeProvider(channel *model.Channel, baseURL string) *ClaudeProvider {
	claudeConfig := getConfig()
	baseURLOverride := ""
	if strings.TrimSpace(baseURL) != "" {
		baseURLOverride = strings.TrimSpace(baseURL)
		claudeConfig.BaseURL = baseURLOverride
	}
	return &ClaudeProvider{
		BaseProvider: base.BaseProvider{
			Config:    claudeConfig,
			Channel:   channel,
			Requester: requester.NewHTTPRequester(*channel.Proxy, RequestErrorHandle),
		},
		BaseURLOverride: baseURLOverride,
	}
}

type ClaudeProvider struct {
	base.BaseProvider
	BaseURLOverride string
}

func getConfig() base.ProviderConfig {
	return base.ProviderConfig{
		BaseURL:         "https://api.anthropic.com",
		ChatCompletions: "/v1/messages",
		ModelList:       "/v1/models",
	}
}

// 请求错误处理
func RequestErrorHandle(resp *http.Response) *types.OpenAIError {
	claudeError := &ClaudeError{}
	err := json.NewDecoder(resp.Body).Decode(claudeError)
	if err != nil {
		return nil
	}

	return errorHandle(claudeError)
}

// 错误处理
func errorHandle(claudeError *ClaudeError) *types.OpenAIError {
	if claudeError == nil {
		return nil
	}

	if claudeError.Type == "" {
		return nil
	}
	return &types.OpenAIError{
		Message: claudeError.ErrorInfo.Message,
		Type:    claudeError.ErrorInfo.Type,
		Code:    claudeError.Type,
	}
}

func (p *ClaudeProvider) GetBaseURL() string {
	if strings.TrimSpace(p.BaseURLOverride) != "" {
		return strings.TrimSpace(p.BaseURLOverride)
	}
	return p.BaseProvider.GetBaseURL()
}

// 获取请求头
func (p *ClaudeProvider) GetRequestHeaders() (headers map[string]string) {
	headers = make(map[string]string)
	p.CommonRequestHeaders(headers)

	headers["x-api-key"] = p.Channel.Key
	anthropicVersion := p.Context.Request.Header.Get("anthropic-version")
	if anthropicVersion == "" {
		anthropicVersion = "2023-06-01"
	}
	headers["anthropic-version"] = anthropicVersion

	return headers
}

func (p *ClaudeProvider) GetFullRequestURL(requestURL string) string {
	baseURL := strings.TrimSuffix(p.GetBaseURL(), "/")
	if strings.HasPrefix(baseURL, "https://gateway.ai.cloudflare.com") {
		requestURL = strings.TrimPrefix(requestURL, "/v1")
	}

	// base_url is treated as the upstream base path, not a full endpoint that
	// this layer sanitizes. If an admin configures /v1/messages here, the final
	// URL intentionally includes the endpoint twice so the bad base path remains
	// visible instead of being silently corrected.
	return fmt.Sprintf("%s%s", baseURL, requestURL)
}

func stopReasonClaude2OpenAI(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return types.FinishReasonStop
	case "max_tokens":
		return types.FinishReasonLength
	case "tool_use":
		return types.FinishReasonToolCalls
	case "refusal":
		return types.FinishReasonContentFilter
	default:
		return reason
	}
}

func convertRole(role string) string {
	switch role {
	case types.ChatMessageRoleUser, types.ChatMessageRoleTool, types.ChatMessageRoleFunction:
		return types.ChatMessageRoleUser
	default:
		return types.ChatMessageRoleAssistant
	}
}
