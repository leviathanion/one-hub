package safty

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// 只提取解码后的字符串值供内容检查，不约束协议字段，也不重建转发字节。
func jsonContentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var text strings.Builder
	if err := appendJSONContentText(decoder, &text); err != nil {
		return "", err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			err = errors.New("content contains trailing JSON")
		}
		return "", err
	}
	return text.String(), nil
}

func appendJSONContentText(decoder *json.Decoder, text *strings.Builder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch value := token.(type) {
	case string:
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(value)
	case json.Delim:
		for decoder.More() {
			if value == '{' {
				// 键名属于结构；按 wire 顺序扫描每个值，包括重复键。
				if _, err := decoder.Token(); err != nil {
					return err
				}
			}
			if err := appendJSONContentText(decoder, text); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	return nil
}
