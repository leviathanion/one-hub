package config

import (
	"time"

	"github.com/spf13/viper"
)

func RealtimeWebsocketReadLimit() int64 {
	limit := viper.GetInt64("realtime.websocket_read_limit")
	if limit <= 0 {
		return 32 << 20
	}
	return limit
}

func RealtimeWebsocketWriteTimeout() time.Duration {
	timeoutMS := viper.GetInt("realtime.websocket_write_timeout_ms")
	if timeoutMS <= 0 {
		return 10 * time.Second
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

func RealtimeWebsocketPingInterval() time.Duration {
	if !viper.IsSet("realtime.websocket_ping_interval_ms") {
		return 25 * time.Second
	}
	intervalMS := viper.GetInt("realtime.websocket_ping_interval_ms")
	if intervalMS <= 0 {
		return 0
	}
	return time.Duration(intervalMS) * time.Millisecond
}

func ResponsesWSFirstFrameTimeout() time.Duration {
	timeoutMS := viper.GetInt("responses_ws.first_frame_timeout_ms")
	if timeoutMS <= 0 {
		return 30 * time.Second
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

func ResponsesWSIdleTimeout() time.Duration {
	timeoutMS := viper.GetInt("responses_ws.idle_timeout_ms")
	if timeoutMS <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

func ResponsesWSMaxLifetime() time.Duration {
	timeoutMS := viper.GetInt("responses_ws.max_lifetime_ms")
	if timeoutMS <= 0 {
		return time.Hour
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

func ResponsesWSPendingProviderEventsMaxBytes() int {
	limit := viper.GetInt("responses_ws.pending_provider_events_max_bytes")
	if limit <= 0 {
		return 2 << 20
	}
	return limit
}
