package model

import (
	"fmt"
	"strings"
)

// ChannelHeaderIdentity 是 model_headers 中不可变的资源命名空间绑定。
// 直接从该行的配置解析，避免再持久化一份可相互偏离的身份副本。
type ChannelHeaderIdentity struct {
	Organization string
	Project      string
}

func (channel *Channel) HeaderIdentity() (ChannelHeaderIdentity, error) {
	identity := ChannelHeaderIdentity{}
	headers, err := channel.GetModelHeadersMap()
	if err != nil {
		return identity, err
	}
	seen := make(map[string]string, 2)
	for name, value := range headers {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "openai-organization" && name != "openai-project" {
			continue
		}
		value = strings.TrimSpace(value)
		if previous, exists := seen[name]; exists && previous != value {
			return identity, fmt.Errorf("model_headers 中 %s 的大小写重复项冲突", name)
		}
		seen[name] = value
	}
	identity.Organization = seen["openai-organization"]
	identity.Project = seen["openai-project"]
	return identity, nil
}

// ApplyTo 先去掉可能已有的大小写别名，最终身份头只来自不可变绑定。
func (identity ChannelHeaderIdentity) ApplyTo(headers map[string]string) {
	for name := range headers {
		if strings.EqualFold(strings.TrimSpace(name), "OpenAI-Organization") || strings.EqualFold(strings.TrimSpace(name), "OpenAI-Project") {
			delete(headers, name)
		}
	}
	if identity.Organization != "" {
		headers["OpenAI-Organization"] = identity.Organization
	}
	if identity.Project != "" {
		headers["OpenAI-Project"] = identity.Project
	}
}
