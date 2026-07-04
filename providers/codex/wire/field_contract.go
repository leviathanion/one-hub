package wire

import "strings"

type operationMask uint8

const (
	outputHTTPCreate operationMask = 1 << iota
	outputHTTPCompact
	outputWSHandshake
)

type identityField int

const (
	identityNone identityField = iota
	identityWindowID
	identityInstallationID
	identityTurnMetadata
	identityParentThreadID
	identitySubagent
	identityMemgenRequest
	identityTurnState
	identityResponsesLite
	identityInferenceCallID
)

type fieldSpec struct {
	header           string
	metadata         string
	valid            func(string) bool
	singleton        bool
	validateMetadata bool
	output           operationMask
	identity         identityField
	clientHeader     bool
}

var fieldSpecs = []fieldSpec{
	{header: "User-Agent", valid: validUserAgent, singleton: true},
	{header: "originator", valid: validOriginator, singleton: true},
	{header: "session-id", metadata: "session_id", valid: validID, singleton: true, validateMetadata: true},
	{header: "thread-id", metadata: "thread_id", valid: validID, singleton: true, validateMetadata: true},
	{header: "x-client-request-id", valid: validID, singleton: true},
	{header: "x-codex-window-id", metadata: "x-codex-window-id", valid: validID, singleton: true, validateMetadata: true, output: outputHTTPCreate | outputHTTPCompact | outputWSHandshake, identity: identityWindowID},
	{header: "x-codex-turn-metadata", metadata: "x-codex-turn-metadata", valid: validTurnMetadata, singleton: true, validateMetadata: true, output: outputHTTPCreate | outputHTTPCompact | outputWSHandshake, identity: identityTurnMetadata},
	{header: "x-codex-parent-thread-id", metadata: "x-codex-parent-thread-id", valid: validID, singleton: true, validateMetadata: true, output: outputHTTPCreate | outputHTTPCompact | outputWSHandshake, identity: identityParentThreadID},
	{header: "x-openai-subagent", metadata: "x-openai-subagent", valid: validSubagent, singleton: true, validateMetadata: true, output: outputHTTPCreate | outputHTTPCompact | outputWSHandshake, identity: identitySubagent},
	{header: "x-openai-memgen-request", valid: validBoolString, singleton: true, output: outputHTTPCreate | outputHTTPCompact | outputWSHandshake, identity: identityMemgenRequest},
	{header: "x-codex-beta-features", valid: validBetaFeatures, singleton: true, output: outputHTTPCreate | outputHTTPCompact | outputWSHandshake, clientHeader: true},
	{header: "x-responsesapi-include-timing-metrics", valid: validBoolString, singleton: true, output: outputHTTPCreate | outputHTTPCompact | outputWSHandshake, clientHeader: true},
	{header: "x-codex-turn-state", metadata: "x-codex-turn-state", valid: validTurnState, singleton: true, validateMetadata: true, output: outputHTTPCreate | outputHTTPCompact, identity: identityTurnState},
	{header: "x-openai-internal-codex-responses-lite", valid: validBoolString, singleton: true, output: outputHTTPCreate | outputHTTPCompact, identity: identityResponsesLite},
	{header: "x-oai-attestation", valid: validAttestation, singleton: true, output: outputHTTPCreate | outputHTTPCompact | outputWSHandshake, clientHeader: true},
	{header: "x-codex-inference-call-id", valid: validID, singleton: true, output: outputHTTPCreate | outputHTTPCompact, identity: identityInferenceCallID},
	{header: "x-codex-installation-id", metadata: "x-codex-installation-id", valid: validInstallationID, singleton: true, validateMetadata: true, output: outputHTTPCreate | outputHTTPCompact, identity: identityInstallationID},
	{header: "traceparent", valid: validTraceparent, singleton: true, output: outputHTTPCreate | outputHTTPCompact, clientHeader: true},
	{header: "tracestate", valid: validTracestate, singleton: true, output: outputHTTPCreate | outputHTTPCompact, clientHeader: true},
	{metadata: "ws_request_header_traceparent", valid: validTraceparent, validateMetadata: true},
	{metadata: "ws_request_header_tracestate", valid: validTracestate, validateMetadata: true},
	{metadata: "x-codex-ws-stream-request-start-ms", valid: validUnixMillisString, validateMetadata: true},
}

func fieldSpecForHeader(name string) (fieldSpec, bool) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	for _, spec := range fieldSpecs {
		if spec.header == "" {
			continue
		}
		if strings.ToLower(spec.header) == normalized {
			return spec, true
		}
	}
	return fieldSpec{}, false
}

func validateOriginatorValue(value string) error {
	if !validOriginator(strings.TrimSpace(value)) {
		return reject("originator", "value is invalid")
	}
	return nil
}

func ValidateOriginator(value string) error {
	return validateOriginatorValue(value)
}
