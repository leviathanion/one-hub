package providerresponse

import (
	"net/http"
	"net/url"
	"strings"

	"one-api/common"
)

// CredentialSource 由实际建立上游连接的 provider 实现，生命周期随该连接。
type CredentialSource interface {
	ProviderCredentials() []string
}

// RequestCredentials 只读取请求构造器持有的认证字段，不扫描用户 body。
func RequestCredentials(req *http.Request) []string {
	if req == nil {
		return nil
	}
	var values []string
	for _, name := range []string{"Authorization", "Api-Key", "X-Api-Key", "X-Goog-Api-Key", "Ocp-Apim-Subscription-Key", "Proxy-Authorization", "X-Amz-Security-Token", "Mj-Api-Secret"} {
		for _, value := range req.Header.Values(name) {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			values = append(values, value)
			if index := strings.IndexByte(value, ' '); index >= 0 {
				values = append(values, strings.TrimSpace(value[index+1:]))
			}
		}
	}
	if username, password, ok := req.BasicAuth(); ok {
		values = append(values, username+":"+password, password)
	}
	if req.URL != nil {
		for key, entries := range req.URL.Query() {
			if key == "key" || key == "authorization" || common.SensitiveCredentialLabel(key) {
				values = append(values, entries...)
			}
		}
	}
	return values
}

// ConnectionCredentials 捕获已校验的 WS 握手实际认证值。
func ConnectionCredentials(rawURL string, headers http.Header) []string {
	parsed, _ := url.Parse(rawURL)
	return RequestCredentials(&http.Request{URL: parsed, Header: headers})
}

// CredentialSnapshot 是连接认证事实的只读副本，随连接及已入队消息释放。
type CredentialSnapshot struct{ values []string }

func NewCredentialSnapshot(values []string) *CredentialSnapshot {
	return &CredentialSnapshot{values: append([]string(nil), values...)}
}

func (s *CredentialSnapshot) ProviderCredentials() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.values...)
}

func (s *CredentialSnapshot) String() string { return "[provider credentials redacted]" }
