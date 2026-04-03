package codex

import (
	"strings"
	"time"
)

const (
	defaultExecutionSessionTTL       = 10 * time.Minute
	defaultWebsocketRetryCooldown    = 2 * time.Minute
	defaultExecutionSessionCap       = 4096
	defaultExecutionSessionCallerCap = 256
)

func (p *CodexProvider) getWebsocketMode() string {
	options := p.getChannelOptions()
	if options == nil {
		return codexWebsocketModeAuto
	}

	switch strings.ToLower(strings.TrimSpace(options.WebsocketMode)) {
	case "", codexWebsocketModeAuto:
		return codexWebsocketModeAuto
	case codexWebsocketModeForce:
		return codexWebsocketModeForce
	case codexWebsocketModeOff:
		return codexWebsocketModeOff
	default:
		return codexWebsocketModeAuto
	}
}

func (p *CodexProvider) getExecutionSessionTTL() time.Duration {
	options := p.getChannelOptions()
	if options == nil || options.ExecutionSessionTTLSeconds <= 0 {
		return defaultExecutionSessionTTL
	}
	return time.Duration(options.ExecutionSessionTTLSeconds) * time.Second
}

func (p *CodexProvider) getWebsocketRetryCooldown() time.Duration {
	options := p.getChannelOptions()
	if options == nil || options.WebsocketRetryCooldownSeconds <= 0 {
		return defaultWebsocketRetryCooldown
	}
	return time.Duration(options.WebsocketRetryCooldownSeconds) * time.Second
}
