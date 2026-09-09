package requestctx

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
)

var registeredExactWireOwnedRequestHeaders = map[string]struct{}{
	"authorization":               {},
	"api-key":                     {},
	"x-api-key":                   {},
	"x-goog-api-key":              {},
	"cookie":                      {},
	"host":                        {},
	"content-length":              {},
	"accept-encoding":             {},
	"connection":                  {},
	"proxy-connection":            {},
	"keep-alive":                  {},
	"proxy-authenticate":          {},
	"proxy-authorization":         {},
	"te":                          {},
	"trailer":                     {},
	"transfer-encoding":           {},
	"upgrade":                     {},
	"openai-organization":         {},
	"openai-project":              {},
	"x-channel-id":                {},
	"x-one-api-channel":           {},
	"x-one-api-channel-id":        {},
	"x-new-api-channel":           {},
	"x-new-api-channel-id":        {},
	"x-one-hub-channel":           {},
	"x-one-hub-channel-id":        {},
	"forwarded":                   {},
	"x-real-ip":                   {},
	"cf-connecting-ip":            {},
	"cf-access-jwt-assertion":     {},
	"x-goog-iap-jwt-assertion":    {},
	"x-envoy-external-address":    {},
	"x-auth-request-access-token": {},
	"x-original-authorization":    {},
	"x-pomerium-jwt-assertion":    {},
	"x-vouch-token":               {},
	"x-amzn-oidc-data":            {},
	"x-amzn-oidc-identity":        {},
	"x-amzn-oidc-accesstoken":     {},
}

var registeredExactWireDeniedRequestHeaderPrefixes = []string{
	"cf-access-",
	"sec-websocket-",
	"x-amzn-oidc-",
	"x-auth-request-",
	"x-forwarded-",
	"x-goog-authenticated-user-",
	"x-goog-iap-",
	"x-ms-client-principal",
	"x-ms-token-",
	"x-pomerium-",
	"x-vouch-",
}

var configuredExactWireOwnedRequestHeaders = struct {
	sync.RWMutex
	names map[string]struct{}
}{names: make(map[string]struct{})}

// ConfigureExactWireOwnedRequestHeaders replaces the deployment-specific
// ingress identity header registry. Invalid HTTP field names are ignored and
// therefore cannot accidentally broaden forwarding.
func ConfigureExactWireOwnedRequestHeaders(names []string) {
	configured := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if !validHTTPHeaderFieldName(name) {
			continue
		}
		configured[name] = struct{}{}
	}
	configuredExactWireOwnedRequestHeaders.Lock()
	configuredExactWireOwnedRequestHeaders.names = configured
	configuredExactWireOwnedRequestHeaders.Unlock()
}

func validHTTPHeaderFieldName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

// ApplyRegisteredExactWireRequestHeaders forwards client business headers for
// an explicitly registered exact-wire HTTP surface. Headers already present in
// dst are authoritative (for example channel model_headers and provider auth).
func ApplyRegisteredExactWireRequestHeaders(dst http.Header, inbound HeaderSnapshot) error {
	if dst == nil {
		return fmt.Errorf("destination headers are nil")
	}
	blocked := registeredExactWireBlockedHeaders(inbound)
	for key, field := range inbound.Fields {
		name := strings.TrimSpace(field.CanonicalName)
		if name == "" {
			name = http.CanonicalHeaderKey(key)
		}
		normalized := strings.ToLower(strings.TrimSpace(name))
		if _, denied := blocked[normalized]; denied || registeredExactWireHeaderHasDeniedPrefix(normalized) {
			continue
		}
		if _, authoritative := dst[http.CanonicalHeaderKey(name)]; authoritative {
			continue
		}
		for _, value := range field.Values {
			if strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf("%s contains an invalid value", name)
			}
			dst.Add(name, value)
		}
	}
	return nil
}

func registeredExactWireHeaderHasDeniedPrefix(normalized string) bool {
	for _, prefix := range registeredExactWireDeniedRequestHeaderPrefixes {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

func registeredExactWireBlockedHeaders(inbound HeaderSnapshot) map[string]struct{} {
	blocked := make(map[string]struct{}, len(registeredExactWireOwnedRequestHeaders))
	for name := range registeredExactWireOwnedRequestHeaders {
		blocked[name] = struct{}{}
	}
	configuredExactWireOwnedRequestHeaders.RLock()
	for name := range configuredExactWireOwnedRequestHeaders.names {
		blocked[name] = struct{}{}
	}
	configuredExactWireOwnedRequestHeaders.RUnlock()
	// RFC 9110 permits Connection to nominate additional hop-by-hop fields.
	for _, value := range inbound.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.ToLower(strings.TrimSpace(token)); token != "" {
				blocked[token] = struct{}{}
			}
		}
	}
	return blocked
}
