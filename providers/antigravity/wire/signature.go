package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
	"unsafe"
)

// MaxRetainedBytes 限制每次转换保留的请求、关联及 carrier 展开数据。
const MaxRetainedBytes = 16 << 20

// CarrierPart 只携带无法通过目标协议原生字段往返的签名与位置。
// Kind 标识文本、思考、函数调用或独立签名；不保存可从客户端历史重建的正文。
type CarrierPart struct {
	Index      int             `json:"index"`
	Kind       string          `json:"kind,omitempty"`
	TextLength int             `json:"text_length,omitempty"`
	Signature  json.RawMessage `json:"signature,omitempty"`
	CallID     string          `json:"call_id,omitempty"`
}

func IsCarrier(value string) bool {
	if !strings.HasPrefix(value, "ag") {
		return false
	}
	i := 2
	for i < len(value) && value[i] >= '0' && value[i] <= '9' {
		i++
	}
	return i > 2 && i < len(value) && value[i] == '.'
}

func EncodeCarrier(parts []CarrierPart) (string, error) {
	if err := validateCarrier(parts); err != nil {
		return "", err
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	if len(raw)+base64.RawURLEncoding.EncodedLen(len(raw))+4+len(parts)*int(unsafe.Sizeof(CarrierPart{})) > MaxRetainedBytes {
		return "", unsupported("signature", "签名展开超过转换容量")
	}
	return "ag1." + base64.RawURLEncoding.EncodeToString(raw), nil
}

func DecodeCarrier(value string) ([]CarrierPart, error) {
	if !strings.HasPrefix(value, "ag1.") {
		return nil, unsupported("signature", "不支持的签名 carrier 版本")
	}
	encoded := value[4:]
	if len(value)+base64.RawURLEncoding.DecodedLen(len(encoded)) > MaxRetainedBytes {
		return nil, unsupported("signature", "签名展开超过转换容量")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, invalid("signature", err)
	}
	var parts []CarrierPart
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&parts); err != nil {
		return nil, invalid("signature", err)
	}
	if !json.Valid(raw) {
		return nil, unsupported("signature", "签名 carrier 不是单一 JSON 值")
	}
	if err = validateCarrier(parts); err != nil {
		return nil, err
	}
	if len(value)+len(raw)+len(parts)*int(unsafe.Sizeof(CarrierPart{})) > MaxRetainedBytes {
		return nil, unsupported("signature", "签名关联超过转换容量")
	}
	return parts, nil
}

func validateCarrier(parts []CarrierPart) error {
	for i, part := range parts {
		if part.Index != i {
			return unsupported("signature", "完整签名布局的位置必须连续")
		}
		if len(part.Signature) > 0 {
			var signature string
			if err := json.Unmarshal(part.Signature, &signature); err != nil || string(part.Signature) == "null" {
				return unsupported("signature", fmt.Sprintf("签名记录 %d 缺少原生字符串", i))
			}
		}
		if part.TextLength < 0 || part.TextLength > MaxRetainedBytes {
			return unsupported("signature", "签名文本关联长度无效")
		}
		switch part.Kind {
		case "text", "thought":
			if part.CallID != "" {
				return unsupported("signature", "签名文本关联不一致")
			}
		case "function":
			if part.CallID == "" || part.TextLength != 0 {
				return unsupported("signature", "签名工具关联不一致")
			}
		case "signature":
			if part.CallID != "" || part.TextLength != 0 || len(part.Signature) == 0 {
				return unsupported("signature", "独立签名关联不一致")
			}
		default:
			return unsupported("signature", "未知的签名内容关联")
		}
	}
	if len(parts) == 0 {
		return unsupported("signature", "状态 carrier 为空")
	}
	return nil
}

func restoreCarrier(parts []json.RawMessage, state, path string) ([]json.RawMessage, error) {
	records, err := DecodeCarrier(state)
	if err != nil {
		return nil, unsupported(path, err.Error())
	}
	return restoreLayout(parts, records, path)
}

func restoreLayout(parts []json.RawMessage, records []CarrierPart, path string) ([]json.RawMessage, error) {
	textBuffers := map[string]*strings.Builder{"text": {}, "thought": {}}
	calls := map[string]json.RawMessage{}
	for _, raw := range parts {
		part, err := object(raw, path)
		if err != nil {
			return nil, err
		}
		if call, ok := part["functionCall"]; ok {
			c, err := object(call, path)
			if err != nil {
				return nil, err
			}
			id := stringValue(c["id"])
			if id == "" {
				return nil, unsupported(path, "工具签名缺少调用关联")
			}
			if _, ok := calls[id]; ok {
				return nil, unsupported(path, "重复工具调用 ID")
			}
			calls[id] = raw
		} else if value, ok := part["text"]; ok {
			text, err := textValue(value, path)
			if err != nil {
				return nil, err
			}
			kind := "text"
			if isTrue(part["thought"]) {
				kind = "thought"
			}
			textBuffers[kind].WriteString(text)
		} else {
			return nil, unsupported(path, "签名状态不能覆盖无法关联的内容")
		}
	}
	texts := map[string]string{"text": textBuffers["text"].String(), "thought": textBuffers["thought"].String()}
	result := make([]json.RawMessage, 0, len(records))
	retained := 0
	for i, record := range records {
		if record.Index != i {
			return nil, unsupported(path, "完整签名布局的位置不连续")
		}
		var part fields
		switch record.Kind {
		case "text", "thought":
			remaining := texts[record.Kind]
			if record.TextLength > len(remaining) || !utf8.ValidString(remaining[:record.TextLength]) {
				return nil, unsupported(path, "签名文本长度与当前内容不一致")
			}
			part = fields{"text": marshal(remaining[:record.TextLength])}
			texts[record.Kind] = remaining[record.TextLength:]
			if record.Kind == "thought" {
				part["thought"] = json.RawMessage("true")
			}
		case "function":
			raw, ok := calls[record.CallID]
			if !ok {
				return nil, unsupported(path, "签名工具调用关联不一致")
			}
			delete(calls, record.CallID)
			var err error
			part, err = object(raw, path)
			if err != nil {
				return nil, err
			}
		case "signature":
			if len(record.Signature) == 0 {
				return nil, unsupported(path, "独立 part 缺少签名")
			}
			part = fields{}
		default:
			return nil, unsupported(path, "签名布局不完整")
		}
		if len(record.Signature) > 0 {
			if current, ok := part["thoughtSignature"]; ok && stringValue(current) != stringValue(record.Signature) {
				return nil, unsupported(path, "签名的多个表示相互冲突")
			}
			part["thoughtSignature"] = record.Signature
		}
		raw := marshal(part)
		retained += len(raw)
		if retained > MaxRetainedBytes {
			return nil, unsupported(path, "签名恢复超过转换容量")
		}
		result = append(result, raw)
	}
	if len(calls) > 0 || texts["text"] != "" || texts["thought"] != "" {
		return nil, unsupported(path, "签名状态与剩余内容不一致")
	}
	return result, nil
}

// InspectCarrier 只扫描协议规定的位置，供共享候选资格边界判断本地扩展归属。
func InspectCarrier(raw []byte, dialect Dialect) (bool, error) {
	root, err := object(raw, "request")
	if err != nil {
		return false, err
	}
	present := false
	validate := func(raw json.RawMessage, path string, explicit bool) error {
		value, err := textValue(raw, path)
		if !explicit && (err != nil || !IsCarrier(value)) {
			return nil
		}
		present = true
		if err != nil {
			return err
		}
		_, err = DecodeCarrier(value)
		if err != nil {
			return unsupported(path, err.Error())
		}
		return nil
	}
	switch dialect {
	case DialectChat, DialectClaude:
		var msgs []json.RawMessage
		if json.Unmarshal(root["messages"], &msgs) != nil {
			return false, nil
		}
		for i, raw := range msgs {
			var msg fields
			if json.Unmarshal(raw, &msg) != nil {
				continue
			}
			if dialect == DialectChat {
				var extra fields
				if json.Unmarshal(msg["extra_content"], &extra) != nil {
					continue
				}
				if state, ok := extra["antigravity_state"]; ok {
					if err = validate(state, fmt.Sprintf("messages[%d].extra_content.antigravity_state", i), true); err != nil {
						return present, err
					}
				}
			} else {
				var blocks []json.RawMessage
				if json.Unmarshal(msg["content"], &blocks) != nil {
					continue
				}
				for j, raw := range blocks {
					var block fields
					if json.Unmarshal(raw, &block) != nil {
						continue
					}
					typ := stringValue(block["type"])
					if typ == "tool_use" || typ == "thinking" {
						if signature, ok := block["signature"]; ok {
							if err = validate(signature, fmt.Sprintf("messages[%d].content[%d].signature", i, j), false); err != nil {
								return present, err
							}
						}
					}
				}
			}
		}
	case DialectResponses:
		var items []json.RawMessage
		if json.Unmarshal(root["input"], &items) != nil {
			return false, nil
		}
		for i, raw := range items {
			var item fields
			if json.Unmarshal(raw, &item) != nil {
				continue
			}
			if stringValue(item["type"]) == "reasoning" {
				if value, ok := item["encrypted_content"]; ok {
					if err = validate(value, fmt.Sprintf("input[%d].encrypted_content", i), false); err != nil {
						return present, err
					}
				}
			}
		}
	}
	return present, nil
}
