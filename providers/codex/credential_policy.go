package codex

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"one-api/common/codexpolicy"
	"one-api/common/config"
	"one-api/common/requestctx"
	"one-api/model"
	"one-api/providers/codex/wire"
)

type codexOfficialPolicyConfig struct {
	FedRAMP                bool
	Residency              string
	DefaultOriginator      string
	DefaultUserAgent       string
	TrustClientAttestation bool
	AutoGenerate           wire.AutoGeneratePolicy
}

func (p *CodexProvider) codexOfficialChannelPolicy() (wire.ChannelPolicy, error) {
	channel := p.codexChannel()
	if channel != nil && !codexModelHeadersEmpty(channel.ModelHeaders) {
		return wire.ChannelPolicy{}, fmt.Errorf("model_headers is not supported for Codex channels; clear it and use other.codex structured policy")
	}
	return p.codexChannelPolicyFor(channel)
}

// 缓存只拥有 other.codex 的解析结果；Responses 专属约束在调用边界检查，
// 避免 realtime/usage 共用缓存时意外继承 model_headers 禁用规则。
func (p *CodexProvider) codexChannelPolicyFor(channel *model.Channel) (wire.ChannelPolicy, error) {
	if p == nil {
		return wire.ChannelPolicy{}, nil
	}
	cacheKey := codexChannelPolicyCacheKey(channel)
	p.channelPolicyMu.Lock()
	defer p.channelPolicyMu.Unlock()
	if p.channelPolicyLoaded && p.channelPolicyKey == cacheKey {
		return p.channelPolicy, p.channelPolicyErr
	}
	policy, err := parseCodexChannelPolicy(channel)
	p.channelPolicyLoaded = true
	p.channelPolicyKey = cacheKey
	p.channelPolicy = policy
	p.channelPolicyErr = err
	return policy, err
}

func codexChannelPolicyCacheKey(channel *model.Channel) string {
	if channel == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d|%s", channel.Id, channel.Other)
}

func parseCodexChannelPolicy(channel *model.Channel) (wire.ChannelPolicy, error) {
	policy := wire.ChannelPolicy{}
	if channel == nil {
		return policy, nil
	}
	if strings.TrimSpace(channel.Other) == "" {
		return policy, nil
	}
	other, err := channel.GetOtherMap()
	if err != nil {
		return policy, err
	}
	raw, ok := other["codex"]
	if !ok || len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return policy, nil
	}
	cfg, err := parseCodexOfficialPolicyConfig(raw)
	if err != nil {
		return policy, err
	}

	policy.FedRAMP = cfg.FedRAMP
	policy.Residency = strings.TrimSpace(cfg.Residency)
	if policy.Residency != "" && !codexpolicy.ValidResidency(policy.Residency) {
		return policy, fmt.Errorf("other.codex.residency is invalid")
	}
	if !codexpolicy.ValidClientIdentityHeader(cfg.DefaultOriginator) {
		return policy, fmt.Errorf("other.codex.default_originator is invalid")
	}
	policy.DefaultOriginator = strings.TrimSpace(cfg.DefaultOriginator)
	policy.TrustClientAttestation = cfg.TrustClientAttestation
	policy.DefaultUserAgent = strings.TrimSpace(cfg.DefaultUserAgent)
	if !codexpolicy.ValidClientIdentityHeader(cfg.DefaultUserAgent) {
		return policy, fmt.Errorf("other.codex.default_user_agent is invalid")
	}
	policy.AutoGenerate = cfg.AutoGenerate
	return policy, nil
}

func parseCodexOfficialPolicyConfig(raw json.RawMessage) (codexOfficialPolicyConfig, error) {
	var cfg codexOfficialPolicyConfig
	nested := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &nested); err != nil || nested == nil {
		if err == nil {
			err = fmt.Errorf("other.codex must be a JSON object")
		}
		return cfg, err
	}
	for key := range nested {
		if !codexpolicy.KnownKey(key) {
			return cfg, fmt.Errorf("other.codex.%s is not supported", key)
		}
	}

	for key, value := range nested {
		switch key {
		case codexpolicy.KeyFedRAMP:
			if err := json.Unmarshal(value, &cfg.FedRAMP); err != nil {
				return cfg, fmt.Errorf("other.codex.%s must be a boolean: %w", key, err)
			}
		case codexpolicy.KeyResidency:
			if err := json.Unmarshal(value, &cfg.Residency); err != nil {
				return cfg, fmt.Errorf("other.codex.%s must be a string: %w", key, err)
			}
		case codexpolicy.KeyDefaultOriginator:
			if err := json.Unmarshal(value, &cfg.DefaultOriginator); err != nil {
				return cfg, fmt.Errorf("other.codex.%s must be a string: %w", key, err)
			}
		case codexpolicy.KeyDefaultUserAgent:
			if err := json.Unmarshal(value, &cfg.DefaultUserAgent); err != nil {
				return cfg, fmt.Errorf("other.codex.%s must be a string: %w", key, err)
			}
		case codexpolicy.KeyTrustClientAttestation:
			if err := json.Unmarshal(value, &cfg.TrustClientAttestation); err != nil {
				return cfg, fmt.Errorf("other.codex.%s must be a boolean: %w", key, err)
			}
		case codexpolicy.KeyAutoGenerate:
			autoGenerate, err := parseCodexOfficialAutoGenerateConfig(value)
			if err != nil {
				return cfg, err
			}
			cfg.AutoGenerate = autoGenerate
		}
	}
	return cfg, nil
}

func parseCodexOfficialAutoGenerateConfig(raw json.RawMessage) (wire.AutoGeneratePolicy, error) {
	var policy wire.AutoGeneratePolicy
	nested := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &nested); err != nil || nested == nil {
		if err == nil {
			err = fmt.Errorf("other.codex.auto_generate must be a JSON object")
		}
		return policy, err
	}
	for key, value := range nested {
		if !codexpolicy.KnownAutoGenerateKey(key) {
			return policy, fmt.Errorf("other.codex.auto_generate.%s is not supported", key)
		}
		var enabled bool
		if err := json.Unmarshal(value, &enabled); err != nil {
			return policy, fmt.Errorf("other.codex.auto_generate.%s must be a boolean: %w", key, err)
		}
		switch key {
		case codexpolicy.AutoGenerateSessionID:
			policy.SessionID = enabled
		case codexpolicy.AutoGenerateThreadID:
			policy.ThreadID = enabled
		case codexpolicy.AutoGenerateClientRequestID:
			policy.ClientRequestID = enabled
		case codexpolicy.AutoGenerateInstallationID:
			policy.InstallationID = enabled
		case codexpolicy.AutoGenerateWSStreamRequestStartMS:
			policy.WSStreamRequestStartMS = enabled
		}
	}
	return policy, nil
}

func (p *CodexProvider) codexPrincipalFingerprint(principal requestctx.Principal) (wire.PrincipalFingerprint, error) {
	secret := strings.TrimSpace(config.CodexIdentitySecret)
	if secret == "" {
		return wire.PrincipalFingerprint{}, fmt.Errorf("codex_identity_secret is required when other.codex.auto_generate.installation_id is enabled")
	}
	if principal.IsZero() {
		return wire.PrincipalFingerprint{}, nil
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(principal.Kind + ":" + principal.StableID))
	return wire.PrincipalFingerprint{
		Kind: principal.Kind,
		HMAC: hex.EncodeToString(mac.Sum(nil)),
	}, nil
}

func codexModelHeadersEmpty(raw *string) bool {
	if raw == nil {
		return true
	}
	trimmed := strings.TrimSpace(*raw)
	return trimmed == "" || trimmed == "{}" || trimmed == "null"
}

func (p *CodexProvider) codexAccountID() string {
	credentials := p.credentialsSnapshot()
	if credentials == nil {
		return ""
	}
	return credentials.AccountID
}
