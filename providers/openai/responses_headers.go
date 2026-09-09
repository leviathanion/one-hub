package openai

import (
	"fmt"
	"net/http"
	"strings"

	"one-api/common/providerresponse"
	"one-api/common/requestctx"
)

const openAISafetyIdentifierHeader = "OpenAI-Safety-Identifier"

var forwardedOpenAIBusinessHeaders = []string{
	"Idempotency-Key",
	"OpenAI-Beta",
	openAISafetyIdentifierHeader,
}

func openAIBusinessHeaderValues(inbound requestctx.HeaderSnapshot, names []string) (map[string]string, error) {
	valuesByName := make(map[string]string, len(names))
	for _, name := range names {
		values := inbound.Values(name)
		if len(values) == 0 {
			continue
		}
		cleaned := make([]string, 0, len(values))
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if strings.ContainsAny(value, "\r\n") {
				return nil, fmt.Errorf("%s contains an invalid value", name)
			}
			cleaned = append(cleaned, value)
		}
		if len(cleaned) == 0 {
			continue
		}
		if name == openAISafetyIdentifierHeader && len(cleaned) != 1 {
			return nil, fmt.Errorf("%s must have exactly one value", name)
		}
		valuesByName[name] = strings.Join(cleaned, ", ")
	}
	return valuesByName, nil
}

func applyResponsesBusinessHeaders(headers map[string]string, inbound requestctx.HeaderSnapshot) error {
	return applyOpenAIBusinessHeaders(headers, inbound, forwardedOpenAIBusinessHeaders)
}

func applyRealtimeBusinessHeaders(headers map[string]string, inbound requestctx.HeaderSnapshot) error {
	return applyOpenAIBusinessHeaders(headers, inbound, []string{openAISafetyIdentifierHeader})
}

func applyOpenAIBusinessHeaders(headers map[string]string, inbound requestctx.HeaderSnapshot, names []string) error {
	valuesByName, err := openAIBusinessHeaderValues(inbound, names)
	if err != nil {
		return err
	}
	for name, value := range valuesByName {
		if !headerMapContainsFold(headers, name) {
			headers[name] = value
		}
	}
	return nil
}

func applyResponsesBusinessHTTPHeaders(headers http.Header, inbound requestctx.HeaderSnapshot) error {
	valuesByName, err := openAIBusinessHeaderValues(inbound, forwardedOpenAIBusinessHeaders)
	if err != nil {
		return err
	}
	for name, value := range valuesByName {
		if len(headers.Values(name)) == 0 {
			headers.Set(name, value)
		}
	}
	return nil
}

func applyResponsesConditionalReadHeaders(headers http.Header, inbound requestctx.HeaderSnapshot) error {
	for _, name := range []string{"If-None-Match", "If-Modified-Since"} {
		if len(headers.Values(name)) > 0 {
			continue
		}
		for _, value := range inbound.Values(name) {
			if strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf("%s contains an invalid value", name)
			}
			headers.Add(name, value)
		}
	}
	return nil
}

func (p *OpenAIProvider) applyOpenAIHTTPHeaders(headers http.Header, inbound requestctx.HeaderSnapshot) error {
	// Only an OpenAI channel is registered as exact-wire. Azure, custom and
	// OpenAI-compatible adapters may forward the small business-header surface,
	// but must not inherit ingress proxy identity or transport headers.
	if p == nil || !p.ProviderRawJSONReplay {
		return applyResponsesBusinessHTTPHeaders(headers, inbound)
	}
	// CommonRequestHeaders initially mirrors these two client fields into a
	// single-value map. Restore their exact multi-value wire form unless an
	// administrator explicitly owns the field through model_headers.
	modelHeaders := map[string]string(nil)
	if p != nil && p.Channel != nil {
		modelHeaders, _ = p.Channel.GetModelHeadersMap()
	}
	for _, name := range []string{"Accept", "Content-Type"} {
		if len(inbound.Values(name)) > 0 && !headerMapContainsFold(modelHeaders, name) {
			headers.Del(name)
		}
	}
	return requestctx.ApplyRegisteredExactWireRequestHeaders(headers, inbound)
}

func headerMapContainsFold(headers map[string]string, name string) bool {
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return true
		}
	}
	return false
}

func (p *OpenAIProvider) captureProviderResponseHeaders(response *http.Response, bodyUnmodified ...bool) {
	if p == nil || p.Context == nil || response == nil {
		return
	}
	// The caller decides whether the final delivered body is byte-identical to
	// the provider body. This cannot be inferred from the provider mode: native
	// same-dialect adapters may safely replay their raw body, while an exact-wire
	// response may still be rewritten for credential or metadata safety.
	unmodified := len(bodyUnmodified) > 0 && bodyUnmodified[0]
	dataPath := providerresponse.DataPathSameDialect
	if p.ProviderRawJSONReplay {
		dataPath = providerresponse.DataPathExactWire
	}
	p.Context.Set(requestctx.ProviderResponseHeadersContextKey, providerresponse.Filter(response.Header, providerresponse.Policy{
		DataPath:       dataPath,
		BodyUnmodified: unmodified,
	}))
	p.Context.Set(requestctx.ProviderResponseStatusContextKey, response.StatusCode)
}
