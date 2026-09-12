package common

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	sensitiveOpenAIKeyPattern           = regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{8,}\b`)
	sensitiveAuthorizationHeaderPattern = regexp.MustCompile(`(?im)(authorization[ \t]*:[ \t]*)[^\r\n]*(?:\r?\n[ \t]+[^\r\n]*)*`)
	sensitiveAuthorizationValuePattern  = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)[^\r\n,;&<>"']+`)
	sensitiveFieldValuePattern          = regexp.MustCompile(`(?i)([a-z][a-z0-9_-]*\s*[:=]\s*)[^,;&\s<>"']+`)
	sensitiveBearerPattern              = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]+=*`)
	sensitiveJWTLikePattern             = regexp.MustCompile(`\b[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	sensitiveProviderIdentityPattern    = regexp.MustCompile(`(?i)\b(organization|org|project|account)([ \t]+)(?:org[-_]|proj[-_]|acct[-_])[A-Za-z0-9_-]+`)
	sensitiveURLPattern                 = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)
	sensitiveBodyLabelValuePattern      = regexp.MustCompile(`(?i)\b(request|response)[-_ ]?body\b\s*[:=]?\s*\S*`)
)

// RedactSensitiveText 处理非结构化诊断，不能应用于 JSON 协议字段。
func RedactSensitiveText(message string) string {
	if message == "" {
		return ""
	}
	message = sensitiveOpenAIKeyPattern.ReplaceAllString(message, "[redacted]")
	message = RedactSensitiveAssignments(message)
	message = sensitiveBearerPattern.ReplaceAllString(message, "[redacted]")
	message = sensitiveJWTLikePattern.ReplaceAllString(message, "[redacted]")
	message = sensitiveProviderIdentityPattern.ReplaceAllString(message, "${1}${2}[redacted]")
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

// RedactCredentialValuesText 匹配已经解码的文本；JSON/SSE 必须先经过协议边界。
func RedactCredentialValuesText(message string, values ...string) (string, bool) {
	var secrets []string
	for _, value := range values {
		if value != "" && strings.Contains(message, value) {
			secrets = append(secrets, value)
		}
	}
	if len(secrets) == 0 {
		return message, false
	}
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	marker := CredentialRedactionMarker(values...)
	pairs := []string{}
	if marker != "" {
		pairs = append(pairs, marker, marker)
	}
	for _, secret := range secrets {
		pairs = append(pairs, secret, marker)
	}
	safe := strings.NewReplacer(pairs...).Replace(message)
	// 替换可能与前后文本重新拼成秘密；仅用于诊断值的有界兜底。
	for _, value := range values {
		if value != "" && strings.Contains(safe, value) {
			return marker, marker != message
		}
	}
	return safe, safe != message
}

// RedactProviderErrorText 保留错误消息的空白和正常协议词，只遮盖明确敏感内容。
func RedactProviderErrorText(message string) string {
	message = sensitiveOpenAIKeyPattern.ReplaceAllString(message, "[redacted]")
	message = RedactSensitiveAssignments(message)
	message = sensitiveBearerPattern.ReplaceAllString(message, "[redacted]")
	message = sensitiveJWTLikePattern.ReplaceAllString(message, "[redacted]")
	return sensitiveProviderIdentityPattern.ReplaceAllString(message, "${1}${2}[redacted]")
}

// SensitiveCredentialLabel 是协议字段与诊断赋值共用的凭据标签词表。
func SensitiveCredentialLabel(label string) bool {
	switch normalizedSensitiveLabel(label) {
	case "apikey", "xapikey", "token", "accesstoken", "refreshtoken", "idtoken", "clientsecret", "clientassertion", "accountid", "organizationid", "orgid", "projectid", "subscriptionid", "tenantid", "billingaccount", "billingaccountid":
		return true
	}
	return false
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
	return sensitiveFieldValuePattern.ReplaceAllStringFunc(message, func(assignment string) string {
		index := strings.IndexAny(assignment, ":=")
		if index < 0 {
			return assignment
		}
		label := strings.TrimSpace(assignment[:index])
		if !SensitiveCredentialLabel(label) {
			return assignment
		}
		start := index + 1
		for start < len(assignment) && isJSONWhitespace(assignment[start]) {
			start++
		}
		return assignment[:start] + "[redacted]"
	})
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
		sensitiveSessionLabel(lower) ||
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
		sensitiveSessionLabel(lower) ||
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

func sensitiveSessionLabel(lower string) bool {
	if delimiter := strings.IndexAny(lower, "=:"); delimiter >= 0 {
		lower = lower[:delimiter]
	}
	switch normalizedSensitiveLabel(lower) {
	case "session", "sessionid", "sessionkey", "sessiontoken":
		return true
	default:
		return false
	}
}

func sensitiveCredentialLabel(label string) bool {
	if delimiter := strings.IndexAny(label, "=:"); delimiter >= 0 {
		label = label[:delimiter]
	}
	lower := strings.ToLower(label)
	return SensitiveCredentialLabel(label) || strings.Contains(lower, "api-key") || strings.Contains(lower, "access-token")
}

func normalizedSensitiveLabel(label string) string {
	return strings.Map(func(r rune) rune {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return -1
		}
		return r
	}, strings.ToLower(label))
}

// SafeClientErrorText 只用于对客的非 JSON 错误与关闭原因；截断前完成脱敏。
func SafeClientErrorText(message string, credentials ...string) string {
	message, _ = RedactCredentialValuesText(message, credentials...)
	message = RedactSensitiveText(message)
	// 启发式规则可能生成与短凭据重叠的占位符。
	message, _ = RedactCredentialValuesText(message, credentials...)
	const maxBytes = 4096
	if len(message) > maxBytes {
		message = message[:maxBytes]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
		message += " [truncated]"
	}
	return message
}

// CredentialRedactionMarker 防止短凭据与占位符重叠，必要时使用空字符串。
func CredentialRedactionMarker(credentials ...string) string {
	for _, value := range credentials {
		if value != "" && strings.Contains("[redacted]", value) {
			return ""
		}
	}
	return "[redacted]"
}
