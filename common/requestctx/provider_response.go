package requestctx

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"one-api/common/providerresponse"
)

const ProviderResponseHeadersContextKey = "provider_response_headers"
const ProviderResponseStatusContextKey = "provider_response_status"

const providerCredentialsKey = "provider_response_credentials"

// SetProviderCredentials 保存实际上游请求的凭据快照，不能读取后续渠道配置。
func SetProviderCredentials(c *gin.Context, values []string) {
	if c != nil {
		c.Set(providerCredentialsKey, append([]string(nil), values...))
	}
}

func ProviderCredentials(c *gin.Context) []string {
	if c == nil {
		return nil
	}
	value, _ := c.Get(providerCredentialsKey)
	credentials, _ := value.([]string)
	return append([]string(nil), credentials...)
}

func SafeProviderResponseHeaders(source http.Header, credentials ...string) http.Header {
	return providerresponse.FilterCredentialHeaders(providerresponse.Filter(source, providerresponse.Policy{}), credentials...)
}
