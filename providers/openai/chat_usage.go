package openai

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"strings"

	"one-api/common/jsonobject"
	"one-api/common/requester"
	runtimesession "one-api/runtime/session"
	"one-api/types"
)

type streamUsageFilter uint8

const (
	keepStreamUsage streamUsageFilter = iota
	hideUsageTail
	hideCollectedUsage
)

func (p *OpenAIProvider) rawStreamUsageFilter(collectionEnabled, exposeUsage bool) streamUsageFilter {
	if exposeUsage {
		return keepStreamUsage
	}
	if collectionEnabled {
		return hideCollectedUsage
	}
	if !p.ProviderRawJSONReplay {
		// 兼容路径原有的展示约定仅隐藏纯用量尾帧。
		return hideUsageTail
	}
	return keepStreamUsage
}

// 每帧只提取一次计费及展示需要的字段，正文继续使用原始载荷。
type streamUsageFrame struct {
	raw                     []byte
	fields                  map[string]json.RawMessage
	choices                 []json.RawMessage
	usage, firstChoiceUsage *types.Usage
	providerError           *types.OpenAIErrorWithStatusCode
	hide, changedChoices    bool
}

func (f *streamUsageFrame) chatUsage() *types.Usage {
	if f.usage != nil {
		return f.usage
	}
	return f.firstChoiceUsage
}

func readStreamUsageFrame(payload []byte, filter streamUsageFilter) streamUsageFrame {
	f := streamUsageFrame{raw: payload}
	hide := filter != keepStreamUsage
	if err := jsonv2.Unmarshal(payload, &f.fields); err != nil {
		// 重名等歧义输入只按原有解码规则观察，不改写上游帧。
		f.fields = nil
		if json.Unmarshal(payload, &f.fields) != nil {
			f.fields = nil
			return f
		}
		hide = false
	}
	if rawError := f.fields["error"]; len(rawError) > 0 && string(rawError) != "null" {
		f.providerError = runtimesession.OpenAIErrorEnvelopeFromPayload(payload)
		hide = false
	}
	f.usage = decodeOpenAIStreamUsage(f.fields["usage"])
	if rawType := f.fields["type"]; len(rawType) > 0 && string(rawType) != "null" {
		hide = false
	}
	if rawObject, exists := f.fields["object"]; hide && exists {
		var kind string
		hide = json.Unmarshal(rawObject, &kind) == nil && (kind == "chat.completion.chunk" || kind == "text_completion")
	}
	if !hide && f.usage != nil {
		return f
	}
	if json.Unmarshal(f.fields["choices"], &f.choices) != nil || f.choices == nil {
		return f
	}
	if filter == hideUsageTail && (f.usage == nil || len(f.choices) != 0 || !f.onlyUsageMetadata()) {
		hide = false
	}
	f.hide = hide
	for i, raw := range f.choices {
		if i > 0 && !f.hide {
			break
		}
		var choice struct {
			Usage json.RawMessage `json:"usage,case:strict"`
		}
		if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &choice) != nil {
			f.hide = false
			continue
		}
		usage := decodeOpenAIStreamUsage(choice.Usage)
		if i == 0 {
			f.firstChoiceUsage = usage
		}
		if f.hide && (usage != nil || string(choice.Usage) == "null") {
			f.choices[i] = withoutStreamUsage(raw)
			f.changedChoices = true
		}
	}
	return f
}

func isChatMessageEvent(event string) bool {
	var name string
	for len(event) > 0 {
		line, rest := requester.SplitSSELine(event)
		event = rest
		field, value, _ := strings.Cut(requester.SSELineContent(line), ":")
		if field == "event" {
			name = strings.TrimPrefix(value, " ")
		}
	}
	return name == "" || name == "message"
}

// embed 接收除 usage 之外的原始成员，保留顺序、扩展值与数值精度。
func withoutStreamUsage(raw []byte) []byte {
	var projection struct {
		Usage json.RawMessage `json:"usage"`
		Rest  jsontext.Value  `json:",embed"`
	}
	if jsonv2.Unmarshal(raw, &projection) != nil {
		return raw
	}
	if len(projection.Rest) == 0 {
		return []byte("{}")
	}
	return projection.Rest
}

func (f *streamUsageFrame) hideUsage(event string) (string, bool) {
	if f.usage != nil && len(f.choices) == 0 && f.onlyUsageMetadata() {
		// 移除纯用量 data，保留 id、retry 等连接控制信息。
		metadata := rewriteChatSSEData(event, nil)
		return metadata, metadata == ""
	}
	removeTop := f.usage != nil || string(f.fields["usage"]) == "null"
	if !f.changedChoices {
		if removeTop {
			return rewriteChatSSEData(event, withoutStreamUsage(f.raw)), false
		}
		return event, false
	}
	// 只有 choice 含 usage 时才需替换 choices 数组。
	object, err := jsonobject.Parse(f.raw)
	if err != nil || object.SetJSON("choices", f.choices) != nil {
		return event, false
	}
	if removeTop {
		object.Delete("usage")
	}
	patched, err := object.MarshalJSON()
	if err != nil {
		return event, false
	}
	return rewriteChatSSEData(event, patched), false
}

func (f *streamUsageFrame) onlyUsageMetadata() bool {
	for key, value := range f.fields {
		switch key {
		case "id", "object", "created", "model", "choices", "usage", "system_fingerprint", "service_tier":
		case "obfuscation":
			if len(value) == 0 || value[0] != '"' {
				return false
			}
		default:
			return false
		}
	}
	return true
}

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

// rewriteChatSSEData 合并 data 行为一个 JSON 载荷。payload 为 nil 时移除所有 data 行。
func rewriteChatSSEData(event string, payload []byte) string {
	var replacement bytes.Buffer
	if payload != nil && json.Compact(&replacement, payload) != nil {
		return event
	}
	var output strings.Builder
	hasContent := false
	for len(event) > 0 {
		line, rest := requester.SplitSSELine(event)
		event = rest
		content := requester.SSELineContent(line)
		field, _, _ := strings.Cut(content, ":")
		if field == "data" {
			if replacement.Len() == 0 {
				continue
			}
			output.WriteString("data: ")
			output.Write(replacement.Bytes())
			replacement.Reset()
			line = line[len(content):]
		}
		if line == "\n" && strings.HasSuffix(output.String(), "\r") {
			// 移除 data 行后，独立的空行仍需与前一行的裸 CR 分开。
			output.WriteByte('\r')
		}
		output.WriteString(line)
		hasContent = hasContent || content != ""
	}
	if !hasContent && payload == nil {
		return ""
	}
	return output.String()
}
