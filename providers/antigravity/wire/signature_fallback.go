package wire

import (
	"encoding/json"
	"strings"
)

const skipThoughtSignatureValidator = "skip_thought_signature_validator"

// 跨协议客户端不必保留上游签名扩展。先恢复已有 carrier，再为缺签名的
// Gemini 工具历史使用上游 bypass；它不恢复丢失的推理状态。
func fillMissingFunctionSignatures(raw json.RawMessage) (json.RawMessage, error) {
	body, err := object(raw, "request")
	if err != nil {
		return nil, err
	}
	contents, err := array(body["contents"], "contents")
	if err != nil {
		return nil, err
	}
	changed := false
	for i, contentRaw := range contents {
		item, err := object(contentRaw, "content")
		if err != nil {
			return nil, err
		}
		if stringValue(item["role"]) != "model" {
			continue
		}
		parts, err := array(item["parts"], "parts")
		if err != nil {
			return nil, err
		}
		contentChanged := false
		for j, partRaw := range parts {
			part, err := object(partRaw, "part")
			if err != nil {
				return nil, err
			}
			if _, ok := part["functionCall"]; !ok {
				continue
			}
			if signature, ok := part["thoughtSignature"]; ok && !null(signature) {
				var value string
				if json.Unmarshal(signature, &value) != nil || strings.TrimSpace(value) != "" {
					continue
				}
			}
			part["thoughtSignature"] = marshal(skipThoughtSignatureValidator)
			parts[j] = marshal(part)
			contentChanged = true
		}
		if contentChanged {
			item["parts"] = marshal(parts)
			contents[i] = marshal(item)
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	body["contents"] = marshal(contents)
	return marshal(body), nil
}
