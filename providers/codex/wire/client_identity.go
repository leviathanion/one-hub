package wire

import (
	"fmt"
	"runtime"
	"strings"

	"one-api/common/codexpolicy"
)

const DefaultOriginator = "pi"

var piUserAgent = formatPiUserAgent(runtime.GOOS, osRelease(), runtime.GOARCH)

// DefaultUserAgent 对齐 PI getPiUserAgent 的 Node 平台和架构命名。
func DefaultUserAgent() string { return piUserAgent }

func formatPiUserAgent(platform, release, arch string) string {
	if platform == "windows" {
		platform = "win32"
	}
	switch arch {
	case "amd64":
		arch = "x64"
	case "ppc64le":
		arch = "ppc64"
	case "386":
		arch = "ia32"
	}
	return fmt.Sprintf("pi (%s %s; %s)", platform, release, arch)
}

// ResolveClientIdentity 只透传 Codex 客户端提供的身份，缺失项不补齐；
// 其他请求逐字段使用渠道配置或 PI 默认值。识别仅用于兼容策略，不是认证。
func ResolveClientIdentity(headers HeaderSnapshot, policy ChannelPolicy) (Identity, []Decision, error) {
	identity := Identity{Sources: make(map[string]Source)}
	decisions := make([]Decision, 0, 2)
	names := []string{"User-Agent", "originator"}
	passthroughClient := isCodexClientIdentity(headers)
	values := []*string{&identity.UserAgent, &identity.Originator}
	fallbacks := []string{policy.DefaultUserAgent, policy.DefaultOriginator}
	defaults := []string{DefaultUserAgent(), DefaultOriginator}
	for i, name := range names {
		if passthroughClient {
			for _, raw := range headers.Values(name) {
				if !codexpolicy.ValidClientIdentityHeader(raw) {
					return Identity{}, nil, reject(name, "invalid client identity header")
				}
			}
			field := headers.Singleton(name, codexpolicy.ValidClientIdentityHeader)
			if field.State == FieldInvalid || field.State == FieldMultiple {
				return Identity{}, nil, reject(name, "invalid or multiple client identity header values")
			}
			if field.State == FieldPresent {
				*values[i] = field.Value
				identity.Sources[name] = SourceClientHeader
				decisions = append(decisions, valueDecision(name, "copy", SourceClientHeader, "client-identity-present", field.Value))
			} else {
				decisions = append(decisions, valueDecision(name, "omit", "", "partial-client-identity", ""))
			}
			continue
		}
		value, source := strings.TrimSpace(fallbacks[i]), SourceChannel
		if value == "" {
			value, source = defaults[i], SourceProtocol
		}
		if !codexpolicy.ValidClientIdentityHeader(value) {
			return Identity{}, nil, reject(name, "invalid default client identity header")
		}
		*values[i], identity.Sources[name] = value, source
		decisions = append(decisions, valueDecision(name, "fallback", source, "non-codex-or-missing-client-identity", value))
	}
	return identity, decisions, nil
}
