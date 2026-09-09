package providerendpoint

import (
	"net/url"
	"strings"
)

const DefaultResponsesURI = "/v1/responses"

// ParseResponsesURI 与 Responses 的路径追加、查询合并保持同一 URI 规范化。
func ParseResponsesURI(rawURI string) (*url.URL, error) {
	return url.Parse(strings.TrimSpace(rawURI))
}

// CustomRequestURL 沿用 Custom 渠道的绝对 URI 优先和 Cloudflare 路径规则。
func CustomRequestURL(baseURL, requestURI string) string {
	baseURL = strings.TrimSuffix(baseURL, "/")
	if parsed, err := url.Parse(strings.TrimSpace(requestURI)); err == nil && parsed.IsAbs() {
		return parsed.String()
	}
	if strings.HasPrefix(baseURL, "https://gateway.ai.cloudflare.com") {
		requestURI = strings.TrimPrefix(requestURI, "/v1")
	}
	return baseURL + requestURI
}

// NonAzureRealtimeRequestURL 保留非 Azure 的 -realtime 命名分支及其原始查询拼接语义。
func NonAzureRealtimeRequestURL(baseURL, requestURI, modelName string) string {
	baseURL = strings.TrimSuffix(baseURL, "/")
	if strings.HasPrefix(baseURL, "https://") {
		baseURL = strings.Replace(baseURL, "https://", "wss://", 1)
	} else {
		baseURL = strings.Replace(baseURL, "http://", "ws://", 1)
	}
	return baseURL + requestURI + "?model=" + modelName
}
