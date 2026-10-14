package codex

import (
	"bytes"
	"encoding/json"
	"strings"
)

func (CodexProviderFactory) WholeCredentialInput() bool { return true }

func (CodexProviderFactory) PrepareCredentialInput(key string) (string, error) {
	// 仅规范化完整 JSON 的空白，保留已有 raw token 和格式错误的处理契约。
	if strings.HasPrefix(strings.TrimSpace(key), "{") {
		var compact bytes.Buffer
		if json.Compact(&compact, []byte(key)) == nil {
			return compact.String(), nil
		}
	}
	return key, nil
}
