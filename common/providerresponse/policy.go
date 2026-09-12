package providerresponse

import (
	"net/http"
	"one-api/common"
	"strings"
)

// Operation identifies the public operation whose provider response is being
// delivered. Header exposure is an HTTP boundary policy and is intentionally
// independent from provider-specific adapters.
type Operation string

const (
	OperationUnknown              Operation = ""
	OperationRawRelay             Operation = "raw.relay"
	OperationBinaryDownload       Operation = "binary.download"
	OperationResponsesCreate      Operation = "responses.create"
	OperationResponsesCompact     Operation = "responses.compact"
	OperationResponsesInputTokens Operation = "responses.input_tokens"
	OperationResponsesRetrieve    Operation = "responses.retrieve"
	OperationResponsesDelete      Operation = "responses.delete"
	OperationResponsesInputItems  Operation = "responses.input_items"
	OperationResponsesWebSocket   Operation = "responses.websocket"
	OperationAudioTranscription   Operation = "audio.transcription"
	OperationAudioTranslation     Operation = "audio.translation"
)

type DataPath string

const (
	DataPathExactWire     DataPath = "exact_wire"
	DataPathSameDialect   DataPath = "same_dialect"
	DataPathCrossProtocol DataPath = "cross_protocol"
)

type Policy struct {
	Operation        Operation
	DataPath         DataPath
	BodyUnmodified   bool
	PreserveRedirect bool
}

var baseHeaders = map[string]struct{}{
	"Apim-Request-Id":      {},
	"Openai-Processing-Ms": {},
	"Openai-Version":       {},
	"Retry-After":          {},
	"X-Request-Id":         {},
}

var conditionalReadHeaders = map[string]struct{}{
	"Etag":          {},
	"Last-Modified": {},
}

var representationHeaders = map[string]struct{}{
	"Cache-Control":    {},
	"Content-Language": {},
	"Content-Length":   {},
	"Digest":           {},
	"Expires":          {},
	"Pragma":           {},
	"Vary":             {},
}

var cacheReadHeaders = map[string]struct{}{
	"Age": {},
}

var downloadHeaders = map[string]struct{}{
	"Accept-Ranges":       {},
	"Content-Disposition": {},
	"Content-Range":       {},
}

// Filter applies a deny-by-default provider response header policy. DataPath is
// deliberately not used to broaden the allowlist: exact-wire preserves the
// provider status/body protocol, not provider account or transport metadata.
func Filter(source http.Header, policy Policy) http.Header {
	result := make(http.Header)
	for name, values := range source {
		canonical := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if !allowed(canonical, policy) {
			continue
		}
		result[canonical] = append([]string(nil), values...)
	}
	return result
}

func allowed(header string, policy Policy) bool {
	if _, ok := baseHeaders[header]; ok {
		return true
	}
	if !policy.BodyUnmodified {
		return false
	}
	if header == "Content-Type" || header == "Content-Encoding" {
		return true
	}
	if _, ok := representationHeaders[header]; ok {
		return true
	}
	if policy.PreserveRedirect && header == "Location" {
		return true
	}
	if operationSupportsConditionalRead(policy.Operation) {
		if _, ok := conditionalReadHeaders[header]; ok {
			return true
		}
		if _, ok := cacheReadHeaders[header]; ok {
			return true
		}
	}
	if operationSupportsDownload(policy.Operation) {
		if _, ok := downloadHeaders[header]; ok {
			return true
		}
	}
	return false
}

func operationSupportsConditionalRead(operation Operation) bool {
	switch operation {
	case OperationRawRelay, OperationBinaryDownload, OperationResponsesRetrieve, OperationResponsesInputItems:
		return true
	default:
		return false
	}
}

func operationSupportsDownload(operation Operation) bool {
	switch operation {
	case OperationRawRelay, OperationBinaryDownload:
		return true
	default:
		return false
	}
}

// FilterCredentialHeaders 只移除泄漏实际凭据的头部，不扩大已授权头部集合。
func FilterCredentialHeaders(source http.Header, credentials ...string) http.Header {
	var safe http.Header
	for name, values := range source {
		for _, value := range values {
			if _, secret := common.RedactCredentialValuesText(value, credentials...); secret {
				if safe == nil {
					safe = source.Clone()
				}
				safe.Del(name)
				break
			}
		}
	}
	if safe != nil {
		return safe
	}
	return source
}
