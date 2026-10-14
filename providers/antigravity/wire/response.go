package wire

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type responseBlock struct {
	kind        string
	index       int
	outputIndex int
	itemID      string
	text        strings.Builder
	length      int
	callID      string
	name        string
	args        json.RawMessage
	signature   json.RawMessage
	standalone  []json.RawMessage
	started     bool
	toolIndex   int
}

type responseCandidate struct {
	index     int
	finish    string
	current   *responseBlock
	blocks    []*responseBlock
	layout    []CarrierPart
	signed    bool
	toolCount int
	started   bool
	callIDs   map[string]struct{}
}

// ResponseDecoder 共享上游事件语义与三种下游编码。Finish 仅能在正常 EOF
// 调用；传输失败不得通过 Finish 合成成功终态。
type ResponseDecoder struct {
	context    *Context
	id         string
	created    int64
	stream     bool
	native     *NativeDecoder
	candidates map[int]*responseCandidate
	started    bool
	finished   bool
	failed     error
	retained   int
	sequence   int
	nextOutput int
}

func NewResponseDecoder(context *Context, responseID string, stream bool) *ResponseDecoder {
	if context == nil {
		context = &Context{}
	}
	return &ResponseDecoder{context: context, id: responseID, created: time.Now().Unix(), stream: stream,
		native: NewNativeDecoder(context.ExpectedCandidates), candidates: make(map[int]*responseCandidate)}
}

func (d *ResponseDecoder) UsageRaw() json.RawMessage { return d.native.UsageRaw() }

func (d *ResponseDecoder) ResponseModel() string { return d.native.ResponseModel() }

func (d *ResponseDecoder) reserve(size int) error {
	if size < 0 || size > MaxRetainedBytes-d.retained {
		return fmt.Errorf("Antigravity 响应转换超过 %d 字节保留预算", MaxRetainedBytes)
	}
	d.retained += size
	return nil
}

func (d *ResponseDecoder) Push(data []byte) (frames [][]byte, err error) {
	if d.failed != nil {
		return nil, d.failed
	}
	if d.finished {
		return nil, fmt.Errorf("Antigravity 响应已结束")
	}
	defer func() {
		if err == nil {
			err = d.checkFrames(frames)
		}
		if err != nil {
			d.failed = err
			frames = nil
		}
	}()
	inner, err := d.native.Push(data)
	if err != nil {
		return nil, err
	}
	var response map[string]json.RawMessage
	_ = json.Unmarshal(inner, &response)
	for key := range response {
		switch key {
		case "candidates", "usageMetadata", "promptFeedback", "modelVersion", "responseId", "createTime":
		default:
			return nil, fmt.Errorf("Antigravity response.%s 无法转换到 %s", key, d.context.Dialect)
		}
	}
	var candidates []map[string]json.RawMessage
	if raw := response["candidates"]; hasJSONValue(raw) && json.Unmarshal(raw, &candidates) != nil {
		return nil, fmt.Errorf("Antigravity candidates 无法转换")
	}
	for position, rawCandidate := range candidates {
		for key := range rawCandidate {
			switch key {
			case "content", "finishReason", "index", "safetyRatings", "finishMessage", "tokenCount", "avgLogprobs", "groundingMetadata", "groundingAttributions", "citationMetadata", "logprobsResult":
			default:
				return nil, fmt.Errorf("Antigravity candidate.%s 无法转换到 %s", key, d.context.Dialect)
			}
		}
		index := position
		if raw, exists := rawCandidate["index"]; exists && json.Unmarshal(raw, &index) != nil {
			return nil, fmt.Errorf("Antigravity candidate index 无效")
		}
		if index != 0 && d.context.Dialect != DialectChat {
			return nil, fmt.Errorf("%s 无法表示多个候选响应", d.context.Dialect)
		}
		candidate := d.candidates[index]
		if candidate == nil {
			if err = d.reserve(256); err != nil {
				return nil, err
			}
			candidate = &responseCandidate{index: index, callIDs: make(map[string]struct{})}
			d.candidates[index] = candidate
		}
		for _, name := range []string{"groundingMetadata", "groundingAttributions", "citationMetadata", "logprobsResult"} {
			if value := rawCandidate[name]; hasJSONValue(value) && string(value) != "{}" && string(value) != "[]" {
				return nil, fmt.Errorf("Antigravity 输出 %s 缺少 %s 等价表示", name, d.context.Dialect)
			}
		}
		var content map[string]json.RawMessage
		if raw := rawCandidate["content"]; hasJSONValue(raw) && json.Unmarshal(raw, &content) != nil {
			return nil, fmt.Errorf("Antigravity content 无法转换")
		}
		for key := range content {
			if key != "role" && key != "parts" {
				return nil, fmt.Errorf("Antigravity content.%s 无法转换", key)
			}
		}
		if raw := content["role"]; hasJSONValue(raw) {
			var role string
			if json.Unmarshal(raw, &role) != nil || role != "model" {
				return nil, fmt.Errorf("Antigravity 输出角色无法转换")
			}
		}
		var parts []map[string]json.RawMessage
		if raw := content["parts"]; hasJSONValue(raw) && json.Unmarshal(raw, &parts) != nil {
			return nil, fmt.Errorf("Antigravity parts 无法转换")
		}
		for partIndex, part := range parts {
			if err = d.acceptPart(candidate, part, partIndex == 0, &frames); err != nil {
				return frames, err
			}
		}
		if raw := rawCandidate["finishReason"]; hasJSONValue(raw) {
			var reason string
			if json.Unmarshal(raw, &reason) != nil {
				return nil, fmt.Errorf("Antigravity finishReason 无法转换")
			}
			if reason != "" {
				if candidate.finish != "" && candidate.finish != reason {
					return nil, fmt.Errorf("Antigravity candidate 终态相互冲突")
				}
				if _, err = mappedFinish(reason, candidate.toolCount > 0, d.context.Dialect); err != nil {
					return frames, err
				}
				candidate.finish = reason
			}
		}
	}
	return frames, nil
}

func (d *ResponseDecoder) acceptPart(c *responseCandidate, part map[string]json.RawMessage, continuation bool, frames *[][]byte) error {
	for name := range part {
		switch name {
		case "text", "thought", "thoughtSignature", "functionCall":
		default:
			return fmt.Errorf("Antigravity 输出 part.%s 无法转换到 %s", name, d.context.Dialect)
		}
	}
	var signature json.RawMessage
	if raw, exists := part["thoughtSignature"]; exists {
		var value string
		if json.Unmarshal(raw, &value) != nil || string(raw) == "null" {
			return fmt.Errorf("Antigravity thoughtSignature 不是字符串")
		}
		signature = append(json.RawMessage(nil), raw...)
	}
	textRaw, hasText := part["text"]
	callRaw, hasCall := part["functionCall"]
	if hasText && hasCall {
		return fmt.Errorf("Antigravity part 同时含 text 与 functionCall，无法确定其顺序")
	}
	if !hasText && !hasCall {
		if len(signature) == 0 {
			return fmt.Errorf("Antigravity 空 part 无法转换")
		}
		if raw := part["thought"]; hasJSONValue(raw) && string(raw) != "false" {
			return fmt.Errorf("Antigravity 独立签名包含无法关联的 thought 标记")
		}
		if err := d.reserve(len(signature)*2 + 160); err != nil {
			return err
		}
		if d.context.Dialect == DialectClaude && (c.current == nil || (c.current.kind != "thought" && c.current.kind != "function")) {
			return fmt.Errorf("Antigravity 独立签名没有可关联的 Claude thinking/tool_use block")
		}
		c.layout = append(c.layout, CarrierPart{Index: len(c.layout), Kind: "signature", Signature: signature})
		c.signed = true
		if c.current != nil {
			c.current.standalone = append(c.current.standalone, signature)
		}
		return nil
	}
	if c.finish != "" {
		return fmt.Errorf("Antigravity candidate 终态之后仍返回生成内容")
	}
	kind := "text"
	var thought bool
	if raw, exists := part["thought"]; exists && json.Unmarshal(raw, &thought) != nil {
		return fmt.Errorf("Antigravity thought 不是布尔值")
	}
	if thought {
		kind = "thought"
	}
	if hasCall {
		if thought {
			return fmt.Errorf("Antigravity functionCall 的 thought 标记无法转换")
		}
		kind = "function"
	}
	var text string
	if hasText && (json.Unmarshal(textRaw, &text) != nil || string(textRaw) == "null") {
		return fmt.Errorf("Antigravity text 不是字符串")
	}
	current := c.current
	// 只有事件首个 part 可续接上一事件的文本增量；同一数组的后续 part 有独立边界。
	merge := continuation && current != nil && current.kind == kind && kind != "function" && len(current.signature) == 0 && len(current.standalone) == 0
	if !merge {
		if err := d.closeBlock(c, frames); err != nil {
			return err
		}
		if err := d.reserve(512); err != nil {
			return err
		}
		current = &responseBlock{kind: kind, index: len(c.layout), outputIndex: d.nextOutput, toolIndex: c.toolCount}
		d.nextOutput++
		current.itemID = fmt.Sprintf("%s_%d", d.id, current.outputIndex)
		c.current = current
		c.layout = append(c.layout, CarrierPart{Index: current.index, Kind: kind})
		if d.collects() {
			c.blocks = append(c.blocks, current)
		}
	}
	if hasCall {
		var call map[string]json.RawMessage
		if json.Unmarshal(callRaw, &call) != nil || call == nil {
			return fmt.Errorf("Antigravity functionCall 不是 object")
		}
		for key := range call {
			if key != "name" && key != "args" && key != "id" {
				return fmt.Errorf("Antigravity functionCall.%s 无法转换", key)
			}
		}
		if json.Unmarshal(call["name"], &current.name) != nil || current.name == "" {
			return fmt.Errorf("Antigravity functionCall 缺少 name")
		}
		if raw := call["id"]; hasJSONValue(raw) && json.Unmarshal(raw, &current.callID) != nil {
			return fmt.Errorf("Antigravity functionCall id 无效")
		}
		if current.callID == "" {
			current.callID = fmt.Sprintf("call_%s_%d_%d", d.id, c.index, c.toolCount)
		}
		if _, exists := c.callIDs[current.callID]; exists {
			return fmt.Errorf("Antigravity 返回重复 functionCall id，无法区分完整调用与增量")
		}
		c.callIDs[current.callID] = struct{}{}
		argsRaw, hasArgs := call["args"]
		if !hasArgs {
			argsRaw = json.RawMessage(`{}`)
		}
		var args map[string]json.RawMessage
		if json.Unmarshal(argsRaw, &args) != nil || args == nil {
			return fmt.Errorf("Antigravity functionCall args 不是 object")
		}
		if err := d.reserve(len(argsRaw) + len(current.callID) + len(current.name)); err != nil {
			return err
		}
		current.args = append(json.RawMessage(nil), argsRaw...)
		c.layout[current.index].CallID = current.callID
		c.toolCount++
	} else {
		current.length += len(text)
		c.layout[current.index].TextLength = current.length
		if d.collects() {
			// Builder 扩容及非流式 Chat 合并正文也属于保留预算。
			copies := 2
			if d.context.Dialect == DialectChat {
				copies = 3
			}
			if err := d.reserve(copies * len(text)); err != nil {
				return err
			}
			current.text.WriteString(text)
		}
	}
	if len(signature) > 0 {
		if d.context.Dialect == DialectClaude && kind == "text" {
			return fmt.Errorf("Claude text block 无法表示附着签名")
		}
		if err := d.reserve(len(signature) * 2); err != nil {
			return err
		}
		current.signature = signature
		c.layout[current.index].Signature = signature
		c.signed = true
	}
	if d.stream {
		return d.emitPart(c, current, text, frames)
	}
	return nil
}

func (d *ResponseDecoder) collects() bool { return !d.stream || d.context.Dialect == DialectResponses }

func (d *ResponseDecoder) checkFrames(frames [][]byte) error {
	remaining := MaxRetainedBytes - d.retained
	for _, frame := range frames {
		if len(frame) > remaining {
			return fmt.Errorf("Antigravity 响应编码超过转换容量")
		}
		remaining -= len(frame)
	}
	return nil
}

func (d *ResponseDecoder) Finish() (frames [][]byte, body []byte, err error) {
	if d.failed != nil {
		return nil, nil, d.failed
	}
	if d.finished {
		return nil, nil, fmt.Errorf("Antigravity 响应已结束")
	}
	defer func() {
		if err == nil {
			err = d.checkFrames(frames)
		}
		if err != nil {
			d.failed = err
			frames, body = nil, nil
		}
	}()
	if err = d.native.Finish(); err != nil {
		return nil, nil, err
	}
	if len(d.candidates) == 0 && d.native.blocked {
		d.candidates[0] = &responseCandidate{index: 0, finish: "SAFETY"}
	}
	indices := make([]int, 0, len(d.candidates))
	for index := range d.candidates {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		if err = d.closeBlock(d.candidates[index], &frames); err != nil {
			return nil, nil, err
		}
	}
	if d.stream {
		if err = d.finishStream(indices, &frames); err != nil {
			return nil, nil, err
		}
	} else {
		var response map[string]any
		if response, err = d.responseObject(indices); err != nil {
			return nil, nil, err
		}
		body, err = json.Marshal(response)
		if err != nil {
			return nil, nil, err
		}
		if len(body)+d.retained > MaxRetainedBytes {
			return nil, nil, fmt.Errorf("Antigravity 聚合输出超过转换容量")
		}
	}
	d.finished = true
	return frames, body, nil
}

func mappedFinish(reason string, tools bool, dialect Dialect) (string, error) {
	switch reason {
	case "STOP":
		if dialect == DialectClaude {
			if tools {
				return "tool_use", nil
			}
			return "end_turn", nil
		}
		if tools {
			return "tool_calls", nil
		}
		return "stop", nil
	case "MAX_TOKENS":
		if dialect == DialectClaude {
			return "max_tokens", nil
		}
		return "length", nil
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT":
		if dialect == DialectClaude {
			return "refusal", nil
		}
		return "content_filter", nil
	default:
		return "", fmt.Errorf("Antigravity finishReason %q 没有成功响应映射", reason)
	}
}
