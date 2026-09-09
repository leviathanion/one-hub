package config

import (
	"testing"

	"github.com/spf13/viper"
)

func TestRealtimeConnectionLimitDefaultsAndValidation(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	defaultConfig()
	if got, err := RealtimeWSConnectPerUserPerMinute(); err != nil || got != 30 {
		t.Fatalf("default connection limit = %d, %v", got, err)
	}
	for _, value := range []int{0, -1, -30} {
		viper.Set("realtime_ws.connect_per_user_per_minute", value)
		if _, err := RealtimeWSConnectPerUserPerMinute(); err == nil {
			t.Fatalf("explicit limit %d must be rejected", value)
		}
		if err := InitConf(); err == nil {
			t.Fatalf("startup accepted invalid connection limit %d", value)
		}
	}
	viper.Set("realtime_ws.connect_per_user_per_minute", 7)
	if got, err := RealtimeWSConnectPerUserPerMinute(); err != nil || got != 7 {
		t.Fatalf("configured connection limit = %d, %v", got, err)
	}
}
