package wire

import (
	"encoding/json"
	"fmt"
	"strings"
)

func convertResponses(src fields, ctx *Context) (json.RawMessage, error) {
	if err := checkFields(src, "request", "model", "input", "instructions", "stream", "store", "background", "previous_response_id", "conversation", "max_output_tokens", "temperature", "top_p", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "text", "include", "truncation"); err != nil {
		return nil, err
	}
	if !isFalse(src["store"]) {
		return nil, unsupported("store", "无状态转换要求显式 store:false")
	}
	for _, name := range []string{"previous_response_id", "conversation"} {
		if raw, ok := src[name]; ok && !null(raw) {
			return nil, unsupported(name, "不支持上游资源续接")
		}
	}
	if raw, ok := src["background"]; ok && !isFalse(raw) {
		return nil, unsupported("background", "不支持后台响应生命周期")
	}
	if raw, ok := src["truncation"]; ok && stringValue(raw) != "disabled" {
		return nil, unsupported("truncation", "不支持自动改写历史")
	}
	if raw, ok := src["parallel_tool_calls"]; ok && !isTrue(raw) {
		return nil, unsupported("parallel_tool_calls", "目标协议无法保证单个工具执行")
	}
	if raw, ok := src["include"]; ok {
		items, err := array(raw, "include")
		if err != nil {
			return nil, err
		}
		for i, item := range items {
			if stringValue(item) != "reasoning.encrypted_content" {
				return nil, unsupported(fmt.Sprintf("include[%d]", i), "不支持该额外交付字段")
			}
		}
	}
	out := fields{}
	cfg := fields{}
	for source, target := range map[string]string{"max_output_tokens": "maxOutputTokens", "temperature": "temperature", "top_p": "topP"} {
		addRaw(cfg, target, src, source)
	}
	if raw, ok := src["reasoning"]; ok {
		reasoning, err := object(raw, "reasoning")
		if err != nil {
			return nil, err
		}
		if err = checkFields(reasoning, "reasoning", "effort", "summary"); err != nil {
			return nil, err
		}
		thinking := fields{}
		if raw, ok := reasoning["effort"]; ok {
			thinking, err = thinkingEffort(raw, "reasoning.effort")
			if err != nil {
				return nil, err
			}
		}
		if raw, ok := reasoning["summary"]; ok {
			switch stringValue(raw) {
			case "auto":
				thinking["includeThoughts"] = json.RawMessage("true")
			case "none":
				thinking["includeThoughts"] = json.RawMessage("false")
			default:
				return nil, unsupported("reasoning.summary", "无法保证指定摘要详细程度")
			}
		}
		if len(thinking) > 0 {
			cfg["thinkingConfig"] = marshal(thinking)
		}
	}
	if raw, ok := src["text"]; ok {
		t, err := object(raw, "text")
		if err != nil {
			return nil, err
		}
		if err = checkFields(t, "text", "format"); err != nil {
			return nil, err
		}
		if format, ok := t["format"]; ok {
			mapped, err := responseFormat(format, "text.format")
			if err != nil {
				return nil, err
			}
			for k, v := range mapped {
				cfg[k] = v
			}
		}
	}
	if raw, ok := src["tools"]; ok {
		tools, err := toolDeclarations(raw, DialectResponses, "tools")
		if err != nil {
			return nil, err
		}
		out["tools"] = tools
	}
	if raw, ok := src["tool_choice"]; ok {
		choice, err := toolChoice(raw, DialectResponses, "tool_choice")
		if err != nil {
			return nil, err
		}
		out["toolConfig"] = choice
	}
	var system []json.RawMessage
	if raw, ok := src["instructions"]; ok && !null(raw) {
		if _, err := textValue(raw, "instructions"); err != nil {
			return nil, err
		}
		system = append(system, textPart(raw))
	}
	contents := []json.RawMessage{}
	input := src["input"]
	if _, err := textValue(input, "input"); err == nil {
		contents = append(contents, content("user", []json.RawMessage{textPart(input)}))
	} else {
		items, err := array(input, "input")
		if err != nil {
			return nil, err
		}
		var modelParts []json.RawMessage
		var toolResults toolResultTurn
		carrier, carrierPath := "", ""
		flush := func() error {
			if len(modelParts) == 0 && carrier == "" {
				return nil
			}
			if carrier != "" {
				var err error
				modelParts, err = restoreCarrier(modelParts, carrier, carrierPath)
				if err != nil {
					return err
				}
			}
			contents = append(contents, content("model", modelParts))
			modelParts = nil
			carrier = ""
			return nil
		}
		for i, item := range items {
			p := fmt.Sprintf("input[%d]", i)
			o, err := object(item, p)
			if err != nil {
				return nil, err
			}
			typ := stringValue(o["type"])
			if typ != "function_call_output" {
				contents = toolResults.flush(contents)
			}
			if typ == "" || typ == "message" {
				if err = checkFields(o, p, "type", "role", "content", "id", "status"); err != nil {
					return nil, err
				}
				role := stringValue(o["role"])
				parts, err := responsesContent(o["content"], p+".content")
				if err != nil {
					return nil, err
				}
				switch role {
				case "assistant":
					modelParts = append(modelParts, parts...)
				case "user":
					if err = flush(); err != nil {
						return nil, err
					}
					contents = append(contents, content("user", parts))
				case "system", "developer":
					if len(contents) > 0 || len(modelParts) > 0 {
						return nil, unsupported(p+".role", "不能移动历史中途的系统指令")
					}
					system = append(system, parts...)
				default:
					return nil, unsupported(p+".role", "无法映射此角色")
				}
				continue
			}
			switch typ {
			case "function_call":
				if err = checkFields(o, p, "type", "id", "call_id", "name", "arguments", "status"); err != nil {
					return nil, err
				}
				id, name := stringValue(o["call_id"]), stringValue(o["name"])
				if err = registerCall(ctx, id, name, p); err != nil {
					return nil, err
				}
				args, err := textValue(o["arguments"], p+".arguments")
				if err != nil {
					return nil, err
				}
				part, err := functionPart(id, name, json.RawMessage(args), p+".arguments")
				if err != nil {
					return nil, err
				}
				modelParts = append(modelParts, part)
			case "function_call_output":
				if err = checkFields(o, p, "type", "id", "call_id", "output", "status"); err != nil {
					return nil, err
				}
				if err = flush(); err != nil {
					return nil, err
				}
				id := stringValue(o["call_id"])
				name, ok := ctx.ToolNames[id]
				if !ok {
					return nil, unsupported(p+".call_id", "找不到对应工具调用")
				}
				part, err := toolResultPart(id, name, o["output"], DialectResponses, false, p+".output")
				if err != nil {
					return nil, err
				}
				toolResults.parts = append(toolResults.parts, part)
			case "reasoning":
				if err = checkFields(o, p, "type", "id", "summary", "encrypted_content", "status"); err != nil {
					return nil, err
				}
				if raw, ok := o["summary"]; ok {
					summaries, err := array(raw, p+".summary")
					if err != nil {
						return nil, err
					}
					for j, summary := range summaries {
						sp := fmt.Sprintf("%s.summary[%d]", p, j)
						s, err := object(summary, sp)
						if err != nil {
							return nil, err
						}
						if err = checkFields(s, sp, "type", "text"); err != nil {
							return nil, err
						}
						if stringValue(s["type"]) != "summary_text" {
							return nil, unsupported(sp+".type", "无法恢复该思考摘要")
						}
						if _, err = textValue(s["text"], sp+".text"); err != nil {
							return nil, err
						}
						modelParts = append(modelParts, marshal(fields{"text": s["text"], "thought": json.RawMessage("true")}))
					}
				}
				if raw, ok := o["encrypted_content"]; ok && !null(raw) {
					if carrier != "" {
						return nil, unsupported(p+".encrypted_content", "同一轮只能有一个完整签名状态")
					}
					carrier, err = textValue(raw, p+".encrypted_content")
					if err != nil {
						return nil, err
					}
					carrierPath = p + ".encrypted_content"
					if !IsCarrier(carrier) {
						return nil, unsupported(carrierPath, "无法解释其他上游的加密状态")
					}
				}
			default:
				return nil, unsupported(p+".type", "该 item 没有等价的目标表示")
			}
		}
		contents = toolResults.flush(contents)
		if err = flush(); err != nil {
			return nil, err
		}
	}
	out["contents"] = marshal(contents)
	if len(system) > 0 {
		out["systemInstruction"] = marshal(map[string]any{"parts": system})
	}
	if len(cfg) > 0 {
		out["generationConfig"] = marshal(cfg)
	}
	return marshal(out), nil
}

func responsesContent(raw json.RawMessage, path string) ([]json.RawMessage, error) {
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
		case "input_text", "output_text":
			if err = checkFields(o, p, "type", "text", "annotations", "logprobs"); err != nil {
				return nil, err
			}
			for _, name := range []string{"annotations", "logprobs"} {
				if value, ok := o[name]; ok && !null(value) {
					a, err := array(value, p+"."+name)
					if err != nil || len(a) > 0 {
						return nil, unsupported(p+"."+name, "无法保留该输出元数据")
					}
				}
			}
			if _, err = textValue(o["text"], p+".text"); err != nil {
				return nil, err
			}
			parts = append(parts, textPart(o["text"]))
		case "input_image":
			image, err := responsesImage(o, p)
			if err != nil {
				return nil, err
			}
			parts = append(parts, image)
		default:
			return nil, unsupported(p+".type", "该内容没有等价表示")
		}
	}
	return parts, nil
}

func responsesImage(o fields, path string) (json.RawMessage, error) {
	if stringValue(o["type"]) != "input_image" {
		return nil, unsupported(path+".type", "仅支持图像附件")
	}
	if err := checkFields(o, path, "type", "image_url", "detail"); err != nil {
		return nil, err
	}
	if detail, ok := o["detail"]; ok && stringValue(detail) != "auto" {
		return nil, unsupported(path+".detail", "图像精度没有等价映射")
	}
	uri := stringValue(o["image_url"])
	if !strings.HasPrefix(uri, "data:") {
		// Responses provider 路径没有共享媒体物化步骤，不能把待下载 URL
		// 冒充已经获准的 Gemini file URI，或由 provider 自行绕过下载边界。
		return nil, unsupported(path+".image_url", "该转换仅支持内联图像，尚无远程图像的等价准备路径")
	}
	return dataImage(uri, path+".image_url")
}
