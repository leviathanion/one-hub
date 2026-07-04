package codexpolicy

import "strings"

const (
	KeyFedRAMP                     = "fedramp"
	KeyResidency                   = "residency"
	KeyDefaultOriginator           = "default_originator"
	KeyTrustClientAttestation      = "trust_client_attestation"
	KeyGenerateProxyInstallationID = "generate_proxy_installation_id"
)

var knownKeys = map[string]struct{}{
	KeyFedRAMP:                     {},
	KeyResidency:                   {},
	KeyDefaultOriginator:           {},
	KeyTrustClientAttestation:      {},
	KeyGenerateProxyInstallationID: {},
}

func KnownKey(key string) bool {
	_, ok := knownKeys[key]
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
