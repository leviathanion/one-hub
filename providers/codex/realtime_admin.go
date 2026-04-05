package codex

import (
	runtimesession "one-api/runtime/session"
)

func GetExecutionSessionStats() runtimesession.Stats {
	return codexExecutionSessions.Stats()
}
