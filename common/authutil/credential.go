package authutil

import (
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

const openAIInsecureAPIKeyProtocolPrefix = "openai-insecure-api-key."

type Credential struct {
	Value         string
	SelectorParts []string
}

func (c Credential) Empty() bool {
	return strings.TrimSpace(c.Value) == ""
}

func ParseCredential(raw string) Credential {
	value := strings.TrimSpace(raw)
	if value == "" {
		return Credential{}
	}

	value = trimBearerPrefix(value)
	value = trimInsensitivePrefix(value, "sk-")
	value = strings.TrimSpace(value)
	if value == "" {
		return Credential{}
	}

	parts := strings.Split(value, "#")
	credential := Credential{
		Value: strings.TrimSpace(parts[0]),
	}
	if len(parts) > 1 {
		credential.SelectorParts = make([]string, 0, len(parts)-1)
		for _, part := range parts[1:] {
			credential.SelectorParts = append(credential.SelectorParts, strings.TrimSpace(part))
		}
	}

	if credential.Empty() {
		return Credential{}
	}
	return credential
}

func ExtractOpenAIRequestCredential(req *http.Request) Credential {
	return extractRequestCredential(req, []string{"Authorization"}, true)
}

func ExtractStableRequestCredential(req *http.Request) Credential {
	return extractRequestCredential(req, []string{"Authorization", "x-api-key"}, true)
}

func ExtractOpenAIWebsocketCredential(req *http.Request) Credential {
	if req == nil {
		return Credential{}
	}

	for _, value := range req.Header.Values("Sec-WebSocket-Protocol") {
		for _, protocol := range strings.Split(value, ",") {
			protocol = strings.TrimSpace(protocol)
			if !strings.HasPrefix(strings.ToLower(protocol), openAIInsecureAPIKeyProtocolPrefix) {
				continue
			}
			return ParseCredential(protocol[len(openAIInsecureAPIKeyProtocolPrefix):])
		}
	}

	return Credential{}
}

func extractRequestCredential(req *http.Request, headerKeys []string, includeOpenAIWebsocket bool) Credential {
	if req == nil {
		return Credential{}
	}

	for _, key := range headerKeys {
		if credential := ParseCredential(req.Header.Get(key)); !credential.Empty() {
			return credential
		}
	}

	if includeOpenAIWebsocket && websocket.IsWebSocketUpgrade(req) {
		return ExtractOpenAIWebsocketCredential(req)
	}

	return Credential{}
}

func trimBearerPrefix(value string) string {
	if len(value) < len("Bearer") || !strings.EqualFold(value[:len("Bearer")], "Bearer") {
		return value
	}
	if len(value) == len("Bearer") {
		return ""
	}
	if !isASCIISpace(value[len("Bearer")]) {
		return value
	}
	return strings.TrimSpace(value[len("Bearer"):])
}

func trimInsensitivePrefix(value, prefix string) string {
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return value
	}
	return value[len(prefix):]
}

func isASCIISpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}
