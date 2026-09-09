package model

import (
	"fmt"
	"strings"

	"gorm.io/datatypes"
	"one-api/common/config"
	"one-api/common/providerendpoint"
)

// NewCustomEndpointPlugin 创建显式的新渠道预设，读取持久化配置时不得调用。
func NewCustomEndpointPlugin() *datatypes.JSONType[PluginType] {
	endpoints := make(map[string]any)
	for _, definition := range providerendpoint.Definitions() {
		endpoints[definition.ID] = (providerendpoint.Setting{Enabled: definition.DefaultEnabled}).Data()
	}
	plugin := datatypes.NewJSONType(PluginType{"endpoints": endpoints})
	return &plugin
}

func (channel *Channel) EndpointSettings() map[string]any {
	if channel == nil || channel.Plugin == nil {
		return nil
	}
	return channel.Plugin.Data()["endpoints"]
}

func (channel *Channel) ResolveEndpoint(id string) (string, error) {
	if err := channel.rejectLegacyEndpointConfig(); err != nil {
		return "", err
	}
	return providerendpoint.Resolve(channel.EndpointSettings(), id)
}

func (channel *Channel) rejectLegacyEndpointConfig() error {
	if channel == nil || channel.Plugin == nil {
		return nil
	}
	for _, key := range []string{"customize", "claude"} {
		if _, exists := channel.Plugin.Data()[key]; exists {
			return fmt.Errorf("旧 plugin.%s 配置已停止支持，请完成渠道接口迁移并改用 plugin.endpoints", key)
		}
	}
	return nil
}

func (channel *Channel) validateEndpointConfig() error {
	if err := channel.rejectLegacyEndpointConfig(); err != nil {
		return err
	}
	for id := range channel.EndpointSettings() {
		if _, ok := providerendpoint.Find(id); !ok {
			return fmt.Errorf("未知渠道上游接口 %q", id)
		}
		setting, err := providerendpoint.Read(channel.EndpointSettings(), id)
		if err != nil {
			return err
		}
		if IsOpenAIDataResidencyBaseURL(setting.UpstreamURL) {
			return fmt.Errorf("OpenAI data-residency endpoints are not supported")
		}
	}
	return nil
}

func (channel *Channel) CustomClaudeRelayEnabled() bool {
	if channel == nil || channel.Type != config.ChannelTypeCustom {
		return false
	}
	uri, err := channel.ResolveEndpoint(providerendpoint.Messages)
	return err == nil && uri != ""
}

func (channel *Channel) CustomClaudeEndpointIdentity() (string, error) {
	if channel == nil || channel.Type != config.ChannelTypeCustom {
		return "", nil
	}
	uri, err := channel.ResolveEndpoint(providerendpoint.Messages)
	if err != nil || uri == "" {
		return "", err
	}
	baseURL := strings.TrimSpace(channel.GetBaseURL())
	if baseURL == "" {
		baseURL = providerendpoint.DefaultClaudeBaseURL
	}
	return providerendpoint.CustomRequestURL(baseURL, uri), nil
}

// 每个上游地址都携带渠道凭据；停用也不授权后台改写已保存的连接目标。
func validateConfiguredEndpointTargets(persisted, candidate *Channel) error {
	for _, definition := range providerendpoint.Definitions() {
		oldTarget, err := persisted.configuredEndpointTarget(definition)
		if err != nil {
			return err
		}
		newTarget, err := candidate.configuredEndpointTarget(definition)
		if err != nil {
			return err
		}
		if oldTarget != newTarget {
			return fmt.Errorf("渠道 %s 上游地址属于连接配置，需要管理员明确编辑", definition.Label)
		}
	}
	return nil
}

func (channel *Channel) configuredEndpointTarget(definition providerendpoint.Definition) (string, error) {
	setting, err := providerendpoint.Read(channel.EndpointSettings(), definition.ID)
	if err != nil {
		return "", err
	}
	baseURL := channel.GetBaseURL()
	if definition.ID == providerendpoint.Messages {
		baseURL = strings.TrimSpace(baseURL)
		if baseURL == "" {
			baseURL = providerendpoint.DefaultClaudeBaseURL
		}
	}
	return providerendpoint.CustomRequestURL(baseURL, setting.TargetURI(definition)), nil
}
