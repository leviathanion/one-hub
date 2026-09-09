package providerendpoint

import (
	"fmt"
	"net/url"
	"strings"

	"one-api/common/config"
)

const (
	Responses            = "openai.responses"
	Messages             = "anthropic.messages"
	DefaultMessagesURI   = "/v1/messages"
	DefaultClaudeBaseURL = "https://api.anthropic.com"
)

// Definition 是代理拥有的可配置上游接口目录；不声明上游参数或模型支持面。
type Definition struct {
	ID             string `json:"id"`
	Group          string `json:"group"`
	Label          string `json:"label"`
	DefaultPath    string `json:"default_path"`
	DefaultEnabled bool   `json:"default_enabled"`
	RelayMode      int    `json:"-"`
}

var definitions = []Definition{
	{"openai.chat_completions", "OpenAI API", "Chat Completions", "/v1/chat/completions", true, config.RelayModeChatCompletions},
	{"openai.completions", "OpenAI API", "Completions", "/v1/completions", true, config.RelayModeCompletions},
	{"openai.embeddings", "OpenAI API", "Embeddings", "/v1/embeddings", true, config.RelayModeEmbeddings},
	{"openai.moderations", "OpenAI API", "Moderations", "/v1/moderations", true, config.RelayModeModerations},
	{"openai.images_generations", "OpenAI API", "Images Generations", "/v1/images/generations", true, config.RelayModeImagesGenerations},
	{"openai.images_edits", "OpenAI API", "Images Edits", "/v1/images/edits", true, config.RelayModeImagesEdits},
	{"openai.images_variations", "OpenAI API", "Images Variations", "/v1/images/variations", true, config.RelayModeImagesVariations},
	{"openai.audio_speech", "OpenAI API", "Audio Speech", "/v1/audio/speech", true, config.RelayModeAudioSpeech},
	{"openai.audio_transcriptions", "OpenAI API", "Audio Transcriptions", "/v1/audio/transcriptions", true, config.RelayModeAudioTranscription},
	{"openai.audio_translations", "OpenAI API", "Audio Translations", "/v1/audio/translations", true, config.RelayModeAudioTranslation},
	{Responses, "OpenAI API", "Responses", DefaultResponsesURI, true, config.RelayModeResponses},
	{Messages, "Claude API", "Messages", DefaultMessagesURI, false, config.RelayModeUnknown},
}

func Definitions() []Definition { return append([]Definition(nil), definitions...) }

func Find(id string) (Definition, bool) {
	for _, definition := range definitions {
		if definition.ID == id {
			return definition, true
		}
	}
	return Definition{}, false
}

func ForRelayMode(mode int) (Definition, bool) {
	for _, definition := range definitions {
		if mode != config.RelayModeUnknown && definition.RelayMode == mode {
			return definition, true
		}
	}
	return Definition{}, false
}

type Setting struct {
	Enabled     bool   `json:"enabled"`
	UpstreamURL string `json:"upstream_url"`
}

func (s Setting) Data() map[string]any {
	return map[string]any{"enabled": s.Enabled, "upstream_url": s.UpstreamURL}
}

// Read 对缺失条目返回关闭；新建预设必须由调用者显式写入。
func Read(endpoints map[string]any, id string) (Setting, error) {
	raw, exists := endpoints[id]
	if !exists {
		return Setting{}, nil
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return Setting{}, fmt.Errorf("plugin.endpoints[%q] 必须是对象", id)
	}
	enabled, ok := fields["enabled"].(bool)
	if !ok {
		return Setting{}, fmt.Errorf("plugin.endpoints[%q].enabled 必须是布尔值", id)
	}
	upstreamURL := ""
	if rawURL, exists := fields["upstream_url"]; exists {
		upstreamURL, ok = rawURL.(string)
		if !ok {
			return Setting{}, fmt.Errorf("plugin.endpoints[%q].upstream_url 必须是字符串", id)
		}
	}
	for field := range fields {
		if field != "enabled" && field != "upstream_url" {
			return Setting{}, fmt.Errorf("plugin.endpoints[%q] 包含未知配置字段 %q", id, field)
		}
	}
	if err := ValidateUpstreamURL(upstreamURL); err != nil {
		return Setting{}, fmt.Errorf("plugin.endpoints[%q].upstream_url: %w", id, err)
	}
	return Setting{Enabled: enabled, UpstreamURL: upstreamURL}, nil
}

func ValidateUpstreamURL(raw string) error {
	if raw == "" {
		return nil
	}
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || value == "" {
		return fmt.Errorf("必须是以 / 开头的路径或完整 HTTP(S) URL")
	}
	if parsed.User != nil || parsed.Fragment != "" || strings.Contains(value, "#") {
		return fmt.Errorf("地址不能包含用户凭据或 fragment")
	}
	if parsed.IsAbs() {
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return fmt.Errorf("必须是完整 HTTP(S) URL")
		}
	} else if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return fmt.Errorf("路径必须以单个 / 开头")
	}
	// 保留原始路径、查询和空白；HTTP/WS 的既有拼接规则分别拥有 wire 语义。
	return nil
}

func Resolve(endpoints map[string]any, id string) (string, error) {
	definition, ok := Find(id)
	if !ok {
		return "", fmt.Errorf("未知上游接口 %q", id)
	}
	setting, err := Read(endpoints, id)
	if err != nil || !setting.Enabled {
		return "", err
	}
	return setting.TargetURI(definition), nil
}

// TargetURI 不解释启用状态，用于请求构造和已保存连接目标的身份比较。
func (s Setting) TargetURI(definition Definition) string {
	if s.UpstreamURL == "" {
		return definition.DefaultPath
	}
	return s.UpstreamURL
}
