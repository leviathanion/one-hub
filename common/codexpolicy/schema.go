package codexpolicy

import "strings"

const (
	KeyFedRAMP                = "fedramp"
	KeyResidency              = "residency"
	KeyDefaultOriginator      = "default_originator"
	KeyTrustClientAttestation = "trust_client_attestation"
	KeyAutoGenerate           = "auto_generate"
)

const (
	AutoGenerateSessionID              = "session_id"
	AutoGenerateThreadID               = "thread_id"
	AutoGenerateClientRequestID        = "client_request_id"
	AutoGenerateInstallationID         = "installation_id"
	AutoGenerateWSStreamRequestStartMS = "ws_stream_request_start_ms"
)

var knownKeys = map[string]struct{}{
	KeyFedRAMP:                {},
	KeyResidency:              {},
	KeyDefaultOriginator:      {},
	KeyTrustClientAttestation: {},
	KeyAutoGenerate:           {},
}

var knownAutoGenerateKeys = map[string]struct{}{
	AutoGenerateSessionID:              {},
	AutoGenerateThreadID:               {},
	AutoGenerateClientRequestID:        {},
	AutoGenerateInstallationID:         {},
	AutoGenerateWSStreamRequestStartMS: {},
}

func KnownKey(key string) bool {
	_, ok := knownKeys[key]
	return ok
}

func KnownAutoGenerateKey(key string) bool {
	_, ok := knownAutoGenerateKeys[key]
	return ok
}

func ValidResidency(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == ':' || c == '-' {
			continue
		}
		return false
	}
	return true
}
