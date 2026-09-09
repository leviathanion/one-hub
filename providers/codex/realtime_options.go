package codex

import (
	"time"
)

const (
	defaultExecutionSessionTTL       = 10 * time.Minute
	defaultExecutionSessionCap       = 4096
	defaultExecutionSessionCallerCap = 256
)

func (p *CodexProvider) getExecutionSessionTTL() time.Duration {
	options := p.getChannelOptions()
	if options == nil || options.ExecutionSessionTTLSeconds <= 0 {
		return defaultExecutionSessionTTL
	}
	return time.Duration(options.ExecutionSessionTTLSeconds) * time.Second
}
