package providerresponse

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"io"
	"strings"

	"one-api/common"
)

// 预算只限制诊断副本的处理成本；超限或解析失败时返回原文。
const maxErrorRedactionBytes = 1 << 20
const maxErrorRedactionDepth = 64

type errorJSONField struct {
	name       string
	start, end int
}
type errorJSONEdit struct {
	start, end int
	value      []byte
}

// SanitizeErrorPayload 只修改已识别错误的诊断对象，保留正文和协议字段。
// 失败没有控制含义：返回原文，不改变状态、重试、计费或交付。
func SanitizeErrorPayload(raw []byte, credentials ...string) ([]byte, bool) {
	return sanitizeErrorEnvelope(raw, false, credentials)
}

// SanitizeErrorResponse 用于已由 HTTP 状态或调用方确认的纯错误响应。
func SanitizeErrorResponse(raw []byte, credentials ...string) ([]byte, bool) {
	return sanitizeErrorEnvelope(raw, true, credentials)
}

func sanitizeErrorEnvelope(raw []byte, knownError bool, credentials []string) ([]byte, bool) {
	if len(raw) > maxErrorRedactionBytes {
		return raw, false
	}
	fields, ok := errorJSONFields(raw)
	if !ok {
		return raw, false
	}
	var edits []errorJSONEdit
	var eventType string
	wrapped := false
	for _, field := range fields {
		value := raw[field.start:field.end]
		switch field.name {
		case "type":
			_ = json.Unmarshal(value, &eventType)
		case "error":
			wrapped = true
			// 字符串形式的 error 可能是 OAuth 错误码，保留其协议值。
			if value[0] == '{' || value[0] == '[' {
				if safe, changed := sanitizeErrorObject(value, credentials); changed {
					edits = append(edits, errorJSONEdit{field.start, field.end, safe})
				}
			}
		case "response":
			wrapped = true
			children, valid := errorJSONFields(value)
			if !valid {
				continue
			}
			for _, child := range children {
				if strings.EqualFold(child.name, "error") {
					if safe, changed := sanitizeErrorObject(value[child.start:child.end], credentials); changed {
						edits = append(edits, errorJSONEdit{field.start + child.start, field.start + child.end, safe})
					}
				}
			}
		}
	}
	if !wrapped && (knownError || strings.EqualFold(strings.TrimSpace(eventType), "error")) {
		return sanitizeErrorObject(raw, credentials)
	}
	return applyErrorJSONEdits(raw, edits)
}

// 只定位 envelope 成员，未知值由标准库跳过，保留全部原始字节。
func errorJSONFields(raw []byte) ([]errorJSONField, bool) {
	decoder := jsontext.NewDecoder(bytes.NewBuffer(raw))
	token, err := decoder.ReadToken()
	if err != nil || token.Kind() != '{' {
		return nil, false
	}
	var fields []errorJSONField
	for decoder.PeekKind() != '}' {
		key, err := decoder.ReadToken()
		if err != nil {
			return nil, false
		}
		name := key.String()
		value, err := decoder.ReadValue()
		if err != nil {
			return nil, false
		}
		end := int(decoder.InputOffset())
		fields = append(fields, errorJSONField{name, end - len(value), end})
	}
	if _, err := decoder.ReadToken(); err != nil {
		return nil, false
	}
	_, err = decoder.ReadToken()
	return fields, err == io.EOF
}

func sanitizeErrorObject(raw []byte, credentials []string) ([]byte, bool) {
	if len(raw) > maxErrorRedactionBytes {
		return raw, false
	}
	decoder := jsontext.NewDecoder(bytes.NewBuffer(raw))
	var edits []errorJSONEdit
	var walk func(int, string) bool
	walk = func(depth int, field string) bool {
		if depth > maxErrorRedactionDepth {
			return false
		}
		if errorProtocolField(field) {
			_, err := decoder.ReadValue()
			return err == nil
		}
		if sensitiveErrorField(field) {
			value, err := decoder.ReadValue()
			if err != nil {
				return false
			}
			marker, _ := json.Marshal(common.CredentialRedactionMarker(credentials...))
			if !bytes.Equal(value, marker) {
				end := int(decoder.InputOffset())
				edits = append(edits, errorJSONEdit{end - len(value), end, marker})
			}
			return true
		}
		start := int(decoder.InputOffset())
		token, err := decoder.ReadToken()
		if err != nil {
			return false
		}
		for start < len(raw) && strings.ContainsRune(" \r\n\t,:", rune(raw[start])) {
			start++
		}
		switch token.Kind() {
		case '{':
			for decoder.PeekKind() != '}' {
				key, err := decoder.ReadToken()
				if err != nil || !walk(depth+1, key.String()) {
					return false
				}
			}
			_, err = decoder.ReadToken()
		case '[':
			for decoder.PeekKind() != ']' {
				if !walk(depth+1, field) {
					return false
				}
			}
			_, err = decoder.ReadToken()
		case '"':
			if !errorProtocolField(field) {
				value := token.String()
				safe := SanitizeErrorText(value, credentials...)
				if safe != value {
					encoded, _ := json.Marshal(safe)
					edits = append(edits, errorJSONEdit{start, int(decoder.InputOffset()), encoded})
				}
			}
		}
		return err == nil
	}
	if !walk(0, "") {
		return raw, false
	}
	if _, err := decoder.ReadToken(); err != io.EOF {
		return raw, false
	}
	return applyErrorJSONEdits(raw, edits)
}

func applyErrorJSONEdits(raw []byte, edits []errorJSONEdit) ([]byte, bool) {
	if len(edits) == 0 {
		return raw, false
	}
	var out bytes.Buffer
	pos := 0
	for _, edit := range edits {
		out.Write(raw[pos:edit.start])
		out.Write(edit.value)
		pos = edit.end
	}
	out.Write(raw[pos:])
	return out.Bytes(), true
}

func errorProtocolField(field string) bool {
	switch strings.ToLower(field) {
	case "code", "type", "param", "id", "response_id", "event_id", "item_id", "previous_response_id", "model", "sequence_number":
		return true
	}
	return false
}
func sensitiveErrorField(field string) bool {
	if common.SensitiveCredentialLabel(field) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "authorization", "secret", "credential", "credentials":
		return true
	}
	return false
}
func SanitizeErrorText(message string, credentials ...string) string {
	safe, _ := common.RedactCredentialValuesText(message, credentials...)
	safe = common.RedactProviderErrorText(safe)
	safe, _ = common.RedactCredentialValuesText(safe, credentials...)
	return safe
}
