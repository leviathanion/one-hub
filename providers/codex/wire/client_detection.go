package wire

import "strings"

// Codex app-server 将 clientInfo.name 写入 originator 和 UA 后缀。
// 名称依据 codex-rs first-party helpers、exec/SDK，以及 sub2api 已知扩展变体。
// 新客户端名称应按实际源码或请求样本补充，不使用裸 codex 子串识别。
func isCodexClientName(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "codex_cli_rs", "codex-cli", "codex-tui", "codex_vscode", "codex_vscode_copilot",
		"codex_app", "codex_chatgpt_desktop", "codex_atlas", "codex_exec", "codex_sdk_ts":
		return true
	}
	return strings.HasPrefix(value, "codex ") && strings.TrimSpace(strings.TrimPrefix(value, "codex ")) != ""
}

func isCodexClientIdentity(headers HeaderSnapshot) bool {
	for _, value := range headers.Values("originator") {
		if isCodexClientName(value) {
			return true
		}
	}
	for _, value := range headers.Values("User-Agent") {
		if isCodexUserAgent(value) {
			return true
		}
	}
	return false
}

func isCodexUserAgent(value string) bool {
	ua := strings.ToLower(strings.TrimSpace(value))
	// 产品名可位于复合 UA 中；按边界分词，避免 evil-codex_cli_rs/ 等误命中。
	products := strings.FieldsFunc(ua, func(c rune) bool {
		return c == ' ' || c == '\t' || strings.ContainsRune("();,[]", c)
	})
	for i, product := range products {
		name, _, _ := strings.Cut(product, "/")
		if isCodexClientName(name) {
			return true
		}
		if product == "codex" && i+1 < len(products) {
			if name, version, ok := strings.Cut(products[i+1], "/"); ok && version != "" && isCodexClientName("codex "+name) {
				return true
			}
		}
	}
	// originator override 不改 app-server 的最后一个 (clientInfo.name; version)。
	if start := strings.LastIndex(ua, "("); start >= 0 && strings.HasSuffix(ua, ")") {
		name, version, ok := strings.Cut(ua[start+1:len(ua)-1], ";")
		return ok && strings.TrimSpace(version) != "" && isCodexClientName(name)
	}
	return false
}
