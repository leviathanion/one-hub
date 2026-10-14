package antigravity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"one-api/common/jsonobject"
	"one-api/providers/claude"
	"one-api/types"
)

// preparedMediaMessage 只供定位已批准的媒体结果，不能成为请求的序列化来源。
type preparedMediaMessage struct {
	role    string
	content any
}

func patchClaudePreparedMedia(raw []byte, prepared *claude.ClaudeRequest) ([]byte, error) {
	var messages []preparedMediaMessage
	if prepared != nil {
		messages = make([]preparedMediaMessage, len(prepared.Messages))
		for i, message := range prepared.Messages {
			messages[i] = preparedMediaMessage{role: message.Role, content: message.Content}
		}
	}
	return patchPreparedMedia(raw, messages, true)
}

func patchChatPreparedMedia(raw []byte, prepared *types.ChatCompletionRequest) ([]byte, error) {
	var messages []preparedMediaMessage
	if prepared != nil {
		messages = make([]preparedMediaMessage, len(prepared.Messages))
		for i, message := range prepared.Messages {
			messages[i] = preparedMediaMessage{role: message.Role, content: message.Content}
		}
	}
	return patchPreparedMedia(raw, messages, false)
}

// 这里没有 I/O。prepared 仅提供共享 materializer 已生成的 source/url；
// 每个补丁仍以 canonical raw 为基底，不从 typed 投影重建消息或内容。
func patchPreparedMedia(raw []byte, prepared []preparedMediaMessage, nativeClaude bool) ([]byte, error) {
	root, err := jsonobject.Parse(raw)
	if err != nil {
		return nil, err
	}
	messages := mediaRawArray(root.Fields["messages"])
	changed := false
	for i, messageRaw := range messages {
		message := mediaRawObject(messageRaw)
		if message == nil || !hasPreparedMediaURL(message.Fields["content"], nativeClaude) {
			continue
		}
		path := fmt.Sprintf("messages[%d]", i)
		if len(messages) != len(prepared) || mediaRawString(message.Fields["role"]) != prepared[i].role {
			return nil, fmt.Errorf("%s does not match prepared media message", path)
		}
		// 只读取这段投影中的媒体字段，绝不把它作为最终 content 写回。
		preparedContent, err := json.Marshal(prepared[i].content)
		if err != nil {
			return nil, fmt.Errorf("%s prepared media: %w", path, err)
		}
		content, patched, err := patchPreparedMediaContent(message.Fields["content"], preparedContent, path+".content", nativeClaude)
		if err != nil {
			return nil, err
		}
		if !patched {
			continue
		}
		if err := message.SetRaw("content", content); err != nil {
			return nil, err
		}
		messages[i], err = message.MarshalJSON()
		if err != nil {
			return nil, err
		}
		changed = true
	}
	if !changed {
		return raw, nil
	}
	if err := root.SetJSON("messages", messages); err != nil {
		return nil, err
	}
	return root.MarshalJSON()
}

func patchPreparedMediaContent(raw, prepared json.RawMessage, path string, nativeClaude bool) ([]byte, bool, error) {
	blocks, preparedBlocks := mediaRawArray(raw), mediaRawArray(prepared)
	if len(blocks) != len(preparedBlocks) {
		return nil, false, fmt.Errorf("%s does not match prepared media content", path)
	}
	changed := false
	for i, blockRaw := range blocks {
		block := mediaRawObject(blockRaw)
		if block == nil {
			continue
		}
		kind := mediaRawKind(block.Fields["type"])
		isImage := hasMediaSourceURL(block, nativeClaude)
		isNested := nativeClaude && kind == claude.ContentTypeToolResult && hasPreparedMediaURL(block.Fields["content"], true)
		if !isImage && !isNested {
			continue
		}
		blockPath := fmt.Sprintf("%s[%d]", path, i)
		preparedBlock := mediaRawObject(preparedBlocks[i])
		if preparedBlock == nil || kind != mediaRawKind(preparedBlock.Fields["type"]) {
			return nil, false, fmt.Errorf("%s does not match prepared media block", blockPath)
		}
		var patched bool
		var err error
		if isNested {
			if mediaRawString(block.Fields["tool_use_id"]) != mediaRawString(preparedBlock.Fields["tool_use_id"]) {
				return nil, false, fmt.Errorf("%s does not match prepared tool result", blockPath)
			}
			var content []byte
			content, patched, err = patchPreparedMediaContent(block.Fields["content"], preparedBlock.Fields["content"], blockPath+".content", true)
			if err == nil && patched {
				err = block.SetRaw("content", content)
			}
		} else {
			patched, err = patchPreparedMediaSource(block, preparedBlock, blockPath, nativeClaude)
		}
		if err != nil {
			return nil, false, err
		}
		if patched {
			blocks[i], err = block.MarshalJSON()
			if err != nil {
				return nil, false, err
			}
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}
	encoded, err := json.Marshal(blocks)
	return encoded, true, err
}

func patchPreparedMediaSource(block, prepared *jsonobject.Object, path string, nativeClaude bool) (bool, error) {
	field := "image_url"
	if nativeClaude {
		field = "source"
	}
	source, replacement := mediaRawObject(block.Fields[field]), mediaRawObject(prepared.Fields[field])
	if source == nil || replacement == nil {
		return false, fmt.Errorf("%s.%s does not match prepared media source", path, field)
	}
	if !nativeClaude {
		url := mediaRawString(replacement.Fields["url"])
		if url == mediaRawString(source.Fields["url"]) {
			return false, nil
		}
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(url)), "data:") {
			return false, fmt.Errorf("%s.image_url.url is not prepared inline media", path)
		}
		if err := source.SetRaw("url", replacement.Fields["url"]); err != nil {
			return false, err
		}
	} else {
		switch mediaRawKind(replacement.Fields["type"]) {
		case "url":
			if mediaRawString(source.Fields["url"]) == mediaRawString(replacement.Fields["url"]) {
				return false, nil
			}
			return false, fmt.Errorf("%s.source.url does not match prepared media", path)
		case "base64":
			for _, name := range []string{"type", "media_type", "data"} {
				var value string
				if len(replacement.Fields[name]) == 0 || bytes.Equal(bytes.TrimSpace(replacement.Fields[name]), []byte("null")) || json.Unmarshal(replacement.Fields[name], &value) != nil {
					return false, fmt.Errorf("%s.source.%s is missing from prepared media", path, name)
				}
				if err := source.SetRaw(name, replacement.Fields[name]); err != nil {
					return false, err
				}
			}
			source.Delete("url")
		default:
			return false, fmt.Errorf("%s.source is not prepared base64 media", path)
		}
	}
	encoded, err := source.MarshalJSON()
	if err != nil {
		return false, err
	}
	return true, block.SetRaw(field, encoded)
}

func hasPreparedMediaURL(raw json.RawMessage, nativeClaude bool) bool {
	for _, blockRaw := range mediaRawArray(raw) {
		block := mediaRawObject(blockRaw)
		if block == nil {
			continue
		}
		if hasMediaSourceURL(block, nativeClaude) {
			return true
		}
		if nativeClaude && mediaRawKind(block.Fields["type"]) == claude.ContentTypeToolResult && hasPreparedMediaURL(block.Fields["content"], true) {
			return true
		}
	}
	return false
}

func hasMediaSourceURL(block *jsonobject.Object, nativeClaude bool) bool {
	if nativeClaude {
		source := mediaRawObject(block.Fields["source"])
		return mediaRawKind(block.Fields["type"]) == claude.ContentTypeImage && source != nil && mediaRawKind(source.Fields["type"]) == "url"
	}
	if mediaRawKind(block.Fields["type"]) != types.ContentTypeImageURL {
		return false
	}
	source := mediaRawObject(block.Fields["image_url"])
	if source == nil {
		return false
	}
	url := mediaRawString(source.Fields["url"])
	return url != "" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(url)), "data:")
}

func mediaRawArray(raw json.RawMessage) []json.RawMessage {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	return items
}

func mediaRawObject(raw json.RawMessage) *jsonobject.Object {
	object, _ := jsonobject.Parse(raw)
	return object
}

func mediaRawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func mediaRawKind(raw json.RawMessage) string {
	return strings.TrimSpace(mediaRawString(raw))
}
