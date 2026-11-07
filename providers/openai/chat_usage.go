package openai

import (
	"bytes"
	"encoding/json"
	"strings"

	"one-api/common/jsonobject"
	"one-api/common/requester"
	"one-api/types"
)

// 只写入 adapter 实际修改的用量字段，避免 DTO 默认零值或嵌套对象覆盖上游扩展。
func patchChatPayloadUsage(raw []byte, usage *types.Usage) ([]byte, error) {
	object, err := jsonobject.Parse(raw)
	if err != nil {
		return nil, err
	}
	if original := decodeOpenAIStreamUsage(object.Fields["usage"]); original != nil {
		before, err := json.Marshal(original)
		if err != nil {
			return nil, err
		}
		after, err := json.Marshal(usage)
		if err != nil {
			return nil, err
		}
		patched, err := patchChangedUsageFields(object.Fields["usage"], before, after)
		if err != nil || bytes.Equal(patched, object.Fields["usage"]) {
			return raw, err
		}
		if err := object.SetRaw("usage", patched); err != nil {
			return nil, err
		}
		return object.MarshalJSON()
	}
	// 部分兼容端点把 usage 放在第一个 choice 中；保持其原有位置。
	var choices []json.RawMessage
	if json.Unmarshal(object.Fields["choices"], &choices) != nil || len(choices) == 0 {
		return raw, nil
	}
	patched, err := patchChatPayloadUsage(choices[0], usage)
	if err != nil || bytes.Equal(patched, choices[0]) {
		return raw, err
	}
	choices[0] = patched
	encoded, err := json.Marshal(choices)
	if err != nil {
		return nil, err
	}
	if err := object.SetRaw("choices", encoded); err != nil {
		return nil, err
	}
	return object.MarshalJSON()
}

func patchChangedUsageFields(raw, before, after []byte) ([]byte, error) {
	if bytes.Equal(before, after) {
		return raw, nil
	}
	var previous, current map[string]json.RawMessage
	if json.Unmarshal(before, &previous) != nil || previous == nil || json.Unmarshal(after, &current) != nil || current == nil {
		return after, nil
	}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = []byte("{}")
	}
	object, err := jsonobject.Parse(raw)
	if err != nil {
		return nil, err
	}
	for name, value := range current {
		if bytes.Equal(previous[name], value) {
			continue
		}
		patched, err := patchChangedUsageFields(object.Fields[name], previous[name], value)
		if err != nil {
			return nil, err
		}
		if err := object.SetRaw(name, patched); err != nil {
			return nil, err
		}
	}
	for name := range previous {
		if _, exists := current[name]; !exists {
			object.Delete(name)
		}
	}
	return object.MarshalJSON()
}

func replaceChatSSEData(event string, payload []byte) string {
	var compact bytes.Buffer
	if json.Compact(&compact, payload) != nil {
		return event
	}
	var output strings.Builder
	wroteData := false
	for len(event) > 0 {
		line, rest := requester.SplitSSELine(event)
		event = rest
		content := requester.SSELineContent(line)
		field, _, _ := strings.Cut(content, ":")
		if field != "data" {
			output.WriteString(line)
			continue
		}
		if !wroteData {
			// 同方言格式化仅重写 data；保留元字段、注释及原事件分隔符。
			// 多行 data 合并为单行，不改变 SSE 的逻辑 JSON 载荷。
			output.WriteString("data: ")
			output.Write(compact.Bytes())
			output.WriteString(line[len(content):])
			wroteData = true
		}
	}
	return output.String()
}
