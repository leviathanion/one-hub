package config

import (
	"fmt"

	"github.com/spf13/viper"
)

const defaultRealtimeWSConnectPerUserPerMinute = 30

func RealtimeWSConnectPerUserPerMinute() (int, error) {
	const key = "realtime_ws.connect_per_user_per_minute"
	if !viper.IsSet(key) {
		return defaultRealtimeWSConnectPerUserPerMinute, nil
	}
	limit := viper.GetInt(key)
	if limit <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return limit, nil
}
