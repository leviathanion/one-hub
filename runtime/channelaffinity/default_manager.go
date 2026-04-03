package channelaffinity

import "sync"

var (
	defaultManagerMu sync.Mutex
	defaultManager   *Manager
)

func DefaultManager() *Manager {
	return ensureDefaultManager(ManagerOptions{}, false)
}

func ConfigureDefault(options ManagerOptions) *Manager {
	return ensureDefaultManager(options, true)
}

func ensureDefaultManager(options ManagerOptions, updateRuntime bool) *Manager {
	defaultManagerMu.Lock()
	defer defaultManagerMu.Unlock()

	if defaultManager == nil {
		defaultManager = NewManagerWithOptions(options)
		return defaultManager
	}
	if updateRuntime {
		defaultManager.UpdateOptions(options)
	}
	return defaultManager
}
