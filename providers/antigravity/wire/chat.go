package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func convertChat(src fields, ctx *Context) (json.RawMessage, error) {
	if err := checkFields(src, "request", "model", "messages", "stream", "stream_options", "temperature", "top_p", "max_tokens", "max_completion_tokens", "stop", "n", "tools", "tool_choice", "parallel_tool_calls", "response_format", "reasoning_effort", "store", "prompt_cache_key"); err != nil {
		return nil, err
	}
	if err := promptCacheKey(src["prompt_cache_key"]); err != nil {
		return nil, err
	}
	if v, ok := src["store"]; ok && !isFalse(v) {
		return nil, unsupported("store", "不支持 Stored Chat")
	}
	if v, ok := src["parallel_tool_calls"]; ok && !isTrue(v) {
		return nil, unsupported("parallel_tool_calls", "目标协议无法保证单工具执行")
	}
	out := fields{}
	cfg := fields{}
	for source, target := range map[string]string{"temperature": "temperature", "top_p": "topP", "max_tokens": "maxOutputTokens", "n": "candidateCount"} {
		addRaw(cfg, target, src, source)
	}
	if value, ok := src["max_completion_tokens"]; ok {
		if _, conflict := src["max_tokens"]; conflict {
			return nil, unsupported("max_completion_tokens", "不能同时表达两个输出上限")
		}
		cfg["maxOutputTokens"] = value
	}
	if n, ok := src["n"]; ok {
		_ = json.Unmarshal(n, &ctx.ExpectedCandidates)
		if ctx.ExpectedCandidates < 1 {
			ctx.ExpectedCandidates = 1
		}
	}
	if stop, ok := src["stop"]; ok {
		if value, err := textValue(stop, "stop"); err == nil {
			cfg["stopSequences"] = marshal([]string{value})
		} else {
			cfg["stopSequences"] = stop
		}
	}
	if value, ok := src["reasoning_effort"]; ok {
		thinking, err := thinkingEffort(value, "reasoning_effort")
		if err != nil {
			return nil, err
		}
		cfg["thinkingConfig"] = marshal(thinking)
	}
	if value, ok := src["response_format"]; ok {
		format, err := responseFormat(value, "response_format")
		if err != nil {
			return nil, err
		}
		for key, raw := range format {
			cfg[key] = raw
		}
	}
	if value, ok := src["stream_options"]; ok {
		options, err := object(value, "stream_options")
		if err != nil {
			return nil, err
		}
		if err = checkFields(options, "stream_options", "include_usage"); err != nil {
			return nil, err
		}
		ctx.IncludeUsage = isTrue(options["include_usage"])
	}
	if value, ok := src["tools"]; ok {
		tools, err := toolDeclarations(value, DialectChat, "tools")
		if err != nil {
			return nil, err
		}
		out["tools"] = tools
	}
	if value, ok := src["tool_choice"]; ok {
		choice, err := toolChoice(value, DialectChat, "tool_choice")
		if err != nil {
			return nil, err
		}
		out["toolConfig"] = choice
	}
	msgs, err := array(src["messages"], "messages")
	if err != nil {
		return nil, err
	}
	contents := make([]json.RawMessage, 0, len(msgs))
	var toolResults toolResultTurn
	var system []json.RawMessage
	seenDialogue := false
	for i, msg := range msgs {
		path := fmt.Sprintf("messages[%d]", i)
		m, err := object(msg, path)
		if err != nil {
			return nil, err
		}
		if err = checkFields(m, path, "role", "content", "tool_calls", "tool_call_id", "reasoning_content", "extra_content"); err != nil {
			return nil, err
		}
		role := stringValue(m["role"])
		switch role {
		case "tool":
			if err = checkFields(m, path, "role", "content", "tool_call_id"); err != nil {
				return nil, err
			}
		case "system", "developer", "user":
			if err = checkFields(m, path, "role", "content"); err != nil {
				return nil, err
			}
		case "assistant":
			if _, ok := m["tool_call_id"]; ok {
				return nil, unsupported(path+".tool_call_id", "工具结果 ID 不能放在 assistant 消息")
			}
		}
		if role == "tool" {
			id := stringValue(m["tool_call_id"])
			name, ok := ctx.ToolNames[id]
			if !ok {
				return nil, unsupported(path+".tool_call_id", "找不到对应工具调用")
			}
			result, err := toolResultPart(id, name, m["content"], DialectChat, false, path+".content")
			if err != nil {
				return nil, err
			}
			toolResults.parts = append(toolResults.parts, result)
			seenDialogue = true
			continue
		}
		contents = toolResults.flush(contents)
		if role != "assistant" && role != "user" && role != "system" && role != "developer" {
			return nil, unsupported(path+".role", "此角色没有等价表示")
		}
		parts, err := chatContent(m["content"], path+".content")
		if err != nil {
			return nil, err
		}
		if role == "system" || role == "developer" {
			if seenDialogue {
				return nil, unsupported(path+".role", "对话中途的指令无法移动到系统指令而保持顺序")
			}
			if _, ok := m["tool_calls"]; ok {
				return nil, unsupported(path+".tool_calls", "系统指令不能携带工具调用")
			}
			system = append(system, parts...)
			continue
		}
		seenDialogue = true
		if value, ok := m["reasoning_content"]; ok && !null(value) {
			if role != "assistant" {
				return nil, unsupported(path+".reasoning_content", "仅 assistant 可包含思考历史")
			}
			if _, err := textValue(value, path+".reasoning_content"); err != nil {
				return nil, err
			}
			parts = append([]json.RawMessage{marshal(fields{"text": value, "thought": json.RawMessage("true")})}, parts...)
		}
		if calls, ok := m["tool_calls"]; ok {
			if role != "assistant" {
				return nil, unsupported(path+".tool_calls", "仅 assistant 可调用工具")
			}
			items, err := array(calls, path+".tool_calls")
			if err != nil {
				return nil, err
			}
			for j, raw := range items {
				p := fmt.Sprintf("%s.tool_calls[%d]", path, j)
				call, err := object(raw, p)
				if err != nil {
					return nil, err
				}
				if err = checkFields(call, p, "id", "type", "function", "index"); err != nil {
					return nil, err
				}
				if rawIndex, ok := call["index"]; ok {
					// OpenAI SDK 流式累积器会在完整历史中保留 delta index。
					// 只有与完整数组位置一致时，它才是可由顺序恢复的冗余关联。
					var index int
					if null(rawIndex) || json.Unmarshal(rawIndex, &index) != nil || index != j {
						return nil, unsupported(p+".index", "工具流索引必须与完整历史中的工具数组位置一致")
					}
				}
				if stringValue(call["type"]) != "function" {
					return nil, unsupported(p+".type", "仅支持 function 调用")
				}
				f, err := object(call["function"], p+".function")
				if err != nil {
					return nil, err
				}
				if err = checkFields(f, p+".function", "name", "arguments"); err != nil {
					return nil, err
				}
				id, name := stringValue(call["id"]), stringValue(f["name"])
				if err = registerCall(ctx, id, name, p); err != nil {
					return nil, err
				}
				args, err := textValue(f["arguments"], p+".function.arguments")
				if err != nil {
					return nil, err
				}
				part, err := functionPart(id, name, json.RawMessage(args), p+".function.arguments")
				if err != nil {
					return nil, err
				}
				parts = append(parts, part)
			}
		}
		if extra, ok := m["extra_content"]; ok {
			o, err := object(extra, path+".extra_content")
			if err != nil {
				return nil, err
			}
			if err = checkFields(o, path+".extra_content", "antigravity_state"); err != nil {
				return nil, err
			}
			if state, ok := o["antigravity_state"]; ok {
				if role != "assistant" {
					return nil, unsupported(path+".extra_content.antigravity_state", "签名状态只能关联 assistant 历史")
				}
				value, err := textValue(state, path+".extra_content.antigravity_state")
				if err != nil {
					return nil, err
				}
				parts, err = restoreCarrier(parts, value, path+".extra_content.antigravity_state")
				if err != nil {
					return nil, err
				}
			}
		}
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, content(role, parts))
	}
	contents = toolResults.flush(contents)
	out["contents"] = marshal(contents)
	if len(system) > 0 {
		out["systemInstruction"] = marshal(map[string]any{"parts": system})
	}
	if len(cfg) > 0 {
		out["generationConfig"] = marshal(cfg)
	}
	return marshal(out), nil
}

func chatContent(raw json.RawMessage, path string) ([]json.RawMessage, error) {
	if null(raw) {
		return nil, nil
	}
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
		o, err := object(item, p)
		if err != nil {
			return nil, err
		}
		switch stringValue(o["type"]) {
		case "text":
			if err = checkFields(o, p, "type", "text"); err != nil {
				return nil, err
			}
			if _, err = textValue(o["text"], p+".text"); err != nil {
				return nil, err
			}
			parts = append(parts, textPart(o["text"]))
		case "image_url":
			if err = checkFields(o, p, "type", "image_url"); err != nil {
				return nil, err
			}
			image, err := object(o["image_url"], p+".image_url")
			if err != nil {
				return nil, err
			}
			if err = checkFields(image, p+".image_url", "url", "detail"); err != nil {
				return nil, err
			}
			if detail, ok := image["detail"]; ok && stringValue(detail) != "auto" {
				return nil, unsupported(p+".image_url.detail", "图像精度没有等价映射")
			}
			part, err := dataImage(stringValue(image["url"]), p+".image_url.url")
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		default:
			return nil, unsupported(p+".type", "内容没有等价表示")
		}
	}
	return parts, nil
}

// 工具响应始终保留调用 ID；附件留在 functionResponse 内，避免并行工具归属丢失。
func toolResultPart(id, name string, raw json.RawMessage, dialect Dialect, isError bool, path string) (json.RawMessage, error) {
	response := fields{}
	var media []json.RawMessage
	result := raw
	if null(raw) {
		result = json.RawMessage("null")
	} else if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
		items, err := array(raw, path)
		if err != nil {
			return nil, err
		}
		textItems := make([]json.RawMessage, 0, len(items))
		for i, item := range items {
			p := fmt.Sprintf("%s[%d]", path, i)
			var o fields
			if json.Unmarshal(item, &o) != nil {
				textItems = append(textItems, item)
				continue
			}
			typ := stringValue(o["type"])
			if dialect == DialectClaude && typ == "text" {
				if _, ok := o["cache_control"]; ok {
					// 开始消费协议字段前拒绝歧义 envelope，不能让 map 的
					// last-wins 行为在重编码时静默折叠同层内容字段。
					o, err = object(item, p)
					if err != nil {
						return nil, err
					}
					if err = claudeCacheControl(o["cache_control"], p+".cache_control"); err != nil {
						return nil, err
					}
					// 这里只消费 Claude 文本块的空缓存声明，不遍历工具 JSON 业务子树。
					delete(o, "cache_control")
					item = marshal(o)
				}
			}
			if typ != "image" && typ != "input_image" && typ != "image_url" {
				// JSON 工具结果已有等价子树；不替上游解释其业务字段。
				textItems = append(textItems, item)
				continue
			}
			var image json.RawMessage
			switch dialect {
			case DialectClaude:
				// 媒体块也消费协议字段；必须从原始 item 检查歧义，不能
				// 将 map 中已被折叠的 source/cache_control 当作完整声明。
				o, err = object(item, p)
				if err == nil {
					image, err = claudeImage(o, p)
				}
			case DialectResponses:
				image, err = responsesImage(o, p)
			case DialectChat:
				parts, partErr := chatContent(marshal([]json.RawMessage{item}), p)
				err = partErr
				if err == nil && len(parts) == 1 {
					image = parts[0]
				}
			}
			if err != nil {
				return nil, err
			}
			if image == nil {
				return nil, unsupported(p, "工具结果媒体没有等价表示")
			}
			media = append(media, image)
		}
		result = marshal(textItems)
	}
	key := "result"
	if isError {
		key = "error"
	}
	response[key] = result
	function := map[string]any{"id": id, "name": name, "response": response}
	if len(media) > 0 {
		function["parts"] = media
	}
	return marshal(map[string]any{"functionResponse": function}), nil
}
