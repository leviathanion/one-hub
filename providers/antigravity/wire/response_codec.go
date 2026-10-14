package wire

import (
	"encoding/json"
	"fmt"
	"strings"
)

func sseFrame(event string, value any) []byte {
	raw, _ := json.Marshal(value)
	if event == "" {
		return append(append([]byte("data: "), raw...), []byte("\n\n")...)
	}
	return append(append([]byte("event: "+event+"\ndata: "), raw...), []byte("\n\n")...)
}

func (d *ResponseDecoder) event(name string, fields map[string]any) []byte {
	fields["type"] = name
	if d.context.Dialect == DialectResponses {
		fields["sequence_number"] = d.sequence
		d.sequence++
	}
	return sseFrame(name, fields)
}

func (d *ResponseDecoder) chatChunk(index int, delta map[string]any, finish any) []byte {
	return sseFrame("", map[string]any{"id": d.id, "object": "chat.completion.chunk", "created": d.created, "model": d.context.Model,
		"choices": []any{map[string]any{"index": index, "delta": delta, "finish_reason": finish}}})
}

func (d *ResponseDecoder) start(c *responseCandidate, frames *[][]byte) {
	if c.started {
		return
	}
	c.started = true
	switch d.context.Dialect {
	case DialectChat:
		*frames = append(*frames, d.chatChunk(c.index, map[string]any{"role": "assistant"}, nil))
	case DialectClaude:
		message := map[string]any{"id": d.id, "type": "message", "role": "assistant", "model": d.context.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil}
		// Claude 起始事件要求完整 usage；初值不写入原始用量或计费证据。
		usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
		for name, value := range usageFields(d.native.usage, DialectClaude) {
			usage[name] = value
		}
		message["usage"] = usage
		*frames = append(*frames, d.event("message_start", map[string]any{"message": message}))
	case DialectResponses:
		if !d.started {
			response := d.responseBase("in_progress", []any{})
			*frames = append(*frames, d.event("response.created", map[string]any{"response": response}), d.event("response.in_progress", map[string]any{"response": response}))
		}
	}
	d.started = true
}

func (d *ResponseDecoder) emitPart(c *responseCandidate, b *responseBlock, text string, frames *[][]byte) error {
	d.start(c, frames)
	switch d.context.Dialect {
	case DialectChat:
		delta := map[string]any{}
		switch b.kind {
		case "text":
			delta["content"] = text
		case "thought":
			delta["reasoning_content"] = text
		case "function":
			delta["tool_calls"] = []any{map[string]any{"index": b.toolIndex, "id": b.callID, "type": "function", "function": map[string]any{"name": b.name, "arguments": string(b.args)}}}
		}
		*frames = append(*frames, d.chatChunk(c.index, delta, nil))
	case DialectClaude:
		if b.kind == "function" {
			return nil
		}
		if !b.started {
			block := map[string]any{"type": "text", "text": ""}
			if b.kind == "thought" {
				block = map[string]any{"type": "thinking", "thinking": ""}
			}
			*frames = append(*frames, d.event("content_block_start", map[string]any{"index": b.outputIndex, "content_block": block}))
			b.started = true
		}
		delta := map[string]any{"type": "text_delta", "text": text}
		if b.kind == "thought" {
			delta = map[string]any{"type": "thinking_delta", "thinking": text}
		}
		*frames = append(*frames, d.event("content_block_delta", map[string]any{"index": b.outputIndex, "delta": delta}))
	case DialectResponses:
		if !b.started {
			*frames = append(*frames, d.event("response.output_item.added", map[string]any{"output_index": b.outputIndex, "item": d.responseItem(b, false)}))
			if b.kind == "text" {
				*frames = append(*frames, d.event("response.content_part.added", map[string]any{"item_id": b.itemID, "output_index": b.outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}))
			} else if b.kind == "thought" {
				*frames = append(*frames, d.event("response.reasoning_summary_part.added", map[string]any{"item_id": b.itemID, "output_index": b.outputIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}))
			}
			b.started = true
		}
		fields := map[string]any{"item_id": b.itemID, "output_index": b.outputIndex, "delta": text}
		event := "response.output_text.delta"
		if b.kind == "text" {
			fields["content_index"] = 0
		}
		if b.kind == "thought" {
			event = "response.reasoning_summary_text.delta"
			fields["summary_index"] = 0
		}
		if b.kind == "function" {
			event = "response.function_call_arguments.delta"
			fields["delta"] = string(b.args)
		}
		*frames = append(*frames, d.event(event, fields))
	default:
		return fmt.Errorf("不支持的响应方言 %q", d.context.Dialect)
	}
	return nil
}

func blockCarrier(b *responseBlock) (string, error) {
	records := []CarrierPart{{Index: 0, Kind: b.kind, TextLength: b.length, CallID: b.callID, Signature: b.signature}}
	for _, signature := range b.standalone {
		records = append(records, CarrierPart{Index: len(records), Kind: "signature", Signature: signature})
	}
	return EncodeCarrier(records)
}

func (c *responseCandidate) needsChatCarrier() bool {
	if c.signed {
		return true
	}
	// Chat 原生字段只能重建单个 thought、单个 text、多个 function 的顺序。
	seenText, seenFunction := false, false
	for i, part := range c.layout {
		switch part.Kind {
		case "thought":
			if i != 0 {
				return true
			}
		case "text":
			if seenText || seenFunction {
				return true
			}
			seenText = true
		case "function":
			seenFunction = true
		}
	}
	return false
}

func (d *ResponseDecoder) closeBlock(c *responseCandidate, frames *[][]byte) error {
	b := c.current
	if b == nil {
		return nil
	}
	if d.stream {
		switch d.context.Dialect {
		case DialectClaude:
			if b.kind == "function" {
				block := map[string]any{"type": "tool_use", "id": b.callID, "name": b.name, "input": map[string]any{}}
				if len(b.signature) > 0 || len(b.standalone) > 0 {
					signature, err := blockCarrier(b)
					if err != nil {
						return err
					}
					block["signature"] = signature
				}
				*frames = append(*frames, d.event("content_block_start", map[string]any{"index": b.outputIndex, "content_block": block}),
					d.event("content_block_delta", map[string]any{"index": b.outputIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(b.args)}}))
			} else if b.kind == "thought" && (len(b.signature) > 0 || len(b.standalone) > 0) {
				var signature string
				if len(b.standalone) > 0 {
					value, err := blockCarrier(b)
					if err != nil {
						return err
					}
					signature = value
				} else {
					_ = json.Unmarshal(b.signature, &signature)
				}
				*frames = append(*frames, d.event("content_block_delta", map[string]any{"index": b.outputIndex, "delta": map[string]any{"type": "signature_delta", "signature": signature}}))
			}
			*frames = append(*frames, d.event("content_block_stop", map[string]any{"index": b.outputIndex}))
		case DialectResponses:
			fields := map[string]any{"item_id": b.itemID, "output_index": b.outputIndex}
			switch b.kind {
			case "text":
				fields["content_index"] = 0
				fields["text"] = b.text.String()
				*frames = append(*frames, d.event("response.output_text.done", fields), d.event("response.content_part.done", map[string]any{"item_id": b.itemID, "output_index": b.outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": b.text.String(), "annotations": []any{}}}))
			case "thought":
				fields["summary_index"] = 0
				fields["text"] = b.text.String()
				*frames = append(*frames, d.event("response.reasoning_summary_text.done", fields), d.event("response.reasoning_summary_part.done", map[string]any{"item_id": b.itemID, "output_index": b.outputIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": b.text.String()}}))
			case "function":
				fields["arguments"] = string(b.args)
				*frames = append(*frames, d.event("response.function_call_arguments.done", fields))
			}
			*frames = append(*frames, d.event("response.output_item.done", map[string]any{"output_index": b.outputIndex, "item": d.responseItem(b, true)}))
		}
	}
	if !d.collects() {
		// 已交付文本不保留；仅 carrier 的位置/长度/签名存活至 turn 结束。
		d.retained -= len(b.args) + len(b.name) + 192
	}
	c.current = nil
	return nil
}

func (d *ResponseDecoder) responseItem(b *responseBlock, complete bool) map[string]any {
	status, text, args := "in_progress", "", ""
	if complete {
		status = "completed"
		text = b.text.String()
		args = string(b.args)
	}
	switch b.kind {
	case "function":
		return map[string]any{"type": "function_call", "id": b.itemID, "call_id": b.callID, "name": b.name, "arguments": args, "status": status}
	case "thought":
		summary := []any{}
		if complete {
			summary = append(summary, map[string]any{"type": "summary_text", "text": text})
		}
		return map[string]any{"type": "reasoning", "id": b.itemID, "summary": summary}
	default:
		content := []any{}
		if complete {
			content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
		}
		return map[string]any{"type": "message", "id": b.itemID, "role": "assistant", "status": status, "content": content}
	}
}

func (d *ResponseDecoder) responseBase(status string, output []any) map[string]any {
	return map[string]any{"id": d.id, "object": "response", "created_at": d.created, "model": d.context.Model, "status": status, "output": output, "store": false}
}

func (d *ResponseDecoder) responseObject(indices []int) (map[string]any, error) {
	switch d.context.Dialect {
	case DialectChat:
		choices := []any{}
		for _, index := range indices {
			c := d.candidates[index]
			message := map[string]any{"role": "assistant", "content": nil}
			var text, thought strings.Builder
			hasText, hasThought := false, false
			tools := []any{}
			for _, b := range c.blocks {
				switch b.kind {
				case "text":
					text.WriteString(b.text.String())
					hasText = true
				case "thought":
					thought.WriteString(b.text.String())
					hasThought = true
				case "function":
					tools = append(tools, map[string]any{"id": b.callID, "type": "function", "function": map[string]any{"name": b.name, "arguments": string(b.args)}})
				}
			}
			if hasText {
				message["content"] = text.String()
			}
			if hasThought {
				message["reasoning_content"] = thought.String()
			}
			if len(tools) > 0 {
				message["tool_calls"] = tools
			}
			if c.needsChatCarrier() {
				carrier, err := EncodeCarrier(c.layout)
				if err != nil {
					return nil, err
				}
				message["extra_content"] = map[string]any{"antigravity_state": carrier}
			}
			finish, err := mappedFinish(c.finish, c.toolCount > 0, d.context.Dialect)
			if err != nil {
				return nil, err
			}
			choices = append(choices, map[string]any{"index": index, "message": message, "finish_reason": finish})
		}
		result := map[string]any{"id": d.id, "object": "chat.completion", "created": d.created, "model": d.context.Model, "choices": choices}
		if usage := usageFields(d.native.usage, DialectChat); usage != nil {
			result["usage"] = usage
		}
		return result, nil
	case DialectClaude:
		c := d.candidates[0]
		content := []any{}
		for _, b := range c.blocks {
			var block map[string]any
			switch b.kind {
			case "text":
				block = map[string]any{"type": "text", "text": b.text.String()}
			case "thought":
				block = map[string]any{"type": "thinking", "thinking": b.text.String()}
				if len(b.standalone) > 0 {
					carrier, err := blockCarrier(b)
					if err != nil {
						return nil, err
					}
					block["signature"] = carrier
				} else if len(b.signature) > 0 {
					block["signature"] = b.signature
				}
			case "function":
				block = map[string]any{"type": "tool_use", "id": b.callID, "name": b.name, "input": b.args}
				if len(b.signature) > 0 || len(b.standalone) > 0 {
					carrier, err := blockCarrier(b)
					if err != nil {
						return nil, err
					}
					block["signature"] = carrier
				}
			}
			content = append(content, block)
		}
		finish, err := mappedFinish(c.finish, c.toolCount > 0, DialectClaude)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"id": d.id, "type": "message", "role": "assistant", "model": d.context.Model, "content": content, "stop_reason": finish, "stop_sequence": nil}
		if usage := usageFields(d.native.usage, DialectClaude); usage != nil {
			result["usage"] = usage
		}
		return result, nil
	case DialectResponses:
		c := d.candidates[0]
		output := []any{}
		for _, b := range c.blocks {
			output = append(output, d.responseItem(b, true))
		}
		if c.signed {
			carrier, err := EncodeCarrier(c.layout)
			if err != nil {
				return nil, err
			}
			output = append(output, map[string]any{"type": "reasoning", "id": d.id + "_continuation", "summary": []any{}, "encrypted_content": carrier})
		}
		finish, err := mappedFinish(c.finish, c.toolCount > 0, DialectResponses)
		if err != nil {
			return nil, err
		}
		status := "completed"
		if finish == "length" || finish == "content_filter" {
			status = "incomplete"
		}
		result := d.responseBase(status, output)
		if status == "incomplete" {
			reason := "content_filter"
			if finish == "length" {
				reason = "max_output_tokens"
			}
			result["incomplete_details"] = map[string]any{"reason": reason}
		}
		if usage := usageFields(d.native.usage, DialectResponses); usage != nil {
			result["usage"] = usage
		}
		return result, nil
	default:
		return nil, fmt.Errorf("不支持的响应方言 %q", d.context.Dialect)
	}
}

func (d *ResponseDecoder) finishStream(indices []int, frames *[][]byte) error {
	for _, index := range indices {
		d.start(d.candidates[index], frames)
	}
	switch d.context.Dialect {
	case DialectChat:
		for _, index := range indices {
			c := d.candidates[index]
			if c.needsChatCarrier() {
				carrier, err := EncodeCarrier(c.layout)
				if err != nil {
					return err
				}
				*frames = append(*frames, d.chatChunk(index, map[string]any{"extra_content": map[string]any{"antigravity_state": carrier}}, nil))
			}
			finish, err := mappedFinish(c.finish, c.toolCount > 0, DialectChat)
			if err != nil {
				return err
			}
			*frames = append(*frames, d.chatChunk(index, map[string]any{}, finish))
		}
		if d.context.IncludeUsage {
			if usage := usageFields(d.native.usage, DialectChat); usage != nil {
				*frames = append(*frames, sseFrame("", map[string]any{"id": d.id, "object": "chat.completion.chunk", "created": d.created, "model": d.context.Model, "choices": []any{}, "usage": usage}))
			}
		}
		*frames = append(*frames, []byte("data: [DONE]\n\n"))
	case DialectClaude:
		c := d.candidates[0]
		finish, err := mappedFinish(c.finish, c.toolCount > 0, DialectClaude)
		if err != nil {
			return err
		}
		fields := map[string]any{"delta": map[string]any{"stop_reason": finish, "stop_sequence": nil}}
		if usage := usageFields(d.native.usage, DialectClaude); usage != nil {
			fields["usage"] = usage
		}
		*frames = append(*frames, d.event("message_delta", fields), d.event("message_stop", map[string]any{}))
	case DialectResponses:
		response, err := d.responseObject(indices)
		if err != nil {
			return err
		}
		encoded, _ := json.Marshal(response)
		if len(encoded)+d.retained > MaxRetainedBytes {
			return fmt.Errorf("Antigravity Responses 完成快照超过转换容量")
		}
		c := d.candidates[0]
		if c.signed {
			output := response["output"].([]any)
			item := output[len(output)-1]
			*frames = append(*frames, d.event("response.output_item.added", map[string]any{"output_index": d.nextOutput, "item": item}), d.event("response.output_item.done", map[string]any{"output_index": d.nextOutput, "item": item}))
		}
		event := "response.completed"
		if response["status"] == "incomplete" {
			event = "response.incomplete"
		}
		*frames = append(*frames, d.event(event, map[string]any{"response": response}))
	}
	return nil
}
