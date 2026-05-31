package codex

import "strings"

// isCodexOfficialClientRequest checks if the User-Agent indicates an official Codex client.
func isCodexOfficialClientRequest(userAgent string) bool {
	return isCodexOfficialClientLikeHeader(userAgent)
}

// isCodexOfficialClientOriginator checks if originator indicates an official Codex client.
func isCodexOfficialClientOriginator(originator string) bool {
	return isCodexOfficialClientLikeHeader(originator)
}

// isCodexOfficialClientByHeaders checks whether the request headers indicate an
// official Codex client family request (either by User-Agent or originator).
func isCodexOfficialClientByHeaders(userAgent, originator string) bool {
	return isCodexOfficialClientRequest(userAgent) || isCodexOfficialClientOriginator(originator)
}

func normalizeCodexClientHeader(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// isCodexOfficialClientLikeHeader intentionally uses a broad case-insensitive
// "codex" prefix instead of a finite client-name list. Trade-off: a custom
// non-official client can opt into Codex-like handling by using a codex*
// prefix, but official clients are less likely to be missed as names evolve.
func isCodexOfficialClientLikeHeader(value string) bool {
	return strings.HasPrefix(normalizeCodexClientHeader(value), "codex")
}

// resolveSmartOriginatorForEffectiveUserAgent decides the fallback originator
// from the User-Agent that will actually be sent upstream. Callers must apply
// client passthrough, channel overrides, and defaults before calling it.
func resolveSmartOriginatorForEffectiveUserAgent(userAgent string) string {
	if strings.TrimSpace(userAgent) == "" {
		return defaultNonOfficialCodexOriginator
	}
	if isCodexOfficialClientRequest(userAgent) {
		return defaultOfficialCodexOriginator
	}
	return defaultNonOfficialCodexOriginator
}
