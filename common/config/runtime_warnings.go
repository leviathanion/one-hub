package config

import (
	"sync"

	"one-api/common/logger"

	"github.com/spf13/viper"
)

var (
	runtimeConfigWarningsMu     sync.Mutex
	runtimeConfigWarningsLogged = map[string]bool{}
)

func LogRuntimeConfigWarnings() {
	logRuntimeConfigWarningOnce("responses_ws.bridge_open_timeout_ms.disabled", func() bool {
		return viper.IsSet("responses_ws.bridge_open_timeout_ms") && viper.GetInt("responses_ws.bridge_open_timeout_ms") <= 0
	}, "responses_ws.bridge_open_timeout_ms<=0 disables the ResponsesWS bridge opening watchdog; slow upstream compatibility improves, but the opening resource retention window is larger")
}

func logRuntimeConfigWarningOnce(key string, shouldLog func() bool, message string) {
	if shouldLog == nil || !shouldLog() {
		return
	}

	runtimeConfigWarningsMu.Lock()
	if runtimeConfigWarningsLogged[key] {
		runtimeConfigWarningsMu.Unlock()
		return
	}
	runtimeConfigWarningsLogged[key] = true
	runtimeConfigWarningsMu.Unlock()

	logger.SysWarn(message)
}
