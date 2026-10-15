package wire

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

func convertClaude(src fields, ctx *Context) (json.RawMessage, error) {
	if err := checkFields(src, "request", "model", "messages", "system", "stream", "max_tokens", "temperature", "top_p", "top_k", "stop_sequences", "tools", "tool_choice", "thinking", "output_config"); err != nil {
		return nil, err
	}
	if raw, ok := src["output_config"]; ok {
		output, err := object(raw, "output_config")
		if err != nil {
			return nil, err
		}
		if err = checkFields(output, "output_config", "effort"); err != nil {
			return nil, err
		}
		if effort := output["effort"]; !null(effort) {
			if _, err = textValue(effort, "output_config.effort"); err != nil {
				return nil, err
			}
			return nil, unsupported("output_config.effort", "尚无经过验证的 Antigravity Claude effort 映射；不能以固定 thinking 预算或档位夹紧代替")
		}
	}
	out := fields{}
	cfg := fields{}
	for source, target := range map[string]string{"max_tokens": "maxOutputTokens", "temperature": "temperature", "top_p": "topP", "top_k": "topK", "stop_sequences": "stopSequences"} {
		addRaw(cfg, target, src, source)
	}
	if raw, ok := src["thinking"]; ok {
		t, err := object(raw, "thinking")
		if err != nil {
			return nil, err
		}
		if err = checkFields(t, "thinking", "type", "budget_tokens", "display"); err != nil {
			return nil, err
		}
		mode := stringValue(t["type"])
		if mode == "disabled" || mode == "adaptive" {
			if _, ok := t["budget_tokens"]; ok {
				return nil, unsupported("thinking.budget_tokens", "显式预算仅能映射 enabled thinking；不能用 disabled/adaptive 的固定或自动预算覆盖")
			}
		}
		thinking := fields{}
		switch mode {
		case "disabled":
			thinking["thinkingBudget"] = json.RawMessage("0")
			thinking["includeThoughts"] = json.RawMessage("false")
		case "enabled":
			addRaw(thinking, "thinkingBudget", t, "budget_tokens")
			thinking["includeThoughts"] = json.RawMessage("true")
		case "adaptive":
			thinking["thinkingBudget"] = json.RawMessage("-1")
			thinking["includeThoughts"] = json.RawMessage("true")
		default:
			return nil, unsupported("thinking.type", "无法映射该 thinking 表示")
		}
		if display, ok := t["display"]; ok && !null(display) {
			if mode == "disabled" {
				return nil, unsupported("thinking.display", "显式摘要显示仅支持 enabled 或 adaptive thinking")
			}
			switch stringValue(display) {
			case "summarized":
				// includeThoughts 返回目标的思考摘要，计算预算保持独立。
				thinking["includeThoughts"] = json.RawMessage("true")
			case "omitted":
				return nil, unsupported("thinking.display", "includeThoughts:false 尚不能保证 Claude 隐藏摘要后的签名续接；需验证真实签名载体后才能支持 omitted")
			default:
				return nil, unsupported("thinking.display", "当前转换仅支持 summarized；无法解释此摘要显示选项")
			}
		}
		cfg["thinkingConfig"] = marshal(thinking)
	}
	if raw, ok := src["tools"]; ok {
		tools, err := toolDeclarations(raw, DialectClaude, "tools")
		if err != nil {
			return nil, err
		}
		out["tools"] = tools
	}
	if raw, ok := src["tool_choice"]; ok {
		choice, err := toolChoice(raw, DialectClaude, "tool_choice")
		if err != nil {
			return nil, err
		}
		out["toolConfig"] = choice
	}
	if raw, ok := src["system"]; ok {
		parts, err := claudeContent(raw, ctx, "system", true)
		if err != nil {
			return nil, err
		}
		out["systemInstruction"] = marshal(map[string]any{"parts": parts})
	}
	messages, err := array(src["messages"], "messages")
	if err != nil {
		return nil, err
	}
	contents := make([]json.RawMessage, 0, len(messages))
	for i, item := range messages {
		p := fmt.Sprintf("messages[%d]", i)
		msg, err := object(item, p)
		if err != nil {
			return nil, err
		}
		if err = checkFields(msg, p, "role", "content"); err != nil {
			return nil, err
		}
		role := stringValue(msg["role"])
		if role != "assistant" && role != "user" {
			return nil, unsupported(p+".role", "无法转换该消息角色")
		}
		parts, err := claudeContent(msg["content"], ctx, p+".content", false)
		if err != nil {
			return nil, err
		}
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, content(role, parts))
	}
	out["contents"] = marshal(contents)
	if len(cfg) > 0 {
		out["generationConfig"] = marshal(cfg)
	}
	return marshal(out), nil
}

func claudeContent(raw json.RawMessage, ctx *Context, path string, system bool) ([]json.RawMessage, error) {
	if _, err := textValue(raw, path); err == nil {
		return []json.RawMessage{textPart(raw)}, nil
	}
	items, err := array(raw, path)
	if err != nil {
		return nil, err
	}
	parts := make([]json.RawMessage, 0, len(items))
	for i, item := range items {
		p := fmt.Sprintf("%s[%d]", path, i)
		block, err := object(item, p)
		if err != nil {
			return nil, err
		}
		typ := stringValue(block["type"])
		if system && typ != "text" {
			return nil, unsupported(p+".type", "系统指令仅支持文本")
		}
		switch typ {
		case "text":
			if err = checkFields(block, p, "type", "text", "cache_control"); err != nil {
				return nil, err
			}
			if err = claudeCacheControl(block["cache_control"], p+".cache_control"); err != nil {
				return nil, err
			}
			if _, err = textValue(block["text"], p+".text"); err != nil {
				return nil, err
			}
			parts = append(parts, textPart(block["text"]))
		case "image":
			image, err := claudeImage(block, p)
			if err != nil {
				return nil, err
			}
			parts = append(parts, image)
		case "thinking":
			if err = checkFields(block, p, "type", "thinking", "signature"); err != nil {
				return nil, err
			}
			if _, err = textValue(block["thinking"], p+".thinking"); err != nil {
				return nil, err
			}
			part := fields{"text": block["thinking"], "thought": json.RawMessage("true")}
			if signature, ok := block["signature"]; ok {
				value, err := textValue(signature, p+".signature")
				if err != nil {
					return nil, err
				}
				if IsCarrier(value) {
					restored, err := restoreCarrier([]json.RawMessage{marshal(part)}, value, p+".signature")
					if err != nil {
						return nil, err
					}
					parts = append(parts, restored...)
					continue
				}
				part["thoughtSignature"] = signature
			}
			parts = append(parts, marshal(part))
		case "tool_use":
			if err = checkFields(block, p, "type", "id", "name", "input", "signature", "cache_control"); err != nil {
				return nil, err
			}
			if err = claudeCacheControl(block["cache_control"], p+".cache_control"); err != nil {
				return nil, err
			}
			id, name := stringValue(block["id"]), stringValue(block["name"])
			if err = registerCall(ctx, id, name, p); err != nil {
				return nil, err
			}
			part, err := functionPart(id, name, block["input"], p+".input")
			if err != nil {
				return nil, err
			}
			if signature, ok := block["signature"]; ok {
				value, err := textValue(signature, p+".signature")
				if err != nil {
					return nil, err
				}
				if IsCarrier(value) {
					restored, err := restoreCarrier([]json.RawMessage{part}, value, p+".signature")
					if err != nil {
						return nil, err
					}
					parts = append(parts, restored...)
					continue
				}
				obj, _ := object(part, p)
				obj["thoughtSignature"] = signature
				part = marshal(obj)
			}
			parts = append(parts, part)
		case "tool_result":
			if err = checkFields(block, p, "type", "tool_use_id", "content", "is_error", "cache_control"); err != nil {
				return nil, err
			}
			if err = claudeCacheControl(block["cache_control"], p+".cache_control"); err != nil {
				return nil, err
			}
			id := stringValue(block["tool_use_id"])
			if value, ok := block["is_error"]; ok && !isTrue(value) && !isFalse(value) {
				return nil, unsupported(p+".is_error", "错误标记必须是布尔值")
			}
			name, ok := ctx.ToolNames[id]
			if !ok {
				return nil, unsupported(p+".tool_use_id", "找不到对应工具调用")
			}
			part, err := toolResultPart(id, name, block["content"], DialectClaude, isTrue(block["is_error"]), p+".content")
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		default:
			return nil, unsupported(p+".type", "该内容没有等价的目标表示")
		}
	}
	return parts, nil
}

func claudeImage(block fields, path string) (json.RawMessage, error) {
	if stringValue(block["type"]) != "image" {
		return nil, unsupported(path+".type", "仅支持图像附件")
	}
	if err := checkFields(block, path, "type", "source", "cache_control"); err != nil {
		return nil, err
	}
	if err := claudeCacheControl(block["cache_control"], path+".cache_control"); err != nil {
		return nil, err
	}
	source, err := object(block["source"], path+".source")
	if err != nil {
		return nil, err
	}
	if err = checkFields(source, path+".source", "type", "media_type", "data", "url"); err != nil {
		return nil, err
	}
	if stringValue(source["type"]) == "url" {
		return remoteImage(stringValue(source["url"]), path+".source.url")
	}
	if stringValue(source["type"]) != "base64" {
		return nil, unsupported(path+".source.type", "需要已经过媒体准备的内联图像")
	}
	mime, err := textValue(source["media_type"], path+".source.media_type")
	if err != nil {
		return nil, err
	}
	data, err := textValue(source["data"], path+".source.data")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(mime, "image/") {
		return nil, unsupported(path+".source.media_type", "媒体不是图像")
	}
	if _, err = base64.StdEncoding.DecodeString(data); err != nil {
		return nil, invalid(path+".source.data", err)
	}
	return marshal(map[string]any{"inlineData": map[string]string{"mimeType": mime, "data": data}}), nil
}
