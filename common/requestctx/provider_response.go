package requestctx

import (
	"net/http"

	"one-api/common/providerresponse"
)

const ProviderResponseHeadersContextKey = "provider_response_headers"
const ProviderResponseStatusContextKey = "provider_response_status"

func SafeProviderResponseHeaders(source http.Header) http.Header {
	return providerresponse.Filter(source, providerresponse.Policy{})
}
