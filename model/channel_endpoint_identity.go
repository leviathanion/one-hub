package model

import (
	"fmt"
	"net/url"

	"one-api/common/providerendpoint"
)

func (channel *Channel) customResponsesURI() (string, error) {
	return channel.ResolveEndpoint(providerendpoint.Responses)
}

// CustomResponsesEndpointIdentity 是已绑定资源使用的 HTTP 端点身份；查询和转义路径也可能选择命名空间。
func (channel *Channel) CustomResponsesEndpointIdentity() (string, error) {
	requestURI, err := channel.customResponsesURI()
	if err != nil || requestURI == "" {
		return "", err
	}
	parsedURI, err := providerendpoint.ParseResponsesURI(requestURI)
	if err != nil {
		return "", fmt.Errorf("Custom Responses 端点无效: %w", err)
	}
	endpoint, err := url.Parse(providerendpoint.CustomRequestURL(channel.GetBaseURL(), parsedURI.String()))
	if err != nil {
		return "", fmt.Errorf("Custom Responses 端点无效: %w", err)
	}
	// fragment 不会发送给 HTTP 服务端，其余 URL 部分保持实际请求语义。
	endpoint.Fragment, endpoint.RawFragment = "", ""
	return endpoint.String(), nil
}

func (channel *Channel) customResponsesWSEndpointIdentity() (string, error) {
	requestURI, err := channel.customResponsesURI()
	if err != nil || requestURI == "" {
		return "", err
	}
	// WS 沿用先拼接再解析完整 URL 的语义，不能被 HTTP 对相对 URI 的预先 trim 改写。
	rawURL := providerendpoint.CustomRequestURL(channel.GetBaseURL(), requestURI)
	return customResponsesWSURLIdentity(rawURL), nil
}

func (channel *Channel) customResponsesRealtimeNamedEndpointIdentity() (string, error) {
	requestURI, err := channel.customResponsesURI()
	if err != nil || requestURI == "" {
		return "", err
	}
	// 模型名仅追加在 URI 之后；固定投影可比较该命名分支的完整前缀，不限制客户端模型集合。
	const projectionModel = "one-hub-realtime-identity"
	rawURL := providerendpoint.NonAzureRealtimeRequestURL(channel.GetBaseURL(), requestURI, projectionModel)
	return customResponsesWSURLIdentity(rawURL), nil
}

func customResponsesWSURLIdentity(rawURL string) string {
	endpoint, err := providerendpoint.ParseResponsesURI(rawURL)
	if err != nil {
		// 旧配置可能仅支持 HTTP；保守保留其无效 WS 标识，仍允许原样的业务编辑。
		return "invalid URL: " + rawURL
	}
	switch endpoint.Scheme {
	case "https":
		endpoint.Scheme = "wss"
	case "http":
		endpoint.Scheme = "ws"
	}
	return endpoint.String()
}
