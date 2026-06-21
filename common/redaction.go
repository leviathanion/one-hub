package common

import (
	"regexp"
	"strings"
)

var (
	sensitiveOpenAIKeyPattern      = regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{8,}\b`)
	sensitiveQuerySecretPattern    = regexp.MustCompile(`(?i)\b(api_key|token|access_token)=([^&\s]+)`)
	sensitiveBearerPattern         = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]+=*`)
	sensitiveJWTLikePattern        = regexp.MustCompile(`\b[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	sensitiveURLPattern            = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)
	sensitiveBodyLabelValuePattern = regexp.MustCompile(`(?i)\b(request|response)[-_ ]?body\b\s*[:=]?\s*\S*`)
)

// RedactSensitiveText is intentionally a safe superset shared by relay
// diagnostics and ResponsesWS provider errors. The trade-off is occasional
// over-redaction of diagnostic labels in exchange for one boundary rule that
// does not drift for credentials, URLs, headers, sessions, or raw bodies.
func RedactSensitiveText(message string) string {
	if message == "" {
		return ""
	}
	message = sensitiveOpenAIKeyPattern.ReplaceAllString(message, "[redacted]")
	message = sensitiveQuerySecretPattern.ReplaceAllString(message, "${1}=[redacted]")
	message = sensitiveBearerPattern.ReplaceAllString(message, "[redacted]")
	message = sensitiveJWTLikePattern.ReplaceAllString(message, "[redacted]")
	message = sensitiveURLPattern.ReplaceAllString(message, "[redacted]")
	message = sensitiveBodyLabelValuePattern.ReplaceAllString(message, "[redacted]")

	fields := strings.Fields(message)
	if len(fields) == 0 {
		return ""
	}
	redactNext := false
	for i, field := range fields {
		lower := strings.ToLower(strings.Trim(field, `"'{}[](),;`))
		redactCurrent := redactNext || sensitiveDiagnosticField(lower)
		if redactCurrent {
			fields[i] = "[redacted]"
		}
		redactNext = sensitiveDiagnosticFieldRequiresValue(lower)
	}
	return strings.Join(fields, " ")
}

func sensitiveDiagnosticField(lower string) bool {
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		sensitiveCredentialLabel(lower) ||
		strings.Contains(lower, "authorization") ||
		strings.Contains(lower, "bearer") ||
		strings.Contains(lower, "session") ||
		strings.Contains(lower, "header") ||
		strings.Contains(lower, "request-body") ||
		strings.Contains(lower, "request_body") ||
		strings.Contains(lower, "requestbody") ||
		strings.Contains(lower, "response-body") ||
		strings.Contains(lower, "response_body") ||
		strings.Contains(lower, "responsebody") ||
		strings.Contains(lower, "upstream-url") ||
		strings.Contains(lower, "upstream_url")
}

func sensitiveDiagnosticFieldRequiresValue(lower string) bool {
	return strings.Contains(lower, "authorization") ||
		strings.Contains(lower, "bearer") ||
		sensitiveCredentialLabel(lower) ||
		strings.Contains(lower, "session") ||
		strings.Contains(lower, "header") ||
		strings.Contains(lower, "request-body") ||
		strings.Contains(lower, "request_body") ||
		strings.Contains(lower, "requestbody") ||
		strings.Contains(lower, "response-body") ||
		strings.Contains(lower, "response_body") ||
		strings.Contains(lower, "responsebody") ||
		strings.Contains(lower, "upstream-url") ||
		strings.Contains(lower, "upstream_url")
}

func sensitiveCredentialLabel(lower string) bool {
	return lower == "api_key" ||
		lower == "api-key" ||
		lower == "token" ||
		lower == "access_token" ||
		lower == "access-token" ||
		strings.HasPrefix(lower, "api_key=") ||
		strings.HasPrefix(lower, "api_key:") ||
		strings.HasPrefix(lower, "api-key=") ||
		strings.HasPrefix(lower, "api-key:") ||
		strings.HasPrefix(lower, "token=") ||
		strings.HasPrefix(lower, "token:") ||
		strings.HasPrefix(lower, "access_token=") ||
		strings.HasPrefix(lower, "access_token:") ||
		strings.HasPrefix(lower, "access-token=") ||
		strings.HasPrefix(lower, "access-token:") ||
		strings.Contains(lower, "api-key") ||
		strings.Contains(lower, "access-token")
}
