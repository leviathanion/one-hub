package config

import (
	"strings"
	"time"

	"one-api/common/utils"

	"github.com/spf13/viper"
)

func InitConf() {
	defaultConfig()
	setEnv()
	Language = viper.GetString("language")
	IsMasterNode = viper.GetString("node_type") != "slave"
	RequestInterval = time.Duration(viper.GetInt("polling_interval")) * time.Second
	SessionSecret = utils.GetOrDefault("session_secret", SessionSecret)
	UserInvoiceMonth = viper.GetBool("user_invoice_month")
	OpenAIRealtimeSessionCompatMode = viper.GetBool("openai.realtime_session_compat")
	RequestBodyDecodeEnabled = viper.GetBool("request_body_decode.enabled")
	RequestBodyDecodeMaxWireBytes = viper.GetInt64("request_body_decode.max_wire_bytes")
	RequestBodyDecodeMaxDecodedBytes = viper.GetInt64("request_body_decode.max_decoded_bytes")
	RequestBodyDecodeMaxDecoderWindowBytes = viper.GetInt64("request_body_decode.max_decoder_window_bytes")
	RequestBodyDecodeMaxExpansionRatio = viper.GetInt64("request_body_decode.max_expansion_ratio")
	RequestBodyDecodeMaxLayers = viper.GetInt("request_body_decode.max_layers")
	GitHubProxy = viper.GetString("github_proxy")
	MCP_ENABLE = viper.GetBool("mcp.enable") != false
	UPTIMEKUMA_ENABLE = viper.GetBool("uptime_kuma.enable") != false
	UPTIMEKUMA_DOMAIN = viper.GetString("uptime_kuma.domain")
	UPTIMEKUMA_STATUS_PAGE_NAME = viper.GetString("uptime_kuma.status_page_name")
}

func setEnv() {
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
}

func defaultConfig() {
	viper.SetDefault("port", "3000")
	viper.SetDefault("gin_mode", "release")
	viper.SetDefault("log_dir", "./logs")
	viper.SetDefault("sqlite_path", "one-api.db")
	viper.SetDefault("sqlite_busy_timeout", 3000)
	viper.SetDefault("sync_frequency", 600)
	viper.SetDefault("batch_update_interval", 5)
	viper.SetDefault("global.api_rate_limit", 300)
	viper.SetDefault("global.web_rate_limit", 180)
	viper.SetDefault("connect_timeout", 30)
	viper.SetDefault("auto_price_updates", false)
	viper.SetDefault("auto_price_updates_mode", "system")
	viper.SetDefault("auto_price_updates_interval", 1440)
	viper.SetDefault("update_price_service", "https://raw.githubusercontent.com/MartialBE/one-api/prices/prices.json")
	viper.SetDefault("language", "zh_CN")
	viper.SetDefault("favicon", "")
	viper.SetDefault("user_invoice_month", false)
	viper.SetDefault("openai.realtime_session_compat", false)
	viper.SetDefault("realtime.unsafe_allow_credential_subprotocol_any_origin", false)
	viper.SetDefault("realtime.websocket_read_limit", int64(32<<20))
	viper.SetDefault("realtime.websocket_ping_interval_ms", 25000)
	viper.SetDefault("realtime_websocket_client_ping_interval_ms", 25000)
	viper.SetDefault("realtime_websocket_client_pong_miss_timeout_ms", 0)
	viper.SetDefault("realtime_websocket_client_inbound_activity_timeout_ms", 0)
	viper.SetDefault("realtime.websocket_write_timeout_ms", 40000)
	viper.SetDefault("responses_ws.connect_per_credential_per_minute", 600)
	viper.SetDefault("responses_ws.active_lease_redis_fail_open", true)
	viper.SetDefault("responses_ws.first_frame_timeout_ms", 30000)
	viper.SetDefault("responses_websocket_client_ping_interval_ms", 25000)
	viper.SetDefault("responses_websocket_client_pong_miss_timeout_ms", 0)
	viper.SetDefault("responses_websocket_client_inbound_activity_timeout_ms", 300000)
	viper.SetDefault("responses_ws.idle_timeout_ms", 1800000)
	viper.SetDefault("responses_ws.max_lifetime_ms", 3600000)
	viper.SetDefault("responses_ws.pending_provider_events_max_bytes", 2<<20)
	viper.SetDefault("request_body_decode.enabled", true)
	viper.SetDefault("request_body_decode.max_wire_bytes", int64(64<<20))
	viper.SetDefault("request_body_decode.max_decoded_bytes", int64(64<<20))
	viper.SetDefault("request_body_decode.max_decoder_window_bytes", int64(128<<20))
	viper.SetDefault("request_body_decode.max_expansion_ratio", int64(64))
	viper.SetDefault("request_body_decode.max_layers", 2)
	viper.SetDefault("codex.execution_session_revocation_timeout_ms", 200)
	viper.SetDefault("mcp.enable", false)
	viper.SetDefault("uptime_kuma.enable", false)
	viper.SetDefault("uptime_kuma.domain", "")
	viper.SetDefault("uptime_kuma.status_page_name", "")
}
