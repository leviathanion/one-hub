package common

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	sensitiveOpenAIKeyPattern           = regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{8,}\b`)
	sensitiveFieldNames                 = `access[_-]?token|refresh[_-]?token|id[_-]?token|authorization|client[_-]?secret|client[_-]?assertion|api[_-]?key|x[_-]?api[_-]?key|token`
	sensitiveAuthorizationHeaderPattern = regexp.MustCompile(`(?im)(authorization[ \t]*:[ \t]*)[^\r\n]*(?:\r?\n[ \t]+[^\r\n]*)*`)
	sensitiveAuthorizationValuePattern  = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)[^\r\n,;&<>"']+`)
	sensitiveFieldValuePattern          = regexp.MustCompile(`(?i)((?:` + sensitiveFieldNames + `)\s*[:=]\s*)[^,;&\s<>"']+`)
	sensitiveBearerPattern              = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]+=*`)
	sensitiveJWTLikePattern             = regexp.MustCompile(`\b[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	sensitiveURLPattern                 = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)
	sensitiveBodyLabelValuePattern      = regexp.MustCompile(`(?i)\b(request|response)[-_ ]?body\b\s*[:=]?\s*\S*`)
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
	message = RedactSensitiveAssignments(message)
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

// RedactSensitiveAssignments redacts credential assignments while preserving
// unrelated diagnostic text. Providers that need a narrower policy can reuse
// this same escape-aware assignment boundary without duplicating it.
func RedactSensitiveAssignments(message string) string {
	message = redactSensitiveQuotedAssignments(message)
	// RFC 7230 obs-fold is obsolete but still appears in upstream diagnostics.
	// A continuation belongs to the Authorization field value, so consume every
	// whitespace-prefixed continuation line rather than exposing its token as an
	// apparently unrelated line.
	message = sensitiveAuthorizationHeaderPattern.ReplaceAllString(message, "${1}[redacted]")
	message = sensitiveAuthorizationValuePattern.ReplaceAllString(message, "${1}[redacted]")
	return sensitiveFieldValuePattern.ReplaceAllString(message, "${1}[redacted]")
}

// redactSensitiveQuotedAssignments scans quoted values rather than matching them
// with a quoted-value regexp. In particular, a quote preceded by an odd number
// of escapes is content, not the end of a JSON string. Escaped JSON embedded in
// diagnostic text uses one additional quoting layer; quoteLayer recognizes that
// representation as well. An unterminated sensitive value consumes the rest of
// the message, because retaining an uncertain suffix risks leaking credentials.
func redactSensitiveQuotedAssignments(message string) string {
	var out strings.Builder
	written := 0
	for i := 0; i < len(message); i++ {
		valueStart, valueEnd, ok := sensitiveQuotedAssignmentAt(message, i)
		if !ok {
			continue
		}
		out.WriteString(message[written:valueStart])
		out.WriteString("[redacted]")
		if valueEnd < 0 {
			return out.String()
		}
		written = valueEnd
		i = valueEnd - 1
	}
	if written == 0 {
		return message
	}
	out.WriteString(message[written:])
	return out.String()
}

func sensitiveQuotedAssignmentAt(message string, start int) (valueStart, valueEnd int, ok bool) {
	if start > 0 && isSensitiveKeyByte(message[start-1]) {
		return 0, 0, false
	}

	pos := start
	key := ""
	keyLayer := 0
	if message[pos] == '\'' || message[pos] == '"' {
		keyLayer = quoteLayer(message, pos)
		end := findClosingQuote(message, pos+1, message[pos], keyLayer)
		if end < 0 {
			return 0, 0, false
		}
		rawKey := message[pos+1 : end]
		// Escaped embedded JSON prefixes its closing structural quote with the
		// layer escape sequence; that prefix is not part of the key itself.
		closingPrefix := (1 << keyLayer) - 1
		if closingPrefix > 0 && len(rawKey) >= closingPrefix {
			rawKey = rawKey[:len(rawKey)-closingPrefix]
		}
		var decoded bool
		key, decoded = decodeQuotedAssignmentKey(rawKey, keyLayer)
		// A malformed escaped key cannot be classified safely. Treat it as
		// sensitive: preserving its value would turn parser ambiguity into a
		// credential disclosure.
		if !decoded {
			key = "access_token"
		}
		pos = end + 1
	} else {
		for pos < len(message) && isSensitiveKeyByte(message[pos]) {
			pos++
		}
		if pos == start {
			return 0, 0, false
		}
		key = message[start:pos]
	}
	if !sensitiveCredentialLabel(key) && !strings.EqualFold(key, "authorization") {
		return 0, 0, false
	}
	for pos < len(message) && isJSONWhitespace(message[pos]) {
		pos++
	}
	if pos >= len(message) || (message[pos] != ':' && message[pos] != '=') {
		return 0, 0, false
	}
	pos++
	for pos < len(message) && isJSONWhitespace(message[pos]) {
		pos++
	}
	// Embedded JSON writes each structural quote with the layer's escape prefix.
	for pos < len(message) && message[pos] == '\\' {
		pos++
	}
	if pos >= len(message) || (message[pos] != '\'' && message[pos] != '"') {
		return 0, 0, false
	}
	layer := quoteLayer(message, pos)
	// A quoted key and value in escaped, embedded JSON must use the same layer.
	if start < len(message) && (message[start] == '\'' || message[start] == '"') && layer != keyLayer {
		return 0, 0, false
	}
	end := findClosingQuote(message, pos+1, message[pos], layer)
	if end < 0 {
		return pos + 1, -1, true
	}
	return pos + 1, end, true
}

func decodeQuotedAssignmentKey(raw string, layer int) (string, bool) {
	decoded := raw
	// Decode the structural quoting layer plus one possible JSON string carried
	// inside diagnostic text. The latter is why a key rendered as
	// "access\\u005ftoken" must still classify as "access_token".
	for i := 0; i <= layer+1; i++ {
		value, err := strconv.Unquote(`"` + decoded + `"`)
		if err != nil {
			return "", false
		}
		decoded = value
	}
	return decoded, true
}

func isSensitiveKeyByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

func quoteLayer(message string, quote int) int {
	slashes := 0
	for i := quote - 1; i >= 0 && message[i] == '\\'; i-- {
		slashes++
	}
	layer := 0
	for slashes&1 == 1 {
		layer++
		slashes >>= 1
	}
	return layer
}

func findClosingQuote(message string, start int, quote byte, layer int) int {
	mask := 1 << (layer + 1)
	want := (1 << layer) - 1
	for i := start; i < len(message); i++ {
		if message[i] != quote {
			continue
		}
		slashes := 0
		for j := i - 1; j >= 0 && message[j] == '\\'; j-- {
			slashes++
		}
		if slashes%mask == want {
			return i
		}
	}
	return -1
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
	if delimiter := strings.IndexAny(lower, "=:"); delimiter >= 0 {
		lower = lower[:delimiter]
	}
	lower = strings.ToLower(lower)
	normalized := strings.Map(func(r rune) rune {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return -1
		}
		return r
	}, lower)
	switch normalized {
	case "apikey", "xapikey", "token", "accesstoken", "refreshtoken", "idtoken", "clientsecret", "clientassertion":
		return true
	default:
		// Preserve the legacy safe-superset behavior for provider-prefixed
		// labels such as "openai-api-key" and "codex-access-token". Restrict
		// this fallback to the two original hyphenated fragments so structured
		// error codes such as "invalid_api_key" remain useful diagnostics.
		return strings.Contains(lower, "api-key") || strings.Contains(lower, "access-token")
	}
}
