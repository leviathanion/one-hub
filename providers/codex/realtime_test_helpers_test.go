package codex

import (
	"testing"

	runtimesession "one-api/runtime/session"
)

func replaceCodexExecutionSessionsForTest(t *testing.T, manager *runtimesession.Manager) {
	t.Helper()
	codexExecutionSessionsMu.Lock()
	originalManager := codexExecutionSessions
	codexExecutionSessions = manager
	codexExecutionSessionsMu.Unlock()
	t.Cleanup(func() {
		codexExecutionSessionsMu.Lock()
		codexExecutionSessions = originalManager
		codexExecutionSessionsMu.Unlock()
		if manager != nil {
			manager.Close()
		}
	})
}
