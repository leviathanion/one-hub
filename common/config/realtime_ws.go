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

func RealtimeWebsocketClientPingInterval() time.Duration {
	return durationFromViperMS("realtime_websocket_client_ping_interval_ms", 25*time.Second, true)
}

func RealtimeWebsocketClientPongMissTimeout() time.Duration {
	return durationFromViperMS("realtime_websocket_client_pong_miss_timeout_ms", 0, true)
}

func RealtimeWebsocketClientInboundActivityTimeout() time.Duration {
	return durationFromViperMS("realtime_websocket_client_inbound_activity_timeout_ms", 0, true)
}

func ResponsesWSFirstFrameTimeout() time.Duration {
	timeoutMS := viper.GetInt("responses_ws.first_frame_timeout_ms")
	if timeoutMS <= 0 {
		return 30 * time.Second
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

func ResponsesWebsocketClientPingInterval() time.Duration {
	return durationFromViperMS("responses_websocket_client_ping_interval_ms", 25*time.Second, true)
}

func ResponsesWebsocketClientPongMissTimeout() time.Duration {
	return durationFromViperMS("responses_websocket_client_pong_miss_timeout_ms", 0, true)
}

func ResponsesWebsocketClientInboundActivityTimeout() time.Duration {
	return durationFromViperMS("responses_websocket_client_inbound_activity_timeout_ms", 5*time.Minute, true)
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

func durationFromViperMS(key string, defaultValue time.Duration, nonPositiveDisables bool) time.Duration {
	if !viper.IsSet(key) {
		return defaultValue
	}
	valueMS := viper.GetInt(key)
	if valueMS <= 0 {
		if nonPositiveDisables {
			return 0
		}
		return defaultValue
	}
	return time.Duration(valueMS) * time.Millisecond
}
